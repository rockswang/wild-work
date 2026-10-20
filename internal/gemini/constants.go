// constants.go Gemini（Google Code Assist / Cloud Code）渠道的静态常量。
//
// 协议依据（两级，冲突时以第一级为准）：
//  1. 本地事实文档 `docs/ref-ofm-渠道-zcode-gemini.md` 的「渠道二：Gemini」整章；
//  2. 参考实现源码 `dsh-our-free-model/vendor/channel-pack/src/gemini*.ts`（只读离线取证，
//     行号与常量名在注释里逐条标注）。
//
// 本文件只放**非敏感**常量：主机、端点路径、身份五头、配额桶 id、档位表、超时与静态模型表。
// OAuth 的 client_id / client_secret 是**上游官方客户端内嵌的公开常量**（协议 §2.2 明确给出，
// 且允许 env 覆盖），按原样内嵌；access_token / refresh_token 一律来自
// `auths/gemini-<uid>.json`，绝不出现在本文件（AGENTS.md §0.2「零 token 落盘」）。
package gemini

import (
	"time"

	"wild-work/internal/provider"
)

// Kind 渠道标识，直接复用 provider.Gemini。
//
// 为什么不写字面量 "gemini"：provider.Gemini 是模型前缀路由（PublicModelID / SplitModelID）、
// BuiltinKinds 与 stats 渠道白名单的共同真源（R10 / R52），本包再抄一份就会与之漂移。
const Kind provider.Kind = provider.Gemini

// 上游主机（协议 §2.1，gemini.ts:65-67）。
const (
	// DailyHost 主端点。
	DailyHost = "https://daily-cloudcode-pa.googleapis.com"
	// SandboxHost 沙箱端点。⚠️ quota / loadCodeAssist **固定走它**（见 EndpointFor）。
	SandboxHost = "https://daily-cloudcode-pa.sandbox.googleapis.com"
)

// 端点路径（协议 §2.1，gemini.ts:78-90）。
const (
	// EpGenerateContent 非流式推理路径（本包只做流式，此处保留以便将来接线时一眼可查）。
	EpGenerateContent = "/v1internal:generateContent"
	// EpStreamGenerate 流式推理路径（**不带** query，query 见 StreamQuery）。
	EpStreamGenerate = "/v1internal:streamGenerateContent"
	// StreamQuery 流式请求的 query。逐字为 `?alt=sse`。
	StreamQuery = "?alt=sse"
	// EpQuotaSummary 配额窗口查询路径。
	EpQuotaSummary = "/v1internal:retrieveUserQuotaSummary"
	// EpLoadCodeAssist 账号档位 / 项目探测路径。
	EpLoadCodeAssist = "/v1internal:loadCodeAssist"

	// LoadCodeAssistBody 探测请求体**逐字**常量（gemini.ts:97，content-length 恰为 38）。
	// ⚠️ 刻意**不带** platform / pluginType / cloudaicompanionProject：多一个字段就可能被
	// 上游识别成非官方客户端。不要"顺手"把 project 塞进去。
	LoadCodeAssistBody = `{"metadata":{"ideType":"ANTIGRAVITY"}}`
)

// 身份五头（协议 §2.2，gemini.ts:109-118）。
//
// ⚠️ 取值**逐字写死、不许随机**，且**不得**注入 `x-goog-api-key` / `x-goog-api-client`
// （原版不带）。任取一个字符不同都可能被上游识别成非官方客户端。
//
// 实现细节（如实标注）：Go 的 net/http 会把头名规范化为 `X-Client-Name` 等形态，
// 头名大小写按 RFC 7230 不敏感；**取值**是本包逐字写死的部分，也是上游真正校验的部分。
// 这个规范化由标准库完成，无法也不要绕开。
const (
	IdentityUserAgent     = "antigravity/4.3.0 (cmdc-pak)"
	IdentityClientName    = "antigravity"
	IdentityClientVersion = "4.3.0"
	IdentityMachineID     = "cmdc-pak"
	IdentitySessionID     = "proxy"

	// EnvelopeUserAgent 信封里的 `userAgent` 字段值（与 x-client-name 同值，gemini.ts:118）。
	EnvelopeUserAgent = "antigravity"
)

