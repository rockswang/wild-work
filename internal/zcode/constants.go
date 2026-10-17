// Package zcode 实现智谱 ZCode（z.ai）渠道，实现 provider.Upstream。
//
// # 协议依据
//
// `docs/ref-ofm-渠道-zcode-gemini.md` 的「渠道一：ZCode」整章（§1.1–§1.9）。
// 该文档由参考实现（`dsh-our-free-model/vendor/channel-pack/src/zcode*.ts`）静态阅读得出，
// 凡标注「实测」的条目按「源码声称的实测事实」采纳。
//
// # 本渠道的三个形态特征
//
//  1. **Anthropic Messages 形态**（不是 OpenAI 兼容）：出站转换见 messages.go，
//     入站 Anthropic SSE → OpenAI SSE 见 sse.go；
//  2. **请求体内容准入**：顶层 `system` 必须内嵌官方身份块，否则上游回
//     HTTP 405 + `{"code":3012}`（见 identity.go）。这是**准入门槛，不是业务提示词**；
//  3. **凭据不可续期**：`zcode_jwt` 的 JWT payload 无 `exp`，上游也没有 refresh 端点
//     （参考实现 `ZCODE_REFRESHABLE = false`）⇒ RefreshToken 是**空实现**，
//     `ExpiresAt` 由登录包写成**远期值**（否则 NeedsRefresh 恒真 → 反复空转刷新 + 账号冷却）。
//
// # 未实现项（**刻意不做，且注释里写明原因，绝不静默失败**）
//
//  1. **积分领取 `POST /api/v1/zcode-plan/billing/claim` 不实现**：参考实现的该路径依赖
//     **阿里云验证码网页 SDK（`AliyunCaptcha.js`）+ headful chromium（CDP 驱动）+ 真实
//     origin 载体页**，且**每个 plan 都必须现 mint 一个一次性 captcha param**
//     （复用必得 `3007`）。这在 Go 侧无法纯静态移植（详见协议 §1.2.5 与 §1.9 的可移植性表）。
//     故 `DailyCheckin` 返回明确的「本渠道不支持自动领取」错误（ErrClaimUnsupported），
//     **不伪造成功**；本渠道因此按「无显式签到」渠道对待（接线时进 noExplicitCheckin 名单）。
//  2. **客户端活跃上报 `POST /api/v1/event/report` 不实现**：它只服务于 `billing/preview`
//     的「补激活 → 计划才出现」这一段（协议 §1.7.2），即**只服务于领取路径**；
//     领取既然不可用，上报就是**零收益的上游写入**，故不发（见 client.go previewPlans 注释）。
//  3. **`coding-plan` 订阅腿的换 key 三步 GET 不实现**：协议 §1.9 判定它可静态移植，但
//     本渠道的登录流程**不会**由上游下发 `coding_plan_key_*`（要靠登录后的
//     `fetchCodingPlanApiKey` 换取），而该换取路径在参考实现里对「从未建过 key 的账号」
//     会退化成官方客户端才做的 POST 建 key（本实现不复刻）。本包只**消费**已有的
//     `coding_plan_key_*`（Extra 里若存在则走订阅腿），不主动换取。
package zcode

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 渠道标识与上游主机（协议 §1.1）。
const (
	// Kind 渠道标识，直接复用 provider.ZCode。
	//
	// 为什么不写字面量 "zcode"：provider.ZCode 是模型前缀路由（provider.PublicModelID /
	// SplitModelID）、BuiltinKinds 与 stats 渠道白名单的共同真源（R10 / R52），
	// 本包再抄一份字符串就会与那个真源漂移。
	Kind provider.Kind = provider.ZCode

	// Origin 平台 origin：积分制（start-plan）对话端点、额度、目录、登录全在这里。
	Origin = "https://zcode.z.ai"

	// CodingPlanOrigin 订阅制（coding-plan）网关 origin。
	// 只有订阅腿走它，且**不带 HTTP-Referer**（协议 §1.2.3 实测：该网关只认
	// Authorization + anthropic-version）。
	CodingPlanOrigin = "https://api.z.ai"

	// AppVersionFallback 上游未下发 app_version 时的兜底版本（协议 §1.2.1）。
	AppVersionFallback = "3.14.3"

	// 身份头族的固定取值（协议 §1.2.1）。**逐字固定**，不做平台自适应：
	// 上游把「像官方 win32 客户端」作为来源标识的一部分（是否参与 3012 判定已证伪，
	// 但它们仍是硬需求的一部分，例如缺 X-Device-Mid 必回 400 code 3001）。
	Platform       = "win32"
	OSCategory     = "windows"
	ReleaseChannel = "stable"
	ClientLanguage = "zh-CN"
	ClientTimezone = "Asia/Shanghai"
	// AnthropicVersion Anthropic 协议版本头（本渠道是 Anthropic Messages 形态）。
	AnthropicVersion = "2023-06-01"

	// UserAgentPrefix User-Agent 前缀：完整值是 `ZCode/<appVersion>`。
	UserAgentPrefix = "ZCode/"

	// Domain 写进 auth 文件的 domain 字段（与 minimax-"minimax.cn" 同口径）。
	Domain = "zcode.z.ai"
)

