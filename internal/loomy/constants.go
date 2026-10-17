// Package loomy 封装讯飞 Loomy（loomyad.xunfei.cn）上游协议，实现 provider.Upstream。
//
// 协议要点（2026-09-22 实测，见 本地 docs/loomy渠道接入备忘.md、本地 docs/loomy-raccoon接入记录.md）：
//   - 推理：POST https://loomyad.xunfei.cn/api/v1/chat/completions（OpenAI 兼容，SSE）。
//     请求头必须同时带 `Authorization: Bearer <session>`、`token: <session>`（客户端双写）、
//     `traceparent`（**缺失会挂死到超时**，官方源码注释实测）、`loomy-version`。
//   - 模型目录：GET https://loomyad.xunfei.cn/api/v1/models
//     → 含 reasoning_enabled / reasoning_catalog_version 与每模型 reasoning_efforts（**上游权威档位**）。
//   - 档位：客户端要"关思考"时，官方客户端会发三件套
//     （reasoning_effort + enable_thinking + chat_template_kwargs.enable_thinking）。
//     实测该模型是"强制思考"型：三件套能把思考从 98 字降到 59 字，但**无法完全关闭**。
//   - 鉴权失效：**HTTP 200 + body {"code":"100002",...}**（不是 4xx）→ 必须按业务码判定。
//   - 无 refresh 端点（客户端用 14 天有效期的 session），到期需重新导入凭据。
package loomy

import (
	"regexp"
	"strconv"
	"strings"

	"wild-work/internal/provider"
)

// 上游域名与端点。
const (
	// GatewayBase 推理网关（= .env.prod 的 LOOMY_POINTS_BASE_URL + /api/v1）。
	GatewayBase = "https://loomyad.xunfei.cn"
	// APIPrefix 网关 API 前缀（bundled-resources.js 按 pointsBaseUrl 派生）。
	APIPrefix = "/api/v1"

	EpChat   = APIPrefix + "/chat/completions"
	EpModels = APIPrefix + "/models"

	// ⚠️ 积分/话费类接口**不在 /api/v1 前缀下**：客户端 points-service.js 里的 path 自带 `/api/`，
	// 实测 `GET /api/v1/v2/points/records` 会 404 —— 必须用站点根 + 完整路径。
	//
	// 余额口径取 **v1 面**（2026-09-23 实测）：v1 直接下发 `availableBalance`
	// （= balance + dailyBalance，上游权威的可用总额）与逐笔 ledger；
	// v2 面只给 `balance` + `dailyBalance`（聊天聚合形状、无 availableBalance），
	// 用它就得自己相加 —— 多一个口径推导就多一处出错面。
	EpPointsV1 = "/api/v1/points/records"
	EpBalance  = "/api/v1/team-points/balance"

	// 宠物每日打工（见 checkin.go 的机制说明）：**这才是 Loomy 唯一"能领"的每日积分**。
	// 每天 0~1000 分，按该账号的**有效对话轮次**算档位，由服务端判定是否达标。
	// 证据：客户端 points-service.js:416-418 / :434-450。
	EpPetWork        = "/api/v1/pet-work"
	EpPetWorkRewards = "/api/v1/pet-work/rewards"
)

// ChannelName 费率面板与日志中的渠道标识。
const ChannelName = "loomy"

// DefaultModel 实测默认模型（/models 里 is_default_model=true）。
const DefaultModel = "deepseek-v4-flash-0731"

// AppVersion 客户端版本（`loomy-version` 头）。桌面版升级后需同步（读
// `resources/app.asar.unpacked/package.json` 的 version；取证时为 0.9.38）。
const AppVersion = "0.9.38"

// effortAll 上游声明的通用档位（实测 8 个模型全部相同，default=low）。
var effortAll = []string{"none", "low", "medium", "high", "xhigh"}

