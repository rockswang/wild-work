// retry.go loomy 上游瞬时失败的重试策略（判据 / 退避 / 预算）。
//
// 背景（实测，见工单 02 与 spec）：loomy 网关偶发返回 502（`data/app.log` 里多日持续，
// 11/07~11/12 共 116 次，非一次性事故），而这类抖动通常 10s 内自愈。当前实现把上游非 2xx
// 原样透传 ⇒ 下游 agent（Codex / pi）把一次抖动当成整段会话失败；且 handler 在 5xx 出口
// **先清粘性路由** ⇒ 与账号健康无关的抖动会打散「同对话同账号」。
//
// 策略（决策台账 D1–D13）：
//   - 只吸收「上游根本没把请求送出去」的失败：HTTP 502/503，以及**快速**的传输层瞬态
//     （连接被重置 / 被拒 / broken pipe / EOF）；
//   - 明确不吸收：500（存在确定性形态）、504 与「等响应头超时」（上游可能**已开始生成**，
//     重发有重复生成 / 重复扣分风险）、429（限流语义 + 风控风险）、其它 4xx（重发必然复现）；
//   - 逐次独立判定：判据对**每一次尝试**的响应独立评估，序列中间改口说 429/400/504
//     立即返回该响应，不看「首次是什么」；
//   - 固定同一账号、不计账号错误、不轮换、不触发模型降级链（本包不通知 handler）；
//   - 只在**响应头阶段**（流为 nil、下游零字节）重试，对下游协议安全。
package loomy

import (
	"errors"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// 重试预算与退避：包级非导出**变量**（工单要求：测试内可改小；不新增配置项、不注入 clock）。
var (
	// chatMaxAttempts 一次逻辑请求的总尝试次数 = 首次 + 最多 3 次重试。
	chatMaxAttempts = 4
	// chatRetryBaseDelay 退避基准（指数退避 1s / 2s / 4s + ±25% 抖动）。
	chatRetryBaseDelay = time.Second
	// chatRetryDeadline 总死线，自首次尝试起算（对下游的「最多多等约 10s」承诺）。
	chatRetryDeadline = 10 * time.Second
)

// chatRetryable 单点判据：这一次尝试的失败是否值得原地重试。
//
// 判据集中在这里（不得在循环里散落 `status == 502 || status == 503` 之类的内联条件）：
//   - 传输层错误走 chatRetryableErr（仅快速瞬态）；
//   - 其余只看状态码：502 / 503 可重试，**500/504/429/其它 4xx 一律不可重试**。
func chatRetryable(status int, err error) bool {
	if err != nil {
		return chatRetryableErr(err)
	}
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable
}

// chatRetryableErr 判断传输层错误是否为**快速**瞬态（可重试）。
//
// 慢失败一律不重试：`timeout awaiting response headers`、`dial i/o timeout`、
// `TLS handshake timeout` 这类超时意味着上游**可能已经开始生成**（首次尝试自身就可能越线），
// 重发既可能双扣分、又会让用户连等两次长超时。
func chatRetryableErr(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, noRetry := range []string{"timeout", "deadline exceeded", "x509", "certificate", "unsupported protocol", "malformed"} {
		if strings.Contains(s, noRetry) {
			return false
		}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	for _, yes := range []string{"connection reset", "connection refused", "broken pipe",
		"eof", "server closed idle connection", "empty reply"} {
		if strings.Contains(s, yes) {
			return true
		}
	}
	return false
}

// chatBackoff 第 attempt 次重试前的等待（指数退避 + ±25% 抖动，避免并发请求同步重试）。
func chatBackoff(attempt int) time.Duration {
	d := chatRetryBaseDelay << (attempt - 1)
	jitter := time.Duration(rand.Int63n(int64(d/2+1))) - d/4
	return d + jitter
}

// chatAttemptReason 一行失败描述（状态码 + 截断的响应体；传输层错误取原文）。
// 用于日志与「零凭据」纪律：响应体截断到 120 字符，绝不落 token。
func chatAttemptReason(status int, respBody []byte, err error) string {
	if err != nil {
		return truncate(err.Error(), 80)
	}
	if len(respBody) == 0 {
		return "http " + http.StatusText(status)
	}
	return "http " + http.StatusText(status) + " body=" + truncate(string(respBody), 120)
}
