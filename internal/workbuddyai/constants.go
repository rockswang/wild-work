// Package workbuddyai 封装 WorkBuddy 国际版（www.workbuddy.ai）上游协议，
// 实现 provider.Upstream 接口。与国内版 workbuddy 渠道完全独立：
// 独立 Kind、独立 auth 文件前缀、独立模型表、独立对话改写，互不影响。
//
// 协议要点（实测）：
//   - 全部走 /v2/* 与 /billing/*；/console/* 用 Bearer 会 500（网关拒绝），不可用。
//   - 目录路径为 /v2/enterprises/personal/models（国内版是 /console/...）。
//   - accessToken 有效期约 365 天，refreshToken 轮换。
package workbuddyai

import "wild-work/internal/provider"

// 上游域名与端点（国际版单域名，chat/billing/catalog/auth 同域）。
const (
	WBAIHost = "https://www.workbuddy.ai"

	clientUA   = "CLI/2.63.2 CodeBuddy/2.63.2"
	originRef  = WBAIHost
	wbaiDomain = "www.workbuddy.ai"

	// EpCatalog 模型目录（国际版专用路径）。
	EpCatalog = "/v2/enterprises/personal/models"
	// EpCatalogV3 模型目录 v3 端点（2026-09-28 实测国际版可用）。
	// 与 v2 差异：全量模型面（22 个 vs cli 面 18 个），extraModels 里的
	// deepseek-v4.1-flash/gpt-6-astra/kimi-k2.8-preview 只在这里有 credits。
	// 有 UA 门禁：web UA → 400 code=12403 "check ua"，必须带 CLI 形 UA。
	EpCatalogV3 = "/v3/config"
	// EpChat 对话（OpenAI 兼容 SSE）。
	EpChat = "/v2/chat/completions"
	// EpRefresh 刷新 token（X-Refresh-Token 专属端点）。
	EpRefresh = "/v2/plugin/auth/token/refresh"
	// EpUserResource 余额（所有套餐聚合）。
	EpUserResource = "/v2/billing/meter/get-user-resource"
	// EpDailyCheckin 每日签到（国际版当前未开启，返回 code=10001）。
	EpDailyCheckin = "/v2/billing/meter/daily-checkin"

	// 登录三端点（与国内版路径完全相同，仅 host 不同）。
	EpAuthState = "/v2/plugin/auth/state?platform=CLI"
	EpAuthToken = "/v2/plugin/auth/token?state="
	EpLoginAcct = "/v2/plugin/login/account?state="
)

// ProductCode 账单查询固定产品码（与国内版一致）。
const ProductCode = "p_tcaca"

// ChannelName 费率面板中的渠道标识。
const ChannelName = "workbuddyai"

// brokenModels 实测不可用的模型（400 code=11102 service info not found），
// 一律从动态目录结果中剔除，避免出现在 /models 与费率面板。
// 注：这些多为国内版命名，国际版靠 -preview/-f/-codex 后缀区分。
var brokenModels = map[string]bool{
	"deepseek-v4-pro":   true,
	"deepseek-v4-flash": true,
	"deepseek-chat":     true,
	"deepseek-reasoner": true,
	"deepseek-v4.1":     true,
	"hy3-preview":       true,
	"hy3-preview-agent": true,
	"gpt-6":             true,
	"kimi-k2.8":         true, // 须带 -preview
}

// extraModels 目录接口不返回、但实测可用的模型，硬编码补进 /models 列表。
// 注意：这些模型无上游能力数据，故不声明任何能力（见 /v1/models 的透传原则）。
//
// 2026-09-28 起 /v3/config 已覆盖其中 3 个（deepseek-v4.1-flash/gpt-6-astra/
// kimi-k2.8-preview，且带能力与倍率字段），合并后此处条目仅作「v3 也不返回时」
// 的兜底；其余 6 个（deepseek-v3/glm-5.1/glm-5v-turbo/hy4-preview-f/kimi-k2.7/
// minimax-m3）在 v2/v3 均无，仍靠本表补进模型清单（无费率，面板显示 unknown）。
var extraModels = []provider.ModelInfo{
	{ID: "deepseek-v4.1-flash", Name: "Deepseek-V4.1-Flash", ContextWindow: 1_000_000, MaxTokens: 128_000},
	{ID: "hy4-preview-f", Name: "Hy4 preview F", ContextWindow: 1_000_000, MaxTokens: 64_000},
	{ID: "gpt-6-astra", Name: "GPT-6-Astra", ContextWindow: 1_000_000, MaxTokens: 128_000},
	{ID: "kimi-k2.8-preview", Name: "Kimi-K2.8-Preview", ContextWindow: 1_000_000, MaxTokens: 32_000},
	{ID: "kimi-k2.7", Name: "Kimi-K2.7", ContextWindow: 1_000_000, MaxTokens: 32_000},
	{ID: "minimax-m3", Name: "MiniMax-M3", ContextWindow: 256_000, MaxTokens: 32_000},
	{ID: "deepseek-v3", Name: "Deepseek-V3", ContextWindow: 192_000, MaxTokens: 32_000},
	{ID: "glm-5.1", Name: "GLM-5.1", ContextWindow: 1_000_000, MaxTokens: 48_000},
	{ID: "glm-5v-turbo", Name: "GLM-5V-Turbo", ContextWindow: 256_000, MaxTokens: 32_000},
}

