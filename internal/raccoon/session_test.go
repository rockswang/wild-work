package raccoon

// R148：X-RACCOON-Session-ID 必须是**会话级**稳定值，不是每请求一个。
//
// 取证（2026-10-31 三轮实验，官方小浣熊 1.1.5 客户端本地日志
// `~/.config/office-raccoon/agent/logs/box-agent.log` 与会话落盘 session.jsonl）：
//   · 一个项目 = 一个 ACP 会话，id 形态 `sess-<进程内序号>-<8 位 hex>`（sess-0-53927d74 等）
//   · 实测 sess-0-53927d74 持续 37 分钟 / 150+ 次 LLM 调用不换
//   · 遇 429 也不换会话；session/done 只结束本轮，会话仍可继续
//   · 只有「用户新建项目」才发 session/new 且序号 +1
//   · 两个项目并行时各自独立会话，交错 2 秒发请求也不触发限流
// 对照本渠道旧行为「每请求新建 sess-<32 位 hex>」：同一账号几分钟内冒出上百个新会话，
// 这是 429 的真正触发条件（频率假说已被官方的 97 次 ≤10 秒零 429 证伪）。
//
// 本组测试**走真实 ChatStream + 假上游**，观测点是上游实际收到的 X-RACCOON-Session-ID，
// 而不是测 sessionFor 自己（那测不出调用点有没有接上线）。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
)

// sessionCapture 记录假上游收到的每个 X-RACCOON-Session-ID 与 Turn-ID。
type sessionCapture struct {
	mu      sync.Mutex
	sessions []string
	turns    []string
}

func (c *sessionCapture) add(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions = append(c.sessions, r.Header.Get("X-RACCOON-Session-ID"))
	c.turns = append(c.turns, r.Header.Get("X-RACCOON-Turn-ID"))
}

// newSessionTestServer 起一个恒回 200 的假上游，把收到的会话/轮次头记下来。
func newSessionTestServer(t *testing.T, cap *sessionCapture) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.add(r)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	c := New()
	c.Base = srv.URL
	return c
}

// resetSessions 清空会话表，让每个测试从确定的初始状态开始（测试间不隐式依赖）。
func resetSessions(t *testing.T) {
	t.Helper()
	sessMu.Lock()
	sessByKey = map[string]sessionEntry{}
	sessSeq = 0
	sessMu.Unlock()
	t.Cleanup(func() {
		sessMu.Lock()
		sessByKey = map[string]sessionEntry{}
		sessSeq = 0
		sessMu.Unlock()
	})
}

// testAuth 构造一个带账号标识的凭据（会话分桶键要用账号维度）。
func testAuth(uid string) *auth.Auth {
	return &auth.Auth{UID: uid, AccessToken: "dummy-" + uid}
}

// testAuthWithToken 指定 token 构造凭据（用于验证日志/密钥不泄露）。
func testAuthWithToken(uid, token string) *auth.Auth {
	return &auth.Auth{UID: uid, AccessToken: token}
}

func chatOnce(t *testing.T, c *Client, model string) {
	t.Helper()
	chatOnceAs(t, c, testAuth("Lily"), model)
}

