// Package qwenwork 封装千问办公（gateway.qwenwork.cn + qwenwork.cn）上游协议，
// 实现 provider.Upstream 接口。与 qoder 渠道同属「Qoder 系 COSY 框架」但完全独立：
// 独立 Kind、独立 auth 文件前缀、独立模型表、独立鉴权组合（COSY + Bearer 双轨）。
//
// 协议要点（2026-09 实测，见 docs/千问办公QwenWork逆向对比备忘.md）：
//   - 推理/模型列表走 COSY 签名（RSA_PKCS1 包 16 字符 AES key + AES-128-CBC 身份 + MD5），
//     与 internal/qoder 的 cosy.go 同构；请求体明文 JSON（不带 Encode=1）。
//   - 余额/费率/账单走网页域 qwenwork.cn，纯 Bearer token（COSY 打它反而 401）。
//   - 推理必需 request_id / session_id；错误以 HTTP 200 + SSE 外层 statusCodeValue>=400 返回。
//   - 服务端无状态：多轮对话必须由客户端携带全量 messages（session_id 不承载上下文）。
//   - 每日 100 积分服务端 00:00（UTC+8）被动发放，无领取接口，无需保活。
package qwenwork

import (
	"wild-work/internal/provider"
)

// 上游域名与端点。
const (
	// GatewayBase 推理网关（COSY 签名端点）。
	GatewayBase = "https://gateway.qwenwork.cn"
	// WebBase 网页域（Bearer 端点：余额/费率/账单/套餐）。
	WebBase = "https://qwenwork.cn"

	EpChat      = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common"
	EpModels    = "/api/v2/model/list"          // COSY GET
	EpDTRefresh = "/api/v1/deviceToken/refresh" // COSY 域的 token 刷新（body {refresh_token,target:"c"}）
	EpBalance   = "/user/balance"               // Bearer GET
	EpUserInfo  = "/user/info"                  // Bearer GET（账号昵称兑底）
	EpWallets   = "/user/wallets"               // Bearer GET
	EpChatModes = "/api/chat-modes"             // Bearer GET（费率表与全量模型目录）
)

// ChannelName 费率面板中的渠道标识。
const ChannelName = "qwenwork"

// 上游模型目录标识（2026-09-24 起推理 body 必须携带，见 prepareChatBody 注释）。
// 与 Cosy-Business-Product / Cosy-Business-Type 静态头同源（官方 1.0.4 取值）。
const (
	BusinessProduct = "qoder_work"
	BusinessType    = "agent"
)

// staticModelKeys 客户端模型名 → 上游 model key。
// key 即 /api/v2/model/list 与 /api/chat-modes 的档位 key（两处 price_factor 一致）。
// 兼容旧 key 与客户端常见别名。
var staticModelKeys = map[string]string{
	"auto":                "pro",
	"qwork-advanced":      "pro",
	"pro":                 "pro",
	"qwork-auto":          "pro",
	"flash":               "flash",
	"qwork-lite":          "flash",
	"qwen4-max":           "qwen4-max",
	"qwen-4-max":          "qwen4-max",
	"qwen3.8-max":         "qwen3.8-max-preview",
	"qwen3.8-max-preview": "qwen3.8-max-preview",
	"qmodel_latest":       "qwen3.8-max-preview",
	"qwen3.8-flash":       "qwen3.8-flash",
	"ling-3.1-flash":      "ling-3.1-flash",
	"deepseek-flash":      "deepseek-flash",
	"deepseek-v4.1-flash": "deepseek-flash",
	"deepseek-v4-flash":   "deepseek-flash",
	"deepseek-chat":       "deepseek-flash",
	"deepseek-v4-pro":     "deepseek-v4-pro",
	"deepseek-reasoner":   "deepseek-v4-pro",
	"glm-5.3":             "glm-5.3",
	"glm-5.3-flash":       "glm-5.3-flash",
	"kimi-k3.1-pro":       "kimi-k3.1-pro",
	"kimi-k3.1":           "kimi-k3.1-pro",
	"kimi":                "kimi-k3.1-pro",
	"minimax-m3.1-flash":  "minimax-m3.1-flash",
	"minimax":             "minimax-m3.1-flash",
	"step-5-preview":      "step-5-preview",
	"step-5":              "step-5-preview",
}

