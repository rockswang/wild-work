package upstream_test

// 两份脱敏表的行为一致性守卫（R115「两份表必须同步」的机器版）。
//
// 现状：脱敏表有两份实现——`internal/sanitize`（生产路径，workbuddyai 与 upstream 都调）与
// `internal/upstream/sanitize.go`（包内平行实现，由 `prepareBodyInner` 调用）。R115 的教训是
// 「只改一份等于没改」，但当时只靠两边的逐字节快照测试 + 人肉 diff 把关。
//
// 本测试验证端到端：`PrepareBody`（两表串联）与 `sanitize.Messages`（只过生产表）一致。
// 注意它抓不住「只改生产表」——两条臂最终都会过生产表；表级一致性由
// `sanitize_test.go` 的 `TestSanitizeTablesParityDirect`（直接对比 sanitizeMessages
// 与 sanitize.Messages）守护。

import (
	"encoding/json"
	"strings"
	"testing"

	"wild-work/internal/sanitize"
	"wild-work/internal/upstream"
)

// 覆盖两表全部特征形态；每条都必须经两条路径得到同一结果。
var parityPayloads = []string{
	"x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli;",
	"bare x-anthropic-billing-header without value",
	"You are Claude Code, Anthropic's official CLI for Claude.",
	"Main branch (you will usually use this for PRs)",
	"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
	"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	"upstream returned code 11128",
	"You are an AI agent powered by DeepSeek Harness",
	"You are an AI agent powered by DeepSeek Harness. DeepSeek Harness Web GUI, not the CLI. they mean this GUI.",
	"普通中文对话，不含任何指纹。",
}

// TestSanitizeEmptyToolCallArgumentsParity 空串 arguments 的处理两表必须一致
// （历史分叉：一边 `args == ""` 跳过、一边只判类型断言——空串是合法 string，
// 行为对齐为跳过）。注意两点：① PrepareBody 会强制 stream 并重序列化，整包
// 字节比较永不相等，须取 arguments 字段值比对；② body 必须带配对的 role:tool
// 结果消息——否则 PrepareBody 的 cleanupOrphanToolCalls（独立安全网，非脱敏职责）
// 会按孤儿调用删掉 tool_calls，干扰比对。
func TestSanitizeEmptyToolCallArgumentsParity(t *testing.T) {
	mkBody := func() []byte {
		b, err := json.Marshal(map[string]any{"model": "m", "messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{
					"name": "f", "arguments": "",
				}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "ok"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	argsOf := func(body []byte) string {
		t.Helper()
		var obj struct {
			Messages []struct {
				ToolCalls []struct {
					Function struct {
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, body)
		}
		for _, m := range obj.Messages {
			for _, c := range m.ToolCalls {
				return c.Function.Arguments
			}
		}
		t.Fatalf("无 tool_calls: %s", body)
		return ""
	}
	viaProd := argsOf(sanitize.Messages(mkBody()))
	viaBoth := argsOf(upstream.PrepareBody(mkBody()))
	if viaProd != viaBoth {
		t.Errorf("两表对空串 arguments 的处理不一致: prod=%q both=%q", viaProd, viaBoth)
	}
	if viaProd != "" {
		t.Errorf("空串 arguments 被改写: %q", viaProd)
	}
}

func TestSanitizeTablesParity(t *testing.T) {
	for _, in := range parityPayloads {
		body := mustBody(t, in)
		viaProd := systemContent(t, sanitize.Messages(append([]byte(nil), body...)))
		viaBoth := systemContent(t, upstream.PrepareBody(append([]byte(nil), body...)))
		if viaProd != viaBoth {
			t.Errorf("两表净化结果不一致（漏改一份等于没修，见 R115）\n in=%q\n sanitize=%q\n upstream=%q",
				in, viaProd, viaBoth)
		}
		// 指纹类输入必须真的被改写，否则说明两表同时失效（而不是「一致」）。
		// 2026-11-02 判据快照：DSH 身份句是活指纹必须被改写；WebGUI 段实测放行
		// 可原样保留（规则已停用），不再作为「必须消失」的断言对象。
		if strings.Contains(in, "You are an AI agent powered by DeepSeek Harness") &&
			strings.Contains(viaProd, "You are an AI agent powered by DeepSeek Harness") {
			t.Errorf("改写后仍留身份句指纹: %q", viaProd)
		}
	}
}

func mustBody(t *testing.T, sys string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "system", "content": sys},
		map[string]any{"role": "user", "content": "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func systemContent(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	ms, _ := obj["messages"].([]any)
	if len(ms) == 0 {
		t.Fatalf("messages 为空: %s", body)
	}
	s, _ := ms[0].(map[string]any)["content"].(string)
	return s
}
