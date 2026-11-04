package oczen

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/provider"
)

// 头/提示词形状断言（2026-11-04 的官方 OpenCode 2.2.5 本机抓包口径，R154）。
var (
	traceShapeB3       = regexp.MustCompile(`^[0-9a-f]{32}-[0-9a-f]{16}-1-[0-9a-f]{16}$`)
	traceShapeW3C      = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`)
	environmentBlockRE = regexp.MustCompile(`<env>\r?\n {2}Working directory: [^\n]*\n[\s\S]*?</env>`)
)

// 官方 2.2.5 正文里的两处标志串（抓包逐字）。
const (
	envHeader2 = "Here is some useful information about the environment you are running in:"
	// officialWorktreeNote 是官方 `<env>` 块**之后**那句话的开头：它的存在本身就证明
	// 「env 块必须收尾」不成立。
	officialWorktreeNote = "When you create a worktree outside the current working directory"
)

// promptHead 取字符串开头 n 个字符（错误信息用）。
func promptHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// systemText 把一条 system 消息的 content 摊平成文本。
func systemText(content any) string { return strings.Join(textFragments(content), "\n") }

// tailOf 取字符串末尾 n 个字符（错误信息用）。
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// sha1Hex 供 project 派生口径断言使用。
func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestCanonicalSessionID 会话头须为官方形状+官方编码
// ses_<12位小写hex><14位Base62>，时间戳段须是 Identifier.descending() 编码
// （末三位 0xFFF-counter，实测只接受 counter 1–63）。
// 形状/编码不对，或摘要段（旧实现）→ 确定性重编码；官方签发的 descending ID 原样保留。
func TestCanonicalSessionID(t *testing.T) {
	got := canonicalSessionID("hello conversation")
	if len(got) != 4+12+14 || !strings.HasPrefix(got, "ses_") {
		t.Fatalf("形状不符: %q (len=%d)", got, len(got))
	}
	for i := 4; i < 16; i++ {
		if c := got[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			t.Fatalf("时间戳段非小写 hex: %q", got)
		}
	}
	if !descendingTimePartPattern.MatchString(got[4:16]) {
		t.Fatalf("生成的 session 时间戳段非 descending 编码: %q", got)
	}
	// 稳定性：同一会话种子必须映射到同一 session（prompt cache 亲和）
	if again := canonicalSessionID("hello conversation"); again != got {
		t.Fatalf("不稳定：%q != %q", again, got)
	}
	// 不同种子应不同
	if other := canonicalSessionID("another conversation"); other == got {
		t.Fatalf("不同种子撞同一 session: %q", got)
	}
}

// TestDescendingTimePartInsideUpstreamWindow 上游自 2026-10-25 起对 ses_ 时间戳做
// 合理性校验：解码后超出约一年窗口回 429 FreeUsageLimitError（与真实配额耗尽同一报文，
// 参考实现 opencode2api 9533101）。哈希直派生（旧实现）落在 1970 年代必被拒；
// 修法锚定今日 UTC 零点 − 哈希偏移(≤30天)，解码日期（mod 2^36 口径）必须落在今日附近。
func TestDescendingTimePartInsideUpstreamWindow(t *testing.T) {
	id := canonicalSessionID("hello conversation")
	var inv uint64
	b, _ := hex.DecodeString(id[4:16])
	for _, c := range b {
		inv = inv<<8 | uint64(c)
	}
	cur := ^inv & 0xFFFFFFFFFFFF
	decodedMS := int64(cur >> 12) // 官方解码口径：36 位 ms（mod 2^36 回绕语义）
	const wrap = int64(1) << 36
	nowMS := time.Now().UnixMilli() % wrap
	if nowMS < 0 {
		nowMS += wrap
	}
	if decodedMS < 0 {
		decodedMS += wrap
	}
	diff := decodedMS - nowMS
	if diff < 0 {
		diff = -diff
	}
	if diff > int64(31*24*time.Hour/time.Millisecond) {
		t.Fatalf("解码日期距今日 %.1f 天，超出上游接受窗口（哈希直派生会落在 1970 年代必被拒）", float64(diff)/86400000)
	}
	// 哈希只挑偏移、不提供日期：同一种子同一天内稳定（对话亲和）
	if id2 := canonicalSessionID("hello conversation"); id2 != id {
		t.Fatalf("不稳定: %q != %q", id2, id)
	}
}

// TestCanonicalSessionIDKeepsGenuineDescendingIDs 官方客户端签发的时间戳段
// （末三位 0xFFF-counter）必须原样保留，保住 prompt cache 亲和。
func TestCanonicalSessionIDKeepsGenuineDescendingIDs(t *testing.T) {
	genuine := []string{
		"ses_ecc5beb7cffe2U0aluUnb5nnpZ", // counter 1
		"ses_ecc441adcffep73ndhq1lBqQim", // counter 2
		"ses_f02a5a612ffepq6RK15QX04Se0", // counter 16
		"ses_4892557ffffeDiICzp8gKhMR0u", // counter 48
		"ses_ecc1f76c9fef3zWGdD1GSCEwPf",
		"ses_ecc1f76c9fcf3zWGdD1GSCEwPf",
		"ses_ecc1f76c9fce3zWGdD1GSCEwPf",
	}
	for _, id := range genuine {
		if got := canonicalSessionID(id); got != id {
			t.Fatalf("canonicalSessionID(%q) = %q, 应原样保留", id, got)
		}
	}
}

// TestCanonicalSessionIDReencodesForeignEncodings 外形对但编码外来（旧摘要段 /
// 升序 ID）必须重编码成 descending 布局，否则上游 403 FreeTierError。
func TestCanonicalSessionIDReencodesForeignEncodings(t *testing.T) {
	foreign := []string{
		"ses_133a414840012U0aluUnb5nnpZ", // 升序 ID（001 后缀）
		"ses_ecc1f76c9f9b3zWGdD1GSCEwPf", // counter 100（9b1 后缀）
		"ses_088a704c30424N5kWDK0snKFbd", // 旧摘要段布局
	}
	for _, id := range foreign {
		got := canonicalSessionID(id)
		if len(got) != 4+12+14 || !strings.HasPrefix(got, "ses_") {
			t.Fatalf("canonicalSessionID(%q) = %q, 非规范外形", id, got)
		}
		if !descendingTimePartPattern.MatchString(got[4:16]) {
			t.Fatalf("canonicalSessionID(%q) = %q, 时间戳段非 descending 编码", id, got)
		}
		if got == id {
			t.Fatalf("canonicalSessionID(%q) 保留了外来编码", id)
		}
	}
}

// TestDescendingTimePartLayout 低 12 位恒为 0xFFE（counter 1 取反）。
func TestDescendingTimePartLayout(t *testing.T) {
	got := descendingTimePart(time.Now(), []byte{0, 0, 0, 0, 1, 0xFF})
	if got[9:] != "ffe" {
		t.Fatalf("descendingTimePart = %q, 末三位应为 ffe", got)
	}
}

// TestShapeChatRequestOfficialToolset 整形层必须把纯聊天请求变成「官方 CLI 会话」：
// 官方 13 工具齐全（各带 strict:false）、官方 system prompt 就位、
// stream_options.include_usage=true、max_completion_tokens=32000、
// tool_choice:none 被删除（v2 闸门，R152）。
func TestShapeChatRequestOfficialToolset(t *testing.T) {
	obj := map[string]any{
		"model":       "big-pickle",
		"stream":      false,
		"tool_choice": "none",
		"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
	}
	if renames := shapeChatRequest(obj); renames.len() != 0 {
		t.Fatalf("纯聊天不应产生工具改名，实际 %d 条", renames.len())
	}
	if obj["stream"] != true {
		t.Fatalf("stream 未强制为 true: %v", obj["stream"])
	}
	names := toolNames(obj)
	for _, want := range officialToolOrder {
		if !names[want] {
			t.Errorf("请求体缺少官方工具 %q", want)
		}
	}
	if len(names) != len(officialToolOrder) {
		t.Errorf("官方工具集应恰好 %d 个，实际 %d: %v", len(officialToolOrder), len(names), names)
	}
	// 2.x 的 Chat 形状给每个官方工具标 strict:false。
	for _, raw := range obj["tools"].([]any) {
		tool, _ := raw.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		if fn == nil || fn["strict"] != false {
			t.Errorf("官方工具 %v 缺少 strict:false", fn["name"])
		}
	}
	if _, ok := obj["tool_choice"]; ok {
		t.Errorf("tool_choice:none 必须删除（上游读成非 CLI 形态）: %v", obj["tool_choice"])
	}
	options, _ := obj["stream_options"].(map[string]any)
	if options["include_usage"] != true {
		t.Errorf("缺少 stream_options.include_usage=true: %v", obj["stream_options"])
	}
	if got := obj["max_completion_tokens"]; got != v2CompletionLimit {
		t.Errorf("max_completion_tokens = %v, want %v", got, v2CompletionLimit)
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("messages 被清空")
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || !carriesOfficialPrompt(first["content"]) {
		t.Fatalf("官方 system prompt 未注入: %+v", first)
	}
}

// TestShapeChatRequestRenamesClientTools 客户端工具必须改名成不透明线上名，
// 且 tool_choice 与 messages.tool_calls 里的引用同步改写（保证 body 自洽）。
func TestShapeChatRequestRenamesClientTools(t *testing.T) {
	obj := map[string]any{
		"model":  "big-pickle",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{
					"name": "ReportFindings", "arguments": "{}"}},
			}},
			map[string]any{"role": "user", "content": "hi"},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name":        "ReportFindings",
			"description": "report findings",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		}}},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "ReportFindings"}},
	}

	renames := shapeChatRequest(obj)
	wire, ok := renames.wireOf("ReportFindings")
	if !ok {
		t.Fatal("客户端工具未登记改名")
	}
	if !strings.HasPrefix(wire, clientToolPrefix) || len(wire) != len(clientToolPrefix)+clientToolDigestLength {
		t.Fatalf("线上名形态不符: %q", wire)
	}
	if got := renames.resolve(wire); got != "ReportFindings" {
		t.Fatalf("resolve(%q) = %q, want ReportFindings", wire, got)
	}
	names := toolNames(obj)
	if names["ReportFindings"] {
		t.Fatalf("客户端原名不得残留在 tools 里: %v", names)
	}
	if !names[wire] {
		t.Fatalf("改名后的工具不在 tools 里: %v", names)
	}
	for _, want := range officialToolOrder {
		if !names[want] {
			t.Errorf("请求体缺少官方工具 %q", want)
		}
	}

	choice, _ := obj["tool_choice"].(map[string]any)
	choiceFn, _ := choice["function"].(map[string]any)
	if choiceFn["name"] != wire {
		t.Errorf("tool_choice 未同步改名: %v", obj["tool_choice"])
	}

	msgs, _ := obj["messages"].([]any)
	var callName any
	for _, raw := range msgs {
		msg, _ := raw.(map[string]any)
		calls, _ := msg["tool_calls"].([]any)
		for _, rawCall := range calls {
			call, _ := rawCall.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			callName = fn["name"]
		}
	}
	if callName != wire {
		t.Errorf("messages.tool_calls 未同步改名: %v", callName)
	}

	// 同名再整形一轮必须幂等（线上名不得被再次改名）。
	before := canonicalJSON(obj)
	second := shapeChatRequest(obj)
	if second.len() != 0 {
		t.Errorf("已改名的工具在第二轮又被登记: %d 条", second.len())
	}
	if after := canonicalJSON(obj); after != before {
		t.Error("重复整形不幂等（第二次改动了请求体）")
	}
}

// TestShapeChatRequestIdempotent 重复整形必须是 no-op：prompt 与官方工具都不重复。
func TestShapeChatRequestIdempotent(t *testing.T) {
	obj := map[string]any{
		"model":  "big-pickle",
		"stream": true,
		"messages": []any{
			map[string]any{"role": "system", "content": "caller prompt"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	shapeChatRequest(obj)
	first := canonicalJSON(obj)
	shapeChatRequest(obj)
	if second := canonicalJSON(obj); second != first {
		t.Fatalf("重复整形改变了请求体\nfirst=%s\nsecond=%s", first, second)
	}
	if tools := toolNames(obj); len(tools) != len(officialToolOrder) {
		t.Fatalf("官方工具被重复追加: %d", len(tools))
	}
	// 调用方 system 文本必须保留，官方 prompt 前置而非替换。
	msgs := obj["messages"].([]any)
	firstMsg, _ := msgs[0].(map[string]any)
	content, _ := firstMsg["content"].(string)
	if !strings.Contains(content, "caller prompt") || !strings.HasPrefix(content, officialPromptMarker) {
		t.Fatalf("system 内容被破坏: %q", content)
	}
}

// TestShapeChatRequestKeepsOfficialDefinition 调用方逐字携带官方定义时，
// 该工具不得被改名，也不得被重复追加。
func TestShapeChatRequestKeepsOfficialDefinition(t *testing.T) {
	obj := map[string]any{
		"model":    "big-pickle",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":    []any{officialTool("read")},
	}
	renames := shapeChatRequest(obj)
	if renames.len() != 0 {
		t.Fatalf("官方定义不得被改名: %d 条", renames.len())
	}
	names := toolNames(obj)
	if len(names) != len(officialToolOrder) || !names["read"] {
		t.Fatalf("工具集不符（read 被重复或丢失）: %v", names)
	}
	count := 0
	for _, raw := range obj["tools"].([]any) {
		tool, _ := raw.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		if fn["name"] == "read" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("read 出现 %d 次（应恰好 1 次）", count)
	}
}

func toolNames(obj map[string]any) map[string]bool {
	out := map[string]bool{}
	tools, _ := obj["tools"].([]any)
	for _, raw := range tools {
		if t, ok := raw.(map[string]any); ok {
			if fn, ok := t["function"].(map[string]any); ok {
				if n, ok := fn["name"].(string); ok {
					out[n] = true
				}
			}
		}
	}
	return out
}

// TestIsFreeModel 只暴露免费模型 + big-pickle；地域受限的也保留。
func TestIsFreeModel(t *testing.T) {
	free := []string{"mimo-v2.6-flash-free", "big-pickle", "muse-spark-1.2-contributor-free", "X-FREE", "nemotron-3.5-lightning-free"}
	for _, id := range free {
		if !IsFreeModel(id) {
			t.Errorf("%s 应判为免费", id)
		}
	}
	for _, id := range []string{"claude-opus-4-6", "gpt-5.4", "kimi-k3", ""} {
		if IsFreeModel(id) {
			t.Errorf("%s 不应判为免费", id)
		}
	}
}

// TestFilterFree 过滤结果保留已知模型的上下文/能力，未知免费模型也保留。
func TestFilterFree(t *testing.T) {
	got := filterFree([]string{"claude-opus-4-6", "mimo-v2.6-flash-free", "big-pickle", "brand-new-free"})
	if len(got) != 3 {
		t.Fatalf("期望 3 个免费模型，得到 %d: %+v", len(got), got)
	}
	byID := map[string]provider.ModelInfo{}
	for _, m := range got {
		byID[m.ID] = m
	}
	if m := byID["mimo-v2.6-flash-free"]; m.ContextWindow != 200000 || !m.SupportsReasoning {
		t.Errorf("已知模型未补齐元数据: %+v", m)
	}
	if m := byID["brand-new-free"]; m.ContextWindow != 0 {
		t.Errorf("未知模型不应编造上下文: %+v", m)
	}
}

// capturedRequest 记录一次上游请求的头与体。
type capturedRequest struct {
	header http.Header
	body   map[string]any
}

// isLowerHex 判断 s 是否为 n 位小写 hex。
func isLowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, ch := range s {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

// TestChatStreamRequestShape 端到端校验发往上游的请求头与请求体形态（v2 闸门，R152）：
// 官方 UA、不发 Accept、不泄漏 x-opencode-request、project 每请求随机 40 hex、
// 会话头族齐全，请求体为官方 CLI 会话（官方工具集 + 官方 prompt）。
func TestChatStreamRequestShape(t *testing.T) {
	var mu sync.Mutex
	var got []capturedRequest
	var traceIDs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		got = append(got, capturedRequest{header: r.Header.Clone(), body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"model\":\"mimo-v2.6-flash-free\",\"choices\":[]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	for i := 0; i < 2; i++ {
		body := `{"model":"mimo-v2.6-flash-free","stream":false,"messages":[{"role":"user","content":"say ok"}]}`
		rc, status, _, err := c.ChatStream(AnonymousAuth(), []byte(body))
		if err != nil || status != 200 {
			t.Fatalf("ChatStream 失败: status=%d err=%v", status, err)
		}
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("上游收到 %d 次请求，期望 2 次", len(got))
	}
	for i, req := range got {
		h := req.header
		if v := h.Get("Authorization"); v != "Bearer "+AnonymousKey {
			t.Errorf("[%d] 匿名凭证错误: %q", i, v)
		}
		if v := h.Get("x-opencode-client"); v != "cli" {
			t.Errorf("[%d] x-opencode-client 错误: %q", i, v)
		}
		if v := h.Get("User-Agent"); v != userAgent {
			t.Errorf("[%d] UA 必须是官方 v2 指纹 %q，实际 %q", i, userAgent, v)
		}
		// 官方 2.2.5 抓包里确实带 Accept: */*（R152「不得发 Accept」已按证据修正）。
		if v := h.Get("Accept"); v != "*/*" {
			t.Errorf("[%d] Accept 必须与官方抓包一致（*/*），实际 %q", i, v)
		}
		// 2026-10-14 起上游把 x-opencode-request 列为 403 触发头（R69/headered）。
		if v := h.Get("x-opencode-request"); v != "" {
			t.Errorf("[%d] 不得发送 x-opencode-request: %q", i, v)
		}
		for _, k := range []string{"x-opencode-session", "x-opencode-session-id", "x-session-affinity", "X-Session-Id"} {
			if v := h.Get(k); !strings.HasPrefix(v, "ses_") || len(v) != 30 {
				t.Errorf("[%d] %s 非规范会话形状: %q", i, k, v)
			}
		}
		// project 必须是裸 40 位小写 hex（prj_ 前缀的规范形态会被拒 403）。
		proj := h.Get("x-opencode-project")
		if !isLowerHex(proj, 40) {
			t.Errorf("[%d] x-opencode-project 必须是裸 40 位小写 hex，实际 %q", i, proj)
		}
		// 链路头：官方 Effect tracer 的 B3 + W3C 两种形态。
		b3 := h.Get("b3")
		tp := h.Get("traceparent")
		if !traceShapeB3.MatchString(b3) {
			t.Errorf("[%d] b3 形状不对: %q", i, b3)
		}
		if !traceShapeW3C.MatchString(tp) {
			t.Errorf("[%d] traceparent 形状不对: %q", i, tp)
		}
		traceIDs = append(traceIDs, b3[:32]+"|"+tp[3:35])
		if req.body["stream"] != true {
			t.Errorf("[%d] 请求体未强制 stream=true: %v", i, req.body["stream"])
		}
		names := toolNames(req.body)
		for _, want := range officialToolOrder {
			if !names[want] {
				t.Errorf("[%d] 请求体缺少官方工具 %q", i, want)
			}
		}
		msgs, _ := req.body["messages"].([]any)
		if len(msgs) == 0 {
			t.Fatalf("[%d] 请求体没有 messages", i)
		}
		first, _ := msgs[0].(map[string]any)
		if first["role"] != "system" || !carriesOfficialPrompt(first["content"]) {
			t.Errorf("[%d] 官方 system prompt 未注入", i)
		}
		// 闸门本体：system 里必须有官方 `<env>` 块，且块后跟着官方那句 worktree 说明
		// （2.2.5 抓包；「以 </env> 收尾」是 R153 的反向误判）。
		if sys := systemText(first["content"]); !environmentBlockRE.MatchString(sys) {
			t.Errorf("[%d] system 缺少官方 <env> 环境块（闸门直接判非 OpenCode 客户端）", i)
		} else if idx := strings.Index(sys, "</env>"); idx < 0 || !strings.Contains(sys[idx:], officialWorktreeNote) {
			t.Errorf("[%d] system 的 </env> 之后缺少官方 worktree 句: …%q", i, tailOf(sys, 120))
		}
	}
	// project 是官方工作区级派生值（sha1("directory:"+目录)），跨请求稳定。
	firstProj := got[0].header.Get("x-opencode-project")
	secondProj := got[1].header.Get("x-opencode-project")
	if firstProj != secondProj {
		t.Errorf("x-opencode-project 必须按工作区稳定派生（官方整个工作区一个 id），两次为 %q / %q", firstProj, secondProj)
	}
	// 同一会话：trace 稳定、span 每请求新生成。
	if traceIDs[0] != traceIDs[1] {
		t.Errorf("同一会话的 trace 必须稳定，实际 %q / %q", traceIDs[0], traceIDs[1])
	}
}

// TestChatStreamPassthroughError 上游 4xx 原文透传（含分类）。
func TestChatStreamPassthroughError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"RegionError","message":"This model is not available in your country."}}`)
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL)
	_, status, raw, err := c.ChatStream(AnonymousAuth(), []byte(`{"model":"muse-spark-1.3-contributor-free","messages":[]}`))
	if err != nil {
		t.Fatalf("不应返回传输错误: %v", err)
	}
	if status != 403 || !strings.Contains(string(raw), "RegionError") {
		t.Fatalf("上游响应未透传: status=%d body=%s", status, raw)
	}
	if k := c.Classify(status, string(raw)); k != provider.ErrPassthrough {
		t.Fatalf("地域受限 403 必须归为透传（不累错不冷却，单账号渠道一冷却即整渠道下线）: %v", k)
	}
}

