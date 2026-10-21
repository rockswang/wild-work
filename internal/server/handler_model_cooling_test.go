// handler_model_cooling_test.go 回归：上游按模型独立限流时，冷却与粘性都必须按 (账号, 模型) 粒度。
//
// 历史缺陷：pool 的冷却只有账号级单时间戳、粘性 key 只按渠道 —— 一个模型撞到每日上限
// （WorkBuddy 系 429/6004，原文即声明「alternatively, you can switch to the other models」）
// 会把同账号上仍可用的模型一起连坐，且粘性整体切到另一个账号。
//
// 假上游的 Classify 委托真实实现（internal/upstream.Classify），因此同时覆盖「分类」与「策略」两段。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// softRateModelScopedBody 上游 6004 频率限流原文（节选，保留判定特征串）。
const softRateModelScopedBody = `{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, ` +
	`your usage will reset at 2026-09-29 00:00:00 UTC+8, alternatively, ` +
	`you can switch to the other models to continue using it.","requestId":"r1"}`

// modelScopedUpstream 假上游：模型名含 "flash" 时返回 429（body 可注入），其余模型返回成功 SSE。
type modelScopedUpstream struct {
	body string // 429 时的 body；空则用 softRateModelScopedBody
}

func (modelScopedUpstream) RefreshToken(*auth.Auth) error { return nil }

