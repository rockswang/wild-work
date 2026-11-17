package chatgpt

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
)

// 本文件在本地 mock 上游上跑通 sentinel 预检链（票 02 验收）。
//
// 关键纪律（防漏改）：
//   - 预取票是一次性的：读后即删；
//   - 两道挑战互为前置：任一道失败都不得继续（尤其不得走到 finalize）；
//   - 漂移门禁只比结构，不比每次都会变的计数；
//   - 漂移时**不发业务请求**、**不落盘新基线**。

const businessPath = "/backend-api/f/conversation"

// mockSentinel 是离线假上游：首页 / prepare / finalize / 业务端点。
type mockSentinel struct {
	mu  sync.Mutex
	hit map[string]int

	build string
	// 每次 prepare 的 dx 程序（按收到的 p token 编码）；nil = turnstile 不要求。
	dxProg func(pTok string) any
	// prepare 响应里额外追加的键（用于制造结构漂移）。
	prepExtraKeys []string
	// finalize 响应里额外追加的键。
	finExtraKeys []string

	turnstileRequired bool
	powRequired       bool
	difficulty        string
	arkose            bool
	noPrepareToken    bool
	// 置真时 prepare 下发一个非法 dx（模拟挑战不可解）。
	badDX bool
	// 置真时 prepare 不带 so 块；否则带一个 so 块（结构漂移观测点）。
	noSo bool
}

func newMockSentinel() *mockSentinel {
	return &mockSentinel{
		hit:               map[string]int{},
		build:             "build-1",
		powRequired:       true,
		turnstileRequired: true,
		difficulty:        "",
		dxProg:            func(string) any { return []any{[]any{float64(3), "turnstile-ok"}} },
	}
}

func (m *mockSentinel) hits(path string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hit[path]
}

func (m *mockSentinel) resetHits() {
	m.mu.Lock()
	m.hit = map[string]int{}
	m.mu.Unlock()
}

