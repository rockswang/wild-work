// constants.go LobsterAI（有道龙虾）渠道的静态常量。
//
// 协议依据：本地文档 docs/ref-ofm-渠道-lobsterai-minimax.md 的「渠道 ① LobsterAI
// （有道龙虾）」整章（§1–§9，逐条标注了参考实现的文件行号）。
//
// 本文件是**本渠道主机 / 端点 / 能力头 / UA / 兜底版本号的单一来源**，全部导出：
// 登录编排（internal/login_lobsterai）与数据面（本包）必须引用同一份取值——
// 上一个渠道曾在两处各写一份常量，被 Lead 收口过一次。
//
// 只放**非敏感**常量：凭据（access_token / refresh_token）来自 auths/lobsterai-*.json，
// **不得**出现在这里（AGENTS.md §0.2 零 token 落盘）。
package lobsterai

import (
	"net/url"
	"strings"
	"time"

	"wild-work/internal/provider"
)

// ---------------------------------------------------------------------------
// 渠道标识与三个主机（协议 §1.1）
// ---------------------------------------------------------------------------

const (
	// Kind 渠道标识，直接复用 provider.LobsterAI。
	//
	// 为什么不写字面量 "lobsterai"：provider.LobsterAI 是模型前缀路由
	// （provider.PublicModelID / SplitModelID）、BuiltinKinds 与 stats 白名单的
	// 共同真源（R10 / R52），本包再抄一份字符串就会与那个真源漂移。
	Kind provider.Kind = provider.LobsterAI

	// APIBase API 基址（对话 / 目录 / 积分 / 签到 / 续期都在这个域上）。
	APIBase = "https://lobsterai-server.youdao.com"

	// PortalBase 登录 portal 基址。协议 §1.1 注释：portal 与 API **同 IP、同 CNAME
	// 目标**（pub-g1-gz.alb.ntes53.netease.com），但上游把它们拆成两个字段，
	// 故这里同样拆开——登录回落 portal、数据面走 API，不要互相顶替。
	PortalBase = "https://lobsterai.youdao.com"

	// VersionURL 客户端版本号查询（**第三方域** api-overmind.youdao.com）。
	// 这个 GET 不需要 Authorization（协议 §2.1 末行）。
	VersionURL = "https://api-overmind.youdao.com/openapi/get/luna/hardware/lobsterai/prod/update"
)

// ---------------------------------------------------------------------------
// 客户端能力头与 UA（协议 §2.2 / §2.3）
// ---------------------------------------------------------------------------

const (
	// Capabilities X-LobsterAI-Client-Capabilities 的取值。**两个能力都不可省**：
	//
	//   - kimi-k3-agentic-v1 是**模型列表准入条件**——不带时 /api/models/available
	//     只返回 25 个模型且**没有 kimi-k3**，带上才是 26 个（协议 §2.2 实测）；
	//   - thinking-level-control-v1 是 reasoning_effort:"off"（关闭思考）的前提——
	//     不带时服务端对 off 直接 HTTP 500（协议 §2.2 实测）。
	Capabilities = "kimi-k3-agentic-v1,thinking-level-control-v1"

	// UserAgent 出站 UA，**固定** LobsterAI/0.1.0。
	//
	// 协议 §2.3 强调这里**刻意不跟随真版本号**：真版本号走
	// X-LobsterAI-Client-Version 头（见 ClientVersion），UA 只是产品形态标识。
	// 把真版本号写进 UA 就是「多发明了一个上游没见过的指纹」，没有收益。
	UserAgent = "LobsterAI/0.1.0"

	// FallbackClientVersion 版本号查询失败时的兜底真值（协议 §2.5 末条）。
	// ⚠️ 兜底值**不缓存**：失败不缓存是协议明确要求，见 version.go。
	FallbackClientVersion = "2026.9.4"

	// HeaderCapabilities / HeaderClientVersion 两个 X-LobsterAI-Client-* 头。
	// 对话与**模型列表**都必需；版本头取动态值（ClientVersion）。
	HeaderCapabilities  = "X-LobsterAI-Client-Capabilities"
	HeaderClientVersion = "X-LobsterAI-Client-Version"
)

// ---------------------------------------------------------------------------
// auth.Auth.Extra 的键名（凭据扩展袋，协议 §8.1）
// ---------------------------------------------------------------------------

// keyfrom 四元组的键名。它们是 refresh 与模型列表的**必需载荷**，随凭据持久化，
// 因此必须落在 auth.Auth.Extra（auth.Auth 没有对应字段，见 internal/auth/auth.go 的
// Extra 注释）。导出是为了让登录包写、本包读，键名只有一处定义。
const (
	// ExtraUUID 安装 UUID：exchange 时生成，**不可每次重生成**（协议 §8.1）。
	ExtraUUID = "uuid"
	// ExtraFirstKeyfrom 首次登录时间戳（毫秒字符串），exchange/refresh 必带。
	ExtraFirstKeyfrom = "first_keyfrom"
	// ExtraLatestKeyfrom 最近活动时间戳。⚠️ 本实现按外部参考实现**刻意不更新**
	// （协议 §3.2 明确要求，见 client.go 的 RefreshToken 注释）。
	ExtraLatestKeyfrom = "latest_keyfrom"
	// ExtraUserID 有道 yid，仅 refresh 请求体用（模型列表用 userId 键名，值同源）。
	ExtraUserID = "user_id"
)

// ---------------------------------------------------------------------------
// 端点路径（协议 §1.2）
// ---------------------------------------------------------------------------

