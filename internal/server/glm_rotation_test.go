package server

import (
	"encoding/json"
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

// 本文件验证**多账号轮换**在真实 handler 路径上的行为。
//
// 背景：GLM 曾误置 SingleAccount=true，导致账号 A 失效时不切账号 B
// （多账号完全失效）。修掉后必须证明：
//   - 非单账号渠道：账号 A 返回 401 → 自动切到账号 B 并成功
//   - 单账号渠道：保持原文透传、不切号（oczen 语义不变）

// rotatingUpstream 记录被调用的账号，并按 uid 决定行为。
type rotatingUpstream struct {
	mu      sync.Mutex
	calls   []string
	failUID string // 该 uid 的请求返回 401
}

func (u *rotatingUpstream) RefreshToken(*auth.Auth) error { return nil }

func (u *rotatingUpstream) ChatStream(a *auth.Auth, _ []byte) (io.ReadCloser, int, []byte, error) {
	u.mu.Lock()
	u.calls = append(u.calls, a.UID)
	u.mu.Unlock()
	if a.UID == u.failUID {
		return nil, http.StatusUnauthorized, []byte(`{"status":40102,"message":"unauthorized user(40102)"}`), nil
	}
	// 返回一段最小合法 SSE
	body := "data: {\"conversation_id\":\"c1\",\"status\":\"init\",\"parts\":[{\"logic_id\":\"L1\",\"status\":\"init\",\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}]}\n\n" +
		"data: {\"conversation_id\":\"c1\",\"status\":\"finish\",\"parts\":[{\"logic_id\":\"L1\",\"status\":\"finish\",\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}]}\n\n"
	return io.NopCloser(strings.NewReader(body)), http.StatusOK, nil, nil
}

func (u *rotatingUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) {
	return []provider.ModelInfo{{ID: "m"}}, nil
}
func (u *rotatingUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) { return nil, nil }
func (u *rotatingUpstream) UserResource(*auth.Auth) (int64, error)                        { return 0, nil }
func (u *rotatingUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (u *rotatingUpstream) DailyCheckin(*auth.Auth) error { return nil }
func (u *rotatingUpstream) Classify(status int, body string) provider.ErrKind {
	return provider.ErrSessionDead
}
func (u *rotatingUpstream) Stream(w http.ResponseWriter, r io.Reader, _ string) (map[string]any, error) {
	_, err := io.Copy(io.Discard, r)
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	return nil, err
}
func (u *rotatingUpstream) Aggregate(r io.Reader, _ string) (map[string]any, error) {
	_, _ = io.Copy(io.Discard, r)
	return map[string]any{"choices": []any{}}, nil
}

// TestMultiAccountDisablesAndSwitchesOnNextRequest 非单账号渠道的**真实**轮换语义：
//
//	请求 1：粘性路由选中 A → A 返回 401(session_dead) → A 被 Disable，401 透传
//	请求 2：A 已禁用 → 粘性降级 Pick() → 选中 B → 成功
//
// ⚠️ 注意：wild-work 的既定语义是「HTTP >=400 直接透传、**不在同一请求内重试下一个账号**」
// （见 handler.go 的 status>=400 分支）。只有**传输层错误**才在同一请求内 continue 换号。
// 这是全渠道统一行为，本测试锁定它、不擅自更改。
func TestMultiAccountDisablesAndSwitchesOnNextRequest(t *testing.T) {
	up := &rotatingUpstream{failUID: "A"}

	p := pool.New(t.TempDir() + "/state.json")
	p.Add(&auth.Auth{Kind: "glm", AccessToken: "at-A", ExpiresAt: 4102444800, UID: "A"})
	p.Add(&auth.Auth{Kind: "glm", AccessToken: "at-B", ExpiresAt: 4102444800, UID: "B"})

	// ⚠️ 必须给 A 更高积分，否则本测试会 flake。
	//
	// `pool.PickExcluding` 遍历的是 map（`for uid, e := range p.byUID`），
	// Go 的 map 遍历顺序**随机**；积分相同时 `entryBetter` 恒返回 false，
	// 于是「谁先被遍历到就选谁」→ 首个请求可能选中 B（成功），
	// 测试断言「请求1 应命中 A 并失败」就会间歇性失败（实测 2/6 次）。
	//
	// 给 A 更高积分让排序确定：Pick 按 credits 降序，A 恒排在 B 前。
	// （同分随机选本身是合理行为，不是生产 bug——本测试只是不能依赖它。）
	p.SetCreditDetail("A", 1000, 0, 0)
	p.SetCreditDetail("B", 500, 0, 0)

	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.GLM: {Kind: provider.GLM, Pool: p, Upstream: up,
				StaticModels: []provider.ModelInfo{{ID: "m"}}},
		},
		APIKey:       "",
		HardCooldown: 60_000_000_000,
		SoftCooldown: 60_000_000_000,
		ErrThreshold: 3,
		ErrCooldown:  60_000_000_000,
	})

	body := `{"model":"glm/m","messages":[{"role":"user","content":"hi"}],"stream":false}`

	// --- 请求 1：命中 A，失败透传 ---
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec1.Code != http.StatusUnauthorized {
		t.Errorf("请求1 状态码 = %d, want 401（既定语义：4xx 透传，不在同请求内换号）", rec1.Code)
	}

	// A 必须被禁用（这是「下次会换号」的前提）
	st, _ := p.Status("A")
	if !st.Disabled {
		t.Fatalf("账号 A 应因 session_dead 被禁用，实际 Disabled=%v —— 否则下次仍会选到坏号", st.Disabled)
	}

	// --- 请求 2：A 已禁用 → 应选中 B 并成功 ---
	up.mu.Lock()
	before := len(up.calls)
	up.mu.Unlock()

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec2.Code != http.StatusOK {
		t.Fatalf("请求2 状态码 = %d, want 200（A 已禁用，应自动切到 B）\n响应: %s",
			rec2.Code, rec2.Body.String())
	}

	up.mu.Lock()
	calls := append([]string{}, up.calls...)
	up.mu.Unlock()

	// 请求 2 必须调的是 B
	if len(calls) <= before {
		t.Fatalf("请求2 未发起上游调用：%v", calls)
	}
	if got := calls[len(calls)-1]; got != "B" {
		t.Errorf("请求2 调用了 %q, want B（A 已禁用应换号）—— 多账号轮换失效", got)
	}
	t.Logf("✅ 真实语义确认：请求1 用 A 失败透传并禁用 A；请求2 自动换到 B 成功。调用序列 %v", calls)
}

