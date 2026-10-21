// 模型降级链行为锁定：主模型全冷 → 按链顶备胎（X-Model-Fallback 标注）；
// 未全冷不触发；链按顺序找第一个有容量的备胎；无规则保持原 503 行为。
// 背景：2026-10-13 workbuddyai/deepseek-v4.1-flash 时段性平台频控（14003）把
// 3 个账号全部打进模型级冷却，fail-fast 503 打断用户任务——本机制让用户可
// 预配置备胎链自动顶上，上游解冻后按请求自动切回（不粘滞）。
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
	"wild-work/internal/oczen"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/stats"
)

// newFbHandler 装配两个渠道（oczen=主、workbuddy=备）的 handler，各一个账号。
func newFbHandler(t *testing.T, upA, upB provider.Upstream, rules []config.ModelFallbackRule) (*Handler, *pool.Pool, *pool.Pool) {
	t.Helper()
	pa := pool.New(t.TempDir() + "/state-a.json")
	pa.Add(oczen.AnonymousAuth())
	pb := pool.New(t.TempDir() + "/state-b.json")
	pb.Add(&auth.Auth{UID: "backup-uid"})
	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.Oczen:     {Kind: provider.Oczen, Pool: pa, Upstream: upA},
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: pb, Upstream: upB},
		},
		APIKey: "k", HardCooldown: time.Hour, SoftCooldown: time.Minute,
		ErrThreshold: 1, ErrCooldown: time.Minute,
		ModelFallbacks: rules,
	})
	return h, pa, pb
}