// 端点路径（协议 §1.1 / §1.2.2）。
const (
	// EpZCodePlanMessages 积分制（start-plan）对话端点，拼在 Origin 后。
	EpZCodePlanMessages = "/api/v1/zcode-plan/anthropic/v1/messages"

	// CodingPlanMessagesURL 订阅制（coding-plan）对话端点（完整 URL，另一台网关）。
	CodingPlanMessagesURL = CodingPlanOrigin + "/api/anthropic/v1/messages"

	// EpBalance 额度余额（**需要 Authorization**，缺则 401；且需要 X-Device-Mid，缺则 400 code 3001）。
	EpBalance = "/api/v1/zcode-plan/billing/balance"
	// EpPreview 可领活动预览（**不需要 Authorization**，但需要 X-Device-Mid）。
	EpPreview = "/api/v1/zcode-plan/billing/preview"
	// EpClientConfigs 客户端配置（模型目录 builtinModels 的来源）。
	// ⚠ query 的 `platform` **必须是 `unknown`**：实测 win32/win64/windows/electron/
	// desktop/linux 一律 400 code 3001（协议 §1.2.2）。
	EpClientConfigs = "/api/v1/client/configs"
	// EpEventReport 客户端活跃上报——**不实现**（只服务于领取路径，见包注释未实现项 2）。
	EpEventReport = "/api/v1/event/report"
	// EpClaim 领取端点——**不实现**（依赖阿里云验证码 + headful 浏览器，见包注释未实现项 1）。
	EpClaim = "/api/v1/zcode-plan/billing/claim"
)

// 请求头名常量。集中一处便于「头族齐全」的回归测试逐项断言（协议 §1.2.1 的 10 个固定头）。
const (
	HdrUserAgent        = "User-Agent"
	HdrReferer          = "HTTP-Referer"
	HdrAppVersion       = "X-ZCode-App-Version"
	HdrReleaseChannel   = "X-Release-Channel"
	HdrClientLanguage   = "X-Client-Language"
	HdrClientTimezone   = "X-Client-Timezone"
	HdrDeviceMid        = "X-Device-Mid"
	HdrPlatform         = "X-Platform"
	HdrOSCategory       = "X-Os-Category"
	HdrAnthropicVersion = "anthropic-version"
	HdrContentType      = "Content-Type"
	HdrAuthorization    = "Authorization"
)

// requiredHeaderNames 返回**所有出站请求都必须齐全**的固定头（协议 §1.2.1 的 10 个）。
// 不含 Authorization / Content-Type：前者按通道与端点取不同凭据（部分端点甚至不需要，
// 如 preview），后者只在有请求体时才有意义。
//
// 这份清单只服务于测试与排障（「头族是否齐全」是 R71 附带的实测纪律在本渠道的落地：
// 缺 X-Device-Mid 会直接 400 code 3001）。
func requiredHeaderNames() []string {
	return []string{
		HdrUserAgent, HdrReferer, HdrAppVersion, HdrReleaseChannel, HdrClientLanguage,
		HdrClientTimezone, HdrDeviceMid, HdrPlatform, HdrOSCategory, HdrAnthropicVersion,
	}
}

// 超时参数。
const (
	// requestTimeout 业务端点（额度 / 目录）的总超时。
	requestTimeout = 30 * time.Second

	// DefaultIdleTimeout 流式空闲看门狗阈值：逐字对齐官方
	// `modelStream.idleTimeoutMs = 6e5`（协议 §1.5 的「空闲超时」行）。
	//
	// 语义：连续该时长**读不到任何字节**才判上游卡死。参考实现另有「每帧 +30s 增量」与
	// 「整轮 15 分钟墙钟」，后者只兜「真静默 / captcha 卡死」——本仓的等价物就是
	// provider.NewIdleReader（per-frame 空闲），不做整轮墙钟（R35：流式请求不设总超时）。
	DefaultIdleTimeout = 600 * time.Second
)

// idleTimeoutEnv 空闲超时的环境变量覆盖名（与参考实现同名，便于排障时对齐口径）。
const idleTimeoutEnv = "DSH_ZCODE_SSE_IDLE_TIMEOUT_MS"

