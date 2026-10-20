package officeace

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRewriteBodyNormalizesDeveloperRole 锁定：officeace 必须把 messages 里的
// developer 角色归一为 system 后再发给 ModelArts。
//
// 背景（2026-10-18 实测 400）：
//
//	400 ModelArts.81001
//	Failed to format request body, message role is 'developer',
//	not one of ['system', 'assistant', 'function', 'user', 'tool']
//
// ModelArts 对 role 做白名单校验，developer 不在名单内。developer 是 OpenAI 新规范里
// system 的别名（Codex / Cursor 等新客户端用它承载 system 级指令），改写为 system 不丢语义。
//
// 这是全仓唯一漏做的渠道：internal/upstream/payload.go:119 的 normalizeRoles 早就写了
// 这条规则，另有 10 处渠道各自实现了一份；officeace 既不调 PrepareBody、也没复制这段，
// 于是把 developer 原样透传 ⇒ DSH 一旦发了 developer 角色就必 400。
func TestRewriteBodyNormalizesDeveloperRole(t *testing.T) {
	// DSH 的真实形状：系统提示位用 developer 角色。
	raw := []byte(`{
		"model": "deepseek-v4.1-flash",
		"messages": [
			{"role": "developer", "content": "You are a helpful assistant."},
			{"role": "user", "content": "hi"}
		],
		"stream": false
	}`)

	out := rewriteBody(raw)

	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("解析改写结果失败：%v\n%s", err, out)
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("改写后 messages 缺失：%s", out)
	}

	first, _ := msgs[0].(map[string]any)
	if role, _ := first["role"].(string); role != "system" {
		t.Errorf("❌ developer 未归一：期望 role=system，实得 role=%q\n"+
			"   这会在 ModelArts 上触发 400 ModelArts.81001（role 白名单不含 developer）", role)
	}

	// 反向保护：其余 role 必须原样保留，不得被误改（不合并、不重排、不删消息）。
	if len(msgs) != 2 {
		t.Errorf("❌ 消息条数被改动：期望 2，实得 %d", len(msgs))
	}
	second, _ := msgs[1].(map[string]any)
	if role, _ := second["role"].(string); role != "user" {
		t.Errorf("❌ user 角色被误改：实得 %q", role)
	}
	if c, _ := second["content"].(string); c != "hi" {
		t.Errorf("❌ user 内容被改动：实得 %q", c)
	}
	// 内容本身不得被改写（只改 role 一个字段）。
	if c, _ := first["content"].(string); c != "You are a helpful assistant." {
		t.Errorf("❌ developer 的 content 被改动：实得 %q", c)
	}
}

// TestRewriteBodyIdempotentForRoles 归一化必须幂等：重复改写不得改变结果。
//
// 为什么需要这条：officeace 现在显式调 upstream.NormalizeRoles，而将来若改成走
// upstream.PrepareBody（内部也会调同款归一），就会发生「双重归一」。若实现不小心
// （例如把 system 也当 developer 处理、或反复包一层），会出现 role 被改成怪值。
// 实测实现只匹配 developer 一个值，天然幂等 —— 本测试把这一点锁死。
func TestRewriteBodyIdempotentForRoles(t *testing.T) {
	raw := []byte(`{
		"model": "deepseek-v4.1-flash",
		"messages": [
			{"role": "developer", "content": "a"},
			{"role": "user", "content": "b"}
		]
	}`)
	once := rewriteBody(raw)
	twice := rewriteBody(once)
	if string(once) != string(twice) {
		t.Errorf("❌ 改写不幂等：\n  一次=%s\n  两次=%s", once, twice)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(twice), &body); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	msgs, _ := body["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	if role, _ := first["role"].(string); role != "system" {
		t.Errorf("❌ 二次改写后 role 异常：%q", role)
	}
}

// TestRewriteBodyKeepsOtherRoles 确保归一化没有波及非 developer 角色
// （尤其 tool / assistant 这些 ModelArts 白名单内的值必须逐字保留）。
func TestRewriteBodyKeepsOtherRoles(t *testing.T) {
	raw := []byte(`{
		"model": "deepseek-v4.1-flash",
		"messages": [
			{"role": "system", "content": "s"},
			{"role": "user", "content": "u"},
			{"role": "assistant", "content": "a"},
			{"role": "tool", "tool_call_id": "x", "content": "t"}
		]
	}`)
	out := rewriteBody(raw)
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	msgs, _ := body["messages"].([]any)
	want := []string{"system", "user", "assistant", "tool"}
	if len(msgs) != len(want) {
		t.Fatalf("消息数被改动：期望 %d，实得 %d", len(want), len(msgs))
	}
	for i, w := range want {
		m, _ := msgs[i].(map[string]any)
		if got, _ := m["role"].(string); got != w {
			t.Errorf("❌ idx=%d role 被误改：期望 %q，实得 %q", i, w, got)
		}
	}
	// 顺带确认原有 5 步改写仍在工作（stream 强制 true）。
	if s, _ := body["stream"].(bool); !s {
		t.Error("❌ 既有行为回归：stream 未被强制为 true")
	}
	if !strings.Contains(string(out), `"tool_call_id"`) {
		t.Error("❌ tool 消息的 tool_call_id 丢失")
	}
}
