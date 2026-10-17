package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// 本文件锁定 R47：Cline 的 429 分两类，**OpenRouter 侧共享上限不得软化账号**。
//
// 为什么渠道只有 3 个模型时这件事特别致命：
// pool.CooldownModel 在「同一账号已有 ≥2 个模型处于冷却」时会**升级为整号冷却**
// （见 internal/pool/pool.go 的升级兜底）。若把共享上限误判成账号级，
// 三个免费模型被撞一遍后所有 Key 都会被逐个冷却 → `503 no_healthy_account`，
// 而这与账号健康毫无关系（换到第二个 Cline 账号照样撞同一个 OpenRouter 日限）。
// 这就是 R16/R38 反复出现的误伤形态。

func newClineTestHandler(t *testing.T, up *kindedUpstream) (*Handler, *pool.Pool) {
	t.Helper()
	p := pool.New(t.TempDir() + "/state-cline.json")
	p.Add(&auth.Auth{Kind: "cline", AccessToken: "sk_A", ExpiresAt: 4102444800, UID: "apk-A"})
	p.Add(&auth.Auth{Kind: "cline", AccessToken: "sk_B", ExpiresAt: 4102444800, UID: "apk-B"})
	// A 积分更高保证首挑确定性（挑号遍历 map 顺序随机）
	p.SetCreditDetail("apk-A", 1000, 0, 0)
	p.SetCreditDetail("apk-B", 500, 0, 0)
	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.Cline: {Kind: provider.Cline, Pool: p, Upstream: up,
				StaticModels: []provider.ModelInfo{{ID: "stepfun/step-5-preview"}}},
		},
		APIKey:       "",
		HardCooldown: 60_000_000_000,
		SoftCooldown: 60_000_000_000,
		ErrThreshold: 3,
		ErrCooldown:  60_000_000_000,
		MaxRotate:    3,
	})
	return h, p
}

func postCline(t *testing.T, h *Handler, model string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}],"stream":false}`)))
	return rec
}

func cooledCount(p *pool.Pool) int {
	n := 0
	for _, s := range p.List() {
		if s.Cooling || s.Disabled {
			n++
		}
	}
	return n
}

// TestClineSharedCapDoesNotSoftenAccounts 是核心回归：
// 上游把 429 压平成 HTTP 500 + `limit_rpd`/`Daily limit reached` 文案，
// Classify 归 ErrPassthrough ⇒ **只调用一次上游、不轮号、不冷却任何账号、原文透传**。
// 去掉「共享上限优先判 passthrough」这条分类即失败。
func TestClineSharedCapDoesNotSoftenAccounts(t *testing.T) {
	capBody := `{"error":"inference request failed: failed to invoke model 'x' from Openrouter: ` +
		`request failed with status 429: {\"error\":{\"message\":\"Rate limit exceeded: limit_rpd/x. ` +
		`Daily limit reached for x:free via Thinking Machines. Credits don't affect this cap.\"}}"}`
	up := &kindedUpstream{status: http.StatusInternalServerError, kind: provider.ErrPassthrough, body: capBody}
	h, p := newClineTestHandler(t, up)

	rec := postCline(t, h, "cline/stepfun/step-5-preview")

	// 原文透传（客户端需要看到 Daily limit 才能正确退避），且不被包装成 503
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望原文透传 500（不得包装成 503）\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Daily limit reached") {
		t.Fatalf("应透传上游原文（含 Daily limit），实际: %.200s", rec.Body.String())
	}
	up.mu.Lock()
	calls := len(up.calls)
	up.mu.Unlock()
	if calls != 1 {
		t.Fatalf("共享上限属上游链共享、换号必然复现 ⇒ 只应调用 1 次，实际 %d 次（把第二个 Key 也浪费了）", calls)
	}
	if n := cooledCount(p); n != 0 {
		t.Fatalf("共享上限不得软化账号，实际有 %d 个账号被整体冷却/禁用", n)
	}
	// 也不得留下模型级冷却（那是 ErrSoftRate 的处置）——否则共享上限会慢慢把三个模型全冻住
	for _, s := range p.List() {
		if len(s.ModelCooling) != 0 {
			t.Fatalf("共享上限不应产生模型级冷却（应只记在渠道内的 capped 表），实际 %v", s.ModelCooling)
		}
	}
}

