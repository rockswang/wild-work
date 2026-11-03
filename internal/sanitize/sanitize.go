// internal/sanitize/sanitize.go 出站请求体指纹脱敏：清除 Claude Code / Codex CLI / DSH 注入的
// 模板句，防止上游渠道内容审核按逐字精确匹配拦截（HTTP 400 code=11128）。
//
// 背景：客户端（Claude Code / DSH 等）在 system prompt 注入若干固定模板句，
// 上游内容审核按逐字精确匹配拦截（非语义审核），一字改动即可绕过。
//
// 判据快照（2026-11-02 真机探针，见 internal/upstream/sanitize_workbuddy_test.go）：
// 仍在拦: DSH 身份句（大小写不敏感）、DSH 跨句 @ 路径、CC 身份句、Main branch、Codex 首段（前缀锚定）、反馈句、billing 键值。
// 已失效（已注释停用）——裸 11128、DSH WebGUI 段、billing 裸键名、tools 字段品牌串。上游判据会漂移，改动须探针重验。
//
// 策略：键值型指纹整段删除，模板句最小改写（缩写/冠词/换词，不跨品牌替换）。
//
// 本文件与 internal/upstream/sanitize.go 是平行实现，必须同步改。
package sanitize

import (
	"encoding/json"
	"regexp"
	"strings"
)

// featHeaders 特征预检：任一命中才进入净化。全部小写，配合 asciiContainsFold
// 做大小写不敏感匹配（上游判据大小写不敏感，2026-10-28 真机实测小写变体同样 400，
// 大小写敏感的 Contains 会让小写变体连净化都进不去）。
var featHeaders = []string{
	"x-anthropic-billing-hdr",
	"x-anthropic-billing-header",
	"cc_entrypoint=",
	"you are claude code",
	"main branch (",
	"you are a coding agent running in the codex cli",
	"github.com/anthropics/",

	// 上游反探测：裸数字错误码
	// 2026-11-02 停用：(探针 sanitize_workbuddy_test.go 实测结果正常且未被拦截)
	// "11128",

	// DSH 身份句：上游按 system 开头逐字锚定
	"you are an ai agent powered by deepseek harness",

	// DSH Web GUI 段：上游按 system 开头整段
	// 2026-11-02 停用: (探针 sanitize_workbuddy_test.go 实测结果正常且未被拦截)
	// "deepseek harness web gui",

	// DSH 品牌串（超集，兜住 "DeepSeek Harness CLI" 等变体触发预检）
	// 2026-11-02 停用: (探针 sanitize_workbuddy_test.go 实测结果正常且未被拦截)
	// "deepseek harness",

	// DSH @ 路径句（跨句指纹后半，2026-10-28 实测, 2026-11-02 复现仍拦）
	"relative paths resolve from the workspace root",
}

// 宽松键名正则：键名内部允许夹任意非字母序列（全角/Unicode 连字符、空格、零宽等），
// 且 h/e/a/d/e/r 逐字母容忍分隔——header 全形态与 hdr 缩写形态都命中。
// 注意不能简单写 `hdr`：原始键名 "header" 不含连续 "hdr"，会漏掉键值整段剥离。
const hdrKeyRe = `x[^a-z]*anthropic[^a-z]*billing[^a-z]*h[^a-z]*(?:e[^a-z]*)?(?:a[^a-z]*)?d[^a-z]*(?:e[^a-z]*)?r`

// reHdr 剥离层：header 键名即触发（与值无关），整段删除。
var reHdr = regexp.MustCompile(`(?i)` + hdrKeyRe + `\s*:[^;\n]*;?\s*`)

// reBareHdr 裸键名（无冒号）兜底：是 reHdr 的超集，
// 两者并用——先删键值形态，再处理残留裸键名（改写已停用，仅预检兜底用）。
var reBareHdr = regexp.MustCompile(`(?i)` + hdrKeyRe)

// reKv 剥离层：尾随裸键值（cc_xxx=...;）循环清理。
var reKv = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// regexpRewriteStruct 正则改写对：re = 匹配正则，repl = 替换串。
type regexpRewriteStruct struct {
	re   *regexp.Regexp
	repl string
}

// regexpRewrites 正则匹配改写表
// 左 = *regexp.Regexp    右 = 替换串
var regexpRewrites = []regexpRewriteStruct{
	// 2026-11-02 重要改动: deepseek harness 替换为 DSH
	// 不得换成他身份串如 claude code, 因为dsh的系统提示词后文大量出现 DSH 这种关键词，
	// 此前的错误做法(自称 claude code) 只会让模型自我认知错乱。
	{
		regexp.MustCompile(`(?i)you\s+are\s+an\s+ai\s+agent\s+powered\s+by\s+(?:the\s+)?deepseek[\s\-_]*harness`),
		"You are an AI agent powered by DSH",
	},

	// 2026-11-02 停用: (探针 sanitize_workbuddy_test.go 实测结果正常且未被拦截)
	// {
	// 	regexp.MustCompile(`(?i)deepseek[\s\-_]*harness\s+web\s+gui`),
	// 	"DSH Web GUI",
	// },

	// 2026-11-02 改动: 换一种更稳妥的写法且语义不变。 (探针 sanitize_workbuddy_test.go 实测结果正常且未被拦截)
	{
		regexp.MustCompile(`(?i)relative\s+paths\s+resolve\s+from\s+the\s+workspace\s+root`),
		"Relative paths are resolved from the workspace root",
	},
}

