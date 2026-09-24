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
	"sort"

	"wild-work/internal/provider"
)

// 上游域名与端点。
const (
	// GatewayBase 推理网关（COSY 签名端点）。
	GatewayBase = "https://gateway.qwenwork.cn"
	// WebBase 网页域（Bearer 端点：余额/费率/账单/套餐）。
	WebBase = "https://qwenwork.cn"

	EpChat      = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common"
	EpChatPath  = "/algo/api/v2/service/pro/sse/agent_chat_generation" // 签名用 path（去 query）
	EpModels    = "/api/v2/model/list"                                 // COSY GET
	EpDTRefresh = "/api/v1/deviceToken/refresh"                        // COSY 域的 token 刷新（body {refresh_token,target:"c"}）
	EpBalance   = "/user/balance"                                      // Bearer GET
	EpUserInfo  = "/user/info"                                         // Bearer GET（账号昵称兑底）
	EpWallets   = "/user/wallets"                                      // Bearer GET
	EpBillings  = "/user/billings?source=all"                          // Bearer GET
	EpChatModes = "/api/chat-modes"                                    // Bearer GET（费率表）
	EpAcctCtx   = "/user/v2/account-context?include=user,plan,quota"   // Bearer GET
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
// 兼容旧 key：qwork-advanced 实测仍被网关接受（路由 glm-5.2）。
var staticModelKeys = map[string]string{
	"auto":                "pro",
	"qwork-advanced":      "pro",
	"pro":                 "pro",
	"qwork-auto":          "pro",
	"flash":               "flash",
	"qwork-lite":          "flash",
	"qwen3.8-max":         "qwen3.8-max-preview",
	"qwen3.8-max-preview": "qwen3.8-max-preview",
	"qmodel_latest":       "qwen3.8-max-preview",
}

// staticModels 静态兜底模型表（动态模型接口不可用时的 fallback）。
// priceFactor 为相对基准倍率（pro=1.0）；SupportsTools 实测支持（tool_calls 流式），
// SupportsImages 按 /api/chat-modes 声明（flash/qwen3.8-max 为 is_vl=true）。
var staticModels = []provider.ModelInfo{
	{ID: "pro", Name: "高级", ContextWindow: 180_000, SupportsTools: true},
	{ID: "flash", Name: "标准", ContextWindow: 180_000, SupportsImages: true, SupportsTools: true},
	{ID: "qwen3.8-max-preview", Name: "Qwen3.8-Max", ContextWindow: 180_000, SupportsImages: true, SupportsTools: true},
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
	case "flash":
		return 0.1
	case "pro":
		return 1.0
	case "qwen3.8-max-preview":
		return 1.8
	}
	return 0
}

// sortStrings 排序副本（避免只为展示顺序引入额外状态）。
func sortStrings(ss []string) []string {
	out := append([]string{}, ss...)
	sort.Strings(out)
	return out
}
