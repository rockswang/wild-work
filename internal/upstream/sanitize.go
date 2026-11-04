// internal/upstream/sanitize.go 出站请求体指纹脱敏：清除 Claude Code / Codex CLI / DSH 注入的
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
// 本文件与 internal/sanitize/sanitize.go 是平行实现，必须同步改。
package upstream

import (
	"regexp"
	"strings"
)

// sanitizeFeatures 特征预检：任一命中才进入净化。全部小写，配合 asciiContainsFold
// 做大小写不敏感匹配（上游判据大小写不敏感，2026-10-28 真机实测小写变体同样 400，
// 大小写敏感的 Contains 会让小写变体连净化都进不去）。
var sanitizeFeatures = []string{
	"x-anthropic-billing-hdr",
	"x-anthropic-billing-header",                      // header 键值段键名
	"cc_entrypoint=",                                  // 尾随裸键值（截断前缀即可命中）
	"you are claude code",                             // 身份句（截断前缀即可命中）
	"main branch (",                                   // 注入指令句（截断前缀即可命中）
	"you are a coding agent running in the codex cli", // Codex instructions 首段（截断前缀即可命中）
	"github.com/anthropics/",                          // 反馈句里的 Anthropic 仓库链接

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

// sanitizeHdrRe 剥离层：header 键名即触发（与值无关），整段删除。
var sanitizeHdrRe = regexp.MustCompile(`(?i)` + hdrKeyRe + `\s*:[^;\n]*;?\s*`)

// sanitizeBareHdrRe 兜底层：裸键名（无冒号无值）同样是指纹——2026-09-13 实验 F4
// 证实 assistant 消息里反引号引用裸键名即触发 11128，而剥离层要求冒号、对裸串无效。
// 键值形态被整段删除后，残留的裸键名做最小缩写（header→hdr）：破坏逐字匹配、
// 语义不变、保留可读性。大小写不敏感，覆盖 X-Anthropic-... 变体。
//
// 注意该正则不要求冒号，是 sanitizeHdrRe 的超集——hasFingerprint 与 sanitizeText
// 中两者并用：先删键值形态（sanitizeHdrRe），再缩写残留裸键名（本正则），
// 替换语义不同（整段删除 vs 最小缩写），不可合并为一个正则。
var sanitizeBareHdrRe = regexp.MustCompile(`(?i)` + hdrKeyRe)

// sanitizeKvRe 剥离层：尾随裸键值（cc_xxx=...;）循环清理。
var sanitizeKvRe = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`)

// regexpRewriteStruct 正则改写对：re = 匹配正则，repl = 替换串。
type regexpRewriteStruct struct {
	re   *regexp.Regexp
	repl string
}

// sanitizeRegexpRewrites 正则匹配改写表
// 左 = *regexp.Regexp    右 = 替换串
var sanitizeRegexpRewrites = []regexpRewriteStruct{
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

// sanitizeStringRewrites 字符串匹配改写表
//
// 身份句的匹配串**不带结尾标点**（只到 "…for Claude" 为止）：
// CLI 版这句以句号收尾（"…for Claude."），桌面版（claude-desktop-3p / Agent SDK）
// 以逗号接后继内容（"…for Claude, running within the Claude Agent SDK."）。
// 带句号的整句只匹配前者，桌面版会漏网、指纹原样发上游 → 400 code=11128。
// 去掉结尾标点后两种形态一并覆盖（替换串同样不带标点，让原有标点原样保留）。
// 注意仍要求 "You are Claude Code, " 前缀，不做更宽的子串替换，
// 以免误伤 TestExactMatchOnlyVariantNotTouched 所保护的零散文本。
var sanitizeStringRewrites = [][2]string{
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

// sanitizeText 单段文本净化：预检不中 → 返回原串（零分配）。
func sanitizeText(text string) string {
	if !hasFingerprint(text) {
		return text
	}
	orig := text
	// 字符串匹配改写表
	for _, rw := range sanitizeStringRewrites {
		text = strings.ReplaceAll(text, rw[0], rw[1])
	}
	// 正则匹配改写表: (2026-11-02 改动: 改成遍历该表而非代码写死)
	for _, rw := range sanitizeRegexpRewrites {
		text = rw.re.ReplaceAllString(text, rw.repl)
	}
	if sanitizeHdrRe.MatchString(text) {
		text = sanitizeHdrRe.ReplaceAllString(text, "")
	}
	if strings.Contains(text, "cc_") {
		prev := ""
		for prev != text { // 清尾随裸 kv（cc_version=...; cc_entrypoint=...;）
			prev = text
			text = sanitizeKvRe.ReplaceAllString(text, "")
		}
	}
	// 2026-11-02 停用: (探针 sanitize_workbuddy_test.go 实测结果正常且未被拦截)
	// text = sanitizeBareHdrRe.ReplaceAllString(text, "x-anthropic-billing-hdr")

	// 预检可能因裸键名变体命中、却没有任何规则生效；此时原样返回，
	// 不能让末尾 TrimSpace 白白改掉首尾空白。
	if text == orig {
		return text
	}
	return strings.TrimSpace(text)
}

// hasFingerprint 特征预检：sanitizeFeatures 全小写 + ASCII 折叠匹配（零分配，
// 不能整串 ToLower——对每个请求体整串拷贝一次会破坏零分配预检）；
// header 键名有大小写变体（X-Anthropic-...）且可能以裸键名形态出现（无冒号），
// 最终以不要求冒号的 (?i) 正则兜底（sanitizeBareHdrRe，是 sanitizeHdrRe 的超集）。
func hasFingerprint(text string) bool {
	for _, f := range sanitizeFeatures {
		if asciiContainsFold(text, f) {
			return true
		}
	}
	return sanitizeBareHdrRe.MatchString(text)
}

// lowerASCII ASCII 大写折小写（仅特征表需要的折叠范围）。
func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// asciiContainsFold 大小写不敏感（ASCII 折叠）子串查找，零分配。
// 特征表全为 ASCII，ASCII 折叠已覆盖上游的大小写判据。
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

// sanitizeContent 兼容字符串与多模态数组；只动 text part，image 等 part 不动。
// 返回净化后的值及是否发生变化。
func sanitizeContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		s := sanitizeText(c)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text, ok := m["text"].(string)
			if !ok {
				continue
			}
			if s := sanitizeText(text); s != text {
				m["text"] = s
				changed = true
			}
		}
		return c, changed
	}
	return v, false
}

// sanitizeToolCalls 净化 assistant.tool_calls[].function.arguments。
//
// arguments 是**字符串化的 JSON**（不是对象），因此按文本走 sanitizeText 即可。
// 这块长期是盲区：工具调用消息的 content 通常是 null，而旧版 sanitizeMessages
// 在 content 缺失时直接 continue，整条消息连 tool_calls 一起被跳过——
// 于是历史里任何写进工具参数的被拦字符串（文件名、命令、写入内容）都会原样漏出。
func sanitizeToolCalls(v any) bool {
	callList, ok := v.([]any)
	if !ok {
		return false
	}
	changed := false
	for _, c := range callList {
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
		if s := sanitizeText(args); s != args {
			fn["arguments"] = s
			changed = true
		}
	}
	return changed
}

// sanitizeMessages 净化 messages 中的 content、reasoning_content 与 tool_calls；任一命中返回 true。
// 2026-11-02 改动: 跳过 role=user/tool/function (探针 sanitize_workbuddy_test.go 实测结果正常且未被拦截)
// role=system/assistant 实测仍拦截，继续保持清洗。无 role 等未知角色继续保持清洗。
func sanitizeMessages(messages []any) bool {
	changed := false
	for _, msg := range messages {
		m, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "user", "tool", "function":
			continue
		}
		// content 与 tool_calls 各自独立判断：content 可以为 null（工具调用轮），
		// 早期版本在此 continue，导致这类消息的 tool_calls 完全不被净化。
		if c, ok := m["content"]; ok {
			if nc, ch := sanitizeContent(c); ch {
				m["content"] = nc
				changed = true
			}
		}
		// reasoning_content（思维链回填字段，见 thinking.go/sse.go）实测同样
		// 携带指纹，与 content 同等净化。string 形态直接走 sanitizeText。
		if rc, ok := m["reasoning_content"].(string); ok {
			if s := sanitizeText(rc); s != rc {
				m["reasoning_content"] = s
				changed = true
			}
		}
		if tc, ok := m["tool_calls"]; ok {
			if sanitizeToolCalls(tc) {
				changed = true
			}
		}
	}
	return changed
}
