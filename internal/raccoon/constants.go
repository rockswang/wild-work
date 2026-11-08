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
	// EpPointsBills 积分流水。`paging.limit` 有效、`paging.offset` 实测无效
	// （offset>0 恒空），条数另有上限（limit=1000 仍只回 100 条）。
	// `type` 是筛选枚举 all / expense / income（缺省 all，其它值 400）——
	// 官方桌面客户端「积分明细」页的「全部/消耗/收入」页签即靠它切换。
	// 本渠道取发放记录时固定带 `type=income`，理由见 fetchDailyGrants。
	EpPointsBills = "/api/web/points/v1/bills"

	// EpOfficeSettingInfo 工作台设置（配额/空间/功能开关），也是**每日积分落账的触发点**。
	//
	// 2026-10-15 实测：每日 300 积分名义上 00:00 记账（bills 的 grant_at），
	// 实际要等账号当天第一次访问「工作台业务接口」才落账（created_at = 该次访问时刻）。
	// 三账号对照：调本接口 → 各入账 300（point_grant_popups 非空）；
	// 纯对话 / 只读余额 / auth/v1 的 user_info 与 iam/confirm → 均不入账。
	// 网页端打开工作台首页时自己就会调它，故调它 ≈ 打开一次网页（见 checkin.go）。
	EpOfficeSettingInfo = "/api/web/office/v3/setting_info"

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
// SupportsTools/SupportsImages/SupportsReasoning 优先取上游 tags；其中图片能力另有
// 2026-10-28 实测兜底（见 measuredVisionModels / R140）。
// measuredVisionModels 是 2026-10-28 通过 wild-work → 小浣熊真实链路逐个实测
// 确认能消费 OpenAI image_url（data:image/png;base64）并正确识别图像的模型。
//
// 采用「有图/无图对照」排除按提示猜答案：同一问题无图均答「无图」，附测试图后
// 才正确识别「左红/右蓝/顶部绿色矩形」。sn-glm-5-3 明确返回
// `当前纯文本模型不支持 image_url`，故刻意不在此表。
//
// 上游 catalog 的 vision tag 曾漏标 raccoon-19b265 / raccoon-405a1c，若只信 tag，
// /v1/models 会把实测可识图的模型错误声明为 text-only（R140）。
var measuredVisionModels = map[string]bool{
	"raccoon-8c4485":              true,
	"raccoon-19b265":              true,
	"raccoon-405a1c":              true,
	"sn-sensenova-6-8-flash":      true,
	"sn-sensenova-6-8-flash-lite": true,
	"sn-kimi-k3":                  true,
	"sn-glm-5-3-flash":            true,
	"sn-deepseek-v4-1-flash-1017": true,
}

var staticModels = []provider.ModelInfo{
	{ID: "raccoon-8c4485", Name: "Raccoon-Work", ContextWindow: 1_000_000, MaxTokens: 100_000,
		ContextFromAPI: true, SupportsImages: true, SupportsReasoning: true},
	{ID: "raccoon-19b265", Name: "Raccoon-Work-260817-A", ContextWindow: 1_000_000, MaxTokens: 100_000,
		ContextFromAPI: true, SupportsImages: true, SupportsReasoning: true},
	{ID: "raccoon-405a1c", Name: "Raccoon-Work-260817-B", ContextWindow: 1_000_000, MaxTokens: 100_000,
		ContextFromAPI: true, SupportsImages: true},
	{ID: "sn-sensenova-6-8-flash-lite", Name: "SenseNova-6.8-Flash-Lite", ContextWindow: 256_000, MaxTokens: 63_999,
		ContextFromAPI: true, SupportsImages: true},
	{ID: "sn-glm-5-3", Name: "GLM-5-3", ContextWindow: 1_000_000, MaxTokens: 100_000,
		ContextFromAPI: true, SupportsReasoning: true},
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
