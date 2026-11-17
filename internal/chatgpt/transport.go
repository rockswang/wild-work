package chatgpt

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"

	"wild-work/internal/auth"
)

// 本文件是出站通道：一个账号一份 TLS 指纹客户端与 cookie jar，外层是 stdlib
// `*http.Client`（devdump 的包装点，也是调用方唯一入口）。
//
// 为什么换成 tls-client：2026-10-14 起 chatgpt.com 把 TLS 指纹（JA3/JA4）与 HTTP 头族
// 交叉校验，标准库 crypto/tls 的 ClientHello 直接被判低可信（表现为伪装 404 或
// 403 Unusual activity）。tls-client 用 utls 复刻真实浏览器的 ClientHello。
//
// 为什么外层仍是 `*http.Client`：`internal/devdump` 的落盘包装只认 `*http.Client`，
// 而渠道包之间历来不共享 HTTP 客户端封装 ⇒ 用一个 std↔fhttp 适配器把 tls-client
// 藏在 Transport 之下，外露的接口与本仓其它渠道完全一致。

// Config 是客户端构造参数。零值即生产缺省（连 BaseURL / chrome_152 / codex 身份）。
type Config struct {
	// Base 覆盖上游基址（测试注入 httptest，或指向本地中转）；空 = BaseURL。
	Base string
	// Timeout 是非流式请求总超时；0 = DefaultTimeout。
	Timeout time.Duration
	// HeaderTimeout 是等响应头上限；0 = DefaultHeaderTimeout，且只允许放宽（见 NewWithConfig）。
	HeaderTimeout time.Duration
	// Proxy 显式代理地址（http/https/socks5）。空 = 读 HTTPS_PROXY/HTTP_PROXY 环境变量。
	Proxy string
	// Insecure 跳过 TLS 校验（仅测试/本地抓包调试）。
	Insecure bool
	// Profile 覆盖 TLS 指纹档；空 = DefaultTLSProfile。
	Profile string
	// Identity 覆盖出站身份；空 = DefaultIdentity。
	Identity string
	// DataDir 是缓存目录根（v2 惯例的 `data/`）；空 = 禁用磁盘缓存（全走内存，测试友好）。
	// 预取票与指纹基线落在 <DataDir>/cache/ 下（spec §8）。
	DataDir string
}

// Client 是 chatgpt 渠道的多账号出站客户端。
//
// 账号隔离：每个账号一份 session（独立 TLS 客户端 + 独立 cookie jar + 独立
// device id / Bearer）——cookie 是**账号级**凭据，共用一个 jar 会让 A 号的
// session cookie 串到 B 号的请求上。
//
// HTTP/StreamHTTP 对所有账号共享同一个 stdlib 客户端，账号由请求 context 携带 ⇒
// devdump 只需包装这两个 client 即可覆盖全渠道出站（与其它渠道同款接线）。
type Client struct {
	// HTTP 供非流式请求（带总超时）。
	HTTP *http.Client
	// StreamHTTP 供长 SSE 流：**不设总超时**（R35），空闲兜底由调用方包 IdleReader。
	StreamHTTP *http.Client
	// Base 上游基址（测试可覆盖；生产由 NewWithConfig 注入）。
	Base string

	cfg        Config
	proxy      string
	profile    string
	identity   string
	headerTime time.Duration
	dataDir    string

	rt       *accountRoundTripper
	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
}

// New 构造生产客户端（默认基址/超时/指纹档）。
func New() *Client { return NewWithConfig(Config{}) }

// NewWithTimeout 构造客户端并注入非流式请求总超时（0 = 默认）。
// main.go 用它接线 `upstream.timeout_seconds`（与其它内建渠道同款）。
func NewWithTimeout(d time.Duration) *Client { return NewWithConfig(Config{Timeout: d}) }

