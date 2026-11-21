package chatgpt

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bogdanfinn/tls-client/profiles"

	"wild-work/internal/auth"
)

// TestMain 清掉宿主的代理环境变量：否则本机 HTTPS_PROXY 会把全部假上游请求
// 拐到真代理上（"代理生效"那条用例通过 t.Setenv 自己设）。
func TestMain(m *testing.M) {
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy",
		"ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

// ── 头族：顺序 + 取值 ────────────────────────────────────────────────────────

// TestHeaderFamilyOrder 头族的**顺序**也是身份的一部分（顺序错同样被拒），
// 所以用裸 TCP 假上游读原始请求头块来断言。
func TestHeaderFamilyOrder(t *testing.T) {
	up := startRawUpstream(t, okJSON(`{"id":"x"}`))
	c := NewWithConfig(Config{Base: up.base()})
	defer c.Close()

	a := &auth.Auth{AccessToken: "tok", UID: "u", DeviceID: "dev-1",
		Extra: map[string]string{"accountId": "acct-1"}}
	if _, _, err := c.Do(a, "POST", "/backend-api/x", []byte(`{}`),
		map[string]string{"content-type": "application/json"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	head := up.wait(t)
	order := headerNames(head)

	// 顺序断言（相邻对，均为实际存在的头）。
	want := []string{"user-agent", "accept", "accept-language", "authorization", "cache-control",
		"chatgpt-account-id", "content-type", "oai-device-id", "oai-did", "oai-language",
		"originator", "pragma", "priority", "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
		"sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site", "referer"}
	for i := 1; i < len(want); i++ {
		if err := assertBefore(order, want[i-1], want[i]); err != nil {
			t.Fatalf("%v（实际顺序: %s）", err, strings.Join(order, ", "))
		}
	}

	// 取值断言：身份头必须齐全且与凭据一致。
	for k, v := range map[string]string{
		"authorization":         "Bearer tok",
		"chatgpt-account-id":    "acct-1",
		"oai-device-id":         "dev-1",
		"oai-did":               "dev-1",
		"originator":            "Codex Browser",
		"x-openai-web-frontend": "codex_webview",
		"sec-ch-ua-platform":    `"Windows"`,
		"content-type":          "application/json",
	} {
		if got := headerValue(head, k); got != v {
			t.Errorf("头 %s = %q，期望 %q", k, got, v)
		}
	}
	if !strings.Contains(headerValue(head, "user-agent"), "Edg/") {
		t.Errorf("codex 身份 UA = %q，期望 Edge", headerValue(head, "user-agent"))
	}
}

// TestLegacyIdentityHeaders 传统网页身份（非 codex）的 UA / 专有头。
func TestLegacyIdentityHeaders(t *testing.T) {
	up := startRawUpstream(t, okJSON(`{"id":"x"}`))
	c := NewWithConfig(Config{Base: up.base(), Identity: IdentityLegacy})
	defer c.Close()

	a := &auth.Auth{AccessToken: "tok", UID: "u", DeviceID: "dev-2"}
	if _, _, err := c.Do(a, "GET", PathMe, nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	head := up.wait(t)
	if ua := headerValue(head, "user-agent"); !strings.Contains(ua, "Chrome/146") {
		t.Errorf("UA = %q，期望 Chrome/146", ua)
	}
	for _, k := range []string{"origin", "oai-session-id", "oai-client-version", "sec-ch-ua-arch"} {
		if headerValue(head, k) == "" {
			t.Errorf("传统身份缺头 %s", k)
		}
	}
	if headerValue(head, "originator") != "" {
		t.Error("传统身份不得带 codex 的 originator")
	}
}

// TestSessionEndpointSendsNoBearer /api/auth/session 只认 cookie，带 Bearer 会被判异常。
func TestSessionEndpointSendsNoBearer(t *testing.T) {
	up := startRawUpstream(t, okJSON(`{"accessToken":"x","expires":"2030-01-01T00:00:00Z"}`))
	c := NewWithConfig(Config{Base: up.base()})
	defer c.Close()

	a := &auth.Auth{AccessToken: "tok", UID: "u", DeviceID: "dev",
		Extra: map[string]string{"accountId": "acct"}, Cookies: map[string]string{"session": "s"}}
	if _, err := c.RefreshSession(a); err != nil {
		t.Fatalf("RefreshSession: %v", err)
	}
	head := up.wait(t)
	if headerValue(head, "authorization") != "" {
		t.Error("/api/auth/session 不得带 Authorization")
	}
	if headerValue(head, "chatgpt-account-id") != "" {
		t.Error("/api/auth/session 不得带 chatgpt-account-id")
	}
	if !strings.Contains(headerValue(head, "cookie"), "session=s") {
		t.Errorf("续期必须带账号 cookie: %q", headerValue(head, "cookie"))
	}
}

// ── TLS 指纹 ────────────────────────────────────────────────────────────────

// TestTLSFingerprintMatchesProfile 断言真发出去的 ClientHello 与所选档位一致。
// 标准库 crypto/tls 的 hello（默认档）会被上游判低可信——这正是本票换 TLS 栈的原因。
func TestTLSFingerprintMatchesProfile(t *testing.T) {
	if _, ok := profiles.MappedTLSClients[DefaultTLSProfile]; !ok {
		t.Fatalf("默认指纹档 %q 在 tls-client 里不存在（升级依赖后必须复核）", DefaultTLSProfile)
	}
	hc := startHelloCapture(t)
	// 用主机名而不是 IP：Go 的 crypto/tls 对 IP 字面量不发 SNI（socket 仍解析到本机）。
	c := NewWithConfig(Config{Base: "https://localhost:" + portOf(hc.addr), Insecure: true})
	defer c.Close()

	// 握手会被假上游主动中止，忽略返回错误——本用例只取证 ClientHello。
	_, _, _ = c.Do(testAuth(map[string]string{"session": "s"}), "GET", PathMe, nil, nil)
	chi := hc.wait(t)

	if !containsStr(chi.SupportedProtos, "h2") {
		t.Errorf("ALPN = %v，期望含 h2（真实浏览器档位）", chi.SupportedProtos)
	}
	if !containsUint16(chi.SupportedVersions, tls.VersionTLS13) {
		t.Errorf("SupportedVersions = %v，期望含 TLS1.3", chi.SupportedVersions)
	}
	if chi.ServerName == "" {
		t.Error("SNI 为空")
	}

	prof := profiles.MappedTLSClients[DefaultTLSProfile]
	id := prof.GetClientHelloId()
	spec, err := id.ToSpec()
	if err != nil {
		t.Fatalf("档位 %q 无 spec: %v", DefaultTLSProfile, err)
	}
	want := dropGREASE(spec.CipherSuites)
	got := dropGREASE(chi.CipherSuites)
	if strings.Join(uint16Strs(want), ",") != strings.Join(uint16Strs(got), ",") {
		t.Errorf("ClientHello cipher suites 与档位 %q 不一致\n want=%v\n got =%v",
			DefaultTLSProfile, want, got)
	}
}

// TestUnknownProfileFailsLoudly 档位拼错必须显式报错，不得静默退回标准库。
func TestUnknownProfileFailsLoudly(t *testing.T) {
	c := NewWithConfig(Config{Profile: "definitely-not-a-profile"})
	defer c.Close()
	if _, err := c.Me(testAuth(map[string]string{"session": "s"})); err == nil ||
		!strings.Contains(err.Error(), "未知 TLS 指纹档") {
		t.Fatalf("未知档位错误 = %v", err)
	}
}

// ── 代理 ────────────────────────────────────────────────────────────────────

// TestProxyHTTPConnect 显式 http 代理：出站必须走 CONNECT 隧道。
func TestProxyHTTPConnect(t *testing.T) {
	p := startProxy(t, "http", okJSON(`{"id":"proxied"}`))
	c := NewWithConfig(Config{Base: "http://upstream.invalid", Proxy: p.proxyURL()})
	defer c.Close()

	st, body, err := c.Do(testAuth(map[string]string{"session": "s"}), "GET", PathMe, nil, nil)
	if err != nil {
		t.Fatalf("Do 经代理: %v", err)
	}
	if st != 200 || !strings.Contains(string(body), "proxied") {
		t.Fatalf("经代理响应 = %d %s", st, body)
	}
	p.waitRequest(t)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.connect) == 0 || !strings.Contains(p.connect[0], "upstream.invalid:80") {
		t.Errorf("CONNECT 目标 = %v，期望 upstream.invalid:80", p.connect)
	}
}

// TestProxySocks5 socks5:// 形态必须原样支持（tls-client 内置 dialer）。
func TestProxySocks5(t *testing.T) {
	p := startProxy(t, "socks5", okJSON(`{"id":"socks"}`))
	c := NewWithConfig(Config{Base: "http://upstream.invalid", Proxy: p.proxyURL()})
	defer c.Close()

	st, body, err := c.Do(testAuth(map[string]string{"session": "s"}), "GET", PathMe, nil, nil)
	if err != nil {
		t.Fatalf("Do 经 socks5: %v", err)
	}
	if st != 200 || !strings.Contains(string(body), "socks") {
		t.Fatalf("经 socks5 响应 = %d %s", st, body)
	}
	req := p.waitRequest(t)
	if !strings.Contains(headerValue(req, "host"), "upstream.invalid") {
		t.Errorf("隧道内请求 Host = %q", headerValue(req, "host"))
	}
}

// TestProxyFromEnvironment HTTPS_PROXY 环境变量（未显式配代理时）必须生效。
func TestProxyFromEnvironment(t *testing.T) {
	p := startProxy(t, "http", okJSON(`{"id":"env"}`))
	t.Setenv("HTTPS_PROXY", p.proxyURL())

	c := NewWithConfig(Config{Base: "http://upstream.invalid"})
	defer c.Close()
	if _, _, err := c.Do(testAuth(map[string]string{"session": "s"}), "GET", PathMe, nil, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	p.waitRequest(t)
}

// TestResolveProxyPrecedence 环境变量优先序（显式配置 > 环境变量，https 基址优先 HTTPS_PROXY）。
func TestResolveProxyPrecedence(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://hp:1")
	t.Setenv("HTTPS_PROXY", "http://sp:2")
	if got := resolveProxy("https"); got != "http://sp:2" {
		t.Errorf("https 基址取到 %q，期望 HTTPS_PROXY", got)
	}
	if got := resolveProxy("http"); got != "http://hp:1" {
		t.Errorf("http 基址取到 %q，期望 HTTP_PROXY", got)
	}
	t.Setenv("HTTP_PROXY", "")
	if got := resolveProxy("http"); got != "http://sp:2" {
		t.Errorf("HTTP_PROXY 缺失时应回退 HTTPS_PROXY，得到 %q", got)
	}
	t.Setenv("HTTPS_PROXY", "")
	if got := resolveProxy("http"); got != "" {
		t.Errorf("全空应返回空串，得到 %q", got)
	}
}

// ── 超时 ────────────────────────────────────────────────────────────────────

// TestHeaderTimeoutNeverShrinks 等响应头上限只允许放宽（用户把总超时调大时不能被压低）。
func TestHeaderTimeoutNeverShrinks(t *testing.T) {
	if got := NewWithConfig(Config{}).HeaderTimeout(); got != DefaultHeaderTimeout {
		t.Errorf("默认 = %v，期望 %v", got, DefaultHeaderTimeout)
	}
	if got := NewWithConfig(Config{Timeout: 10 * time.Minute}).HeaderTimeout(); got != 10*time.Minute {
		t.Errorf("总超时更大时应跟着放宽，得到 %v", got)
	}
	if got := NewWithConfig(Config{Timeout: 10 * time.Minute, HeaderTimeout: 30 * time.Second}).HeaderTimeout(); got != 10*time.Minute {
		t.Errorf("显式小值不得压低到 30s，得到 %v", got)
	}
	// 静默上游：流式 client（无总超时）必须靠看门狗自己脱身。
	su := startSilentUpstream(t)
	// 等响应头看门狗只允许放宽，故两端都设成同一个短值（静默上游下只有它能脱身）。
	c := NewWithConfig(Config{Base: "http://" + su.addr, Timeout: 300 * time.Millisecond,
		HeaderTimeout: 300 * time.Millisecond})
	defer c.Close()
	start := time.Now()
	if _, err := c.OpenStream(testAuth(map[string]string{"session": "s"}), PathMe, nil, nil); err == nil {
		t.Fatal("静默上游应当超时报错（等响应头看门狗失效）")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("看门狗耗时 %v，期望 ~300ms", d)
	}
}

// TestStreamClientHasNoTotalTimeout R35：流式 client 不得设总超时。
func TestStreamClientHasNoTotalTimeout(t *testing.T) {
	c := NewWithConfig(Config{Timeout: 90 * time.Second})
	defer c.Close()
	if c.StreamHTTP.Timeout != 0 {
		t.Errorf("StreamHTTP.Timeout = %v，必须为 0（R35）", c.StreamHTTP.Timeout)
	}
	if c.HTTP.Timeout != 90*time.Second {
		t.Errorf("HTTP.Timeout = %v", c.HTTP.Timeout)
	}
}

// ── 小工具 ──────────────────────────────────────────────────────────────────

// headerNames 从原始请求头块里按出现顺序取头名（小写）。
func headerNames(head string) []string {
	var out []string
	for _, line := range strings.Split(head, "\r\n")[1:] {
		if line == "" {
			break
		}
		name, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out = append(out, strings.ToLower(strings.TrimSpace(name)))
	}
	return out
}

func headerValue(head, name string) string {
	name = strings.ToLower(name)
	for _, line := range strings.Split(head, "\r\n")[1:] {
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.ToLower(strings.TrimSpace(k)) == name {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func assertBefore(order []string, a, b string) error {
	ia, ib := indexOf(order, a), indexOf(order, b)
	if ia < 0 || ib < 0 {
		return fmt.Errorf("头族缺项: %s(%d) / %s(%d)", a, ia, b, ib)
	}
	if ia > ib {
		return fmt.Errorf("头顺序错: %s 应在 %s 之前", a, b)
	}
	return nil
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// portOf 从 host:port 里取端口（SNI 用例需要把假上游地址挪到主机名上）。
func portOf(hostPort string) string {
	_, p, err := net.SplitHostPort(hostPort)
	if err != nil {
		return ""
	}
	return p
}

// isGREASE 判断是否 GREASE 值（0x?a?a）——两侧都要剔掉再比。
func isGREASE(v uint16) bool { return v&0x0f0f == 0x0a0a }

func dropGREASE(in []uint16) []uint16 {
	out := make([]uint16, 0, len(in))
	for _, v := range in {
		if !isGREASE(v) {
			out = append(out, v)
		}
	}
	return out
}

func uint16Strs(in []uint16) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, itoa(int(v)))
	}
	return out
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func containsUint16(s []uint16, v uint16) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