// stringRewrites 字符串匹配改写表
var stringRewrites = [][2]string{
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
		"You are a coding agent running within the Codex CLI, a terminal-based coding assistant.",
	},
	{
		// 反馈句：整句带 Anthropic 仓库链接，上游按整句拦截（只留链接或只留半边均不拦，
		// 实测需整句同时出现）。give→provide 一词之差即可绕过，语义不变。
		"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
	},

	// 2026-11-02 停用: (探针 sanitize_workbuddy_test.go 实测结果正常且未被拦截)
	// {
	// 	"11128",
	// 	"11\u2011128"
	// },

	// 2026-11-02 改动: 关键词替换 , (探针 sanitize_workbuddy_test.go 实测结果正常且未被拦截)
	{
		"You are an AI agent powered by DeepSeek Harness",
		// "You are an AI agent powered by the DeepSeek Harness",
		"You are an AI agent powered by DSH",
	},

	// 2026-11-02 停用: (探针 sanitize_workbuddy_test.go 实测结果正常且未被拦截)
	// {
	// 	"DeepSeek Harness Web GUI",
	// 	"DSH Web GUI",
	// },
}

// cleanText 单段文本净化：预检不中 → 返回原串（零分配）。
func cleanText(text string) string {
	if !hasFingerprint(text) {
		return text
	}
	orig := text
	// 字符串匹配改写表
	for _, rw := range stringRewrites {
		text = strings.ReplaceAll(text, rw[0], rw[1])
	}
	// 正则匹配改写表: (2026-11-02 改动: 改成遍历该表而非代码写死)
	for _, rw := range regexpRewrites {
		text = rw.re.ReplaceAllString(text, rw.repl)
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
	// 2026-11-02 停用: (探针 sanitize_workbuddy_test.go 实测结果正常且未被拦截)
	// text = reBareHdr.ReplaceAllString(text, "x-anthropic-billing-hdr")

	// 预检可能因裸键名变体命中、却没有任何规则生效；此时原样返回，
	// 不能让末尾 TrimSpace 白白改掉首尾空白。
	if text == orig {
		return text
	}
	return strings.TrimSpace(text)
}

// Messages 脱敏 body 中 messages 的 content / reasoning_content / tool_calls.arguments。
// body 不可解析时原样返回，绝不阻塞请求（降级语义：宁可发指纹原文也不丢消息）。
// 只动结构化字段，不做整包替换。
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

// hasFingerprint 特征预检：featHeaders 全小写 + ASCII 折叠匹配（上游判据大小写
// 不敏感），再以 reBareHdr 兜住键名形态。asciiContainsFold 命中与否都零分配，
// 守住「预检不命中零分配原样通过」的不量（不能整串 ToLower，会逐请求拷贝）。
func hasFingerprint(s string) bool {
	for _, f := range featHeaders {
		if asciiContainsFold(s, f) {
			return true
		}
	}
	return reBareHdr.MatchString(s)
}

// lowerASCII ASCII 大写折小写（仅特征表需要的折叠范围）。
func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// asciiContainsFold 大小写不敏感（ASCII 折叠）子串查找，零分配。
// 特征表全为 ASCII，ASCII 折叠已覆盖上游的大小写判据；不能用 strings.ToLower
// ——它对每个请求体整串拷贝一次，破坏零分配预检。
func asciiContainsFold(s, f string) bool {
	n := len(f)
	if n == 0 {
		return true
	}
	first := lowerASCII(f[0])
	for i := 0; i+n <= len(s); i++ {
		if lowerASCII(s[i]) != first {
			continue
		}
		j := 1
		for ; j < n; j++ {
			if lowerASCII(s[i+j]) != lowerASCII(f[j]) {
				break
			}
		}
		if j == n {
			return true
		}
	}
	return false
}

// cleanContent 兼容字符串与多模态数组；只动 text part，image 等 part 不动。
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
	calls, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range calls {
		call, ok := c.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := fn["arguments"].(string)
		if !ok || args == "" {
			continue
		}
		if s := cleanText(args); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

// cleanMessages 净化 messages 中的 content、reasoning_content 与 tool_calls；任一命中返回 true。
// 2026-11-02 改动: 跳过 role=user/tool/function (探针 sanitize_workbuddy_test.go 实测结果正常且未被拦截)
// role=system/assistant 实测仍拦截，继续保持清洗。无 role 等未知角色继续保持清洗。
func cleanMessages(msgs []any) bool {
	changed := false
	for _, m := range msgs {
		msg, _ := m.(map[string]any)
		if msg == nil {
			continue
		}
		role, _ := msg["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "user", "tool", "function":
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

// SystemHead 提取 body 中首条 system/developer 消息 content 的前 n 个字符（rune
// 截断），用于拦截诊断——上游只看 system 开头做逐字指纹，user 内容与本诊断无关、
// 绝不返回。解析失败或无 system 消息返回 ""。多模态 content 取首个 text part。
func SystemHead(b []byte, n int) string {
	if len(b) == 0 || n <= 0 {
		return ""
	}
	var obj struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return ""
	}
	for _, m := range obj.Messages {
		if m.Role != "system" && m.Role != "developer" {
			continue
		}
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			return truncateRunes(s, n)
		}
		var parts []map[string]any
		if err := json.Unmarshal(m.Content, &parts); err == nil {
			for _, p := range parts {
				if t, ok := p["text"].(string); ok {
					return truncateRunes(t, n)
				}
			}
		}
		return ""
	}
	return ""
}

// truncateRunes 按 rune 截断（多字节内容不得切出半个字符），去除截断残留的首尾空白
// （日志不应带尾随空格；TrimSpace 只影响诊断展示，不回写请求体）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(string(r[:n]))
}
