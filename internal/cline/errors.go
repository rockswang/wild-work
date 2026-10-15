package cline

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"wild-work/internal/provider"
)

// innerStatusRE 从压平的响应体里取**内层**状态码。
// 实测形态：`… from Openrouter: request failed with status 429: {"error":{…}}`
// 外层一律是 HTTP 500，不挖这一层就只能把所有 429 当 5xx（违反不变量 15 的「429 优先」）。
var innerStatusRE = regexp.MustCompile(`(?i)\bstatus\s+(\d{3})`)

// capMarkers 标识「上限在 Cline→OpenRouter 共享出口上」的文案。
// 命中即意味着：**换 Cline 账号必然复现**（与 R38 的模型级 429 同构），
// 所以不得罚账号，也不得同请求换号。
var capMarkers = []string{
	"limit_rpd",                // OpenRouter 每日请求数上限（实测 thinkingmachines/inkling:free）
	"daily limit reached",      // 同上的人类可读形式，且明说 "Credits don't affect this cap"
	"temporarily rate-limited", // 短时上游限流，实测可在数十秒后自愈（gemma-4-26b 一轮 429、一轮 200）
	"rate limit exceeded",
}

// passMarkers 是「越界用法」：请求了订阅池或不受支持的池，属请求本身的问题。
var passMarkers = []string{"entitlement_error", "not subscribed", "this request is not supported"}

// regionBlockedMarkers 是「模型在本地区被限制」：实测 muse-spark-1.3-contributor 恒返回
// 403 `access forbidden: … is not available in your region`（地区限制，不是账号问题）。
// 与 R38（WorkBuddyAI 模型级 429）/ R47（cline 共享出口上限）同族：**模型级属性**，
// 换账号必然复现 ⇒ 必须 ErrPassthrough（原文透传、不计错、不冷却、不换号）。
// 实测教训（2026-10-12）：未识别时它落 ErrClient（R29 可轮换）⇒ 重试耗尽后把账号判为
// 「连续错误」冷却 60s，导致同账号下另外 3 个正常模型全部 503 no_healthy_account。
var regionBlockedMarkers = []string{"not available in your region", "access forbidden"}

// regionRetryMarkers 是「换出口 IP 有可能救回来」的地区拒绝文案，**触发备用通道重放**用。
// 比 regionBlockedMarkers 严：后者只决定「不罚账号」（误判代价低），而本表命中会多发
// 一发经第三方代理的重放请求，判据宁窄勿宽 —— "access forbidden" 单独出现不足以断定
// 是地区问题（可能是别的风控），不纳入重试判据。
var regionRetryMarkers = []string{
	"not available in your region", // 实测形态（muse-spark-1.3-contributor 国内直连恒回）
	"available in your region", "in your region",
	"in your country", "country not supported",
	"region-restricted", "geo-block", "geoblock",
}

