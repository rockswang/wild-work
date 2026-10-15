// 思考强度档位注入行为锁定：客户端未传 reasoning_effort 且配置命中时注入
// （精确命中优先、未命中回退 `*` 全局默认）；键已存在不碰（任意值含 null——
// 客户端显式意图）；未配置零开销直通；面板保存热更新即生效；注入失败原样
// 转发不中断请求。移植自 sidecar v1.28.0/1 的 applyEffort/injectEffortField。
package server

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
	"wild-work/internal/oczen"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// effUpstream 假上游：捕获发往上游的请求体（ChatStream 的 body 参数即最终转发内容），
// 非流式按官方语义回填模型名。
type effUpstream struct {
	mu     sync.Mutex
	calls  int
	bodies [][]byte
}

func (u *effUpstream) lastBody() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.bodies) == 0 {
		return nil
	}
	return u.bodies[len(u.bodies)-1]
}

func (u *effUpstream) RefreshToken(*auth.Auth) error                                 { return nil }
func (u *effUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error)          { return nil, nil }
func (u *effUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) { return nil, nil }
func (u *effUpstream) UserResource(*auth.Auth) (int64, error)                        { return 0, nil }
func (u *effUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (u *effUpstream) DailyCheckin(*auth.Auth) error         { return nil }
func (u *effUpstream) Classify(int, string) provider.ErrKind { return provider.ErrServer }
func (u *effUpstream) ChatStream(_ *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	u.mu.Lock()
	u.calls++
	u.bodies = append(u.bodies, body)
	u.mu.Unlock()
	return io.NopCloser(strings.NewReader("data: [DONE]\n\n")), 200, nil, nil
}
func (u *effUpstream) Stream(http.ResponseWriter, io.Reader, string) (map[string]any, error) {
	return nil, nil
}
func (u *effUpstream) Aggregate(_ io.Reader, model string) (map[string]any, error) {
	return map[string]any{"model": model, "choices": []any{}}, nil
}

// newEffHandler 装配单渠道（oczen）handler，注入指定思考强度映射。
func newEffHandler(t *testing.T, up provider.Upstream, efforts map[string]string) *Handler {
	t.Helper()
	pa := pool.New(t.TempDir() + "/state-a.json")
	pa.Add(oczen.AnonymousAuth())
	return NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.Oczen: {Kind: provider.Oczen, Pool: pa, Upstream: up},
		},
		APIKey: "k", MaxRotate: 3,
		HardCooldown: time.Hour, SoftCooldown: time.Minute,
		ErrThreshold: 1, ErrCooldown: time.Minute,
		ReasoningEfforts: efforts,
	})
}

