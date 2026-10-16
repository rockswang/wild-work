package officeace

import "wild-work/internal/provider"

// 已知可用模型（作为上游目录拉取失败时的兜底；正常以 /models 动态目录为准）。
// 来源：officeace2api 实测「能用的 11 个」+ 其目录里其余条目；上游 /models 无能力声明，
// 故这里也只透出 id，不编造 SupportsTools/SupportsReasoning（R49 哲学）。
var knownModels = []string{
	"glm-5.3", "glm-5.2", "glm-5.1",
	"deepseek-v4.1-flash", "deepseek-v4.1-flash-0930",
	"deepseek-v4-flash", "deepseek-v4-flash-0731",
	"deepseek-v4-pro", "deepseek-v4-flash-A3",
	"deepseek-v3.2", "deepseek-r1-250528",
	"Kimi-K2.6", "kimi-k2.6",
	"qwen3-32b", "qwen3-30b-a3b",
	"glm-5.3-test", "glm-5.3-zx", "glm-5-test",
	"glm-5.2-a3", "glm-5.2-zx", "glm-5.2-test", "glm-5.2-exercise", "glm-5.2-harmony",
	"glm-5.1-test", "glm-5.1-exercise", "glm.5.2-arkts-spark",
	"deepseek-v3-test",
}

// StaticModels 返回兜底模型表（上游目录不可用时使用）。
func StaticModels() []provider.ModelInfo {
	out := make([]provider.ModelInfo, 0, len(knownModels))
	for _, id := range knownModels {
		out = append(out, provider.ModelInfo{
			ID:   id,
			Name: id,
		})
	}
	return out
}
