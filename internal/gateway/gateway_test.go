// pipe_test.go 兼容层骨架测试：in-process 调用、状态握手、流式语义、并发隔离。
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedHandler 假内层 handler：按请求体的 "x-status"/"x-shape" 脚本化响应。
// 通过请求体额外字段驱动，便于在同一测试中覆盖 200/400/流式三种路径。
type scriptedHandler struct {
	mu    sync.Mutex
	calls []string // 记录收到的 Authorization 头与模型名，用于断言透传正确
}

func (h *scriptedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(raw, &req)

	h.mu.Lock()
	h.calls = append(h.calls, fmt.Sprintf("%s|%s|%v", r.URL.Path, r.Header.Get("Authorization"), req["model"]))
	h.mu.Unlock()

	if code, ok := asInt(req["x-status"]); ok && code != 200 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream boom","type":"api_error","code":"boom"}}`))
		return
	}
	if asBool(req["stream"]) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		for _, s := range []string{"Hello", " world"} {
			chunk := fmt.Sprintf(`data: {"id":"c1","object":"chat.completion.chunk","model":"up","choices":[{"index":0,"delta":{"content":%q}}]}`+"\n\n", s)
			_, _ = w.Write([]byte(chunk))
			if f != nil {
				f.Flush()
			}
		}
		_, _ = w.Write([]byte(`data: {"id":"c1","object":"chat.completion.chunk","model":"up","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f != nil {
			f.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"chatcmpl-abc","object":"chat.completion","model":"up",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"Hello world"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
}

// testRouter 测试用路由规则：默认 workbuddy，claude-* 走 qoder。
func testRouter() Router {
	return Router{
		Default:  "workbuddy",
		Map:      map[string]string{"claude-sonnet-*": "workbuddy/glm-5.2", "claude-*": "qoder/gmodel"},
		Channels: []string{"workbuddy", "qoder"},
	}
}

// newTestGateway 构造兼容层 + 外层 mux（与原 main 装配方式一致）。
func newTestGateway(inner http.Handler) (*Gateway, *http.ServeMux) {
	g := New(Config{Inner: inner, APIKey: "secret", Router: testRouter()})
	mux := http.NewServeMux()
	g.Routes(mux)
	mux.Handle("/", inner)
	return g, mux
}

// post 发一个已带鉴权的请求（同时带两种头，覆盖 Anthropic 与 OpenAI 客户端）。
func post(mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("x-api-key", "secret")
	req.Header.Set("Authorization", "Bearer secret")
	mux.ServeHTTP(rec, req)
	return rec
}

// TestPipeStatusHandshake 非流式：状态码与 body 正确透传。
func TestPipeStatusHandshake(t *testing.T) {
	inner := &scriptedHandler{}
	g := New(Config{Inner: inner, APIKey: "secret", Router: Router{Default: "workbuddy", Channels: []string{"workbuddy"}}})
	res, err := g.call(t.Context(), "/v1/chat/completions",
		[]byte(`{"model":"workbuddy/x","stream":false}`), "secret")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	defer res.Close()
	if res.Status != 200 {
		t.Fatalf("status = %d, want 200", res.Status)
	}
	raw, err := res.ReadAll()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), "Hello world") {
		t.Errorf("body = %s", raw)
	}
}

// TestPipeErrorStatus 内层先 WriteHeader(400) 再写错误体时，握手仍能拿到 400。
func TestPipeErrorStatus(t *testing.T) {
	g := New(Config{Inner: &scriptedHandler{}, Router: Router{Default: "workbuddy"}})
	res, err := g.call(t.Context(), "/v1/chat/completions",
		[]byte(`{"model":"workbuddy/x","x-status":429}`), "")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Status != 429 {
		t.Fatalf("status = %d, want 429", res.Status)
	}
	raw, _ := res.ReadAll()
	if !strings.Contains(string(raw), "upstream boom") {
		t.Errorf("body = %s", raw)
	}
}

// TestPipeFlushStreaming 流式：多次 Write 必须逐次可读（不被缓冲成一次）。
func TestPipeFlushStreaming(t *testing.T) {
	g := New(Config{Inner: &scriptedHandler{}, Router: Router{Default: "workbuddy"}})
	res, err := g.call(t.Context(), "/v1/chat/completions", []byte(`{"model":"workbuddy/x","stream":true}`), "")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	defer res.Close()
	var events int
	if err := iterateChatSSE(res.Body, func(c chatChunk) error {
		if c.Content != "" {
			events++
		}
		return nil
	}); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if events != 2 {
		t.Errorf("text chunk 数 = %d, want 2（说明流被缓冲合并了）", events)
	}
}

// TestPipeClientDisconnectCancels 调用方取消后，内层 handler 的写端应拿到错误并退出（不泄漏 goroutine）。
func TestPipeClientDisconnectCancels(t *testing.T) {
	released := make(chan struct{})
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: start\n\n"))
		<-r.Context().Done() // 模拟长流：调用方取消后必须能感知
		close(released)
	})
	g := New(Config{Inner: inner, Router: Router{Default: "workbuddy"}})

	ctx, cancel := context.WithCancel(context.Background())
	res, err := g.call(ctx, "/v1/chat/completions", []byte(`{}`), "")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	cancel() // 模拟客户端断连
	_ = res.Body.Close()

	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("内层 handler 未随调用方取消而退出（疑似 goroutine 泄漏）")
	}
}

// TestResponsesNonStream 验证 Responses 非流式形状：output 数组 + output_text。
func TestResponsesNonStream(t *testing.T) {
	_, mux := newTestGateway(&scriptedHandler{})
	rec := post(mux, "/v1/responses", `{"model":"gpt-5","input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Errorf("object/status = %v/%v", resp["object"], resp["status"])
	}
	if resp["model"] != "gpt-5" { // 必须回填客户端模型名
		t.Errorf("model = %v, want gpt-5", resp["model"])
	}
	out, _ := resp["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output len = %d, want 1", len(out))
	}
	item, _ := out[0].(map[string]any)
	if item["type"] != "message" {
		t.Errorf("item.type = %v", item["type"])
	}
	content, _ := item["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "Hello world" {
		t.Errorf("content part = %v", part)
	}
}

