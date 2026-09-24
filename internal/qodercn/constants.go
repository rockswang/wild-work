// Package qodercn 封装 QoderCN（qoder.com.cn）上游协议：COSY 签名、
// QoderEncoding 编码、嵌套 SSE 解析，并实现 provider.Upstream 接口。
// 独立渠道：移植自 qoder2api（Zhengyuuuui，参考其 CN 分支参数），
// 不与 internal/qoder（QoderWork 渠道）共享代码运行时，仅代码级复制改造。
// 主要差异（对照 qoderwork 渠道）：cosyVersion=1.0.10、COSY 头多
// cosy-scene/cosy-business-product/cosy-business-type、identity 的 user_type
// 来自 /api/v1/userinfo 实测值、请求体 session_type="qoder" 含 parameters 等字段。
// 模型列表无静态兜底表：仅用「上次成功拉取的动态表」作缓存（内存态）。
package qodercn

import (
	"wild-work/internal/provider"
)

// 上游域名与端点（CN only）。
const (
	OpenAPIBase = "https://openapi.qoder.com.cn" // 业务 API（dt- Bearer，无签名）
	GatewayBase = "https://gateway.qoder.com.cn" // 推理网关（COSY 签名）

	EpQuotaUsage = "/api/v2/quota/usage"
	EpCampaigns  = "/sash/api/v1/me/campaigns" // 活动列表+领取（唯一领取路径；daily-check-in 已 DISABLED）
	EpPlan       = "/api/v2/user/plan"
	EpUserInfo   = "/api/v1/userinfo"
	EpDTRefresh  = "/api/v1/deviceToken/refresh"
	EpModels     = "/algo/api/v2/model/list?Encode=1"
	EpChat       = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"

	clientUA = "Go-http-client/2.0"
)

// StaticModels 返回空的静态兜底（QoderCN 无静态表）。
// server 端 /v1/models 首次成功依赖动态 FetchModels；失败时渠道返回空列表由前端提示。
func StaticModels() []provider.ModelInfo {
	return nil
}
