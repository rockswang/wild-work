// 14018 积分耗尽修复（补丁 0023）行为锁定：
//
//	A. 分类修正：429+14018 归 hard_credit → 账号级 12h 硬冷却（Reason=余额/权益不足，
//	   非模型级——修复前误判 soft_rate 只冷「账号×模型」60s 且降级链永不触发）。
//	   双账号全冷 → 下一请求入口降级链正常顶替；充值后 ReenableIfCredits 自动解冻。
//	B. 请求内无感降级：轮转耗尽且最后 kind=hard_credit 时，本请求当场按降级链切备胎
//	   重入——客户端全程 200，X-Model-Fallback 头 + 响应 model 字段标注实际备胎；
//	   重入仅一次；单账号渠道豁免（惩罚豁免不变量）；无规则/备胎也无容量 → 与现状
//	   一致（原样透传）。
//	记账：B 重入命中的请求，请求日志/token 流水/模型消耗全记备胎（0021/PR #148
//	口径在请求内降级同样成立）；statRow.Status 重置（出口不把 200 记成 429）。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/ledger"
	"wild-work/internal/oczen"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/stats"
	"wild-work/internal/workbuddyai"
)

// body14018 生产实录原文 body（2026-09-23 app.log，workbuddyai uid=e840f08f…）。
const body14018 = `{"error":{"data":{"code":14018,"msg":"Credits exhausted. Please visit the link below to purchase add-on packs and get more credits: https://www.codebuddy.ai/profile/usage ","requestId":"9044201e"}}}`

// hardCreditUpstream 模拟积分耗尽渠道：ChatStream 恒回 429+14018；Classify 委托真实
// workbuddyai.Classify——生产 WorkBuddyAI 渠道的 Classify 即它（修复对象正是它），
// 锁定「分类→冷却→降级」全链路，而非仅测 handler 对某个 kind 的处置。
// 口径澄清（S5）：生产 Classify 实为四份——本补丁覆盖 workbuddyai/upstream 两份
// （14018 语义域）；oczen/monkeycode 是渠道专用分类（429 恒软限流、不查 body），
// 不加特判是对的。本文件借用 newFbHandler 的 oczen+workbuddy 两渠道骨架模拟
// WorkBuddyAI 场景：serveRotation/降级处置逻辑渠道无关，行为锁有效。
type hardCreditUpstream struct{ calls int }

