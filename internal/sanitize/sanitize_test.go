package sanitize

// 生产路径脱敏表的回归护栏。
//
// 背景（R115 / PR #185）：脱敏表在本仓有**两份实现**——本包（`upstream.PrepareBody` 与
// `workbuddyai` 都调它）与 `internal/upstream/sanitize.go` 的包内平行实现。历史上两次
// 「改了上游没改这里」都靠人肉同步发现，本文件把两件事锁死：
//  1. 本包表的逐字节内容快照（与 upstream 侧快照同版本号，改一处必两处同时红）；
//  2. 与 `upstream.PrepareBody` 的**行为一致性**（同一段指纹文本，两路净化结果必须逐字相同）。
//
// 判据层级：身份句/整段逐字串是 2026-10-21 单发 A/B 实测；「上游按连续逐字前缀匹配」
// 是实测结论，不是关键词或语义审核——所以改写只要能破坏逐字串即可，措辞尽量原样保留。

import (
	"encoding/json"
	"strings"
	"testing"
)

const (
	// DSH 身份句：上游对 system 消息**开头**逐字前缀匹配即回 400 code=11-128。
	dshIdentity = "You are an AI agent powered by DeepSeek Harness"
	// DSH Web GUI 段：与后半句连成 221 字符整段逐字串才拦，单句放行。
	dshWebGUI = "DeepSeek Harness Web GUI"
)

// 本包 featHeaders 的逐字节快照（9 项活跃，全小写——上游判据大小写不敏感，
// 配合 asciiContainsFold 折叠匹配）。2026-11-02 探针停用项（11128 / webgui /
// 品牌串超集）不在快照内，证据见实现处注释；恢复时须同步本快照 + upstream 侧。
var wantFeatures = []string{
	"x-anthropic-billing-hdr",
	"x-anthropic-billing-header",                      // header 键值段键名
	"cc_entrypoint=",                                  // 尾随裸键值（截断前缀即可命中）
	"you are claude code",                             // 身份句（截断前缀即可命中）
	"main branch (",                                   // 注入指令句（截断前缀即可命中）
	"you are a coding agent running in the codex cli", // Codex instructions 首段（截断前缀即可命中）
	"github.com/anthropics/",                          // 反馈句里的 Anthropic 仓库链接
	"you are an ai agent powered by deepseek harness", // DSH 身份句：上游按 system 开头逐字锚定（2026-10-21 实测，2026-11-02 复现仍拦）
	"relative paths resolve from the workspace root",  // DSH @ 路径句（跨句指纹后半，2026-10-28 实测，2026-11-02 复现仍拦）
}

// 本包 stringRewrites 的逐字节快照（5 对活跃；11128 / WebGUI 两对 2026-11-02 探针
// 实测停用，见实现处注释）。
var wantRewrites = [][2]string{
	{"You are Claude Code, Anthropic's official CLI for Claude",
		"You are Claude Code, Anthropic's official CLI tool for Claude"},
	{"Main branch (you will usually use this for PRs)",
		"Default branch (you will usually use this for PRs)"},
	// Codex 首段：in→within 前缀中段打断（2026-11-02 实测 200；旧「CLI tool」
	// 句尾插词产物实测仍被拦）。
	{"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
		"You are a coding agent running within the Codex CLI, a terminal-based coding assistant."},
	{"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"},
	{dshIdentity, "You are an AI agent powered by DSH"},
}

// 本包 regexpRewrites 的逐字节快照（2 对活跃；WebGUI 对 2026-11-02 探针实测停用）。
var wantRegexpRewrites = []struct{ pattern, repl string }{
	{`(?i)you\s+are\s+an\s+ai\s+agent\s+powered\s+by\s+(?:the\s+)?deepseek[\s\-_]*harness`,
		"You are an AI agent powered by DSH"},
	{`(?i)relative\s+paths\s+resolve\s+from\s+the\s+workspace\s+root`,
		"Relative paths are resolved from the workspace root"},
}

func TestTablesByteExactSnapshot(t *testing.T) {
	if len(featHeaders) != len(wantFeatures) {
		t.Fatalf("featHeaders 项数=%d want %d（改表必须同步本快照 + upstream 侧快照）",
			len(featHeaders), len(wantFeatures))
	}
	for i, want := range wantFeatures {
		if featHeaders[i] != want {
			t.Errorf("featHeaders[%d]=%q want %q", i, featHeaders[i], want)
		}
	}
	if len(stringRewrites) != len(wantRewrites) {
		t.Fatalf("stringRewrites 对数=%d want %d", len(stringRewrites), len(wantRewrites))
	}
	for i, want := range wantRewrites {
		if stringRewrites[i][0] != want[0] || stringRewrites[i][1] != want[1] {
			t.Errorf("stringRewrites[%d]=%q→%q want %q→%q", i,
				stringRewrites[i][0], stringRewrites[i][1], want[0], want[1])
		}
	}
	if len(regexpRewrites) != len(wantRegexpRewrites) {
		t.Fatalf("regexpRewrites 对数=%d want %d", len(regexpRewrites), len(wantRegexpRewrites))
	}
	for i, want := range wantRegexpRewrites {
		if got := regexpRewrites[i].re.String(); got != want.pattern {
			t.Errorf("regexpRewrites[%d].re=%q want %q", i, got, want.pattern)
		}
		if got := regexpRewrites[i].repl; got != want.repl {
			t.Errorf("regexpRewrites[%d].repl=%q want %q", i, got, want.repl)
		}
	}
}

