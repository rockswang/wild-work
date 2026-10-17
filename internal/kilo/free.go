// free.go Kilo 免费集合的判定、清单解析与静态兜底。
//
// 判定口径**只认上游声明**：清单行里 `isFree === true` 才算免费池（参考实现
// catalog.js:330-331 的 isKiloFreeRow）。不按模型名含 "free" 猜——那是 oczen
// 的口径（其上游不声明该字段），两个渠道的清单形状不同，不能互相套用。
package kilo

import (
	"strings"

	"wild-work/internal/provider"
)

// listingRow 上游 GET /models 的单行（OpenRouter 风格清单）。
//
// 字段来源：参考实现 catalog.js:355 的 kiloEntryOf —— 上下文窗口优先取
// 顶层 `context_length`，缺失时退回 `top_provider.context_length`，两者都缺
// 才用参考实现的默认值 131072（本仓把「用了默认值」标注为估值，见 rowToModelInfo）。
type listingRow struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	IsFree              bool     `json:"isFree"`
	ContextLength       int64    `json:"context_length"`
	SupportedParameters []string `json:"supported_parameters"`
	TopProvider         struct {
		ContextLength int64 `json:"context_length"`
	} `json:"top_provider"`
	MaxOutput int64 `json:"max_output"`
}

// defaultContextWindow / defaultMaxTokens 是参考实现的兜底容量（catalog.js:355/415）。
// 上游清单不带这些字段时使用，且**只作为估值**（ContextFromAPI=false），
// 面板据此显示「未知」而非把估值冒充成上游声明。
const (
	defaultContextWindow int64 = 131072
	defaultMaxTokens     int64 = 32768
)

// IsFreeRow 报告清单行是否属于免费池：上游显式 isFree=true，且 id 非空。
func IsFreeRow(r listingRow) bool {
	return r.IsFree && strings.TrimSpace(r.ID) != ""
}

// rowToModelInfo 把一行清单转成 provider.ModelInfo。
//
// 能力字段只认上游显式声明（supported_parameters 里出现对应项才算 true），
// 缺失即「未知」——与 provider.ModelInfo 的字段哲学一致（不清空、不猜测）。
// 已知的 parameter 词汇沿用 OpenAI 生态惯例：reasoning / tools / vision。
func rowToModelInfo(r listingRow) provider.ModelInfo {
	ctx := r.ContextLength
	if ctx <= 0 {
		ctx = r.TopProvider.ContextLength
	}
	fromAPI := ctx > 0
	if ctx <= 0 {
		ctx = defaultContextWindow
	}
	maxOut := r.MaxOutput
	fromMaxOut := maxOut > 0
	if maxOut <= 0 {
		maxOut = defaultMaxTokens
	}

	mi := provider.ModelInfo{
		ID:            strings.TrimSpace(r.ID),
		Name:          strings.TrimSpace(r.Name),
		ContextWindow: ctx,
		MaxTokens:     maxOut,
		// 上游只在给了 context_length 时才算「声明了容量」；只给了 max_output
		// 时上下文仍是估值，故两个标志分开判断。
		ContextFromAPI: fromAPI && fromMaxOut,
	}
	if mi.Name == "" {
		mi.Name = mi.ID
	}
	for _, p := range r.SupportedParameters {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "reasoning":
			mi.SupportsReasoning = true
		case "tools", "tool_choice":
			mi.SupportsTools = true
		case "vision", "image", "image_url":
			mi.SupportsImages = true
		}
	}
	return mi
}

// staticFree 静态兜底目录：上游清单拉取失败时至少让面板与客户端看到一条可用模型。
//
// 只有一条，因为**只有这一条有公开可查的出处**：参考实现 README 明确写了
// Kilo 免费池含 `kilo-auto/free` 自动路由。其余免费 id 会随上游变动，
// 与其编造一份会过期的清单，不如让动态拉取去拿（拉不到就短一点，但都是真的）。
func staticFree() []provider.ModelInfo {
	return []provider.ModelInfo{{
		ID:             "kilo-auto/free",
		Name:           "Kilo Auto (Free)",
		ContextWindow:  defaultContextWindow,
		MaxTokens:      defaultMaxTokens,
		ContextFromAPI: false, // 估值：出处是参考实现的默认值，不是上游声明
	}}
}

// staticByID 静态目录的 id → 元数据索引。
func staticByID() map[string]provider.ModelInfo {
	out := make(map[string]provider.ModelInfo, 1)
	for _, mi := range staticFree() {
		out[mi.ID] = mi
	}
	return out
}

// filterFree 从上游清单里筛出免费集合，并用静态目录补齐已知模型的元数据。
//
// 顺序保持上游给出的顺序（不排序）：面板与 /v1/models 的展示顺序跟随上游，
// 上游调整排序时本工具不与之对抗。
func filterFree(rows []listingRow) []provider.ModelInfo {
	known := staticByID()
	seen := make(map[string]bool, len(rows))
	out := make([]provider.ModelInfo, 0, len(rows))
	for _, r := range rows {
		if !IsFreeRow(r) {
			continue
		}
		id := strings.TrimSpace(r.ID)
		if seen[id] {
			continue
		}
		seen[id] = true
		// 命中静态目录：以静态条目为基底（它带着人工核对的容量与能力），
		// 但**上游显式声明的内容优先**——上游说支持 tools/reasoning 就是支持，
		// 不能因为静态表写得简略就把能力丢掉（漏声明会让面板把可用模型标成不支持工具）。
		// 容量同理：上游给了就用上游的，没给才保留静态估值（且 ContextFromAPI 仍为 false，
		// 保证「估值」不会被冒充成「上游声明」）。
		if mi, ok := known[id]; ok {
			if n := strings.TrimSpace(r.Name); n != "" {
				mi.Name = n
			}
			up := rowToModelInfo(r)
			if up.SupportsTools {
				mi.SupportsTools = true
			}
			if up.SupportsReasoning {
				mi.SupportsReasoning = true
			}
			if up.SupportsImages {
				mi.SupportsImages = true
			}
			if up.ContextFromAPI {
				mi.ContextWindow = up.ContextWindow
				mi.MaxTokens = up.MaxTokens
				mi.ContextFromAPI = true
			}
			out = append(out, mi)
			continue
		}
		out = append(out, rowToModelInfo(r))
	}
	return out
}
