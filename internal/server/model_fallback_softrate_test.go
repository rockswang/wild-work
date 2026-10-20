// 模型饱和（14003 / 6004）请求内无感降级（补丁 0024）行为锁定：
//
//	修复前：请求内降级只认 ErrHardCredit（积分耗尽）⇒ 模型饱和时轮转耗尽后
//	        原样透传 429，客户端在饱和时段的**第一个**请求必然失败，第二个请求
//	        才由入口降级链顶上（2026-10-17 app.log 20:46:00 / 20:46:01 实录）。
//	修复后：ErrSoftRate 同样触发请求内降级——客户端全程 200，X-Model-Fallback
//	        头 + 响应 model 字段标注实际备胎；仅重入一次；单账号渠道仍豁免。
//	不改：内容类/请求类/5xx 等错误不换模型（换模型必然复现，只白烧备胎额度）。
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

// body14003 生产实录原文 body（2026-10-17 app.log，workbuddyai uid=e840f08f…，
// requestId=24926fbed651df1824926fbed651df18）。
const body14003 = `{"code":14003,"msg":"too many requests","displayMsg":{"en":"Model busy. Please switch models or retry later","zh":"模型繁忙，请换模型或稍后重试","zh-hant":"模型繁忙，請更換模型或稍後重試"},"displayTips":{"en":"This model is currently saturated. It is not a network issue. Please switch to another model or wait a moment and retry."},"requestId":"24926fbed651df1824926fbed651df18"}`

// body6004 按模型限额原文（带重置时间戳，softRateCooldown 会解析到该时刻）。
const body6004 = `{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your usage will reset at 2026-10-18 07:14:59 UTC+8, alternatively, you can switch to the other models to continue using it.","requestId":"c0ffee"}`

// softRateUpstream 模拟模型饱和渠道：ChatStream 恒回 429+指定 body；Classify 委托
// 真实 workbuddyai.Classify——生产 WorkBuddyAI 渠道的分类器即它，本文件锁的是
// 「分类 → 模型级冷却 → 轮转耗尽 → 请求内降级」全链路，而非仅测 handler 对某个
// kind 的处置。借用 newFbHandler 的 oczen+workbuddy 两渠道骨架（轮转/降级处置
// 逻辑与渠道无关），装配形态与 14018 测试同构。
type softRateUpstream struct {
	calls int
	body  string
}

