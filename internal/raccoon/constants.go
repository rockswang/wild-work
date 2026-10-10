// Package raccoon 封装小浣熊（xiaohuanxiong.com）上游协议，实现 provider.Upstream。
//
// 协议要点（2026-09-22 实测，见 本地 docs/raccoon渠道接入备忘.md、本地 docs/loomy-raccoon接入记录.md）：
//   - 推理走官方托管网关：POST https://xiaohuanxiong.com/api/web/llm/v2/chat/completions
//     鉴权只认 `Authorization: Bearer <access_token>`（token 头 / Cookie / X-Access-Token 均 401）。
//   - 模型目录：GET /api/web/llm/v2/model_catalog（含 billing_multiplier 倍率 → 费率面板数据源）。
//   - 凭据：access_token（JWT，实测 ≈1h）+ refresh_token（JWT，实测 ≈30 天），
//     刷新端点为 POST https://xiaohuanxiong.com/api/web/auth/v1/refresh，
//     body {"refresh_token":…} → resp {data:{access_token,refresh_token}}；**refresh_token 会轮换**。
//   - 错误信封是 LiteLLM 风格：401 + {"code":200001|200003}；400 + {"error":{"message":"litellm…"}}。
//   - ⚠️ 未知模型名会被上游**静默回落到默认模型**并返回 200 —— 渠道层必须自行校验模型名，
//     否则用户写错模型会静默消耗默认模型额度（阶段 C 实测）。
package raccoon

import "wild-work/internal/provider"

// 上游域名与端点。
const (
	// BaseURL 站点根（认证与业务 API 同域）。
	BaseURL = "https://xiaohuanxiong.com"
	// LLMBase 官方托管模型网关（OpenAI 兼容）。
	LLMBase = BaseURL + "/api/web/llm/v2"

	EpChat         = "/chat/completions"
	EpModelCatalog = "/model_catalog"

	// EpPointsBalance 积分余额（**不在 /api/web/llm/v2 前缀下**，是站点根的 points 服务）。
	// 2026-09-22 实测：GET → {"code":0,"data":{available_points,daily_points,monthly_points,
	// reward_points,topup_points,topup_frozen}}。路径取自 renderer chunk
	// `75741-*.js` 的 `class extends ... static get baseUrl(){return `${origin}/api/web/points/v1`}`。
	EpPointsBalance = "/api/web/points/v1/balance"
	// EpPointsBills 积分流水（分页参数为 `paging.limit` / `paging.offset` 或 `cursor`，
	// 另需合法 `type`；暂无必要，保留备查）。
	EpPointsBills = "/api/web/points/v1/bills"

	// EpAuthRefresh 桌面端旧前缀（客户端 env: NEXT_PUBLIC_AUTH_API_PREFIX = /api/electron/auth/v1）。
	// 2026-10-10 实测该前缀的 /refresh 已 **404**（上游下线），仅作历史兑底保留。
	EpAuthRefresh = "/api/electron/auth/v1/refresh"
	// EpAuthRefreshWeb 现行刷新前缀（客户端 env: NEXT_PUBLIC_DESKTOP_REMOTE_AUTH_API_PREFIX
	// = /api/web/auth/v1）。**2026-10-10 实测唯一可用**：网页版与桌面版凭据都用它刷新，
	// 故 RefreshToken 以它为**首选**，404 才回落 electron 旧前缀。
	EpAuthRefreshWeb = "/api/web/auth/v1/refresh"
)

// ChannelName 费率面板与日志中的渠道标识。
const ChannelName = "raccoon"

// DefaultModel 官方默认对话模型（model_catalog 的 default_model，实测）。
const DefaultModel = "raccoon-8c4485"

// ClientAliasModel 客户端内置配置里的别名（resources/default-llm-config.json），
// 实测该别名同样被上游接受并路由到默认模型。
const ClientAliasModel = "raccoon-chat-ml-5-5"

// staticModels 静态兜底模型表（模型目录接口不可用时）。
// 数据来源：2026-09-22 实测 GET /model_catalog。
// SupportsTools/SupportsImages/SupportsReasoning 按上游 tags 与约定：
// 仅当上游明确声明时才置 true（provider.ModelInfo 的约定：未知保持 false）。
var staticModels = []provider.ModelInfo{
	{ID: "raccoon-8c4485", Name: "Raccoon-Work", ContextWindow: 1_000_000, MaxTokens: 100_000,
		ContextFromAPI: true, SupportsImages: true, SupportsReasoning: true},
	{ID: "raccoon-19b265", Name: "Raccoon-Work-260817-A", ContextWindow: 1_000_000, MaxTokens: 100_000,
		ContextFromAPI: true, SupportsReasoning: true},
	{ID: "raccoon-405a1c", Name: "Raccoon-Work-260817-B", ContextWindow: 1_000_000, MaxTokens: 100_000,
		ContextFromAPI: true},
	{ID: "sn-sensenova-6-8-flash-lite", Name: "SenseNova-6.8-Flash-Lite", ContextWindow: 256_000, MaxTokens: 63_999,
		ContextFromAPI: true, SupportsImages: true},
	{ID: "sn-glm-5-3", Name: "GLM-5-3", ContextWindow: 1_000_000, MaxTokens: 100_000,
		ContextFromAPI: true, SupportsImages: true, SupportsReasoning: true},
	{ID: "sn-kimi-k3", Name: "Kimi-K3", ContextWindow: 1_000_000, MaxTokens: 100_000,
		ContextFromAPI: true, SupportsImages: true, SupportsReasoning: true},
	{ID: "sn-glm-5-3-flash", Name: "GLM-5-3-Flash", ContextWindow: 1_000_000, MaxTokens: 100_000,
		ContextFromAPI: true, SupportsImages: true},
	{ID: "sn-deepseek-v4-1-flash", Name: "DeepSeek-V4.1-Flash", ContextWindow: 1_000_000, MaxTokens: 100_000,
		ContextFromAPI: true, SupportsImages: true, SupportsReasoning: true},
}

// StaticModels 返回静态兜底模型表副本（供 server.Runtime.StaticModels）。
func StaticModels() []provider.ModelInfo {
	return append([]provider.ModelInfo{}, staticModels...)
}

// KnownModel 报告该模型名是否在上游目录内（静态表 + 客户端别名 + 默认模型）。
//
// 为什么需要本地校验：阶段 C 实测确认，上游对未知模型名**不报错**，
// 而是静默回落到默认模型并返回 200 —— 不校验就会让用户以为在用 A 模型、实际消耗 B 模型的额度。
// 动态目录拉取成功时，调用方应优先用动态结果（见 Client.knownModels）。
func KnownModel(id string) bool {
	if id == DefaultModel || id == ClientAliasModel {
		return true
	}
	for _, m := range staticModels {
		if m.ID == id {
			return true
		}
	}
	return false
}
