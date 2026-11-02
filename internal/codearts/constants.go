// constants.go 华为云 CodeArts Agent 渠道的静态常量（主机、端点、签名参数、Extra 键名）。
//
// 协议依据：docs/ref-ofm-渠道-codearts-opencode-kilo.md 的「2. 渠道①：CodeArts」整章
// （每条事实都带参考实现的 `文件:行号`）。本文件只放**非敏感**常量：主机、路径、头名、
// Extra 键名、超时与静态兜底模型表。
//
// 凭据（access_key_id / secret_access_key / security_token / refresh_token / DPoP 私钥）
// 一律来自 auths/codearts-*.json（见 account.go），**不得**出现在这里（AGENTS.md §0.2 零 token 落盘）。
package codearts

import (
	"time"

	"wild-work/internal/provider"
)

// Kind 渠道标识，直接复用 provider.CodeArts（模型前缀路由 / BuiltinKinds / stats 白名单的
// 共同真源；本包再抄字面量就会与真源漂移，R10/R52）。
const Kind provider.Kind = provider.CodeArts

// 上游主机（协议 §2.1）。
const (
	// APIBase 对话与控制面主机：华为云 snap-access（cn-north-4）。
	// 协议明确**刻意不实现** `.com → .cn` 域名回退（§6 第 10 条），故只此一个。
	APIBase = "https://snap-access.cn-north-4.myhuaweicloud.com"
	// BenefitBase benefit（免费额度）模型目录主机。
	BenefitBase = "https://opengw.developer.huaweicloud.com"
	// STSTokenURL OAuth 授权码 / refresh_token 换取临时 AK/SK 的端点（协议 §2.1）。
	STSTokenURL = "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens"
	// PortalAuthorizeURL 新式（默认）OAuth 授权页。
	PortalAuthorizeURL = "https://codearts.huaweicloud.com/portal/authorize"
	// PortalLoginURL 新式流程的登录结果页。
	PortalLoginURL = "https://codearts.huaweicloud.com/portal/login"
)

// OAuth / PKCE 参数（协议 §2.1、§2.2）。导出供登录包（internal/login_codearts）复用，
// 避免两处各写一份字面量而漂移。
const (
	OAuthClientID = "codearts-agent"
	// URIScheme 桌面端自定义 scheme。本工具走本机 http 回调（127.0.0.1:<port>），
	// 但授权页参数仍按协议原样带上。
	URIScheme = "codearts-agent"
	// PluginName / PluginVersion 授权页参数（协议 §2.1）。
	PluginName    = "snap_AIIDE"
	PluginVersion = "5.2.0"
	// OAuthCallbackPath 新式流程的本地回调路径（旧 ticket 流程是 /authentication）。
	OAuthCallbackPath = "/oauth/callback"
	// CodeChallengeMethod ⚠️ **非标准取值**：RFC 7636 是 `S256`，portal 只认字面量
	// `SHA-256`；写 S256 会**静默回退**到旧 ticket 流程（协议 §2.2 末条）。
	CodeChallengeMethod = "SHA-256"
	// MinCallbackPort 回调端口下限（协议 §2.1：端口须 ≥10000）。
	MinCallbackPort = 10000
	// MaxCallbackPort 随机取端口的上限（避免落在 65536 上）。
	MaxCallbackPort = 65000
)

// 端点路径（协议 §2.1）。
const (
	// EpChatCompletions 对话端点（挂在 APIBase 的 /api/v2 下，故整段写全）。
	EpChatCompletions = "/api/v2/chat/completions"
	// EpQueueStatus 并发排队状态（本包只做错误分类，不做排队轮询，见 classify.go 注释）。
	EpQueueStatus = "/api/v1/queue/status"
	// EpStatisticsPlugin 账户 / 套餐 + 积分（**裸对象**响应，无 {code,data} 信封）。
	EpStatisticsPlugin = "/snap-manager/v1/statistics/plugin"
	// EpOpsDelivery 活动列表（**有** {code,message,data} 信封）。
	EpOpsDelivery = "/v1/ops/delivery"
	// EpOpsClaim 积分领取。
	EpOpsClaim = "/v1/ops/claim"
	// EpOpsConfirm 领取确认。
	EpOpsConfirm = "/v1/ops/confirm"
	// EpBuiltinModels 常规模型目录。
	EpBuiltinModels = "/v1/model/builtin"
	// EpBenefitConfig benefit 模型目录（挂在 BenefitBase 下）。
	EpBenefitConfig = "/api/v1/gateway/config"
	// EpChatSessionHeartbeat 会话坑位心跳（**决定性修复 R145**）。
	//
	// 上游对每个账号有「并发会话数上限 3」的硬限制（实测 body：
	// `{"error_code":"TM.00001041","error_msg":"并发会话数已达上限(3个)…"}`）。
	// 该限制的实际计数键是 `user-session-id` 头，坑位**不会被对话流结束自动释放**；
	// 唯一释放手段是官方 agentkernelServer 的 ChatSessionHeartbeat：
	//   PUT {Base}/snap-manager/v1/chat-session/heartbeat?status=idle   （user-session-id 头）
	// 实测（§0.1 单发 A/B，同一凭据同一 body 只改这一处）：
	//   · 不盖心跳、每请求新 id ⇒ 第 3~4 次必 400 TM.00001041；
	//   · 每次对话毕对该 sid 发一次 idle 心跳 ⇒ 连打 8 次全 200；
	//   · 同一 sid 反复用但不盖心跳 ⇒ 也全 200（坑位按 sid 计，复用一个 id 只占 1 坑）。
	// 官方源码取证：extension 的 ChatSessionHeartbeat 服务（`/snap-manager/v1/chat-session/heartbeat`、
	// status 走 query 参数、`user-session-id` 头取会话 id、busy 每 30s 一次、idle 收尾一次）。
	EpChatSessionHeartbeat = "/snap-manager/v1/chat-session/heartbeat"
)