// TestFetchModelsFallsBackToStatic 上游不可达时回静态免费清单，不报错。
func TestFetchModelsFallsBackToStatic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL)
	models, err := c.FetchModels(AnonymousAuth())
	if err != nil {
		t.Fatalf("兜底不应报错: %v", err)
	}
	if len(models) != len(freeIDs) {
		t.Fatalf("兜底清单长度不符: %d", len(models))
	}
	for _, m := range models {
		if !IsFreeModel(m.ID) {
			t.Fatalf("兜底清单混入非免费模型: %s", m.ID)
		}
	}
}

// TestFetchModelsFilters 动态列表只保留免费模型 + big-pickle。
func TestFetchModelsFilters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"object":"list","data":[
			{"id":"claude-opus-4-6"},{"id":"big-pickle"},{"id":"mimo-v2.6-flash-free"},
			{"id":"muse-spark-1.3-contributor-free"},{"id":"gpt-5.4"}]}`)
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL)
	models, err := c.FetchModels(AnonymousAuth())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range models {
		ids[m.ID] = true
	}
	if len(ids) != 3 || !ids["big-pickle"] || !ids["mimo-v2.6-flash-free"] || !ids["muse-spark-1.3-contributor-free"] {
		t.Fatalf("过滤结果不符: %v", ids)
	}
}

// TestStreamRewritesModel 流式响应中的 model 字段回填成客户端模型名。
func TestStreamRewritesModel(t *testing.T) {
	in := strings.NewReader("data: {\"id\":\"x\",\"model\":\"mimo-v2.6-flash-free\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	rec := httptest.NewRecorder()
	if _, err := New().Stream(rec, in, "oczen/mimo-v2.6-flash-free"); err != nil {
		t.Fatalf("Stream 失败: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"model":"oczen/mimo-v2.6-flash-free"`) {
		t.Fatalf("model 未回填: %s", out)
	}
	if !strings.Contains(out, `"content":"ok"`) || !strings.Contains(out, "[DONE]") {
		t.Fatalf("流内容被破坏: %s", out)
	}
}

