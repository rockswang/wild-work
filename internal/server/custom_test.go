package server

// custom_test.go 自定义模型直转回归：去掉 serveCustom 分流（chatCompletions 接入点）
// 即全红的用例集。全部走 httptest 假第三方，不碰真实网络。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/custommodels"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	statsEngine "wild-work/internal/stats"
)

// fakeThirdParty 假第三方 OpenAI 兼容源：记录每次请求的路径/鉴权/请求体，
// 按预设脚本返回状态码与响应体（脚本耗尽后重复最后一个）。
type fakeThirdParty struct {
	mu       sync.Mutex
	calls    int
	path     string
	authz    string
	body     []byte
	ct       string // 响应 Content-Type
	script   []int  // 依次返回的状态码（耗尽后重复末位）
	respBody string
}

func (f *fakeThirdParty) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls++
	f.path = r.URL.Path
	f.authz = r.Header.Get("Authorization")
	f.body, _ = io.ReadAll(r.Body)
	f.mu.Unlock()
	status := http.StatusOK
	if len(f.script) > 0 {
		idx := f.calls - 1
		if idx >= len(f.script) {
			idx = len(f.script) - 1
		}
		status = f.script[idx]
	}
	ct := f.ct
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, f.respBody)
}

func (f *fakeThirdParty) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeThirdParty) lastReq() (path, authz string, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.path, f.authz, f.body
}

// newCustomTestStore 建一个带单源单模型的 Store（源指向假第三方）。
func newCustomTestStore(t *testing.T, dir, srcName, baseURL, modelName, upstreamID string) *custommodels.Store {
	t.Helper()
	s, err := custommodels.Load(dir)
	if err != nil {
		t.Fatalf("custommodels.Load: %v", err)
	}
	if err := s.UpsertSource(custommodels.Source{Name: srcName, BaseURL: baseURL, APIKey: "dummy-key-123", Enabled: true}); err != nil {
		t.Fatalf("UpsertSource: %v", err)
	}
	if err := s.UpsertModel(custommodels.Model{Name: modelName, Source: srcName, UpstreamID: upstreamID, Enabled: true}); err != nil {
		t.Fatalf("UpsertModel: %v", err)
	}
	return s
}

// TestCustomModelBareNameProxied 裸名命中 → 直转：转发 URL 拼接、Bearer、
// upstream_id 改写、其余字段保真、响应原样透传。
func TestCustomModelBareNameProxied(t *testing.T) {
	tp := &fakeThirdParty{respBody: `{"id":"cmpl-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}]}`}
	srv := httptest.NewServer(tp)
	defer srv.Close()

	// 两种 base_url 习惯（不带 /v1 与带 /v1）都必须拼出 /v1/chat/completions
	for _, baseURL := range []string{srv.URL, srv.URL + "/v1"} {
		store := newCustomTestStore(t, t.TempDir(), "ds", baseURL, "ds-chat", "ds-real")
		h := NewHandler(Config{Custom: store})

		body := `{"model":"ds-chat","messages":[{"role":"user","content":"你好"}],"stream":false}`
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("base_url=%s: 状态码 = %d, want 200, body=%s", baseURL, rec.Code, rec.Body.String())
		}
		if got := rec.Body.String(); got != tp.respBody {
			t.Fatalf("响应应原样透传:\n got  %s\n want %s", got, tp.respBody)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("Content-Type 应透传, got %q", ct)
		}
		path, authz, reqBody := tp.lastReq()
		if path != "/v1/chat/completions" {
			t.Fatalf("base_url=%s: 转发路径 = %q, want /v1/chat/completions", baseURL, path)
		}
		if authz != "Bearer dummy-key-123" {
			t.Fatalf("Authorization = %q, want Bearer dummy-key-123", authz)
		}
		var sent map[string]any
		if err := json.Unmarshal(reqBody, &sent); err != nil {
			t.Fatalf("转发 body 应为 JSON: %v", err)
		}
		if sent["model"] != "ds-real" {
			t.Fatalf("model 字段应改写为 upstream_id: %v", sent["model"])
		}
		msgs, _ := sent["messages"].([]any)
		if len(msgs) != 1 {
			t.Fatalf("messages 应保真转发, got %v", sent["messages"])
		}
		if sent["stream"] != false {
			t.Fatalf("stream 字段应保真转发, got %v", sent["stream"])
		}
	}
}