func (u modelScopedUpstream) ChatStream(_ *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &req)
	if strings.Contains(req.Model, "flash") {
		b := u.body
		if b == "" {
			b = softRateModelScopedBody
		}
		return io.NopCloser(strings.NewReader("")), http.StatusTooManyRequests, []byte(b), nil
	}
	const sse = `data: {"id":"c1","object":"chat.completion.chunk","model":"upstream-auto",` +
		`"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"upstream-auto",` +
		`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	return io.NopCloser(strings.NewReader(sse)), http.StatusOK, nil, nil
}

func (modelScopedUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) { return nil, nil }
func (modelScopedUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (modelScopedUpstream) UserResource(*auth.Auth) (int64, error) { return 1, nil }
func (modelScopedUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (modelScopedUpstream) DailyCheckin(*auth.Auth) error { return nil }
func (modelScopedUpstream) Classify(status int, body string) provider.ErrKind {
	return upstream.Classify(status, body)
}
func (modelScopedUpstream) Stream(http.ResponseWriter, io.Reader, string) (map[string]any, error) {
	return nil, nil
}

// Aggregate 回填入参 model（与真实实现一致：网关把客户端请求名写回响应）。
func (modelScopedUpstream) Aggregate(_ io.Reader, model string) (map[string]any, error) {
	return map[string]any{
		"id": "c1", "object": "chat.completion", "created": time.Now().Unix(),
		"model": model,
		"choices": []any{map[string]any{
			"index": 0, "message": map[string]any{"role": "assistant", "content": "hi"},
			"finish_reason": "stop",
		}},
	}, nil
}

// newModelCoolingHandler 两个账号（u1 余额更高，无冷却时必被选中）+ 单渠道。
// body 注入 429 响应体，用于区分模型级与账号级限流。
func newModelCoolingHandler(body string) (*Handler, *pool.Pool) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "t", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "t", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})
	p.SetCreditDetail("u1", 500, 0, 0, time.Time{})
	p.SetCreditDetail("u2", 100, 0, 0, time.Time{})
	rt := &Runtime{Kind: provider.Qoder, Pool: p, Upstream: modelScopedUpstream{body: body}}
	h := NewHandler(Config{
		Runtimes:     map[provider.Kind]*Runtime{provider.Qoder: rt},
		ErrThreshold: 1,
		ErrCooldown:  time.Hour,
		HardCooldown: time.Hour,
		SoftCooldown: time.Minute,
	})
	return h, p
}

func postModel(h *Handler, model string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(fmt.Sprintf(`{"model":%q,"stream":false,"messages":[{"role":"user","content":"hi"}]}`, model)))
	h.ServeHTTP(rec, req)
	return rec
}

// TestModelScopedSoftRateDoesNotCoolOtherModels 模型级限流（6004 带「可换其他模型」文案）
// 只能冷却当前模型：账号级状态必须保持健康，同账号的其他模型仍可路由。
func TestModelScopedSoftRateDoesNotCoolOtherModels(t *testing.T) {
	h, p := newModelCoolingHandler("")

	if rec := postModel(h, "qoder/flash-1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("flash status=%d want 429, body=%s", rec.Code, rec.Body.String())
	}

	// 关键断言：账号未被整体冷却，只有 flash-1 进入模型级冷却
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 应存在")
	}
	if st.Cooling {
		t.Errorf("模型级限流不应把账号标记为整体冷却：%+v", st)
	}
	if st.Reason != "" {
		t.Errorf("模型级冷却不应写账号级 reason（面板会误报整号被限流）：%q", st.Reason)
	}
	if !modelCooling(st, "flash-1") {
		t.Errorf("u1 的 flash-1 应处于模型级冷却：%+v", st.ModelCooling)
	}

	// luna 必须仍然可用，且回到余额最高的 u1（模型级冷却不该把它挤到 u2）
	rec := postModel(h, "qoder/luna-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("luna status=%d want 200, body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Model != "qoder/luna-1" {
		t.Errorf("响应 model=%q want qoder/luna-1", resp.Model)
	}
	h.stickyMu.RLock()
	luna := h.sticky[h.stickyKey(provider.Qoder, "luna-1")]
	h.stickyMu.RUnlock()
	if luna == nil || luna.uid != "u1" {
		t.Errorf("luna 应路由到余额最高的 u1（flash 冷却不连坐）：%+v", luna)
	}
}

// TestAccountScopedSoftRateCoolsAccount 无「可换其他模型」特征串的 429 是账号级限流，
// 必须保守按账号级冷却（若误判为模型级，同账号 N 个模型会各白撞一次）。
func TestAccountScopedSoftRateCoolsAccount(t *testing.T) {
	h, p := newModelCoolingHandler(`{"code":9999,"msg":"too many requests"}`)

	if rec := postModel(h, "qoder/flash-1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d want 429, body=%s", rec.Code, rec.Body.String())
	}
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Errorf("账号级 429 应冷却整个账号：%+v", st)
	}
	if len(st.ModelCooling) != 0 {
		t.Errorf("账号级冷却不应产生 ModelCooling：%+v", st.ModelCooling)
	}
}

// TestStickyIsolatedPerModel 粘性记录按 (渠道, 模型) 隔离：
// flash 的失败只清 flash 的粘性，luna 已建立的粘性不受影响。
func TestStickyIsolatedPerModel(t *testing.T) {
	h, _ := newModelCoolingHandler("")

	for i := 0; i < 2; i++ {
		if rec := postModel(h, "qoder/luna-1"); rec.Code != http.StatusOK {
			t.Fatalf("luna#%d status=%d, body=%s", i, rec.Code, rec.Body.String())
		}
	}
	if rec := postModel(h, "qoder/flash-1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("flash status=%d want 429", rec.Code)
	}

	h.stickyMu.RLock()
	luna := h.sticky[h.stickyKey(provider.Qoder, "luna-1")]
	flash := h.sticky[h.stickyKey(provider.Qoder, "flash-1")]
	h.stickyMu.RUnlock()

	if luna == nil || luna.uid != "u1" {
		t.Errorf("flash 的失败不应清掉 luna 的粘性记录：%+v", luna)
	}
	if flash != nil {
		t.Errorf("flash 失败后其自身粘性记录应被清除：%+v", flash)
	}
}

// TestStickyEvictionKeepsBound 粘性 key 含模型后条目数不再有天然上界，
// 超出上限时须淘汰最久未用者（否则长期运行下 map 无界增长）。
func TestStickyEvictionKeepsBound(t *testing.T) {
	h, _ := newModelCoolingHandler("")

	h.stickyMu.Lock()
	for i := 0; i < maxStickyEntries+10; i++ {
		h.sticky[fmt.Sprintf("k%d", i)] = &stickyEntry{
			uid: "u1", maxReqs: 50,
			lastUsed: time.Now().Add(time.Duration(i) * time.Second), // i 越大越新
		}
	}
	h.evictStickyLocked()
	n := len(h.sticky)
	_, oldestKept := h.sticky["k0"]
	h.stickyMu.Unlock()

	if n > maxStickyEntries {
		t.Errorf("粘性表应被淘汰到上限内：%d > %d", n, maxStickyEntries)
	}
	if oldestKept {
		t.Error("最久未用的条目应被淘汰")
	}
}