// 整段逐字串（身份句 + Web GUI 段 + 后缀）：身份句是活指纹必须改写；
// WebGUI 段 2026-11-02 探针实测放行（规则停用）——原样保留不得再改。
func TestDSHFullPromptSanitizedViaMessages(t *testing.T) {
	in := dshIdentity + ". " + dshWebGUI + ", not the CLI or desktop app. If the user says gui, they mean this GUI."
	out := string(Messages(bodyWithSystem(t, in)))
	if strings.Contains(out, dshIdentity) {
		t.Fatalf("身份句指纹残留: %q", out)
	}
	if !strings.Contains(out, "You are an AI agent powered by DSH") {
		t.Errorf("身份句改写缺失（应整句替换为 DSH 缩写形态）: %q", out)
	}
	// WebGUI 段：判据已失效，必须原样保留（防停用规则被悄悄复活）。
	if !strings.Contains(out, dshWebGUI) {
		t.Errorf("WebGUI 段被误改（2026-11-02 探针实测放行，规则应停用）: %q", out)
	}
	if !strings.Contains(out, "they mean this GUI.") {
		t.Errorf("非指纹措辞被误删: %q", out)
	}
}

// 反向护栏：措辞相近但不是逐字形态的文本一律不得改动（防误伤用户内容）。
func TestDSHVariantsUntouched(t *testing.T) {
	for _, in := range []string{
		"You are an AI agent, powered by DeepSeek Harness.", // 多一个逗号
		"You are an AI agent powered by DeepSeek.",          // 截去 Harness
		"An AI agent powered by DeepSeek Harness.",          // 缺 "You are an"
		"DeepSeek Harness CLI",                              // 含品牌但不是 Web GUI 段
		"You use the web GUI to do work.",                   // 含 web gui 但不是 DSH 短语
	} {
		if got := systemOf(t, Messages(bodyWithSystem(t, in))); got != in {
			t.Errorf("变体被误改: %q -> %q", in, got)
		}
	}
}

// 无指纹请求必须零改动（同一切片返回，不重新序列化）。
func TestNoFingerprintSameBytes(t *testing.T) {
	body := bodyWithSystem(t, "帮我看下这段日志。")
	if got := Messages(body); string(got) != string(body) {
		t.Errorf("无指纹请求被改写: %s", got)
	}
}

// 第三条跨句 @ 路径指纹（2026-10-28 真机实测：t_join 跨句连写回 400、单句各自回 200；
// 2026-11-03 复测：改谓语 are resolved 回 200）：后半句必须被改写，前半句单独出现不是指纹、不得改动。
func TestACPCrossSentenceFingerprintSanitized(t *testing.T) {
	in := "Tokens prefixed with @ are paths the user explicitly referenced. " +
		"Relative paths resolve from the workspace root; anything else is a file: use the read tool."
	out := string(Messages(bodyWithSystem(t, in)))
	if strings.Contains(out, "Relative paths resolve from the workspace root") {
		t.Fatalf("@ 路径句跨句指纹未被改写: %q", out)
	}
	if !strings.Contains(out, "Relative paths are resolved from the workspace root") {
		t.Errorf("@ 路径句改写缺失: %q", out)
	}
	// 前半句单独出现：不是指纹（上游按跨句连写锚定），零改动。
	if got := systemOf(t, Messages(bodyWithSystem(t, "Tokens prefixed with @ are paths the user explicitly referenced."))); got != "Tokens prefixed with @ are paths the user explicitly referenced." {
		t.Errorf("前半句被误改: %q", got)
	}
}

// 上游大小写不敏感（2026-10-28 真机实测：全小写身份句回 400）：预检与改写必须认出全小写变体。
func TestLowercaseIdentitySanitized(t *testing.T) {
	in := "you are an ai agent powered by deepseek harness. 你是助手。"
	out := systemOf(t, Messages(bodyWithSystem(t, in)))
	if strings.Contains(strings.ToLower(out), "deepseek harness") {
		t.Fatalf("小写变体未被改写（上游大小写不敏感，仍会 400）: %q", out)
	}
	if !strings.Contains(out, "You are an AI agent powered by DSH") {
		t.Errorf("小写身份句未整句替换: %q", out)
	}
}