// NewWithConfig 构造客户端。返回后可直接覆盖 Base（测试注入 httptest 假上游）。
func NewWithConfig(cfg Config) *Client {
	if cfg.Base == "" {
		cfg.Base = BaseURL
	}
	cfg.Base = strings.TrimRight(cfg.Base, "/")
	if cfg.Profile == "" {
		cfg.Profile = DefaultTLSProfile
	}
	if cfg.Identity == "" {
		cfg.Identity = DefaultIdentity
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	// 等响应头上限只允许放宽：总超时（用户可调）若比下限还小，就拿总超时兜住，
	// 否则用户调大 timeout_seconds 也救不回「首字节慢」的请求（R110/R118 同口径）。
	head := cfg.HeaderTimeout
	if head <= 0 {
		head = DefaultHeaderTimeout
	}
	if head < cfg.Timeout {
		head = cfg.Timeout
	}

	c := &Client{
		Base:       cfg.Base,
		cfg:        cfg,
		profile:    cfg.Profile,
		identity:   cfg.Identity,
		headerTime: head,
		dataDir:    strings.TrimRight(cfg.DataDir, "/"),
		sessions:   map[string]*session{},
	}
	c.rt = &accountRoundTripper{c: c}
	c.HTTP = &http.Client{Transport: c.rt, Timeout: cfg.Timeout}
	c.StreamHTTP = &http.Client{Transport: c.rt}
	return c
}

// HeaderTimeout 返回实际生效的等响应头上限（测试/日志用）。
func (c *Client) HeaderTimeout() time.Duration { return c.headerTime }

// SetDataDir 设置缓存目录根（wiring 方便在构造之后注入 v2 的 `data/`）。
// 传空串则禁用磁盘缓存（缓存与指纹退化为纯内存语义：每次都真跑预检）。
func (c *Client) SetDataDir(dir string) {
	c.mu.Lock()
	c.dataDir = strings.TrimRight(dir, "/")
	c.mu.Unlock()
}

// dataDirValue 锁内读缓存目录根。
func (c *Client) dataDirValue() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dataDir
}

// accountRoundTripper 把 stdlib 请求转交给「请求 context 里那个账号」的 session。
type accountRoundTripper struct{ c *Client }

func (rt *accountRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	s := sessionFromContext(req.Context())
	if s == nil {
		return nil, errors.New("chatgpt: 请求未绑定账号（内部错误）")
	}
	return s.roundTrip(req)
}

// CloseIdleConnections 让 stdlib 的 CloseIdleConnections 真的生效（转发到各 session）。
func (rt *accountRoundTripper) CloseIdleConnections() { rt.c.closeSessions() }

// Close 关闭全部 session 的空闲连接（进程退出/测试清理用）。
func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	sessions := make([]*session, 0, len(c.sessions))
	for _, s := range c.sessions {
		sessions = append(sessions, s)
	}
	c.mu.Unlock()
	for _, s := range sessions {
		s.cli.CloseIdleConnections()
	}
}

func (c *Client) closeSessions() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.sessions {
		s.cli.CloseIdleConnections()
	}
}

// ── session：单账号的出站通道 ────────────────────────────────────────────────

// session 持有**一个账号**的 TLS 客户端、cookie jar 与账号级请求头取值。
type session struct {
	cli      tls_client.HttpClient
	hostURL  *url.URL // cookie jar 的作用域（= 上游基址）
	base     string
	identity string
	ua       string
	secChUa  string
	headerTo time.Duration

	mu          sync.RWMutex
	deviceID    string
	sessionID   string
	accessToken string
	accountID   string
	seeded      map[string]string // 最近一次从 auth 灌进 jar 的 cookie 快照
}

// newSession 建一个账号的出站通道（不联网）。
func (c *Client) newSession(a accountSeed) (*session, error) {
	prof, ok := profiles.MappedTLSClients[c.profile]
	if !ok {
		return nil, fmt.Errorf("chatgpt: 未知 TLS 指纹档 %q", c.profile)
	}
	ua, secChUa := c.cfgUA()
	opts := []tls_client.HttpClientOption{
		// 总超时交给外层 stdlib client（HTTP 有 Timeout、StreamHTTP 没有 ⇒ R35）；
		// tls-client 自带的 timeout 会连流式一起掐断，故显式关掉。
		tls_client.WithTimeoutSeconds(0),
		tls_client.WithClientProfile(prof),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
	}
	if p := c.proxyURL(); p != "" {
		// 只注入自定义工厂（代理地址烘进闭包）：tls-client 不允许同时设 proxyUrl 与工厂。
		opts = append(opts, tls_client.WithProxyDialerFactory(newProxyDialerFactory(p)))
	}
	if c.cfg.Insecure {
		opts = append(opts, tls_client.WithInsecureSkipVerify())
	}
	cli, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		return nil, fmt.Errorf("chatgpt: 初始化 TLS 客户端失败: %w", err)
	}
	s := &session{
		cli:         cli,
		hostURL:     &url.URL{Scheme: c.scheme(), Host: c.host()},
		base:        c.baseURL(),
		identity:    c.identity,
		ua:          ua,
		secChUa:     secChUa,
		headerTo:    c.headerTime,
		deviceID:    a.deviceID,
		sessionID:   newUUID(),
		accessToken: a.accessToken,
		accountID:   a.accountID,
	}
	if s.deviceID == "" {
		s.deviceID = newUUID()
	}
	s.seedCookies(a.cookies)
	return s, nil
}

