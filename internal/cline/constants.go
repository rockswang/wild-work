// Package cline 是 Cline（api.cline.bot）渠道的**免费模型池**上游实现。
//
// 凭据有**两种**形态，由 Auth 的字段区分（2026-10-14 扩展）：
//
//  1. **官方 API Key**（`sk_` + 64 hex）：在 https://app.cline.bot/ → Account / API Keys
//     创建。Key **不过期、不轮换** ⇒ 存 AccessToken、ExpiresAt 取 FarFutureExpiry()、
//     不参与任何刷新。
//  2. **refreshToken**（25 位短串）：WorkOS OAuth 登录链路的产物（同 cline2api 等
//     第三方网关的号池格式）。存 RefreshToken，用它换 accessToken（有效期约 1h），
//     换来的 token 需带 `workos:` 前缀才能过鉴权（见 authHeader）。**可批量导入**，
//     是「大量账号池」场景的主用形态。
//
// 两种形态在 client 层统一收敛为「一个可用的 access token」：Key 模式直接就是 token，
// refreshToken 模式经 ensureAccessToken 按需换取（带 single-flight）。
//
// 四个必须知道的实测事实（2026-10-10，逐项对照实验，详见 docs/cline渠道接入计划.md）：
//  1. **`X-CLIENT-TYPE` 头必须非空**，否则所有 `cline-free/*` 别名一律 403
//     `API_REQUEST_ERROR_CODE`，而**值内容无关**（`bogus-value` 也放行）⇒ 是个「存在性」闸门。
//  2. **上游把真实状态压平成 HTTP 500**：限流是 `500 {…from Openrouter: request failed
//     with status 429…}`。故 Classify 必须从 body 里正则取**内层**状态码，否则 429 会被
//     归成 5xx（违反不变量 15 的「429 优先」）。
//  3. **失败可能以「HTTP 200 + 流内 error 帧 + 上游自己发 [DONE]」呈现**
//     （`stream_initialization_failed`）⇒ 必须转成 error 返回调用方，否则被伪装成空回答（R40）。
//  4. **路由可从 `response.id` 前缀判别**：`gen-<ts>-<b62>` = OpenRouter；
//     `gen_<ULID>` = Cline 自有网关（带 provider_metadata/marketCost）。配额键名
//     `…UsageCostUSDPerUser` 表明上限按**账号**计 ⇒ 只有走 Cline 网关的模型，
//     多账号轮换才有意义；走 OpenRouter 的撞的是 Cline 共享 key 的 limit_rpd 日限，换号必复现。
package cline

import (
	"time"

	"wild-work/internal/provider"
)

const Kind = provider.Cline

// 上游端点。公开目录 /models 与 recommended-models 都**无需鉴权**，
// 但对话与 api-keys 需要 Bearer sk_。
const (
	APIBase        = "https://api.cline.bot/api/v1"
	EpChat         = "/chat/completions"
	EpRecommends   = "/ai/cline/recommended-models" // 返回 recommended/free/clinePass/clineCloud 四组；free[] 是模型面唯一来源
	EpAPIKeys      = "/api-keys"                    // 返回本账号全部 Key（uid 与同账号判定的来源）
	EpPlans        = "/plans"                       // 权益目录（含 inferenceCapThreshold 结构）
	EpRefresh      = "/auth/refresh"                // refreshToken → accessToken（两种凭据形态都可用）
	defaultBaseURL = APIBase
)

// workosPrefix 是 Cline access token 的传输前缀，**不可省略**（见 authHeader 实测说明）。
// 取自官方 SDK：sdk/packages/shared/.../cline-environment.ts 的 WORKOS_TOKEN_PREFIX。
const workosPrefix = "workos:"

// 请求头常量。HeaderClientTypeValue 取何值都放行，**但不得为空**；
// 一旦这个头被删，症状是「免费模型全部 403 且面板无异常」——同 R34 的静默失败家族。
const (
	HeaderClientType      = "X-CLIENT-TYPE"
	HeaderClientTypeValue = "cline-sdk"
	HeaderAuth            = "Authorization"
)

// max_tokens 三段实测：8 → 上游 400（低于提供方最小值）；16 → 500 `empty response content`
// （过了校验但推理吃光预算，可见内容为 0）；64~200000 → 全部 200；1e6 → 500。
// 客户端不传时**一律不传**（实测不传最稳，可自由跑到 3242 token 并正常出内容）。
const (
	minUpstreamMaxTokens = 64
	maxUpstreamMaxTokens = 200000
)

// KeyPrefixLen 是官方 api-keys 回显的 key_prefix 长度（`sk_` + 7 位 hex）。
// 用它把「我手上的 Key」匹配回列表中的那一条，从而拿到稳定的 apk- id 作 uid；
// 面板的「同账号多 Key 不扩额度」告警也用它比对（app.SubmitClineKey）。
const KeyPrefixLen = 10

// DefaultIdleTimeout 无 config 注入时的流式空闲上限。
// ⚠️ 生产恒取本值：配置项不存在（见 Client.IdleTimeout 注释）。
const DefaultIdleTimeout = 90 * time.Second

// FarFutureExpiry Key 无过期概念。ExpiresAt 必须给远期值而**不得为 0**：
// 0 会让 auth.NeedsRefresh 恒真 ⇒ 每次请求都尝试刷新（不变量 21 的同款陷阱）。
func FarFutureExpiry() int64 { return time.Now().AddDate(80, 0, 0).Unix() }

// freePrefix 是 Cline 自有免费命名空间；passPrefix/cloudPrefix 属订阅/云池，本渠道**不使用**
// （cline-pass 需要订阅，实测加头也无效：ENTITLEMENT_ERROR）。
const (
	freePrefix  = "cline-free/"
	passPrefix  = "cline-pass/"
	cloudPrefix = "cline-cloud/"
)

// routePrefix 判据：response.id 前缀 ⇒ 该请求实际走的是哪条链。
const (
	routeOpenRouter = "gen-" // 经 OpenRouter 转发（无 provider_metadata）
	routeClineGW    = "gen_" // Cline 自有网关（有 marketCost/routing/fallbacks）
)