// 2026-11-02 探针推翻的判据必须保持停用（防悄悄复活篡改用户内容）：
// ① 裸 11128 不再改写（两上下文实测 200）；② 裸键名不再缩写（system 与
// assistant 双角色实测 200）。两者即使在净化中（同文本含活指纹）也不得被动。
func TestRefutedRulesStayInactive(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude. " +
		"The upstream returned code=11128; see `x-anthropic-billing-header` for the billing key."
	out := systemOf(t, Messages(bodyWithSystem(t, in)))
	if !strings.Contains(out, "code=11128") {
		t.Errorf("裸 11128 被误改写（判据 2026-11-02 实测已失效）: %q", out)
	}
	if !strings.Contains(out, "`x-anthropic-billing-header`") {
		t.Errorf("裸键名被误缩写（判据 2026-11-02 实测已失效）: %q", out)
	}
	// 同文本里的活指纹仍必须正常改写（停用其它规则不得误伤活规则）。
	if strings.Contains(out, "official CLI for Claude.") {
		t.Errorf("CC 身份句活指纹未被改写: %q", out)
	}
	if !strings.Contains(out, "official CLI tool for Claude.") {
		t.Errorf("CC 身份句改写缺失: %q", out)
	}
}

// 2026-11-03 角色探针：上游不扫 user（7 条活指纹放 user 全部 200），
// 用户内容必须原样保留；同文本放 system 仍须净化。
func TestUserMessageUntouched(t *testing.T) {
	const fp = "You are Claude Code, Anthropic's official CLI for Claude. Main branch (you will usually use this for PRs)"
	body, err := json.Marshal(map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "user", "content": fp},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if out := Messages(body); string(out) != string(body) {
		t.Errorf("user 内容被清洗（上游不扫 user）: %s -> %s", body, out)
	}
}

// tool / function 同样不扫（2026-11-03 各 7 条活指纹全 200），内容原样保留。
func TestUserToolFunctionMessagesUntouched(t *testing.T) {
	const fp = "You are Claude Code, Anthropic's official CLI for Claude. Main branch (you will usually use this for PRs)"
	body, err := json.Marshal(map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "user", "content": fp},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": fp},
		map[string]any{"role": "function", "name": "probe_fn", "content": fp},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if out := Messages(body); string(out) != string(body) {
		t.Errorf("user/tool/function 内容被清洗（上游不扫这三类角色）: %s -> %s", body, out)
	}
}

func TestUserMessageUntouchedWhileSystemSanitized(t *testing.T) {
	const fp = "You are an AI agent powered by DeepSeek Harness"
	body, err := json.Marshal(map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "system", "content": fp},
		map[string]any{"role": "user", "content": fp},
	}})
	if err != nil {
		t.Fatal(err)
	}
	out := Messages(body)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages 长度=%d", len(msgs))
	}
	sys, _ := msgs[0].(map[string]any)["content"].(string)
	if !strings.Contains(sys, "You are an AI agent powered by DSH") {
		t.Errorf("system 活指纹未被净化: %q", sys)
	}
	if user, _ := msgs[1].(map[string]any)["content"].(string); user != fp {
		t.Errorf("user 内容被清洗: %q", user)
	}
}

// 预检可能因裸键名变体命中，但没有任何规则生效；此时不得 TrimSpace 改首尾空白。
func TestBareHeaderPrecheckKeepsWhitespace(t *testing.T) {
	in := "  x-anthropic-billing-h-e-a-d-e-r  "
	if out := cleanText(in); out != in {
		t.Errorf("无规则生效时被 TrimSpace 改动: %q -> %q", in, out)
	}
	// 有实际改写时仍应 TrimSpace。
	if out := cleanText("  x-anthropic-billing-header: cc_version=1.0;  "); out != "" {
		t.Errorf("实际改写后未 TrimSpace: %q", out)
	}
}

// SystemHead：取首条 system/developer 的 content 开头（供 diag 日志），rune 截断。
func TestSystemHead(t *testing.T) {
	b, err := json.Marshal(map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "u"},
		map[string]any{"role": "system", "content": "You are an AI agent powered by DeepSeek Harness. 你是助手。"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if head := SystemHead(b, 20); head != "You are an AI agent" {
		t.Errorf("SystemHead=%q want %q", head, "You are an AI agent")
	}
	// rune 截断：中文按 rune 计，不切坏 UTF-8。
	b2, err := json.Marshal(map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "你好，世界。这是系统提示。"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := SystemHead(b2, 2); got != "你好" {
		t.Errorf("SystemHead rune 截断=%q want %q", got, "你好")
	}
}

func bodyWithSystem(t *testing.T, sys string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "system", "content": sys},
		map[string]any{"role": "user", "content": "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func systemOf(t *testing.T, body []byte) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	ms, _ := obj["messages"].([]any)
	if len(ms) == 0 {
		t.Fatalf("无 messages: %s", body)
	}
	s, _ := ms[0].(map[string]any)["content"].(string)
	return s
}
