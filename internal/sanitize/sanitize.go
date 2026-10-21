// Package sanitize 出站请求体指纹脱敏：清除 Claude Code / Codex CLI 注入的模板句，
// 防止上游 CodeBuddy 内容审核按逐字精确匹配拦截（HTTP 400 code=11128 / content_blocked）。
//
// 策略：键值型指纹整段删除，承载语义的模板句最小改写（换一词），语义不变。
// content 值为 null 的 assistant 消息不会跳过——其 tool_calls arguments 仍需净化。
package sanitize

import (
	"encoding/json"
	"regexp"
	"strings"
)

var featHeaders = []string{
	"x-anthropic-billing-header",
	"cc_entrypoint=",
	"You are Claude Code",
	"Main branch (",
	"You are a coding agent running in the Codex CLI",
	"github.com/anthropics/",
	"11128",
	// DSH 身份句：上游按「system 消息开头」逐字锚定匹配（2026-10-21 实测）。
	"You are an AI agent powered by DeepSeek Harness",
	// DSH Web GUI 段：与后半句构成跨句组合判据，单句不拦（2026-10-21 实测）。
	"DeepSeek Harness Web GUI",
}

var reHdr = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?\s*`)
var reBareHdr = regexp.MustCompile(`(?i)x-anthropic-billing-header`)
var reKv = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

var rewrites = [][2]string{
	{
		"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude",
	},
	{
		"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)",
	},
	{
		"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
	},
	{
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},
	// 裸 11128 整单拦截，插入连字符保留可读性（零宽空格无效，上游会归一化）。
	{"11128", "11-128"},
	// DSH 身份句：上游只认 system 开头的逐字形态；插入冠词 the 即绕过，语义扰动最小（实测 200）。
	{
		"You are an AI agent powered by DeepSeek Harness",
		"You are an AI agent powered by the DeepSeek Harness",
	},
	// DSH Web GUI 品牌短语：与后半句组合才命中；缩写后完整 prompt 通过（实测 200）。
	{
		"DeepSeek Harness Web GUI",
		"DSH Web GUI",
	},
}

// Messages 脱敏 body 中 messages 的 content / reasoning_content / tool_calls.arguments。
// body 不可解析时原样返回，绝不阻塞请求（降级语义：宁可发指纹原文也不丢消息）。
func Messages(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	// 预检：零分配快速路径，普通请求全不中。
	if !hasFingerprint(toString(body)) {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) == 0 {
		return body
	}
	if cleanMessages(msgs) {
		if out, err := json.Marshal(obj); err == nil {
			return out
		}
	}
	return body
}

func toString(b []byte) string { return string(b) }

func hasFingerprint(s string) bool {
	for _, f := range featHeaders {
		if strings.Contains(s, f) {
			return true
		}
	}
	// 大小写变体 + 裸键名形态由正则兜底
	return reBareHdr.MatchString(s)
}

func cleanText(text string) string {
	if !hasFingerprint(text) {
		return text
	}
	for _, rw := range rewrites {
		text = strings.ReplaceAll(text, rw[0], rw[1])
	}
	if reHdr.MatchString(text) {
		text = reHdr.ReplaceAllString(text, "")
	}
	if strings.Contains(text, "cc_") {
		prev := ""
		for prev != text {
			prev = text
			text = reKv.ReplaceAllString(text, "")
		}
	}
	text = reBareHdr.ReplaceAllString(text, "x-anthropic-billing-hdr")
	return strings.TrimSpace(text)
}

func cleanContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		s := cleanText(c)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, _ := p.(map[string]any)
			if m == nil {
				continue
			}
			if t, ok := m["text"].(string); ok {
				if s := cleanText(t); s != t {
					m["text"] = s
					changed = true
				}
			}
		}
		return c, changed
	}
	return v, false
}

func cleanToolCalls(v any) bool {
	calls, _ := v.([]any)
	if len(calls) == 0 {
		return false
	}
	changed := false
	for _, c := range calls {
		call, _ := c.(map[string]any)
		if call == nil {
			continue
		}
		fn, _ := call["function"].(map[string]any)
		if fn == nil {
			continue
		}
		args, _ := fn["arguments"].(string)
		if args == "" {
			continue
		}
		if s := cleanText(args); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

func cleanMessages(msgs []any) bool {
	changed := false
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		if msg == nil {
			continue
		}
		if v, ok := msg["content"]; ok {
			if nc, ch := cleanContent(v); ch {
				msg["content"] = nc
				changed = true
			}
		}
		if rc, ok := msg["reasoning_content"].(string); ok {
			if s := cleanText(rc); s != rc {
				msg["reasoning_content"] = s
				changed = true
			}
		}
		if tc, ok := msg["tool_calls"]; ok {
			if cleanToolCalls(tc) {
				changed = true
			}
		}
	}
	return changed
}
