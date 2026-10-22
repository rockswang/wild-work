// models.go 模型档表与静态模型清单。
//
// 协议依据：事实文档 docs/ref-ofm-渠道-zcode-gemini.md §2.6 与参考实现
// gemini.ts:404-554 / :572-570（档位表、档位→预算、模型 id 归一、静态表）。
package gemini

import (
	"encoding/json"
	"strings"

	"wild-work/internal/provider"
)

// StaticModels 静态模型表（参考 OmniRoute ANTIGRAVITY_SHARED_MODELS 与协议规范）。
//
// 包含 Gemini 3.8 Flash、Gemini 3.7 Flash、Gemini 3.1 Pro、Gemini 3.1 Flash Lite、
// Claude (Sonnet 4.6 / Opus 4.6 Thinking / 5.5 系列) 及 GPT-OSS 等 Antigravity 渠道支持的模型。
func StaticModels() []provider.ModelInfo {
	canDisableThinking := false
	canDisableClaudeThinking := true
	return []provider.ModelInfo{
		{
			ID:                     ModelFlash,
			Name:                   ModelDisplayName,
			ContextWindow:          ContextWindow,
			MaxTokens:              MaxOutputTokens,
			ContextFromAPI:         false,
			SupportsImages:         true,
			SupportsReasoning:      true,
			SupportsTools:          true,
			ReasoningEfforts:       []string{TierLow, TierMedium, TierHigh, TierTiered},
			DefaultReasoningEffort: TierMedium,
			CanDisableThinking:     &canDisableThinking,
		},
		{
			ID:                     "gemini-3.7-flash-tiered",
			Name:                   "Gemini 3.7 Flash (Tiered)",
			ContextWindow:          ContextWindow,
			MaxTokens:              MaxOutputTokens,
			ContextFromAPI:         false,
			SupportsImages:         true,
			SupportsReasoning:      true,
			SupportsTools:          true,
			ReasoningEfforts:       []string{TierLow, TierMedium, TierHigh, TierTiered},
			DefaultReasoningEffort: TierMedium,
			CanDisableThinking:     &canDisableThinking,
		},
		{
			ID:                     "gemini-pro-agent",
			Name:                   "Gemini 3.1 Pro (High)",
			ContextWindow:          ContextWindow,
			MaxTokens:              MaxOutputTokens,
			ContextFromAPI:         false,
			SupportsImages:         true,
			SupportsReasoning:      true,
			SupportsTools:          true,
		},
		{
			ID:                     "gemini-3.1-pro-low",
			Name:                   "Gemini 3.1 Pro (Low)",
			ContextWindow:          ContextWindow,
			MaxTokens:              MaxOutputTokens,
			ContextFromAPI:         false,
			SupportsImages:         true,
			SupportsReasoning:      true,
			SupportsTools:          true,
		},
		{
			ID:                     "gemini-3.1-flash-lite",
			Name:                   "Gemini 3.1 Flash Lite",
			ContextWindow:          ContextWindow,
			MaxTokens:              MaxOutputTokens,
			ContextFromAPI:         false,
			SupportsImages:         true,
			SupportsTools:          true,
		},
		{
			ID:                     "claude-sonnet-4-6",
			Name:                   "Claude Sonnet 4.6 (Thinking)",
			ContextWindow:          ContextWindow,
			MaxTokens:              MaxOutputTokens,
			ContextFromAPI:         false,
			SupportsImages:         true,
			SupportsReasoning:      true,
			SupportsTools:          true,
			CanDisableThinking:     &canDisableClaudeThinking,
		},
		{
			ID:                     "claude-opus-4-6-thinking",
			Name:                   "Claude Opus 4.6 (Thinking)",
			ContextWindow:          ContextWindow,
			MaxTokens:              MaxOutputTokens,
			ContextFromAPI:         false,
			SupportsImages:         true,
			SupportsReasoning:      true,
			SupportsTools:          true,
		},
		{
			ID:                     "gpt-oss-120b-medium",
			Name:                   "GPT-OSS 120B (Medium)",
			ContextWindow:          131072,
			MaxTokens:              32768,
			ContextFromAPI:         false,
			SupportsReasoning:      true,
			SupportsTools:          true,
		},
		{
			ID:                     "gemini-4-argon-high",
			Name:                   "Gemini 4 Argon (High)",
			ContextWindow:          ContextWindow,
			MaxTokens:              MaxOutputTokens,
			ContextFromAPI:         false,
			SupportsImages:         true,
			SupportsReasoning:      true,
			SupportsTools:          true,
		},
		{
			ID:                     "gemini-4-argon-medium",
			Name:                   "Gemini 4 Argon (Medium)",
			ContextWindow:          ContextWindow,
			MaxTokens:              MaxOutputTokens,
			ContextFromAPI:         false,
			SupportsImages:         true,
			SupportsReasoning:      true,
			SupportsTools:          true,
		},
		{
			ID:                     "gemini-4-argon-low",
			Name:                   "Gemini 4 Argon (Low)",
			ContextWindow:          ContextWindow,
			MaxTokens:              MaxOutputTokens,
			ContextFromAPI:         false,
			SupportsImages:         true,
			SupportsReasoning:      true,
			SupportsTools:          true,
		},
		{
			ID:                     "claude-opus-5-5-high",
			Name:                   "Claude Opus 5.5 (High)",
			ContextWindow:          ContextWindow,
			MaxTokens:              MaxOutputTokens,
			ContextFromAPI:         false,
			SupportsImages:         true,
			SupportsReasoning:      true,
			SupportsTools:          true,
		},
		{
			ID:                     "claude-sonnet-5-5-high",
			Name:                   "Claude Sonnet 5.5 (High)",
			ContextWindow:          ContextWindow,
			MaxTokens:              MaxOutputTokens,
			ContextFromAPI:         false,
			SupportsImages:         true,
			SupportsReasoning:      true,
			SupportsTools:          true,
		},
	}
}