// IsRegionBlocked 报告响应是否是「上游按地区拒绝」（可尝试经备用通道重试）。
// 451 是「因法律/政策原因不可用」的规范状态码，一并认；其余状态一律不认（保守）。
func IsRegionBlocked(status int, body string) bool {
	if status != http.StatusForbidden && status != http.StatusUnavailableForLegalReasons {
		return false
	}
	lower := strings.ToLower(body)
	for _, m := range regionRetryMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// badParamMarkers 是本端参数/头造成的失败，不得计入账号健康度。
var badParamMarkers = []string{
	"empty response content", // max_tokens 太小，推理吃光预算 ⇒ 可见内容为空
	"api_request_error_code", // 多半是漏发 X-CLIENT-TYPE（自己的 bug，不能算到账号头上）
	"invalid 'max_output_tokens'",
	"below minimum value",
	"integer below minimum",
}

// IsCapBody 报告响应体是否命中「共享出口上限」文案（供 ChatStream/Stream 记录模型级冷却）。
func IsCapBody(body string) bool {
	lower := strings.ToLower(body)
	for _, m := range capMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// InnerStatus 取响应体里的内层状态码，无则 0。
func InnerStatus(body string) int {
	if m := innerStatusRE.FindStringSubmatch(body); len(m) == 2 {
		var n int
		if _, err := fmt.Sscanf(m[1], "%d", &n); err == nil && n >= 400 && n <= 599 {
			return n
		}
	}
	return 0
}

// effectiveStatus 把网关压平的 500 还原成内层真实状态码。
//
// 这是对不变量 8「上游 ≥400 直接透传原始响应」的**刻意偏离**：响应体仍然原文透传，
// 只把状态码修正为上游本来的意思。理由：上游把 429 报成 500 是它们网关的缺陷，
// 原样转给客户端会让客户端按「永久错误」处理而放弃重试（429 才带退避语义）。
// 无内层码时保持外层状态不变。
func effectiveStatus(status int, body string) int {
	if status >= 500 {
		if in := InnerStatus(body); in != 0 {
			return in
		}
	}
	return status
}

// Classify 实现 provider.Upstream。顺序敏感：共享上限 → 越界 → 参数 → 内层码 → 外层码。
func Classify(status int, body string) provider.ErrKind {
	lower := strings.ToLower(body)

	// 1) 共享出口上限：账号健康与此无关 ⇒ 透传原文、不罚号、不换号（换号必复现）。
	if IsCapBody(body) {
		return provider.ErrPassthrough
	}
	// 2) 订阅池/云池越界：客户端硬填了 cline-pass/* 或 cline-cloud/*。
	for _, m := range passMarkers {
		if strings.Contains(lower, m) {
			return provider.ErrPassthrough
		}
	}
	// 3) 本端参数/头造成的失败。
	for _, m := range badParamMarkers {
		if strings.Contains(lower, m) {
			return provider.ErrBadParams
		}
	}
	// 3.5) 模型地区限制：模型级属性，换账号必复现 ⇒ 透传原文、不罚号（见 regionBlockedMarkers）。
	for _, m := range regionBlockedMarkers {
		if strings.Contains(lower, m) {
			return provider.ErrPassthrough
		}
	}

	// 4) 内层码优先（外层几乎恒为 500）。
	if in := InnerStatus(body); in != 0 {
		status = in
	}

	// 5) 余额/权益文案（须在 429 之后判：限流 body 高频带 quota/credit 字样，
	//    先判 hardRule 会把限流误归成余额耗尽 → 12h 硬冷却，即不变量 15 描述的坑）。
	if strings.Contains(lower, "insufficient") || strings.Contains(lower, "credit balance") ||
		strings.Contains(lower, "exceeded your current quota") {
		return provider.ErrHardCredit
	}

	switch {
	case status == 401 || (status == 403 && strings.Contains(lower, "unauthorized")):
		// Key 被删/失效：多账号池里禁用该号是正确行为。
		// 注意本渠道 Key 不可刷新 ⇒ 不变量 19 的自愈在此不适用，只能人工换 Key。
		return provider.ErrSessionDead
	case status == 429:
		// 无上限文案的 429 ⇒ 才是真·账号级（配额键名 …PerUser）⇒ 允许冷却+轮换。
		return provider.ErrSoftRate
	case status == 402:
		return provider.ErrHardCredit
	case status == 404:
		// 实测 poolside/laguna-xs：404 `please check the model you provided` + previous_errors 里
		// 有 429 ⇒ 像被驱逐/下架。模型属性而非账号属性，但按短冷却处理不会误伤整号。
		return provider.ErrNotFound
	case status >= 500:
		return provider.ErrServer
	case status >= 400:
		return provider.ErrClient
	default:
		return provider.ErrClient
	}
}

// StreamError 承载「HTTP 200 + 流内 error 帧」的失败，供 handler 走 R40 的惩罚分支。
// 实现 provider.StreamErrorClassifier。
type StreamError struct {
	Model  string // 出错的模型 ID（用于记模型级冷却）
	Code   string // 上游 error.code，实测见 stream_initialization_failed / "502"
	Msg    string // 上游原文
	capped bool   // 是否命中共享上限文案
	kind   provider.ErrKind
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("cline stream error model=%s code=%s: %s", e.Model, e.Code, e.Msg)
}

// Kind 返回账号侧处置口径：命中共享上限 ⇒ ErrPassthrough（不罚号）；
// 其余流内错误一律 ErrPassthrough 兜底 —— 本渠道的流内失败几乎都源于上游模型/池状态，
// 与账号健康无关，误罚整号会像 R16/R38 那样把好号轮空。
func (e *StreamError) Kind() provider.ErrKind {
	if e.kind != provider.ErrNone {
		return e.kind
	}
	return provider.ErrPassthrough
}

// Capped 报告该流内错误是否属于「共享出口上限」（调用方据此记模型级冷却）。
func (e *StreamError) Capped() bool { return e.capped }
