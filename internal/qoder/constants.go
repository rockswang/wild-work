// Package qoder 封装 QoderWork（qoder.com.cn）上游协议：COSY 签名、
// QoderEncoding 编码、嵌套 SSE 解析，并实现 provider.Upstream 接口。
// 移植自 qoderwork2api（Sliverkiss），凭证模型改为 internal/auth.Auth。
package qoder

import (
	"sort"

	"wild-work/internal/provider"
)

// 上游域名与端点（CN only）。
const (
	OpenAPIBase = "https://openapi.qoder.com.cn" // 业务 API（dt- Bearer，无签名）
	GatewayBase = "https://gateway.qoder.com.cn" // 推理网关（COSY 签名）

	EpQuotaUsage = "/api/v2/quota/usage"
	EpCheckinSt  = "/sash/api/v1/me/daily-check-in/status"
	EpCheckinCl  = "/sash/api/v1/me/daily-check-in/claim"
	EpPlan       = "/api/v2/user/plan"
	EpUserInfo   = "/api/v1/userinfo"
	EpDTRefresh  = "/api/v1/deviceToken/refresh"
	EpModels     = "/algo/api/v2/model/list?Encode=1"
	EpChat       = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"

	clientUA = "Go-http-client/2.0"
)

// staticModelKeys 客户端模型名 → 上游 model key 静态兜底表。
// 与 qoderwork2api fallbackModelMap 对齐；优先用动态模型接口（COSY /algo/api/v2/model/list）。
var staticModelKeys = map[string]string{
	"auto":              "auto",
	"qwen3.8-max":       "qmodel_38max",
	"qwen3.7-max":       "qmodel_latest",
	"qwen3.7-plus":      "qmodel",
	"qwen3.7-flash":     "q37fmodel",
	"qwen3.6-flash":     "q36fmodel",
	"deepseek-v4-pro":   "dmodel",
	"deepseek-v4-flash": "dfmodel",
	"glm-5.3":           "gmodel",
	"glm-5.2":           "gm51model",
	"kimi-k2.7-code":    "kmodel",
	"minimax-m2.7":      "mmodel",
}

// StaticModels 返回 provider.ModelInfo 静态兜底（供 server.Runtime.StaticModels）。
func StaticModels() []provider.ModelInfo {
	keys := make([]string, 0, len(staticModelKeys))
	for k := range staticModelKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]provider.ModelInfo, 0, len(keys))
	for _, k := range keys {
		out = append(out, provider.ModelInfo{ID: k, ContextWindow: 180000})
	}
	return out
}

// ModelKey 返回客户端模型名对应的上游 model key；未知模型返回空。
func ModelKey(clientName string) string {
	return staticModelKeys[clientName]
}
