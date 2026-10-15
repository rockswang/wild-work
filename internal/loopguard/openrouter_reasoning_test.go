// openrouter_reasoning_test.go ReasoningDeltaOf 对 OpenRouter 系字段名的
// 提取兼容（2026-10-13）。
//
// 背景：Cline 免费池经 OpenRouter 下发思考增量的字段是 `delta.reasoning`
// 与 `delta.reasoning_details`，不是 `delta.reasoning_content`。守卫此前
// 只读后者 ⇒ 在该渠道**永远喂不进**、防癫拦截完全失效（与 upstream
// 白名单丢弃思考是同一个根因的两面）。本文件锁死三种字段名的提取与优先级。
package loopguard

import "testing"

// chunkOf 构造 choices[0].delta 为给定 delta 的 chunk。
func chunkOf(delta map[string]any) map[string]any {
	return map[string]any{
		"choices": []any{map[string]any{"index": float64(0), "delta": delta}},
	}
}

// TestReasoningDeltaOfFieldNames 三种上游字段名都能提取。
func TestReasoningDeltaOfFieldNames(t *testing.T) {
	cases := []struct {
		name  string
		delta map[string]any
		want  string
	}{
		{"reasoning_content（DeepSeek/WorkBuddy 系）",
			map[string]any{"reasoning_content": "思考A"}, "思考A"},
		{"reasoning（OpenRouter 归一化字段）",
			map[string]any{"reasoning": "思考B"}, "思考B"},
		{"reasoning_details.summary（OpenRouter 结构化块）",
			map[string]any{"reasoning_details": []any{
				map[string]any{"type": "reasoning.summary", "summary": "思考C"},
			}}, "思考C"},
		{"reasoning_details.text（原始全文，优先于 summary）",
			map[string]any{"reasoning_details": []any{
				map[string]any{"type": "reasoning.text", "text": "全文D"},
				map[string]any{"type": "reasoning.summary", "summary": "摘要D"},
			}}, "全文D"},
		{"reasoning_details.encrypted 无可读文本",
			map[string]any{"reasoning_details": []any{
				map[string]any{"type": "reasoning.encrypted", "data": "Q-PaDgGE"},
			}}, ""},
		{"空 delta", map[string]any{}, ""},
		{"reasoning 为 null（实测帧形态）",
			map[string]any{"reasoning": nil}, ""},
	}
	for _, c := range cases {
		if got := ReasoningDeltaOf(chunkOf(c.delta)); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// TestReasoningDeltaOfPriority 三字段并存时按 reasoning_content >
// reasoning > reasoning_details.summary 取值（与 upstream.reasoningDeltaOf
// 同优先级；同内容并存时避免重复拼接）。
func TestReasoningDeltaOfPriority(t *testing.T) {
	delta := map[string]any{
		"reasoning_content": "首选",
		"reasoning":         "次选",
		"reasoning_details": []any{
			map[string]any{"type": "reasoning.summary", "summary": "末选"},
		},
	}
	if got := ReasoningDeltaOf(chunkOf(delta)); got != "首选" {
		t.Fatalf("并存时取 %q want 首选", got)
	}
	delete(delta, "reasoning_content")
	if got := ReasoningDeltaOf(chunkOf(delta)); got != "次选" {
		t.Fatalf("降级后取 %q want 次选", got)
	}
}

// TestReasoningDeltaOfShapeTolerance 形状缺失不得 panic（防御性：各渠道
// 解出的 chunk 形状不完全一致）。
func TestReasoningDeltaOfShapeTolerance(t *testing.T) {
	for _, obj := range []map[string]any{
		{},
		{"choices": []any{}},
		{"choices": "not-a-list"},
		{"choices": []any{map[string]any{"delta": nil}}},
		{"choices": []any{"not-a-map"}},
	} {
		if got := ReasoningDeltaOf(obj); got != "" {
			t.Errorf("%v: got %q want empty", obj, got)
		}
	}
}
