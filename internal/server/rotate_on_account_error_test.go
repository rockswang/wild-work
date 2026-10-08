package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// 本文件锁定「账号级错误同请求换号」的三个组件：
// ① Rotatable 错误族划分；② 6004 重置时间解析；③ 候选耗尽透传真实上游错误。

// kindedUpstream 按 uid 决定返回形态的可配置假上游。
type kindedUpstream struct {
	mu     sync.Mutex
	calls  []string
	status int                     // 所有账号都返回这个错误形态
	kind   provider.ErrKind        // Classify 固定返回
	body   string                  // 错误响应体
	succeed func(uid string) bool   // 该 uid 返回 200（nil = 全部失败）
}

func (u *kindedUpstream) RefreshToken(*auth.Auth) error { return nil }

func (u *kindedUpstream) ChatStream(a *auth.Auth, _ []byte) (io.ReadCloser, int, []byte, error) {
	u.mu.Lock()
	u.calls = append(u.calls, a.UID)
	u.mu.Unlock()
	if u.succeed != nil && u.succeed(a.UID) {
		return io.NopCloser(strings.NewReader("data: [DONE]\n\n")), http.StatusOK, nil, nil
	}
	return nil, u.status, []byte(u.body), nil
}

func (u *kindedUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) {
	return []provider.ModelInfo{{ID: "m"}}, nil
}
func (u *kindedUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) { return nil, nil }
func (u *kindedUpstream) UserResource(*auth.Auth) (int64, error)                     { return 0, nil }
func (u *kindedUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (u *kindedUpstream) DailyCheckin(*auth.Auth) error { return nil }
func (u *kindedUpstream) Classify(status int, body string) provider.ErrKind {
	if status == http.StatusOK {
		return provider.ErrNone
	}
	return u.kind
}
func (u *kindedUpstream) Stream(w http.ResponseWriter, r io.Reader, _ string) (map[string]any, error) {
	_, err := io.Copy(io.Discard, r)
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	return nil, err
}
func (u *kindedUpstream) Aggregate(r io.Reader, _ string) (map[string]any, error) {
	_, _ = io.Copy(io.Discard, r)
	return map[string]any{"choices": []any{}}, nil
}

func newRotateTestHandler(t *testing.T, up *kindedUpstream, uidPrefix string) *Handler {
	p := pool.New(t.TempDir() + "/state.json")
	p.Add(&auth.Auth{Kind: "glm", AccessToken: "at-A", ExpiresAt: 4102444800, UID: uidPrefix + "A"})
	p.Add(&auth.Auth{Kind: "glm", AccessToken: "at-B", ExpiresAt: 4102444800, UID: uidPrefix + "B"})
	// A 积分更高保证首挑确定性（pool.PickExcluding 遍历 map 顺序随机）
	p.SetCreditDetail(uidPrefix+"A", 1000, 0, 0)
	p.SetCreditDetail(uidPrefix+"B", 500, 0, 0)
	return NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.GLM: {Kind: provider.GLM, Pool: p, Upstream: up,
				StaticModels: []provider.ModelInfo{{ID: "m"}}},
		},
		APIKey:       "",
		HardCooldown: 60_000_000_000,
		SoftCooldown: 60_000_000_000,
		ErrThreshold: 3,
		ErrCooldown:  60_000_000_000,
		MaxRotate:    3,
	})
}

// TestAllCandidatesExhaustedPassesThroughLastUpstreamError 候选耗尽时透传**最后一次
// 上游原始响应**（429+6004 原文），而不是包装成 503 no_healthy_account——
// 客户端需要看到重置时间等上游细节才能正确退避。
func TestAllCandidatesExhaustedPassesThroughLastUpstreamError(t *testing.T) {
	up := &kindedUpstream{
		status: http.StatusTooManyRequests,
		kind:   provider.ErrSoftRate,
		body:   `{"status":6004,"message":"usage exceeds frequency limit, 您也可以切换其他模型继续使用，将在 2026-09-23 14:47:19 UTC+8 重置"}`,
	}
	h := newRotateTestHandler(t, up, "e")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm/m","messages":[{"role":"user","content":"hi"}],"stream":false}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d, want 429（候选耗尽应透传最后的上游 429，而非包装 503）\n响应: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "6004") || !strings.Contains(rec.Body.String(), "重置") {
		t.Fatalf("应透传上游原始响应体（含 6004 与重置时间），实际: %s", rec.Body.String())
	}
	up.mu.Lock()
	calls := len(up.calls)
	up.mu.Unlock()
	if calls != 2 {
		t.Fatalf("应逐号尝试 A、B 各一次（共 2 次上游调用），实际 %d 次", calls)
	}
}