// TestClineAccountLevelRateLimitDoesRotate 对照：无共享上限文案的真 429
// 才按账号级处置 ⇒ 冷却中招号并同请求换号（R29 全量适用，本渠道不置 SingleAccount）。
func TestClineAccountLevelRateLimitDoesRotate(t *testing.T) {
	up := &kindedUpstream{
		status: http.StatusTooManyRequests,
		kind:   provider.ErrSoftRate,
		body:   `{"error":"upstream 429: too many requests on this key"}`,
		// A 失败、B 成功：验证确实换了号
		succeed: func(uid string) bool { return uid == "apk-B" },
	}
	h, p := newClineTestHandler(t, up)

	rec := postCline(t, h, "cline/stepfun/step-5-preview")

	if rec.Code != http.StatusOK {
		t.Fatalf("换号后应成功，实得 %d: %.200s", rec.Code, rec.Body.String())
	}
	up.mu.Lock()
	calls := append([]string(nil), up.calls...)
	up.mu.Unlock()
	if len(calls) < 2 || calls[0] != "apk-A" {
		t.Fatalf("应先撞 A 再换到 B，实际调用序列 %v", calls)
	} // ErrSoftRate 走的是「按模型冷却」（pool.CooldownModel，≥2 模型才升级整号），
	// 故断言模型级冷却存在，而不是断言整号 Cooling。
	cooled := false
	for _, s := range p.List() {
		if s.UID == "apk-A" && len(s.ModelCooling) > 0 {
			cooled = true
		}
	}
	if !cooled {
		// 本仓对「无模型级特征串」的 429 按账号级冷却（见 TestAccountScopedSoftRateCoolsAccount）。
		for _, s := range p.List() {
			if s.UID == "apk-A" && s.Cooling {
				cooled = true
			}
		}
	}
	if !cooled {
		t.Fatal("真账号级 429 应冷却中招号，否则下次还会撞上它")
	}
}

// TestClineBadParamsDoesNotPenalize 压平 500 里的 `empty response content`
// （本端 max_tokens 太小造成）属请求级 ⇒ 不换号也不冷却。
func TestClineBadParamsDoesNotPenalize(t *testing.T) {
	up := &kindedUpstream{status: http.StatusInternalServerError,
		kind: provider.ErrBadParams, body: `{"error":"empty response content","success":false}`}
	h, p := newClineTestHandler(t, up)

	rec := postCline(t, h, "cline/stepfun/step-5-preview")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("应原文透传 500，实得 %d", rec.Code)
	}
	up.mu.Lock()
	calls := len(up.calls)
	up.mu.Unlock()
	if calls != 1 {
		t.Fatalf("参数类错误换号必然复现，应只调 1 次，实际 %d", calls)
	}
	if n := cooledCount(p); n != 0 {
		t.Fatalf("参数类错误不得冷却账号，实际 %d 个", n)
	}
}

// TestClineSessionDeadDisablesKey Key 被删/失效 ⇒ 禁用该号（不可刷新，只能人工换 Key）。
func TestClineSessionDeadMarksKey(t *testing.T) {
	up := &kindedUpstream{status: http.StatusUnauthorized,
		kind: provider.ErrSessionDead, body: `{"error":"Unauthorized"}`,
		succeed: func(uid string) bool { return uid == "apk-B" }}
	h, p := newClineTestHandler(t, up)

	if rec := postCline(t, h, "cline/stepfun/step-5-preview"); rec.Code != http.StatusOK {
		t.Fatalf("应换到 B 成功，实得 %d", rec.Code)
	}
	st := p.List()
	var dead int
	for _, s := range st {
		if s.SessionDead {
			dead++
		}
	}
	if dead != 1 {
		t.Fatalf("失效 Key 应被标记登录失效 1 个，实得 %d（多账号池的正确行为）", dead)
	}
}
