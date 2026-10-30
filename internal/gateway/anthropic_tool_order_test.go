// anthropic_tool_order_test.go 回归：Anthropic user 消息里 tool_result 与 text
// 混排时，文本必须排在工具结果**之后**。
//
// Chat 协议要求 assistant.tool_calls 之后紧跟同批 role:tool，中间插入任何消息
// 都算配对断裂。Claude Code 每轮把「工具结果 + 新指令」放在同一条 user 消息里
// （[tool_result, tool_result, text]），旧实现把 text prepend 到结果之前，
// 出站变成 user→tool→tool，上游按「tool calls and tool results do not match」
// （workbuddy code 11148）整条拒收。该错误跟着请求体走，换号重试必然同样失败，
// 表现为会话彻底顶死。
package gateway

import "testing"

// TestAnthropicToolResultThenTextOrder 是本次修复的主护栏：两个工具结果 + 一条
// 新指令，必须产出 tool(t1), tool(t2), user("继续") 这个顺序。
func TestAnthropicToolResultThenTextOrder(t *testing.T) {
	msgs, err := convertAnthropicMessage("user", []any{
		map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok1"},
		map[string]any{"type": "tool_result", "tool_use_id": "t2", "content": "ok2"},
		map[string]any{"type": "text", "text": "继续"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("want 3 msgs (tool,tool,user), got %d: %#v", len(msgs), msgs)
	}
	for i, id := range []string{"t1", "t2"} {
		m := msgs[i].(map[string]any)
		if m["role"] != "tool" || m["tool_call_id"] != id {
			t.Fatalf("msgs[%d] 应为 tool(%s)，实为 %#v", i, id, m)
		}
	}
	last := msgs[2].(map[string]any)
	if last["role"] != "user" || last["content"] != "继续" {
		t.Fatalf("msgs[2] 应为 user(继续)，实为 %#v", last)
	}
}

// TestAnthropicTextOnlyUnaffected 纯文本 user 消息不受影响，仍是一条 user。
func TestAnthropicTextOnlyUnaffected(t *testing.T) {
	msgs, err := convertAnthropicMessage("user", []any{
		map[string]any{"type": "text", "text": "你好"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d: %#v", len(msgs), msgs)
	}
	if m := msgs[0].(map[string]any); m["role"] != "user" || m["content"] != "你好" {
		t.Fatalf("msgs[0] = %#v", m)
	}
}

// TestAnthropicToolResultOnlyNoExtraUser 纯工具结果不得额外造一条空 user 消息
// （凭空多出来的空 user 同样是配对断裂的一种形态）。
func TestAnthropicToolResultOnlyNoExtraUser(t *testing.T) {
	msgs, err := convertAnthropicMessage("user", []any{
		map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d: %#v", len(msgs), msgs)
	}
	if m := msgs[0].(map[string]any); m["role"] != "tool" || m["tool_call_id"] != "t1" {
		t.Fatalf("msgs[0] = %#v", m)
	}
}

// TestAnthropicToolResultImageThenText 工具结果里连图带字时，图片块走 vision
// 通道（R108 既有行为），但整条消息同样必须排在工具结果之后。
func TestAnthropicToolResultImageThenText(t *testing.T) {
	msgs, err := convertAnthropicMessage("user", []any{
		map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "ok"},
		map[string]any{"type": "image", "source": map[string]any{
			"type":       "base64",
			"media_type": "image/png",
			"data":       tinyPNG,
		}},
		map[string]any{"type": "text", "text": "看图"},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 msgs (tool,user), got %d: %#v", len(msgs), msgs)
	}
	if m := msgs[0].(map[string]any); m["role"] != "tool" {
		t.Fatalf("msgs[0] 应为 tool，实为 %#v", m)
	}
	m := msgs[1].(map[string]any)
	if m["role"] != "user" {
		t.Fatalf("msgs[1] 应为 user，实为 %#v", m)
	}
	blocks, ok := m["content"].([]any)
	if !ok {
		t.Fatalf("含图片的 user 消息 content 应为块数组，实为 %#v", m["content"])
	}
	var sawText, sawImage bool
	for _, b := range blocks {
		bm, _ := b.(map[string]any)
		switch bm["type"] {
		case "text":
			sawText = true
		case "image_url":
			sawImage = true
		}
	}
	if !sawText || !sawImage {
		t.Fatalf("want text+image_url blocks, got %#v", blocks)
	}
}

// TestAnthropicAssistantToolUseWithText assistant 分支不变：文本进 assistant.content，
// 且 assistant 消息仍排在 tool 结果之前。
func TestAnthropicAssistantToolUseWithText(t *testing.T) {
	msgs, err := convertAnthropicMessage("assistant", []any{
		map[string]any{"type": "text", "text": "我来调用工具"},
		map[string]any{"type": "tool_use", "id": "t1", "name": "Bash", "input": map[string]any{"cmd": "ls"}},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d", len(msgs))
	}
	m := msgs[0].(map[string]any)
	if m["role"] != "assistant" || m["content"] != "我来调用工具" {
		t.Fatalf("assistant content = %#v", m["content"])
	}
	if tcs, _ := m["tool_calls"].([]any); len(tcs) != 1 {
		t.Fatalf("tool_calls = %#v", m["tool_calls"])
	}
}