const (
	// EpAuthExchange 授权码换 token；**不带 Authorization**。
	EpAuthExchange = "/api/auth/exchange"
	// EpAuthRefresh 静默续期；**不带 Authorization**。
	EpAuthRefresh = "/api/auth/refresh"
	// EpChatCompletions 对话（OpenAI 兼容面，恒 stream=true）。
	EpChatCompletions = "/api/proxy/v1/chat/completions"
	// EpModelsAvailable 模型列表；query 是 keyfrom 身份载荷。
	EpModelsAvailable = "/api/models/available"
	// EpProfileSummary 积分余额与明细。
	//
	// ⚠️ 协议 §7.2 明确：**不要**用 /api/user/quota —— 它只显示 freeCreditsTotal=300、
	// 不含活动积分（实测同账号 profile-summary 5297.72 而 quota 只有 300）。
	EpProfileSummary = "/api/user/profile-summary"
	// EpActivitySlot 活动槽位（签到三步的第一步）。
	EpActivitySlot = "/api/client-activities/slot"
)

// activityBase 活动详情/签到的路径前缀（拼 activityCode 后再接后缀）。
const activityBase = "/api/client-activities/"

// ---------------------------------------------------------------------------
// 活动槽位的固定 query 与动作名（协议 §1.3 / §7.1）
// ---------------------------------------------------------------------------

const (
	// ActivityPlacement / ActivityContainerAPIVersion / ActivityPlatform 是槽位查询的
	// 固定四参数中的三个（第四个是动态 clientVersion）。
	ActivityPlacement = "desktop_sidebar"
	// ActivityContainerAPIVersion 容器 API 版本，固定 2。
	ActivityContainerAPIVersion = "2"
	// ActivityPlatform 固定 win32。协议 §1.3 明确：这是**伪装客户端形态**，
	// 与运行环境无关（在 mac/Linux 上也照样发 win32）。
	ActivityPlatform = "win32"
	// ActivityCheckInAction 上下文的 actions[] 里必须出现它，签到才可用。
	ActivityCheckInAction = "check_in"
)

// ActivityContextPath 活动上下文路径（GET，query 为 configRevision）。
func ActivityContextPath(activityCode string) string {
	return activityBase + url.PathEscape(strings.TrimSpace(activityCode)) + "/context"
}

// ActivityCheckInPath 签到动作路径（POST）。
func ActivityCheckInPath(activityCode string) string {
	return activityBase + url.PathEscape(strings.TrimSpace(activityCode)) + "/actions/" + ActivityCheckInAction
}

// ---------------------------------------------------------------------------
// 超时（本工具自有工程参数）
// ---------------------------------------------------------------------------

// ⚠️ 协议文档**没有**给出 LobsterAI 的任何超时值（§1–§9 无此项，参考实现里也没有
// 空闲看门狗这一概念）。除登录流程超时外，下列取值都是本仓的工程参数，与上游协议无关：
const (
	// RequestTimeout 非流式业务请求（目录 / 积分 / 签到）的总超时。
	RequestTimeout = 30 * time.Second
	// OAuthTimeout 续期（refresh）等凭据类请求的总超时。
	OAuthTimeout = 20 * time.Second
	// VersionTimeout 版本号查询的总超时：它是第三方域、纯装饰性字段，不该拖慢主流程。
	//
	// 为什么压到 5s：版本号失败**不缓存**（协议 §2.5 明确要求），因此版本接口长期
	// 不可用时每个对话请求都会白等一次这个超时。5s 是「给第三方域一次机会」与
	// 「别把对话请求拖垮」之间的取值；宁可短一点，也不要让每个请求多等 10s。
	VersionTimeout = 5 * time.Second
	// LoginFlowTimeout 登录整体超时。**这一条是协议事实**：参考实现为 10 分钟
	// （协议 §3 流程超时 `10 * 60 * 1000`）。导出给登录编排复用。
	LoginFlowTimeout = 10 * time.Minute
	// DefaultIdleTimeout 流式空闲看门狗阈值（只拦「真的一个字节都不出」，不掐长思考）。
	// 取与本仓 minimax / monkeycode 相同的 90s。
	DefaultIdleTimeout = 90 * time.Second
	// ClientVersionTTL 版本号的进程内缓存时长。**协议事实**：12 小时
	// （协议 §2.5 流程第一步）。
	ClientVersionTTL = 12 * time.Hour
)

// ---------------------------------------------------------------------------
// 兜底模型清单（协议 §6）
// ---------------------------------------------------------------------------

// staticModelIDs 静态兜底模型 id 全清单（19 条，顺序照抄协议 §6）。
//
// ⚠️ 兜底表的 contextWindow **统一 131072，是本地估计值、不是远端权威值**
// （协议 §6 明写）。因此 modelInfosOf 对兜底条目一律置 ContextFromAPI=false，
// 免得把估值冒充成「上游接口返回」。若上游未来下发目录接口，动态路径自然接管。
var staticModelIDs = []string{
	"deepseek-v4-flash",
	"deepseek-v4-pro",
	"MiniMax-M3",
	"MiniMax-M2.7",
	"qwen3.7-max",
	"qwen3.7-plus",
	"qwen3.6-plus",
	"qwen3.5-plus-2026-04-20",
	"kimi-k2.7-code",
	"kimi-k2.7-code-highspeed",
	"kimi-k2.6",
	"kimi-k2.5",
	"doubao-seed-2-1-pro-260628",
	"doubao-seed-2-1-turbo-260628",
	"doubao-seed-2-0-code-preview-260215",
	"glm-5.2",
	"glm-5.1",
	"glm-5v-turbo",
	"glm-5",
}

// staticContextWindow 兜底表的统一上下文窗口（本地估计值，见 staticModelIDs 注释）。
const staticContextWindow int64 = 131072
