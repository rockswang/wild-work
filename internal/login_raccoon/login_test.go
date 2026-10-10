package login_raccoon

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"wild-work/internal/raccoon"
)

// fakeJWT 造一个仅含 payload 的假 JWT（本包只解 payload，不验签）。
func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, _ := json.Marshal(claims)
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// TestJwtOwnerType 区分真实账号（users）与访客（visitors）——这是「Cookie 出现 ≠ 登录完成」
// 的判据：小浣熊对未登录访客也会下发 raccoon_refresh_token，必须靠 owner_type 甄别，
// 否则会抓到一个不可用的访客凭据（对齐智谱清言 R28 的教训）。
func TestJwtOwnerType(t *testing.T) {
	cases := []struct {
		name string
		tok  string
		want string
	}{
		{"真实账号", fakeJWT(t, map[string]any{"owner_type": "users", "name": "RaccoonAiden"}), "users"},
		{"访客", fakeJWT(t, map[string]any{"owner_type": "visitors", "name": ""}), "visitors"},
		{"无字段", fakeJWT(t, map[string]any{"name": "x"}), ""},
		{"非法", "not-a-jwt", ""},
		{"空", "", ""},
	}
	for _, c := range cases {
		if got := jwtOwnerType(c.tok); got != c.want {
			t.Errorf("%s: jwtOwnerType 期望 %q 实际 %q", c.name, c.want, got)
		}
	}
}

// TestJwtName 取 uid（name claim）。
func TestJwtName(t *testing.T) {
	tok := fakeJWT(t, map[string]any{"owner_type": "users", "name": "RaccoonAiden"})
	if got := jwtName(tok); got != "RaccoonAiden" {
		t.Fatalf("jwtName 期望 RaccoonAiden 实际 %q", got)
	}
	for _, bad := range []string{"", "not-a-jwt", "a.b"} {
		if got := jwtName(bad); got != "" {
			t.Fatalf("jwtName(%q) 期望空 实际 %q", bad, got)
		}
	}
}

// TestJwtExp 覆盖 exp 解析（含非法输入不得 panic）。
func TestJwtExp(t *testing.T) {
	if got := jwtExp(fakeJWT(t, map[string]any{"exp": float64(123456)})); got != 123456 {
		t.Fatalf("jwtExp 期望 123456 实际 %d", got)
	}
	for _, bad := range []string{"", "not-a-jwt", "a.b", "a.!!!.c"} {
		if got := jwtExp(bad); got != 0 {
			t.Fatalf("jwtExp(%q) 期望 0 实际 %d", bad, got)
		}
	}
}

// TestWebLoginURL 登录入口必须指向网页版登录页（带 loginModal=true 直接弹登录框）。
func TestWebLoginURL(t *testing.T) {
	if WebLoginURL != "https://office.xiaohuanxiong.com/home?loginModal=true" {
		t.Fatalf("WebLoginURL 不符：%s", WebLoginURL)
	}
}

// TestCookieConstants Cookie 名与域必须与实盘抓包一致
// （2026-10-10 抓包：raccoon_refresh_token 落在 .xiaohuanxiong.com 域）。
func TestCookieConstants(t *testing.T) {
	if CookieName != "raccoon_refresh_token" {
		t.Fatalf("CookieName 不符：%s", CookieName)
	}
	if CookieDomain != "xiaohuanxiong.com" {
		t.Fatalf("CookieDomain 不符：%s", CookieDomain)
	}
}

// TestPollWithoutSession 无会话时 Poll 报错而非 panic。
func TestPollWithoutSession(t *testing.T) {
	mu.Lock()
	old := current
	current = nil
	mu.Unlock()
	defer func() { mu.Lock(); current = old; mu.Unlock() }()

	if _, err := Poll(); err != ErrPending {
		t.Fatalf("无会话时 Poll 应返回 ErrPending（取消由上层 ctx 分支处理），实际 %v", err)
	}
}

// TestPollSessionStates 覆盖 Poll 对各状态的映射（不启动真实浏览器）。
func TestPollSessionStates(t *testing.T) {
	mk := func(status string, err error, res Result) *WebSession {
		s := &WebSession{status: status, err: err}
		if status == "success" {
			s.result = res
		}
		return s
	}
	cases := []struct {
		name    string
		sess    *WebSession
		wantErr bool
		wantUID string
	}{
		{"pending", mk("pending", nil, Result{}), true, ""},
		{"success", mk("success", nil, Result{AccessToken: "at", RefreshToken: "rt"}), false, ""},
		{"failed", mk("failed", errStr("浏览器未找到"), Result{}), true, ""},
		{"cancelled", mk("cancelled", nil, Result{}), true, ""},
	}
	for _, c := range cases {
		mu.Lock()
		current = c.sess
		mu.Unlock()
		r, err := Poll()
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: 期望错误，实际成功 %+v", c.name, r)
			}
			if c.name == "pending" && err != ErrPending {
				t.Errorf("%s: 期望 ErrPending 实际 %v", c.name, err)
			}
		} else {
			if err != nil {
				t.Errorf("%s: 期望成功，实际 %v", c.name, err)
			}
			if r.RefreshToken != "rt" {
				t.Errorf("%s: 凭据不符 %+v", c.name, r)
			}
		}
	}
	mu.Lock()
	current = nil
	mu.Unlock()
}

// TestStartRejectsConcurrentLogin 已有 pending 会话时拒绝再次发起。
func TestStartRejectsConcurrentLogin(t *testing.T) {
	mu.Lock()
	current = &WebSession{status: "pending"}
	mu.Unlock()
	defer func() { mu.Lock(); current = nil; mu.Unlock() }()

	if _, err := Start(); err == nil {
		t.Fatal("已有 pending 会话时应拒绝并发登录")
	}
}

// TestShutdownIdempotent 无会话时 Shutdown 必须静默成功（幂等）。
func TestShutdownIdempotent(t *testing.T) {
	mu.Lock()
	current = nil
	mu.Unlock()
	Shutdown()
	Shutdown()
}

// TestRefreshWithTokenEmpty 空 refresh_token 应立刻报错，不发请求、不 panic。
func TestRefreshWithTokenEmpty(t *testing.T) {
	if _, _, err := raccoon.New().RefreshWithToken(""); err == nil {
		t.Fatal("空 refresh_token 应报错")
	}
}

type errStr string

func (e errStr) Error() string { return string(e) }
