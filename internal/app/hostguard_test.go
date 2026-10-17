// hostguard_test.go 面板 Host 校验的回归（2026-10-17 DNS rebinding 修复）。
//
// 这些用例锁两件事：
//  1. 未启用管理密码时，非环回 Host 一律 421，且**不得**到达内层 handler；
//  2. 已启用管理密码时不做 Host 限制（局域网 / 反代访问照常）。
package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"wild-work/internal/config"
)

// newHostGuardApp 构造只用于 Host 校验测试的最小 App。
// ⚠️ 不能用 &App{}：App.cfg 是 *config.Config，零值 nil 会在 panelAuthEnabled 里 nil deref。
func newHostGuardApp(adminPass string) *App {
	return &App{cfg: &config.Config{AdminPass: adminPass}}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		// 环回：整个 127/8 + localhost + ::1
		{"127.0.0.1", true},
		{"127.0.0.1:7863", true},
		{"127.1.2.3:80", true},
		{"localhost", true},
		{"LOCALHOST:7863", true},
		{" 127.0.0.1 ", true},
		{"::1", true},
		{"[::1]", true},
		{"[::1]:7863", true},
		{"[::ffff:127.0.0.1]", true}, // IPv4-mapped 环回
		// 非环回：外域名、私网、自定义 hosts 名、畸形、空
		{"", false},
		{"evil.com", false},
		{"evil.com:7863", false},
		{"192.168.1.5:7863", false},
		{"10.0.0.7", false},
		{"wildwork.local", false},
		{"localhost.evil.com", false},   // 前缀伪装
		{"127.0.0.1.evil.com", false},   // 后缀伪装
		{"not-a-host", false},
	}
	for _, tc := range cases {
		if got := isLoopbackHost(tc.host); got != tc.want {
			t.Errorf("isLoopbackHost(%q) = %v，want %v", tc.host, got, tc.want)
		}
	}
}

// TestHostGuardRejectsNonLoopbackWithoutPassword 未启用管理密码时，非环回 Host 必须被 421 拦下。
func TestHostGuardRejectsNonLoopbackWithoutPassword(t *testing.T) {
	a := newHostGuardApp("") // 零值 cfg ⇒ AdminPass 为空 = 未启用面板鉴权（默认部署形态）
	reached := false
	h := a.hostGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))

	for _, host := range []string{"evil.com", "evil.com:7863", "192.168.1.5:7863", "wildwork.local", ""} {
		reached = false
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
		req.Host = host
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("Host=%q 应被拒（421），实际 %d", host, rec.Code)
		}
		if reached {
			t.Errorf("Host=%q 被拒后仍到达内层 handler —— 校验位置不对", host)
		}
	}
}

// TestHostGuardAllowsLoopbackWithoutPassword 环回 Host 正常放行。
func TestHostGuardAllowsLoopbackWithoutPassword(t *testing.T) {
	a := newHostGuardApp("")
	h := a.hostGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, host := range []string{"127.0.0.1:7863", "localhost:7863", "localhost", "[::1]:7863"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
		req.Host = host
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Host=%q 应放行，实际 %d", host, rec.Code)
		}
	}
}

// TestHostGuardSkipsCheckWhenPasswordEnabled 已设管理密码时不做 Host 限制
// （局域网 / 反向代理访问是合法用法，密码才是那条路径的保护）。
func TestHostGuardSkipsCheckWhenPasswordEnabled(t *testing.T) {
	a := newHostGuardApp("")
	a.cfg.AdminPass = "pw12345678"
	h := a.hostGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, host := range []string{"evil.com:7863", "192.168.1.5:7863", "wildwork.local"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/state", nil)
		req.Host = host
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("已启用密码时 Host=%q 应放行（由密码保护），实际 %d", host, rec.Code)
		}
	}
}

// TestHostGuardNilInnerIsNil hostGuard(nil) 必须返回 nil：serveHandler 用 typed-nil 判断
// 决定是否让 http.Server 起来，包一层 handlerFunc 会把 nil 变成非 nil 接口，导致
// 「本该报错的启动」变成「服务起来了但每个请求 panic」。
func TestHostGuardNilInnerIsNil(t *testing.T) {
	a := newHostGuardApp("")
	if h := a.hostGuard(nil); h != nil {
		t.Fatal("hostGuard(nil) 应返回 nil")
	}
}