// callEffBody 发一次非流式 chat 请求（body 原样作为请求体）。
func callEffBody(h *Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func callEffModel(h *Handler, model string) *httptest.ResponseRecorder {
	return callEffBody(h, `{"model":"`+model+`","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
}

// upstreamEffort 读上游收到的最后一发请求体里的 reasoning_effort（exists=键是否存在）。
func upstreamEffort(t *testing.T, up *effUpstream) (val any, exists bool) {
	t.Helper()
	raw := up.lastBody()
	if raw == nil {
		t.Fatal("上游未收到任何请求")
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("上游请求体不是合法 JSON: %v", err)
	}
	val, exists = obj["reasoning_effort"]
	return val, exists
}

// TestEffortInjectsWhenMissing 未传 reasoning_effort 且精确命中 → 注入配置档位，
// 其余字段（messages 等）原样保留。
func TestEffortInjectsWhenMissing(t *testing.T) {
	up := &effUpstream{}
	h := newEffHandler(t, up, map[string]string{"oczen/big-pickle": "high"})
	rec := callEffModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK {
		t.Fatalf("应正常返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	val, ok := upstreamEffort(t, up)
	if !ok || val != "high" {
		t.Errorf("reasoning_effort = %v (exists=%v)，want 注入 high", val, ok)
	}
	var obj map[string]any
	_ = json.Unmarshal(up.lastBody(), &obj)
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("messages 应原样保留，实际 %d 条", len(msgs))
	}
}

// TestEffortKeyPresentUntouched 键已存在不碰：显式 low 保持 low；显式 null 保持 null
// （null 也是客户端显式意图，「键存在即不动」而非「值为空才注入」）。
func TestEffortKeyPresentUntouched(t *testing.T) {
	up := &effUpstream{}
	h := newEffHandler(t, up, map[string]string{"oczen/big-pickle": "high"})
	rec := callEffBody(h, `{"model":"oczen/big-pickle","stream":false,"reasoning_effort":"low","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("应正常返回 200，实际 %d", rec.Code)
	}
	if val, _ := upstreamEffort(t, up); val != "low" {
		t.Errorf("客户端显式 reasoning_effort=%v 应原样保留，不得覆盖", val)
	}
	up2 := &effUpstream{}
	h2 := newEffHandler(t, up2, map[string]string{"oczen/big-pickle": "high"})
	rec2 := callEffBody(h2, `{"model":"oczen/big-pickle","stream":false,"reasoning_effort":null,"messages":[{"role":"user","content":"hi"}]}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("应正常返回 200，实际 %d", rec2.Code)
	}
	val, ok := upstreamEffort(t, up2)
	if !ok {
		t.Error("reasoning_effort:null 键应保留（键存在即不动），实际键被删")
	}
	if val != nil {
		t.Errorf("reasoning_effort=null 应原样保留，实际 %v", val)
	}
}

// TestEffortWildcardFallback 精确未命中回退 `*` 全局默认；精确命中优先于 `*`。
func TestEffortWildcardFallback(t *testing.T) {
	up := &effUpstream{}
	h := newEffHandler(t, up, map[string]string{"*": "medium"})
	rec := callEffModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK {
		t.Fatalf("应正常返回 200，实际 %d", rec.Code)
	}
	if val, ok := upstreamEffort(t, up); !ok || val != "medium" {
		t.Errorf("reasoning_effort = %v (exists=%v)，want 回退注入 `*` 的 medium", val, ok)
	}

	up2 := &effUpstream{}
	h2 := newEffHandler(t, up2, map[string]string{"*": "low", "oczen/big-pickle": "high"})
	_ = callEffModel(h2, "oczen/big-pickle")
	if val, _ := upstreamEffort(t, up2); val != "high" {
		t.Errorf("精确命中应优先于 `*`，实际 %v", val)
	}
}

// TestEffortUnconfiguredOrNoMatchNoInject 未配置（空 map）或未命中 → 不注入
// （未配置 = 常态，请求路径零改动）。
func TestEffortUnconfiguredOrNoMatchNoInject(t *testing.T) {
	up := &effUpstream{}
	h := newEffHandler(t, up, nil)
	if rec := callEffModel(h, "oczen/big-pickle"); rec.Code != http.StatusOK {
		t.Fatalf("未配置应正常直通，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if _, ok := upstreamEffort(t, up); ok {
		t.Error("未配置时不应注入 reasoning_effort")
	}

	up2 := &effUpstream{}
	h2 := newEffHandler(t, up2, map[string]string{"workbuddy/other": "high"})
	_ = callEffModel(h2, "oczen/big-pickle")
	if _, ok := upstreamEffort(t, up2); ok {
		t.Error("模型未命中且无 `*` 时不应注入")
	}
}

// TestSetReasoningEffortsHotReload 热更新：SetReasoningEfforts 后立即按新映射注入，
// 清空（nil）即恢复不注入（面板保存即生效的契约）。
func TestSetReasoningEffortsHotReload(t *testing.T) {
	up := &effUpstream{}
	h := newEffHandler(t, up, nil)
	_ = callEffModel(h, "oczen/big-pickle")
	if _, ok := upstreamEffort(t, up); ok {
		t.Fatal("未配置时不应注入")
	}
	h.SetReasoningEfforts(map[string]string{"oczen/big-pickle": "high"})
	_ = callEffModel(h, "oczen/big-pickle")
	if val, _ := upstreamEffort(t, up); val != "high" {
		t.Errorf("热更新后应立即注入，实际 %v", val)
	}
	h.SetReasoningEfforts(nil)
	_ = callEffModel(h, "oczen/big-pickle")
	if _, ok := upstreamEffort(t, up); ok {
		t.Error("清空映射后不应再注入")
	}
}

// TestEffortConcurrentHotReload 并发热更新与请求转发的竞态安全（-race 下跑）：
// 面板保存（SetReasoningEfforts 换表）与 chatCompletions 读取（lookupEffort）并发不炸。
func TestEffortConcurrentHotReload(t *testing.T) {
	up := &effUpstream{}
	h := newEffHandler(t, up, nil)
	maps := []map[string]string{
		{"oczen/big-pickle": "high"},
		{"*": "low"},
		nil,
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				h.SetReasoningEfforts(maps[i%len(maps)])
			}
		}
	}()
	for i := 0; i < 50; i++ {
		if rec := callEffModel(h, "oczen/big-pickle"); rec.Code != http.StatusOK {
			t.Fatalf("并发热更新期间请求应正常，实际 %d：%s", rec.Code, rec.Body.String())
		}
	}
	close(stop)
	wg.Wait()
}

// TestInjectEffortField 注入函数单测：非法 body（坏 JSON / null / 数组）返回 error
// 由调用方原样兜底；键存在返回 changed=false；正常注入保留其余字段。
func TestInjectEffortField(t *testing.T) {
	if _, _, err := injectEffortField([]byte(`{bad json`), "high"); err == nil {
		t.Error("坏 JSON 应返回 error")
	}
	if _, _, err := injectEffortField([]byte(`null`), "high"); err == nil {
		t.Error("null body 应返回 error（不是 JSON 对象）")
	}
	if _, _, err := injectEffortField([]byte(`[1,2]`), "high"); err == nil {
		t.Error("数组 body 应返回 error（不是 JSON 对象）")
	}
	nb, changed, err := injectEffortField([]byte(`{"model":"m","reasoning_effort":"low"}`), "high")
	if err != nil || changed {
		t.Errorf("键已存在应 changed=false，实际 changed=%v err=%v", changed, err)
	}
	nb, changed, err = injectEffortField([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`), "high")
	if err != nil || !changed {
		t.Fatalf("应注入成功，实际 changed=%v err=%v", changed, err)
	}
	var obj map[string]any
	if err := json.Unmarshal(nb, &obj); err != nil {
		t.Fatalf("注入产物应为合法 JSON: %v", err)
	}
	if obj["reasoning_effort"] != "high" || obj["model"] != "m" {
		t.Errorf("注入产物字段不符: %v", obj)
	}
	if msgs, _ := obj["messages"].([]any); len(msgs) != 1 {
		t.Errorf("其余字段应原样保留: %v", obj)
	}
}