// callModel 发一次非流式 chat 请求（可指定模型），返回响应记录。
func callModel(h *Handler, model string) *httptest.ResponseRecorder {
	body := `{"model":"` + model + `","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestModelFallbackTriggersWhenAllCooling 主模型全冷 → 备胎顶上：上游 B 收到请求、
// 响应 200、带 X-Model-Fallback 标注。
func TestModelFallbackTriggersWhenAllCooling(t *testing.T) {
	upA := &codeUpstream{status: 200}
	upB := &codeUpstream{status: 200}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	// 主渠道唯一账号对 big-pickle 模型级冷却（账号本身健康）
	// （返回值=是否升级为整号冷却，首次调用恒 false，不校验）
	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", pool.CoolSoft, 10*time.Minute, "test")
	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK {
		t.Fatalf("应命中降级链返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Model-Fallback"); got != "oczen/big-pickle -> workbuddy/glm-free" {
		t.Errorf("X-Model-Fallback = %q，want oczen/big-pickle -> workbuddy/glm-free", got)
	}
	if upB.calls != 1 {
		t.Errorf("备胎渠道上游收到 %d 次请求，want 1", upB.calls)
	}
	if upA.calls != 0 {
		t.Errorf("主渠道上游不应收到请求（挑号阶段已全冷），实际 %d 次", upA.calls)
	}
}

// fbUpstream 假上游：Aggregate 按官方语义回填客户端模型名（resp["model"]=入参），
// 用于验证降级时响应 model 字段暴露实际备胎（agent 感知通道）。
type fbUpstream struct {
	calls int
}

func (u *fbUpstream) RefreshToken(*auth.Auth) error                                 { return nil }
func (u *fbUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error)          { return nil, nil }
func (u *fbUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) { return nil, nil }
func (u *fbUpstream) UserResource(*auth.Auth) (int64, error)                        { return 0, nil }
func (u *fbUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (u *fbUpstream) DailyCheckin(*auth.Auth) error         { return nil }
func (u *fbUpstream) Classify(int, string) provider.ErrKind { return provider.ErrServer }
func (u *fbUpstream) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	u.calls++
	return io.NopCloser(strings.NewReader("data: [DONE]\n\n")), 200, nil, nil
}
func (u *fbUpstream) Stream(http.ResponseWriter, io.Reader, string) (map[string]any, error) {
	return nil, nil
}
func (u *fbUpstream) Aggregate(_ io.Reader, model string) (map[string]any, error) {
	return map[string]any{"model": model, "choices": []any{}}, nil
}

// TestModelFallbackExposesActualModelInBody 降级后响应体 model 字段 = 实际备胎全路径
// （agent 的感知通道：OpenAI 协议 model 字段=「实际执行的模型」；X-Model-Fallback 头
// 很多客户端不读，body.model 是更可靠的信号）。无降级时保持官方「回填请求名」不变。
func TestModelFallbackExposesActualModelInBody(t *testing.T) {
	upA, upB := &fbUpstream{}, &fbUpstream{}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	// 无降级：model 字段保持客户端请求名（官方语义）
	rec := callModel(h, "oczen/big-pickle")
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["model"] != "oczen/big-pickle" {
		t.Errorf("无降级时 model=%v，want 客户端请求名 oczen/big-pickle", resp["model"])
	}
	// 降级：model 字段 = 备胎全路径
	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", pool.CoolSoft, 10*time.Minute, "test")
	rec2 := callModel(h, "oczen/big-pickle")
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp)
	if resp["model"] != "workbuddy/glm-free" {
		t.Errorf("降级后 model=%v，want 备胎 workbuddy/glm-free（agent 感知）", resp["model"])
	}
}

// TestModelFallbackFeedsStatsEvents 降级/恢复事件喂入运行统计（面板「降级记录」）：
// 降级命中记 Active 事件；原模型容量回归后的下一请求关闭事件（恢复）。
func TestModelFallbackFeedsStatsEvents(t *testing.T) {
	st, err := stats.New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	upA, upB := &fbUpstream{}, &fbUpstream{}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	h.stats = st // 测试直装（newFbHandler 未暴露 Stats 配置；同包可见）

	// 健康请求：恢复探针空转，无事件
	callModel(h, "oczen/big-pickle")
	if n := len(st.Snapshot(time.Now()).Fallbacks); n != 0 {
		t.Fatalf("健康请求不应产生事件: %d 条", n)
	}

	// 全冷 → 降级：事件 Active（from=请求名，to=备胎）
	// （短冷却 50ms + sleep，同 TestModelFallbackUnfreezeAutoSwitchBack 的解冻手法；
	// ClearPenalty 不清模型级冷却，不能用于此处解冻）
	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", pool.CoolSoft, 50*time.Millisecond, "test")
	callModel(h, "oczen/big-pickle")
	evs := st.Snapshot(time.Now()).Fallbacks
	if len(evs) != 1 || !evs[0].Active || evs[0].From != "oczen/big-pickle" || evs[0].To != "workbuddy/glm-free" {
		t.Fatalf("降级事件不符: %+v", evs)
	}

	// 解冻 → 下一请求走原模型并顺带关闭事件
	time.Sleep(80 * time.Millisecond)
	callModel(h, "oczen/big-pickle")
	evs = st.Snapshot(time.Now()).Fallbacks
	if len(evs) != 1 || evs[0].Active || evs[0].Recovered == "" || evs[0].DurSec < 0 {
		t.Fatalf("恢复后事件不符: %+v", evs)
	}
}

// TestModelFallbackNotTriggeredWhenHealthy 未全冷 → 走原模型，不降级、无标注头。
func TestModelFallbackNotTriggeredWhenHealthy(t *testing.T) {
	upA := &codeUpstream{status: 200}
	upB := &codeUpstream{status: 200}
	h, _, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK {
		t.Fatalf("健康时应正常返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Model-Fallback") != "" {
		t.Error("健康时不应有 X-Model-Fallback 头")
	}
	if upA.calls != 1 || upB.calls != 0 {
		t.Errorf("upA=%d upB=%d，want 1/0（未降级）", upA.calls, upB.calls)
	}
}

// TestModelFallbackChainOrder 链内按顺序找第一个有容量的备胎：
// 第一个备胎也在备胎渠道全冷 → 自动落到第二个。
func TestModelFallbackChainOrder(t *testing.T) {
	upA := &codeUpstream{status: 200}
	upB := &codeUpstream{status: 200}
	h, pa, pb := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/m-cool", "workbuddy/glm-free"}},
	})
	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", pool.CoolSoft, 10*time.Minute, "test")
	pb.CooldownModel("backup-uid", "m-cool", pool.CoolSoft, 10*time.Minute, "test")
	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK {
		t.Fatalf("链内应落到第二个备胎，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Model-Fallback"); got != "oczen/big-pickle -> workbuddy/glm-free" {
		t.Errorf("X-Model-Fallback = %q，want 第二个备胎 glm-free", got)
	}
}

// TestModelFallbackNoRuleKeeps503 全冷但无规则 → 保持原 no_healthy_account 503 行为。
func TestModelFallbackNoRuleKeeps503(t *testing.T) {
	upA := &codeUpstream{status: 200}
	upB := &codeUpstream{status: 200}
	h, pa, _ := newFbHandler(t, upA, upB, nil)
	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", pool.CoolSoft, 10*time.Minute, "test")
	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无规则时应保持 503，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no_healthy_account") {
		t.Errorf("应保持 no_healthy_account 语义：%s", rec.Body.String())
	}
}

// TestModelFallbackUnfreezeAutoSwitchBack 不粘滞：冷却过期后自动切回原模型
// （下一个请求按原模型重新判定，不再碰备胎）。
func TestModelFallbackUnfreezeAutoSwitchBack(t *testing.T) {
	upA := &codeUpstream{status: 200}
	upB := &codeUpstream{status: 200}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	// 冷却 50ms：先触发降级，随后过期自动切回
	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", pool.CoolSoft, 50*time.Millisecond, "test")
	rec := callModel(h, "oczen/big-pickle")
	if rec.Header().Get("X-Model-Fallback") == "" {
		t.Fatalf("冷却期内应降级，实际未降级：%d", rec.Code)
	}
	time.Sleep(80 * time.Millisecond)
	rec2 := callModel(h, "oczen/big-pickle")
	if rec2.Header().Get("X-Model-Fallback") != "" {
		t.Error("冷却过期后仍降级——违反「不粘滞、自动切回」契约")
	}
	if upA.calls != 1 {
		t.Errorf("解冻后原模型上游应收到 1 次请求，实际 %d", upA.calls)
	}
}

// TestModelFallbackFromOtherChannelIgnores 规则 From 渠道不匹配时忽略
// （oczen 的全冷不吃 workbuddy 规则，反之亦然）。
func TestModelFallbackFromOtherChannelIgnores(t *testing.T) {
	upA := &codeUpstream{status: 200}
	upB := &codeUpstream{status: 200}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "workbuddy/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", pool.CoolSoft, 10*time.Minute, "test")
	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("From 渠道不匹配的规则不应触发，应保持 503，实际 %d", rec.Code)
	}
}

// TestSetModelFallbacksHotReload 热更新：SetModelFallbacks 后立即按新链路由
// （无重启；面板保存即生效的契约）。
func TestSetModelFallbacksHotReload(t *testing.T) {
	upA := &codeUpstream{status: 200}
	upB := &codeUpstream{status: 200}
	h, pa, _ := newFbHandler(t, upA, upB, nil)
	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", pool.CoolSoft, 10*time.Minute, "test")
	if rec := callModel(h, "oczen/big-pickle"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无规则时应 503，实际 %d", rec.Code)
	}
	h.SetModelFallbacks([]config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK || rec.Header().Get("X-Model-Fallback") == "" {
		t.Fatalf("热更新后应立即降级生效，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

// TestModelFallbackRecoveryNotFakedWhenBackupAlsoCold 原模型仍全冷、只是备胎也没容量时，
// 不得把进行中事件谎报成「已恢复」；去掉 pickModelFallback 的 primaryAvail 判据即失败
// （旧实现「未降级」分支无条件调 NoteFallbackRecovery，面板会先显示恢复再重开一条事件）。
func TestModelFallbackRecoveryNotFakedWhenBackupAlsoCold(t *testing.T) {
	st, err := stats.New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	upA, upB := &fbUpstream{}, &fbUpstream{}
	h, pa, pb := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	h.stats = st

	pa.CooldownModel(oczen.AnonymousUID, "big-pickle", pool.CoolSoft, 300*time.Millisecond, "test")
	callModel(h, "oczen/big-pickle") // 降级命中 → 事件 Active
	pb.CooldownModel("backup-uid", "glm-free", pool.CoolSoft, time.Hour, "test")
	if rec := callModel(h, "oczen/big-pickle"); rec.Code == http.StatusOK {
		t.Fatal("备胎也全冷时应回到原 503 路径")
	}
	if evs := st.Snapshot(time.Now()).Fallbacks; len(evs) != 1 || !evs[0].Active {
		t.Fatalf("原模型仍全冷，事件必须保持进行中：%+v", evs)
	}
	// 原模型解冻 → 下一请求才真正算恢复
	time.Sleep(320 * time.Millisecond)
	callModel(h, "oczen/big-pickle")
	if evs := st.Snapshot(time.Now()).Fallbacks; len(evs) != 1 || evs[0].Active || evs[0].Recovered == "" {
		t.Fatalf("解冻后事件应关闭且只有一条：%+v", evs)
	}
}

// TestModelFallbackCrossChannelWhenWholeChannelCold 整渠道全灭（无健康账号）时
// 跨渠道备胎仍须顶上：唯一账号整号冷却后，请求不应直接 503，而应落到另一渠道的备胎。
// 去掉 pickModelFallback 的 !hasHealthy 提前返回即失败（这正是本次修复的目标）。
func TestModelFallbackCrossChannelWhenWholeChannelCold(t *testing.T) {
	upA := &codeUpstream{status: 200}
	upB := &codeUpstream{status: 200}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	// 唯一账号整号冷却 ⇒ 渠道无健康账号（整渠道全灭形态）。
	// 上游版用「多模型接连限流升级整号冷却」构造；本仓无该升级，账号级冷却等效。
	pa.Cooldown(oczen.AnonymousUID, pool.CoolSoft, 10*time.Minute, "test")
	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK {
		t.Fatalf("整渠道全灭时应跨渠道降级返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Model-Fallback"); got != "oczen/big-pickle -> workbuddy/glm-free" {
		t.Errorf("X-Model-Fallback = %q，want 跨渠道备胎 workbuddy/glm-free", got)
	}
	if upB.calls != 1 {
		t.Errorf("备胎渠道上游收到 %d 次请求，want 1", upB.calls)
	}
	if upA.calls != 0 {
		t.Errorf("主渠道上游不应收到请求（整渠道已全灭），实际 %d 次", upA.calls)
	}
}

// TestModelFallbackCrossChannelWhenChannelDisabled 整渠道被禁用（同理无健康账号）
// 时同样跨渠道降级，不得直接 503。
func TestModelFallbackCrossChannelWhenChannelDisabled(t *testing.T) {
	upA := &codeUpstream{status: 200}
	upB := &codeUpstream{status: 200}
	h, pa, _ := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	pa.Disable(oczen.AnonymousUID, "test")
	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusOK {
		t.Fatalf("渠道禁用时应跨渠道降级返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Model-Fallback"); got != "oczen/big-pickle -> workbuddy/glm-free" {
		t.Errorf("X-Model-Fallback = %q，want 跨渠道备胎", got)
	}
	if upB.calls != 1 || upA.calls != 0 {
		t.Errorf("upA=%d upB=%d，want 0/1（跨渠道降级）", upA.calls, upB.calls)
	}
}

// TestModelFallbackWholeChannelColdNoBackupKeeps503 整渠道全灭且备胎渠道也无容量时，
// 仍回到原 no_healthy_account 503 路径（不得因删了提前返回而改变「无备胎可用」的收尾）。
func TestModelFallbackWholeChannelColdNoBackupKeeps503(t *testing.T) {
	upA := &codeUpstream{status: 200}
	upB := &codeUpstream{status: 200}
	h, pa, pb := newFbHandler(t, upA, upB, []config.ModelFallbackRule{
		{From: "oczen/big-pickle", To: []string{"workbuddy/glm-free"}},
	})
	pa.Disable(oczen.AnonymousUID, "test")
	pb.CooldownModel("backup-uid", "glm-free", pool.CoolSoft, time.Hour, "test")
	rec := callModel(h, "oczen/big-pickle")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("无可用备胎时应保持 503，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if upB.calls != 0 {
		t.Errorf("备胎渠道已全冷，不应收到请求，实际 %d 次", upB.calls)
	}
}
