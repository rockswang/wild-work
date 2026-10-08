// handler_model_cooldown_test.go 回归测试：429/6004 模型级冷却（补丁 0006）。
//
// 背景（2026-09-23 生产事故）：WorkBuddy 6004 限流按模型计（「您也可以切换其他模型继续使用」），
// 但 -p4 的冷却是账号级——glm 撞墙把同号 deepseek 一起拖死，最终 14 账号全部冷却、
// 503 文案还误导用户「需重新登录」。本组测试锁定：
//  1. 单模型 429 只冷却该模型，同号其他模型继续可用；
//  2. 第二个模型也撞墙 → 升级整号冷却；
//  3. 全池仅该模型被冷却时，503 文案给出最早解冻时间而非「重新登录」误导。
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
)

// rateLimitUpstream 假上游：对指定模型返回 429/6004（带未来重置时间戳），
// 其余模型返回正常 SSE。Classify 按 body 是否含 6004 判定 ErrSoftRate。
type rateLimitUpstream struct {
	limitedModels map[string]bool
}

func (rateLimitUpstream) RefreshToken(*auth.Auth) error { return nil }

func (u rateLimitUpstream) ChatStream(_ *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	var peek struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)
	if u.limitedModels[peek.Model] {
		reset := time.Now().Add(2 * time.Hour).In(time.FixedZone("UTC+8", 8*3600))
		resp := fmt.Sprintf(`{"code":6004,"msg":"您的使用量已超出频率限制，将在 %s UTC+8 重置，您也可以切换其他模型继续使用。","requestId":"t"}`,
			reset.Format("2006-01-02 15:04:05"))
		return nil, http.StatusTooManyRequests, []byte(resp), nil
	}
	const sse = `data: {"id":"c1","object":"chat.completion.chunk","model":"up",` +
		`"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"up",` +
		`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	return io.NopCloser(strings.NewReader(sse)), 200, nil, nil
}

func (rateLimitUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) { return nil, nil }
func (rateLimitUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (rateLimitUpstream) UserResource(*auth.Auth) (int64, error) { return 1, nil }
func (rateLimitUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (rateLimitUpstream) DailyCheckin(*auth.Auth) error { return nil }
func (rateLimitUpstream) Classify(_ int, body string) provider.ErrKind {
	if strings.Contains(body, "6004") {
		return provider.ErrSoftRate
	}
	return provider.ErrNone
}
func (rateLimitUpstream) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	return echoUpstream{}.Stream(w, r, model)
}
func (rateLimitUpstream) Aggregate(r io.Reader, model string) (map[string]any, error) {
	return echoUpstream{}.Aggregate(r, model)
}

// newRateLimitHandler 单渠道单账号 Handler，limited 为返回 429 的模型集合。
func newRateLimitHandler(limited ...string) *Handler {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "t", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})
	lm := map[string]bool{}
	for _, m := range limited {
		lm[m] = true
	}
	rt := &Runtime{Kind: provider.Qoder, Pool: p, Upstream: rateLimitUpstream{limitedModels: lm}}
	return NewHandler(Config{Runtimes: map[provider.Kind]*Runtime{provider.Qoder: rt}})
}

func doChat(t *testing.T, h *Handler, model string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"stream":false,"messages":[{"role":"user","content":"hi"}]}`, model)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	h.ServeHTTP(rec, req)
	return rec
}

// Test429ModelCooldownKeepsOtherModelsUsable glm 撞 6004 后：
// 该请求透传 429 原文（含重置时间），随后同号 deepseek 请求应 200。
func Test429ModelCooldownKeepsOtherModelsUsable(t *testing.T) {
	h := newRateLimitHandler("glm-5.3")

	rec := doChat(t, h, "qoder/glm-5.3")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "6004") {
		t.Fatalf("单账号耗尽应透传 429 原文: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = doChat(t, h, "qoder/deepseek-v4.1-flash")
	if rec.Code != http.StatusOK {
		t.Fatalf("同号其他模型应继续可用（模型级冷却）: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// Test429SecondModelEscalatesAccountCooldown glm 与 deepseek 先后撞墙：
// 第二次触发整号升级冷却，第三个模型（kimi）的请求得到 503，且文案含解冻时间、
// 不再误导「需重新登录」。
func Test429SecondModelEscalatesAccountCooldown(t *testing.T) {
	h := newRateLimitHandler("glm-5.3", "deepseek-v4.1-flash")

	if rec := doChat(t, h, "qoder/glm-5.3"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("glm 首撞应 429: %d", rec.Code)
	}
	if rec := doChat(t, h, "qoder/deepseek-v4.1-flash"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("deepseek 二撞应 429（透传）: %d", rec.Code)
	}

	rec := doChat(t, h, "qoder/kimi-k2.7")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("升级整号后第三模型应 503: status=%d body=%s", rec.Code, rec.Body.String())
	}
	msg := rec.Body.String()
	if !strings.Contains(msg, "解冻") {
		t.Errorf("503 文案应含最早解冻时间: %s", msg)
	}
	if strings.Contains(msg, "重新登录") {
		t.Errorf("冷却场景不应误导用户重新登录: %s", msg)
	}
}

// TestModelAllAccountsCoolingMessageUnfreezeTime 账号健康但请求模型已全池模型级冷却：
// 503 文案明确「账号均健康、模型限流冷却、最早 HH:MM 解冻、无需重新登录」。
func TestModelAllAccountsCoolingMessageUnfreezeTime(t *testing.T) {
	h := newRateLimitHandler("glm-5.3")

	if rec := doChat(t, h, "qoder/glm-5.3"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("glm 首撞应 429: %d", rec.Code)
	}

	rec := doChat(t, h, "qoder/glm-5.3")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("模型级冷却后同模型再请求应 503: status=%d body=%s", rec.Code, rec.Body.String())
	}
	msg := rec.Body.String()
	for _, want := range []string{"均健康", "限流冷却", "解冻", "无需重新登录"} {
		if !strings.Contains(msg, want) {
			t.Errorf("503 文案应含 %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "refresh token is invalid") {
		t.Errorf("不应再出现重登误导文案: %s", msg)
	}
}