func (u *softRateUpstream) RefreshToken(*auth.Auth) error                        { return nil }
func (u *softRateUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) { return nil, nil }
func (u *softRateUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (u *softRateUpstream) UserResource(*auth.Auth) (int64, error) { return 0, nil }
func (u *softRateUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (u *softRateUpstream) DailyCheckin(*auth.Auth) error { return nil }
func (u *softRateUpstream) Classify(status int, body string) provider.ErrKind {
	return workbuddyai.Classify(status, body)
}
func (u *softRateUpstream) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	u.calls++
	body := u.body
	if body == "" {
		body = body14003
	}
	return nil, http.StatusTooManyRequests, []byte(body), nil
}
func (u *softRateUpstream) Stream(http.ResponseWriter, io.Reader, string) (map[string]any, error) {
	return nil, nil
}
func (u *softRateUpstream) Aggregate(_ io.Reader, model string) (map[string]any, error) {
	return map[string]any{"model": model, "choices": []any{}}, nil
}

// TestSoftRateInRequestFallbackRescuesClient 主场景（生产实录复刻）：双账号对同一
// 模型都撞 14003 → 轮转耗尽当场切备胎重入 → 客户端 200（不再吃 429）、
// X-Model-Fallback 与响应 model 均指向备胎；两账号只进**模型级**冷却（14003 是
// 模型饱和而非账号余额问题，不得升级整号冷却）。
func TestSoftRateInRequestFallbackRescuesClient(t *testing.T) {
	upA := &softRateUpstream{}
	upB := &fbUpstream{}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	pa.Add(&auth.Auth{UID: "main-2"}) // 生产同形：workbuddyai 双账号

	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK {
		t.Fatalf("模型饱和时请求内降级应让客户端拿到 200（无感），实际 %d：%s", rec.Code, rec.Body.String())
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
		if s.Cooling {
			t.Errorf("14003 是模型级饱和，账号 %s 不应整号冷却（Reason=%q）", s.UID, s.Reason)
		}
		if len(s.ModelCooling) != 1 || !modelCooling(s, "big-pickle") {
			t.Errorf("账号 %s 应对 big-pickle 有模型级冷却，实际 %v", s.UID, s.ModelCooling)
		}
	}
}

// TestSoftRate6004InRequestFallbackRescuesClient 6004（按模型限额、带重置时间戳）
// 同属 ErrSoftRate，必须同样触发请求内降级——防止有人把判定写死成只看 14003 文案。
func TestSoftRate6004InRequestFallbackRescuesClient(t *testing.T) {
	upA := &softRateUpstream{body: body6004}
	upB := &fbUpstream{}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	pa.Add(&auth.Auth{UID: "main-2"})

	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK {
		t.Fatalf("6004 模型限额也应请求内降级返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if upA.calls != 2 || upB.calls != 1 {
		t.Errorf("A=%d B=%d，want A=2 B=1", upA.calls, upB.calls)
	}
}

// TestSoftRateBackupAlsoBusyTransparentOnce：备胎也饱和 → 重入仅一次后原样透传
// （备胎渠道的 14003），不无限递归降级。
func TestSoftRateBackupAlsoBusyTransparentOnce(t *testing.T) {
	upA := &softRateUpstream{}
	upB := &softRateUpstream{}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	pa.Add(&auth.Auth{UID: "main-2"}) // 双账号：生产同形，逐个撞墙后才耗尽
	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("备胎也饱和应透传 429，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"code":14003`) {
		t.Fatalf("透传应保留上游原始 body（14003），实际：%s", rec.Body.String())
	}
	if upA.calls != 2 || upB.calls != 1 {
		t.Errorf("重入应一次封顶：A=2（双账号耗尽）B=1（备胎也饱和即停），实际 A=%d B=%d", upA.calls, upB.calls)
	}
}

// TestSoftRateSingleAccountChannelExempted：单账号渠道（oczen 形态）仍豁免请求内
// 重入——软限流在 SingleAccount 分支本就不罚账号，直接原文透传；降级由下一请求的
// 入口链承担（惩罚豁免不变量，补丁 0024 不改变该约束）。
func TestSoftRateSingleAccountChannelExempted(t *testing.T) {
	upA := &softRateUpstream{}
	upB := &fbUpstream{}
	h, _, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	h.cfg.Runtimes[provider.Oczen].SingleAccount = true

	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"code":14003`) {
		t.Fatalf("单账号渠道应原样透传 429/14003，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if upB.calls != 0 {
		t.Errorf("单账号渠道不应请求内重入，备胎上游被调 %d 次", upB.calls)
	}
}

// TestSoftRateInRequestFallbackAccounting：请求内降级的请求，请求日志/token 流水/
// 今日模型消耗全部记**备胎**（0021/PR #148 口径），且请求日志 Status=200
// （statRow.Status 清零生效，不把成功记成 429）。
func TestSoftRateInRequestFallbackAccounting(t *testing.T) {
	upA := &softRateUpstream{}
	upB := &acctUpstream{} // 上报 11 prompt + 7 completion
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	pa.Add(&auth.Auth{UID: "main-2"})
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

// TestSoftRateInRequestFallbackSkipsSameChannel：请求内重入跳过同渠道备胎，
// 继续往链后找跨渠道候选。
//
// 背景（对抗性复审 Warning）：降级链里允许配同渠道兄弟模型（生产配置
// workbuddyai/deepseek-v4.1-flash → workbuddyai/deepseek-v4.1-flash-sg）。同渠道
// 备胎走的是同一批账号——若本轮 429 实为账号级限流（traework/qwenwork/qodercn/
// qodercom 等把任意 429 都归 ErrSoftRate），重打一遍必然二次撞墙，而 pool 的
// 「同账号 ≥2 模型接连限流 → 整号冷却」会把 60s 限流升级成长时间整号冻结。
func TestSoftRateInRequestFallbackSkipsSameChannel(t *testing.T) {
	upA := &softRateUpstream{}
	upB := &fbUpstream{}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		// 链首是同渠道兄弟模型（必须被跳过），链尾才是跨渠道备胎
		{From: "oczen/big-pickle", To: []string{"oczen/other-model", "workbuddy/glm-free"}},
	})
	pa.Add(&auth.Auth{UID: "main-2"})

	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK {
		t.Fatalf("应跳过同渠道备胎并由跨渠道备胎顶上返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Model-Fallback"); got != "oczen/big-pickle -> workbuddy/glm-free" {
		t.Errorf("X-Model-Fallback = %q，want 指向跨渠道备胎 workbuddy/glm-free", got)
	}
	if upB.calls != 1 {
		t.Errorf("跨渠道备胎应被调用 1 次，实际 %d", upB.calls)
	}
	for _, s := range pa.List() {
		if modelCooling(s, "other-model") {
			t.Errorf("同渠道兄弟模型不应被请求内重入打一遍（账号 %s 已记 other-model 冷却）", s.UID)
		}
	}
}

// TestSoftRateInRequestFallbackOnlySameChannelStaysTransparent：降级链只剩同渠道
// 备胎时，请求内不重入、原样透传 429——宁可让客户端看到真实限流，也不拿同一批
// 账号再撞一次墙（代价是把 60s 限流升级成整号冻结，且备份必然失败）。
// 入口降级不受此限（见 TestEntryFallbackAllowsSameChannel）。
func TestSoftRateInRequestFallbackOnlySameChannelStaysTransparent(t *testing.T) {
	upA := &softRateUpstream{}
	upB := &fbUpstream{}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"oczen/other-model"}},
	})
	pa.Add(&auth.Auth{UID: "main-2"})

	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"code":14003`) {
		t.Fatalf("同渠道备胎不可用时应原样透传 429/14003，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if upB.calls != 0 {
		t.Errorf("跨渠道备胎上游不应被调用，实际 %d 次", upB.calls)
	}
	if upA.calls != 2 {
		t.Errorf("主渠道应只在原模型上各撞一次（双账号 2 次），实际 %d 次——多出即说明同渠道兄弟模型被重打", upA.calls)
	}
}

// TestEntryFallbackAllowsSameChannel：入口降级**不**跳过同渠道备胎——那里
// 「原模型在本渠道已全冷」是既有证据，换同渠道兄弟模型正是降级链配置的本意
// （上游配额按模型计，模型 A 冷不代表模型 B 也冷）。收紧只针对请求内重入。
func TestEntryFallbackAllowsSameChannel(t *testing.T) {
	upA := &fbUpstream{}
	upB := &fbUpstream{}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"oczen/other-model"}},
	})
	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", pool.CoolSoft, 10*time.Minute, "test")

	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK {
		t.Fatalf("入口降级应允许同渠道备胎返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Model-Fallback"); got != "oczen/big-pickle -> oczen/other-model" {
		t.Errorf("X-Model-Fallback = %q，want oczen/big-pickle -> oczen/other-model", got)
	}
	if upB.calls != 0 {
		t.Errorf("备胎在 oczen 渠道内，workbuddy 上游不应被调用，实际 %d 次", upB.calls)
	}
}