// 项目与会话（协议 §2.4 / §2.7）。
const (
	// DefaultProject 探测为空/失败时的项目兜底（gemini.ts:132）。
	// ⚠️ 它不是"恒为此值"：免费档账号的 loadCodeAssist 返回空 project 时才回落到它。
	DefaultProject = "aicode-consumers"
	// SessionLaneInfer 推理会话的派生标签（进 sessionId 哈希，gemini.ts:144）。
	SessionLaneInfer = "infer"
)

// 模型与档位（协议 §2.6，gemini.ts:409-491）。
const (
	// ModelFlash 主模型上游名（**不带**档位后缀）。
	// ⚠️ 裸名上游 404：档位后缀是准入钥匙，真正发出的是 `gemini-3.8-flash-<tier>`。
	ModelFlash = "gemini-3.8-flash"
	// ModelDisplayName 面板显示名（gemini.ts:463）。
	ModelDisplayName = "Gemini 3.8 Flash"
	// ContextWindow 上下文窗口（用户拍板填 1_000_000，gemini.ts:421）。
	ContextWindow = 1_000_000
	// MaxOutputTokens 默认最大输出（产品定义 64_000，gemini.ts:249）。
	MaxOutputTokens = 64_000

	TierLow    = "low"
	TierMedium = "medium"
	TierHigh   = "high"
	// TierTiered 自适应档：**只发 includeThoughts，不发 thinkingBudget**。
	TierTiered = "tiered"

	BudgetLow    = 1000
	BudgetMedium = 4000
	BudgetHigh   = 10000
	// BudgetTiered 负数哨兵 = 本档不下发 thinkingBudget（预算交给上游自适应）。
	BudgetTiered = -1
)

// 配额窗口（协议 §2.7，gemini-credits.ts:85-94）。
const (
	// BucketFiveHour 5 小时窗口的桶 id。
	BucketFiveHour = "gemini-5h"
	// BucketWeekly 周窗口的桶 id。
	BucketWeekly = "gemini-weekly"
	// WindowLabelFiveHour / WindowLabelWeekly 面板显示名。
	// ⚠️ 不写「积分」二字：单位是**剩余百分比**，不是可以相加的积分（协议 §2.7）。
	WindowLabelFiveHour = "5 小时窗口"
	WindowLabelWeekly   = "周窗口"
	// QuotaUnit 配额单位（百分比）。
	QuotaUnit = "%"
)

// OAuth 常量（协议 §2.3，gemini-oauth.ts:30-66）。
const (
	AuthEndpoint     = "https://accounts.google.com/o/oauth2/v2/auth"
	TokenEndpoint    = "https://oauth2.googleapis.com/token"
	UserInfoEndpoint = "https://www.googleapis.com/oauth2/v2/userinfo"
	RevokeEndpoint   = "https://oauth2.googleapis.com/revoke"

	// ⚠️ 此处**曾**内嵌 Google 官方客户端的两枚**公开** OAuth 凭据（gemini-oauth.ts:36-38）。
	// 它们不是用户凭据（token 端点强制校验客户端身份：只发 client_id 会回
	// `invalid_request: client_secret is missing`），但 GitHub 的 Secret Scanning
	// Push Protection **只看格式、不看是否公开**，会把它们判定为 secret 并**拒绝推送整个分支**
	// （GH013，2026-10-17 实测）。故改为**运行时获取** —— 实现与优先级见 oauthclient.go：
	// env → 本机官方 CLI 包内提取 → 空串（调用方给可操作错误）。

	// EnvClientID / EnvClientSecret 覆盖用的环境变量名（**最高优先级**；自建凭据场景，gemini-oauth.ts:37-38）。
	EnvClientID     = "CMDC_PAK_GOOGLE_CLIENT_ID"
	EnvClientSecret = "CMDC_PAK_GOOGLE_CLIENT_SECRET"

	// CallbackPath 本机回调路径。**与 client 注册值逐字一致**，改了就是 redirect_uri_mismatch。
	CallbackPath = "/oauth-callback"
	// RedirectHost 重定向 URI 里的主机名。⚠️ 必须是 `localhost`，不能是 127.0.0.1
	// （gemini-oauth.ts:44，Google 侧的注册值）。
	RedirectHost = "localhost"
	// DefaultCallbackPort 固定端口兜底。
	// ⚠️ 默认走**动态端口**（listen :0）；动态分配失败时才退到本端口，
	// 本端口被占用时再退到另一个空闲端口（gemini-oauth.ts:47、:409-463）。
	DefaultCallbackPort = 8845

	ScopeOpenID          = "openid"
	ScopeCloudPlatform   = "https://www.googleapis.com/auth/cloud-platform"
	ScopeUserInfoEmail   = "https://www.googleapis.com/auth/userinfo.email"
	ScopeUserInfoProfile = "https://www.googleapis.com/auth/userinfo.profile"
	// ScopeCCLog / ScopeExperiments 已从 Scopes 剔除（issue #167，未登记 scope 必 403）；
	// 常量保留仅作历史对照，勿加回 Scopes。
	ScopeCCLog       = "https://www.googleapis.com/auth/cclog"
	ScopeExperiments = "https://www.googleapis.com/auth/experimentsandconfigs"
)

