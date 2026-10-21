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
)

const (
	ccIdentity = "You are Claude Code, Anthropic's official CLI for Claude."
	ccBranch   = "Main branch (you will usually use this for PRs)"
	ccHeader   = "x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli;"
	// Codex instructions 首段（上游逐字精确指纹，三句缺一不可）。
	codexInstructions = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant. Codex CLI is an open source project led by OpenAI. You are expected to be precise, safe, and helpful."
	// DSH 身份句：上游对 system 消息**开头**逐字前缀匹配这一串即回 400 code=11-128
	// （放 user role、非开头、或写成 "powered by DeepSeek." 都放行，2026-10-21 单发 A/B 实测）。
	dshIdentity = "You are an AI agent powered by DeepSeek Harness"
)

// DSH 身份句必须被改写：只插冠词 the，其余措辞原样保留（语义扰动最小）。
func TestDSHIdentityRewritten(t *testing.T) {
	out := sanitizeText(dshIdentity + " You own the repository.")
	if strings.Contains(out, dshIdentity) {
		t.Errorf("DSH identity still present: %q", out)
	}
	if !strings.Contains(out, "powered by the DeepSeek Harness") {
		t.Errorf("DSH rewrite missing: %q", out)
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
	if !strings.Contains(sys, "powered by the DeepSeek Harness") {
		t.Errorf("DSH rewrite missing: %q", sys)
	}
}

// 反向护栏：措辞相近但非逐字形态不得被改写（防误伤）。
func TestDSHVariantNotTouched(t *testing.T) {
	for _, in := range []string{
		"You are an AI agent, powered by DeepSeek Harness.", // 多一个逗号
		"You are an AI agent powered by DeepSeek.",          // 截去 Harness
		"An AI agent powered by DeepSeek Harness.",          // 缺 "You are an"
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

// 上游反探测：请求体里出现裸数字 11128 即整单拦截（与上下文无关）。
// 回归用例：该串会被改写为 11-128 以打断精确匹配。
func TestUpstreamErrorCodeRewritten(t *testing.T) {
	in := "upstream returned code=11128 for this request"
	out := sanitizeText(in)
	if strings.Contains(out, "11128") {
		t.Errorf("error code not rewritten: %q", out)
	}
	if !strings.Contains(out, "11-128") {
		t.Errorf("error code not rewritten as expected: %q", out)
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
				"arguments": `{"command":"echo 11128"}`,
			}},
		}},
	}
	if !sanitizeMessages(msgs) {
		t.Fatal("sanitizeMessages 未报告任何改动，tool_calls 被跳过")
	}
	fn := msgs[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	got := fn["arguments"].(string)
	if strings.Contains(got, "11128") {
		t.Errorf("tool_call arguments 未被净化: %q", got)
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

// Codex instructions 首段：命中预告且整句改写，逐字指纹被破坏、语义保留。
func TestCodexInstructionsRewritten(t *testing.T) {
	out := sanitizeText(codexInstructions)
	if strings.Contains(out, codexInstructions) {
		t.Errorf("codex fingerprint still present: %q", out)
	}
	if !strings.Contains(out, "You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.") {
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

// TestBareHeaderAbbreviated 裸键名（无冒号）兜底缩写——2026-09-13 实验 F4：
// assistant 消息反引号引用裸键名即触发 11128，剥离层 sanitizeHdrRe 要求冒号、
// 对裸串无效。键值形态被整段删除后，残留裸键名做最小缩写（header→hdr），
// 破坏逐字匹配、语义不变、保留可读性。
func TestBareHeaderAbbreviated(t *testing.T) {
	for _, in := range []string{
		"引用 `x-anthropic-billing-header` 这个键",
		"lower: x-anthropic-billing-header",
		"mixed: X-Anthropic-Billing-Header",
	} {
		out := sanitizeText(in)
		if strings.Contains(strings.ToLower(out), "x-anthropic-billing-header") {
			t.Fatalf("裸键名未被兜底: in=%q out=%q", in, out)
		}
		if !strings.Contains(strings.ToLower(out), "x-anthropic-billing-hdr") {
			t.Fatalf("裸键名未缩写为 hdr 形态: in=%q out=%q", in, out)
		}
	}
	// 键值形态仍走整段删除（不留 hdr 残骸）
	out := sanitizeText("prefix x-anthropic-billing-header: cc_version=1.0; cc_entrypoint=cli; suffix")
	if strings.Contains(strings.ToLower(out), "x-anthropic-billing") {
		t.Fatalf("键值形态应整段删除: out=%q", out)
	}
}

// TestReasoningContentSanitized reasoning_content（思维链回填字段）与 content
// 同等净化——实测该字段同样携带指纹。
func TestReasoningContentSanitized(t *testing.T) {
	msgs := []any{
		map[string]any{
			"role":              "assistant",
			"content":           nil,
			"reasoning_content": "上文出现过 `x-anthropic-billing-header` 键名",
		},
	}
	if !sanitizeMessages(msgs) {
		t.Fatal("reasoning_content 中的指纹未被净化")
	}
	rc := msgs[0].(map[string]any)["reasoning_content"].(string)
	if strings.Contains(rc, "x-anthropic-billing-header") {
		t.Fatalf("reasoning_content 指纹残留: %q", rc)
	}
}

// TestReasoningSanitized reasoning（思考强度镜像字段）与 reasoning_content 同等
// 净化——deepseek 回填会把 reasoning_content 镜像进 reasoning，漏净化等于绕开出站脱敏。
func TestReasoningSanitized(t *testing.T) {
	msgs := []any{
		map[string]any{
			"role":      "assistant",
			"content":   nil,
			"reasoning": "上文出现过 `" + sanitizeFeatures[0] + "` 键名",
		},
	}
	if !sanitizeMessages(msgs) {
		t.Fatal("reasoning 中的指纹未被净化")
	}
	r := msgs[0].(map[string]any)["reasoning"].(string)
	if hasFingerprint(r) {
		t.Fatalf("reasoning 指纹残留: %q", r)
	}
}

// 集成：deepseek 回填把 reasoning_content 镜像进 reasoning 后，两字段均无指纹残留。
func TestPrepareBodySanitizesMirroredReasoning(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-pro","messages":[` +
		`{"role":"assistant","content":"a","reasoning_content":"见 ` + sanitizeFeatures[0] + ` 键"}],` +
		`"reasoning_effort":"high"}`)
	out := PrepareBody(body)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msg, _ := obj["messages"].([]any)[0].(map[string]any)
	for _, k := range []string{"reasoning_content", "reasoning"} {
		s, ok := msg[k].(string)
		if !ok || s == "" {
			t.Fatalf("%s 缺失（回填未生效）", k)
		}
		if hasFingerprint(s) {
			t.Fatalf("%s 指纹残留: %q", k, s)
		}
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

// TestSanitizeLiteralByteExact 逐字节快照护栏：把 sanitizeFeatures 8 项 +
// sanitizeRewrites 6 对 + 3 个正则的当前字节值硬编码断言。
// 这些字面量是实验逆向出的上游内容审核黑名单（无契约可引用），上游按逐字精确
// 匹配拦截，一字之差即漏拦（400 code=11128）或误伤。任何未来改动（含看似无害的
// 统一常量/抽配置）都会先红在本测试——必须走 analysis 报告 #6 的逐字节验证
// 步骤：grep 全部出现点（表/正则/测试断言四处同查）+ 真实账号上游实测 +
// TestSanitize 全族回归，方可同步更新本快照。
func TestSanitizeLiteralByteExact(t *testing.T) {
	features := []string{
		"x-anthropic-billing-header", // header 键值段键名
		"cc_entrypoint=",             // 尾随裸键值（截断前缀即可命中）
		"You are Claude Code",        // 身份句（截断前缀即可命中）
		"Main branch (",              // 注入指令句（截断前缀即可命中）
		"You are a coding agent running in the Codex CLI", // Codex instructions 首段（截断前缀即可命中）
		"github.com/anthropics/",                          // 反馈句里的 Anthropic 仓库链接
		"11128",                                           // 上游反探测：裸数字错误码
		// DSH 身份句：上游按 system 开头逐字锚定（2026-10-21 实测）
		"You are an AI agent powered by DeepSeek Harness",
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
			"You are a coding agent running in the Codex CLI, a terminal-based coding assistant.",
			"You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.",
		},
		{
			"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
			"To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues",
		},
		{
			"11128",
			"11-128",
		},
		{
			// DSH 身份句：只插冠词 the，保留 powered by 原措辞，语义扰动最小
			"You are an AI agent powered by DeepSeek Harness",
			"You are an AI agent powered by the DeepSeek Harness",
		},
	}
	if len(sanitizeRewrites) != len(rewrites) {
		t.Fatalf("sanitizeRewrites 对数=%d want %d（快照与实现不同步，见测试头注释的验证步骤）", len(sanitizeRewrites), len(rewrites))
	}
	for i, want := range rewrites {
		if sanitizeRewrites[i][0] != want[0] || sanitizeRewrites[i][1] != want[1] {
			t.Errorf("sanitizeRewrites[%d]=%q→%q want %q→%q（逐字节不一致，改动前先走快照验证流程）",
				i, sanitizeRewrites[i][0], sanitizeRewrites[i][1], want[0], want[1])
		}
	}

	regexes := map[string]string{
		"sanitizeHdrRe":     `(?i)x-anthropic-billing-header:[^;\n]*;?\s*`,
		"sanitizeBareHdrRe": `(?i)x-anthropic-billing-header`,
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
