package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// 本文件锁定 ChatGPT 渠道的路由接线（票 04）：
// ① `chatgpt/<model>` 能出字（前缀被剥掉后才交给 Upstream）；
// ② 渠道前缀与 `/v1/models` 的对外 ID 都是 `chatgpt/`；
// ③ 未知/裸模型名仍按既有行为报错，且报错文案里带上 chatgpt 前缀
//   （清单漏改 = 「渠道能用但报错说不能用」的自相矛盾提示）。

const chatgptFakeSSE = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n\ndata: [DONE]\n\n"

// chatgptFakeUp 最小假上游：只关心「交给它的 body 用哪个模型」与「能不能吐字」。
type chatgptFakeUp struct {
	mu       sync.Mutex
	gotBody  string
	gotModel []string
}

func (u *chatgptFakeUp) RefreshToken(*auth.Auth) error { return nil }

func (u *chatgptFakeUp) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	u.mu.Lock()
	u.gotBody = string(body)
	u.mu.Unlock()
	return io.NopCloser(strings.NewReader(chatgptFakeSSE)), http.StatusOK, nil, nil
}

func (u *chatgptFakeUp) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) {
	return []provider.ModelInfo{{ID: "auto", Name: "自动选择"}}, nil
}

func (u *chatgptFakeUp) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

func (u *chatgptFakeUp) UserResource(*auth.Auth) (int64, error) { return 0, nil }

func (u *chatgptFakeUp) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}

func (u *chatgptFakeUp) DailyCheckin(*auth.Auth) error { return nil }

func (u *chatgptFakeUp) Classify(status int, body string) provider.ErrKind {
	if status == http.StatusOK {
		return provider.ErrNone
	}
	return provider.ErrPassthrough
}

func (u *chatgptFakeUp) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	u.mu.Lock()
	u.gotModel = append(u.gotModel, model)
	u.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	_, err := w.Write([]byte(chatgptFakeSSE))
	return nil, err
}

func (u *chatgptFakeUp) Aggregate(r io.Reader, model string) (map[string]any, error) {
	u.mu.Lock()
	u.gotModel = append(u.gotModel, model)
	u.mu.Unlock()
	_, err := io.Copy(io.Discard, r)
	return map[string]any{"choices": []any{}}, err
}

func newChatGPTTestHandler(t *testing.T, up provider.Upstream) *Handler {
	t.Helper()
	p := pool.New(t.TempDir() + "/state-chatgpt.json")
	p.Add(&auth.Auth{
		Kind:        provider.ChatGPT.String(),
		AccessToken: "fake-token",
		ExpiresAt:   4102444800, // 远期：不触发 refresh
		UID:         "cgpt-test-uid",
		Cookies:     map[string]string{"__Secure-next-auth.session-token": "fake"},
	})
	return NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.ChatGPT: {
				Kind:         provider.ChatGPT,
				Pool:         p,
				Upstream:     up,
				StaticModels: []provider.ModelInfo{{ID: "auto", Name: "自动选择"}},
			},
		},
		HardCooldown: 60_000_000_000,
		SoftCooldown: 60_000_000_000,
		ErrThreshold: 3,
		ErrCooldown:  60_000_000_000,
	})
}

// TestChatGPTPrefixedModelStreamsText 带渠道前缀的请求必须真的出字，
// 且交给 Upstream 的 body 里模型已剥掉前缀（Upstream 只认上游自己的 slug）。
func TestChatGPTPrefixedModelStreamsText(t *testing.T) {
	up := &chatgptFakeUp{}
	h := newChatGPTTestHandler(t, up)
	srv := httptest.NewServer(h)
	defer srv.Close()

	body := `{"model":"chatgpt/auto","stream":true,"messages":[{"role":"user","content":"你好"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "你好") {
		t.Fatalf("响应里没有正文（说明 chatgpt 渠道没接通）：%s", raw)
	}
	if up.gotBody == "" {
		t.Fatal("上游没收到请求体")
	}
	if !strings.Contains(up.gotBody, `"model":"auto"`) {
		t.Fatalf("交给上游的模型应是上游 slug（auto），实际 body=%s", up.gotBody)
	}
	if strings.Contains(up.gotBody, "chatgpt/") {
		t.Fatalf("渠道前缀必须剥掉再出站，实际 body=%s", up.gotBody)
	}
}

// TestChatGPTModelsUseChannelPrefix `/v1/models` 里该渠道的对外 ID 必须带 `chatgpt/` 前缀。
func TestChatGPTModelsUseChannelPrefix(t *testing.T) {
	h := newChatGPTTestHandler(t, &chatgptFakeUp{})
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), `"chatgpt/auto"`) {
		t.Fatalf("/v1/models 里没有 chatgpt/auto：%s", raw)
	}
}

// TestChatGPTUnknownModelStillRejected 未带渠道前缀/未知渠道仍按既有行为 400，
// 且文案里必须出现 chatgpt 前缀（否则用户看到「渠道不能用」的错误提示）。
func TestChatGPTUnknownModelStillRejected(t *testing.T) {
	h := newChatGPTTestHandler(t, &chatgptFakeUp{})
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, model := range []string{"auto", "gpt-6", "nosuchchannel/x"} {
		body := `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("%s: 请求失败：%v", model, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status=%d，want 400（body=%s）", model, resp.StatusCode, raw)
		}
	}
	// 裸模型名的报错文案必须列出 chatgpt 前缀（清单由人手维护，漏改即自相矛盾）。
	body := `{"model":"auto","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), "chatgpt/") {
		t.Fatalf("报错文案没列出 chatgpt 前缀：%s", raw)
	}
}

// TestChatGPTRuntimeNotSingleAccount 账号池语义：ChatGPT 是正常多账号渠道，
// 账号级错误必须走换号路径，而不是像匿名渠道那样整条渠道下线（R16/R22 的反面）。
func TestChatGPTRuntimeNotSingleAccount(t *testing.T) {
	h := newChatGPTTestHandler(t, &chatgptFakeUp{})
	rt := h.getRuntime(provider.ChatGPT)
	if rt == nil {
		t.Fatal("chatgpt 运行时未注册")
	}
	if rt.SingleAccount {
		t.Fatal("ChatGPT 是真多账号渠道，不得置 SingleAccount")
	}
}
