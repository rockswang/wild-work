package login_doubao

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// setPassportBase 把扫码流程的整个基点指到本地测试服务，返回恢复函数。
//
// 为什么必须重定向：否则 newPassportFlow/requestQR/pollQR 会打到真上游
// （§0.1：不拿真上游做实验）—— 2026-11-15 就漏过这一次：改了流程却没改测试的
// 覆盖点，结果 go test 真去请求了 doubao 的 passport。
func setPassportBase(t *testing.T, base string) func() {
	t.Helper()
	old := passportBase
	passportBase = base
	return func() { passportBase = old }
}

// TestNormalizeStatus 状态值归一化（上游返回字符串/数字字符串两种形态）。
func TestNormalizeStatus(t *testing.T) {
	cases := map[string]string{
		"new": "waiting", "1": "waiting", "0": "waiting",
		"scanning": "scanning", "scanned": "scanning", "2": "scanning",
		"confirmed": "confirmed", "success": "confirmed", "3": "confirmed",
		"expired": "expired", "4": "expired",
		"canceled": "cancelled", "8": "cancelled",
		"garbage": "",
	}
	for in, want := range cases {
		if got := normalizeStatus(in); got != want {
			t.Errorf("normalizeStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCollectCookies Set-Cookie 要拼成请求头格式。
func TestCollectCookies(t *testing.T) {
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Add("Set-Cookie", "sessionid=abc; Path=/; Domain=.doubao.com")
	resp.Header.Add("Set-Cookie", "ttwid=def; Path=/")
	got := collectCookies(resp)
	if !strings.Contains(got, "sessionid=abc") || !strings.Contains(got, "ttwid=def") {
		t.Fatalf("collectCookies = %q", got)
	}
}

// TestQRFlowConfirmed 完整扫码流：申请 → scanning → confirmed（带 Set-Cookie）
// → 校验通过 → onFound 回调 → success。全程打本地 httptest，**不碰真上游**。
func TestQRFlowConfirmed(t *testing.T) {
	oldClient, oldVerify := qrHTTPClient, verifyCookie
	defer func() { qrHTTPClient, verifyCookie = oldClient, oldVerify }()

	var polls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			http.SetCookie(w, &http.Cookie{Name: "ttwid", Value: "tw-1", Path: "/"})
			_, _ = w.Write([]byte("<html>home</html>"))
		case strings.HasSuffix(r.URL.Path, "/csrf_token/"):
			if r.Header.Get("x-tt-passport-csrf-token") != "" {
				t.Errorf("取 CSRF 时不应带头")
			}
			_, _ = w.Write([]byte(`{"data":{"passport_csrf_token":"csrf-abc"}}`))
		case strings.HasSuffix(r.URL.Path, "/get_qrcode/"):
			if got := r.Header.Get("x-tt-passport-csrf-token"); got != "csrf-abc" {
				t.Errorf("申请二维码必须带 CSRF 头，实际 %q", got)
			}
			if got := r.URL.Query().Get("aid"); got != "497858" {
				t.Errorf("扫码 aid 必须是网页端 497858，实际 %q", got)
			}
			_, _ = w.Write([]byte(`{"data":{"error_code":0,"token":"tok-1","qrcode":"QUJDRA=="}}`))
		case strings.HasSuffix(r.URL.Path, "/check_qrconnect/"):
			if got := r.Header.Get("x-tt-passport-csrf-token"); got != "csrf-abc" {
				t.Errorf("轮询必须带 CSRF 头，实际 %q", got)
			}
			if r.URL.Query().Get("token") != "tok-1" {
				t.Errorf("轮询必须带 token，实际 %q", r.URL.Query().Get("token"))
			}
			polls++
			if polls < 2 {
				_, _ = w.Write([]byte(`{"data":{"status":"scanning"}}`))
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: "sess-xyz", Path: "/"})
			_, _ = w.Write([]byte(`{"data":{"status":"confirmed","redirect_url":"http://` + r.Host + `/login/callback"}}`))
		case strings.HasSuffix(r.URL.Path, "/login/callback"):
			// 确认后的重定向链：sessionid 在这条链上才下发
			http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: "sess-xyz", Path: "/"})
			_, _ = w.Write([]byte("ok"))
		default:
			t.Errorf("意外路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	qrHTTPClient = srv.Client()
	verifyCookie = func(cookie string) error {
		if !strings.Contains(cookie, "sessionid=sess-xyz") {
			t.Errorf("校验拿到的 Cookie 不对：%q", cookie)
		}
		return nil
	}
	_ = polls
	defer setPassportBase(t, srv.URL)()

	var got Result
	sess, err := Start(func(r Result) error { got = r; return nil })
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer Shutdown()

	// 等它走完（轮询间隔 2s，最多等 12s）
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if sess.State() == StateSuccess {
			break
		}
		if sess.State() == StateFailed {
			t.Fatalf("会话失败：%v", sess.Snapshot())
		}
		time.Sleep(200 * time.Millisecond)
	}
	if sess.State() != StateSuccess {
		t.Fatalf("应到达 success，实际 %v", sess.Snapshot())
	}
	if !strings.Contains(got.Cookie, "sessionid=sess-xyz") {
		t.Fatalf("回调拿到的 Cookie 不对：%q", got.Cookie)
	}
	if got.UID == "" {
		t.Fatal("uid 不应为空")
	}
	snap := sess.Snapshot()
	if snap["qr"] == "" {
		t.Fatal("二维码图片不应为空")
	}
}

// TestQRFlowVerifyFail 校验不过必须转 failed，且**不得**回调 onFound。
func TestQRFlowVerifyFail(t *testing.T) {
	oldClient, oldVerify := qrHTTPClient, verifyCookie
	defer func() { qrHTTPClient, verifyCookie = oldClient, oldVerify }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			_, _ = w.Write([]byte("<html>home</html>"))
		case strings.HasSuffix(r.URL.Path, "/csrf_token/"):
			_, _ = w.Write([]byte(`{"data":{"passport_csrf_token":"csrf-abc"}}`))
		case strings.HasSuffix(r.URL.Path, "/get_qrcode/"):
			_, _ = w.Write([]byte(`{"data":{"error_code":0,"token":"tok-2","qrcode":"QUJDRA=="}}`))
		case strings.HasSuffix(r.URL.Path, "/check_qrconnect/"):
			http.SetCookie(w, &http.Cookie{Name: "sessionid", Value: "stale", Path: "/"})
			_, _ = w.Write([]byte(`{"data":{"status":"confirmed"}}`))
		default:
			t.Errorf("意外路径：%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	qrHTTPClient = srv.Client()
	verifyCookie = func(string) error { return errString("登录态已过期") }

	defer setPassportBase(t, srv.URL)()

	called := false
	sess, err := Start(func(Result) error { called = true; return nil })
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer Shutdown()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if sess.State() == StateFailed || sess.State() == StateSuccess {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if sess.State() != StateFailed {
		t.Fatalf("校验失败必须转 failed，实际 %v", sess.Snapshot())
	}
	if called {
		t.Fatal("校验不过不得回调 onFound")
	}
}

type errString string

func (e errString) Error() string { return string(e) }
