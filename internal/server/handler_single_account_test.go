// handler_single_account_test.go 回归测试：单账号渠道（oczen）不得对唯一账号施加任何惩罚。
//
// 背景（2026-09-24 实测）：oczen 是匿名渠道，整池只有一个虚拟账号且**不可重登**。
// 任何账号级惩罚（冷却冷却/错误计数/禁用）都等价于「整条渠道下线」——因为无号可轮换，
// 惩罚后 handler 会在挑号阶段直接返回 no_healthy_account(503)，而不是把上游原始错误
// 透传给客户端，连「稍后重试」都做不到。
//
// 历史缺陷（本测试锁定）：
//  1. 429 → ErrSoftRate → Cooldown(SoftCooldown)：文档声称「429 是唯一需要的背压」，
//     但单账号渠道冷却 = 下线：第 2 个请求直接被挡成 503 no_healthy_account；
//  2. 传输层错误 → NoteError：网络抖动累计 ErrThreshold(默认 3) 次即冷却唯一账号。
//
// 正确行为：SingleAccount 渠道对所有错误一律原文透传（不冷却、不计数、不禁用）。
package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/oczen"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// codeUpstream 假上游：按指定 HTTP 状态码返回错误，或模拟传输层错误。
type codeUpstream struct {
	status   int
	terr     bool
	calls    int // 实际发出的上游请求数（用于区分「透传」与「被挑号阶段提前拒绝」）
	classify func(int, string) provider.ErrKind
}

func (u *codeUpstream) RefreshToken(*auth.Auth) error { return nil }
func (u *codeUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) {
	return nil, nil
}
func (u *codeUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (u *codeUpstream) UserResource(*auth.Auth) (int64, error) { return 0, nil }
func (u *codeUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (u *codeUpstream) DailyCheckin(*auth.Auth) error { return nil }
func (u *codeUpstream) Classify(st int, body string) provider.ErrKind {
	if u.classify != nil {
		return u.classify(st, body)
	}
	return (&oczen.Client{}).Classify(st, body)
}
func (u *codeUpstream) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	u.calls++
	if u.terr {
		return nil, 0, nil, errors.New("stub transport error")
	}
	if u.status >= 400 {
		return nil, u.status, []byte(`{"error":{"message":"upstream original body"}}`), nil
	}
	return io.NopCloser(strings.NewReader("data: [DONE]\n\n")), u.status, nil, nil
}
func (u *codeUpstream) Stream(http.ResponseWriter, io.Reader, string) (map[string]any, error) {
	return nil, nil
}
func (u *codeUpstream) Aggregate(io.Reader, string) (map[string]any, error) {
	return map[string]any{"choices": []any{}}, nil
}

// newSingleAccountHandler 装配一个「单账号渠道」handler（ErrThreshold=1 让任何计数立即冷却，
// 以便测试能捕捉到「哪怕计一次错」的回归）。
func newSingleAccountHandler(t *testing.T, up *codeUpstream) (*Handler, *pool.Pool) {
	t.Helper()
	p := pool.New(t.TempDir() + "/state.json")
	p.Add(oczen.AnonymousAuth())
	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.Oczen: {Kind: provider.Oczen, Pool: p, Upstream: up, SingleAccount: true},
		},
		APIKey: "k", MaxRotate: 3,
		HardCooldown: time.Hour, SoftCooldown: time.Minute,
		ErrThreshold: 1, ErrCooldown: time.Minute,
	})
	return h, p
}