// kindPrefix 模型 ID 的渠道前缀（`zcode/<上游模型名>`）。
const kindPrefix = "zcode/"

// zcodeHeaders 构造 ZCode 的**统一请求头族**（协议 §1.2.1）。
//
// authorization 为空则不设 Authorization（preview 端点不需要它）；
// withJSON 为 true 才设 Content-Type（GET 端点带上它虽无害，但参考实现的
// `json: false` 形态刻意不带，这里照抄以免多一个可变维度）。
//
// ⚠ 与其它渠道的关键差别：**鉴权不是一个头**，而是「头族齐全」——
//   - 缺 Authorization：balance 回 **401**；
//   - 缺 X-Device-Mid：balance / preview / 对话端点回 **400 {"code":3001}**。
func zcodeHeaders(a *auth.Auth, authorization string, withJSON bool) http.Header {
	h := http.Header{}
	version := appVersionOf(a)
	h.Set(HdrUserAgent, UserAgentPrefix+version)
	h.Set(HdrReferer, Origin)
	h.Set(HdrAppVersion, version)
	h.Set(HdrReleaseChannel, ReleaseChannel)
	h.Set(HdrClientLanguage, ClientLanguage)
	h.Set(HdrClientTimezone, ClientTimezone)
	h.Set(HdrDeviceMid, deviceMidOf(a))
	h.Set(HdrPlatform, Platform)
	h.Set(HdrOSCategory, OSCategory)
	h.Set(HdrAnthropicVersion, AnthropicVersion)
	if withJSON {
		h.Set(HdrContentType, "application/json")
	}
	if strings.TrimSpace(authorization) != "" {
		h.Set(HdrAuthorization, authorization)
	}
	return h
}

// appVersionOf 取凭据里记录的官方客户端版本，缺失时用兜底值。
func appVersionOf(a *auth.Auth) string {
	if a != nil && a.Extra != nil {
		if v := strings.TrimSpace(a.Extra["app_version"]); v != "" {
			return v
		}
	}
	return AppVersionFallback
}

// deviceMidOf 取 `X-Device-Mid`。
//
// 首选凭据里的 `device_mid`（登录时随机生成的 UUIDv4，协议 §1.8：值**不被服务端绑定校验**，
// 只需「稳定」）。
//
// 兜底：凭据没有该字段时（手工建的账号文件、或早期版本导入的账号），按
// `sha256(user_id + "\x00" + access_token)` 派生一个**确定性**的 UUIDv4 形态值，而不是
// 现场随机——随机值会让「同一账号每进程一个设备标识」，服务端的用量归集会看起来像
// 不断换设备。确定性派生保证：同一凭据跨进程稳定、不写盘、不改写凭据文件。
//
// ⚠ 为什么不在这里生成随机值并落盘：渠道包不应在**请求路径**里改写凭据文件
// （并发请求会互相覆盖，且会引入「读请求顺手写盘」的隐藏副作用）。正式凭据一律由
// login_zcode 在登录时写好 device_mid。
func deviceMidOf(a *auth.Auth) string {
	if a == nil {
		return fallbackDeviceMid("", "")
	}
	if a.Extra != nil {
		if v := strings.TrimSpace(a.Extra["device_mid"]); v != "" {
			return v
		}
	}
	return fallbackDeviceMid(a.UID, a.AccessTokenValue())
}

// fallbackDeviceMid 由账号标识与凭据派生一个 UUIDv4 形态的稳定 mid（见 deviceMidOf）。
func fallbackDeviceMid(uid, token string) string {
	sum := sha256Sum(uid + "\x00" + token)
	// 版本位（第 13 个 hex 位固定 4）与变体位（第 17 个 hex 位取 8/9/a/b）按 RFC 4122 摆好，
	// 使其形态与官方 `randomUUID()` 一致（虽不被校验，但保持形态一致没有任何代价）。
	hexs := []byte(sum)
	hexs[12] = '4'
	switch hexs[16] {
	case '8', '9', 'a', 'b', 'c', 'd', 'e', 'f':
	default:
		hexs[16] = '8'
	}
	return string(hexs[0:8]) + "-" + string(hexs[8:12]) + "-" + string(hexs[12:16]) + "-" +
		string(hexs[16:20]) + "-" + string(hexs[20:32])
}

// idleTimeout 生效的空闲超时：环境变量（毫秒）覆盖 → 默认值。
func idleTimeout() time.Duration {
	if raw := strings.TrimSpace(os.Getenv(idleTimeoutEnv)); raw != "" {
		if ms, err := strconv.ParseInt(raw, 10, 64); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	return DefaultIdleTimeout
}
