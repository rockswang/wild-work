// gateway.go 网关层错误页识别：APISIX/openresty 在**认证之前**拦下请求时，
// 返回的是 HTML 错误页而非上游业务信封（`{"code":…,"msg":…}`）。
//
// 为什么必须单独识别（2026-10-17 现场）：两个 WorkBuddy 账号的 refresh token 被
// 上游吊销后，`www.codebuddy.cn` 的账单/目录/对话接口一律返回
//
//	HTTP 401
//	<html><head><title>401 Authorization Required</title></head>
//	<body><center><h1>401 Authorization Required</h1></center>
//	<hr><center>openresty</center><p><em>Powered by <a href="https://apisix…">
//
// 而 `Classify` 只认业务码（`12153` / `Offline user session not found`），
// HTML 页一路落到「其它 4xx」= `ErrClient`。后果有三层，全部静默：
//  1. `app.refreshIfSessionDead` 只对 `ErrSessionDead` 生效 ⇒ 401 自愈永不触发；
//  2. `FetchModels`/`FetchModelPricing` 当时返回的是裸 `fmt.Errorf`（非 *provider.Error）
//     ⇒ 连分类都没有，模型面与费率每 30min 只打一条日志，面板长期陈旧；
//  3. 面板「刷新积分」只报「失败 2」，错误文案是整段 HTML（用户无法判断该重登还是重试）。
//
// 判据「窄而确定」：状态码 401 + 无业务信封 + 是 HTML 页 + 含网关文案。
// 宁可漏判（退回 ErrClient，行为与修复前一致）也不误判——误判成 ErrSessionDead
// 会把账号永久 Disable（不变量 19/22），代价远高于少自愈一次。
package upstream

import "strings"

// GatewayAuthHint 网关层 401 的可行动文案（替换掉无信息量的 HTML 页）。
// 面板/日志/API 错误消息统一用它，避免把整段 openresty 页面抛给用户。
const GatewayAuthHint = "上游网关 401（APISIX/openresty 拦截页）：access token 已被拒绝，需刷新 token 或重新登录"

// gatewayAuthMarkers 网关自带 401 页面的特征文案（区分「网关拒绝」与「上游业务拒绝」）。
var gatewayAuthMarkers = []string{"401 Authorization Required", "Authorization Required"}

// IsGatewayAuthPage 报告 body 是否是网关（APISIX/openresty）的认证拦截页。
//
// 反例（必须返回 false，否则会把业务错误误判成登录态失效）：
//   - `{"code":9999,"msg":"bad token"}`：有业务信封，是上游业务拒绝；
//   - `{"error":"<html>401 Authorization Required</html>"}`：JSON 里嵌了 HTML 片段，
//     它不是网关页本身（故判据要求 body **以 `<` 开头**，而不是「包含 `<html`」）；
//   - 纯文本 / 任意 JSON：不是 HTML 页；
//   - HTML 但无网关文案：可能是上游自己的错误页，语义未知，不猜。
func IsGatewayAuthPage(body string) bool {
	s := strings.TrimSpace(body)
	if s == "" || s[0] != '<' {
		return false
	}
	if !strings.Contains(strings.ToLower(s), "<html") {
		return false
	}
	for _, m := range gatewayAuthMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
