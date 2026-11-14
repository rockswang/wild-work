// e2e_modelscope_fake_test.go ModelScope 渠道端到端测试的**假上游**与产物样本。
//
// 上游一律 httptest 假服务器（AGENTS.md §0.1：绝不打真实上游，禁用生产账号做批量探测）。
// 假上游刻意照抄真实协议的三个面：目录（OpenAI 形状）、对话（JSON + SSE 含末帧 usage）、
// 生图（异步 submit → 轮询 → output_images）。只有"照抄协议"才能验证网关的转换逻辑，
// 而不是验证我们自己对协议的想象。
//
// 计数器（reqs）是本层的核心证据面：用户故事的承诺大多是"上游到底收到了什么"，
// 只能从假上游的记录里读，不能从网关的响应体反推（响应体可能被网关改写）。

package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// ---------------------------------------------------------------------------
// 1x1 PNG（假上游的"产物图"）：断言 b64 往返与原样透传都用它。
// ---------------------------------------------------------------------------

const tinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(tinyPNGBase64)
	if err != nil {
		t.Fatalf("内置 PNG 样本损坏: %v", err)
	}
	return b
}

// ---------------------------------------------------------------------------
// 假 ModelScope 上游
// ---------------------------------------------------------------------------

// msReq 一条被假上游收到的请求（断言出站形状用）。
type msReq struct {
	Method string
	Path   string
	Token  string
	Header http.Header
	Body   []byte
}

// msFake 模拟 api-inference.modelscope.cn / .ai 的四个面：
//
//	GET  /v1/models                    目录（OpenAI 形状）
//	POST /v1/chat/completions          对话（JSON 或 SSE）
//	POST /v1/images/generations        异步提交（要求 X-ModelScope-Async-Mode: true）
//	GET  /v1/tasks/{id}                轮询（要求 X-ModelScope-Task-Type: image_generation）
//	GET  /files/*.png                  产物图字节
//
// 所有可变量都带锁：并发请求（换号、轮询）会同时打进来。
type msFake struct {
	mu sync.Mutex

	// tokens 非 nil 时只接受这些 Bearer token（nil = 接受任意 token）。
	tokens map[string]bool
	// catalog /v1/models 的 data[]；modelsStatus 非 0 时用于模拟目录失败。
	catalog      []map[string]any
	modelsStatus int
	// chatStatus/chatBody：按 token 定制对话应答（模拟 429 等账号级错误）。
	chatStatus map[string]int
	chatBody   map[string]string
	chatText   string
	// submitStatus/submitBody：按 token 定制生图提交应答。
	submitStatus map[string]int
	submitBody   map[string]string
	// pollScript 依次返回的 task_status（用尽后重复最后一个）；pollIdx 是游标。
	pollScript []string
	pollIdx    int
	taskID     string
	image      []byte
	// outputCount 一次任务返回几张产物（默认 1）——「上游给几张返几张，不与 n 对齐」用。
	outputCount int

	reqs []msReq
	srv  *httptest.Server
}

func newMSFake(t *testing.T, image []byte) *msFake {
	t.Helper()
	f := &msFake{
		catalog: []map[string]any{
			// 静态表里也有它（并集断言用）——带上游声明的能力字段，验证「上游说了才透传」。
			{"id": "Qwen/Qwen3.5-122B-A10B", "object": "model", "owned_by": "modelscope",
				"context_length": float64(131072), "supports_tools": true},
			// 只在远程目录、不在静态表里（证明「静态 ∪ 远程」不是一句空话）。
			{"id": "Qwen/Qwen2.5-7B-Instruct", "object": "model", "owned_by": "modelscope"},
			// 生图模型（对话路径必须本地拒绝，生图路径必须可路由）。
			{"id": "Qwen/Qwen-Image", "object": "model", "owned_by": "modelscope"},
		},
		chatStatus:   map[string]int{},
		chatBody:     map[string]string{},
		chatText:     "你好，我是魔搭。",
		submitStatus: map[string]int{},
		submitBody:   map[string]string{},
		pollScript:   []string{"SUCCEED"},
		taskID:       "task-e2e-0001",
		outputCount:  1,
		image:        image,
	}
	f.srv = httptest.NewServer(f.handler())
	t.Cleanup(f.srv.Close)
	return f
}

func (f *msFake) URL() string { return f.srv.URL }

// acceptOnly 限定可接受的 token 集合（模拟「两区 token 不通用」）。
func (f *msFake) acceptOnly(tokens ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = make(map[string]bool, len(tokens))
	for _, t := range tokens {
		f.tokens[t] = true
	}
}

// setChatStatus 让某个 token 的对话请求固定回某个状态码（账号级错误模拟）。
func (f *msFake) setChatStatus(token string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chatStatus[token] = status
	f.chatBody[token] = body
}

// reqsByPath 取某路径的全部请求快照。
func (f *msFake) reqsByPath(path string) []msReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]msReq, 0, len(f.reqs))
	for _, r := range f.reqs {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// total 本次测试期间收到的上游请求总数（「本地拒绝 = 零上游请求」断言用）。
func (f *msFake) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *msFake) record(r *http.Request, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, msReq{
		Method: r.Method, Path: r.URL.Path,
		Token:  strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "),
		Header: r.Header.Clone(),
		Body:   append([]byte(nil), body...),
	})
}

func (f *msFake) authorized(token string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tokens == nil {
		return true
	}
	return f.tokens[token]
}