// TestCustomModelSourcePrefixProxied 「源名/模型」名命中 → 直转：与渠道模型的
// 「渠道/模型」命名观感一致（前缀=所属源名）；upstream_id 改写照常生效。
func TestCustomModelSourcePrefixProxied(t *testing.T) {
	tp := &fakeThirdParty{respBody: `{"ok":true}`}
	srv := httptest.NewServer(tp)
	defer srv.Close()

	store := newCustomTestStore(t, t.TempDir(), "YD", srv.URL, "YD/GLM-5.3", "GLM-5.3")
	h := NewHandler(Config{Custom: store})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"YD/GLM-5.3","messages":[],"stream":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("「源名/模型」名应直转 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	path, _, reqBody := tp.lastReq()
	if path == "" {
		t.Fatal("第三方未被调用")
	}
	var sent map[string]any
	if err := json.Unmarshal(reqBody, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["model"] != "GLM-5.3" {
		t.Fatalf("upstream_id 应改写为 GLM-5.3，实际 %v", sent["model"])
	}
	if n := tp.count(); n != 1 {
		t.Fatalf("应恰好 1 次上游调用，实际 %d", n)
	}
}

// TestChannelPrefixStillWinsWithPrefixEntries 自定义表存在「源名/模型」条目时，
// 渠道前缀请求仍走渠道——入库校验保证渠道 Kind 不可能成为自定义前缀。
func TestChannelPrefixStillWinsWithPrefixEntries(t *testing.T) {
	up := &rotatingUpstream{}
	p := pool.New(t.TempDir() + "/state.json")
	p.Add(&auth.Auth{Kind: "workbuddy", AccessToken: "at", ExpiresAt: 4102444800, UID: "wb-1"})

	tp := &fakeThirdParty{respBody: `{}`}
	srv := httptest.NewServer(tp)
	defer srv.Close()

	store := newCustomTestStore(t, t.TempDir(), "YD", srv.URL, "YD/m", "")
	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: p, Upstream: up, StaticModels: []provider.ModelInfo{{ID: "m"}}},
		},
		Custom: store,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"workbuddy/m","messages":[],"stream":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("渠道请求应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if n := tp.count(); n != 0 {
		t.Fatalf("渠道前缀请求不得被自定义模型劫持，第三方被调 %d 次", n)
	}
}

// TestChannelPrefixNotHijackedByCustom 渠道前缀永远走渠道：自定义模型与渠道模型
// 同名（裸名 "m" vs "workbuddy/m"），前缀请求必须进渠道、不碰第三方。
func TestChannelPrefixNotHijackedByCustom(t *testing.T) {
	up := &rotatingUpstream{}
	p := pool.New(t.TempDir() + "/state.json")
	p.Add(&auth.Auth{Kind: "workbuddy", AccessToken: "at", ExpiresAt: 4102444800, UID: "wb-1"})

	tp := &fakeThirdParty{respBody: `{}`}
	srv := httptest.NewServer(tp)
	defer srv.Close()

	store := newCustomTestStore(t, t.TempDir(), "ds", srv.URL, "m", "")
	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: p, Upstream: up, StaticModels: []provider.ModelInfo{{ID: "m"}}},
		},
		Custom: store,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"workbuddy/m","messages":[],"stream":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("渠道请求应成功，实际 %d: %s", rec.Code, rec.Body.String())
	}
	up.mu.Lock()
	calls := append([]string{}, up.calls...)
	up.mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("渠道上游未被调用（前缀请求必须走渠道）")
	}
	if tp.count() != 0 {
		t.Fatalf("带渠道前缀的请求不得被自定义模型劫持，第三方被调 %d 次", tp.count())
	}
}