// staticModels 静态兜底模型表（/models 不可用时）。
// ContextWindow/MaxTokens 与倍率均来自 2026-09-22 实测；倍率从名称文本（xN.N）解析。
var staticModels = []provider.ModelInfo{
	{ID: "deepseek-v4-flash-0731", Name: "DeepSeek V4 Flash 0731", ContextWindow: 1_048_576, MaxTokens: 384_000,
		ContextFromAPI: true, SupportsReasoning: true, SupportsTools: true, SupportedEfforts: effortAll, DefaultEffort: "low"},
	{ID: "MiniMax-M3", Name: "MiniMax M3", ContextWindow: 1_048_576, MaxTokens: 512_000,
		ContextFromAPI: true, SupportsImages: true, SupportsReasoning: true, SupportsTools: true, SupportedEfforts: effortAll, DefaultEffort: "low"},
	{ID: "Kimi-k2.6", Name: "Kimi k2.6", ContextWindow: 262_144, MaxTokens: 65_536,
		ContextFromAPI: true, SupportsImages: true, SupportsReasoning: true, SupportsTools: true, SupportedEfforts: effortAll, DefaultEffort: "low"},
	{ID: "qwen-3.8-max", Name: "Qwen 3.8 Max", ContextWindow: 1_000_000, MaxTokens: 65_536,
		ContextFromAPI: true, SupportsReasoning: true, SupportsTools: true, SupportedEfforts: effortAll, DefaultEffort: "low"},
	{ID: "GLM-5.3-Flash", Name: "GLM 5.3 Flash", ContextWindow: 1_048_576, MaxTokens: 131_072,
		ContextFromAPI: true, SupportsImages: true, SupportsReasoning: true, SupportsTools: true, SupportedEfforts: effortAll, DefaultEffort: "low"},
	{ID: "qwen3.8-flash", Name: "qwen 3.8 flash", ContextWindow: 1_000_000, MaxTokens: 131_072,
		ContextFromAPI: true, SupportsImages: true, SupportsReasoning: true, SupportsTools: true, SupportedEfforts: effortAll, DefaultEffort: "low"},
	{ID: "spark-x", Name: "Spark X2.5", ContextWindow: 1_048_576, MaxTokens: 65_536,
		ContextFromAPI: true, SupportsReasoning: true, SupportsTools: true, SupportedEfforts: effortAll, DefaultEffort: "low"},
	{ID: "mimo-v2.5", Name: "MiMo V2.5", ContextWindow: 1_048_576, MaxTokens: 131_072,
		ContextFromAPI: true, SupportsImages: true, SupportsReasoning: true, SupportsTools: true, SupportedEfforts: effortAll, DefaultEffort: "low"},
}

// StaticModels 返回静态兜底模型表副本（供 server.Runtime.StaticModels）。
func StaticModels() []provider.ModelInfo {
	return append([]provider.ModelInfo{}, staticModels...)
}

// KnownModel 报告模型名是否在上游目录内（静态表）。
// 与 raccoon 同理：上游对未知模型**静默回落到默认模型**并返回 200（阶段 C 实测），
// 因此渠道层必须本地校验，否则用户写错模型会静默消耗默认模型额度。
func KnownModel(id string) bool {
	for _, m := range staticModels {
		if m.ID == id {
			return true
		}
	}
	return false
}

// priceRe 从模型显示名里取倍率，形如 `DeepSeek V4 Flash 0731（x3.0）` / `Qwen 3.8 Max (x12.0)`。
var priceRe = regexp.MustCompile(`[（(]\s*[xX×]\s*([0-9]+(?:\.[0-9]+)?)\s*[)）]`)

// priceFromName 解析显示名中的倍率；无法解析返回 0（调用方应视为「未知」而不是「免费」）。
func priceFromName(name string) float64 {
	m := priceRe.FindStringSubmatch(name)
	if len(m) != 2 {
		return 0
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	return v
}

// cleanName 去掉显示名里的倍率尾巴，用于面板展示更干净的名字。
func cleanName(name string) string {
	return strings.TrimSpace(priceRe.ReplaceAllString(name, ""))
}
