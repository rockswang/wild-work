package upstream

// 出站脱敏回归测试。纯脱敏单元用例 + 逐字节快照护栏从参考项目 workbuddy2api 平移；
// 依赖参考项目私有 API（PrepareBodyOpt / Client.SanitizeFingerprints / 带 ChatMeta 的
// ChatStream 签名）的用例改用本项目对应入口（PrepareBody / testClient.ChatStream），
// 且本项目固定开启脱敏、无开关，故不再有 "disable 保留指纹" 的用例。

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/sanitize"
)

func TestCustomRewriteKeepsToolArgumentsJSON(t *testing.T) {
	origFeatures, origRewrites := sanitizeFeatures, sanitizeStringRewrites
	t.Cleanup(func() { sanitizeFeatures, sanitizeStringRewrites = origFeatures, origRewrites })
	sanitizeFeatures = append(append([]string(nil), origFeatures...), "secret")
	sanitizeStringRewrites = append(append([][2]string(nil), origRewrites...), [2]string{"secret", `"`})
	obj := map[string]any{"messages": []any{map[string]any{"role": "assistant", "tool_calls": []any{
		map[string]any{"function": map[string]any{"arguments": `{"q":"secret"}`}},
	}}}}
	sanitizeMessagesInPlace(obj)
	args := obj["messages"].([]any)[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	if !json.Valid([]byte(args)) || args != `{"q":"secret"}` {
		t.Fatalf("非法自定义替换必须跳过工具参数: %q", args)
	}
}

const (
	ccIdentity = "You are Claude Code, Anthropic's official CLI for Claude."
	ccBranch   = "Main branch (you will usually use this for PRs)"
	ccHeader   = "x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli;"
	// Codex instructions 首段（上游逐字精确指纹，三句缺一不可）。
	codexInstructions = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI. You are expected to be precise, safe, and helpful."
	// DSH 身份句：上游对 system 消息**开头**逐字前缀匹配这一串即回 400 code=11-128
	// （放 user role、非开头、或写成 "powered by DeepSeek." 都放行，2026-10-21 单发 A/B 实测）。
	dshIdentity = "You are an AI agent powered by DeepSeek Harness"
	// DSH Web GUI 段：2026-10-21 单发实测「与后半句连成 221 字符整段」才拦；
	// 2026-11-07 线上被拦体重放推翻该结论——品牌串原样即可能 400，生产已统一缩写为 DSH。
	dshWebGUI = "DeepSeek Harness Web GUI"
)

// DSH 身份句必须被改写：整句替换为 DSH 缩写形态（品牌全称从 system 彻底消失）。
// 不得替换为 Claude Code 等他牌身份串——后文仍有 DSH Web GUI / deepseek 模型名，
// 首句自称他牌会让模型自我认知错乱。
func TestDSHIdentityRewritten(t *testing.T) {
	out := sanitizeText(dshIdentity + " You own the repository.")
	if strings.Contains(out, dshIdentity) {
		t.Errorf("DSH identity still present: %q", out)
	}
	if !strings.Contains(out, "You are an AI agent powered by DSH") {
		t.Errorf("DSH rewrite missing: %q", out)
	}
	if strings.Contains(strings.ToLower(out), "deepseek harness") {
		t.Errorf("brand string remains after identity rewrite: %q", out)
	}
	if !strings.Contains(out, " You own the repository.") {
		t.Errorf("suffix lost: %q", out)
	}
}

// DSH 身份句必须经 PrepareBody 落进 system content（真实调用链）。
func TestPrepareBodySanitizesDSHSystem(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + dshIdentity + `"},` +
		`{"role":"user","content":"hi"}]}`)
	var obj map[string]any
	if err := json.Unmarshal(PrepareBody(body), &obj); err != nil {
		t.Fatal(err)
	}
	sys, _ := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, dshIdentity) {
		t.Errorf("DSH fingerprint remains: %q", sys)
	}
	if !strings.Contains(sys, "You are an AI agent powered by DSH") {
		t.Errorf("DSH rewrite missing: %q", sys)
	}
}

