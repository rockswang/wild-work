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

// 本文件锁定「上游 401 → 先试 refresh → 成功则不罚账号并重试本号」这一闭环。
//
// 背景（2026-10-17 现场）：两个 WorkBuddy 账号的 refresh token 被上游吊销后，
// 账单/目录/对话接口一律回网关 HTML 401。修复前 `ErrSessionDead` 分支**直接**
// `pool.Disable`：只要本地 expiresAt 还没到（token 在别处被轮换、或上次刷新未落盘），
// 一个完全能救回来的账号就被永久停用，需人工重登。

// sessionDeadUpstream 第一次返回 401（session dead），refresh 后恢复成功。
type sessionDeadUpstream struct {
	mu           sync.Mutex
	calls        []string
	refreshCalls int
	refreshErr   error
	deadLeft     int // 还需返回几次 401
}

func (u *sessionDeadUpstream) RefreshToken(a *auth.Auth) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.refreshCalls++
	if u.refreshErr != nil {
		return u.refreshErr
	}
	a.AccessToken = "at-renewed"
	return nil
}

func (u *sessionDeadUpstream) ChatStream(a *auth.Auth, _ []byte) (io.ReadCloser, int, []byte, error) {
	u.mu.Lock()
	u.calls = append(u.calls, a.UID)
	dead := u.deadLeft > 0
	if dead {
		u.deadLeft--
	}
	u.mu.Unlock()
	if dead {
		return nil, http.StatusUnauthorized, []byte(`<html><head><title>401 Authorization Required</title></head><body><center><h1>401 Authorization Required</h1></center><hr><center>openresty</center></body></html>`), nil
	}
	return io.NopCloser(strings.NewReader("data: [DONE]\n\n")), http.StatusOK, nil, nil
}

func (u *sessionDeadUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) {
	return []provider.ModelInfo{{ID: "m"}}, nil
}
func (u *sessionDeadUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (u *sessionDeadUpstream) UserResource(*auth.Auth) (int64, error) { return 0, nil }
func (u *sessionDeadUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (u *sessionDeadUpstream) DailyCheckin(*auth.Auth) error { return nil }
func (u *sessionDeadUpstream) Classify(status int, body string) provider.ErrKind {
	if status == http.StatusOK {
		return provider.ErrNone
	}
	// 生产同款判据：网关 HTML 401 → session dead（见 upstream/gateway.go）
	return provider.ErrSessionDead
}
func (u *sessionDeadUpstream) Stream(w http.ResponseWriter, r io.Reader, _ string) (map[string]any, error) {
	_, err := io.Copy(io.Discard, r)
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	return nil, err
}
func (u *sessionDeadUpstream) Aggregate(r io.Reader, _ string) (map[string]any, error) {
	_, _ = io.Copy(io.Discard, r)
	return map[string]any{"choices": []any{}}, nil
}

func newSessionDeadHandler(t *testing.T, up *sessionDeadUpstream) (*Handler, *pool.Pool) {
	t.Helper()
	p := pool.New(t.TempDir() + "/state.json")
	p.Add(&auth.Auth{Kind: "workbuddy", UID: "A", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 4102444800})
	return NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: p, Upstream: up,
				StaticModels: []provider.ModelInfo{{ID: "m"}}},
		},
		HardCooldown: 60 * time.Second,
		SoftCooldown: 60 * time.Second,
		ErrThreshold: 3,
		ErrCooldown:  60 * time.Second,
		MaxRotate:    3,
	}), p
}

// TestSessionDeadRefreshesInsteadOfDisabling refresh 能救回来时**不得**禁用账号。
func TestSessionDeadRefreshesInsteadOfDisabling(t *testing.T) {
	up := &sessionDeadUpstream{deadLeft: 1}
	h, p := newSessionDeadHandler(t, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"workbuddy/m","messages":[{"role":"user","content":"hi"}],"stream":false}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码=%d want 200（refresh 成功后应重试本号成功）\n响应: %s", rec.Code, rec.Body.String())
	}
	st, ok := p.Status("A")
	if !ok {
		t.Fatal("账号应从池中查到")
	}
	if st.Disabled {
		t.Fatal("refresh 成功时不得 Disable 账号（本地 token 只是被别处轮换）")
	}
	if up.refreshCalls != 1 {
		t.Fatalf("refresh 调用次数=%d want 1", up.refreshCalls)
	}
	if len(up.calls) != 2 {
		t.Fatalf("上游应被调用 2 次（401 一次 + 重试一次），实际 %d 次: %v", len(up.calls), up.calls)
	}
	if got := p.AuthByUID("A").AccessTokenValue(); got != "at-renewed" {
		t.Fatalf("刷新后的 token 应写回池内账号，实得 %q", got)
	}
}

// TestSessionDeadMarkedWhenRefreshFails refresh 也失败（会话真被吊销）才落 SessionDead 标记。
//
// 注意「标记」而非「Disable」：R76 把机器状态（登录失效）与用户控制项（停用）分离——
// 标记会被挑号跳过、面板显示「需重新登录」，且重登后按 auth 文件 mtime 自动清除。
func TestSessionDeadDisablesWhenRefreshFails(t *testing.T) {
	up := &sessionDeadUpstream{deadLeft: 99, refreshErr: &provider.Error{
		Kind: provider.ErrSessionDead, Status: 401,
		Msg: "12153: invalid_grant: Session doesn't have required client",
	}}
	h, p := newSessionDeadHandler(t, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"workbuddy/m","messages":[{"role":"user","content":"hi"}],"stream":false}`)))

	if rec.Code == http.StatusOK {
		t.Fatalf("会话已吊销时不应报成功: %s", rec.Body.String())
	}
	st, _ := p.Status("A")
	if !st.SessionDead {
		t.Fatal("refresh 失败后应落 SessionDead 标记（需人工重登）")
	}
	if st.Disabled {
		t.Fatal("登录失效不得混入用户停用开关（R76）")
	}
	if st.SessionDeadReason == "" {
		t.Fatal("标记应带原因，面板据此显示「需重新登录」")
	}
	if up.refreshCalls != 1 {
		t.Fatalf("refresh 只应尝试一次，实际 %d 次", up.refreshCalls)
	}
}

// TestSessionDeadNoRefreshTokenMarked 无 refresh token 时不能白试，直接落标记。
func TestSessionDeadNoRefreshTokenDisables(t *testing.T) {
	up := &sessionDeadUpstream{deadLeft: 99}
	h, p := newSessionDeadHandler(t, up)
	// 抹掉 refresh token（模拟手填只有 access token 的账号）
	if au := p.AuthByUID("A"); au != nil {
		au.RefreshToken = ""
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"workbuddy/m","messages":[{"role":"user","content":"hi"}],"stream":false}`)))

	if up.refreshCalls != 0 {
		t.Fatalf("无 refresh token 时不应调用 refresh，实际 %d 次", up.refreshCalls)
	}
	if st, _ := p.Status("A"); !st.SessionDead {
		t.Fatal("无 refresh token 且 401 时应落 SessionDead 标记")
	}
}