// TestBareNameMissKeepsOriginalPath 回归：裸名未命中自定义模型 → 照旧走原有
// 渠道路径（现状 = 报「需要显式前缀」的 400），零行为变化。
func TestBareNameMissKeepsOriginalPath(t *testing.T) {
	tp := &fakeThirdParty{respBody: `{}`}
	srv := httptest.NewServer(tp)
	defer srv.Close()

	store := newCustomTestStore(t, t.TempDir(), "ds", srv.URL, "m", "")
	h := NewHandler(Config{Custom: store})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"totally-unknown","messages":[],"stream":false}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未命中裸名应保持原有 400，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "explicit prefix") {
		t.Fatalf("错误文案应与改动前一致（explicit prefix），实际 %s", rec.Body.String())
	}
	if tp.count() != 0 {
		t.Fatalf("未命中不得转发第三方，被调 %d 次", tp.count())
	}
}

// TestCustomStreamPassthrough 流式直转：SSE 响应逐字节透传（io.Copy 直通）。
func TestCustomStreamPassthrough(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n" +
		"data: [DONE]\n\n"
	tp := &fakeThirdParty{ct: "text/event-stream", respBody: sse}
	srv := httptest.NewServer(tp)
	defer srv.Close()

	store := newCustomTestStore(t, t.TempDir(), "ds", srv.URL, "ds-chat", "")
	h := NewHandler(Config{Custom: store})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ds-chat","messages":[],"stream":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("流式直转状态码 = %d, want 200", rec.Code)
	}
	if rec.Body.String() != sse {
		t.Fatalf("SSE 应逐字节透传:\n got  %q\n want %q", rec.Body.String(), sse)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type 应透传 text/event-stream, got %q", ct)
	}
}

// TestCustomRetryOn5xxExactlyTwice 5xx 瞬时错误：恰好重试一次 → 第二次成功。
func TestCustomRetryOn5xxExactlyTwice(t *testing.T) {
	tp := &fakeThirdParty{script: []int{http.StatusInternalServerError, http.StatusOK}, respBody: `{"ok":true}`}
	srv := httptest.NewServer(tp)
	defer srv.Close()

	store := newCustomTestStore(t, t.TempDir(), "ds", srv.URL, "ds-chat", "")
	h := NewHandler(Config{Custom: store})

	old := customRetryDelay
	customRetryDelay = 5 * time.Millisecond
	defer func() { customRetryDelay = old }()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ds-chat","messages":[],"stream":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("重试后应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if n := tp.count(); n != 2 {
		t.Fatalf("5xx 应恰好触发 2 次上游调用（首错 + 重试一次），实际 %d", n)
	}
}

// TestCustomNoRetryOn4xx 4xx 属请求级终态：不重试、原文透传。
func TestCustomNoRetryOn4xx(t *testing.T) {
	tp := &fakeThirdParty{script: []int{http.StatusUnauthorized}, respBody: `{"error":{"message":"bad key"}}`}
	srv := httptest.NewServer(tp)
	defer srv.Close()

	store := newCustomTestStore(t, t.TempDir(), "ds", srv.URL, "ds-chat", "")
	h := NewHandler(Config{Custom: store})

	old := customRetryDelay
	customRetryDelay = 5 * time.Millisecond
	defer func() { customRetryDelay = old }()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ds-chat","messages":[],"stream":false}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("4xx 应原样透传，实际 %d", rec.Code)
	}
	if rec.Body.String() != tp.respBody {
		t.Fatalf("4xx 错误体应原样透传, got %s", rec.Body.String())
	}
	if n := tp.count(); n != 1 {
		t.Fatalf("4xx 不得重试，上游调用 %d 次", n)
	}
}

// TestCustomUnreachableSource502 传输层不可达（重试一次后仍失败）→ 502 upstream_error。
func TestCustomUnreachableSource502(t *testing.T) {
	store := newCustomTestStore(t, t.TempDir(), "ds", "http://127.0.0.1:1", "ds-chat", "")
	h := NewHandler(Config{Custom: store})

	old := customRetryDelay
	customRetryDelay = 5 * time.Millisecond
	defer func() { customRetryDelay = old }()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ds-chat","messages":[],"stream":false}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("不可达源应 502，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upstream_error") {
		t.Fatalf("502 错误码应为 upstream_error, got %s", rec.Body.String())
	}
}

