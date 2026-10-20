package lobsterai

import (
	"encoding/json"
	"testing"
)

// TestBuildChatBodyNormalizesDeveloperRole 锁定：lobsterai 必须把 messages 里的
// developer 角色归一为 system 后再发给上游。
//
// 背景（2026-10-18 devdump 实测，同一账号 123905、同一模型 deepseek-flash）：
//
//	role=user      → HTTP 200，正常 SSE
//	role=developer → HTTP 500  {"code":500,"message":"服务器内部错误","data":null}（空体）
//
// 唯一变量就是 role。上游对 developer 不做优雅的校验拒绝，而是**内部崩掉**。
// 后果被放大成整条渠道不可用：三个健康账号各挨一发 500 → consecutive errors →
// 冷却 10 分钟 → 期间所有请求 503 no_healthy_account。
//
// 同一个病在 officeace 上表现为 400 ModelArts.81001（那边有 role 白名单校验）；
// 本仓另有 10 处渠道各自做过归一，lobsterai 当时漏了。
func TestBuildChatBodyNormalizesDeveloperRole(t *testing.T) {
	in := []byte(`{
		"model": "deepseek-flash",
		"messages": [
			{"role": "developer", "content": "You are a helpful assistant."},
			{"role": "user", "content": "hi"}
		]
	}`)
	out, err := buildChatBody(in)
	if err != nil {
		t.Fatalf("buildChatBody 失败：%v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(out, &body); err != nil {
		t.Fatalf("解析结果失败：%v\n%s", err, out)
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("messages 缺失：%s", out)
	}
	first, _ := msgs[0].(map[string]any)
	if role, _ := first["role"].(string); role != "system" {
		t.Errorf("❌ developer 未归一：期望 role=system，实得 role=%q\n"+
			"   这会让上游返回 500 服务器内部错误，并把健康账号打成 consecutive errors（冷却 10 分钟）", role)
	}
	// content 一个字都不能改（只改 role 一个字段）。
	if c, _ := first["content"].(string); c != "You are a helpful assistant." {
		t.Errorf("❌ content 被改动：%q", c)
	}
	// 其余 role 原样保留。
	if len(msgs) != 2 {
		t.Errorf("❌ 消息条数被改动：期望 2，实得 %d", len(msgs))
	}
	second, _ := msgs[1].(map[string]any)
	if role, _ := second["role"].(string); role != "user" {
		t.Errorf("❌ user 被误改：%q", role)
	}
	// 既有行为：stream 恒为 true（上游只支持 SSE，stream:false 直接 500）。
	if s, _ := body["stream"].(bool); !s {
		t.Error("❌ 既有行为回归：stream 未被强制为 true")
	}
}

// TestBuildChatBodyKeepsWhitelistedRoles 归一化不得波及上游正常接受的 role。
func TestBuildChatBodyKeepsWhitelistedRoles(t *testing.T) {
	in := []byte(`{
		"model": "deepseek-flash",
		"messages": [
			{"role": "system", "content": "s"},
			{"role": "user", "content": "u"},
			{"role": "assistant", "content": "a"},
			{"role": "tool", "tool_call_id": "x", "content": "t"}
		]
	}`)
	out, err := buildChatBody(in)
	if err != nil {
		t.Fatalf("buildChatBody 失败：%v", err)
	}
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
}
