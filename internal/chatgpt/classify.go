package chatgpt

import (
	"fmt"
	"strings"

	"wild-work/internal/provider"
)

// 上游错误文案（判定顺序见 Classify）。
//
// 这些是**文案指纹**，上游改版即失效 —— 与其它渠道同款做法，命中不了就退回状态码分类。
var (
	// unusualActivityMarkers 命中即账号级风控瞬时拒绝（换号 + 软冷却，同一账号不重试）。
	unusualActivityMarkers = []string{
		"unusual activity has been detected",
	}
	// sessionDeadMarkers 命中即网页会话失效（可自愈标记，绝不永久禁用账号）。
	sessionDeadMarkers = []string{
		"not_login", "not logged in", "session expired", "invalid session",
		"authentication token has been invalidated",
	}
	// promptTooLongMarkers 命中即输入超限（请求级错误，原样透传，不罚号不换号）。
	promptTooLongMarkers = []string{
		"input_too_large", "message_length_exceeds_limit", "maximum context length",
	}
	// modelNotFoundMarkers 命中即「伪装成 404 的请求质量不足」（多半是 sentinel 质量不足，
	// 不是模型不存在）⇒ 归上游任务级错误：不换号、不罚账号，让调用方看到真实原因。
	modelNotFoundMarkers = []string{
		"model not found",
	}
)

// hasAny 报告 s（调用方已小写）是否包含任一 marker（marker 自身按小写书写）。
func hasAny(lower string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// Classify 把上游状态码 + 响应体映射成本仓错误分类（spec §10 全表）。
//
// 顺序纪律：
//  1. **429 最先**（不变量 15）：限流报文高频带余额/额度字样，先判文案会把限流误归长冷却；
//  2. 然后文案指纹（403 二分、413、伪装 404）；
//  3. 最后状态码兜底。
func Classify(status int, body string) provider.ErrKind {
	if status == 429 {
		return provider.ErrSoftRate
	}
	lower := strings.ToLower(body)

	if hasAny(lower, unusualActivityMarkers) {
		// 403 Unusual activity = 账号级风控：换号 + 软冷却；同一账号不重试
		// （重复请求无效，且每次重试都在放大风控命中面）。
		return provider.ErrAccountFault
	}
	if hasAny(lower, sessionDeadMarkers) {
		return provider.ErrSessionDead
	}
	if hasAny(lower, promptTooLongMarkers) || status == 413 {
		return provider.ErrPromptTooLong
	}
	if hasAny(lower, modelNotFoundMarkers) {
		return provider.ErrUpstream
	}

	switch {
	case status == 401:
		// 401 一律会话失效：可自愈（重登后按 auth 文件 mtime 自动清标记），不得 Disable。
		return provider.ErrSessionDead
	case status == 403:
		// 其余 403（地区封锁 / 没配代理 / WAF 页）：原文透传，不换号不罚号
		// —— 同机同出口，换号只是空转（spec §12 P2 裁定）。
		return provider.ErrPassthrough
	case status == 402:
		return provider.ErrHardCredit
	case status == 404:
		return provider.ErrNotFound
	case status >= 500:
		return provider.ErrServer
	case status >= 400:
		return provider.ErrClient
	default:
		return provider.ErrClient
	}
}

// StatusError 把非 2xx 响应翻成类型化上游错误（`errors.As` 依赖它判逃生路径，R79）。
func StatusError(method, path string, status int, body []byte) *provider.Error {
	return &provider.Error{
		Kind:   Classify(status, string(body)),
		Status: status,
		Msg:    fmt.Sprintf("%s %s HTTP %d: %s", method, path, status, truncate(string(body), 600)),
	}
}