func (u *hardCreditUpstream) RefreshToken(*auth.Auth) error                        { return nil }
func (u *hardCreditUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) { return nil, nil }
func (u *hardCreditUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (u *hardCreditUpstream) UserResource(*auth.Auth) (int64, error) { return 0, nil }
func (u *hardCreditUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (u *hardCreditUpstream) DailyCheckin(*auth.Auth) error { return nil }
func (u *hardCreditUpstream) Classify(status int, body string) provider.ErrKind {
	return workbuddyai.Classify(status, body)
}
func (u *hardCreditUpstream) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	u.calls++
	return nil, 429, []byte(body14018), nil
}
func (u *hardCreditUpstream) Stream(http.ResponseWriter, io.Reader, string) (map[string]any, error) {
	return nil, nil
}
func (u *hardCreditUpstream) Aggregate(_ io.Reader, model string) (map[string]any, error) {
	return map[string]any{"model": model, "choices": []any{}}, nil
}

// Test14018HardCreditCooldownThenEntryFallback A 组全链路：无规则时双账号 14018 →
// 原样透传 429（客户端看到真实原因）+ 两账号**账号级**硬冷却（Reason=余额/权益不足，
// 非模型级）；热配规则后下一请求入口降级链顶替（修复前永不触发的路径）。
func Test14018HardCreditCooldownThenEntryFallback(t *testing.T) {
	upA := &hardCreditUpstream{}
	upB := &fbUpstream{}
	h, pa, _ := newFbHandler(t, upA, upB, nil)
	pa.Add(&auth.Auth{UID: "main-2"}) // 生产同形：workbuddyai 双账号

	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("无规则时候选耗尽应原样透传 429，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":14018`) {
		t.Fatalf("透传应保留上游原始 body（14018），实际：%s", rec.Body.String())
	}
	if upA.calls != 2 {
		t.Errorf("双账号应各尝试一次，实际上游调用 %d 次", upA.calls)
	}
	for _, s := range pa.List() {
		if !s.Cooling || s.Reason != "余额/权益不足" {
			t.Errorf("账号 %s 应账号级硬冷却（Reason=余额/权益不足），实际 Cooling=%v Reason=%q", s.UID, s.Cooling, s.Reason)
		}
		if len(s.ModelCooling) != 0 {
			t.Errorf("14018 是账号级问题，不应有模型级冷却，实际 %v", s.ModelCooling)
		}
	}

	// 热配规则（面板保存路径）→ 下一请求：整渠道全灭 → 入口降级链顶上。
	h.SetModelFallbacks([]config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	rec2 := callModel(h, "oczen/big-pickle")
	if rec2.Code != http.StatusOK {
		t.Fatalf("冷却后下一请求应降级到备胎返回 200，实际 %d：%s", rec2.Code, rec2.Body.String())
	}
	if got := rec2.Header().Get("X-Model-Fallback"); got != "oczen/big-pickle -> workbuddy/glm-free" {
		t.Errorf("X-Model-Fallback = %q，want oczen/big-pickle -> workbuddy/glm-free", got)
	}
	if upB.calls != 1 || upA.calls != 2 {
		t.Errorf("降级后应只调备胎上游（A=%d B=%d），want A=2 B=1", upA.calls, upB.calls)
	}
}

// Test14018InRequestFallbackRescuesClient B 组主场景：同一请求内轮转耗尽（双账号
// 14018）→ 不透传，当场切备胎重入 → 客户端全程 200（无感）、响应 model=备胎全路径、
// X-Model-Fallback 头正确；两账号仍进账号级硬冷却（后续请求继续走降级链）。
func Test14018InRequestFallbackRescuesClient(t *testing.T) {
	upA := &hardCreditUpstream{}
	upB := &fbUpstream{}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	pa.Add(&auth.Auth{UID: "main-2"})

	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK {
		t.Fatalf("请求内降级应让客户端拿到 200（无感），实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Model-Fallback"); got != "oczen/big-pickle -> workbuddy/glm-free" {
		t.Errorf("X-Model-Fallback = %q，want oczen/big-pickle -> workbuddy/glm-free", got)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应体非 JSON：%v", err)
	}
	if m["model"] != "workbuddy/glm-free" {
		t.Errorf("响应 model 字段应为备胎全路径，实际 %v", m["model"])
	}
	if upA.calls != 2 || upB.calls != 1 {
		t.Errorf("轮转应 A=2（双账号耗尽）后 B=1（备胎顶上），实际 A=%d B=%d", upA.calls, upB.calls)
	}
	for _, s := range pa.List() {
		if !s.Cooling || s.Reason != "余额/权益不足" {
			t.Errorf("请求内降级后账号 %s 仍应账号级硬冷却，实际 Cooling=%v Reason=%q", s.UID, s.Cooling, s.Reason)
		}
	}
}

// Test14018BackupAlsoExhaustedTransparentOnce：备胎也积分耗尽 → 重入仅一次后原样
// 透传（备胎渠道的 14018），不无限递归降级。
func Test14018BackupAlsoExhaustedTransparentOnce(t *testing.T) {
	upA := &hardCreditUpstream{}
	upB := &hardCreditUpstream{}
	h, _, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("备胎也无容量应透传 429，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":14018`) {
		t.Fatalf("透传应保留 14018 原始 body，实际：%s", rec.Body.String())
	}
	if upB.calls != 1 {
		t.Errorf("重入应一次封顶（备胎上游调用 %d 次，want 1）", upB.calls)
	}
}

// Test14018SingleAccountChannelExemptedFromInRequestFallback：单账号渠道（AGENTS.md
// 惩罚豁免不变量）不触发请求内重入——14018 走 402 例外（仅模型级冷却+原样透传）；
// 下一请求挑号阶段模型全冷 → 入口降级链顶替（既有设计路径，非 B）。
// 口径澄清（S5）：本用例锁 handler 对单账号渠道的处置（渠道无关），模拟 WorkBuddyAI
// 装配形态；真实 oczen 生产链路的 Classify 是渠道专用恒软限流，14018 特判不在其上
// 生效（更保守方向：不罚匿名唯一账号）。
func Test14018SingleAccountChannelExemptedFromInRequestFallback(t *testing.T) {
	upA := &hardCreditUpstream{}
	upB := &fbUpstream{}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	h.cfg.Runtimes[provider.Oczen].SingleAccount = true

	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"code":14018`) {
		t.Fatalf("单账号渠道应原样透传 429/14018，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if upB.calls != 0 {
		t.Errorf("单账号渠道不应请求内重入，备胎上游被调 %d 次", upB.calls)
	}
	s := pa.List()[0]
	if s.Cooling {
		t.Errorf("402 例外应只模型级冷却，账号不应整号冷却（单账号=整渠道下线）")
	}
	if len(s.ModelCooling) != 1 || s.ModelCooling[0].Model != "big-pickle" || s.ModelCooling[0].Until.IsZero() {
		t.Errorf("big-pickle 应进入模型级硬冷却，实际 %v", s.ModelCooling)
	}
	// 下一请求：模型全冷 → 入口降级链顶替（单账号渠道的设计路径，非 B）。
	rec2 := callModel(h, "oczen/big-pickle")
	if rec2.Code != http.StatusOK || upB.calls != 1 {
		t.Fatalf("下一请求应入口降级到备胎 200，实际 %d B=%d：%s", rec2.Code, upB.calls, rec2.Body.String())
	}
}

// Test14018InRequestFallbackAccountingBookedUnderBackup：请求内降级命中的请求，
// 请求日志行/token 流水/今日模型消耗全部记**备胎**（0021/PR #148 口径在 B 重入
// 同样成立）；请求日志 Status=200（statRow.Status 重置生效，不把成功记成 429）。
func Test14018InRequestFallbackAccountingBookedUnderBackup(t *testing.T) {
	upA := &hardCreditUpstream{}
	upB := &acctUpstream{} // 复用记账测试的 usage 上报上游（11 prompt + 7 completion）
	h, _, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	st, err := stats.New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ledgerDir := t.TempDir()
	ld, err := ledger.New(ledgerDir)
	if err != nil {
		t.Fatal(err)
	}
	h.stats, h.ledger = st, ld

	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK {
		t.Fatalf("请求内降级应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	ld.Flush()
	rows := st.Logs(5)
	if len(rows) != 1 {
		t.Fatalf("请求日志应一行，实际 %d 行", len(rows))
	}
	if rows[0].Model != "workbuddy/glm-free" {
		t.Errorf("请求日志 Model 应记备胎 workbuddy/glm-free，实际 %q", rows[0].Model)
	}
	if rows[0].Status != http.StatusOK {
		t.Errorf("请求日志 Status 应 200（statRow.Status 重置生效），实际 %d", rows[0].Status)
	}
	ues := readUsageToday(t, ledgerDir)
	if len(ues) != 1 || ues[0].Model != "glm-free" || ues[0].Ch != "workbuddy" {
		t.Fatalf("token 流水应记备胎（Model=glm-free Ch=workbuddy），实际 %+v", ues)
	}
	if ms := st.Snapshot(time.Now()).Models; len(ms) != 1 || ms[0].Model != "workbuddy/glm-free" {
		t.Fatalf("今日模型消耗应归备胎，实际 %+v", ms)
	}
	ld.Close() // Windows：关文件句柄，否则 t.TempDir 清理失败
}

// callModelStream 发一次流式 chat 请求（S6：B 重入的流式记账覆盖——callModel 固定
// 非流式，此前流式路径零覆盖）。
func callModelStream(h *Handler, model string) *httptest.ResponseRecorder {
	body := `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// streamAcctUpstream 流式记账桩：ChatStream 回 200+SSE 假流并计数；Stream 上报
// usage（11 prompt + 7 completion，与非流式 acctUpstream.Aggregate 同值）——流式
// 分支的 token 流水/今日消耗由 Stream 返回的 usage 驱动，与非流式路径独立。
type streamAcctUpstream struct {
	acctUpstream
	calls int
}

func (u *streamAcctUpstream) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	u.calls++
	return io.NopCloser(strings.NewReader("data: [DONE]\n\n")), 200, nil, nil
}
func (u *streamAcctUpstream) Stream(http.ResponseWriter, io.Reader, string) (map[string]any, error) {
	return map[string]any{"prompt_tokens": float64(11), "completion_tokens": float64(7)}, nil
}

// Test14018RetryOnceLockedByTestEvenWithChainedRule（S4）：封顶独立锁定——配 A→B、
// B→C 链式规则后，规则匹配依然成立，唯一拦住「备胎再降备胎」的就是 fbAttempt==0
// 封顶；若封顶被误删，二轮判定会命中 B→C 规则重入 traework，upC.calls>0 即失败。
func Test14018RetryOnceLockedByTestEvenWithChainedRule(t *testing.T) {
	upA := &hardCreditUpstream{}
	upB := &hardCreditUpstream{}
	upC := &fbUpstream{}
	pa := pool.New(t.TempDir() + "/state-a.json")
	pa.Add(oczen.AnonymousAuth())
	pa.Add(&auth.Auth{UID: "main-2"}) // oczen 双号全 14018 → B 判定成立
	pb := pool.New(t.TempDir() + "/state-b.json")
	pb.Add(&auth.Auth{UID: "backup-uid"})
	pc := pool.New(t.TempDir() + "/state-c.json")
	pc.Add(&auth.Auth{UID: "chain-end-uid"})
	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.Oczen:     {Kind: provider.Oczen, Pool: pa, Upstream: upA},
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: pb, Upstream: upB},
			provider.TraeWork:  {Kind: provider.TraeWork, Pool: pc, Upstream: upC},
		},
		APIKey: "k", HardCooldown: time.Hour, SoftCooldown: time.Minute,
		ErrThreshold: 1, ErrCooldown: time.Minute,
		ModelFallbacks: []config.ModelFallbackRule{
			{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
			{From: "workbuddy/glm-free", To: []string{"traework/chain-end"}},
		},
	})

	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("备胎（workbuddy 单号）也耗尽时应在重入一次后透传 429，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":14018`) {
		t.Fatalf("透传应保留备胎渠道的 14018 原始 body，实际：%s", rec.Body.String())
	}
	if upA.calls != 2 || upB.calls != 1 {
		t.Errorf("A=2（双号耗尽）后 B=1（备胎顶上），实际 A=%d B=%d", upA.calls, upB.calls)
	}
	if upC.calls != 0 {
		t.Errorf("链式规则 B→C 存在，但封顶后不得从备胎再降备胎（traework 上游被调 %d 次，want 0）", upC.calls)
	}
}

// Test14018InRequestFallbackStreamingAccounting（S6）：流式请求下 B 重入记账同样
// 归备胎（此前仅非流式 callModel 覆盖；流式分支的流水/消耗由 Stream 上报的 usage
// 驱动，与 acctModel 传参路径独立，须单独锁定）。
func Test14018InRequestFallbackStreamingAccounting(t *testing.T) {
	upA := &hardCreditUpstream{}
	upB := &streamAcctUpstream{}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	pa.Add(&auth.Auth{UID: "main-2"}) // oczen 双号全 14018 → B 判定成立
	st, err := stats.New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ledgerDir := t.TempDir()
	ld, err := ledger.New(ledgerDir)
	if err != nil {
		t.Fatal(err)
	}
	h.stats, h.ledger = st, ld

	rec := callModelStream(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK {
		t.Fatalf("流式请求内降级应 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Model-Fallback"); got != "oczen/big-pickle -> workbuddy/glm-free" {
		t.Errorf("X-Model-Fallback = %q，want oczen/big-pickle -> workbuddy/glm-free", got)
	}
	if upB.calls != 1 {
		t.Fatalf("备胎上游（流式）应被调 1 次，实际 %d", upB.calls)
	}
	ld.Flush()
	rows := st.Logs(5)
	if len(rows) != 1 || rows[0].Model != "workbuddy/glm-free" || rows[0].Status != http.StatusOK || rows[0].Mode != "流" {
		t.Fatalf("请求日志应记备胎+200+流式，实际 %+v", rows)
	}
	if rows[0].Tok != 7 {
		t.Errorf("请求日志 Tok 应为 completion 7，实际 %d", rows[0].Tok)
	}
	ues := readUsageToday(t, ledgerDir)
	if len(ues) != 1 || ues[0].Model != "glm-free" || ues[0].Ch != "workbuddy" || ues[0].PT != 11 || ues[0].CT != 7 {
		t.Fatalf("token 流水应记备胎（Model=glm-free Ch=workbuddy PT=11 CT=7），实际 %+v", ues)
	}
	if ms := st.Snapshot(time.Now()).Models; len(ms) != 1 || ms[0].Model != "workbuddy/glm-free" || ms[0].Tokens != 18 {
		t.Fatalf("今日模型消耗应归备胎（18 token），实际 %+v", ms)
	}
	ld.Close() // Windows：关文件句柄，否则 t.TempDir 清理失败
}
