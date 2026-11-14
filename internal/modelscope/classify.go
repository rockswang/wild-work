package modelscope

import (
	"net/http"
	"strings"

	"wild-work/internal/provider"
)

// 模型不存在的上游措辞（各家网关写法不一，只收已见过的保守形态）。
//
// 为什么要在 4xx 里把「模型不存在」单独拎出来：这类错误跟着**模型**走，
// 换账号必然同样失败——归 ErrNotFound 之后 handler 只做短冷却不累计错误计数，
// 而如果被归成 ErrClient 就会带着整池账号一起轮转，白烧一轮时间。
var notFoundMarkers = []string{
	"model not found",
	"model_not_found",
	"model does not exist",
	"unknown model",
}

// Classify 错误分类。**顺序是硬约束**（不变量 15 / R111）：
//
//	① 429 最先判——限流报文里常出现 `quota` / `rate limit` 字样，任何基于 body
//	   的 marker 判定都必须排在它之后，否则限流会被误判成余额类硬冷却；
//	② 401/403 = 凭据失效（R76）：走可自愈的 SessionDead 标记（面板徽标 + 重新粘贴
//	   入口），**不** Disable。「ms-」token 没有 refresh 端点，失效只能靠人工重贴；
//	③ 404 / 「model not found」= 模型级问题（换号无用）；
//	④ 5xx = 上游故障（ErrServer：不罚号、不换号，直接透传）；
//	⑤ 400/422 = 请求形态/参数问题（ErrBadParams：请求级，不罚号不轮转，原文透传）；
//	⑥ 其余 4xx 兜底 ErrClient。
//
// 本渠道**没有**余额/权益概念（credits_na，D5），因此没有任何 hardMarker：
// 不会也不该出现 ErrHardCredit。
func (c *Client) Classify(status int, body string) provider.ErrKind {
	switch {
	case status == http.StatusTooManyRequests:
		return provider.ErrSoftRate
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return provider.ErrSessionDead
	case status == http.StatusNotFound, hasMarker(body, notFoundMarkers):
		return provider.ErrNotFound
	case status >= http.StatusInternalServerError:
		return provider.ErrServer
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return provider.ErrBadParams
	case status >= http.StatusBadRequest:
		return provider.ErrClient
	default:
		return provider.ErrNone
	}
}

// hasMarker 大小写不敏感的子串匹配（marker 表全小写）。
func hasMarker(body string, markers []string) bool {
	if body == "" {
		return false
	}
	lower := strings.ToLower(body)
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}
