package modelscope

import "wild-work/internal/provider"

// staticEntry 静态目录条目（票 01 只提供最小静态集，票 02 与上游 /v1/models 取并集）。
type staticEntry struct {
	// ID 上游模型名，**保留上游斜杠**（如 Qwen/Qwen-Image）。对外 ID 由
	// provider.PublicModelID 拼成 `modelscope/Qwen/Qwen-Image`，渠道内不得自己拼前缀。
	ID string
	// Image 标记「生图 / 改图」模型（D12）：进 /v1/models 目录（客户端要能看到可画图模型），
	// 但**不参与对话路由**。
	//
	// ⚠️ 与 provider.ModelInfo.SupportsImages 是**两个维度**：后者是「能看图」（视觉输入），
	// 本字段是「能出图」。`Qwen2.5-VL` 这类视觉理解模型不得标 Image。
	Image bool
}

// staticCatalog 最小静态目录：至少一款对话模型 + 一款生图模型（票 01 验收）。
//
// 能力字段（tools / 思考档位 / 上下文）**一律不声明**：上游没说过的话不得替它说
// （R49/R119）。真实能力由票 02 从 /v1/models 的上游声明透传。
var staticCatalog = []staticEntry{
	{ID: "Qwen/Qwen3-235B-A22B-Instruct-2507"},
	{ID: "Qwen/Qwen-Image", Image: true},
	{ID: "Qwen/Qwen-Image-Edit", Image: true},
}

// StaticModels 静态目录（对外 ID 仍是上游名，前缀由 server 层用 PublicModelID 拼）。
//
// 上游 /v1/models 拉取失败时的兜底，也是「启动即有目录」的来源（R42 并集口径，
// 远程目录由票 02 补）。
func StaticModels() []provider.ModelInfo {
	out := make([]provider.ModelInfo, 0, len(staticCatalog))
	for _, e := range staticCatalog {
		out = append(out, provider.ModelInfo{ID: e.ID, Name: e.ID})
	}
	return out
}

// IsImageModel 报告上游模型名是否被静态标记为生图模型（D12）。
// 票 02 会在静态标记之外再叠加「模型名启发式」判定。
func IsImageModel(upstreamID string) bool {
	for _, e := range staticCatalog {
		if e.Image && e.ID == upstreamID {
			return true
		}
	}
	return false
}