// Web GUI 段 2026-11-07 复测改判：真实 DSH system 里该段原样即 400（线上被拦体重放），
// 品牌串须缩写为 DSH。2026-11-02「放行」结论已被推翻。
func TestDSHWebGuiRewritten(t *testing.T) {
	in := dshWebGUI + ", not the CLI or desktop app. If the user says GUI, they mean this GUI."
	want := "DSH Web GUI, not the CLI or desktop app. If the user says GUI, they mean this GUI."
	if out := sanitizeText(in); out != want {
		t.Errorf("WebGUI 段应缩写为 DSH: %q -> %q (want %q)", in, out, want)
	}
}

// 身份句与 WebGUI 段都必须缩写为 DSH（品牌全称从 system 彻底消失）。
func TestDSHFullPromptBroken(t *testing.T) {
	in := dshIdentity + ". " + dshWebGUI + ", not the CLI or desktop app. " +
		"If the user says gui, they mean this GUI."
	out := sanitizeText(in)
	if strings.Contains(out, dshIdentity) {
		t.Errorf("身份句指纹仍在，上游仍会回 11-128: %q", out)
	}
	if !strings.Contains(out, "You are an AI agent powered by DSH") {
		t.Errorf("身份句改写缺失: %q", out)
	}
	if strings.Contains(strings.ToLower(out), "deepseek harness") {
		t.Errorf("品牌串残留: %q", out)
	}
	if !strings.Contains(out, "they mean this GUI.") {
		t.Errorf("后半句被误删: %q", out)
	}
}

// 反向护栏：不含 DSH 品牌串的文本不得被该规则介入。
func TestDSHWebGuiRuleDoesNotLeak(t *testing.T) {
	for _, in := range []string{
		"You use the web GUI to do work.", // 含 web gui 但不是 DSH 品牌短语
		"DeepSeek CLI",                    // 含 DeepSeek 但不含 Harness 品牌串
	} {
		if out := sanitizeText(in); out != in {
			t.Errorf("非指纹文本被改动: %q -> %q", in, out)
		}
	}
}

// 反向护栏：措辞相近但无品牌串（"DeepSeek Harness"）的文本不得被改写（防误伤）。
func TestDSHVariantNotTouched(t *testing.T) {
	for _, in := range []string{
		"You are an AI agent powered by DeepSeek.",
		"You are an AI agent powered by DSH.",
	} {
		if out := sanitizeText(in); out != in {
			t.Errorf("variant wrongly rewritten: %q -> %q", in, out)
		}
	}
}

func TestIdentityRewritten(t *testing.T) {
	out := sanitizeText(ccIdentity)
	if !strings.Contains(out, "official CLI tool for Claude.") {
		t.Errorf("identity not rewritten: %q", out)
	}
	if strings.Contains(out, ccIdentity) {
		t.Errorf("original identity still present: %q", out)
	}
}

// 桌面版（claude-desktop-3p / Agent SDK）的身份句以逗号接后继内容，结尾不是句号。
// 回归用例：匹配串曾带结尾句号，导致该形态漏网、指纹原样发上游 → 400 code=11128。
func TestIdentityDesktopVariantRewritten(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK."
	out := sanitizeText(in)
	if strings.Contains(out, "official CLI for Claude") {
		t.Errorf("desktop identity not rewritten: %q", out)
	}
	if !strings.Contains(out, "official CLI tool for Claude, running within the Claude Agent SDK.") {
		t.Errorf("desktop identity suffix not preserved: %q", out)
	}
}

func TestBranchRewritten(t *testing.T) {
	out := sanitizeText(ccBranch)
	if !strings.Contains(out, "Default branch (you will usually use this for PRs)") {
		t.Errorf("branch not rewritten: %q", out)
	}
	if strings.Contains(out, "Main branch") {
		t.Errorf("original branch still present: %q", out)
	}
}

