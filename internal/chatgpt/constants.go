// Package chatgpt 是 chatgpt.com 网页逆向通道的内核：TLS 指纹传输层、凭据、
// sentinel 对抗算法链路、对话/生图。
//
// 同源：ww3 客户端（MIT）的 internal/cgpt，commit 8c9bf2d。升级 diff 入口见 README.md。
//
// 分层纪律（与其它渠道一致）：
//   - 本包只做「渠道事实」：上游怎么调、怎么解析、怎么分类错误；
//   - 重试/换号/降级/冷却由 internal/server 的策略层负责，本包不做退避重试
//     （fail early：重试会掩盖上游真实状态，且每次重试都要重走预检链路）。
//   - 零 token 落盘：日志/错误串绝不带 access token 或 cookie。
package chatgpt

import "time"

// 渠道身份。两套身份都能通过上游校验，但**不能混用**（自相矛盾的组合本身是特征）。
const (
	// IdentityCodex 是 Codex webview 身份，当前真实网页走这套，是默认值。
	IdentityCodex = "codex"
	// IdentityLegacy 是旧版网页身份（带 sec-ch-ua-full-version-list 等）。
	IdentityLegacy = "legacy"
)

// 默认常量（同源于 ww3 2026-10-26 抓包）。
const (
	// BaseURL 是上游网页基址；可用 Config.Base 覆盖（测试注入 httptest、或指向本地中转）。
	BaseURL = "https://chatgpt.com"

	// UAEdge151 是抓包里的真实 UA（Codex webview 用 Edge）。
	UAEdge151 = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36 Edg/151.0.0.0"
	// UAChrome146 是旧版网页身份用的 UA。
	UAChrome146 = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"

	SecChUaEdge151   = `"Not=A?Brand";v="99", "Microsoft Edge";v="151", "Chromium";v="151"`
	SecChUaChrome146 = `"Chromium";v="146", "Google Chrome";v="146", "Not/A)Brand";v="99"`

	// DefaultTLSProfile 是缺省 TLS 指纹档。Edge 是 Chromium 派生，同版本 ClientHello
	// 与 Chrome 几乎一致，差异只在 HTTP 层 sec-ch-ua ⇒ 用 chrome_152 配 Edge 151 UA 是对的。
	DefaultTLSProfile = "chrome_152"
	DefaultIdentity   = IdentityCodex

	// DefaultTimeout 是非流式请求总超时（可被 upstream.timeout_seconds 覆盖）。
	DefaultTimeout = 120 * time.Second
	// DefaultHeaderTimeout 是「等响应头」下限。它只允许放宽（取 max(下限, 总超时)）：
	// 首字节耗时由上游排队决定，拿总超时去压这一段会把「慢」误判成「失败」。
	DefaultHeaderTimeout = 180 * time.Second
	// DefaultIdleTimeout 是流中「连续无字节」的空闲上限（同源其它渠道的 90s 口径）。
	// 它与 DefaultTimeout 分工不同：拿到响应头之后彻底卡死，多等不会变好。
	DefaultIdleTimeout = 90 * time.Second
)

// 账号端点。
const (
	PathSession = "/api/auth/session"
	PathMe      = "/backend-api/me"
	PathModels  = "/backend-api/models"
)

// modelsQuery 是 /backend-api/models 的固定查询串（同源于 ww3 抓包）。
const modelsQuery = "?iim=false&include_icons=false&is_gizmo=false"

// 对话端点（登录链路；匿名链路 /backend-anon/conversation 本仓不用，spec §8）。
const (
	// PathFConversationPrepare 是 conduit 预取（零配额），返回 60 秒有效的 conduit_token。
	// 它必须**现取现用**：TTL 只有 60 秒，且每轮响应头里还会再下发新的。
	PathFConversationPrepare = "/backend-api/f/conversation/prepare"
	// PathFConversation 是唯一消耗配额的对话端点（两段式：先 conduit 再此处）。
	PathFConversation = "/backend-api/f/conversation"
)

// 对话请求的固定取值（同源于 ww3 2026-10 抓包）。
const (
	// parentMessageRoot 是「新会话」的 parent_message_id 哨兵值（上游认识它）。
	parentMessageRoot = "client-created-root"
	// defaultTimezone / defaultTimezoneOffsetMin 与参考实现一致（上游用它渲染「今天」）。
	// JS `getTimezoneOffset()` 对 UTC+8 返回 -480，故此处也是负数。
	defaultTimezone          = "Asia/Shanghai"
	defaultTimezoneOffsetMin = -480
	// conduitPromptCap 是 conduit 预取里 partial_query 的截断长度（上游只用它做打字防抖）。
	conduitPromptCap = 200
	// defaultChatModel 是上游「自动选模型」的取值。
	defaultChatModel = "auto"
)