func (f *msFake) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		f.mu.Lock()
		status, catalog := f.modelsStatus, f.catalog
		f.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"catalog unavailable"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": catalog})
	})

	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.record(r, raw)
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !f.authorized(token) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"401","message":"invalid token (region mismatch)"}}`))
			return
		}
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
			Model_ string `json:"modelName"`
		}
		_ = json.Unmarshal(raw, &req)
		f.mu.Lock()
		status, override := f.chatStatus[token], f.chatBody[token]
		text := f.chatText
		f.mu.Unlock()
		if status >= 400 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(override))
			return
		}
		if req.Stream {
			writeFakeSSE(w, req.Model, text)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-ms-e2e", "object": "chat.completion", "created": 1700000000,
			"model": req.Model,
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": text},
			}},
			"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18},
		})
	})

	mux.HandleFunc("/v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.record(r, raw)
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !f.authorized(token) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"401","message":"invalid token (region mismatch)"}}`))
			return
		}
		f.mu.Lock()
		status, override := f.submitStatus[token], f.submitBody[token]
		taskID := f.taskID
		f.mu.Unlock()
		if status >= 400 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(override))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"task_id": taskID})
	})

	mux.HandleFunc("/v1/tasks/", func(w http.ResponseWriter, r *http.Request) {
		f.record(r, nil)
		f.mu.Lock()
		idx := f.pollIdx
		status := "SUCCEED"
		if len(f.pollScript) > 0 {
			if idx >= len(f.pollScript) {
				idx = len(f.pollScript) - 1
			}
			status = f.pollScript[idx]
		}
		f.pollIdx++
		urls := make([]string, 0, f.outputCount)
		for i := 0; i < f.outputCount; i++ {
			if f.outputCount == 1 {
				urls = append(urls, f.srv.URL+"/files/out.png")
				break
			}
			urls = append(urls, fmt.Sprintf("%s/files/out%d.png", f.srv.URL, i))
		}
		f.mu.Unlock()
		out := map[string]any{"task_id": strings.TrimPrefix(r.URL.Path, "/v1/tasks/"), "task_status": status}
		if status == "SUCCEED" {
			out["output_images"] = urls
		}
		_ = json.NewEncoder(w).Encode(out)
	})

	mux.HandleFunc("/files/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		img := f.image
		f.mu.Unlock()
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(img)
	})

	return mux
}

// writeFakeSSE 按 OpenAI chunk 形状发流（含末帧 usage：handler 据此记 token 流水）。
func writeFakeSSE(w http.ResponseWriter, model, text string) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)

	frame := func(v map[string]any) {
		b, _ := json.Marshal(v)
		_, _ = io.WriteString(w, "data: "+string(b)+"\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
	base := map[string]any{"id": "chatcmpl-ms-e2e", "object": "chat.completion.chunk",
		"created": 1700000000, "model": model}
	for _, part := range []string{text[:len(text)/2], text[len(text)/2:]} {
		base["choices"] = []any{map[string]any{"index": 0, "finish_reason": nil,
			"delta": map[string]any{"content": part}}}
		frame(base)
	}
	base["choices"] = []any{map[string]any{"index": 0, "finish_reason": "stop",
		"delta": map[string]any{}}}
	base["usage"] = map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18}
	frame(base)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// ---------------------------------------------------------------------------
// glm 生图回归桩（票 03「抽接口后 glm/image 行为零变化」）
// ---------------------------------------------------------------------------
//
// 这里刻意**不**复用 glm 包的真实实现：要验的是 server 层的路由与形状契约
// （model 归一 → runtime 选择 → ImageRequest → 响应 data[]），glm 私有协议本身已由
// internal/glm 的测试覆盖。桩不实现 InlineImageRefProvider ⇒ 走默认（glm）语义。
type glmStub struct {
	mu       sync.Mutex
	genReqs  []provider.ImageRequest
	editReqs []provider.ImageRequest
	image    string // 产物 URL
}

func (g *glmStub) GenerateImage(_ *auth.Auth, req provider.ImageRequest) ([]provider.ImageResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.genReqs = append(g.genReqs, req)
	return []provider.ImageResult{{URL: g.image, RevisedPrompt: req.Prompt + " (revised)"}}, nil
}

func (g *glmStub) EditImage(_ *auth.Auth, req provider.ImageRequest) ([]provider.ImageResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.editReqs = append(g.editReqs, req)
	return []provider.ImageResult{{URL: g.image}}, nil
}

func (g *glmStub) gens() []provider.ImageRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]provider.ImageRequest(nil), g.genReqs...)
}

// —— provider.Upstream 的空实现（对话面不参与本层断言）——

func (g *glmStub) RefreshToken(*auth.Auth) error                        { return nil }
func (g *glmStub) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) { return nil, nil }
func (g *glmStub) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (g *glmStub) UserResource(*auth.Auth) (int64, error) { return 0, nil }
func (g *glmStub) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (g *glmStub) DailyCheckin(*auth.Auth) error         { return nil }
func (g *glmStub) Classify(int, string) provider.ErrKind { return provider.ErrServer }
func (g *glmStub) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	return io.NopCloser(strings.NewReader("data: [DONE]\n\n")), 200, nil, nil
}
func (g *glmStub) Stream(http.ResponseWriter, io.Reader, string) (map[string]any, error) {
	return nil, nil
}
func (g *glmStub) Aggregate(io.Reader, string) (map[string]any, error) {
	return map[string]any{"choices": []any{}}, nil
}