// TestCustomStatRowRecorded 直转路径保留既有 statRow 插桩：模型名记自定义模型名。
func TestCustomStatRowRecorded(t *testing.T) {
	tp := &fakeThirdParty{respBody: `{"choices":[]}`}
	srv := httptest.NewServer(tp)
	defer srv.Close()

	st, err := statsEngine.New(t.TempDir(), t.Logf)
	if err != nil {
		t.Fatalf("stats.New: %v", err)
	}
	store := newCustomTestStore(t, t.TempDir(), "ds", srv.URL, "ds-chat", "ds-real")
	h := NewHandler(Config{Custom: store, Stats: st})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ds-chat","messages":[],"stream":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("直转应 200，实际 %d", rec.Code)
	}
	rows := st.Logs(5)
	if len(rows) == 0 {
		t.Fatal("直转请求应记一行请求日志")
	}
	row := rows[0]
	if row.Model != "ds-chat" {
		t.Fatalf("统计行模型名应为自定义模型名 ds-chat，实际 %q", row.Model)
	}
	if row.Status != http.StatusOK {
		t.Fatalf("统计行状态应为 200，实际 %d", row.Status)
	}
	if row.Mode != "非流" {
		t.Fatalf("统计行模式应为 非流，实际 %q", row.Mode)
	}
}

// TestModelsMergeCustomEntries /v1/models 合并：启用中的自定义模型以裸名列出，
// 停用模型/停用源的模型不列；渠道模型照旧。
func TestModelsMergeCustomEntries(t *testing.T) {
	up := &rotatingUpstream{}
	p := pool.New(t.TempDir() + "/state.json")
	p.Add(&auth.Auth{Kind: "workbuddy", AccessToken: "at", ExpiresAt: 4102444800, UID: "wb-1"})

	store, err := custommodels.Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := store.UpsertSource(custommodels.Source{Name: "ds", BaseURL: "https://example.com", APIKey: "k", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSource(custommodels.Source{Name: "off-src", BaseURL: "https://example.org", APIKey: "k", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	for _, m := range []custommodels.Model{
		{Name: "ds-chat", Source: "ds", Enabled: true},
		{Name: "ds-off", Source: "ds", Enabled: false},
		{Name: "src-off", Source: "off-src", Enabled: true},
	} {
		if err := store.UpsertModel(m); err != nil {
			t.Fatal(err)
		}
	}

	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: p, Upstream: up, StaticModels: []provider.ModelInfo{{ID: "m"}}},
		},
		Custom: store,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d", rec.Code)
	}
	var doc struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("解析 models 响应: %v", err)
	}
	ids := map[string]any{}
	for _, e := range doc.Data {
		ids[e["id"].(string)] = e
	}
	if _, ok := ids["workbuddy/m"]; !ok {
		t.Fatal("渠道模型应照旧列出（workbuddy/m）")
	}
	entry, ok := ids["ds-chat"]
	if !ok {
		t.Fatal("启用中的自定义模型应合并进 /v1/models")
	}
	if entry.(map[string]any)["owned_by"] != "custom" {
		t.Fatalf("自定义条目 owned_by 应为 custom, got %v", entry.(map[string]any)["owned_by"])
	}
	if _, ok := ids["ds-off"]; ok {
		t.Fatal("停用模型不得列出")
	}
	if _, ok := ids["src-off"]; ok {
		t.Fatal("停用源下的模型不得列出")
	}
}

// findModelStat 在 Snapshot 的模型消耗表里找指定模型行。
func findModelStat(t *testing.T, st *statsEngine.Stats, model string) *statsEngine.ModelStat {
	t.Helper()
	models := st.Snapshot(time.Now()).Models
	for i := range models {
		if models[i].Model == model {
			return &models[i]
		}
	}
	return nil
}