// tierOf 把思考档位归一成已知四档之一；未知/缺失一律 medium
// （gemini.ts:478-483 的默认分支）。
func tierOf(effort string) string {
	switch strings.ToLower(trimSpace(effort)) {
	case TierLow:
		return TierLow
	case TierMedium:
		return TierMedium
	case TierHigh:
		return TierHigh
	case TierTiered:
		return TierTiered
	default:
		return TierMedium
	}
}

// thinkingBudget 档位 → thinkingBudget；负数（BudgetTiered）表示**本档不下发该字段**
// （gemini.ts:486-491：tiered 只发 includeThoughts，预算交给上游自适应）。
func thinkingBudget(tier string) int64 {
	switch tier {
	case TierLow:
		return BudgetLow
	case TierHigh:
		return BudgetHigh
	case TierTiered:
		return BudgetTiered
	default:
		return BudgetMedium
	}
}

// isDiscoverableAgyModelID 判断模型是否可作为聊天模型被客户端发现和使用。
// 规则参考 OmniRoute isDiscoverableAgyModelId：
// 排除 internal 内部调试模型、chat_ 前缀、tab_ 前缀、_weekly 配额桶、多媒体/嵌入模型以及退役废弃模型。
func isDiscoverableAgyModelID(id string) bool {
	s := strings.TrimSpace(id)
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "chat_") || strings.HasPrefix(lower, "tab_") || strings.HasSuffix(lower, "_weekly") {
		return false
	}
	switch lower {
	case "tab_flash_lite_preview", "tab_jump_flash_lite_preview",
		"gemini-3-pro-preview", "gemini-3.1-pro",
		"gemini-3.7-flash", "gemini-3.7-flash-high", "gemini-3.7-flash-medium", "gemini-3.7-flash-low",
		"gemini-3.6-flash-high", "gemini-3.6-flash-medium", "gemini-3.6-flash-low",
		"gemini-3-flash-agent", "gemini-3.5-flash", "gemini-3.5-flash-extra-low", "gemini-3.5-flash-low",
		"gemini-3.5-flash-high", "gemini-3.5-flash-medium", "gemini-3.5-flash-preview",
		"gemini-2.5-pro", "gemini-2.5-flash-thinking", "gemini-2.5-flash", "gemini-2.5-flash-lite",
		"gemini-2.5-computer-use-preview-10-2025":
		return false
	}
	// 排除图像、视频、音频、嵌入等非 chat 模型
	for _, kw := range []string{"image", "imagen", "audio", "tts", "embedding", "embed", "video", "veo"} {
		if strings.Contains(lower, kw) {
			return false
		}
	}
	return true
}