// TestContentErrorDoesNotRotate 内容类错误不换号：请求内容问题换号必然复现，
// 循环纯属浪费好号配额——必须单次调用即透传原文。
func TestContentErrorDoesNotRotate(t *testing.T) {
	up := &kindedUpstream{
		status: http.StatusBadRequest,
		kind:   provider.ErrContentBlocked,
		body:   `{"status":10200,"message":"content blocked"}`,
	}
	h := newRotateTestHandler(t, up, "c")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm/m","messages":[{"role":"user","content":"hi"}],"stream":false}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d, want 400 原文透传", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "content blocked") {
		t.Fatalf("应透传上游原文: %s", rec.Body.String())
	}
	up.mu.Lock()
	calls := len(up.calls)
	up.mu.Unlock()
	if calls != 1 {
		t.Fatalf("内容类错误不得换号重试（应只调 1 次），实际 %d 次", calls)
	}
}

// TestRotatableKindFamilies Rotatable 族划分：账号级错误可换号，
// 请求级/内容级错误换号必然复现，不得换号。
func TestRotatableKindFamilies(t *testing.T) {
	rotatable := []provider.ErrKind{
		provider.ErrSoftRate, provider.ErrHardCredit, provider.ErrSessionDead,
		provider.ErrNotFound, provider.ErrServer, provider.ErrAccountFault, provider.ErrClient,
	}
	for _, k := range rotatable {
		if !k.Rotatable() {
			t.Errorf("%v 应为 Rotatable（账号级错误，换号有价值）", k)
		}
	}
	notRotatable := []provider.ErrKind{
		provider.ErrContentBlocked, provider.ErrPromptTooLong, provider.ErrPassthrough,
	}
	for _, k := range notRotatable {
		if k.Rotatable() {
			t.Errorf("%v 不应为 Rotatable（内容/请求级错误，换号必然复现）", k)
		}
	}
}

// TestSoftRateResetTs 6004 重置时间解析：命中「将在 <ts> 重置」形态（可带
// UTC+8 说明后缀）；未命中/非法格式返回 false。
func TestSoftRateResetTs(t *testing.T) {
	const withSuffix = `...将在 2026-09-23 14:47:19 UTC+8 重置...`
	ts, ok := softRateResetTs(withSuffix)
	if !ok {
		t.Fatal("带 UTC+8 后缀的形态应命中")
	}
	want := time.Date(2026, 9, 23, 14, 47, 19, 0, time.FixedZone("UTC+8", 8*3600))
	if !ts.Equal(want) {
		t.Fatalf("解析时刻 = %v, want %v", ts, want)
	}
	if _, ok := softRateResetTs(`...将在 2026-09-23 14:47:19 重置...`); !ok {
		t.Fatal("不带后缀的形态应命中")
	}
	for _, miss := range []string{``, `将在`, `将在  重置`, `将在 not-a-time 重置`, `usage exceeds frequency limit`} {
		if _, ok := softRateResetTs(miss); ok {
			t.Fatalf("未命中形态 %q 不应解析出时间", miss)
		}
	}
}

// TestSoftRateCooldownParsesResetTime 冷却时长按真实重置点计算（而非固定 60s）：
// 6004 按重置点解封，固定 60s 软冷却过期后账号会被反复选中反复 429。
// 上限 24h：上游时间戳异常时不至于把号冻死；解析失败/已过期回退 SoftCooldown。
func TestSoftRateCooldownParsesResetTime(t *testing.T) {
	h := &Handler{cfg: Config{SoftCooldown: 60 * time.Second}}

	// 未来 2h 的重置点 → 冷却约 2h（秒级容差）
	future := time.Now().In(time.FixedZone("UTC+8", 8*3600)).Add(2 * time.Hour)
	body := "将在 " + future.Format("2006-01-02 15:04:05") + " 重置"
	got := h.softRateCooldown(body)
	if got < 90*time.Minute || got > 2*time.Hour+5*time.Second {
		t.Fatalf("未来 2h 重置点应冷却约 2h，实际 %v", got)
	}

	// 超过 24h 上限 → 钳到 24h
	far := time.Now().Add(365 * 24 * time.Hour)
	body = "将在 " + far.In(time.FixedZone("UTC+8", 8*3600)).Format("2006-01-02 15:04:05") + " 重置"
	if got := h.softRateCooldown(body); got != 24*time.Hour {
		t.Fatalf("超远重置点应钳到 24h，实际 %v", got)
	}

	// 无时间戳/已过期/负时长 → 回退默认 SoftCooldown
	for _, b := range []string{"no ts here", "将在 2000-01-01 00:00:00 重置"} {
		if got := h.softRateCooldown(b); got != 60*time.Second {
			t.Fatalf("未命中/过期应回退 SoftCooldown，body=%q 实际 %v", b, got)
		}
	}
}