func (m *mockSentinel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.hit[r.URL.Path]++
	build, dxProg := m.build, m.dxProg
	prepExtra, finExtra := append([]string(nil), m.prepExtraKeys...), append([]string(nil), m.finExtraKeys...)
	ttRequired, powRequired, diff := m.turnstileRequired, m.powRequired, m.difficulty
	arkose, noPrepTok, noSo := m.arkose, m.noPrepareToken, m.noSo
	badDX := m.badDX
	m.mu.Unlock()

	switch r.URL.Path {
	case "/":
		_, _ = w.Write([]byte(`<!doctype html><html data-build="` + build +
			`"><script>script.src = 'https://chatgpt.com/sentinel/abcd1234/sdk.js'</script></html>`))
	case PathSentinelPrepare:
		var req struct {
			P string `json:"p"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp := map[string]any{}
		if !noPrepTok {
			resp["prepare_token"] = "prep-token-abc"
		}
		if powRequired {
			resp["proofofwork"] = map[string]any{"required": true, "seed": "seed-x", "difficulty": diff}
		}
		if ttRequired {
			dx := ""
			if badDX {
				dx = "!!!not-base64!!!"
			} else {
				dx = encodeDXTest(dxProg(req.P), req.P)
			}
			resp["turnstile"] = map[string]any{"required": true, "dx": dx}
		}
		if arkose {
			resp["arkose"] = map[string]any{"required": true}
		}
		if !noSo {
			resp["so"] = map[string]any{"collector_dx": "x"}
		}
		for _, k := range prepExtra {
			resp[k] = 1
		}
		writeJSONTest(w, resp)
	case PathSentinelFinalize:
		resp := map[string]any{"token": "req-token-xyz", "expire_after": 540,
			"expire_at": time.Now().Add(540 * time.Second).Unix()}
		for _, k := range finExtra {
			resp[k] = 2
		}
		writeJSONTest(w, resp)
	default:
		writeJSONTest(w, map[string]any{"ok": true})
	}
}

func writeJSONTest(w http.ResponseWriter, v any) {
	w.Header().Set("content-type", "application/json")
	b, _ := json.Marshal(v)
	_, _ = w.Write(b)
}

// encodeDXTest 复刻生产解码：dx = base64(xor(JSON 指令流, p token))。
func encodeDXTest(prog any, key string) string {
	raw, _ := json.Marshal(prog)
	b := make([]byte, len(raw))
	for i := 0; i < len(raw); i++ {
		if key == "" {
			b[i] = raw[i]
		} else {
			b[i] = raw[i] ^ key[i%len(key)]
		}
	}
	return base64.StdEncoding.EncodeToString(b)
}

// newSentinelClient 建一个指向 mock 的客户端（DataDir 走 t.TempDir，测试互不污染）。
func newSentinelClient(t *testing.T, m *mockSentinel) (*Client, *httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	c := NewWithConfig(Config{Base: srv.URL, DataDir: dir})
	t.Cleanup(c.Close)
	return c, srv, dir
}

func sentinelAuth() *auth.Auth {
	return &auth.Auth{AccessToken: "tok", UID: "u1", DeviceID: "dev-1", Cookies: map[string]string{"session": "s"}}
}

// TestSentinelChainEndToEnd 跑通首页 → prepare → finalize，三令牌就位。
func TestSentinelChainEndToEnd(t *testing.T) {
	m := newMockSentinel()
	c, _, dir := newSentinelClient(t, m)
	a := sentinelAuth()

	s, err := c.RunSentinel(a)
	if err != nil {
		t.Fatalf("RunSentinel: %v", err)
	}
	if s.ReqToken != "req-token-xyz" {
		t.Errorf("ReqToken = %q", s.ReqToken)
	}
	if !strings.HasPrefix(s.Proof, "gAAAAAB") || !strings.HasSuffix(s.Proof, "~S") {
		t.Errorf("Proof 形态不对: %q", s.Proof)
	}
	if s.Turnstile != base64.StdEncoding.EncodeToString([]byte("turnstile-ok")) {
		t.Errorf("Turnstile = %q", s.Turnstile)
	}
	// 三步各一次，业务端点零次。
	for _, p := range []string{"/", PathSentinelPrepare, PathSentinelFinalize} {
		if got := m.hits(p); got != 1 {
			t.Errorf("%s 命中 %d 次，期望 1", p, got)
		}
	}
	if got := m.hits(businessPath); got != 0 {
		t.Errorf("预检阶段不得发业务请求，命中 %d 次", got)
	}
	// 三个令牌挂到正确位置（头名）。
	h := s.Headers()
	for _, k := range []string{"openai-sentinel-chat-requirements-token",
		"openai-sentinel-proof-token", "openai-sentinel-turnstile-token"} {
		if h[k] == "" {
			t.Errorf("Headers() 缺 %s", k)
		}
	}
	if h["openai-sentinel-chat-requirements-token"] != "req-token-xyz" {
		t.Errorf("requirements 头 = %q", h["openai-sentinel-chat-requirements-token"])
	}
	// 首页缓存落在 v2 数据目录惯例路径下。
	if _, err := os.Stat(dir + "/cache/home.json"); err != nil {
		t.Errorf("首页缓存未落盘: %v", err)
	}
	if s.Home.DataBuild != "build-1" || s.Home.Scripts[0] != "https://chatgpt.com/sentinel/abcd1234/sdk.js" {
		t.Errorf("Home = %+v", s.Home)
	}
}

// TestSentinelPrefetchRoundTrip 预取票：落盘、消费、读后即删。
func TestSentinelPrefetchRoundTrip(t *testing.T) {
	m := newMockSentinel()
	c, _, _ := newSentinelClient(t, m)
	a := sentinelAuth()

	if _, err := c.EnsureFresh(a); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	path := c.tokenCachePath(a)
	if path == "" {
		t.Fatal("未解析出预取票路径（DataDir 失效？）")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("预取票未落盘: %v", err)
	}

	m.resetHits()
	s, err := c.RunSentinel(a)
	if err != nil {
		t.Fatalf("RunSentinel(缓存): %v", err)
	}
	if !s.TokenCached {
		t.Error("应消费预取票（TokenCached=true）")
	}
	if s.ReqToken != "req-token-xyz" {
		t.Errorf("缓存票的 ReqToken = %q", s.ReqToken)
	}
	// 命中缓存 ⇒ 一个上游请求都不发（首字慢的主因被省掉）。
	if total := m.hits("/") + m.hits(PathSentinelPrepare) + m.hits(PathSentinelFinalize); total != 0 {
		t.Errorf("命中缓存后仍发了 %d 次上游请求", total)
	}
	// 一次性语义：读后即删。
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("预取票未被消费掉（读后即删）: %v", err)
	}
	// 再取一次必须重跑完整链路（票已被消费）。
	m.resetHits()
	s2, err := c.RunSentinel(a)
	if err != nil {
		t.Fatalf("RunSentinel(重跑): %v", err)
	}
	if s2.TokenCached {
		t.Error("票已消费，不得再命中缓存")
	}
	if m.hits(PathSentinelPrepare) != 1 || m.hits(PathSentinelFinalize) != 1 {
		t.Errorf("重跑未走完整链路: prepare=%d finalize=%d",
			m.hits(PathSentinelPrepare), m.hits(PathSentinelFinalize))
	}
}

// TestSentinelExpiredTicketRecomputed 名义 TTL（120s）到期必须重算，不得复用过期票。
func TestSentinelExpiredTicketRecomputed(t *testing.T) {
	m := newMockSentinel()
	c, _, _ := newSentinelClient(t, m)
	a := sentinelAuth()

	if _, err := c.EnsureFresh(a); err != nil {
		t.Fatalf("EnsureFresh: %v", err)
	}
	path := c.tokenCachePath(a)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读预取票: %v", err)
	}
	var tc tokenCache
	if err := json.Unmarshal(raw, &tc); err != nil {
		t.Fatalf("解预取票: %v", err)
	}
	tc.FetchedAt = time.Now().Add(-(TokenMaxAge + 10) * time.Second).Unix()
	out, _ := json.Marshal(tc)
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("改写预取票: %v", err)
	}

	m.resetHits()
	s, err := c.RunSentinel(a)
	if err != nil {
		t.Fatalf("RunSentinel: %v", err)
	}
	if s.TokenCached {
		t.Error("过期票不得被复用")
	}
	if m.hits(PathSentinelPrepare) != 1 {
		t.Errorf("过期票必须重跑 prepare，命中 %d 次", m.hits(PathSentinelPrepare))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("过期票也应被读后即删: %v", err)
	}
}

// TestSentinelDriftUnknownOpcodeFailClosed 未知 opcode 是致命漂移：不发业务请求、不落新基线。
func TestSentinelDriftUnknownOpcodeFailClosed(t *testing.T) {
	m := newMockSentinel()
	c, _, _ := newSentinelClient(t, m)
	a := sentinelAuth()

	if _, err := c.RunSentinel(a); err != nil {
		t.Fatalf("基线轮: %v", err)
	}
	base0, err := os.ReadFile(c.fingerprintPath(a))
	if err != nil {
		t.Fatalf("基线未落盘: %v", err)
	}

	m.dxProg = func(string) any {
		return []any{[]any{float64(999), float64(0)}, []any{float64(3), "ok"}}
	}
	m.resetHits()
	_, err = c.RunSentinel(a)
	if err == nil {
		t.Fatal("未知 opcode 必须 fail-closed")
	}
	var de *SentinelDriftError
	if !errors.As(err, &de) || de.Level != "fatal" {
		t.Fatalf("错误 = %v，期望 *SentinelDriftError{fatal}", err)
	}
	if !strings.Contains(err.Error(), "本渠道需要升级") {
		t.Errorf("错误文案应可行动: %q", err.Error())
	}
	// 致命漂移在 finalize 之前就刹车。
	if got := m.hits(PathSentinelFinalize); got != 0 {
		t.Errorf("未知 opcode 不得走到 finalize，命中 %d 次", got)
	}
	if got := m.hits(businessPath); got != 0 {
		t.Errorf("漂移时业务请求数必须为 0，实际 %d", got)
	}
	// 不落盘新基线（否则第二次就看不到漂移了）。
	base1, err := os.ReadFile(c.fingerprintPath(a))
	if err != nil {
		t.Fatalf("基线丢失: %v", err)
	}
	if string(base0) != string(base1) {
		t.Error("漂移时不得覆盖基线快照")
	}
}

// TestSentinelStructureDriftFailClosed 结构漂移（响应键集变化）：拦下且保留旧基线。
func TestSentinelStructureDriftFailClosed(t *testing.T) {
	m := newMockSentinel()
	c, _, _ := newSentinelClient(t, m)
	a := sentinelAuth()

	if _, err := c.RunSentinel(a); err != nil {
		t.Fatalf("基线轮: %v", err)
	}
	base0, _ := os.ReadFile(c.fingerprintPath(a))

	m.prepExtraKeys = []string{"brand_new_field"}
	m.resetHits()
	_, err := c.RunSentinel(a)
	if err == nil {
		t.Fatal("结构漂移必须 fail-closed")
	}
	var de *SentinelDriftError
	if !errors.As(err, &de) || de.Level != "block" {
		t.Fatalf("错误 = %v，期望 *SentinelDriftError{block}", err)
	}
	if m.hits(businessPath) != 0 {
		t.Errorf("漂移时业务请求数必须为 0，实际 %d", m.hits(businessPath))
	}
	base1, _ := os.ReadFile(c.fingerprintPath(a))
	if string(base0) != string(base1) {
		t.Error("结构漂移时不得覆盖基线快照（否则第二次就看不到漂移）")
	}
}

// TestDriftIgnoresInstructionCount dx 指令数每次都变，**不得**据此判漂移（只比结构）。
func TestDriftIgnoresInstructionCount(t *testing.T) {
	m := newMockSentinel()
	c, _, _ := newSentinelClient(t, m)
	a := sentinelAuth()

	if _, err := c.RunSentinel(a); err != nil {
		t.Fatalf("轮 1: %v", err)
	}
	// 同结构、指令数不同。
	m.dxProg = func(string) any {
		return []any{
			[]any{float64(2), float64(0), "x"}, // Q[0] = "x"
			[]any{float64(2), float64(1), "y"}, // Q[1] = "y"
			[]any{float64(3), "turnstile-ok"},
		}
	}
	s, err := c.RunSentinel(a)
	if err != nil {
		t.Fatalf("指令数变化不得判漂移: %v", err)
	}
	if s.Drift != nil && (s.Drift.Level == "block" || s.Drift.Level == "fatal") {
		t.Fatalf("指令数变化被误判为漂移: %+v", s.Drift)
	}
	if s.DXOps != 3 {
		t.Errorf("DXOps = %d，期望 3", s.DXOps)
	}
}

// TestBuildChangeIsWarnOnly data-build 变化只是日常发版，必须放行（只警告）。
func TestBuildChangeIsWarnOnly(t *testing.T) {
	m := newMockSentinel()
	c, _, _ := newSentinelClient(t, m)
	a := sentinelAuth()

	if _, err := c.RunSentinel(a); err != nil {
		t.Fatalf("轮 1: %v", err)
	}
	_ = os.Remove(c.cachePath(cacheFileNameHome)) // 否则首页缓存会掩盖 build 变化
	m.build = "build-2"
	s, err := c.RunSentinel(a)
	if err != nil {
		t.Fatalf("build 变化不得拦: %v", err)
	}
	if s.Drift == nil || s.Drift.Level != "warn" {
		t.Fatalf("Drift = %+v，期望 warn", s.Drift)
	}
}

// TestChallengesArePrerequisites 任一道挑战失败都不得继续（不得把另一道的成功当通过）。
func TestChallengesArePrerequisites(t *testing.T) {
	m := newMockSentinel()
	m.turnstileRequired = true
	m.badDX = true // turnstile 这道挑战不可能通过
	c, _, _ := newSentinelClient(t, m)
	a := sentinelAuth()

	_, err := c.RunSentinel(a)
	if err == nil {
		t.Fatal("turnstile 无法通过时必须报错")
	}
	if m.hits(PathSentinelFinalize) != 0 {
		t.Errorf("挑战失败不得走到 finalize，命中 %d 次", m.hits(PathSentinelFinalize))
	}
	if m.hits(businessPath) != 0 {
		t.Errorf("业务请求数必须为 0，实际 %d", m.hits(businessPath))
	}
}

// TestValidateTurnstileToken 四道拒绝都是「静默失效」形态，必须全部拦下。
func TestValidateTurnstileToken(t *testing.T) {
	seed := "gAAAAACseed"
	cases := []struct {
		tok, want string
	}{
		{"", "空 token"},
		{seed, "token == seed"},
		{"12345", "纯数字"},
		{base64.StdEncoding.EncodeToString([]byte("37: TypeError: x")), "VM 内部报错"},
	}
	for _, c := range cases {
		if got := validateTurnstileToken(c.tok, seed); !strings.Contains(got, c.want) {
			t.Errorf("validate(%q) = %q，期望含 %q", c.tok, got, c.want)
		}
	}
	if got := validateTurnstileToken(base64.StdEncoding.EncodeToString([]byte("hello")), seed); got != "" {
		t.Errorf("正常 token 被误拒: %q", got)
	}
}

// TestDriftLeavesSearchableLog 漂移必须留下可检索日志（运维靠它定位「上游改算法了」）。
func TestDriftLeavesSearchableLog(t *testing.T) {
	m := newMockSentinel()
	c, _, _ := newSentinelClient(t, m)
	a := sentinelAuth()

	var buf strings.Builder
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)

	m.badDX = false
	m.dxProg = func(string) any {
		return []any{[]any{float64(999), float64(0)}}
	}
	if _, err := c.RunSentinel(a); err == nil {
		t.Fatal("未知 opcode 应 fail-closed")
	}
	if !strings.Contains(buf.String(), "chatgpt sentinel drift") {
		t.Errorf("日志缺少可检索前缀，实际: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "fatal") {
		t.Errorf("日志缺少级别，实际: %q", buf.String())
	}
}

// TestReadHomeCacheGuards 首页缓存的三个失配维度（base / UA / TTL / 空 build）都必须 miss。
func TestReadHomeCacheGuards(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/home.json"
	body := func(build, base, ua string, at time.Time) string {
		b, _ := json.Marshal(cachedHome{Base: base, UA: ua, DataBuild: build, FetchedAt: at.Unix()})
		return string(b)
	}
	if err := os.WriteFile(path, []byte(body("b", "https://x", "UA", time.Now())), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readHomeCache(path, "https://x", "UA"); !ok {
		t.Error("匹配的缓存应命中")
	}
	if _, ok := readHomeCache(path, "https://y", "UA"); ok {
		t.Error("换 base 不得命中")
	}
	if _, ok := readHomeCache(path, "https://x", "UA2"); ok {
		t.Error("换 UA 不得命中")
	}
	_ = os.WriteFile(path, []byte(body("b", "https://x", "UA", time.Now().Add(-(HomeTTL+time.Minute)))), 0o600)
	if _, ok := readHomeCache(path, "https://x", "UA"); ok {
		t.Error("过期不得命中")
	}
	_ = os.WriteFile(path, []byte(body("", "https://x", "UA", time.Now())), 0o600)
	if _, ok := readHomeCache(path, "https://x", "UA"); ok {
		t.Error("空 data-build 不得命中")
	}
}
