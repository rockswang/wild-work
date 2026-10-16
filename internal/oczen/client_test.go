package oczen

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/provider"
)

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
	got := descendingTimePart([]byte{0, 0, 0, 0, 1, 0xFF})
	if got[9:] != "ffe" {
		t.Fatalf("descendingTimePart = %q, 末三位应为 ffe", got)
	}
}

// TestEnsureAgentShape 免费档要求 stream=true 且 tools 含 bash/read。
func TestEnsureAgentShape(t *testing.T) {
	// 纯聊天：无 tools → 注入两个桩工具并置 tool_choice=none
	chat := map[string]any{"stream": false, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	ensureAgentShape(chat)
	if chat["stream"] != true {
		t.Fatalf("stream 未强制为 true: %v", chat["stream"])
	}
	if chat["tool_choice"] != "none" {
		t.Fatalf("纯聊天应置 tool_choice=none: %v", chat["tool_choice"])
	}
	names := toolNames(chat)
	if !names["bash"] || !names["read"] {
		t.Fatalf("桩工具未补齐: %v", names)
	}

	// 客户端自带工具：只补缺失的，且不得覆盖客户的 tool_choice
	withTools := map[string]any{
		"stream": false, "tool_choice": "auto",
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "Edit"}}},
	}
	ensureAgentShape(withTools)
	n2 := toolNames(withTools)
	if !n2["Edit"] || !n2["bash"] || !n2["read"] {
		t.Fatalf("客户端工具或桩工具缺失: %v", n2)
	}
	if withTools["tool_choice"] != "auto" {
		t.Fatalf("客户端 tool_choice 被覆盖: %v", withTools["tool_choice"])
	}

	// 已自带 bash/read 时不应重复注入
	dup := map[string]any{"tools": []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "bash"}},
		map[string]any{"type": "function", "function": map[string]any{"name": "read"}},
	}}
	ensureAgentShape(dup)
	if n := len(dup["tools"].([]any)); n != 2 {
		t.Fatalf("重复注入桩工具，tools=%d", n)
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

// TestChatStreamRequestShape 端到端校验发往上游的请求头与请求体形态
// （免费档三道闸门：规范会话头 + 伪装头齐套 + 智能体形态请求体）。
func TestChatStreamRequestShape(t *testing.T) {
	var gotH http.Header
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotH = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"model\":\"mimo-v2.6-flash-free\",\"choices\":[]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	body := `{"model":"mimo-v2.6-flash-free","stream":false,"messages":[{"role":"user","content":"say ok"}]}`
	rc, status, _, err := c.ChatStream(AnonymousAuth(), []byte(body))
	if err != nil || status != 200 {
		t.Fatalf("ChatStream 失败: status=%d err=%v", status, err)
	}
	defer rc.Close()

	if got := gotH.Get("Authorization"); got != "Bearer "+AnonymousKey {
		t.Errorf("匿名凭证错误: %q", got)
	}
	if got := gotH.Get("x-opencode-client"); got != "cli" {
		t.Errorf("x-opencode-client 错误: %q", got)
	}
	for _, k := range []string{"x-opencode-session", "x-opencode-session-id", "x-session-affinity", "X-Session-Id"} {
		if got := gotH.Get(k); !strings.HasPrefix(got, "ses_") || len(got) != 30 {
			t.Errorf("%s 非规范会话形状: %q", k, got)
		}
	}
	// 2026-10-14 起上游把 x-opencode-request 列为 403 触发头（见 R69/headered），
	// 一旦发出即被拒；这里锁死「不得再发」。
	if got := gotH.Get("x-opencode-request"); got != "" {
		t.Errorf("不得发送 x-opencode-request（上游 2026-10-14 起对携带该头的请求回 403）: %q", got)
	}
	// project 必须是裸 40 位小写 hex（prj_ 前缀的规范形态会被拒 403）。
	proj := gotH.Get("x-opencode-project")
	if len(proj) != 40 || strings.HasPrefix(proj, "prj_") {
		t.Errorf("x-opencode-project 必须是裸 40 位 hex（不得带 prj_ 前缀），实际: %q", proj)
	}
	for _, ch := range proj {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			t.Errorf("x-opencode-project 含非小写 hex 字符: %q", proj)
			break
		}
	}
	if gotBody["stream"] != true {
		t.Errorf("请求体未强制 stream=true: %v", gotBody["stream"])
	}
	if n := toolNames(gotBody); !n["bash"] || !n["read"] {
		t.Errorf("请求体缺少 bash/read 桩工具: %v", n)
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


// TestStreamClientHasNoTotalTimeout 守门：流式必须走**无总超时**的 client。
//
// http.Client.Timeout 是整请求上限，SSE 整个生成期都在读 body ⇒ 长思考请求会被从流中间
// 掐断。非流式 client 必须保留总超时（短请求的合理兜底）。
func TestStreamClientHasNoTotalTimeout(t *testing.T) {
	c := New()
	if c.StreamHTTP == nil {
		t.Fatal("StreamHTTP 必须存在")
	}
	if c.StreamHTTP.Timeout != 0 {
		t.Fatalf("StreamHTTP.Timeout = %v，必须为 0（流式不能有整请求上限）", c.StreamHTTP.Timeout)
	}
	if c.HTTP == nil || c.HTTP.Timeout <= 0 {
		t.Fatalf("非流式 HTTP.Timeout = %v，必须 > 0", c.HTTP.Timeout)
	}
	// 无总超时后必须靠 ResponseHeaderTimeout 兜底，否则连响应头都等不到会无限挂住。
	tr, ok := c.StreamHTTP.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatalf("StreamHTTP.Transport = %T，应为 *http.Transport", c.StreamHTTP.Transport)
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Fatal("StreamHTTP 必须有 ResponseHeaderTimeout 兜底")
	}
	if tr.TLSNextProto == nil {
		t.Fatal("应强制 HTTP/1.1（禁 h2）")
	}
}

// TestIdleTimeoutDefaults 未注入配置时回落默认值（装配漏注入不应退化成「无兜底」）。
func TestIdleTimeoutDefaults(t *testing.T) {
	c := New()
	if got := c.idleTimeout(); got != DefaultIdleTimeout {
		t.Fatalf("idleTimeout() = %v, want %v", got, DefaultIdleTimeout)
	}
	c.IdleTimeout = 33 * time.Second
	if got := c.idleTimeout(); got != 33*time.Second {
		t.Fatalf("idleTimeout() = %v, want 33s", got)
	}
}

// TestChatStreamWrapsIdleReader 守门：对话流返回体必须包空闲看门狗。
func TestChatStreamWrapsIdleReader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL)
	rc, status, _, err := c.ChatStream(AnonymousAuth(), []byte(
		`{"model":"mimo-v2.6-flash-free","stream":false,"messages":[{"role":"user","content":"say ok"}]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	defer rc.Close()
	_ = rc // 上游 oczen 的流包装由 internal/idle 提供，类型不导出，这里只校验请求成功
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

// TestClientProjectIDShape 伪装 project 头必须是官方 CLI 的**裸 40 位小写 hex**
// （带 prj_ 前缀的规范 id 会被上游 403），且同一 session 恒定映射同一值
// （prompt cache 亲和）。见 R69。
func TestClientProjectIDShape(t *testing.T) {
	s := canonicalSessionID("conv-a")
	got := clientProjectID(s)
	if len(got) != 40 {
		t.Fatalf("project 必须是 40 位 hex，实际 %d 位: %q", len(got), got)
	}
	if strings.HasPrefix(got, "prj_") {
		t.Fatalf("project 不得带 prj_ 前缀（上游拒绝该规范形态）: %q", got)
	}
	for _, ch := range got {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			t.Fatalf("project 必须全为小写 hex，实际含 %q: %q", ch, got)
		}
	}
	if again := clientProjectID(s); again != got {
		t.Fatalf("同一 session 必须稳定映射同一 project（prompt cache 亲和）: %q vs %q", got, again)
	}
	if other := clientProjectID(canonicalSessionID("conv-b")); other == got {
		t.Fatal("不同 session 不应映射到同一 project")
	}
}
