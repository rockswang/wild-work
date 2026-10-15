// constants.go OpenCodeZen 匿名免费通道的常量与静态模型目录。
//
// 渠道特性（2026-09 实测）：
//   - 匿名凭证是字面量 "public"（Authorization: Bearer public），无需登录、无 token 轮换；
//   - 上游免费档有三道校验，缺一即 403 FreeTierError：
//     ① 会话头不仅须匹配官方客户端外形 ses_<12位小写hex><14位Base62>，其 12 位
//     hex 时间戳段还须是官方 Identifier.descending() 编码（~(unix_ms<<12|counter)
//     取低 48 位，故末三位为 0xFFF-counter，实测只接受 counter 1–63）；
//     摘要段/升序 ID 即便外形正确也被拒（2026-10-13 实盘）。
//     ② 请求体须为「智能体形态」：stream=true 且 tools 内同时含 bash 与 read 两个 function；
//     ③ 伪装头齐套（User-Agent / x-opencode-client / x-session-affinity / x-opencode-request / x-opencode-project）。
//   - 匿名通道未观测到请求数配额（1900+ 次、峰值 230 req/min、64 并发均无 429），
//     唯一约束是并发升高时的延迟背压（排队不拒绝）。
package oczen

import (
	"time"

	"wild-work/internal/provider"
)

const (
	// Kind 渠道标识，同时是模型名前缀。
	Kind = provider.Oczen

	// DefaultBase 上游 OpenCode Zen 基址（OpenAI 兼容面）。
	DefaultBase = "https://opencode.ai/zen/v1"

	// AnonymousKey 匿名免费凭证：字面量 "public"，无需注册。
	// 来源：opencode2api gateway.go anonymousZenKey。
	AnonymousKey = "public"

	// AnonymousUID 虚拟账号 UID。仅有一个，不可增删停用。
	AnonymousUID = "oczen-anonymous"

	// AnonymousName 面板展示名：仅凭证形态（匿名/私有Key），
	// 渠道名由前端 badge 渲染（避免重复「OpenCodeZen OpenCodeZen」）。
	AnonymousName = "匿名"

	// userAgent 伪装 OpenCode CLI 的 UA（上游 1.18.x 实测可通过）。
	userAgent = "opencode/1.18.31 (windows amd64; node22)"

	// noExpiry 虚拟账号的过期时刻（2100-01-01）。
	// 必须设成远期值：auth.NeedsRefresh 对 ExpiresAt<=0 恒返回 true，
	// 会让 server/定价刷新路径反复调用 RefreshToken 并冷却账号（匿名通道无 token 可刷）。
	noExpiry int64 = 4102444800

	// requestTimeout 单次对话请求超时上限。
	requestTimeout = 10 * time.Minute
)

// stubToolName 免费档校验要求的两个 function 工具名（大小写敏感，实测仅小写可通过）。
var stubToolNames = [2]string{"bash", "read"}