// cfgUA 返回身份对应的 UA 与 sec-ch-ua（同源于 ww3 的两套身份）。
func (c *Client) cfgUA() (ua, secChUa string) {
	if c.identity == IdentityCodex {
		return UAEdge151, SecChUaEdge151
	}
	return UAChrome146, SecChUaChrome146
}

func (c *Client) baseURL() string {
	if strings.TrimSpace(c.Base) != "" {
		return strings.TrimRight(c.Base, "/")
	}
	return BaseURL
}

func (c *Client) scheme() string {
	if strings.HasPrefix(strings.ToLower(c.baseURL()), "http://") {
		return "http"
	}
	return "https"
}

func (c *Client) host() string {
	u := c.baseURL()
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if i := strings.IndexAny(u, "/?#"); i >= 0 {
		u = u[:i]
	}
	return u
}

// proxyURL 解析生效代理：Config.Proxy 优先，否则按基址协议读环境变量。
// 先例 custom / officeace 走 http.ProxyFromEnvironment；这里换成显式解析是因为
// tls-client 只吃一个代理 URL 字符串（且 socks5:// 必须原样传入）。
func (c *Client) proxyURL() string {
	if strings.TrimSpace(c.cfg.Proxy) != "" {
		return strings.TrimSpace(c.cfg.Proxy)
	}
	return resolveProxy(c.scheme())
}

// resolveProxy 读代理环境变量。https 基址优先 HTTPS_PROXY，http 基址优先 HTTP_PROXY；
// 两者互为兜底（返回值原样使用，`socks5://` 形态由 tls-client 支持）。
func resolveProxy(scheme string) string {
	keys := []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"}
	if scheme == "http" {
		keys = []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"}
	}
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// seedCookies 用账号凭据清空并重灌 cookie jar。
func (s *session) seedCookies(m map[string]string) {
	if len(m) == 0 {
		return
	}
	s.cli.SetCookieJar(tls_client.NewCookieJar())
	ck := make([]*fhttp.Cookie, 0, len(m))
	for k, v := range m {
		if k == "" {
			continue
		}
		ck = append(ck, &fhttp.Cookie{Name: k, Value: v})
	}
	s.cli.SetCookies(s.hostURL, ck)
	s.mu.Lock()
	s.seeded = cloneCookies(m)
	s.mu.Unlock()
}

// Cookies 读回 jar 里的 cookie（续期后持久化用）。
func (s *session) Cookies() map[string]string {
	out := map[string]string{}
	for _, ck := range s.cli.GetCookies(s.hostURL) {
		out[ck.Name] = ck.Value
	}
	return out
}

// sync 把 auth 的最新取值同步进 session（token/accountID/cookie）。
// cookie 只在「auth 与灌入时快照不同」时重灌，避免把上游 Set-Cookie 的新值冲掉。
func (s *session) sync(a accountSeed) {
	s.mu.Lock()
	s.accessToken, s.accountID = a.accessToken, a.accountID
	if a.deviceID != "" {
		s.deviceID = a.deviceID
	}
	stale := !cookiesEqual(s.seeded, a.cookies)
	s.mu.Unlock()
	if stale {
		s.seedCookies(a.cookies)
	}
}

func (s *session) token() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.accessToken
}

func (s *session) accountHeader() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.accountID
}

func (s *session) device() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.deviceID
}

// ── 请求 ────────────────────────────────────────────────────────────────────

// ctxKey 是请求 context 里携带账号 session 的键（类型化，避免与其它包撞键）。
type ctxKey struct{}

func withSession(ctx context.Context, s *session) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

func sessionFromContext(ctx context.Context) *session {
	s, _ := ctx.Value(ctxKey{}).(*session)
	return s
}

// roundTrip 把 stdlib 请求交给 tls-client，并把响应转回 stdlib 形状。
func (s *session) roundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	// 等响应头看门狗：`Do` 返回即表示响应头已到，此后 body 流式读取**不受**该定时器影响
	// （timer.Stop 后 context 不会被取消）。自定义 RoundTripper 不受 net/http 的
	// TLSHandshakeTimeout / ResponseHeaderTimeout 约束，必须自己兜底，否则静默上游会挂死。
	timer := time.AfterFunc(s.headerTo, cancel)
	defer timer.Stop()

	freq, err := fhttp.NewRequestWithContext(ctx, req.Method, req.URL.String(), req.Body)
	if err != nil {
		return nil, fmt.Errorf("chatgpt: 构造上游请求失败: %w", err)
	}
	freq.Header = s.header(req.Header, noAuthFromContext(ctx))

	resp, err := s.cli.Do(freq)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("chatgpt: %s %s 请求失败: %w", req.Method, req.URL.Path, err)
	}
	return toNetResponse(req, resp), nil
}