// chatOnceAs 以指定账号发一次请求（用于验证账号维度对会话分桶的影响）。
func chatOnceAs(t *testing.T, c *Client, a *auth.Auth, model string) {
	t.Helper()
	body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`)
	rc, _, _, err := c.ChatStream(a, body)
	if err != nil {
		t.Fatalf("ChatStream error: %v", err)
	}
	if rc != nil {
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
}

// TestSessionIDStableAcrossRequests 同一模型连续 5 次请求必须共用 1 个会话 ID。
//
// 这是本决策的核心断言：旧实现下这里会收到 5 个不同的 sess-xxx。
func TestSessionIDStableAcrossRequests(t *testing.T) {
	resetSessions(t)
	cap := &sessionCapture{}
	c := newSessionTestServer(t, cap)

	const n = 5
	for i := 0; i < n; i++ {
		chatOnce(t, c, "raccoon-8c4485")
	}

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.sessions) != n {
		t.Fatalf("上游收到 %d 次请求，want %d", len(cap.sessions), n)
	}
	first := cap.sessions[0]
	if first == "" {
		t.Fatal("X-RACCOON-Session-ID 为空：调用点没接上 sessionFor")
	}
	for i, got := range cap.sessions {
		if got != first {
			t.Fatalf("第 %d 次请求的会话 ID = %q，want 与首次相同的 %q（会话必须跨请求稳定）", i+1, got, first)
		}
	}
	// turn id 仍应每请求变化（官方是每轮一个，本渠道每请求一个，见 newTurnID 注释）。
	seenTurn := map[string]bool{}
	for _, tid := range cap.turns {
		if tid == "" {
			t.Fatal("X-RACCOON-Turn-ID 为空")
		}
		seenTurn[tid] = true
	}
	if len(seenTurn) != n {
		t.Fatalf("turn id 只出现 %d 个不同值，want %d（每请求应新建）", len(seenTurn), n)
	}
}

// TestSessionIDMatchesOfficialShape 会话 ID 形态必须是官方实测的 `sess-<序号>-<8 位 hex>`。
//
// 旧实现是 `sess-<32 位 hex>`（缺序号、hex 段过长），形态与语义两处都不对。
func TestSessionIDMatchesOfficialShape(t *testing.T) {
	resetSessions(t)
	cap := &sessionCapture{}
	c := newSessionTestServer(t, cap)
	chatOnce(t, c, "raccoon-8c4485")

	cap.mu.Lock()
	defer cap.mu.Unlock()
	got := cap.sessions[0]
	// 官方样例：sess-0-53927d74 / sess-1-798ffb2b / sess-2-f436038c
	re := regexp.MustCompile(`^sess-([0-9]+)-([0-9a-f]{8})$`)
	m := re.FindStringSubmatch(got)
	if m == nil {
		t.Fatalf("会话 ID = %q，不符合官方形态 sess-<序号>-<8 位 hex>", got)
	}
	if m[1] != "0" {
		t.Errorf("进程内首个会话序号 = %q，want 0（官方 sess-0 起）", m[1])
	}
}

// TestSessionIDPerModelDistinct 不同模型各自独立会话，不会互相复用。
//
// 官方不同项目 = 不同会话；网关没有项目概念，用「模型」作为最接近的分桶维度。
func TestSessionIDPerModelDistinct(t *testing.T) {
	resetSessions(t)
	cap := &sessionCapture{}
	c := newSessionTestServer(t, cap)

	chatOnce(t, c, "raccoon-8c4485")
	chatOnce(t, c, "raccoon-8c4485") // 同模型，应复用
	chatOnce(t, c, "raccoon-19b265") // 表内另一模型，应独立会话

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.sessions) != 3 {
		t.Fatalf("上游收到 %d 次请求，want 3", len(cap.sessions))
	}
	if cap.sessions[0] != cap.sessions[1] {
		t.Errorf("同模型两次会话 ID 不同：%q vs %q", cap.sessions[0], cap.sessions[1])
	}
	if cap.sessions[1] == cap.sessions[2] {
		t.Errorf("不同模型复用了同一会话 ID %q，应按模型分桶", cap.sessions[2])
	}
	// 第二个不同模型应拿到序号 1（官方新建项目即序号 +1）。
	if !regexp.MustCompile(`^sess-1-[0-9a-f]{8}$`).MatchString(cap.sessions[2]) {
		t.Errorf("第二个模型的会话 = %q，want sess-1-<hex>（序号应递增）", cap.sessions[2])
	}
}

// TestSessionIDRotatesAfterTTL 超过轮换周期后必须换新会话（序号继续递增）。
func TestSessionIDRotatesAfterTTL(t *testing.T) {
	resetSessions(t)
	cap := &sessionCapture{}
	c := newSessionTestServer(t, cap)

	chatOnce(t, c, "raccoon-8c4485")
	first := sessionFor(sessionKeyOf(testAuth("Lily"), "raccoon-8c4485"))

	// 直接把表里该 key 的到期时刻拨到过去，模拟 24h 之后（不真等一天）。
	sessMu.Lock()
	sessByKey[sessionKeyOf(testAuth("Lily"), "raccoon-8c4485")] = sessionEntry{id: first, expires: time.Now().Add(-time.Second)}
	sessMu.Unlock()

	second := sessionFor(sessionKeyOf(testAuth("Lily"), "raccoon-8c4485"))
	if second == first {
		t.Fatalf("到期后仍返回旧会话 %q，应轮换", first)
	}
	if !regexp.MustCompile(`^sess-1-[0-9a-f]{8}$`).MatchString(second) {
		t.Errorf("轮换后的会话 = %q，want sess-1-<hex>", second)
	}
}

// TestSessionIDPerAccountDistinct 不同账号用**同一模型**必须拿到不同会话 ID。
//
// 这是 R148 的核心断言：会话 ID 会随请求发往上游，两个账号共用一个 sess- 会被上游
// 看成「同一会话出现在两个账号的请求里」——官方客户端不可能这样。
// 旧 key（只有模型名）下这里会拿到同一个 ID（测试转红的判据）。
func TestSessionIDPerAccountDistinct(t *testing.T) {
	resetSessions(t)
	cap := &sessionCapture{}
	c := newSessionTestServer(t, cap)

	lily := testAuth("Lily")
	bob := testAuth("Bob")
	const model = "raccoon-8c4485"

	// Lily 连打两次、Bob 连打两次，同模型同会话期。
	chatOnceAs(t, c, lily, model)
	chatOnceAs(t, c, lily, model)
	chatOnceAs(t, c, bob, model)
	chatOnceAs(t, c, bob, model)

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.sessions) != 4 {
		t.Fatalf("上游收到 %d 次请求，want 4", len(cap.sessions))
	}
	// 同账号内两次必须相同。
	if cap.sessions[0] != cap.sessions[1] {
		t.Errorf("Lily 两次会话不同：%q vs %q", cap.sessions[0], cap.sessions[1])
	}
	if cap.sessions[2] != cap.sessions[3] {
		t.Errorf("Bob 两次会话不同：%q vs %q", cap.sessions[2], cap.sessions[3])
	}
	// 跨账号必须不同（旧实现这里相等 ⇒ 会话跨账号漂移）。
	if cap.sessions[0] == cap.sessions[2] {
		t.Errorf("Lily 与 Bob 共用了会话 %q —— 会话绝不能跨账号复用（R148）", cap.sessions[0])
	}
}

// TestSessionSwitchAccountDoesNotReuseOldSession 模拟粘性路由换号：
// 账号 A → 账号 B → 再回账号 A，A 必须拿回**自己原来的**会话，而不是 B 的。
//
// 这是「三个维度都满足才新建」的直接验证：换号本身不销毁 A 的会话。
func TestSessionSwitchAccountDoesNotReuseOldSession(t *testing.T) {
	resetSessions(t)
	cap := &sessionCapture{}
	c := newSessionTestServer(t, cap)

	a := testAuth("AcctA")
	bb := testAuth("AcctB")
	const model = "raccoon-8c4485"

	chatOnceAs(t, c, a, model)  // A 首次 → sess-0
	chatOnceAs(t, c, bb, model) // 换号 B  → sess-1
	chatOnceAs(t, c, a, model)  // 切回 A  → 必须回到 sess-0

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.sessions) != 3 {
		t.Fatalf("上游收到 %d 次请求，want 3", len(cap.sessions))
	}
	if cap.sessions[0] != cap.sessions[2] {
		t.Errorf("切回账号 A 后会话变了：%q → %q（换号不应销毁 A 的会话，R148）",
			cap.sessions[0], cap.sessions[2])
	}
	if cap.sessions[0] == cap.sessions[1] {
		t.Errorf("换号时复用了 A 的会话 %q —— 会话绝不能跨账号", cap.sessions[0])
	}
	// B 应拿到序号 1（进程内第二个新会话）。
	if !regexp.MustCompile(`^sess-1-[0-9a-f]{8}$`).MatchString(cap.sessions[1]) {
		t.Errorf("换号后新会话 = %q，want sess-1-<hex>", cap.sessions[1])
	}
}

// TestAccountKeyFallbacks 账号标识的降级链：UID → Nickname → token 指纹 → anon。
//
// 防的是「UID 为空导致所有账号塌缩成一个 key」——raccoon 的 uid 在 auth 文件里存于
// account.uid，其它渠道/旧凭据可能没有。
func TestAccountKeyFallbacks(t *testing.T) {
	cases := []struct {
		name string
		auth *auth.Auth
		want string
	}{
		{"UID 优先", &auth.Auth{UID: "Lily", Nickname: "nick", AccessToken: "tok"}, "Lily"},
		{"UID 空退回昵称", &auth.Auth{Nickname: "nick", AccessToken: "tok"}, "nick"},
		{"UID 与昵称都空退回 token 指纹", &auth.Auth{AccessToken: "tok"}, ""},
		{"全空用 anon", &auth.Auth{}, "anon"},
		{"nil 用 anon", nil, "anon"},
	}
	for _, tc := range cases {
		got := accountKey(tc.auth)
		if tc.want == "" {
			// token 指纹形态：tok- + 8 位 hex
			if !regexp.MustCompile(`^tok-[0-9a-f]{8}$`).MatchString(got) {
				t.Errorf("%s: accountKey = %q，want tok-<8 位 hex>", tc.name, got)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("%s: accountKey = %q，want %q", tc.name, got, tc.want)
		}
	}
	// 不同 token 必须得出不同指纹（否则兜底等于没区分账号）。
	a1 := accountKey(&auth.Auth{AccessToken: "tok1"})
	a2 := accountKey(&auth.Auth{AccessToken: "tok2"})
	if a1 == a2 {
		t.Errorf("不同 token 得到同一指纹 %q —— 兜底失去区分能力", a1)
	}
}

// TestAccountKeyNeverLeaksToken 兜底指纹不得包含 token 明文。
func TestAccountKeyNeverLeaksToken(t *testing.T) {
	const secret = "sekrit-value-do-not-leak"
	got := accountKey(&auth.Auth{AccessToken: secret})
	if strings.Contains(got, secret) {
		t.Fatalf("accountKey 输出了 token 明文：%q", got)
	}
	if !strings.HasPrefix(got, "tok-") {
		t.Errorf("accountKey = %q，want tok- 前缀", got)
	}
}

// TestSessionIDConcurrentSameKey 并发首访同一「账号+模型」只能产出一个会话 ID。
//
// 竞态下会造出「一次对话被拆成两个会话」，正是要修的旧行为。
func TestSessionIDConcurrentSameKey(t *testing.T) {
	resetSessions(t)
	const n = 16
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i] = sessionFor(sessionKeyOf(testAuth("Lily"), "raccoon-8c4485"))
		}(i)
	}
	wg.Wait()
	for i, got := range ids {
		if got != ids[0] {
			t.Fatalf("并发第 %d 个拿到 %q，want 全部相同（首个 %q）", i, got, ids[0])
		}
	}
}