// 反馈句带 Anthropic 仓库链接，上游按整句拦截（实测只留链接或只留半边均不拦）。
// 回归用例：give→provide 一词之差即可绕过。
func TestFeedbackSentenceRewritten(t *testing.T) {
	in := "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"
	out := sanitizeText(in)
	if strings.Contains(out, "To give feedback") {
		t.Errorf("feedback sentence not rewritten: %q", out)
	}
	if !strings.Contains(out, "To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues") {
		t.Errorf("feedback sentence not rewritten as expected: %q", out)
	}
}

// 裸 11128 2026-11-02 探针实测放行（两个不同上下文均 200，原「任何上下文整单
// 拦截」判据已失效）：必须原样保留，防停用规则被悄悄复活篡改用户日志/错误码回显。
func TestUpstreamErrorCodeUntouchedAfterProbe(t *testing.T) {
	in := "upstream returned code=11128 for this request"
	if out := sanitizeText(in); out != in {
		t.Errorf("裸 11128 被误改写（判据 2026-11-02 实测已失效）: %q -> %q", in, out)
	}
}

// 回归：工具调用消息的 content 常为 null，而旧版 sanitizeMessages 在 content 缺失时
// 直接 continue，整条消息连 tool_calls 一起被跳过 → arguments 里的被拦字符串原样漏出。
func TestToolCallArgumentsSanitized(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "run"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{
				"name":      "Bash",
				"arguments": `{"command":"git switch \"Main branch (you will usually use this for PRs)\""}`,
			}},
		}},
	}
	if !sanitizeMessages(msgs) {
		t.Fatal("sanitizeMessages 未报告任何改动，tool_calls 被跳过")
	}
	fn := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	got := fn["arguments"].(string)
	if strings.Contains(got, "Main branch (") {
		t.Errorf("tool_call arguments 未被净化: %q", got)
	}
	if !strings.Contains(got, "Default branch (") {
		t.Errorf("tool_call arguments 改写缺失: %q", got)
	}
}

// 2026-11-03 角色探针：上游不扫 user/tool/function（各 7 条活指纹全 200），
// 这三类消息的内容不得清洗；assistant 仍须清洗（同批实测身份句放 assistant 仍 400）。
func TestUserMessagesNotSanitized(t *testing.T) {
	const fp = "You are Claude Code, Anthropic's official CLI for Claude. Main branch (you will usually use this for PRs)"
	msgs := []any{
		map[string]any{"role": "user", "content": fp},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": fp},
		map[string]any{"role": "function", "name": "probe_fn", "content": fp},
	}
	if sanitizeMessages(msgs) {
		t.Fatal("user/tool/function 消息被清洗（上游不扫这三类角色，2026-11-03 实测）")
	}
	for _, m := range msgs {
		obj := m.(map[string]any)
		if got, _ := obj["content"].(string); got != fp {
			t.Errorf("%s 内容被改写: %q", obj["role"], got)
		}
	}
}