// TestFallbackWorthyKinds 白名单锁定：只有「本渠道确实顶不住」的两类错误换模型。
// 误把 ErrServer/ErrClient/内容类错误纳入会导致每个上游抖动都白烧一份备胎额度。
func TestFallbackWorthyKinds(t *testing.T) {
	cases := []struct {
		kind provider.ErrKind
		want bool
	}{
		{provider.ErrHardCredit, true},
		{provider.ErrSoftRate, true},
		{provider.ErrServer, false},
		{provider.ErrClient, false},
		{provider.ErrSessionDead, false},
		{provider.ErrNotFound, false},
		{provider.ErrContentBlocked, false},
		{provider.ErrPromptTooLong, false},
		{provider.ErrPassthrough, false},
		{provider.ErrNone, false},
	}
	for _, c := range cases {
		if got := fallbackWorthy(c.kind); got != c.want {
			t.Errorf("fallbackWorthy(%s) = %v，want %v", c.kind, got, c.want)
		}
	}
}

// TestRotateSuccessLogsStatus200 轮转内换号成功的请求，请求日志必须记 200。
// 修复前：status>=400 分支无条件写 statRow.Status=status 且成功分支从不复位，
// 「A 号 429、B 号成功」的请求在面板/请求日志里被记成 429——模型饱和是多账号
// 渠道的高频形态，这会让降级是否生效完全看不出来。
func TestRotateSuccessLogsStatus200(t *testing.T) {
	up := &kindedUpstream{
		status:  http.StatusTooManyRequests,
		kind:    provider.ErrSoftRate,
		body:    body6004,
		succeed: func(uid string) bool { return uid == "eB" }, // A 号撞墙、B 号成功
	}
	h := newRotateTestHandler(t, up, "e")
	st, err := stats.New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	h.stats = st

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"glm/m","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer k")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("B 号成功应回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	rows := st.Logs(5)
	if len(rows) != 1 {
		t.Fatalf("请求日志应一行，实际 %d 行", len(rows))
	}
	if rows[0].Status != http.StatusOK {
		t.Errorf("轮转内换号成功应记 200，实际 %d（statRow.Status 未复位）", rows[0].Status)
	}
}
