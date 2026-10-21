// models.go 模型档表与静态模型清单。
//
// 协议依据：事实文档 docs/ref-ofm-渠道-zcode-gemini.md §2.6 与参考实现
// gemini.ts:404-554 / :572-570（档位表、档位→预算、模型 id 归一、静态表）。
package gemini

import (
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

// resolveModelID 将用户传入的模型 ID（可能带档位后缀或别名）映射为上游接受的模型 ID 和对应档位。
func resolveModelID(rawID, effort string) (upstreamModel string, resolvedTier string, ok bool) {
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

	// 2. 处理别名与带档位后缀的 gemini-3.8-flash
	for _, t := range []string{TierLow, TierMedium, TierHigh, TierTiered} {
		if strings.EqualFold(s, ModelFlash+"-"+t) {
			return ModelFlash + "-" + t, t, true
		}
	}

	// 3. 常见别名映射（参考 OmniRoute ANTIGRAVITY_MODEL_ALIASES）
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