func TestPrepareBodySanitizesAssistantKeepsUser(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"You are a helpful assistant."},` +
		`{"role":"user","content":"` + ccIdentity + `"},` +
		`{"role":"assistant","content":"` + ccIdentity + `"}]}`)
	out := PrepareBody(body)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	msgs := obj["messages"].([]any)
	if got, _ := msgs[1].(map[string]any)["content"].(string); got != ccIdentity {
		t.Errorf("user 内容被清洗: %q", got)
	}
	asst, _ := msgs[2].(map[string]any)["content"].(string)
	if strings.Contains(asst, "official CLI for Claude") || !strings.Contains(asst, "official CLI tool for Claude") {
		t.Errorf("assistant 活指纹未被净化: %q", asst)
	}
}

// 预检可能因裸键名变体命中，但没有任何规则生效；此时不得 TrimSpace 改首尾空白。
func TestBareHeaderPrecheckKeepsWhitespace(t *testing.T) {
	in := "  x-anthropic-billing-h-e-a-d-e-r  "
	if out := sanitizeText(in); out != in {
		t.Errorf("无规则生效时被 TrimSpace 改动: %q -> %q", in, out)
	}
	// 有实际改写时仍应 TrimSpace。
	if out := sanitizeText("  x-anthropic-billing-header: cc_version=1.0;  "); out != "" {
		t.Errorf("实际改写后未 TrimSpace: %q", out)
	}
}

func TestBillingHeaderStrippedValueIrrelevant(t *testing.T) {
	out := sanitizeText(ccHeader)
	if strings.Contains(out, "x-anthropic-billing-header") {
		t.Errorf("header not stripped: %q", out)
	}
}

// 附加验证：正常对话里出现 github.com/anthropics/ 链接（但不是反馈句整句）
// 时，预检特征命中（进入净化），但改写层只动精确匹配的整句——普通链接文本
// 不该被改写。同理，既不含 11128 也不含反馈整句的文本原样返回。
func TestNormalAnthropicLinkNotRewritten(t *testing.T) {
	in := "see https://github.com/anthropics/anthropic-cookbook for examples"
	if out := sanitizeText(in); out != in {
		t.Errorf("normal anthropic link should be untouched: %q -> %q", in, out)
	}
}

// Codex instructions 首段：命中且整句改写（in→within 前缀中段打断），逐字指纹
// 被破坏、语义保留。2026-11-02 探针实测：旧「CLI tool」句尾插词产物仍被拦——
// 上游按前缀锚定，句尾插词打不断。
func TestCodexInstructionsRewritten(t *testing.T) {
	out := sanitizeText(codexInstructions)
	if strings.Contains(out, codexInstructions) {
		t.Errorf("codex fingerprint still present: %q", out)
	}
	if !strings.Contains(out, "You are a coding agent running within the Codex CLI, a terminal-based coding assistant.") {
		t.Errorf("codex first sentence not rewritten: %q", out)
	}
	// 其余两句原样保留，语义不变。
	if !strings.Contains(out, "Codex CLI is an open source project led by OpenAI.") ||
		!strings.Contains(out, "You are expected to be precise, safe, and helpful.") {
		t.Errorf("codex remaining sentences altered: %q", out)
	}
}

// 预检必须能认出 Codex 特征（此前只含 Claude Code，导致提前放行）。
func TestCodexFingerprintDetected(t *testing.T) {
	if !hasFingerprint(codexInstructions) {
		t.Error("codex fingerprint not detected by precheck")
	}
}

// 非精确变体不应被改写：仅去掉其中一个词即视为已破坏，无需改动。
func TestCodexVariantNotTouched(t *testing.T) {
	in := "You are a coding agent running in a CLI, a terminal-based coding assistant."
	if out := sanitizeText(in); out != in {
		t.Errorf("already-broken variant should be untouched: %q -> %q", in, out)
	}
}

func TestBillingHeaderCaseInsensitive(t *testing.T) {
	alt := "X-Anthropic-Billing-Header: cc_version=1.0;"
	out := strings.ToLower(sanitizeText(alt))
	if strings.Contains(out, "billing") {
		t.Errorf("case-insensitive header not stripped: %q", out)
	}
}

func TestTrailingKVStripped(t *testing.T) {
	out := sanitizeText("...; cc_version=2.0; cc_entrypoint=cli;")
	if strings.Contains(out, "cc_version") || strings.Contains(out, "cc_entrypoint") {
		t.Errorf("trailing kv not stripped: %q", out)
	}
}

func TestExactMatchOnlyVariantNotTouched(t *testing.T) {
	in := "...official CLI for Claude!"
	if out := sanitizeText(in); out != in {
		t.Errorf("variant should be untouched: %q -> %q", in, out)
	}
}

func TestUserFreeTextNotTouched(t *testing.T) {
	in := "please use main branch for this repo"
	if out := sanitizeText(in); out != in {
		t.Errorf("free text should be untouched: %q -> %q", in, out)
	}
}

func TestNoFeatureReturnsSameString(t *testing.T) {
	in := "ordinary user message"
	if out := sanitizeText(in); out != in {
		t.Errorf("no-feature text should pass through unchanged: %q -> %q", in, out)
	}
}

func TestMultimodalTextPartOnly(t *testing.T) {
	imgPart := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "..."}}
	content := []any{
		map[string]any{"type": "text", "text": ccIdentity},
		imgPart,
	}
	out, changed := sanitizeContent(content)
	if !changed {
		t.Fatal("expected change")
	}
	parts := out.([]any)
	txt, _ := parts[0].(map[string]any)["text"].(string)
	if !strings.Contains(txt, "CLI tool") {
		t.Errorf("text part not sanitized: %q", txt)
	}
	img, _ := parts[1].(map[string]any)
	if img["type"] != "image" || img["source"].(map[string]any)["data"] != "..." {
		t.Error("image part modified")
	}
}

// 出站边界集成：ChatStream 发往上游的 wire body 必须无残留指纹。
func TestChatStreamWireBodySanitized(t *testing.T) {
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotBody, _ = io.ReadAll(r.Body)
		return jsonResp(200, "data: [DONE]\n\n"), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", EnterpriseID: "e1"}

	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + ccIdentity + ` ` + ccHeader + `"},` +
		`{"role":"user","content":"hi"}]}`)
	rc, status, respBody, err := c.ChatStream(a, body)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if status >= 400 {
		t.Fatalf("upstream status %d: %s", status, respBody)
	}
	// 上游收到的 body：stream 强制 + 指纹已净化
	var obj map[string]any
	if err := json.Unmarshal(gotBody, &obj); err != nil {
		t.Fatalf("wire body not json: %v", err)
	}
	if obj["stream"] != true {
		t.Error("wire body stream not forced")
	}
	sys, _ := obj["messages"].([]any)[0].(map[string]any)["content"].(string)
	for _, fp := range []string{"x-anthropic-billing-header", ccIdentity, ccBranch} {
		if strings.Contains(sys, fp) {
			t.Errorf("wire body contains fingerprint %q: %q", fp, sys)
		}
	}
}