// Scopes 逐字照抄 CLI 客户端已登记的部分（gemini-oauth.ts:59-66）：`cloud-platform` 必需。
//
// ⚠️ 2026-10-19 剔除 `cclog` / `experimentsandconfigs`（issue #167）：这两项是 Cloud Code
// 自己的内部 scope，旧 CLI 的公开 client 未登记它们 ⇒ 授权页必报 403 restricted_client
// 「Unregistered scope(s)」。用户实测：收紧后这堵墙消失（后面还撞 client 停用那是另一堵墙，
// 上游政策问题，见登录错误文案的引导）；而 Cloud Code 端点的鉴权只认 access_token，
// 不复核 scope 清单，删掉后运行时行为不变。
var Scopes = []string{
	ScopeOpenID,
	ScopeCloudPlatform,
	ScopeUserInfoEmail,
	ScopeUserInfoProfile,
}

// Extra 键名（写进 auth.Auth.Extra 的渠道专有字段）。
//
// 键名一律用**协议原文的 snake_case / 原文名**，让读代码的人能一眼对上文档字段表：
//   - ExtraKeyProject 对应凭据字段 `cloudaicompanionProject`（协议 §2.8）；
//   - ExtraKeyPaidTier* 对应 `loadCodeAssist` 响应的 `paidTier.{id,name}`（协议 §2.7）。
//
// ⚠️ access_token / refresh_token / uid / nickname / expires_at **不**进 Extra：它们已有
// auth.Auth 顶层字段，重复存两处必然漂移。
//
// ⚠️ sessionId **不落盘**：它是 f(project, contents[0] 首个文本 part, lane) 的确定性派生值
// （协议 §2.4），按请求现算才能做到「同对话稳定、跨对话隔离」；落盘会让不同首条消息复用
// 同一个会话，反而破坏该派生口径。
const (
	ExtraKeyProject       = "cloudaicompanionProject"
	ExtraKeyPaidTier      = "paid_tier"
	ExtraKeyPaidTierName  = "paid_tier_name"
	ExtraKeyPaidTierLabel = "paid_tier_label"
)

// 超时（协议 §2.5 末段 / gemini.ts:210-227）。
const (
	// requestTimeout 业务请求总超时（含配额 / 探测）。
	requestTimeout = 120 * time.Second
	// quotaTimeout 配额 / 探测请求超时（CREDITS_TIMEOUT_MS = 30_000）。
	quotaTimeout = 30 * time.Second
	// oauthTimeout OAuth 端点请求超时（OAUTH_TIMEOUT_MS = 20_000）。
	oauthTimeout = 20 * time.Second
	// DefaultIdleTimeout 流式空闲看门狗阈值（GEMINI_IDLE_TIMEOUT_MS = 120_000）。
	//
	// ⚠️ 必须存在：上游生成大 functionCall 参数期间可能长时间不 flush 任何字节，
	// 裸 `reader.Read()` 会无限期挂起（协议 §2.5）。
	DefaultIdleTimeout = 120 * time.Second
	// QuotaCacheTTL 配额查询的缓存 TTL（QUOTA_CACHE_TTL_MS = 60_000）。
	QuotaCacheTTL = 60 * time.Second
)

// ClientID 返回生效的 client_id：env 覆盖优先，其次从本机官方 CLI 提取，都没有则空串。
// 实现与理由见 oauthclient.go。
func ClientID() string { return oauthClient().ID }

// ClientSecret 返回生效的 client_secret（获取顺序同 ClientID）。
func ClientSecret() string { return oauthClient().Secret }
