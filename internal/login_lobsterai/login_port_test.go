// login_port_test.go 固定回调端口的契约测试（零真实网络、零上游请求）。
//
// 为什么需要固定端口：portal 的回调派发器只认 `http://127.0.0.1:<port>/auth/callback`
// 这一种形态（hostname 必须严格等于 127.0.0.1，见 docs/决议项.md R97 的源码证据）。
// 于是「浏览器与网关不同机」时唯一的自动回调路径，是让浏览器侧也能连到
// `127.0.0.1:<port>` —— 即在那台机器上预先建端口转发。随机端口没法预建隧道，
// 故允许在配置里固定它。
//
// 本文件同时锁住一个已被证伪的方向：**不许再把 redirect_uri 的 host 换成 LAN IP**
// （2026-10-18 实测：portal 会丢弃它、改发 lobsterai:// deep-link，回调彻底收不到）。
package login_lobsterai

import (
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// freeLoopbackPort 取一个当前空闲的环回端口（拿完立刻释放）。
func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("找空闲端口失败: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// TestStartWithFixedPortUsesIt 固定端口被如实采用：state、redirect_uri、登录 URL 三处一致。
func TestStartWithFixedPortUsesIt(t *testing.T) {
	port := freeLoopbackPort(t)
	statePath := filepath.Join(t.TempDir(), "login-state.json")

	if _, err := StartWithPort(NewClient(), statePath, port); err != nil {
		t.Fatalf("StartWithPort(%d): %v", port, err)
	}
	t.Cleanup(Shutdown)

	st, err := readState(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if st.CallbackPort != port {
		t.Errorf("CallbackPort = %d，期望固定端口 %d", st.CallbackPort, port)
	}
	want := "http://127.0.0.1:" + strconv.Itoa(port) + callbackPath
	if st.RedirectURI != want {
		t.Fatalf("RedirectURI = %q，期望 %q", st.RedirectURI, want)
	}
	// 登录 URL 里内嵌的 redirect_uri 必须与落盘值逐字一致（编码后再解码比对）。
	if !strings.Contains(st.AuthURL, "redirect_uri=http%3A%2F%2F127.0.0.1%3A"+strconv.Itoa(port)) {
		t.Errorf("登录 URL 未内嵌固定端口的 redirect_uri: %s", st.AuthURL)
	}

	// 端口确实在听：错 state 的回调能得到 400（不依赖上游，只验证监听就位）。
	resp, err := http.Get(want + "?code=x&state=wrong")
	if err != nil {
		t.Fatalf("回调口未监听: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("错 state 应回 400，实际 %d", resp.StatusCode)
	}
}

// TestStartWithOccupiedPortFallsBackToRandom 固定端口被占时退回随机端口，
// 且 redirect_uri 跟随**实际**端口（否则浏览器会连到一个网关没在听的地址）。
func TestStartWithOccupiedPortFallsBackToRandom(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听失败: %v", err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port

	statePath := filepath.Join(t.TempDir(), "login-state.json")
	if _, err := StartWithPort(NewClient(), statePath, port); err != nil {
		t.Fatalf("StartWithPort(占用端口 %d): %v", port, err)
	}
	t.Cleanup(Shutdown)

	st, err := readState(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if st.CallbackPort == port {
		t.Errorf("端口被占时不应复用 %d", port)
	}
	if want := "http://127.0.0.1:" + strconv.Itoa(st.CallbackPort) + callbackPath; st.RedirectURI != want {
		t.Errorf("RedirectURI = %q，期望跟随实际端口 %q", st.RedirectURI, want)
	}
}

// TestRedirectURIHostIsAlwaysLoopback 护栏：redirect_uri 的 host **必须**是 127.0.0.1。
//
// 上游 portal 对 host 做严格等值判断，写成 LAN IP/主机名时它会静默丢弃该地址、
// 改发 lobsterai:// deep-link ⇒ 网关永远收不到 code（2026-10-18 实证）。
// 谁要是再想出「把 host 换成局域网 IP」的主意，这条断言会立刻红。
func TestRedirectURIHostIsAlwaysLoopback(t *testing.T) {
	for _, port := range []int{0, freeLoopbackPort(t)} {
		statePath := filepath.Join(t.TempDir(), "login-state.json")
		if _, err := StartWithPort(NewClient(), statePath, port); err != nil {
			t.Fatalf("StartWithPort(%d): %v", port, err)
		}
		st, err := readState(statePath)
		if err != nil {
			t.Fatalf("read state: %v", err)
		}
		Shutdown()
		if !strings.HasPrefix(st.RedirectURI, "http://127.0.0.1:") {
			t.Errorf("port=%d 时 RedirectURI = %q，必须以 http://127.0.0.1: 开头（portal 硬约束）", port, st.RedirectURI)
		}
		if st.CallbackPort <= 0 {
			t.Errorf("port=%d 时 CallbackPort = %d，必须为正", port, st.CallbackPort)
		}
	}
}
