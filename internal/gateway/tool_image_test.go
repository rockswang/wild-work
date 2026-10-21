// tool_image_test.go 工具结果里的图片必须走 vision 通道（issue #178）。
//
// 背景：Codex 的 view_image 把截图作为 input_image 块放进 function_call_output。
// 旧实现（stringifyOutput）取不到文本块时回退 json.Marshal，整张 PNG 的 base64
// 变成一条超长文本消息——实测 1.6MB base64 ≈ 146 万 token，直接把 1M 上下文的
// 模型打成 `prompt is too long: ... > 1048576 maximum`；同一张图按 vision 只花约 1K。
//
// 这里锁住四条不变量：
//  1. 图片块保留成 Chat 的 image_url 内容块（不是文本）；
//  2. 纯文本工具结果仍是 string（零行为变化，兼容只认 string 的上游）；
//  3. 只有图片时补一条文本占位（剥离非 text 块的渠道不至于拿到空 content）；
//  4. Anthropic 的 tool_result 图片（source.base64）同样保留为 image_url。
package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// tinyPNG 是一张 1x1 PNG 的 data URL（base64 内容不重要，只看形态）。
const tinyPNG = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// contentBlocksOf 断言 content 是块数组并返回之。
func contentBlocksOf(t *testing.T, v any) []any {
	t.Helper()
	blocks, ok := v.([]any)
	if !ok {
		t.Fatalf("content 应为块数组（图片走 vision），实际 %T: %v", v, v)
	}
	return blocks
}

