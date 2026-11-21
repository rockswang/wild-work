// Package modelscope 魔搭社区（ModelScope）API-Inference 渠道，实现 provider.Upstream。
//
// 两个内建 kind 对应**两个独立服务区域**：
//
//	modelscope   → api-inference.modelscope.cn（国内站）
//	modelscopeai → api-inference.modelscope.ai（国际站）
//
// **两区 token 不通用**：凭据是一枚 `ms-` Access Token（一枚 = 一个账号，真多 token 池），
// 由面板手工粘贴（票 04）。不做 CDP、不做 cookie 换 token。
//
// 上游协议是 OpenAI 兼容对话面 + **异步**文生图/图生图（submit 拿 task_id → 轮询），
// 与 glm 的同步生图面完全不同。额度面是主站的「魔粒」（magicubes）：余额与今日用量
// 与对话面**不在同一个 host**（见 credits.go），按 provider.UserResourceDetail 接入
// 积分体系（R17 双流水）。
package modelscope

import (
	"time"

	"wild-work/internal/provider"
)

const (
	// DefaultBaseCN 国内站 API-Inference 基址（**不含** /v1：路径由各端点自己拼，
	// 如 /v1/chat/completions、/v1/models、/v1/tasks/{id}）。
	DefaultBaseCN = "https://api-inference.modelscope.cn"
	// DefaultBaseAI 国际站 API-Inference 基址（同上，不含 /v1）。
	DefaultBaseAI = "https://api-inference.modelscope.ai"

	// requestTimeout 无 config 注入时的非流式总超时。
	// 生产由 main 用 upstream.timeout_seconds 覆盖（与其它渠道同一口径）。
	requestTimeout = 120 * time.Second
)

// BaseURL 按渠道 kind 选基址：两个 kind 各自独立服务，绝不互相兜底
// （token 不通用，兜底只会换来一个误导性的 401）。
func BaseURL(kind provider.Kind) string {
	if kind == provider.ModelScopeAI {
		return DefaultBaseAI
	}
	return DefaultBaseCN
}

// FarFutureExpiry `ms-` Access Token 无过期时间字段（失效只能靠上游 401 发现）。
// ExpiresAt 必须给远期值而**不得为 0**：0 会让 auth.NeedsRefresh 恒真
// ⇒ 每次请求都尝试刷新（本渠道没有 refresh 端点），换来无谓的冷却（不变量 21 同款陷阱）。
// 写入点在票 04 的粘贴登录路径（与 comate/lingxi 同款）。
func FarFutureExpiry() int64 { return time.Now().AddDate(80, 0, 0).Unix() }
