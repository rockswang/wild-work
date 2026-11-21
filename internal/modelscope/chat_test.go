package modelscope

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
	"wild-work/internal/provider"
)

// fakeUpstream 魔搭 API-Inference 的假上游：记录收到的请求，按需回 SSE 或普通 JSON。
type fakeUpstream struct {
	mu       sync.Mutex
	hits     int
	lastPath string
	lastAuth string
	lastBody []byte
	// streamMode 为假时回非流式 JSON，为真时回 SSE。
	streamMode bool
	// status 非 0 时直接回该状态码（模拟上游错误）。
	status int
	errMsg string
}

func (f *fakeUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.hits++
		f.lastPath = r.URL.Path
		f.lastAuth = r.Header.Get("Authorization")
		f.lastBody = body
		stream, status, msg := f.streamMode, f.status, f.errMsg
		f.mu.Unlock()

		if status >= 400 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, msg)
			return
		}
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"upstream-only-name",`+
				`"choices":[{"index":0,"message":{"role":"assistant","content":"hello"}}],`+
				`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		frames := []string{
			`data: {"id":"c1","object":"chat.completion.chunk","model":"upstream-only-name","choices":[{"index":0,"delta":{"content":"he"}}]}`,
			`data: {"id":"c1","object":"chat.completion.chunk","model":"upstream-only-name","choices":[{"index":0,"delta":{"content":"llo"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
			`data: [DONE]`,
		}
		for _, fr := range frames {
			_, _ = io.WriteString(w, fr+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
	}
}

func (f *fakeUpstream) snapshot() (int, string, string, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits, f.lastPath, f.lastAuth, append([]byte(nil), f.lastBody...)
}

func newTestClient(t *testing.T, f *fakeUpstream) *Client {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c := New(provider.ModelScope)
	c.Base = srv.URL
	return c
}

// TestChatStreamPassThroughAndModelEcho 对话主链路的三条契约：
//   - 请求体**原样透传**（不改造 messages/tools），落到 `{base}/v1/chat/completions`，
//     鉴权是 `Authorization: Bearer <ms-token>`；
//   - 流式走 StreamHTTP（**不设**总超时，R35），非流式走 HTTP；
//   - 输出里的 `model` 回填为**客户端请求的全串**（provider.go 的 Upstream 契约），
//     不是上游回的裸名；末帧 usage 被捕获（OpenAI 形状）。
func TestChatStreamPassThroughAndModelEcho(t *testing.T) {
	const clientModel = "modelscope/Qwen/Qwen3-235B-A22B-Instruct-2507"
	// 上游收到的 model 是剥掉渠道前缀后的裸名（handler 已改写）。
	upBody := []byte(`{"model":"Qwen/Qwen3-235B-A22B-Instruct-2507","stream":true,` +
		`"messages":[{"role":"user","content":"hi"}],"temperature":0.7}`)

	// 流式
	f := &fakeUpstream{streamMode: true}
	c := newTestClient(t, f)
	if c.HTTP.Timeout <= 0 {
		t.Error("非流式 client 必须带总超时（upstream.timeout_seconds 兜底）")
	}
	if c.StreamHTTP.Timeout != 0 {
		t.Errorf("StreamHTTP.Timeout = %v，必须为 0（R35：长回答会被总超时从中间掐断）", c.StreamHTTP.Timeout)
	}
	rc, status, respBody, err := c.ChatStream(&auth.Auth{AccessToken: "ms-test"}, upBody)
	if err != nil || rc == nil {
		t.Fatalf("ChatStream: rc=%v status=%d body=%s err=%v", rc, status, respBody, err)
	}
	defer rc.Close()
	present, path, authz, got := f.snapshot()
	if present != 1 {
		t.Errorf("上游命中数 = %d，期望 1", present)
	}
	if path != "/v1/chat/completions" {
		t.Errorf("路径 = %q，期望 /v1/chat/completions", path)
	}
	if authz != "Bearer ms-test" {
		t.Errorf("Authorization = %q，期望 Bearer ms-test", authz)
	}
	if string(got) != string(upBody) {
		t.Errorf("请求体被改造了：%s\n期望原样：%s", got, upBody)
	}

	rec := httptest.NewRecorder()
	usage, err := c.Stream(rec, rc, clientModel)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"model":"`+clientModel+`"`) {
		t.Errorf("流式输出未把 model 回填为客户端全串：%s", out)
	}
	if strings.Contains(out, "upstream-only-name") {
		t.Errorf("流式输出残留上游裸 model 名：%s", out)
	}
	if !strings.Contains(out, `"content":"he"`) {
		t.Errorf("流式输出丢失正文增量：%s", out)
	}
	if usage == nil {
		t.Fatal("末帧 usage 未捕获")
	}
	if usage["prompt_tokens"] != float64(10) || usage["completion_tokens"] != float64(5) {
		t.Errorf("捕获的 usage = %v，期望 pt=10 ct=5", usage)
	}

	// 非流式
	f2 := &fakeUpstream{}
	c2 := newTestClient(t, f2)
	nonStreamBody := []byte(`{"model":"Qwen/Qwen3-235B-A22B-Instruct-2507","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	rc2, status2, respBody2, err := c2.ChatStream(&auth.Auth{AccessToken: "ms-test"}, nonStreamBody)
	if err != nil || rc2 == nil || status2 != http.StatusOK {
		t.Fatalf("非流式 ChatStream: rc=%v status=%d body=%s err=%v", rc2, status2, respBody2, err)
	}
	defer rc2.Close()
	resp, err := c2.Aggregate(rc2, clientModel)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if got := resp["model"]; got != clientModel {
		t.Errorf("非流式 model = %v，期望客户端全串 %s", got, clientModel)
	}
	if _, _, _, body := f2.snapshot(); string(body) != string(nonStreamBody) {
		t.Errorf("非流式请求体被改造了：%s", body)
	}
}

// TestChatStreamRejectsImageModelLocally 生图模型不参与对话路由（D12）：
// 本地 400 + image_model_not_chat，**不打上游**（省一次必然失败的出站），
// 且分类必须是请求级 ErrBadParams（不罚号、不轮转）。
func TestChatStreamRejectsImageModelLocally(t *testing.T) {
	f := &fakeUpstream{}
	c := newTestClient(t, f)
	body := []byte(`{"model":"Qwen/Qwen-Image","messages":[{"role":"user","content":"画一只猫"}]}`)
	rc, status, respBody, err := c.ChatStream(&auth.Auth{AccessToken: "ms-test"}, body)
	if err != nil {
		t.Fatalf("本地拒绝不应以传输错误形式返回：%v", err)
	}
	if rc != nil {
		t.Error("本地拒绝不应返回响应体流")
	}
	if status != http.StatusBadRequest {
		t.Errorf("status = %d，期望 400（不得 404，R139：本地拒绝必须是调用方参数错误）", status)
	}
	if !strings.Contains(string(respBody), "image_model_not_chat") {
		t.Errorf("错误体缺少 image_model_not_chat：%s", respBody)
	}
	if hits, _, _, _ := f.snapshot(); hits != 0 {
		t.Errorf("本地拒绝却打了上游 %d 次", hits)
	}
	if kind := c.Classify(status, string(respBody)); kind != provider.ErrBadParams {
		t.Errorf("Classify = %s，期望 bad_params（请求级：不罚号不轮转）", kind)
	}
}

// TestChatStreamIdleWatchdog 流式 body 被空闲看门狗包住：拿到响应头后不再续命
// 就把「静默的读」掐断，而不是无限期挂住（R58）。这里把 idle 压到 50ms 验证。
func TestChatStreamIdleWatchdog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(400 * time.Millisecond) // 之后彻底静默
	}))
	defer srv.Close()
	c := New(provider.ModelScope)
	c.Base = srv.URL
	c.idle = 50 * time.Millisecond

	rc, _, _, err := c.ChatStream(&auth.Auth{AccessToken: "ms-test"}, []byte(`{"model":"m","stream":true}`))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer rc.Close()
	if _, err := rc.Read(make([]byte, 64)); err != nil {
		t.Fatalf("首帧应可读：%v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(rc)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("静默超时后读应返回错误，而不是 clean EOF")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("空闲看门狗未生效：读被无限期挂住")
	}
}

// TestChatStreamErrorPassthrough 上游 ≥400 原样透传（不变量 8）：状态码与响应体
// 都由 handler 交给 Classify，渠道不加工、不包装。
func TestChatStreamErrorPassthrough(t *testing.T) {
	f := &fakeUpstream{status: http.StatusUnauthorized, errMsg: `{"error":{"message":"invalid token","code":"invalid_token"}}`}
	c := newTestClient(t, f)
	rc, status, respBody, err := c.ChatStream(&auth.Auth{AccessToken: "ms-bad"}, []byte(`{"model":"Qwen/x"}`))
	if err != nil {
		t.Fatalf("上游 401 不应变成传输错误：%v", err)
	}
	if rc != nil {
		t.Error("错误响应不应返回 body 流")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d，期望 401", status)
	}
	var probe map[string]any
	if err := json.Unmarshal(respBody, &probe); err != nil {
		t.Errorf("上游错误体被包装/改造了：%s", respBody)
	}
}