// TestToolOutputImageKeptAsVisionBlock 回归 issue #178：function_call_output 里的图片
// 必须转成 image_url 块，且绝不能被序列化成文本。
func TestToolOutputImageKeptAsVisionBlock(t *testing.T) {
	msgs, err := convertResponsesItems([]any{
		map[string]any{"type": "message", "role": "user", "content": []any{
			map[string]any{"type": "input_text", "text": "look"},
		}},
		map[string]any{"type": "function_call", "call_id": "c1", "name": "view_image", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": []any{
			map[string]any{"type": "input_image", "image_url": tinyPNG, "detail": "high"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d, want 3", len(msgs))
	}
	tool, _ := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "c1" {
		t.Fatalf("messages[2] = %v", tool)
	}
	if s, isStr := tool["content"].(string); isStr {
		t.Fatalf("工具结果的图片被序列化成了字符串（长度 %d）——这正是 issue #178 的形态", len(s))
	}
	blocks := contentBlocksOf(t, tool["content"])
	// 第一块是文本占位，第二块是图片。
	var img map[string]any
	for _, b := range blocks {
		bm, _ := b.(map[string]any)
		if bm["type"] == "image_url" {
			img = bm
		}
	}
	if img == nil {
		t.Fatalf("未找到 image_url 块: %v", blocks)
	}
	inner, _ := img["image_url"].(map[string]any)
	if inner["url"] != tinyPNG {
		t.Errorf("image_url.url = %v, want 原 data URL", inner["url"])
	}
	if inner["detail"] != "high" {
		t.Errorf("image_url.detail = %v, want high（必须透传，否则客户端指定的分辨率被抹掉）", inner["detail"])
	}
}

// TestToolOutputImageOnlyGetsTextPlaceholder 只有图片时必须补文本占位，
// 否则「只保留 text 块」的渠道（codearts 等）会拿到空 content。
func TestToolOutputImageOnlyGetsTextPlaceholder(t *testing.T) {
	v := toolContentValue([]any{
		map[string]any{"type": "input_image", "image_url": tinyPNG},
	})
	blocks := contentBlocksOf(t, v)
	if len(blocks) < 2 {
		t.Fatalf("blocks = %v, want 文本占位 + 图片", blocks)
	}
	first, _ := blocks[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "[image]" {
		t.Errorf("首块 = %v, want {type:text,text:[image]}", first)
	}
}

// TestToolOutputTextOnlyStaysString 纯文本工具结果保持 string（零行为变化）。
func TestToolOutputTextOnlyStaysString(t *testing.T) {
	cases := []any{
		"hello",
		[]any{map[string]any{"type": "input_text", "text": "line1"},
			map[string]any{"type": "output_text", "text": "line2"}},
		[]any{map[string]any{"type": "text", "text": "only"}},
	}
	for i, in := range cases {
		got := toolContentValue(in)
		if _, ok := got.(string); !ok {
			t.Errorf("case %d: 纯文本应保持 string，实际 %T: %v", i, got, got)
		}
	}
	if got := toolContentValue("hello"); got != "hello" {
		t.Errorf("string 原样返回失败: %v", got)
	}
	joined := toolContentValue([]any{
		map[string]any{"type": "input_text", "text": "a"},
		map[string]any{"type": "input_text", "text": "b"},
	})
	if joined != "a\nb" {
		t.Errorf("多文本块拼接 = %q, want \"a\\nb\"", joined)
	}
}

// TestToolOutputImageAndTextKeepsBoth 图文混合时文本与图片都要在，且顺序保持。
func TestToolOutputImageAndTextKeepsBoth(t *testing.T) {
	v := toolContentValue([]any{
		map[string]any{"type": "input_text", "text": "截图如下"},
		map[string]any{"type": "input_image", "image_url": map[string]any{"url": tinyPNG, "detail": "original"}},
	})
	blocks := contentBlocksOf(t, v)
	if len(blocks) != 2 {
		t.Fatalf("blocks len = %d, want 2: %v", len(blocks), blocks)
	}
	if b0, _ := blocks[0].(map[string]any); b0["text"] != "截图如下" {
		t.Errorf("文本块丢失: %v", blocks[0])
	}
	img, _ := blocks[1].(map[string]any)
	inner, _ := img["image_url"].(map[string]any)
	if inner["url"] != tinyPNG || inner["detail"] != "original" {
		t.Errorf("图片块 = %v", img)
	}
}

// TestToolOutputNonArrayFallback 非 string/数组（对象等）仍按 JSON 字面量兜底。
func TestToolOutputNonArrayFallback(t *testing.T) {
	got := toolContentValue(map[string]any{"ok": true})
	s, ok := got.(string)
	if !ok {
		t.Fatalf("对象输出应兜底成 string，实际 %T", got)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(s), &back); err != nil || back["ok"] != true {
		t.Errorf("兜底 JSON 不可解析或内容不对: %q", s)
	}
	if got := toolContentValue(nil); got != "" {
		t.Errorf("nil → 空串，实际 %v", got)
	}
}

// TestAnthropicToolResultImageKept Anthropic 侧 tool_result 的图片同样走 vision。
func TestAnthropicToolResultImageKept(t *testing.T) {
	msgs, err := convertAnthropicMessage("user", []any{
		map[string]any{"type": "tool_result", "tool_use_id": "tu1", "content": []any{
			map[string]any{"type": "text", "text": "这是截图"},
			map[string]any{"type": "image", "source": map[string]any{
				"type": "base64", "media_type": "image/png",
				"data": strings.TrimPrefix(tinyPNG, "data:image/png;base64,"),
			}},
		}},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("msgs len = %d, want 1", len(msgs))
	}
	tool, _ := msgs[0].(map[string]any)
	if tool["role"] != "tool" {
		t.Fatalf("role = %v, want tool", tool["role"])
	}
	blocks := contentBlocksOf(t, tool["content"])
	var img map[string]any
	for _, b := range blocks {
		if bm, _ := b.(map[string]any); bm["type"] == "image_url" {
			img = bm
		}
	}
	if img == nil {
		t.Fatalf("Anthropic tool_result 图片未转成 image_url 块: %v", blocks)
	}
	inner, _ := img["image_url"].(map[string]any)
	if u, _ := inner["url"].(string); !strings.HasPrefix(u, "data:image/png;base64,") {
		t.Errorf("image url = %q, want data URL", u)
	}
}

// TestToolOutputImagePayloadIsNotText 端到端：请求体里图片不能以「文本消息」形态出现。
// 取一段足够大的假 base64，断言 messages[2].content 不是字符串（否则 token 爆炸）。
func TestToolOutputImagePayloadIsNotText(t *testing.T) {
	bigB64 := "data:image/png;base64," + strings.Repeat("A", 200000)
	req, err := responsesToChat(map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "go"},
			}},
			map[string]any{"type": "function_call", "call_id": "c1", "name": "view_image", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "c1", "output": []any{
				map[string]any{"type": "input_image", "image_url": bigB64},
			}},
		},
	}, "workbuddyai/deepseek-v4.1-flash", 0)
	if err != nil {
		t.Fatal(err)
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d", len(msgs))
	}
	tool, _ := msgs[2].(map[string]any)
	if _, isStr := tool["content"].(string); isStr {
		t.Fatal("20 万字符的 base64 被当成文本发出——token 会按文本计费，issue #178 复发")
	}
}
