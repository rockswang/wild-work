// classify.go CodeArts 上游错误的分类（驱动 pool 冷却状态机 / 轮换决策）。
//
// 本渠道的错误面有三条与其它渠道不同的规则（协议 §2.4）：
//
//  1. **错误常以 HTTP 200 + 内嵌 `error_code` 下发**（不只是非 2xx）⇒ 非 2xx 路径与
//     流内路径必须共用同一套分类；
//  2. **`429` 必须按「独立数字」匹配**：裸子串匹配会命中额度码 `4291`，把「额度用尽」
//     误判成「可重试排队」，表现为 30 分钟静默重试、界面零输出（历史事故）；
//  3. **`4291`（额度用尽）不可重试** → ErrHardCredit；`TM.00001041` / `81111`（排队）
//     → ErrSoftRate（可重试）。
//
// 判定顺序（两处坑都要躲开，见下）：
//
//	① 精确排队码 → 软限流；② 精确额度码 → 硬额度；③ 独立数字 429 → 软限流；
//	④ 泛化限流词 → 软限流；⑤ 鉴权 → 会话终态；⑥ 其余按状态码。
//
// 为什么精确额度码排在「独立数字 429」**之前**：额度失败方向最贵（误判成可重试 =
// 30 分钟静默重试），而华为的响应里带 requestId/traceId 这类十六进制串，
// 随机出现的 `429` 三个数字（如 `...429a...`）会被独立数字正则命中。
// 反过来 ① 在 ② 之前，保证排队报文里若同时出现额度字样也仍按排队处理——这与
// AGENTS.md 不变量 15「429 优先于 hardMarkers」同向（`4291` 是**精确码**而非泛化词）。
package codearts

import (
	"net/http"
	"regexp"
	"strings"

	"wild-work/internal/provider"
)

var (
	// reQueueCode 精确排队码：TM.00001041（排队）/ 81111（APIG 限流）。
	reQueueCode = regexp.MustCompile(`TM\.00001041|81111`)
	// reQuotaCode 精确额度码：4291（InferHub.4291.200）+ insufficient quota 措辞。
	reQuotaCode = regexp.MustCompile(`(?i)4291|insufficient quota|quota[ _-]?exhausted`)
	// reStandalone429 独立数字 429：前后都不得是数字（`4291` 不得命中）。
	reStandalone429 = regexp.MustCompile(`(?:^|[^0-9])429(?:[^0-9]|$)`)
	// reRateWord 泛化限流词表。
	reRateWord = regexp.MustCompile(`(?i)\bTPM\b|rate[ _-]?limit|too many requests|排队|限流`)
	// reAuthText 鉴权失效文本（含 401 报文里的签名失败）。
	reAuthText = regexp.MustCompile(`(?i)APIG\.0301|APIG\.0602|invalid token|token expired|verify ak sk signature fail`)
	// reNoBenefitPackage 该账号无 benefit 包（InferHub.4004.200）：正确处置是**去掉
	// maas_type 头重试一次**（见 client.go 的 chatStream），分类上归请求级。
	reNoBenefitPackage = regexp.MustCompile(`4004\.200`)
	// reModelRejected 模型未注册 / 不支持（benefit 属性判错时的两种报错）。
	reModelRejected = regexp.MustCompile(`(?i)002002009|model is not registered|unsupported model`)
	// reContextWindow 上下文超限措辞。
	reContextWindow = regexp.MustCompile(`(?i)context (length|window)|maximum context|input (is )?too long|exceeds? the (maximum )?(context|token)`)
)

// Classify 实现 provider.Upstream。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// Classify 按状态码 + body 语义分类。
func Classify(status int, body string) provider.ErrKind {
	low := strings.ToLower(body)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// 401/403：AK/SK 被撤、临时凭据过期、签名失败都落这里。本渠道有 refresh_token
		// 可续期（handler 会 refresh+落盘+重试一次），故归 ErrSessionDead。
		return provider.ErrSessionDead
	case reQueueCode.MatchString(body):
		// 排队/限流：可重试（协议：等 10s 重发，上限 30 分钟）。
		return provider.ErrSoftRate
	case reQuotaCode.MatchString(low):
		// 免费额度用尽：**不可重试**（重试到天亮也没用），按长冷却处理。
		return provider.ErrHardCredit
	case status == http.StatusTooManyRequests || reStandalone429.MatchString(body):
		return provider.ErrSoftRate
	case reRateWord.MatchString(low):
		return provider.ErrSoftRate
	case reAuthText.MatchString(low):
		return provider.ErrSessionDead
	case reNoBenefitPackage.MatchString(low):
		// 该账号无 benefit 包：账号健康，是「这次请求带错了 maas_type」。归请求级
		// （不罚号），由 chatStream 去掉 maas_type 重试一次。
		return provider.ErrClient
	case reModelRejected.MatchString(low):
		// 模型级拒绝（(账号,模型) 负缓存避让），账号未必坏。
		return provider.ErrModelBlocked
	case status == http.StatusNotFound:
		return provider.ErrNotFound
	case reContextWindow.MatchString(low):
		return provider.ErrPromptTooLong
	case status >= 500:
		return provider.ErrServer
	case status == http.StatusBadRequest:
		return provider.ErrBadParams
	case status >= 400:
		return provider.ErrClient
	default:
		return provider.ErrNone
	}
}

// statusOfPayload 从「HTTP 200 + 内嵌 error_code」的报文里还原一个**用于分类与日志**
// 的合成状态码。
//
// 分类本身按 body 判据（与真状态码路径同一套），这里只是为了日志与 provider.Error.Status
// 有个可读数字，不参与分类分支。
func statusOfPayload(body string) int {
	low := strings.ToLower(body)
	switch {
	case reQuotaCode.MatchString(low):
		return http.StatusPaymentRequired // 402 权益不足
	case reQueueCode.MatchString(body) || reStandalone429.MatchString(body) || reRateWord.MatchString(low):
		return http.StatusTooManyRequests
	case reAuthText.MatchString(low):
		return http.StatusUnauthorized
	case reNoBenefitPackage.MatchString(low) || reModelRejected.MatchString(low):
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

// StreamError 是「HTTP 200 + 流内 error_code」的业务错误，实现 provider.StreamErrorClassifier
// （handler 用 errors.As 取值后据此冷却账号——HTTP 200 与响应头早已发出，无法在同一请求内换号）。
type StreamError struct {
	Status int
	Body   string
	// hint 是额度类错误的额外说明（含 UTC+8 重置时刻），创建时算好，保证 Error() 稳定。
	hint string
}

func (e *StreamError) Error() string {
	msg := "upstream stream error (http " + itoa(e.Status) + "): " + snippet([]byte(e.Body))
	if e.hint != "" {
		msg += "；" + e.hint
	}
	return msg
}

// Kind 复用与 HTTP 路径同一套分类。
func (e *StreamError) Kind() provider.ErrKind { return Classify(e.Status, e.Body) }

var _ provider.StreamErrorClassifier = (*StreamError)(nil)

// itoa 小整数转十进制（避免为一处格式化引入 strconv 的依赖噪音）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
