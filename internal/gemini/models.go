// models.go 模型档表与静态模型清单。
//
// 协议依据：事实文档 docs/ref-ofm-渠道-zcode-gemini.md §2.6 与参考实现
// gemini.ts:404-554 / :572-570（档位表、档位→预算、模型 id 归一、静态表）。
package gemini

import (
	"strings"

	"wild-work/internal/provider"
)

// StaticModels 静态模型表（协议 §2.6：**纯静态表**，用户 2026-10-03 拍板不拉远端目录）。
//
// 只暴露**一条裸名**模型（`gemini-3.8-flash`），档位走 `ReasoningEfforts` 下拉框；
// 刻意**不**暴露 4 个带档位后缀的模型名，也**不**暴露 lite —— 实测
// `gemini-3.8-flash-lite` 在两端点上恒 404（与是否传档位无关），暴露它只会让用户
// 选中即失败（协议 §2.6）。
//
// 字段口径：
//   - `ContextFromAPI=false`：这是本工具内嵌的静态表，不是上游下发的目录值；
//   - `CanDisableThinking=&false`：上游「关闭思考」是**假关**（关掉照样思考照样计费，
//     实测 195 token），故如实声明「不可关闭」，与 minimax forced_on 档同款表达；
//   - `MaxTokens=64_000`：产品定义值（原版 65536 已被用户拍板改成 64000）。
func StaticModels() []provider.ModelInfo {
	canDisableThinking := false
	return []provider.ModelInfo{{
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
	}}
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

// canonicalModelID 剥掉**已知档位后缀**，得到目录裸名（gemini.ts:513-518）。
//
// 用途：客户端/面板/探测链路可能传来带档位的名字（`gemini-3.8-flash-high`），
// 校验前必须先归一，否则「测一下这个档位」会被自己误拒。
//
// ⚠️ 只剥已知档位后缀：将来加第二个模型时，若其真名恰好以档位词结尾
// （如 `gemini-x-high`），这里会把它误剥成 `gemini-x` —— 届时必须改成
// 「按静态表全名优先匹配」再回落剥后缀。
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