// TestCustomNonStreamUsageRecorded 非流式：响应带 usage → 进模型消耗表
// （tokens = prompt+completion），请求日志行记 completion tokens；credit 恒 0。
func TestCustomNonStreamUsageRecorded(t *testing.T) {
	tp := &fakeThirdParty{respBody: `{"id":"cmpl-1","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":16}}`}
	srv := httptest.NewServer(tp)
	defer srv.Close()

	st, err := statsEngine.New(t.TempDir(), t.Logf)
	if err != nil {
		t.Fatalf("stats.New: %v", err)
	}
	store := newCustomTestStore(t, t.TempDir(), "ds", srv.URL, "ds-chat", "")
	h := NewHandler(Config{Custom: store, Stats: st})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ds-chat","messages":[],"stream":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("直转应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != tp.respBody {
		t.Fatalf("usage 解析不得改动响应透传:\n got  %s\n want %s", rec.Body.String(), tp.respBody)
	}
	hit := findModelStat(t, st, "ds-chat")
	if hit == nil {
		t.Fatal("usage 应计入模型消耗表（ds-chat）")
	}
	if hit.Reqs != 1 || hit.Tokens != 21 {
		t.Fatalf("模型消耗应为 1 次 / 21 tokens，实际 %d / %d", hit.Reqs, hit.Tokens)
	}
	if hit.Credit != 0 {
		t.Fatalf("自定义模型 credit 应恒 0，实际 %v", hit.Credit)
	}
	rows := st.Logs(5)
	if len(rows) == 0 || rows[0].Tok != 16 {
		t.Fatalf("请求日志行应记 completion tokens=16，实际 %+v", rows)
	}
}

// TestCustomStreamUsageRecorded 流式：SSE 尾帧带 usage → 同样进模型消耗表。
func TestCustomStreamUsageRecorded(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"
	tp := &fakeThirdParty{ct: "text/event-stream", respBody: sse}
	srv := httptest.NewServer(tp)
	defer srv.Close()

	st, err := statsEngine.New(t.TempDir(), t.Logf)
	if err != nil {
		t.Fatalf("stats.New: %v", err)
	}
	store := newCustomTestStore(t, t.TempDir(), "ds", srv.URL, "ds-chat", "")
	h := NewHandler(Config{Custom: store, Stats: st})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ds-chat","messages":[],"stream":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("流式直转应 200，实际 %d", rec.Code)
	}
	if rec.Body.String() != sse {
		t.Fatalf("SSE 应逐字节透传:\n got  %q\n want %q", rec.Body.String(), sse)
	}
	hit := findModelStat(t, st, "ds-chat")
	if hit == nil {
		t.Fatal("流式尾帧 usage 应计入模型消耗表（ds-chat）")
	}
	if hit.Reqs != 1 || hit.Tokens != 10 {
		t.Fatalf("模型消耗应为 1 次 / 10 tokens，实际 %d / %d", hit.Reqs, hit.Tokens)
	}
	rows := st.Logs(5)
	if len(rows) == 0 || rows[0].Tok != 7 {
		t.Fatalf("请求日志行应记 completion tokens=7，实际 %+v", rows)
	}
}

// TestCustomUsageAnthropicAliases 第三方回 Anthropic 字段名（input_tokens/
// output_tokens）也能记账——与渠道路径 usageTokens 的别名兼容口径一致。
func TestCustomUsageAnthropicAliases(t *testing.T) {
	tp := &fakeThirdParty{respBody: `{"choices":[],"usage":{"input_tokens":4,"output_tokens":6}}`}
	srv := httptest.NewServer(tp)
	defer srv.Close()

	st, err := statsEngine.New(t.TempDir(), t.Logf)
	if err != nil {
		t.Fatalf("stats.New: %v", err)
	}
	store := newCustomTestStore(t, t.TempDir(), "ds", srv.URL, "ds-chat", "")
	h := NewHandler(Config{Custom: store, Stats: st})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ds-chat","messages":[],"stream":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("直转应 200，实际 %d", rec.Code)
	}
	hit := findModelStat(t, st, "ds-chat")
	if hit == nil || hit.Tokens != 10 {
		t.Fatalf("Anthropic 字段名 usage 应记 10 tokens，实际 %+v", hit)
	}
}