// TestResponsesToolCallIsSeparateItem 工具调用必须是独立 function_call item（而非塞进 message.content）。
func TestResponsesToolCallIsSeparateItem(t *testing.T) {
	inner := chatFixtureHandler(`{"id":"c1","object":"chat.completion","model":"up","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":9,"total_tokens":14}}`)
	_, mux := newTestGateway(inner)
	rec := post(mux, "/v1/responses", `{"model":"gpt-5","input":"hi"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	out, _ := resp["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output len = %d（空文本 message item 不应输出）, want 1", len(out))
	}
	item, _ := out[0].(map[string]any)
	if item["type"] != "function_call" {
		t.Fatalf("item.type = %v, want function_call", item["type"])
	}
	if item["call_id"] != "call_1" || item["name"] != "read_file" {
		t.Errorf("call_id/name = %v/%v", item["call_id"], item["name"])
	}
	if item["arguments"] != `{"path":"a.txt"}` {
		t.Errorf("arguments = %v", item["arguments"])
	}
}

// TestResponsesStreamEventSequence 流式事件序列必须按官方顺序出现。
func TestResponsesStreamEventSequence(t *testing.T) {
	_, mux := newTestGateway(&scriptedHandler{})
	rec := post(mux, "/v1/responses", `{"model":"gpt-5","input":"hi","stream":true}`)
	body := rec.Body.String()
	wantOrder := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	assertEventOrder(t, body, wantOrder)
	if !strings.Contains(body, "data: [DONE]") {
		t.Error("缺少 data: [DONE]")
	}
}

// TestAnthropicNonStream Anthropic 非流式形状。
func TestAnthropicNonStream(t *testing.T) {
	_, mux := newTestGateway(&scriptedHandler{})
	rec := post(mux, "/v1/messages", `{"model":"claude-sonnet-4-5","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["type"] != "message" || resp["role"] != "assistant" {
		t.Errorf("type/role = %v/%v", resp["type"], resp["role"])
	}
	if resp["model"] != "claude-sonnet-4-5" {
		t.Errorf("model = %v, want 客户端请求名", resp["model"])
	}
	if resp["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v", resp["stop_reason"])
	}
	content, _ := resp["content"].([]any)
	part, _ := content[0].(map[string]any)
	if part["type"] != "text" || part["text"] != "Hello world" {
		t.Errorf("content = %v", part)
	}
}