// TestBareHeaderUntouchedAfterProbe 裸键名（无冒号）2026-11-02 探针实测放行
// （system 与 assistant 双角色均 200，F4「反引号引用裸键名即拦」未复现）：
// 必须原样保留，防停用规则被悄悄复活。键值形态仍拦，继续整段删除。
func TestBareHeaderUntouchedAfterProbe(t *testing.T) {
	for _, in := range []string{
		"引用 `x-anthropic-billing-header` 这个键",
		"lower: x-anthropic-billing-header",
		"mixed: X-Anthropic-Billing-Header",
	} {
		out := sanitizeText(in)
		if out != in {
			t.Fatalf("裸键名被误改（判据 2026-11-02 实测已失效）: in=%q out=%q", in, out)
		}
	}
	// 键值形态仍是活指纹（2026-11-02 探针复现 400），必须整段删除。
	out := sanitizeText("prefix x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli; suffix")
	if strings.Contains(strings.ToLower(out), "x-anthropic-billing") {
		t.Fatalf("键值形态应整段删除: out=%q", out)
	}
}

// TestReasoningContentSanitized reasoning_content（思维链回填字段）与 content
// 同等净化——实测该字段同样携带指纹（2026-11-02 判据快照：键值形态仍是活指纹；
// 裸键名已实测放行不再改写）。
func TestReasoningContentSanitized(t *testing.T) {
	msgs := []any{
		map[string]any{
			"role":              "assistant",
			"content":           nil,
			"reasoning_content": "上文出现过 x-anthropic-billing-header: cc_version=1.0; 键值段",
		},
	}
	if !sanitizeMessages(msgs) {
		t.Fatal("reasoning_content 中的指纹未被净化")
	}
	rc := msgs[0].(map[string]any)["reasoning_content"].(string)
	if strings.Contains(rc, "x-anthropic-billing") {
		t.Fatalf("reasoning_content 指纹残留: %q", rc)
	}
}

