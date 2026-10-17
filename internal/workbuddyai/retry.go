// retry.go 上游请求重试策略。
//
// 实测背景：www.workbuddy.ai 的 openresty/APISIX 网关存在间歇性抖动，
// 同一请求连发多次会随机出现 502/503/504（直连亦如此，非本层引入）。
// 504 恒定卡在 ~12.3s（网关自身超时阈值）。
//
// 策略：
//   - 网关类 5xx（502/503/504/500）与传输层错误可重试；
//   - 4xx 一律不重试：402/403 疑似风控或余额不足，400/429 是业务/限流语义，
//     重试只会放大问题（429 频繁重试还可能触发风控）；
//   - **唯一例外**：上游在响应里显式声明 `retry_policy.retry=true` 的 429
//     （14003 瞬时拥塞）。上游自己说"可以重试"时直接抛错并罚号，会把一次可自愈的
//     抖动放大成渠道级故障（见 retryPolicyAllowsRetry 的实测），故单独开一道
//     **收紧的**重试预算（rateLimitMaxAttempts），绝不与 5xx 的额度混用；
//   - 5xx 中的 500 视为上游服务端错误，同样重试。
package workbuddyai

import (
	"encoding/json"
	"errors"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"time"
)

// 重试参数：总尝试次数与退避基准（指数退避 + 抖动）。
const (
	retryMaxAttempts = 3
	retryBaseDelay   = 400 * time.Millisecond

	// rateLimitMaxAttempts 上游显式声明可重试的限流的总尝试数（含首次）。
	// 收紧到 2 = 只重试一次。429 频繁重试有风控风险（见文件头注释），
	// 且不与 5xx 的 retryMaxAttempts 额度混用，避免把可重试限流刷成刷屏。
	rateLimitMaxAttempts = 2
)

// rateLimitBackoff 限流重试前的等待。取变量以便测试提速（勿在运行期改）。
// 上游 displayTips 原文是 "Wait about a minute before retry"，此处取 20s 折中：
// 足以避开最小拥塞窗口，又不至于把一个对话请求挂满一分钟。
var rateLimitBackoff = 20 * time.Second

// retryableStatus 判断 HTTP 状态码是否值得重试。
// 仅 5xx 网关/服务端错误可重试；4xx 明确不重试。
func retryableStatus(status int) bool {
	return status >= 500 && status <= 599
}

// retryPolicyAllowsRetry 判定上游是否在响应体里显式声明「这个错误可以重试」。
//
// WorkBuddyAI 网关对瞬时拥塞返回的 429，早期形态带结构化字段：
//
//	{"code":14003,"msg":"too many requests","retry_policy":{"retry":true},
//	 "displayTips":{"en":"Wait about a minute before retry, or switch to a less busy model."}}
//
// 2026-10-17 上游形态漂移：14003 不再下发 retry_policy，改为 actions 数组
// （当晚生产实测："actions":["SWITCH_MODEL","SUBMIT_FEEDBACK","RETRY"]）。
// 两种结构化信号任一命中即视为上游声明可重试；判据始终不取英文文案
// （displayMsg/displayTips 措辞会随上游改版漂移，6004 与 14003 就是同语义两种措辞）。
//
// 为什么必须理会它：2026-10-12 生产实测，state v4 显示 14003 落到**账号级**冷却，
// 两个账号在 18~48s 内相继中招 → 双双冷却 → 无健康账号 → 客户端拿到
// `503 no_healthy_account`（用户看到的"自定义模型错误"）。
// 而上游明确说"等一分钟再试"即可——即故障本可自愈，是我们把罚号叠上去才放大的。
func retryPolicyAllowsRetry(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var env struct {
		RetryPolicy struct {
			Retry bool `json:"retry"`
		} `json:"retry_policy"`
		Actions []string `json:"actions"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return false
	}
	if env.RetryPolicy.Retry {
		return true
	}
	// 只认 RETRY 元素：SWITCH_MODEL 单独出现是「换模型」语义，
	// 不代表同一请求可以原样重发。
	for _, a := range env.Actions {
		if strings.EqualFold(strings.TrimSpace(a), "RETRY") {
			return true
		}
	}
	return false
}

// retryDecision 单点判断「这次失败是否值得重试」。
// 返回 (retry, isRateLimit)：后者用于走收紧的限流预算与退避。
func retryDecision(status int, body []byte, err error) (retry, isRateLimit bool) {
	if err != nil {
		return retryableErr(err), false
	}
	if status == http.StatusTooManyRequests && retryPolicyAllowsRetry(body) {
		return true, true
	}
	return retryableStatus(status), false
}

// retryableErr 判断传输层错误是否值得重试。
// 连接被重置/中断/eof 等瞬态错误重试；证书类/参数类错误不重试。
func retryableErr(err error) bool {
	if err == nil {
		return false
	}
	// 明确不可重试：证书校验、协议不支持等
	var certErr *net.OpError // 仍可能是瞬态，继续判断
	_ = certErr
	s := strings.ToLower(err.Error())
	for _, noRetry := range []string{"x509", "certificate", "unsupported protocol", "malformed"} {
		if strings.Contains(s, noRetry) {
			return false
		}
	}
	// 超时、连接重置、EOF、对端关闭等 → 可重试
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	for _, yes := range []string{"connection reset", "connection refused", "broken pipe",
		"eof", "timeout", "no such host", "connection attempt failed", "empty reply"} {
		if strings.Contains(s, yes) {
			return true
		}
	}
	return false
}

// backoff 第 attempt 次重试前的等待时长（指数退避 + 抖动，避免请求对齐）。
func backoff(attempt int) time.Duration {
	d := retryBaseDelay << (attempt - 1)
	// 抖动 ±40%
	jitter := time.Duration(rand.Int63n(int64(d/2+1))) - d/4
	return d + jitter
}