// 参与 / 不参与签名的头名（协议 §2.2）。**小写字面量**是签名侧的键名；
// 出站用 Go 的 Header.Set（会自动规范成 X-Sdk-Date 形态），两者不冲突。
const (
	hdrHost          = "host"
	hdrSDKDate       = "x-sdk-date"
	hdrContentSHA256 = "x-sdk-content-sha256"
	hdrSecurityToken = "x-security-token"
	hdrContentType   = "content-type"
	hdrAuthorization = "Authorization"
	HeaderMaaSType   = "maas_type"
	HeaderAgentType  = "Agent-Type"
	HeaderXLanguage  = "X-Language"
	HeaderChatID     = "Chat-Id"
	HeaderSessionID  = "Session-Id"
	// HeaderUserSessionID 会话坑位计数键（R145）。官方 agentkernelServer 在 chat 与心跳
	// **两处都发**，缺了它上游无法把坑位与会话对上 ⇒ 每个请求独占一个新坑且永不释放，
	// 累积到 3 即 `TM.00001041`（实测）。心跳端点靠它定位要释放的会话。
	// ⚠️ 与 x-ot-* 同纪律：必须**签名后追加**，进 canonical 会 401 APIG.0301。
	HeaderUserSessionID = "user-session-id"
	HeaderLang          = "lang"
	HeaderSnapTraceID   = "x-snap-traceid"
)

// 业务常量。
const (
	// MaaSTypeBenefit benefit 模型必带的签名头取值（协议 §2.2 / §2.5(c)）。
	MaaSTypeBenefit = "benefit"
	// ChannelIDE 活动接口的渠道参数。
	ChannelIDE = "IDE"
	// AgentTypePromptCenter 目录 / 积分 / 活动端点的 Agent-Type 取值。
	AgentTypePromptCenter = "PromptCenter"
	// AgentTypeInferHub queue/status 端点的 Agent-Type 取值。
	AgentTypeInferHub = "INFERHUB_AGENT"
	// LangZH / LangEN X-Language 取值：目录与积分为 zh-cn，对话与 queue 为 en。
	LangZH = "zh-cn"
	LangEN = "en"
	// CampaignTypeUserLogin 「每日签到得积分」活动的 type（其余三类不属每日签到）。
	CampaignTypeUserLogin = "USER_LOGIN"
)

// 积分 metric 名（协议 §2.6(b)）。
const (
	creditTotal    = "usageTotalPackageCredit"
	creditBasic    = "usageBasicPackageCredit"
	creditOnDemand = "usageOnDemandPackageCredit"
	creditBonus    = "usageBonusPackageCredit"
)

// 活动领取状态（协议 §2.6(c) 第 3 步的幂等预检判据）。
const (
	campaignStatusClaimed   = "CLAIMED"
	campaignStatusConfirmed = "CONFIRMED"
	campaignStatusConsumed  = "CONSUMED"
)

// Extra 键名（协议原文 snake_case；与 §2.7 的 CodeArtsCredential 字段一一对应）。
//
// 这里承载的正是**本渠道的凭据本体**：CodeArts 不是 OAuth Bearer 渠道，出站请求靠
// SDK-HMAC-SHA256 签名，参与签名的头由这三件套组成：
//
//	access_key_id（Access= 声明）、secret_access_key（HMAC 的 key）、security_token（x-security-token 头）
//
// 另有 code_verifier（PKCE）+ dpop_private_key_jwk（ES256 私钥）用于静默续期。
// 通用字段（access_token / refresh_token / uid / nickname / expires_at）**不重复进 Extra**：
// 它们已有 auth.Auth 的顶层字段承载（refresh_token → RefreshToken、到期 → ExpiresAt/秒）。
const (
	ExtraAccessKeyID       = "access_key_id"
	ExtraSecretAccessKey   = "secret_access_key"
	ExtraSecurityToken     = "security_token"
	ExtraDomainID          = "domain_id"
	ExtraUserID            = "user_id"
	ExtraUserName          = "user_name"
	ExtraProjectID         = "project_id"
	ExtraRegion            = "region"
	ExtraMaaSType          = "maas_type"
	ExtraCodeVerifier      = "code_verifier"
	ExtraDPoPPrivateKeyJWK = "dpop_private_key_jwk"
)

// 超时与看门狗。
const (
	// requestTimeout 控制面（目录 / 积分 / 活动）请求总超时。
	requestTimeout = 60 * time.Second
	// heartbeatTimeout 会话 idle 心跳的超时（R145）。心跳是善后动作，卡住只会拖住
	// 客户端 Close，故给短值；失败按日志记录，不影响已流完的响应。
	heartbeatTimeout = 10 * time.Second
	// oauthTimeout STS 换取 / 续期的总超时。
	oauthTimeout = 30 * time.Second
	// DefaultFirstTokenTimeout 首 token 超时（协议 §2.4：参考实现 300 000 ms）。
	DefaultFirstTokenTimeout = 300 * time.Second
	// DefaultChunkTimeout chunk 间超时（协议 §2.4：参考实现 600 000 ms）。
	DefaultChunkTimeout = 600 * time.Second
	// catalogTTL 动态目录进程内缓存时长（协议 §2.5(b)：参考实现 2 h 定时刷新）。
	catalogTTL = 2 * time.Hour
	// maxSnippet 错误摘要的字节上限（日志/错误文本用，避免长响应体撑爆日志）。
	maxSnippet = 300
)
