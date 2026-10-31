package gateway

import (
	"testing"
)

// 本组用例锁定 Anthropic 入站的「工具结果」与「同一轮新指令」的出站顺序。
//
// 背景：Chat 协议要求 assistant.tool_calls 之后紧跟同批 role:tool 消息。
// 上游（workbuddy 明确返回 code 11148 "tool calls and tool results do not match,
// please start a new conversation and retry"；qoder 系只回信封层的
// "Error in upstream response"）会拒收中间插了别的消息的请求。
// 而 Anthropic 允许客户端把 tool_result 与新一轮文本放在同一条 user 消息里
// （Claude Code 在「工具执行完 + 用户又发指令/附图」时就是这么发的）。
//
// 因此转换层必须保证：文本/图片排在工具结果之后，而不是之前。

func toolUseBlocks() []any {
	return []any{
		map[string]any{"type": "tool_use", "id": "t1", "name": "bash", "input": map[string]any{"command": "ls"}},
		map[string]any{"type": "tool_use", "id": "t2", "name": "read", "input": map[string]any{"path": "a.txt"}},
	}
}

// 一条 user 里同时有多个 tool_result 与新一轮文本：必须转成 tool, tool, user，
// 且文本那条必须排在最后。
func TestAnthropicToolResultThenTextOrder(t *testing.T) {
	content := []any{
		map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "out-1"},
		map[string]any{"type": "tool_result", "tool_use_id": "t2", "content": "out-2"},
		map[string]any{"type": "text", "text": "继续"},
	}
	msgs, err := convertAnthropicMessage("user", content, 0)
	if err != nil {
		t.Fatalf("convertAnthropicMessage 报错: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("应产出 3 条消息（2 条 tool + 1 条 user），实得 %d 条: %#v", len(msgs), msgs)
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "tool" || first["tool_call_id"] != "t1" {
		t.Fatalf("msgs[0] 应为 tool(t1)，实为 %#v", first)
	}
	second := msgs[1].(map[string]any)
	if second["role"] != "tool" || second["tool_call_id"] != "t2" {
		t.Fatalf("msgs[1] 应为 tool(t2)，实为 %#v", second)
	}
	last := msgs[2].(map[string]any)
	if last["role"] != "user" || asString(last["content"]) != "继续" {
		t.Fatalf("msgs[2] 应为 user(\"继续\")，实为 %#v", last)
	}
}

// 纯文本 user 不受影响，仍是一条 user。
func TestAnthropicTextOnlyUnaffected(t *testing.T) {
	msgs, err := convertAnthropicMessage("user", []any{
		map[string]any{"type": "text", "text": "你好"},
	}, 0)
	if err != nil {
		t.Fatalf("convertAnthropicMessage 报错: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("应产出 1 条消息，实得 %d 条: %#v", len(msgs), msgs)
	}
	m := msgs[0].(map[string]any)
	if m["role"] != "user" || asString(m["content"]) != "你好" {
		t.Fatalf("应为 user(\"你好\")，实为 %#v", m)
	}
}

// 只有工具结果时必须只产出 tool 消息，不得凭空造一条空 user
// （空 content 的 user 在部分上游同样会被拒）。
func TestAnthropicToolResultOnlyNoExtraUser(t *testing.T) {
	msgs, err := convertAnthropicMessage("user", []any{
		map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "out-1"},
	}, 0)
	if err != nil {
		t.Fatalf("convertAnthropicMessage 报错: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("应只产出 1 条 tool 消息，实得 %d 条: %#v", len(msgs), msgs)
	}
	m := msgs[0].(map[string]any)
	if m["role"] != "tool" {
		t.Fatalf("应为 tool，实为 %#v", m)
	}
}

// 工具结果 + 图片 + 文本：图片与文本合并成一条 user 块数组，且必须排在结果之后。
// 注意源块顺序刻意写成「结果、文本、图片」，转换后文本仍在图片前（见 imageslim 约定）。
func TestAnthropicToolResultImageThenText(t *testing.T) {
	content := []any{
		map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "out-1"},
		map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": "image/png", "data": tinyPNGBase64(),
		}},
		map[string]any{"type": "text", "text": "这张图是什么"},
	}
	msgs, err := convertAnthropicMessage("user", content, 0)
	if err != nil {
		t.Fatalf("convertAnthropicMessage 报错: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("应产出 2 条消息（1 条 tool + 1 条带图 user），实得 %d 条: %#v", len(msgs), msgs)
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "tool" {
		t.Fatalf("msgs[0] 应为 tool，实为 %#v", first)
	}
	last := msgs[1].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("msgs[1] 应为 user，实为 %#v", last)
	}
	blocks := contentBlocksOf(t, last["content"])
	var hasText, hasImage bool
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch bm["type"] {
		case "text":
			if asString(bm["text"]) == "这张图是什么" {
				hasText = true
			}
		case "image_url":
			hasImage = true
		}
	}
	if !hasText || !hasImage {
		t.Fatalf("带图 user 应同时含 text 与 image_url 块，实为 %#v", blocks)
	}
}

// assistant 侧同时有文本和 tool_use：仍是单条 assistant，content 保留、tool_calls 齐全。
func TestAnthropicAssistantToolUseWithText(t *testing.T) {
	content := append([]any{
		map[string]any{"type": "text", "text": "我来跑两个命令"},
	}, toolUseBlocks()...)
	msgs, err := convertAnthropicMessage("assistant", content, 0)
	if err != nil {
		t.Fatalf("convertAnthropicMessage 报错: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("应产出 1 条 assistant，实得 %d 条: %#v", len(msgs), msgs)
	}
	m := msgs[0].(map[string]any)
	if m["role"] != "assistant" {
		t.Fatalf("应为 assistant，实为 %#v", m)
	}
	if asString(m["content"]) != "我来跑两个命令" {
		t.Fatalf("assistant content 应保留文本，实为 %#v", m["content"])
	}
	tc, ok := m["tool_calls"].([]any)
	if !ok || len(tc) != 2 {
		t.Fatalf("tool_calls 应有 2 项，实为 %#v", m["tool_calls"])
	}
}

// tinyPNGBase64 复用 tool_image_test.go 里的 tinyPNG 常量，取其中 data: 之后的部分。
func tinyPNGBase64() string {
	const prefix = "base64,"
	for i := 0; i+len(prefix) <= len(tinyPNG); i++ {
		if tinyPNG[i:i+len(prefix)] == prefix {
			return tinyPNG[i+len(prefix):]
		}
	}
	return ""
}