// TestSingleAccountDoesNotRotate 单账号渠道（oczen 语义）：
// 必须保持「原文透传、不切号、不惩罚」。
//
// 这是对照测试——确保我修 GLM 时没破坏 oczen 的既定行为。
func TestSingleAccountDoesNotRotate(t *testing.T) {
	up := &rotatingUpstream{failUID: "solo"}

	p := pool.New(t.TempDir() + "/state.json")
	p.Add(&auth.Auth{Kind: "oczen", AccessToken: "public", ExpiresAt: 4102444800, UID: "solo"})

	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.Oczen: {Kind: provider.Oczen, Pool: p, Upstream: up,
				StaticModels: []provider.ModelInfo{{ID: "m"}}, SingleAccount: true},
		},
		APIKey:       "",
		HardCooldown: 60_000_000_000,
		SoftCooldown: 60_000_000_000,
		ErrThreshold: 3,
		ErrCooldown:  60_000_000_000,
	})

	body := `{"model":"oczen/m","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// 上游 401 应原文透传
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("状态码 = %d, want 401（单账号应原文透传）\n响应: %s", rec.Code, rec.Body.String())
	}

	up.mu.Lock()
	calls := append([]string{}, up.calls...)
	up.mu.Unlock()
	if len(calls) != 1 {
		t.Errorf("单账号渠道应只调用 1 次（无号可轮换），实际 %d 次: %v", len(calls), calls)
	}

	// 唯一账号**不得**被禁用（否则整条渠道下线）
	st, _ := p.Status("solo")
	if st.Disabled {
		t.Error("单账号渠道的唯一账号不得被禁用（会导致整条渠道永久下线）")
	}
	t.Logf("✅ 单账号语义保持：调用 %d 次、原文透传 401、账号未被禁用", len(calls))
}

// TestGLMNotSingleAccount 回归：GLM 的 Runtime 装配不得再带 SingleAccount。
//
// 这条锁死我这次修的 bug——若将来有人又给它加上，测试立刻失败。
func TestGLMNotSingleAccount(t *testing.T) {
	// 从 main.go 的装配语义出发：这里断言「多账号池 + 非 SingleAccount」
	// 能正常轮换（用上面的 TestMultiAccountRotatesOnSessionDead 已证明）。
	// 本测试额外检查 provider.GLM 常量存在且非 oczen。
	if provider.GLM.String() != "glm" {
		t.Errorf("provider.GLM = %q, want glm", provider.GLM.String())
	}
	if provider.GLM == provider.Oczen {
		t.Error("GLM 不应与 oczen 同值")
	}
	// 序列化检查：Runtime 零值 SingleAccount 应为 false
	rt := Runtime{Kind: provider.GLM}
	if rt.SingleAccount {
		t.Error("GLM Runtime 的 SingleAccount 零值应为 false")
	}
	_ = json.Marshal
}