// header 构造请求头族（**顺序也是身份的一部分**——顺序错也会被拒）。
// extra 是调用方追加的头（如 content-type）；同名时调用方优先。
func (s *session) header(extra http.Header, noAuth bool) fhttp.Header {
	h := fhttp.Header{
		"user-agent":      {s.ua},
		"accept":          {"*/*"},
		"accept-language": {"en,en-GB;q=0.9,en-US;q=0.8"},
		"cache-control":   {"no-cache"},
		"pragma":          {"no-cache"},
		"priority":        {"u=1, i"},
		"referer":         {s.base + "/"},
		"sec-ch-ua":       {s.secChUa},
		"sec-fetch-dest":  {"empty"},
		"sec-fetch-mode":  {"cors"},
		"sec-fetch-site":  {"same-origin"},
		"oai-device-id":   {s.device()},
		"oai-did":         {s.device()},
		"oai-language":    {"en-US"},
	}
	order := []string{"user-agent", "accept", "accept-language", "authorization", "cache-control",
		"chatgpt-account-id", "content-type", "oai-device-id", "oai-did", "oai-language"}

	if s.identity == IdentityCodex {
		h["originator"] = []string{"Codex Browser"}
		h["x-openai-codex-window-type"] = []string{"browser"}
		h["x-openai-rate-limit-join-workspace"] = []string{"1"}
		h["x-openai-web-frontend"] = []string{"codex_webview"}
		h["sec-ch-ua-mobile"] = []string{"?0"}
		h["sec-ch-ua-platform"] = []string{`"Windows"`}
		order = append(order, "originator", "pragma", "priority",
			"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
			"sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site",
			"x-conduit-token", "x-oai-turn-trace-id",
			"x-openai-codex-window-type", "x-openai-rate-limit-join-workspace",
			"x-openai-web-frontend", "x-openai-web-sse-compression", "referer")
	} else {
		h["origin"] = []string{s.base}
		h["sec-ch-ua-arch"] = []string{`"x86"`}
		h["sec-ch-ua-bitness"] = []string{`"64"`}
		h["sec-ch-ua-full-version"] = []string{`"146.0.0.0"`}
		h["sec-ch-ua-full-version-list"] = []string{`"Chromium";v="146.0.0.0", "Google Chrome";v="146.0.0.0", "Not/A)Brand";v="99.0.0.0"`}
		h["sec-ch-ua-mobile"] = []string{"?0"}
		h["sec-ch-ua-model"] = []string{`""`}
		h["sec-ch-ua-platform"] = []string{`"Windows"`}
		h["sec-ch-ua-platform-version"] = []string{`"19.0.0"`}
		h["oai-session-id"] = []string{s.sessionID}
		h["oai-client-version"] = []string{"prod-a194cd50d4416d3c0b47c740f206b12ce60f5887"}
		h["oai-client-build-number"] = []string{"6708908"}
		order = append(order, "origin", "pragma", "priority",
			"sec-ch-ua", "sec-ch-ua-arch", "sec-ch-ua-bitness", "sec-ch-ua-full-version",
			"sec-ch-ua-full-version-list", "sec-ch-ua-mobile", "sec-ch-ua-model",
			"sec-ch-ua-platform", "sec-ch-ua-platform-version",
			"sec-fetch-dest", "sec-fetch-mode", "sec-fetch-site",
			"oai-session-id", "oai-client-version", "oai-client-build-number",
			"x-conduit-token", "x-oai-turn-trace-id", "x-openai-web-sse-compression", "referer")
	}

	if !noAuth {
		if tok := s.token(); tok != "" {
			h["authorization"] = []string{"Bearer " + tok}
		}
		if acc := s.accountHeader(); acc != "" {
			h["chatgpt-account-id"] = []string{acc}
		}
	}
	for k, vs := range extra {
		if k == "" || len(vs) == 0 {
			continue
		}
		h[strings.ToLower(k)] = vs
	}
	h[fhttp.HeaderOrderKey] = order
	h[fhttp.PHeaderOrderKey] = []string{":method", ":authority", ":scheme", ":path"}
	return h
}

