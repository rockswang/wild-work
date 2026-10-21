// Package headers 构造三类上游请求头（common / chat / billing / refresh）。
// 规则来自 本地 docs/upstream-reverse-engineering.md §1/§4。
package upstream

import (
	"net/http"

	"wild-work/internal/auth"
)

// 官方 WorkBuddy 桌面客户端身份（2026-10-21 对本机 /Applications/WorkBuddy.app 5.8.3
// 静态解包取证，不启动客户端、不请求上游）：
//
//	UA           = "WorkBuddy/<app> WorkBuddy/<app> CLI/<cli>"
//	X-IDE-Version= <app>（桌面版本）
//	X-Product    = product.json 的 deploymentType（SaaS）
//
// 取证链：main/sidecar-manager.js 的 getCachedBaseEnv 给内嵌 CLI 配
// CLIENT_INFO_PLATFORM/PRODUCT_NAME=WorkBuddy、PLATFORM_VERSION/PRODUCT_VERSION=appVersion、
// USER_AGENT_EXTENSION=CLI/<cliVersion>；cli.mjs 的 buildUserAgent 依次 unshift platform、
// productName 再把 extension push 到末尾；appVersion 来自 Info.plist（5.8.3），cliVersion 来自
// 内嵌 @tencent-ai/codebuddy-code 的版本（2.161.0）。
//
// 旧值 `CLI/2.63.2 CodeBuddy/2.63.2` + X-IDE-Version 5.5.4 是 CodeBuddy CLI 时代的形态，
// 与桌面宿主实际发出的一整串（UA + IDE 版本 + Product + Private-Data + Agent-Intent）
// 不一致——混合身份本身就是特征，见 R117。
const (
	clientUA            = "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0"
	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
	defaultClientVer    = "5.8.3"
)

func originRefererFor(a *auth.Auth) string {
	if a != nil && a.Region() == "global" {
		return originRefererGlobal
	}
	return originRefererCN
}

// CommonHeaders 设置所有 API 共享的请求头。
func CommonHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-CodeBuddy-Request", "1")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("Accept-Language", acceptLanguageFor(a))
}

// ChatHeaders 在 common 之上加 chat 专属的账号头。
// 缺省字段用 X-No-* 约定（与 CodeBuddy 官方 CLI 一致）。
//
// conv / turn 是 conversationAnchors 抽出的会话锚点（国内版此前完全不发 X-Conversation-ID，
// 属单边裸奔 —— 见 R116）；锚点为空时退回每请求随机，行为与历史一致。
func ChatHeaders(req *http.Request, a *auth.Auth, conv, turn string) {
	CommonHeaders(req, a)
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	} else {
		req.Header.Set("X-No-Authorization", "1")
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	} else {
		req.Header.Set("X-No-User-Id", "1")
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	} else {
		req.Header.Set("X-No-Enterprise-Id", "1")
	}
	// 安全红线：绝不在 chat 请求里携带 X-Refresh-Token。
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	} else {
		req.Header.Set("X-No-Department-Info", "1")
	}
	injectAttribution(req, a)
	injectConversationHeaders(req, a, conv, turn)
}

// BillingHeaders billing 接口请求头。
func BillingHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只允许出现在这里）。
// **调用方必须持有 a.Lock()**（本函数直接读 a.RefreshToken，不加锁——
// 若调用方未持锁，与 keepalive 刷新写回构成数据竞争）。
func RefreshHeaders(req *http.Request, a *auth.Auth) {
	CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
	}
	req.Header.Set("X-Auth-Refresh-Source", "plugin")
}

// injectAttribution 注入归属头——按官方桌面客户端的实际取值（R117）：
// X-Product 取 product.json 的 deploymentType（SaaS），X-Private-Data 默认 true，
// X-Agent-Intent 缺省 craft（cli.mjs: meta["codebuddy.ai/mode"] ?? "craft"）。
func injectAttribution(req *http.Request, a *auth.Auth) {
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Purpose", "conversation")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", defaultClientVer)
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Private-Data", "true")
}

// injectConversationHeaders 注入会话头族（X-Conversation-* / B3 链路）。
//
// 语义与形态见同目录 convheaders.go：会话级恒定、轮级一次 user send（工具回调/重试复用）、
// 调用级每请求新生成；X-Root-Request-ID 取本轮 id（官方主对话同形）。
// B3 是每请求链路头，仍跟调用级 id 走。
func injectConversationHeaders(req *http.Request, a *auth.Auth, conv, turn string) {
	uid := ""
	if a != nil {
		uid = a.UID
	}
	msgID := newMessageID()
	turnID := turnIDOf(uid, conv, turn)
	req.Header.Set("X-Conversation-ID", conversationID(uid, conv))
	req.Header.Set("X-Conversation-Request-ID", turnID)
	req.Header.Set("X-Conversation-Message-ID", msgID)
	req.Header.Set("X-Request-ID", msgID)
	req.Header.Set("X-Root-Request-ID", turnID)
	req.Header.Set("X-B3-TraceId", msgID)
	req.Header.Set("X-B3-SpanId", msgID[:16])
	req.Header.Set("X-B3-Sampled", "1")
}

func acceptLanguageFor(a *auth.Auth) string {
	if a != nil && a.Region() == "global" {
		return "en-US"
	}
	return "zh-CN"
}


