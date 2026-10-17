// constants.go Kilo AI 免费池渠道的常量。
//
// 渠道特性（2026-10-17 依据 dsh-our-free-model 的 src/kilo.js 142 行只读分析得出）：
//   - **免密**：网关的免费池不需要任何凭据——没有 Authorization、没有 key、
//     也没有指纹头。这是本仓所有渠道里启动成本最低的一条。
//   - **免费集合由上游声明**：模型清单里 `isFree === true` 的行才是本渠道的
//     服务范围；请求付费 id 会得到 `PAID_MODEL_AUTH_REQUIRED`（凭据问题，不是
//     请求问题），故不在清单侧按模型名猜「哪个像免费」。
//   - **无账号**：没有登录、没有 token 轮换、没有签到、没有积分——与 oczen
//     同属「无账号渠道」形态（R16）：不建 auth 加载器、不进 reloadAccounts、
//     由 main 注入唯一虚拟账号。
//   - **SSE 注释行**：上游在排队时下发 `: KILO PROCESSING` 这类注释行来保活。
//     internal/upstream 的通用 SSE 解析会跳注释行，故无需在渠道侧特判；
//     但**绝不能**把它当成 idle 超时的证据（它恰恰证明连接活着）。
//
// 为什么**不**预先接备用上游通道（backuplink）：本仓与参考实现都没有观测到该
// 网关的地区限制（没有 403 RegionError 记录）。按 R57「备用通道默认不参与任何
// 请求，仅渠道按需取用」，在没有证据的情况下接上它，等于把流量默认发给第三方
// 出口——那是比「暂时不可用」更坏的默认值。若实测出现地区拒绝，再按 R62 的
// backuplink.Replay 编排补上（oczen 与 cline 是现成范式）。
package kilo

import (
	"time"

	"wild-work/internal/provider"
)

const (
	// Kind 渠道标识，同时是模型名前缀（客户端填 `kilo/<上游模型ID>`）。
	Kind = provider.Kilo

	// DefaultBase Kilo AI 网关基址（OpenAI 兼容面）。
	// 参考实现允许用环境变量覆盖以便离线测试；本仓用 Client.Base 注入假上游。
	DefaultBase = "https://api.kilo.ai/api/gateway"

	// AnonymousUID 虚拟账号 UID。整池只有这一个，不可增删停用。
	AnonymousUID = "kilo-anonymous"

	// AnonymousName 面板展示名。渠道名由前端 badge 渲染，此处只描述凭证形态。
	AnonymousName = "免费池"

	// userAgent 出站 UA。上游不校验指纹（参考实现发的是自己的包名），
	// 这里如实标识本工具，便于上游侧排查异常流量。
	userAgent = "wild-work"

	// noExpiry 虚拟账号的过期时刻（2100-01-01）。
	// 必须设成远期值：auth.NeedsRefresh 对 ExpiresAt <= 0 恒返回 true，
	// 会让 server 与定价刷新路径反复调用 RefreshToken 并冷却这个唯一账号。
	noExpiry int64 = 4102444800

	// requestTimeout 非流式请求（模型清单等）的总超时。
	requestTimeout = 2 * time.Minute

	// listTimeout 模型清单拉取的上限（参考实现 15s，这里留出国内出口余量）。
	listTimeout = 20 * time.Second
)