// 集成：完整请求体经 PrepareBody 净化后无残留指纹，且 stream/tool_choice 行为不受影响。
func TestPrepareBodySanitizesSystem(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[` +
		`{"role":"system","content":"` + ccIdentity + ` ` + ccHeader + `"},` +
		`{"role":"user","content":"hi"}]}`)
	out := PrepareBody(body)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["stream"] != true {
		t.Error("stream not forced")
	}
	msgs := obj["messages"].([]any)
	sys, _ := msgs[0].(map[string]any)["content"].(string)
	if strings.Contains(sys, "x-anthropic-billing-header") || strings.Contains(sys, ccIdentity) || strings.Contains(sys, ccBranch) {
		t.Errorf("fingerprints remain: %q", sys)
	}
	if !strings.Contains(sys, "CLI tool") {
		t.Errorf("rewrite missing: %q", sys)
	}
}

// TestSanitizeLiteralByteExact 逐字节快照护栏：把 sanitizeFeatures 10 项 +
// sanitizeStringRewrites 5 对 + 3 个活跃正则的当前字节值硬编码断言。
// 这些字面量是实验逆向出的上游内容审核黑名单（无契约可引用），上游按逐字精确
// 匹配拦截，一字之差即漏拦（400 code=11128）或误伤。任何未来改动（含看似无害的
// 统一常量/抽配置、以及复活 2026-11-02 探针停用的规则）都会先红在本测试——必须
// 先用 sanitize_workbuddy_test.go 探针真机重验，再同步更新本快照（判据快照
// 2026-11-02：11128 / 裸键名 / tools 品牌串实测放行，规则仍停用；
// 2026-11-07：品牌串恢复为活指纹，并新增「品牌串全量缩写为 DSH」正则）。
func TestSanitizeLiteralByteExact(t *testing.T) {
	// 全小写表（上游判据大小写不敏感，配合 asciiContainsFold 折叠匹配）。
	features := []string{
		"x-anthropic-billing-hdr",
		"x-anthropic-billing-header",                      // header 键值段键名
		"cc_entrypoint=",                                  // 尾随裸键值（截断前缀即可命中）
		"you are claude code",                             // 身份句（截断前缀即可命中）
		"main branch (",                                   // 注入指令句（截断前缀即可命中）
		"you are a coding agent running in the codex cli", // Codex instructions 首段（截断前缀即可命中）
		"github.com/anthropics/",                          // 反馈句里的 Anthropic 仓库链接
		// DSH 身份句：上游按 system 开头逐字锚定（2026-10-21 实测，2026-11-02 复现仍拦）
		"you are an ai agent powered by deepseek harness",
		// DSH 品牌串超集（2026-11-07 恢复：WebGUI/checkout 段品牌原样即 400）
		"deepseek harness",
		// DSH @ 路径句（跨句指纹后半，2026-10-28 实测，2026-11-02 复现仍拦）
		"relative paths resolve from the workspace root",
	}
	if len(sanitizeFeatures) != len(features) {
		t.Fatalf("sanitizeFeatures 项数=%d want %d（快照与实现不同步，见测试头注释的验证步骤）", len(sanitizeFeatures), len(features))
	}
	for i, want := range features {
		if sanitizeFeatures[i] != want {
			t.Errorf("sanitizeFeatures[%d]=%q want %q（逐字节不一致，改动前先走快照验证流程）", i, sanitizeFeatures[i], want)
		}
	}

	rewrites := [][2]string{
		{
			"You are Claude Code, Anthropic's official CLI for Claude",
			"You are Claude Code, Anthropic's official CLI tool for Claude",
		},
		{
			"Main branch (you will usually use this for PRs)",
			"Default branch (you will usually use this for PRs)",
		},
		{
			// Codex 首段：in→within 前缀中段打断（2026-11-02 实测 200；旧「CLI tool」
			// 句尾插词产物实测仍被拦——上游按前缀锚定）。
			"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
			"You are a coding agent running within the Codex CLI, a terminal-based coding assistant.",
		},
		{
			"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
			"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		},
		{
			// DSH 身份句：精确对直接落到缩写终态（与 "You are an AI agent powered by DSH" 一致）。
			"You are an AI agent powered by DeepSeek Harness",
			"You are an AI agent powered by DSH",
		},
	}
	if len(sanitizeStringRewrites) != len(rewrites) {
		t.Fatalf("sanitizeStringRewrites 对数=%d want %d（快照与实现不同步，见测试头注释的验证步骤）", len(sanitizeStringRewrites), len(rewrites))
	}
	for i, want := range rewrites {
		if sanitizeStringRewrites[i][0] != want[0] || sanitizeStringRewrites[i][1] != want[1] {
			t.Errorf("sanitizeStringRewrites[%d]=%q→%q want %q→%q（逐字节不一致，改动前先走快照验证流程）",
				i, sanitizeStringRewrites[i][0], sanitizeStringRewrites[i][1], want[0], want[1])
		}
	}

	// 正则改写表的逐字节快照：3 对活跃（2026-11-07 新增品牌串全量缩写）。
	wantRegexpRewrites := []struct{ pattern, replace string }{
		{`(?i)you\s+are\s+an\s+ai\s+agent\s+powered\s+by\s+(?:the\s+)?deepseek[\s\-_]*harness`,
			"You are an AI agent powered by DSH"},
		{`(?i)deepseek[\s\-_]*harness`, "DSH"},
		{`(?i)relative\s+paths\s+resolve\s+from\s+the\s+workspace\s+root`,
			"Relative paths are resolved from the workspace root"},
	}
	if len(sanitizeRegexpRewrites) != len(wantRegexpRewrites) {
		t.Fatalf("sanitizeRegexpRewrites 对数=%d want %d", len(sanitizeRegexpRewrites), len(wantRegexpRewrites))
	}
	for i, want := range wantRegexpRewrites {
		if got := sanitizeRegexpRewrites[i].regexp.String(); got != want.pattern {
			t.Errorf("sanitizeRegexpRewrites[%d].regexp=%q want %q", i, got, want.pattern)
		}
		if got := sanitizeRegexpRewrites[i].replace; got != want.replace {
			t.Errorf("sanitizeRegexpRewrites[%d].replace=%q want %q", i, got, want.replace)
		}
	}

	// 其余剥离层正则（不属于改写表）。
	regexes := map[string]string{
		// 宽松键名正则（hdrKeyRe）：h/e/a/d/e/r 逐字母容忍分隔，header 全形态与
		// hdr 缩写形态都命中；直接写 `hdr` 会漏 "header"（不含连续 hdr）。
		"sanitizeHdrRe":     `(?i)x[^a-z]*anthropic[^a-z]*billing[^a-z]*h[^a-z]*(?:e[^a-z]*)?(?:a[^a-z]*)?d[^a-z]*(?:e[^a-z]*)?r\s*:[^;\n]*;?\s*`,
		"sanitizeBareHdrRe": `(?i)x[^a-z]*anthropic[^a-z]*billing[^a-z]*h[^a-z]*(?:e[^a-z]*)?(?:a[^a-z]*)?d[^a-z]*(?:e[^a-z]*)?r`,
		"sanitizeKvRe":      `(?i)\bcc_[a-z0-9_]+=[^;\n]*;?\s*`,
	}
	for name, want := range regexes {
		var got string
		switch name {
		case "sanitizeHdrRe":
			got = sanitizeHdrRe.String()
		case "sanitizeBareHdrRe":
			got = sanitizeBareHdrRe.String()
		case "sanitizeKvRe":
			got = sanitizeKvRe.String()
		}
		if got != want {
			t.Errorf("%s=%q want %q（逐字节不一致，改动前先走快照验证流程）", name, got, want)
		}
	}
}

// TestSanitizeTablesParityDirect 直接对比两套表：绕过 PrepareBody 的双 pass，
// 生产表与包内 sanitizeMessages 对同一输入必须得到相同的 system content。
// 仅靠 PrepareBody vs Messages 无法发现「只改生产表」——两条臂都会过生产表。
func TestSanitizeTablesParityDirect(t *testing.T) {
	cases := []string{
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
	for _, in := range cases {
		msgs := []any{
			map[string]any{"role": "system", "content": in},
			map[string]any{"role": "user", "content": "hi"},
		}
		body, err := json.Marshal(map[string]any{"model": "m", "messages": msgs})
		if err != nil {
			t.Fatal(err)
		}
		sanitizeMessages(msgs)
		upSys, _ := msgs[0].(map[string]any)["content"].(string)

		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatalf("unmarshal prod: %v", err)
		}
		sanitize.MessagesInPlace(obj)
		prodMsgs, _ := obj["messages"].([]any)
		prodSys, _ := prodMsgs[0].(map[string]any)["content"].(string)
		if upSys != prodSys {
			t.Errorf("两表不一致（只改一份即暴露）\n in=%q\n upstream=%q\n prod=%q", in, upSys, prodSys)
		}
	}
}

// 第三条跨句 @ 路径指纹（2026-10-28 真机实测：t_join 跨句连写回 400、单句各自回 200；
// 2026-11-03 复测：改谓语 are resolved 回 200）：后半句必须被改写，前半句单独出现不是指纹、不得改动。
func TestACPPathCrossSentenceRewritten(t *testing.T) {
	in := "Tokens prefixed with @ are paths the user explicitly referenced. " +
		"Relative paths resolve from the workspace root; anything else is a file: use the read tool."
	out := sanitizeText(in)
	if strings.Contains(out, "Relative paths resolve from the workspace root") {
		t.Errorf("@ 路径句跨句指纹未被改写: %q", out)
	}
	if !strings.Contains(out, "Relative paths are resolved from the workspace root") {
		t.Errorf("@ 路径句改写缺失: %q", out)
	}
	// 前半句单独出现：不是指纹（上游按跨句连写锚定），零改动。
	if got := sanitizeText("Tokens prefixed with @ are paths the user explicitly referenced."); got != "Tokens prefixed with @ are paths the user explicitly referenced." {
		t.Errorf("前半句被误改: %q", got)
	}
}

// 上游大小写不敏感（2026-10-28 真机实测：全小写身份句回 400）：预检与改写都必须认出全小写变体。
func TestLowercaseIdentityRewritten(t *testing.T) {
	in := "you are an ai agent powered by deepseek harness. 你是助手。"
	out := sanitizeText(in)
	if strings.Contains(strings.ToLower(out), "deepseek harness") {
		t.Errorf("小写变体未被改写（上游大小写不敏感，仍会 400）: %q", out)
	}
	if !strings.Contains(out, "You are an AI agent powered by DSH") {
		t.Errorf("小写身份句未整句替换: %q", out)
	}
}

// TestToolsBrandFieldUntouched tools[].function.description 含品牌串实测放行
// （2026-11-02 补测 case 4）；2026-11-07 品牌串在 messages 内恢复为活指纹，
// 但 tools 字段仍按原判据放行，且整包兜底 BodyFallback 已删除——PrepareBody
// 必须原样保留该字段（防整包替换被重新引入）。
func TestToolsBrandFieldUntouched(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"f","description":"Powered by DeepSeek Harness"}}]}`)
	out := PrepareBody(body)
	if !strings.Contains(string(out), "Powered by DeepSeek Harness") {
		t.Errorf("tools 字段品牌串被误改（判据 2026-11-02 实测已失效，不应整包替换）: %q", out)
	}
}