// TestAggregateRewritesModel 非流式聚合结果回填客户端模型名。
func TestAggregateRewritesModel(t *testing.T) {
	in := strings.NewReader("data: {\"id\":\"x\",\"model\":\"big-pickle\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	resp, err := New().Aggregate(in, "oczen/big-pickle")
	if err != nil {
		t.Fatalf("Aggregate 失败: %v", err)
	}
	if resp["model"] != "oczen/big-pickle" {
		t.Fatalf("model 未回填: %v", resp["model"])
	}
}

// TestClassify 单账号渠道的错误分类：只有 429 才该冷却账号。
func TestClassify(t *testing.T) {
	c := New()
	cases := []struct {
		status int
		body   string
		want   provider.ErrKind
	}{
		{429, `{"error":"rate limit exceeded"}`, provider.ErrSoftRate},
		{403, `{"type":"error","error":{"type":"FreeTierError"}}`, provider.ErrPassthrough},
		{403, `{"type":"error","error":{"type":"RegionError"}}`, provider.ErrPassthrough},
		{401, `unauthorized`, provider.ErrPassthrough},
		{400, `bad request`, provider.ErrPassthrough},
		{404, `not found`, provider.ErrPassthrough},
		{500, `boom`, provider.ErrServer},
		{502, `bad gateway`, provider.ErrServer},
	}
	for _, tc := range cases {
		if got := c.Classify(tc.status, tc.body); got != tc.want {
			t.Errorf("Classify(%d) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// TestAnonymousAuthShape 虚拟账号必须永不触发 token 刷新路径。
func TestAnonymousAuthShape(t *testing.T) {
	a := AnonymousAuth()
	if a.UID != AnonymousUID || a.Nickname != AnonymousName {
		t.Fatalf("虚拟账号身份不符: %+v", a)
	}
	if a.FilePath != "" {
		t.Fatal("虚拟账号不应有凭证文件（否则可能被删/被写回）")
	}
	if a.NeedsRefresh(365 * 24 * 60 * 60 * 1e9) {
		t.Fatal("虚拟账号不应需要刷新（ExpiresAt 必须为远期值）")
	}
}

// TestBaseSelectsLaneByCredential 基址必须按车道分流（R69）。
//
// 上游 2026-10-14 把 **keyless**（匿名 public）推理从 /zen 迁到 /inference，
// 两条车道基址不再相同。issue #117 的现场就是「匿名走 /zen 恒 403」。
// 显式 Base（测试注入）必须仍然优先，否则全部 httptest 用例会打到真上游。
func TestBaseSelectsLaneByCredential(t *testing.T) {
	c := New()
	if got := c.base(); got != AnonymousBase || !strings.Contains(got, "/inference") {
		t.Fatalf("匿名凭证必须走 AnonymousBase(/inference)，实际 %q", got)
	}
	c.SetAPIKey("sk-user-key")
	if got := c.base(); got != DefaultBase {
		t.Fatalf("自备 key 必须走 DefaultBase(/zen)，实际 %q", got)
	}
	c.SetAPIKey("")
	if got := c.base(); got != AnonymousBase {
		t.Fatalf("清空 key 后应回匿名车道，实际 %q", got)
	}
	// 显式 Base 优先于两条车道（测试注入 httptest 上游）。
	c.Base = "http://127.0.0.1:1"
	if got := c.base(); got != c.Base {
		t.Fatalf("显式 Base 必须优先，实际 %q", got)
	}
}

// TestChatURL 对话端点按车道分流：匿名走 v2 的 /inference/openai/v1 族，
// 自备 key 走 /zen，显式 Base（测试注入）优先（R69/R152）。
func TestChatURL(t *testing.T) {
	c := New()
	if got := c.chatURL(); got != AnonymousChatURL {
		t.Fatalf("匿名对话端点应为 AnonymousChatURL，实际 %q", got)
	}
	if !strings.HasSuffix(AnonymousChatURL, "/inference/openai/v1/chat/completions") {
		t.Fatalf("匿名对话端点必须落在 v2 的 /inference/openai/v1 族，实际 %q", AnonymousChatURL)
	}
	c.SetAPIKey("sk-user-key")
	if got := c.chatURL(); got != DefaultBase+"/chat/completions" {
		t.Fatalf("自备 key 必须走 DefaultBase，实际 %q", got)
	}
	c.SetAPIKey("")
	if got := c.chatURL(); got != AnonymousChatURL {
		t.Fatalf("清空 key 后应回匿名车道，实际 %q", got)
	}
	c.Base = "http://127.0.0.1:1"
	if got := c.chatURL(); got != "http://127.0.0.1:1/chat/completions" {
		t.Fatalf("显式 Base 必须优先，实际 %q", got)
	}
	// 末尾斜杠不得拼出双斜杠。
	c.Base = "http://127.0.0.1:1/"
	if got := c.chatURL(); got != "http://127.0.0.1:1/chat/completions" {
		t.Fatalf("显式 Base 末尾斜杠未归一，实际 %q", got)
	}
}

// TestProjectID 官方 project id 是**工作区级派生值**：裸 40 位小写 hex，
// 跨请求稳定（OpenCode `packages/core/src/project.ts`：sha1("directory:"+目录)）。
func TestProjectID(t *testing.T) {
	a := projectID()
	b := projectID()
	if !isLowerHex(a, 40) {
		t.Fatalf("project 必须是 40 位小写 hex，实际 %q", a)
	}
	if a != b {
		t.Fatalf("project 必须按工作区稳定派生，两次得到 %q / %q", a, b)
	}
	// 派生口径逐字对齐官方：sha1("directory:" + 工作目录)。
	want := sha1Hex("directory:" + workDir())
	if a != want {
		t.Fatalf("project 派生口径不符：got %q want %q", a, want)
	}
}

// TestOfficialPromptIsCaptured225 官方 2.2.5 正文必须逐项来自本机抓包（R154）：
// 首句、机型段、skills、日期、`<env>` 块、以及**块之后**的 worktree 句。
func TestOfficialPromptIsCaptured225(t *testing.T) {
	sys := officialSystemPrompt("big-pickle")
	if !strings.HasPrefix(sys, officialPromptMarker) {
		t.Fatalf("官方正文首句不符: %q", promptHead(sys, 80))
	}
	if strings.Contains(sys, "{{") || strings.Contains(sys, "}}") {
		t.Fatalf("正文残留未替换占位符: %q", promptHead(sys, 120))
	}
	for _, want := range []string{
		"# Harness",
		"# Communication",
		"# Working in codebases",
		"# Your Model",
		"# Code Mode",
		"- Provider ID: opencode",
		"- Model ID: big-pickle",
		"<available_skills>",
		"Today's date: ",
		envHeader2,
		"<env>",
		"  Working directory: " + workDir(),
		"  Workspace root folder: ",
		"  Platform: " + officialPlatform(),
		"  Prefer " + officialTmpDir() + " over generic system temporary directories such as /tmp; it is pre-created and approved for external access.",
		"</env>",
		officialWorktreeNote,
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("官方正文缺少 %q", want)
		}
	}
	// 关键反例：`</env>` 之后**还有** worktree 句 ⇒ 环境块不是收尾段。
	if strings.HasSuffix(strings.TrimSpace(sys), "</env>") {
		t.Fatalf("官方正文不该以 </env> 收尾（R153 的反向结论已被抓包推翻）")
	}
	if idx := strings.Index(sys, "</env>"); idx < 0 || !strings.Contains(sys[idx:], officialWorktreeNote) {
		t.Fatalf("worktree 句不在 </env> 之后")
	}
	// 官方正文里 `<env>` 恰好一块。
	if n := strings.Count(sys, "<env>"); n != 1 {
		t.Fatalf("官方正文里 <env> 出现 %d 次（应为 1）", n)
	}
	// 模型名/ID 必须跟随请求模型（官方 `# Your Model` 是机型相关段）。
	other := officialSystemPrompt("mimo-v2.6-flash-free")
	if !strings.Contains(other, "- Model ID: mimo-v2.6-flash-free") {
		t.Fatalf("模型 ID 未跟随请求: %q", promptHead(other, 60))
	}
	if !strings.Contains(other, "- Name: Mimo V2.6 Flash Free") {
		t.Errorf("模型显示名未按 ID 派生: %q", "Mimo V2.6 Flash Free")
	}
}

// TestOfficialToolsetMatchesCapture 官方工具集必须是抓包里那 12 个：顺序一致、
// 各带 strict:false、`execute` 收尾，且**没有**旧快照里的 `patch`。
func TestOfficialToolsetMatchesCapture(t *testing.T) {
	want := []string{"edit", "glob", "grep", "question", "read", "shell", "skill", "subagent", "webfetch", "websearch", "write", "execute"}
	if len(officialToolOrder) != len(want) {
		t.Fatalf("官方工具数 %d，抓包是 %d", len(officialToolOrder), len(want))
	}
	for i, name := range want {
		if officialToolOrder[i] != name {
			t.Fatalf("第 %d 个官方工具 = %q，抓包是 %q", i, officialToolOrder[i], name)
		}
		entry := officialTool(name)
		fn, _ := entry["function"].(map[string]any)
		if fn == nil {
			t.Fatalf("官方工具 %q 渲染失败", name)
		}
		if fn["strict"] != false {
			t.Errorf("官方工具 %q 缺少 strict:false", name)
		}
		if desc, _ := fn["description"].(string); strings.TrimSpace(desc) == "" {
			t.Errorf("官方工具 %q 描述为空（资产未加载？）", name)
		}
	}
	_, index := loadOfficialTools()
	if _, ok := index["patch"]; ok {
		t.Error("官方 2.2.5 没有 patch 工具，旧快照的它必须消失")
	}
}

// TestShellDescriptionFollowsHostPlatform 官方 shell 描述里的宿主 shell 与
// `<env>` 的 `Platform:` 行必须讲同一个平台故事（同一份资产里两处措辞自洽）。
func TestShellDescriptionFollowsHostPlatform(t *testing.T) {
	fn, _ := officialTool("shell")["function"].(map[string]any)
	desc, _ := fn["description"].(string)
	sys := officialSystemPrompt("big-pickle")
	var want string
	switch {
	case strings.Contains(sys, "Platform: win32"):
		want = "Commands run on Windows using powershell."
	case strings.Contains(sys, "Platform: darwin"):
		want = "Commands run on macOS using zsh."
	default:
		want = "Commands run on Linux using bash."
	}
	if !strings.Contains(desc, want) {
		t.Fatalf("shell 描述未跟随宿主平台: 期望含 %q，实际 %q", want, desc)
	}
}

// TestShapePrependsOfficialPromptAndKeepsCallerText 官方正文**整体连续**前置，
// 调用方 system 文本追加其后；`<env>` 块仍在官方位置（后面还有 worktree 句）。
func TestShapePrependsOfficialPromptAndKeepsCallerText(t *testing.T) {
	obj := map[string]any{
		"model": "mimo-v2.6-flash-free",
		"messages": []any{
			map[string]any{"role": "system", "content": "caller prompt"},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	shapeChatRequest(obj)
	msgs, _ := obj["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	sys, _ := first["content"].(string)
	if !strings.HasPrefix(sys, officialPromptMarker) {
		t.Fatalf("官方正文未前置: %q", promptHead(sys, 80))
	}
	if !strings.Contains(sys, "caller prompt") {
		t.Fatalf("调用方 system 文本被丢弃")
	}
	if idx := strings.Index(sys, "caller prompt"); idx < strings.Index(sys, officialWorktreeNote) {
		t.Errorf("调用方文本应落在官方正文之后")
	}
	if !environmentBlockRE.MatchString(sys) {
		t.Fatalf("system 缺少官方 <env> 环境块: …%q", tailOf(sys, 200))
	}
	if n := strings.Count(sys, "<env>"); n != 1 {
		t.Fatalf("环境块出现 %d 次（应为 1）", n)
	}
}

// TestEnvironmentNotDuplicated 重复整形不得叠加环境块。
func TestEnvironmentNotDuplicated(t *testing.T) {
	obj := map[string]any{
		"model":    "mimo-v2.6-flash-free",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	shapeChatRequest(obj)
	first := canonicalJSON(obj)
	shapeChatRequest(obj)
	if second := canonicalJSON(obj); second != first {
		t.Fatalf("重复整形改变了请求体\nfirst=%s\nsecond=%s", first, second)
	}
	msgs, _ := obj["messages"].([]any)
	sys := systemText(msgs[0].(map[string]any)["content"])
	if n := strings.Count(sys, "<env>"); n != 1 {
		t.Fatalf("环境块出现 %d 次（应恰好 1 次）", n)
	}
}

// TestForeignEnvironmentStillGetsOfficialPrompt 别的 agent 的 `<env>` 块（Claude Code
// 单空格形态）不满足上游判形，官方正文仍必须整段注入——判据是官方首句 marker，
// 不是「有没有 env 块」。
func TestForeignEnvironmentStillGetsOfficialPrompt(t *testing.T) {
	foreign := "Here is useful information:\n<env>\n Working directory: /home/u/proj\n Platform: linux\n</env>"
	obj := map[string]any{
		"model": "mimo-v2.6-flash-free",
		"messages": []any{
			map[string]any{"role": "system", "content": foreign},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	shapeChatRequest(obj)
	msgs, _ := obj["messages"].([]any)
	sys := systemText(msgs[0].(map[string]any)["content"])
	if !strings.HasPrefix(sys, officialPromptMarker) {
		t.Fatalf("外来 env 块导致官方正文未注入: …%q", promptHead(sys, 120))
	}
	if !environmentBlockRE.MatchString(sys) {
		t.Fatalf("官方 <env> 块缺失: …%q", tailOf(sys, 240))
	}
	if !strings.Contains(sys, foreign) {
		t.Fatalf("调用方的外来块被改写")
	}
	if n := strings.Count(sys, "<env>"); n != 2 {
		t.Fatalf("应为「外来块 + 官方块」两块，实际 %d", n)
	}
}

// TestOfficialPromptPrependedToFragmentSystem 分片 system 里官方正文作为**新的首个
// 文本分片**前置，调用方的分片与 cache_control 标记原样保留，且重复整形幂等。
func TestOfficialPromptPrependedToFragmentSystem(t *testing.T) {
	block := map[string]any{
		"type":          "text",
		"text":          "caller prompt",
		"cache_control": map[string]any{"type": "ephemeral"},
	}
	obj := map[string]any{
		"model": "mimo-v2.6-flash-free",
		"messages": []any{
			map[string]any{"role": "system", "content": []any{block}},
			map[string]any{"role": "user", "content": "hi"},
		},
	}
	shapeChatRequest(obj)
	msgs, _ := obj["messages"].([]any)
	first := msgs[0].(map[string]any)
	fragments, _ := first["content"].([]any)
	if len(fragments) < 2 {
		t.Fatalf("官方正文未作为独立分片前置: %+v", fragments)
	}
	head, _ := fragments[0].(map[string]any)
	headText, _ := head["text"].(string)
	if !strings.HasPrefix(headText, officialPromptMarker) {
		t.Fatalf("首个分片不是官方正文: %q", promptHead(headText, 80))
	}
	kept := map[string]any(nil)
	for _, raw := range fragments {
		if part, ok := raw.(map[string]any); ok && part["text"] == "caller prompt" {
			kept = part
		}
	}
	if kept == nil || kept["cache_control"] == nil {
		t.Fatalf("调用方的分片/缓存标记被改写: %+v", fragments)
	}
	// 重复整形幂等（分片路径也不能叠加）。
	before := canonicalJSON(obj)
	shapeChatRequest(obj)
	if after := canonicalJSON(obj); after != before {
		t.Fatal("分片 system 重复整形不幂等")
	}
}

// TestNewToolNameReaderRestoresClientNames 流式响应里的 clint_* 与官方工具名
// 都要还原成调用方声明的名字，且不得吞掉终止帧。
func TestNewToolNameReaderRestoresClientNames(t *testing.T) {
	renames := newToolRenameMap()
	renames.add("ReportFindings", "clint_0123456789ab")
	renames.add("Read", "clint_ba9876543210")

	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"clint_0123456789ab","arguments":"{}"}}]}}]}`,
		``,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"read","arguments":"{}"}}]}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")
	rc := newToolNameReader(io.NopCloser(strings.NewReader(stream)), renames)
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if !strings.Contains(text, `"name":"ReportFindings"`) {
		t.Errorf("clint_* 未还原成客户端名: %s", text)
	}
	if !strings.Contains(text, `"name":"Read"`) {
		t.Errorf("官方 read 未折叠回客户端声明的 Read: %s", text)
	}
	if strings.Contains(text, clientToolPrefix) {
		t.Errorf("线上名泄漏给客户端: %s", text)
	}
	if !strings.Contains(text, "[DONE]") {
		t.Errorf("终止帧被吞: %s", text)
	}
}

// TestRewriteToolNamesLineSkipsEscapedNames arguments 里被转义的 "name"
// （工具参数内容）不得被误改，只还原真实字段。
func TestRewriteToolNamesLineSkipsEscapedNames(t *testing.T) {
	renames := newToolRenameMap()
	renames.add("Edit", "clint_aaaaaaaaaaaa")
	line := `data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"clint_aaaaaaaaaaaa","arguments":"{\"name\":\"clint_aaaaaaaaaaaa\"}"}}]}}]}` + "\n"
	got := rewriteToolNamesLine(line, renames)
	if !strings.Contains(got, `"name":"Edit"`) {
		t.Fatalf("真实字段未还原: %s", got)
	}
	if !strings.Contains(got, `\"name\":\"clint_aaaaaaaaaaaa\"`) {
		t.Fatalf("arguments 里的转义 name 被误改: %s", got)
	}
}