// freeModels 实测 x0.00 且可用的免费模型，按优先级排列。
// 每日活跃任务（见 client.DailyCheckin）只用这些模型，确保不扣积分。
var freeModels = []string{
	"deepseek-v4.1-flash",
	"hy3",
	"hy4-preview",
	"hy4-preview-f",
}

// staticModels 静态兜底模型表：目录接口不可用时的 fallback。
// 含目录内 18 个中的常用项 + extraModels，已剔除 brokenModels。
var staticModels = []provider.ModelInfo{
	{ID: "hy3", Name: "Hy3", ContextWindow: 192_000, MaxTokens: 64_000},
	{ID: "hy4-preview", Name: "Hy4 preview", ContextWindow: 1_000_000, MaxTokens: 64_000},
	{ID: "deepseek-v4.1-flash", Name: "Deepseek-V4.1-Flash", ContextWindow: 1_000_000, MaxTokens: 128_000},
	{ID: "hy4-preview-f", Name: "Hy4 preview F", ContextWindow: 1_000_000, MaxTokens: 64_000},
	{ID: "default-model", Name: "Auto", ContextWindow: 176_000, MaxTokens: 24_000},
	{ID: "fast-model", Name: "Fast", ContextWindow: 200_000, MaxTokens: 32_000},
	{ID: "balanced-model", Name: "Balanced", ContextWindow: 256_000, MaxTokens: 32_000},
	{ID: "primary-model", Name: "Primary", ContextWindow: 272_000, MaxTokens: 72_000},
	{ID: "deep-model", Name: "Deep", ContextWindow: 176_000, MaxTokens: 24_000},
	{ID: "gpt-5.6-sol", Name: "GPT-5.6-Sol", ContextWindow: 1_000_000, MaxTokens: 128_000},
	{ID: "gpt-5.6-terra", Name: "GPT-5.6-Terra", ContextWindow: 1_000_000, MaxTokens: 128_000},
	{ID: "gpt-5.6-luna", Name: "GPT-5.6-Luna", ContextWindow: 1_000_000, MaxTokens: 128_000},
	{ID: "gpt-6-astra", Name: "GPT-6-Astra", ContextWindow: 1_000_000, MaxTokens: 128_000},
	{ID: "gpt-5.5", Name: "GPT-5.5", ContextWindow: 1_000_000, MaxTokens: 128_000},
	{ID: "gpt-5.4", Name: "GPT-5.4", ContextWindow: 272_000, MaxTokens: 72_000},
	{ID: "gpt-5.3-codex", Name: "GPT-5.3-Codex", ContextWindow: 272_000, MaxTokens: 72_000},
	{ID: "gemini-3.5-flash", Name: "Gemini-3.5-Flash", ContextWindow: 1_000_000, MaxTokens: 65_536},
	{ID: "glm-5.3", Name: "GLM-5.3", ContextWindow: 1_000_000, MaxTokens: 48_000},
	{ID: "glm-5.2", Name: "GLM-5.2", ContextWindow: 1_000_000, MaxTokens: 48_000},
	{ID: "kimi-k3", Name: "Kimi-K3", ContextWindow: 1_000_000, MaxTokens: 32_000},
	{ID: "kimi-k2.6", Name: "Kimi-K2.6", ContextWindow: 256_000, MaxTokens: 32_000},
	{ID: "kimi-k2.8-preview", Name: "Kimi-K2.8-Preview", ContextWindow: 1_000_000, MaxTokens: 32_000},
	{ID: "kimi-k2.7", Name: "Kimi-K2.7", ContextWindow: 1_000_000, MaxTokens: 32_000},
	{ID: "minimax-m3", Name: "MiniMax-M3", ContextWindow: 256_000, MaxTokens: 32_000},
	{ID: "deepseek-v3", Name: "Deepseek-V3", ContextWindow: 192_000, MaxTokens: 32_000},
	{ID: "glm-5.1", Name: "GLM-5.1", ContextWindow: 1_000_000, MaxTokens: 48_000},
	{ID: "glm-5v-turbo", Name: "GLM-5V-Turbo", ContextWindow: 256_000, MaxTokens: 32_000},
}

// StaticModels 返回静态兜底模型表副本（供 server.Runtime.StaticModels）。
func StaticModels() []provider.ModelInfo {
	return append([]provider.ModelInfo{}, staticModels...)
}