// TestAnthropicStreamEventSequence Anthropic 流式事件序列（含 input_json_delta）。
func TestAnthropicStreamEventSequence(t *testing.T) {
	_, mux := newTestGateway(streamingToolHandler())
	rec := post(mux, "/v1/messages",
		`{"model":"qoder/gmodel","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	assertEventOrder(t, body, []string{
		"message_start",
		"content_block_start", // text
		"content_block_delta",
		"content_block_stop",
		"content_block_start", // tool_use
		"content_block_delta", // input_json_delta
		"content_block_stop",
		"message_delta",
		"message_stop",
	})
	if !strings.Contains(body, `"input_json_delta"`) {
		t.Error("工具参数必须通过 input_json_delta 增量下发")
	}
	if !strings.Contains(body, `"stop_reason":"tool_use"`) {
		t.Error("finish_reason=tool_calls 应映射为 stop_reason=tool_use")
	}
}

// TestCountTokens count_tokens 返回正整数。
func TestCountTokens(t *testing.T) {
	_, mux := newTestGateway(&scriptedHandler{})
	rec := post(mux, "/v1/messages/count_tokens",
		`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"你好，世界"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if n, _ := asInt(resp["input_tokens"]); n <= 0 {
		t.Errorf("input_tokens = %v, want > 0", resp["input_tokens"])
	}
}

// TestAuthRejectsBadKey 鉴权：错误 key 被外层拦下，且错误体形状按协议区分。
func TestAuthRejectsBadKey(t *testing.T) {
	_, mux := newTestGateway(&scriptedHandler{})

	recA := httptest.NewRecorder()
	reqA := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"qoder/x","messages":[{"role":"user","content":"hi"}]}`))
	reqA.Header.Set("x-api-key", "wrong")
	mux.ServeHTTP(recA, reqA)
	if recA.Code != 401 || !strings.Contains(recA.Body.String(), `"type":"error"`) {
		t.Errorf("Anthropic 401 形状错误: %d %s", recA.Code, recA.Body.String())
	}

	recO := httptest.NewRecorder()
	reqO := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hi"}`))
	mux.ServeHTTP(recO, reqO)
	if recO.Code != 401 || !strings.Contains(recO.Body.String(), `"error"`) {
		t.Errorf("Responses 401 形状错误: %d %s", recO.Code, recO.Body.String())
	}
}