// TestCustomNoUsageNotCounted 无 usage 响应：只计请求数（请求日志），
// 不进模型消耗表——与渠道路径「usage 缺失记 0 token 仅计请求数」口径一致。
func TestCustomNoUsageNotCounted(t *testing.T) {
	tp := &fakeThirdParty{respBody: `{"choices":[]}`}
	srv := httptest.NewServer(tp)
	defer srv.Close()

	st, err := statsEngine.New(t.TempDir(), t.Logf)
	if err != nil {
		t.Fatalf("stats.New: %v", err)
	}
	store := newCustomTestStore(t, t.TempDir(), "ds", srv.URL, "ds-chat", "")
	h := NewHandler(Config{Custom: store, Stats: st})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ds-chat","messages":[],"stream":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("直转应 200，实际 %d", rec.Code)
	}
	if hit := findModelStat(t, st, "ds-chat"); hit != nil {
		t.Fatalf("无 usage 不得进模型消耗表，实际 %+v", hit)
	}
	if rows := st.Logs(5); len(rows) == 0 {
		t.Fatal("无 usage 也应记请求日志行（只计请求数）")
	}
}

// TestCustomOversizeBodyPassthrough 非流式响应超体积护栏：降级纯透传，
// 响应逐字节完整下发，不做 usage 解析（不记账不报错）。
func TestCustomOversizeBodyPassthrough(t *testing.T) {
	respBody := `{"choices":[{"message":{"content":"这段响应体比注入后的护栏大，必须完整透传且不做 usage 解析"}}],"usage":{"prompt_tokens":5,"completion_tokens":16}}`
	tp := &fakeThirdParty{respBody: respBody}
	srv := httptest.NewServer(tp)
	defer srv.Close()

	st, err := statsEngine.New(t.TempDir(), t.Logf)
	if err != nil {
		t.Fatalf("stats.New: %v", err)
	}
	store := newCustomTestStore(t, t.TempDir(), "ds", srv.URL, "ds-chat", "")
	h := NewHandler(Config{Custom: store, Stats: st})

	old := customUsageBodyCap
	customUsageBodyCap = 8 // 8 字节：任何真实响应都超限
	defer func() { customUsageBodyCap = old }()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"ds-chat","messages":[],"stream":false}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("直转应 200，实际 %d", rec.Code)
	}
	if rec.Body.String() != respBody {
		t.Fatalf("超限响应必须逐字节透传:\n got  %s\n want %s", rec.Body.String(), respBody)
	}
	if hit := findModelStat(t, st, "ds-chat"); hit != nil {
		t.Fatalf("超限降级不得解析 usage，实际 %+v", hit)
	}
}

// TestSSEUsageScanFragments 扫描器单元：任意 Read 切分（字节级碎片喂入）
// 都能切行捕获 usage；后帧覆盖前帧；超限行弃扫。
func TestSSEUsageScanFragments(t *testing.T) {
	s := &sseUsageScan{}
	src := "event: message\n" +
		"data: {\"choices\":[{\"delta\":{}}]}\n" +
		"data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":9}}\n" +
		"data: [DONE]\n\n"
	for i := 0; i < len(src); i++ {
		s.feed([]byte{src[i]})
	}
	if s.usage == nil {
		t.Fatal("碎片喂入应捕获 usage 帧")
	}
	if pt, ct, _ := usageTokens(s.usage); pt != 2 || ct != 9 {
		t.Fatalf("usage 应为 2/9，实际 %d/%d", pt, ct)
	}
	// 后帧覆盖前帧（OpenAI include_usage 即尾帧语义）
	s.feed([]byte("data: {\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":5}}\n\n"))
	if pt, ct, _ := usageTokens(s.usage); pt != 1 || ct != 5 {
		t.Fatalf("后帧 usage 应覆盖前帧，实际 %d/%d", pt, ct)
	}

	// 超限行弃扫：转发不受影响，只是放弃记账
	s2 := &sseUsageScan{}
	big := make([]byte, customUsageScanLineCap+10)
	for i := range big {
		big[i] = 'x'
	}
	copy(big, []byte("data: "))
	s2.feed(big)
	if !s2.drop || s2.usage != nil {
		t.Fatalf("超限行应弃扫: drop=%v usage=%v", s2.drop, s2.usage)
	}
}
