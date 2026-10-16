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
//     ③ 伪装头齐套且**只能带被接受的那些**：User-Agent / x-opencode-client /
//     x-opencode-session / x-opencode-session-id / x-session-affinity / X-Session-Id /
//     裸 40 位小写 hex 的 x-opencode-project。**不得带 x-opencode-request**，
//     也不得用 prj_ 前缀的规范 project id（见 headered 与 R69）。
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

	// DefaultBase 键控车道基址（用户自备 sk-… key，OpenAI 兼容面）。
	//
	// 凭据为空（匿名 public）时**不用本常量**，见 AnonymousBase —— 上游
	// 2026-10-14 把无密钥推理迁到独立入口，两条车道的基址已分离。
	DefaultBase = "https://opencode.ai/zen/v1"

	// AnonymousBase 匿名免费档基址。
	//
	// 上游自 2026-10-14 起把 **keyless**（匿名 public）推理从 /zen 迁到 /inference。
	// 移值后果：user 报「测试恒 403 / 对话恒 403 FreeTierError」而模型目录一切正常
	// （两个基址的 GET /models 完全相同）。参考实现 opencode2api 的 anonymous
	// 车道走的也是 /inference（其 config 注释原文："Upstream moved keyless
	// inference off /zen onto its own host"）。
	//
	// 本仓无法从 CN/JP 出口复现该 403（约 56 次匿名调用全 200，含 /zen/v1），
	// 判断为上游按出口/后端金丝雀下发；换到 /inference 在两种情形下都安全，
	// 故跟随参考实现。见 R69。
	AnonymousBase = "https://opencode.ai/inference/v1"

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