// noAuthKey 标记「本次请求不带 Bearer / chatgpt-account-id」（/api/auth/session 专用）。
type noAuthKey struct{}

func withNoAuth(ctx context.Context) context.Context {
	return context.WithValue(ctx, noAuthKey{}, true)
}

func noAuthFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(noAuthKey{}).(bool)
	return v
}

// toNetResponse 把 fhttp 响应转成 stdlib 响应（头顺序键不外泄）。
func toNetResponse(req *http.Request, resp *fhttp.Response) *http.Response {
	h := make(http.Header, len(resp.Header))
	for k, vs := range resp.Header {
		if k == fhttp.HeaderOrderKey || k == fhttp.PHeaderOrderKey {
			continue
		}
		h[http.CanonicalHeaderKey(k)] = vs
	}
	return &http.Response{
		Status:        resp.Status,
		StatusCode:    resp.StatusCode,
		Proto:         resp.Proto,
		ProtoMajor:    resp.ProtoMajor,
		ProtoMinor:    resp.ProtoMinor,
		Header:        h,
		Body:          resp.Body,
		ContentLength: resp.ContentLength,
		Request:       req,
	}
}

// ── 对外请求入口 ─────────────────────────────────────────────────────────────

// Do 发一个非流式请求，返回状态码与完整 body（已解 gzip）。
func (c *Client) Do(a *auth.Auth, method, path string, body []byte, extra map[string]string) (int, []byte, error) {
	resp, err := c.do(a, c.HTTP, method, path, body, extra, false)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := readAllMaybeGzip(resp)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("chatgpt: 读响应失败: %w", err)
	}
	return resp.StatusCode, b, nil
}

// DoJSON 发请求并把响应解析成 map（非 2xx 也解析，交给上层分类）。
func (c *Client) DoJSON(a *auth.Auth, method, path string, body any, extra map[string]string) (int, map[string]any, []byte, error) {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return 0, nil, nil, fmt.Errorf("chatgpt: 请求体序列化失败: %w", err)
		}
		if extra == nil {
			extra = map[string]string{}
		}
		if _, ok := extra["content-type"]; !ok {
			extra["content-type"] = "application/json"
		}
	}
	st, b, err := c.Do(a, method, path, raw, extra)
	if err != nil {
		return st, nil, b, err
	}
	var m map[string]any
	if len(b) > 0 {
		_ = json.Unmarshal(b, &m)
	}
	return st, m, b, nil
}

// OpenStream 发起 SSE 请求，返回未关闭的响应（调用方负责 Close，并自行包空闲兜底）。
func (c *Client) OpenStream(a *auth.Auth, path string, body []byte, extra map[string]string) (*http.Response, error) {
	return c.do(a, c.StreamHTTP, "POST", path, body, extra, false)
}

// do 是唯一的请求构造点：解析账号 session、绑定 context、注入头。
func (c *Client) do(a *auth.Auth, cli *http.Client, method, path string, body []byte, extra map[string]string, noAuth bool) (*http.Response, error) {
	s, err := c.session(a)
	if err != nil {
		return nil, err
	}
	ctx := withSession(context.Background(), s)
	if noAuth {
		ctx = withNoAuth(ctx)
	}
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, rd)
	if err != nil {
		return nil, fmt.Errorf("chatgpt: 构造请求失败: %w", err)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// readAllMaybeGzip 读全部 body；声明了 gzip 而 fhttp 未自动解压时兜底解压。
func readAllMaybeGzip(resp *http.Response) ([]byte, error) {
	if strings.Contains(strings.ToLower(resp.Header.Get("content-encoding")), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return io.ReadAll(resp.Body)
		}
		defer gz.Close()
		return io.ReadAll(gz)
	}
	return io.ReadAll(resp.Body)
}

// readSSE 逐行读 SSE，把 `data: ` 后面的载荷交给 fn（票 03 的流式解析用）。
//
// 三点必须照做（否则会静默漏内容）：裸字符串帧、`[DONE]` 不是 JSON、上游可能 gzip。
func readSSE(resp *http.Response, fn func(payload string) error) error {
	body := io.Reader(resp.Body)
	if strings.Contains(strings.ToLower(resp.Header.Get("content-encoding")), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return fmt.Errorf("chatgpt: 解 gzip 失败: %w", err)
		}
		defer gz.Close()
		body = gz
	}
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20) // 单帧上限 8MB（生图帧可能很大）
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
		if payload == "" {
			continue
		}
		if err := fn(payload); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("chatgpt: 读流中断: %w", err)
	}
	return nil
}