// call 发一次 chat 请求，返回响应记录。
func call(h *Handler) *httptest.ResponseRecorder {
	body := `{"model":"oczen/big-pickle","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestSingleAccountNoCooldownOnAnyStatus 遍历错误码，断言唯一账号不被冷却/禁用，
// 且**上游原始错误被透传**（而非被 no_healthy_account 掩盖）。
func TestSingleAccountNoCooldownOnAnyStatus(t *testing.T) {
	for _, code := range []int{400, 401, 402, 403, 404, 409, 429, 500, 502, 503} {
		up := &codeUpstream{status: code}
		h, p := newSingleAccountHandler(t, up)
		rec := call(h)

		st, _ := p.Status(oczen.AnonymousUID)
		if st.Cooling || st.Disabled {
			t.Errorf("status=%d: 唯一账号被惩罚（cooling=%t disabled=%t）——单账号渠道不应冷却",
				code, st.Cooling, st.Disabled)
		}
		if st.ErrCount != 0 {
			t.Errorf("status=%d: errCount=%d，不应累计错误计数", code, st.ErrCount)
		}
		if rec.Code != code {
			t.Errorf("status=%d: 响应码 %d，应透传上游状态码", code, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "upstream original body") {
			t.Errorf("status=%d: 响应体未透传上游原文: %s", code, rec.Body.String())
		}
	}
}

// TestSingleAccount429NotCooled_SecondRequestStillReachesUpstream 锁定最关键的行为：
// 单账号渠道遇到 429 后，下一个请求仍须真正发到上游（而非被挑号阶段挡成 503）。
// 旧实现：第 1 次 429 → 冷却 → 第 2 次直接 503 no_healthy_account（upstream_calls 不增）。
func TestSingleAccount429NotCooled_SecondRequestStillReachesUpstream(t *testing.T) {
	up := &codeUpstream{status: http.StatusTooManyRequests}
	h, p := newSingleAccountHandler(t, up)

	first := call(h)
	if first.Code != http.StatusTooManyRequests {
		t.Fatalf("第 1 次应为 429 透传，实际 %d：%s", first.Code, first.Body.String())
	}

	second := call(h)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("第 2 次应为 429 透传（不冷却），实际 %d：%s", second.Code, second.Body.String())
	}
	if up.calls != 2 {
		t.Fatalf("上游实际收到 %d 次请求，want 2（冷却会让第 2 次被挑号阶段挡掉）", up.calls)
	}
	if st, _ := p.Status(oczen.AnonymousUID); st.Cooling {
		t.Fatal("429 后账号被冷却——单账号渠道冷却即整条渠道下线")
	}
}

// TestSingleAccountTransportErrorNeverCools 锁定传输层错误路径：
// 网络抖动（连接失败/超时）反复发生也不得冷却唯一账号。
func TestSingleAccountTransportErrorNeverCools(t *testing.T) {
	up := &codeUpstream{terr: true}
	h, p := newSingleAccountHandler(t, up)

	for i := 1; i <= 4; i++ {
		if rec := call(h); rec.Code != http.StatusBadGateway {
			t.Fatalf("第 %d 次传输层错误应返回 502，实际 %d：%s", i, rec.Code, rec.Body.String())
		}
		if st, _ := p.Status(oczen.AnonymousUID); st.Cooling || st.ErrCount != 0 {
			t.Fatalf("第 %d 次后账号被惩罚（cooling=%t errCount=%d）——传输层抖动不应罚单账号",
				i, st.Cooling, st.ErrCount)
		}
	}
}

// TestClearPenaltyHealsLegacyState 锁定启动自愈：旧版本可能已在 state 文件里
// 留下冷却/错误计数，ClearPenalty 必须把它们清掉（否则升级后渠道仍然下线）。
func TestClearPenaltyHealsLegacyState(t *testing.T) {
	p := pool.New(t.TempDir() + "/state.json")
	p.Add(oczen.AnonymousAuth())
	// 模拟旧版本写下的脏状态：冷却 1 小时 + 错误计数
	p.Cooldown(oczen.AnonymousUID, pool.CoolSoft, time.Hour, "429 rate limit")
	p.NoteError(oczen.AnonymousUID, 1, time.Hour)
	if st, _ := p.Status(oczen.AnonymousUID); !st.Cooling {
		t.Fatal("前置条件失败：账号应处于冷却中")
	}

	p.ClearPenalty(oczen.AnonymousUID)

	st, _ := p.Status(oczen.AnonymousUID)
	if st.Cooling || st.ErrCount != 0 || st.Reason != "" {
		t.Fatalf("ClearPenalty 后仍有惩罚残留：cooling=%t errCount=%d reason=%q",
			st.Cooling, st.ErrCount, st.Reason)
	}
	// 必须真的能挑出账号（否则渠道仍然下线）
	if p.Pick() == nil {
		t.Fatal("ClearPenalty 后仍挑不出账号——渠道依旧不可用")
	}
}