// formatDisplayName 为远端拉取的模型生成友好展示名
func formatDisplayName(id, upstreamDisplayName string) string {
	if upstreamDisplayName != "" && upstreamDisplayName != id {
		return upstreamDisplayName
	}
	for _, sm := range StaticModels() {
		if strings.EqualFold(sm.ID, id) {
			return sm.Name
		}
	}
	return id
}

// resolveModelID 将用户传入的模型 ID（可能带档位后缀或别名）映射为上游接受的模型 ID 和对应档位。
func resolveModelID(rawID, effort string) (upstreamModel string, resolvedTier string, ok bool) {
	return resolveModelIDWithCatalog(rawID, effort, nil)
}

// resolveModelIDWithCatalog 支持结合动态拉取的远端目录进行解析。
func resolveModelIDWithCatalog(rawID, effort string, extraModels []provider.ModelInfo) (upstreamModel string, resolvedTier string, ok bool) {
	s := trimSpace(rawID)
	tier := tierOf(effort)

	// 1. 优先精确匹配静态表中的模型
	for _, m := range StaticModels() {
		if strings.EqualFold(m.ID, s) {
			// 如果是 gemini-3.8-flash，上游需要拼接档位后缀（如 gemini-3.8-flash-medium）
			if m.ID == ModelFlash {
				return ModelFlash + "-" + tier, tier, true
			}
			return m.ID, tier, true
		}
	}

	// 2. 匹配动态拉取的模型
	for _, m := range extraModels {
		if strings.EqualFold(m.ID, s) {
			if m.ID == ModelFlash {
				return ModelFlash + "-" + tier, tier, true
			}
			return m.ID, tier, true
		}
	}

	// 3. 处理别名与带档位后缀的 gemini-3.8-flash
	for _, t := range []string{TierLow, TierMedium, TierHigh, TierTiered} {
		if strings.EqualFold(s, ModelFlash+"-"+t) {
			return ModelFlash + "-" + t, t, true
		}
	}

	// 4. 常见别名映射（参考 OmniRoute ANTIGRAVITY_MODEL_ALIASES）
	switch strings.ToLower(s) {
	case "gemini-3.7-flash", "gemini-3.7-flash-high", "gemini-3.7-flash-medium", "gemini-3.7-flash-low":
		return "gemini-3.7-flash-tiered", tier, true
	case "gemini-3.1-pro-high":
		return "gemini-pro-agent", tier, true
	case "gpt-oss-120b":
		return "gpt-oss-120b-medium", tier, true
	case "claude-3-7-sonnet", "gemini-claude-sonnet-4-5", "claude-sonnet-4-5":
		return "claude-sonnet-4-6", tier, true
	case "claude-opus-4-5-thinking", "gemini-claude-opus-4-5-thinking":
		return "claude-opus-4-6-thinking", tier, true
	}

	return "", "", false
}

// canonicalModelID 兼容保留辅助函数
func canonicalModelID(id string) string {
	s := trimSpace(id)
	for _, tier := range []string{TierLow, TierMedium, TierHigh, TierTiered} {
		if strings.HasSuffix(s, "-"+tier) {
			return s[:len(s)-len(tier)-1]
		}
	}
	return s
}