// TestModelResolution 模型名路由：前缀 / 通配 map / 兜底渠道 / 未知渠道报错。
func TestModelResolution(t *testing.T) {
	rt := Router{Default: "workbuddy", Map: map[string]string{"claude-*": "qoder/gmodel"}, Channels: []string{"workbuddy", "qoder"}}
	cases := []struct{ in, want string }{
		{"workbuddy/glm-5.2", "workbuddy/glm-5.2"},
		{"claude-sonnet-4-5", "qoder/gmodel"},
		{"gpt-5", "workbuddy/gpt-5"},
	}
	for _, c := range cases {
		got, err := rt.Resolve(c.in)
		if err != nil || got != c.want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	if _, err := rt.Resolve("unknown/x"); err == nil {
		t.Error("未知渠道前缀应报错")
	}
	if _, err := rt.Resolve(""); err == nil {
		t.Error("空模型名应报错")
	}
}

// Cline 的对外模型 ID 直接用上游命名空间（决议 R59）：
//   - 已经是完整上游 ID（cline-free/*）⇒ 路由层原样放行（不把它当未知渠道名而报错）；
//   - 旧的双层写法（cline/cline-free/*）⇒ 仍然放行（老配置兼容）；
//   - default_channel=cline 且客户端发裸名 ⇒ 拼命名空间前缀（而非 cline/），
//     否则裸名会被拼成 cline/<name>，而 cline 上游只认 cline-free/<name>。
func TestModelResolutionClineNamespace(t *testing.T) {
	rt := Router{Default: "cline", Channels: []string{"cline"}}
	cases := []struct{ in, want string }{
		{"cline-free/mimo-v2.6-flash", "cline-free/mimo-v2.6-flash"},
		{"cline/cline-free/mimo-v2.6-flash", "cline/cline-free/mimo-v2.6-flash"},
		{"mimo-v2.6-flash", "cline-free/mimo-v2.6-flash"},
	}
	for _, c := range cases {
		got, err := rt.Resolve(c.in)
		if err != nil || got != c.want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	// 上游命名空间前缀不是渠道名：不应出现在报错的「已知渠道」里误导用户，但必须放行
	if _, err := rt.Resolve("cline-free/x"); err != nil {
		t.Errorf("上游命名空间前缀应放行：%v", err)
	}
	// model_map 目标同样接受上游命名空间（用户可从面板复制新形态的 ID 填进去）
	rt2 := Router{Map: map[string]string{"claude-*": "cline-free/mimo-v2.6-flash"}, Channels: []string{"workbuddy", "cline"}}
	if got, err := rt2.Resolve("claude-sonnet-4"); err != nil || got != "cline-free/mimo-v2.6-flash" {
		t.Errorf("model_map 目标应接受上游命名空间：%q, %v", got, err)
	}
}

// TestResponsesRejectsPreviousResponseID 无状态实现必须明确拒绝 previous_response_id。
func TestResponsesRejectsPreviousResponseID(t *testing.T) {
	_, mux := newTestGateway(&scriptedHandler{})
	rec := post(mux, "/v1/responses", `{"model":"gpt-5","input":"hi","previous_response_id":"resp_1"}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "previous_response_id") {
		t.Errorf("应返回 400 且说明原因，得到 %d %s", rec.Code, rec.Body.String())
	}
}

// TestAnthropicThinkingBlock 思考链（reasoning_content）应转成 thinking 内容块。
// 部分思考模型只输出 reasoning_content 而无 content，不下发会让客户端收到空消息。
func TestAnthropicThinkingBlock(t *testing.T) {
	// 非流式
	inner := chatFixtureHandler(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"答案","reasoning_content":"先思考一下"},"finish_reason":"stop"}]}`)
	_, mux := newTestGateway(inner)
	rec := post(mux, "/v1/messages", `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	content, _ := resp["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content 块数 = %d, want 2 (thinking + text)", len(content))
	}
	if b, _ := content[0].(map[string]any); b["type"] != "thinking" || b["thinking"] != "先思考一下" {
		t.Errorf("首块 = %v, want thinking", content[0])
	}
	if b, _ := content[1].(map[string]any); b["type"] != "text" {
		t.Errorf("次块 = %v, want text", content[1])
	}

	// 流式：thinking 块 index 必须小于 text 块
	streamInner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, l := range []string{
			`data: {"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"思考"}}]}`,
			`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"答案"}}]}`,
			`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			"data: [DONE]",
		} {
			_, _ = w.Write([]byte(l + "\n\n"))
		}
	})
	_, mux2 := newTestGateway(streamInner)
	rec2 := post(mux2, "/v1/messages", `{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	body := rec2.Body.String()
	thinkIdx := strings.Index(body, `"type":"thinking"`)
	textIdx := strings.Index(body, `"type":"text"`)
	if thinkIdx < 0 || textIdx < 0 || thinkIdx > textIdx {
		t.Errorf("thinking 块必须在 text 块之前；thinkIdx=%d textIdx=%d\n%s", thinkIdx, textIdx, body)
	}
	assertEventOrder(t, body, []string{
		"message_start", "content_block_start", "content_block_delta", "content_block_stop",
		"content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop",
	})
}

// TestResponsesFunctionCallRoundTrip 回填轮：客户端原样回传 function_call item 时，
// 必须还原为 assistant.tool_calls，否则紧随的 function_call_output 会成为「无宿主」
// 的 role=tool 消息（上游报 11148 tool_call_sequence_broken，Codex CLI 第二轮必失败）。
func TestResponsesFunctionCallRoundTrip(t *testing.T) {
	var got []any
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &req)
		got, _ = req["messages"].([]any)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`))
	})
	_, mux := newTestGateway(inner)
	rec := post(mux, "/v1/responses", `{"model":"gpt-5","input":[`+
		`{"role":"user","content":"run it"},`+
		`{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"command\":\"ls\"}"},`+
		`{"type":"function_call_output","call_id":"call_1","output":"ok"},`+
		`{"role":"user","content":"what did it print?"}]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if len(got) != 4 {
		t.Fatalf("messages len = %d, want 4: %v", len(got), got)
	}
	asst, _ := got[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Fatalf("messages[1].role = %v, want assistant（function_call 必须还原为 assistant.tool_calls）", asst["role"])
	}
	tcs, _ := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls = %v", asst["tool_calls"])
	}
	tc, _ := tcs[0].(map[string]any)
	fn, _ := tc["function"].(map[string]any)
	if tc["id"] != "call_1" || fn["name"] != "shell" || fn["arguments"] != `{"command":"ls"}` {
		t.Errorf("tool_call = %v", tc)
	}
	tool, _ := got[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Errorf("messages[2] = %v, want role=tool 紧跟其后", tool)
	}
}

// TestResponsesParallelFunctionCalls 并行工具调用：连续多个 function_call 合并进同一条 assistant 消息。
func TestResponsesParallelFunctionCalls(t *testing.T) {
	msgs, err := convertResponsesItems([]any{
		map[string]any{"type": "function_call", "call_id": "c1", "name": "a", "arguments": "{}"},
		map[string]any{"type": "function_call", "call_id": "c2", "name": "b", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "c1", "output": "1"},
		map[string]any{"type": "function_call_output", "call_id": "c2", "output": "2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("messages len = %d, want 3（2 个 function_call 合并 + 2 条 tool）", len(msgs))
	}
	asst, _ := msgs[0].(map[string]any)
	if tcs, _ := asst["tool_calls"].([]any); len(tcs) != 2 {
		t.Errorf("tool_calls = %v, want 2", asst["tool_calls"])
	}
}

// TestResponsesFunctionCallMissingCallID 缺 call_id 必须报错，而不是静默产生坏序列。
func TestResponsesFunctionCallMissingCallID(t *testing.T) {
	_, err := convertResponsesItems([]any{map[string]any{"type": "function_call", "name": "a"}})
	if err == nil {
		t.Error("缺 call_id 应报错")
	}
}

// TestResponsesUsageNoNullDetails usage details 不应输出 null 子字段。
func TestResponsesUsageNoNullDetails(t *testing.T) {
	inner := chatFixtureHandler(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	_, mux := newTestGateway(inner)
	rec := post(mux, "/v1/responses", `{"model":"gpt-5","input":"hi"}`)
	if strings.Contains(rec.Body.String(), "null") {
		t.Errorf("响应中不应出现 null: %s", rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	u, _ := resp["usage"].(map[string]any)
	if u["input_tokens"] != float64(10) || u["output_tokens"] != float64(5) || u["total_tokens"] != float64(15) {
		t.Errorf("usage = %v", u)
	}
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

func chatFixtureHandler(respJSON string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respJSON))
	})
}

// streamingToolHandler 内层流式响应：先文本增量，再工具调用增量。
func streamingToolHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		lines := []string{
			`data: {"id":"c1","model":"up","choices":[{"index":0,"delta":{"content":"Let me "}}]}`,
			`data: {"id":"c1","model":"up","choices":[{"index":0,"delta":{"content":"check."}}]}`,
			`data: {"id":"c1","model":"up","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","function":{"name":"read_file","arguments":"{\"pa"}}]}}]}`,
			`data: {"id":"c1","model":"up","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"th\":\"a\"}"}}]}}]}`,
			`data: {"id":"c1","model":"up","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":11,"total_tokens":18}}`,
		}
		for _, l := range lines {
			_, _ = w.Write([]byte(l + "\n\n"))
			if f != nil {
				f.Flush()
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f != nil {
			f.Flush()
		}
	})
}

// assertEventOrder 断言事件按给定顺序出现（允许中间夹杂其它事件）。
func assertEventOrder(t *testing.T, body string, events []string) {
	t.Helper()
	pos := 0
	for _, ev := range events {
		idx := strings.Index(body[pos:], "event: "+ev+"\n")
		if idx < 0 {
			t.Fatalf("事件 %q 未出现或顺序错误；body 片段:\n%s", ev, truncateForLog(body))
		}
		pos += idx
	}
}

func truncateForLog(s string) string {
	if len(s) > 2000 {
		return s[:2000] + "..."
	}
	return s
}
