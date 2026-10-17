// free.go 免费模型判定与静态目录。
//
// 判定规则（需求：仅对客户端暴露免费模型 + big-pickle）：
//   - 模型 ID 含 "free"（大小写不敏感）→ 免费；
//   - 或 ID 恰为 "big-pickle"（上游命名不含 free 但成本为 0）。
//
// 地域受限的免费模型（muse-spark-*-contributor-free 实测返回 403 RegionError）
// 一并保留：用户自备代理即可使用，不在列表侧做地域过滤。
package oczen

import (
	"strings"

	"wild-work/internal/provider"
)

// freeIDs 已知免费模型元数据（models.dev 目录 + 2026-09 实测上下文/能力）。
// 用途有二：① 给动态拉取到的模型补上下文与能力（/v1/models 不返回这些字段）；
// ② 上游列表拉取失败时的静态兜底，保证面板与客户端至少能看到可用清单。
// 2026-09-24 与 models.dev opencode 渠道对齐：补 deepseek-v4-flash-free/space-bunny-free
// （此前缺项致 live enrichment 落空、面板显示「未知」）；
// mimo/muse-spark/space-bunny 按 models.dev 补 SupportsImages 多模态声明。
var freeIDs = []provider.ModelInfo{
	{ID: "big-pickle", Name: "Big Pickle", ContextWindow: 200000, MaxTokens: 32000, SupportsReasoning: true},
	{ID: "deepseek-v4-flash-free", Name: "DeepSeek V4 Flash Free", ContextWindow: 200000, MaxTokens: 128000, SupportsReasoning: true},
	{ID: "ling-3.0-flash-fin-free", Name: "Ling 3.0 Flash Fin Free", ContextWindow: 262144, MaxTokens: 32768, SupportsReasoning: true},
	{ID: "mimo-v2.5-free", Name: "MiMo V2.5 Free", ContextWindow: 200000, MaxTokens: 32000, SupportsReasoning: true, SupportsImages: true},
	{ID: "mimo-v2.6-flash-free", Name: "MiMo-V2.6-Flash Free", ContextWindow: 200000, MaxTokens: 32000, SupportsReasoning: true, SupportsImages: true},
	{ID: "muse-spark-1.2-contributor-free", Name: "Muse Spark 1.2 Free", ContextWindow: 1048576, MaxTokens: 131072, SupportsReasoning: true, SupportsImages: true},
	{ID: "muse-spark-1.3-contributor-free", Name: "Muse Spark 1.3 Free", ContextWindow: 1048576, MaxTokens: 131072, SupportsReasoning: true, SupportsImages: true},
	{ID: "nemotron-3-ultra-free", Name: "Nemotron 3 Ultra Free", ContextWindow: 1000000, MaxTokens: 128000, SupportsReasoning: true},
	{ID: "nemotron-3.5-lightning-free", Name: "Nemotron 3.5 Lightning Free", ContextWindow: 262144, MaxTokens: 262144, SupportsReasoning: true},
	{ID: "space-bunny-free", Name: "Space Bunny Free", ContextWindow: 1048576, MaxTokens: 524288, SupportsReasoning: true, SupportsImages: true},
}

// staticFree 返回静态目录副本（StaticModels 用，避免调用方改到包级变量）。
func staticFree() []provider.ModelInfo {
	return append([]provider.ModelInfo{}, freeIDs...)
}

// staticByID 已知模型的 id → 元数据索引。
func staticByID() map[string]provider.ModelInfo {
	m := make(map[string]provider.ModelInfo, len(freeIDs))
	for _, mi := range freeIDs {
		m[mi.ID] = mi
	}
	return m
}

// IsFreeModel 报告模型 ID 是否属于对客户端暴露的免费集合
// （ID 含 free，或为 big-pickle）。
func IsFreeModel(id string) bool {
	low := strings.ToLower(strings.TrimSpace(id))
	if low == "" {
		return false
	}
	return strings.Contains(low, "free") || low == "big-pickle"
}

// filterFree 从上游返回的模型清单中筛出免费集合，并用静态目录补齐
// 名称/上下文/能力。ContextFromAPI 恒为 false：/v1/models 不返回这些字段，
// 已知模型的值来自静态目录（估值），未知模型保持 0 由前端显示「未知」。
func filterFree(ids []string) []provider.ModelInfo {
	known := staticByID()
	out := make([]provider.ModelInfo, 0, len(ids))
	for _, id := range ids {
		if !IsFreeModel(id) {
			continue
		}
		if mi, ok := known[id]; ok {
			out = append(out, mi)
			continue
		}
		out = append(out, provider.ModelInfo{ID: id, Name: id})
	}
	return out
}