// upstreamModelName 去掉客户端模型 ID 上的渠道前缀（`gemini/gemini-3.8-flash` →
// `gemini-3.8-flash`）。
//
// handler 已把上游名写回 body.model，这里再兜一次：手填/直连场景可能仍带前缀
// （同 minimax.upstreamModelName 的取舍）。
func upstreamModelName(id string) string {
	return trimSpace(strings.TrimPrefix(trimSpace(id), Kind.String()+"/"))
}

// rawModelItem 远端 /v1internal:fetchAvailableModels 返回的模型项
type rawModelItem struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	IsInternal  bool   `json:"isInternal"`
}

// rawModelsEnvelope fetchAvailableModels 响应解析（兼容对象 map 与列表数组）
type rawModelsEnvelope struct {
	Models map[string]rawModelItem `json:"models"`
}

// parseAvailableModelsResponse 解析上游模型响应
func parseAvailableModelsResponse(raw []byte) []provider.ModelInfo {
	if len(raw) == 0 {
		return nil
	}
	var env rawModelsEnvelope
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Models) > 0 {
		out := make([]provider.ModelInfo, 0, len(env.Models))
		for id, m := range env.Models {
			if m.IsInternal {
				continue
			}
			realID := id
			if realID == "" {
				realID = m.ID
			}
			if !isDiscoverableAgyModelID(realID) {
				continue
			}
			displayName := m.DisplayName
			if displayName == "" {
				displayName = realID
			}
			out = append(out, makeModelInfo(realID, displayName))
		}
		return out
	}

	// 备选兼容：直接是 rawModelItem 数组或 {"models": [...]}
	var altEnv struct {
		Models []rawModelItem `json:"models"`
	}
	if err := json.Unmarshal(raw, &altEnv); err == nil && len(altEnv.Models) > 0 {
		out := make([]provider.ModelInfo, 0, len(altEnv.Models))
		for _, m := range altEnv.Models {
			if m.IsInternal || !isDiscoverableAgyModelID(m.ID) {
				continue
			}
			out = append(out, makeModelInfo(m.ID, m.DisplayName))
		}
		return out
	}

	return nil
}

// makeModelInfo 构造从远端动态拉取的 ModelInfo。
//
// 恪守 R49「只透传上游声明，绝不编造」与 R119 能力来源规范：
// 动态拉取的目录仅承载上游明确返回的 ID 与展示名称。
// 严禁通过模型名称字串猜测 SupportsReasoning / ReasoningEfforts 等思考能力；
// 上游目录未下发 ContextWindow / MaxTokens 时不设 ContextFromAPI=true。
// 若该模型已在 StaticModels() 静态已知表中定义，优先复用静态表经过人工实测校验的能力声明。
func makeModelInfo(id, displayName string) provider.ModelInfo {
	// 如果在已知静态表中有定义，直接继承已知实测能力（包含思考档位与开关声明）
	for _, sm := range StaticModels() {
		if strings.EqualFold(sm.ID, id) {
			m := sm
			if displayName != "" && displayName != id {
				m.Name = displayName
			}
			return m
		}
	}

	// 纯动态发现的未知模型：宁缺勿编，仅保留基本标识与上游展示名
	return provider.ModelInfo{
		ID:             id,
		Name:           formatDisplayName(id, displayName),
		ContextFromAPI: false,
	}
}

// mergeModelCatalog 合并动态拉取的模型与静态兜底表
func mergeModelCatalog(dynamic, fallback []provider.ModelInfo) []provider.ModelInfo {
	seen := map[string]bool{}
	out := make([]provider.ModelInfo, 0, len(dynamic)+len(fallback))
	for _, m := range dynamic {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		out = append(out, m)
	}
	for _, m := range fallback {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		out = append(out, m)
	}
	return out
}