// staticModels 静态兜底模型表（动态模型接口不可用时的 fallback）。
// priceFactor 为相对基准倍率（pro=1.0）；SupportsTools 实测支持（tool_calls 流式），
// SupportsImages / SupportsReasoning 按 /api/chat-modes 官方声明。
var staticModels = []provider.ModelInfo{
	{ID: "pro", Name: "高级", ContextWindow: 180_000, SupportsReasoning: true, SupportsTools: true},
	{ID: "flash", Name: "标准", ContextWindow: 180_000, SupportsImages: true, SupportsReasoning: true, SupportsTools: true},
	{ID: "qwen4-max", Name: "Qwen4-Max", ContextWindow: 180_000, SupportsImages: true, SupportsReasoning: true, SupportsTools: true},
	{ID: "qwen3.8-max-preview", Name: "Qwen3.8-Max", ContextWindow: 180_000, SupportsImages: true, SupportsReasoning: true, SupportsTools: true},
	{ID: "qwen3.8-flash", Name: "Qwen3.8-Flash", ContextWindow: 180_000, SupportsImages: true, SupportsReasoning: true, SupportsTools: true},
	{ID: "ling-3.1-flash", Name: "Ling-3.1-Flash", ContextWindow: 180_000, SupportsReasoning: true, SupportsTools: true},
	{ID: "deepseek-flash", Name: "DeepSeek-V4.1-Flash", ContextWindow: 180_000, SupportsImages: true, SupportsReasoning: true, SupportsTools: true},
	{ID: "deepseek-v4-pro", Name: "Deepseek-V4-Pro", ContextWindow: 180_000, SupportsReasoning: true, SupportsTools: true},
	{ID: "glm-5.3", Name: "GLM-5.3", ContextWindow: 180_000, SupportsReasoning: true, SupportsTools: true},
	{ID: "glm-5.3-flash", Name: "GLM-5.3-Flash", ContextWindow: 180_000, SupportsReasoning: true, SupportsTools: true},
	{ID: "kimi-k3.1-pro", Name: "Kimi-K3.1-Pro", ContextWindow: 180_000, SupportsImages: true, SupportsReasoning: true, SupportsTools: true},
	{ID: "minimax-m3.1-flash", Name: "MiniMax-M3.1-Flash", ContextWindow: 180_000, SupportsImages: true, SupportsReasoning: true, SupportsTools: true},
	{ID: "step-5-preview", Name: "Step-5-Preview", ContextWindow: 180_000, SupportsImages: true, SupportsReasoning: true, SupportsTools: true},
}

// StaticModels 返回静态兜底模型表副本（供 server.Runtime.StaticModels）。
func StaticModels() []provider.ModelInfo {
	return append([]provider.ModelInfo{}, staticModels...)
}

// ModelKey 返回客户端模型名对应的上游 model key；未知时原样返回（透传上游报错）。
func ModelKey(clientName string) string {
	if k, ok := staticModelKeys[clientName]; ok {
		return k
	}
	return clientName
}

// priceFactorOf 模型 key → 相对倍率（pro 基准 1.0）。
// 动态费率优先；此处仅静态兜底。
func priceFactorOf(key string) float64 {
	switch key {
	case "flash", "qwen3.8-flash", "ling-3.1-flash", "deepseek-flash":
		return 0.1
	case "deepseek-v4-pro", "glm-5.3-flash", "minimax-m3.1-flash":
		return 0.2
	case "step-5-preview":
		return 0.7
	case "pro":
		return 1.0
	case "glm-5.3":
		return 1.2
	case "qwen4-max":
		return 1.6
	case "qwen3.8-max-preview":
		return 1.8
	case "kimi-k3.1-pro":
		return 2.4
	}
	return 0
}
