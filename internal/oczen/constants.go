// constants.go OpenCodeZen 匿名免费通道的常量与静态模型目录。
//
// 渠道特性（2026-09 实测，2026-11 随上游闸门更新，见 R69/R126/R152/R154/R155）：
//   - 匿名凭证是字面量 "public"（Authorization: Bearer public），无需登录、无 token 轮换；
//   - 上游免费档按「官方 OpenCode 2.x CLI 会话」判定（闸门）：
//     ① 会话头须匹配官方客户端外形 ses_<12位小写hex><14位Base62>，其 12 位 hex
//     时间戳段须是官方 Identifier.descending() 编码（~(unix_ms<<12|counter) 取低
//     48 位，故末三位为 0xFFF-counter，实测只接受 counter 1–63），且时间锚必须
//     落在「今日 −30 天」窗口内（超窗回 429 FreeUsageLimitError）。摘要段/升序 ID
//     即便外形正确也被拒（2026-10-13 实盘）。
//     ② 请求体须整形为官方 CLI 会话：官方 2.2.5 system 正文（prompt.go，内嵌抓包
//     原文）、官方 **12** 工具全集（assets.go）、客户端工具全部改名成 `clint_*`
//     （tools.go），并带 `stream_options.include_usage` 与 `max_completion_tokens`
//     （shape.go）。
//     ③ 伪装头齐套且**只能带被接受的那些**：v2 UA / x-opencode-client /
//     x-opencode-session / x-opencode-session-id / x-session-affinity / X-Session-Id /
//     裸 40 位小写 hex 的 x-opencode-project（每请求随机）/ `Accept: */*`。
//     **不得带 x-opencode-request**，project 也不得用 prj_ 前缀
//     （见 headered 与 R69/R154）。
//   - 匿名通道的免费额度按「出口 IP × 天」记账（429 的 Retry-After 恒指向下一个
//     UTC 零点）；**但恒 429 的原因不是额度**：2026-11-04 实测（R155）同一台机器、
//     同一出口、同一匿名免费档，官方 2.2.5 CLI 多次 200 并记真实 usage，而同一份
//     请求体换成 Go/Node/Chrome 的 ClientHello 一律 429。
//     闸门的真判据是 **TLS ClientHello 指纹**：必须与官方 CLI（Bun 1.4.4/BoringSSL）
//     一致，见 tlsfp.go —— 整形层（②③）只是形态对齐，不改变放行与否。
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

	// AnonymousBase 匿名免费档基址（模型目录与备用端点用的族根）。
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

	// AnonymousChatURL 匿名车道的对话端点。
	//
	// 与 AnonymousBase（目录用的 /inference/v1 族根）**不是同一路径**：v2 客户端
	// 的对话走 `/inference/openai/v1/chat/completions`（参考实现
	// opencode2api `gateway` 的 v2 端点表，R152）。混用会让请求落到 v1 车道。
	AnonymousChatURL = "https://opencode.ai/inference/openai/v1/chat/completions"

	// AnonymousKey 匿名免费凭证：字面量 "public"，无需注册。
	// 来源：opencode2api gateway.go anonymousZenKey。
	AnonymousKey = "public"

	// AnonymousUID 虚拟账号 UID。仅有一个，不可增删停用。
	AnonymousUID = "oczen-anonymous"

	// AnonymousName 面板展示名：仅凭证形态（匿名/私有Key），
	// 渠道名由前端 badge 渲染（避免重复「OpenCodeZen OpenCodeZen」）。
	AnonymousName = "匿名"

	// userAgent 官方 OpenCode 2.x CLI 的 UA（`opencode/<channel>/<version>/cli`）。
	//
	// 版本取 2026-11-01 时 npm 上最新的 2.2.5（magpie 0.1.24 抓包同值；官方
	// 客户端会自我更新，本仓不联网取版本，属**快照**）。上游 2026-10-26 起对
	// 「旧客户端的原样 UA」（1.19.0）直接回 429，故版本段落后需要复核。
	userAgent = "opencode/latest/2.2.5/cli"

	// noExpiry 虚拟账号的过期时刻（2100-01-01）。
	// 必须设成远期值：auth.NeedsRefresh 对 ExpiresAt<=0 恒返回 true，
	// 会让 server/定价刷新路径反复调用 RefreshToken 并冷却账号（匿名通道无 token 可刷）。
	noExpiry int64 = 4102444800

	// requestTimeout 单次对话请求超时上限。
	requestTimeout = 10 * time.Minute
)

// stubToolNames 已删除：v1 车道的 bash/read 两个占位桩工具被官方 13 工具全集
// 取代（assets.go / shape.go，R152）。
