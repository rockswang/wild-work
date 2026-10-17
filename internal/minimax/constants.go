// constants.go MiniMax Code（中国版）渠道的静态常量。
//
// 协议依据：本地文档 docs/ref-ofm-渠道-lobsterai-minimax.md 的「渠道 ② MiniMax Code
// （中国版）」整章（§1–§9，逐条标注了参考实现的文件行号）。本文件只放**非敏感**常量：
// 主机、端点、OAuth 客户端参数、超时与静态兜底模型表。凭据（access_token / refresh_token，
// 前缀 mmoat_ / mmort_）一律来自 auths/minimax-*.json，**不得**出现在这里（AGENTS.md §0.2）。
package minimax

import (
	"os"
	"strings"
	"time"

	"wild-work/internal/provider"
)

// 渠道标识与上游主机（协议 §1.1）。
const (
	// Kind 渠道标识，直接复用 provider.Minimax。
	//
	// 为什么不写字面量 "minimax"：provider.Minimax 是模型前缀路由（provider.PublicModelID /
	// SplitModelID）、BuiltinKinds 与 stats 渠道白名单的共同真源（R10 / R52），
	// 本包再抄一份字符串就会与那个真源漂移。
	Kind provider.Kind = provider.Minimax

	// ProviderID 远端模型目录里本渠道的 providerId（协议 §6：在顶层 providers[] 里找它）。
	// 与 Kind 同值但**语义不同**：前者是本工具的渠道名，后者是上游目录的字段值。
	ProviderID = "minimax"

	// AccountHost 账号 / OAuth 主机（设备码、令牌、续期、撤销）。
	AccountHost = "https://account.minimax.cn"
	// APIHost API 主机（模型目录 / 推理 / 签到 / 积分）。
	APIHost = "https://agent.minimax.cn"

	// Region / BuildEnv 模型目录接口的定位参数（协议 §1.1）。
	Region   = "cn"
	BuildEnv = "prod"
)

// OAuth 客户端常量（协议 §2）：**全部是请求体字段，不是请求头**。
// 导出是为了让登录编排（设备码流程）与续期共用同一份取值，避免两处漂移。
const (
	OAuthClientID = "mcode-public"
	OAuthAudience = "agent-backend"
	OAuthScope    = "agent.default"
)

// 端点路径（协议 §1.2）。
const (
	EpDeviceCode = "/oauth2/device/code"
	EpToken      = "/oauth2/token"
	// EpRevoke 撤销端点：常量在参考实现里已定义，但其调用点**未确认**
	// （协议 §1.2 ⚠️ 与 §9：`minimax*.ts` 范围内检索未见使用）。本包不实现登出流程。
	EpRevoke        = "/oauth2/revoke"
	EpModels        = "/mavis/api/v1/models"
	EpMessages      = "/mavis/api/v1/llm/v1/messages"
	EpSigninStatus  = "/minimax-cloud/api/v1/signin/status"
	EpSigninClaim   = "/minimax-cloud/api/v1/signin/claim"
	EpCreditDetails = "/minimax-cloud/api/v1/credit/details"
)

// 超时（协议 §1.2 末行）：业务 30 s、OAuth 20 s。
const (
	requestTimeout = 30 * time.Second
	oauthTimeout   = 20 * time.Second
)

// DefaultIdleTimeout 流式空闲看门狗阈值（只拦「真的一个字节都不出」，不掐长思考）。
//
// ⚠️ 协议文档**没有**给出该值（参考实现里不存在空闲看门狗这一概念），
// 取与本仓 monkeycode 相同的 90 s，属本工具自有的工程参数，不是上游协议事实。
// 空闲看门狗只服务于「上游中途卡死」这一形态，与 §5 的截断判定互补（R58）。
const DefaultIdleTimeout = 90 * time.Second

// 签到领取结果（协议 §7.1，取自参考实现 asar 常量名 SigninClaimResult）。
//
// ⚠️ **幂等判据是这里的 claim_result，不是 HTTP 状态码**：重复领取同样回 HTTP 200
// （`minimax-credits.ts:358-389`）。判据错位会让「今天已领过」被当成失败而反复重领。
const (
	SigninClaimClaimed        = 1 // 本次真领取
	SigninClaimAlreadyClaimed = 2 // 今日已领取（重复领取）
)

// TimezoneID 签到 query 里的 IANA 时区名（`?timezone_id=`，协议 §7.1）。
//
// 参考实现取 `Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC"`；Go 侧没有
// 等价的浏览器 API，故按「显式注入 → TZ 环境变量 → time.Local 的 IANA 名 → "UTC"」
// 依次回退（见 timezoneID）。装配时可由上层按配置注入，避免本地探测不准。
var TimezoneID string

// timezoneID 返回生效的时区名。
//
// ⚠️ 已知限制：Windows 上 Go 的 time.Local 名恒为 "Local"（不是 IANA 名），
// 因此本机会回退到 "UTC" —— 与参考实现的兜底值一致，但签到的「今日」边界会按 UTC
// 划分（对 UTC+8 用户即每天 08:00 换日）。需要精确到本地日期时，
// 由上层给 TimezoneID 注入真实 IANA 名（如 "Asia/Shanghai"）。
//
// 「query 参数生效、请求头无效、不带参数/非法时区名都会报错」是协议 §7.1 实测过的
// 三种组合，故这里只保证「参数存在且形态像 IANA 名」。
func timezoneID() string {
	if s := strings.TrimSpace(TimezoneID); s != "" {
		return s
	}
	if s := strings.TrimSpace(os.Getenv("TZ")); s != "" {
		return s
	}
	if time.Local != nil {
		if n := time.Local.String(); n != "" && n != "Local" && strings.Contains(n, "/") {
			return n
		}
	}
	// 参考实现同款兜底（`|| "UTC"`）：宁可让上游按 UTC 划分日期，也不发一个非法值
	// （非法时区名同样 HTTP 200，但业务码非 0，会被判失败）。
	return "UTC"
}
