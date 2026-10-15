package officeace

import (
	"net/http"
	"regexp"
	"strings"

	"wild-work/internal/provider"
)

// 华为 AgentArts / APIG 网关错误码 → provider.ErrKind 分类。
// 参考 officeace2api/src/server.mjs 的 classifyUpstream。
// 关键：码可能装在 HTTP 200 的 SSE error 帧里，也可能压平成 500，故正文一并传入。
var (
	reTooLarge       = regexp.MustCompile(`81113|exceeds the maximum size|RequestEntityTooLarge|payload too large|request body is too (large|long)`)
	reRateLimited    = regexp.MustCompile(`81111|81112|81114|0308|TooManyRequests|rate limit|too many requests|\bTPM\b`)
	reModelRejected  = regexp.MustCompile(`81004|81009|Insufficient permission|Invalid model`)
	reAuthFailed     = regexp.MustCompile(`APIG\.1009|APIG\.1001|APIG\.1002|AppKey or AppSecret is invalid|invalid api key|unauthorized`)
	reUnsupportedPrm = regexp.MustCompile(`unsupported|not support|unrecognized|unknown (parameter|field|argument)|no such (parameter|field)`)
	reBadRequest     = regexp.MustCompile(`81001|validation error|BadRequest`)
)

// classifyUpstream 把 (status, body) 归到 provider.ErrKind。
func classifyUpstream(status int, body string) provider.ErrKind {
	low := strings.ToLower(body)
	full := low

	switch {
	case reTooLarge.MatchString(full):
		// request body 过大（正常压成 429，需还原成请求级、不罚号）
		return provider.ErrPassthrough
	case status == http.StatusTooManyRequests || reRateLimited.MatchString(full):
		// TPM 限流（凭据×模型）→ 软冷却
		return provider.ErrSoftRate
	case reModelRejected.MatchString(full):
		// 81004 无权限 / 81009 名字不认 → 模型级拒绝（账号未必坏，透传）
		return provider.ErrModelBlocked
	case status == http.StatusUnauthorized || status == http.StatusForbidden || reAuthFailed.MatchString(full):
		// 整条凭据鉴权失败
		return provider.ErrSessionDead
	case reBadRequest.MatchString(full) || status == http.StatusBadRequest:
		if reUnsupportedPrm.MatchString(full) {
			return provider.ErrBadParams
		}
		return provider.ErrBadParams
	case status >= 500:
		return provider.ErrServer
	case status == http.StatusNotFound:
		return provider.ErrNotFound
	case status >= 400:
		return provider.ErrClient
	default:
		return provider.ErrNone
	}
}

// statusOfPayload 从伪 200 的 error 体里还原真实 HTTP 状态码。
// 81111/81112/81113/81114/0308 → 429；APIG.1009/1001/1002 → 401；81001 → 400；否则 502。
func statusOfPayload(body string) int {
	low := strings.ToLower(body)
	switch {
	case regexp.MustCompile(`81111|81112|81113|81114|0308`).MatchString(low):
		return http.StatusTooManyRequests
	case regexp.MustCompile(`apig\.1009|apig\.1001|apig\.1002`).MatchString(low):
		return http.StatusUnauthorized
	case regexp.MustCompile(`81001`).MatchString(low):
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}
