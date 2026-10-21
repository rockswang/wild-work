package app

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// fakeUpstream 只实现 UserResource / RefreshToken，其余为空实现。
type fakeUpstream struct {
	resourceCalls atomic.Int32
	remain        int64
	failResource  atomic.Bool
	// sessionDead 为 true 时 UserResourceDetail 先报一次 401（模拟本地 token 已失效），
	// 刷新后恢复——用于验证「401 自愈」。
	sessionDead  atomic.Bool
	refreshCalls atomic.Int32
}

func (f *fakeUpstream) RefreshToken(a *auth.Auth) error {
	f.refreshCalls.Add(1)
	f.sessionDead.Store(false) // 刷新即恢复
	return nil
}

func (f *fakeUpstream) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	return nil, 200, nil, nil
}

func (f *fakeUpstream) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) { return nil, nil }

func (f *fakeUpstream) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

func (f *fakeUpstream) UserResource(a *auth.Auth) (int64, error) {
	f.resourceCalls.Add(1)
	if f.failResource.Load() {
		return 0, io.ErrUnexpectedEOF
	}
	return f.remain, nil
}

// UserResourceDetail 与 UserResource 同源：remain 即可用余额，unusable 固定 0。
// 计入 resourceCalls，因为生产代码的刷新路径只走 Detail（不再调 UserResource）。
func (f *fakeUpstream) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	f.resourceCalls.Add(1)
	if f.sessionDead.Load() {
		return 0, nil, &provider.Error{Kind: provider.ErrSessionDead, Status: 401, Msg: "token invalid"}
	}
	if f.failResource.Load() {
		return 0, nil, io.ErrUnexpectedEOF
	}
	return f.remain, []provider.ResourceItem{{Name: "套餐", Remain: f.remain, Usable: true}}, nil
}

func (f *fakeUpstream) DailyCheckin(a *auth.Auth) error { return nil }

func (f *fakeUpstream) Classify(status int, body string) provider.ErrKind {
	return provider.ErrNone
}

func (f *fakeUpstream) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	return nil, nil
}

func (f *fakeUpstream) Aggregate(r io.Reader, model string) (map[string]any, error) { return nil, nil }

// newTestApp 构造仅含单个渠道的最小 App。
func newTestApp(t *testing.T, kind provider.Kind, up provider.Upstream, uid string) (*App, *pool.Pool) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuthDir = dir

	p := pool.New(filepath.Join(dir, "state-"+kind.String()+".json"))
	p.Add(&auth.Auth{Kind: kind.String(), UID: uid, AccessToken: "t", RefreshToken: "r", Domain: "www.workbuddy.ai"})

	a, err := New(Options{
		ConfigPath: filepath.Join(dir, "config.json"),
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			kind: {Kind: kind, Pool: p, Upstream: up},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a, p
}

// TestCreditAutoRefreshRunsImmediatelyAndPeriodically 启动即刷一次，之后按间隔重复。
func TestCreditAutoRefreshRunsImmediatelyAndPeriodically(t *testing.T) {
	up := &fakeUpstream{remain: 347}
	a, p := newTestApp(t, provider.WorkBuddyAI, up, "uid-1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartCreditAutoRefresh(ctx, []provider.Kind{provider.WorkBuddyAI}, 120*time.Millisecond)

	// 启动即刷：等待首次写入
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := p.Status("uid-1"); ok && st.Credits == 347 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st, _ := p.Status("uid-1"); st.Credits != 347 {
		t.Fatalf("启动未自动刷新积分，credits=%d", st.Credits)
	}

	// 周期性：等待至少再来一次
	first := up.resourceCalls.Load()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if up.resourceCalls.Load() > first {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if up.resourceCalls.Load() <= first {
		t.Fatalf("未按间隔重复刷新（calls=%d）", up.resourceCalls.Load())
	}
}

// TestCreditAutoRefreshScopedToKinds 未传入的渠道不应被刷新（保护国内版行为）。
func TestCreditAutoRefreshScopedToKinds(t *testing.T) {
	wba := &fakeUpstream{remain: 100}
	wb := &fakeUpstream{remain: 999}
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuthDir = dir

	pA := pool.New(filepath.Join(dir, "state-a.json"))
	pA.Add(&auth.Auth{Kind: "workbuddyai", UID: "a", AccessToken: "t", RefreshToken: "r"})
	pB := pool.New(filepath.Join(dir, "state-b.json"))
	pB.Add(&auth.Auth{Kind: "workbuddy", UID: "b", AccessToken: "t", RefreshToken: "r"})

	a, err := New(Options{
		ConfigPath: filepath.Join(dir, "config.json"),
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddyAI: {Kind: provider.WorkBuddyAI, Pool: pA, Upstream: wba},
			provider.WorkBuddy:   {Kind: provider.WorkBuddy, Pool: pB, Upstream: wb},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartCreditAutoRefresh(ctx, []provider.Kind{provider.WorkBuddyAI}, 80*time.Millisecond)
	time.Sleep(500 * time.Millisecond)

	if wba.resourceCalls.Load() == 0 {
		t.Errorf("workbuddyai 应被刷新")
	}
	if n := wb.resourceCalls.Load(); n != 0 {
		t.Errorf("workbuddy（国内版）不应被此循环刷新，却调用了 %d 次", n)
	}
	if st, _ := pB.Status("b"); st.Credits != 0 {
		t.Errorf("国内版积分被改动：%d", st.Credits)
	}
}

// TestCreditAutoRefreshSurvivesFailure 单账号失败不应中断循环。
func TestCreditAutoRefreshSurvivesFailure(t *testing.T) {
	up := &fakeUpstream{remain: 50}
	up.failResource.Store(true)
	a, p := newTestApp(t, provider.WorkBuddyAI, up, "uid-x")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.StartCreditAutoRefresh(ctx, []provider.Kind{provider.WorkBuddyAI}, 60*time.Millisecond)
	time.Sleep(300 * time.Millisecond)

	// 放开失败，循环应继续并在下个周期成功
	up.failResource.Store(false)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := p.Status("uid-x"); ok && st.Credits == 50 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("失败后未恢复刷新，calls=%d", up.resourceCalls.Load())
}

// TestCreditAutoRefreshCancels 取消 ctx 后应停止刷新。
func TestCreditAutoRefreshCancels(t *testing.T) {
	up := &fakeUpstream{remain: 10}
	a, _ := newTestApp(t, provider.WorkBuddyAI, up, "uid-c")

	ctx, cancel := context.WithCancel(context.Background())
	a.StartCreditAutoRefresh(ctx, []provider.Kind{provider.WorkBuddyAI}, 50*time.Millisecond)
	time.Sleep(200 * time.Millisecond)
	cancel()
	time.Sleep(150 * time.Millisecond)

	after := up.resourceCalls.Load()
	time.Sleep(300 * time.Millisecond)
	if up.resourceCalls.Load() != after {
		t.Errorf("取消后仍在刷新：%d → %d", after, up.resourceCalls.Load())
	}
}

// ---------------------------------------------------------------------------
// 模型列表 ⊕ 费率 的合并逻辑
// ---------------------------------------------------------------------------

// TestBuildFeesChannels 以 models 列表为基准，逐行配费率；
// 无对应费率条目的模型标记 Priced=false（前端显示 unknown）。
func TestBuildFeesChannels(t *testing.T) {
	models := map[provider.Kind][]provider.ModelInfo{
		provider.WorkBuddyAI: {
			{ID: "hy3", ContextWindow: 192000},                  // 有费率且免费
			{ID: "glm-5.2", ContextWindow: 1000000},             // 有费率且付费
			{ID: "deepseek-v4.1-flash", ContextWindow: 1000000}, // 硬编码，无费率
		},
	}
	pricing := []provider.ModelPricing{
		{Channel: "workbuddyai", Model: "hy3", Rate: 0, Note: "Free now"},
		{Channel: "workbuddyai", Model: "glm-5.2", Rate: 0.79},
		// 上游多返回一个 models 列表里没有的模型 → 不应出现在结果中
		{Channel: "workbuddyai", Model: "ghost-model", Rate: 9.99},
	}

	got := buildFeesChannels(models, pricing, []provider.Kind{provider.WorkBuddyAI}, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 channel, got %d", len(got))
	}
	rows := got[0].Models
	if len(rows) != 3 {
		t.Fatalf("want 3 rows (只按 models 列表), got %d: %+v", len(rows), rows)
	}
	byID := map[string]feesModelRow{}
	for _, r := range rows {
		byID[r.Model] = r
	}
	if r := byID["hy3"]; !r.Priced || !r.Free || r.Rate != 0 || r.Note != "Free now" {
		t.Errorf("hy3 应为已定价且免费: %+v", r)
	}
	if r := byID["glm-5.2"]; !r.Priced || r.Free || r.Rate != 0.79 {
		t.Errorf("glm-5.2 应为已定价付费: %+v", r)
	}
	if r := byID["deepseek-v4.1-flash"]; r.Priced {
		t.Errorf("硬编码模型应标记为未定价（unknown）: %+v", r)
	}
	if _, ok := byID["ghost-model"]; ok {
		t.Errorf("费率接口中多余、且不在 models 列表的模型不应展示")
	}
	// 排序：免费在前，付费次之，未定价置后
	if rows[0].Model != "hy3" || rows[1].Model != "glm-5.2" || rows[2].Model != "deepseek-v4.1-flash" {
		t.Errorf("排序应为 免费→付费→未定价，实得 %s,%s,%s", rows[0].Model, rows[1].Model, rows[2].Model)
	}
}

// TestBuildFeesChannelsNoPricing 无费率数据时全部为 unknown，不应丢失模型。
// TestFeeChannelOrderCoversAllKinds：费率面板的渠道清单必须覆盖**全部已实现渠道**。
//
// 为什么必须锁（2026-10-12 实测踩坑）：buildFeesChannels 只遍历 feeChannelOrder，
// 没在表里的渠道**永远不会出现在面板**，而 /v1/models 与 CachedChannelModels 都有它
// ⇒ 表现为「接口能调、模型列表里有、但费率面板没这个 tab」。cline 渠道接入时漏登此表，
// 症状持续到手动排查才发现（与 R34「声明了没接线」同族：不报错、不崩溃、纯静默）。
// 本测试直接以 provider.Kind 常量全集为准，新增渠道若不登记就会失败。
//
// 例外：旧 Qoder（QoderWork）已从界面下线（路由保留供存量账号用），故显式豁免。
func TestFeeChannelOrderCoversAllKinds(t *testing.T) {
	all := []provider.Kind{
		provider.WorkBuddy, provider.WorkBuddyAI, provider.TraeWork, provider.TraeCode,
		provider.QoderCN, provider.QoderCOM, provider.QwenWork, provider.GLM,
		provider.MonkeyCode, provider.Raccoon, provider.Loomy, provider.Oczen, provider.Cline,
		provider.Qoder, // 已下线，仅存量路由保留
	}
	inOrder := map[provider.Kind]bool{}
	for _, k := range feeChannelOrder {
		inOrder[k] = true
	}
	for _, k := range all {
		if k == provider.Qoder {
			continue // 旧 Qoder 已下线，不应出现在面板
		}
		if !inOrder[k] {
			t.Errorf("渠道 %s 未登记进 feeChannelOrder ⇒ 费率面板不会出现该渠道 tab", k)
		}
	}
	if inOrder[provider.Qoder] {
		t.Error("旧 Qoder 已从界面下线，不应再登记进 feeChannelOrder")
	}
}

// TestBuildFeesChannelsHandMeasured：本工具**手工实测**的上下文也要能在面板展示
// （has_context=true），但必须同时带 hand_measured 标记让前端如实标注来源。
//
// 为什么单独一条：cline 这类渠道上游根本不返回上下文，若不把实测值放行，
// 面板永远只显示「未知」（用户看不到实测的 1M）；但若直接置 ContextFromAPI=true
// 就是把实测冒充成上游官方值（违反 R46/issue #40 的「不把缺失当结论」原则）。
func TestBuildFeesChannelsHandMeasured(t *testing.T) {
	models := map[provider.Kind][]provider.ModelInfo{
		provider.Cline: {
			{ID: "cline-free/step-5-preview", ContextWindow: 1_000_000, HandMeasured: true},
			{ID: "cline-free/unknown-new", ContextWindow: 0}, // 无实测数据
		},
	}
	got := buildFeesChannels(models, nil, []provider.Kind{provider.Cline}, nil)
	if len(got) != 1 {
		t.Fatalf("want 1 channel, got %d", len(got))
	}
	byID := map[string]feesModelRow{}
	for _, r := range got[0].Models {
		byID[r.Model] = r
	}
	step := byID["cline-free/step-5-preview"]
	if !step.HasContext || step.ContextWindow != 1_000_000 {
		t.Errorf("实测上下文应可展示（has_context=true, ctx=1M）：%+v", step)
	}
	if !step.HandMeasured {
		t.Error("实测值必须带 hand_measured 标记，否则前端无从区分、会把它当上游官方值")
	}
	if newm := byID["cline-free/unknown-new"]; newm.HasContext || newm.HandMeasured {
		t.Errorf("无实测数据的模型不得展示上下文：%+v", newm)
	}
}

func TestBuildFeesChannelsNoPricing(t *testing.T) {
	models := map[provider.Kind][]provider.ModelInfo{
		provider.TraeWork: {{ID: "glm-5.2"}, {ID: "kimi-k2.7"}},
	}
	got := buildFeesChannels(models, nil, []provider.Kind{provider.TraeWork}, nil)
	if len(got) != 1 || len(got[0].Models) != 2 {
		t.Fatalf("模型不应因缺费率而消失: %+v", got)
	}
	for _, r := range got[0].Models {
		if r.Priced {
			t.Errorf("%s 应标记未定价", r.Model)
		}
	}
}

// TestBuildFeesChannelsAbsentCreditsIsNotFree CN 的 auto 调度器无 credits 字段：
// 「缺失→0」不得被当作免费（应显示 unknown）。
func TestBuildFeesChannelsAbsentCreditsIsNotFree(t *testing.T) {
	explicitFalse := false
	explicitTrue := true
	models := map[provider.Kind][]provider.ModelInfo{
		provider.WorkBuddy: {{ID: "auto"}, {ID: "hy4-preview"}, {ID: "glm-5.2"}},
	}
	pricing := []provider.ModelPricing{
		{Channel: "workbuddy", Model: "auto", Rate: 0, Explicit: &explicitFalse},                         // 无 credits 字段
		{Channel: "workbuddy", Model: "hy4-preview", Rate: 0, Note: "Free now", Explicit: &explicitTrue}, // 显式 x0.00
		{Channel: "workbuddy", Model: "glm-5.2", Rate: 0.79, Explicit: &explicitTrue},
	}
	rows := buildFeesChannels(models, pricing, []provider.Kind{provider.WorkBuddy}, nil)[0].Models
	byID := map[string]feesModelRow{}
	for _, r := range rows {
		byID[r.Model] = r
	}
	if r := byID["auto"]; r.Priced || r.Free {
		t.Errorf("auto 无 credits 字段，应为 unknown（非免费）: %+v", r)
	}
	if r := byID["hy4-preview"]; !r.Priced || !r.Free {
		t.Errorf("显式 x0.00 应标为免费: %+v", r)
	}
	if r := byID["glm-5.2"]; !r.Priced || r.Free {
		t.Errorf("付费模型不应标免费: %+v", r)
	}
}

// TestModelPricingIsExplicit 零值兼容：未设置 Explicit 视为显式（保持旧行为）。
func TestModelPricingIsExplicit(t *testing.T) {
	if !(provider.ModelPricing{Rate: 0}).IsExplicit() {
		t.Error("未设置 Explicit 应视为显式（兼容旧缓存）")
	}
	f := false
	if (provider.ModelPricing{Explicit: &f}).IsExplicit() {
		t.Error("Explicit=false 应报告非显式")
	}
}

// TestBuildFeesChannelsContextFlag 只有上游接口返回的上下文才带 has_context=true。
func TestBuildFeesChannelsContextFlag(t *testing.T) {
	models := map[provider.Kind][]provider.ModelInfo{
		provider.WorkBuddyAI: {
			{ID: "hy3", ContextWindow: 192000, MaxTokens: 64000, ContextFromAPI: true},
			{ID: "deepseek-v4.1-flash", ContextWindow: 1000000, MaxTokens: 128000, ContextFromAPI: false},
		},
	}
	rows := buildFeesChannels(models, nil, []provider.Kind{provider.WorkBuddyAI}, nil)[0].Models
	byID := map[string]feesModelRow{}
	for _, r := range rows {
		byID[r.Model] = r
	}
	if r := byID["hy3"]; !r.HasContext || r.ContextWindow != 192000 {
		t.Errorf("接口返回的上下文应标记 has_context: %+v", r)
	}
	if r := byID["deepseek-v4.1-flash"]; r.HasContext {
		t.Errorf("硬编码估算的上下文不应标记 has_context: %+v", r)
	}
}

// 档位与 /v1/models 同源：有档位能力的渠道透出，TraeWork 不透出。
// 注：旧 Qoder 渠道已从面板下线，buildFeesChannels 会跳过它，故此处用 QoderCN 代表
// 「有档位能力的渠道」（三个 Qoder 渠道的档位投影同源）。
func TestBuildFeesChannelsEfforts(t *testing.T) {
	models := map[provider.Kind][]provider.ModelInfo{
		provider.QoderCN: {
			{ID: "glm-5.3", SupportsReasoning: true,
				SupportedEfforts: []string{"low", "high", "max"}, DefaultEffort: "max"},
		},
		provider.TraeWork: {
			{ID: "deepseek-v4-pro", SupportsReasoning: true,
				SupportedEfforts: []string{"low", "high"}},
		},
	}
	channels := buildFeesChannels(models, nil, []provider.Kind{provider.QoderCN, provider.TraeWork}, nil)
	byCh := map[string][]feesModelRow{}
	for _, ch := range channels {
		byCh[ch.Channel] = ch.Models
	}
	if r := byCh["qodercn"][0]; len(r.SupportedEfforts) != 3 || r.DefaultEffort != "max" {
		t.Errorf("QoderCN 应透出档位：%+v", r)
	}
	if r := byCh["traework"][0]; len(r.SupportedEfforts) != 0 || r.DefaultEffort != "" {
		t.Errorf("TraeWork 协议无档位字段，不应透出：%+v", r)
	}
}

// TestBuildFeesChannelsCarriesColor 促销标签颜色应透传到前端行。
func TestBuildFeesChannelsCarriesColor(t *testing.T) {
	explicitTrue := true
	models := map[provider.Kind][]provider.ModelInfo{
		provider.WorkBuddy: {{ID: "deepseek-v4.1-flash", ContextFromAPI: true, ContextWindow: 192000}},
	}
	pricing := []provider.ModelPricing{
		{Channel: "workbuddy", Model: "deepseek-v4.1-flash", Rate: 0.03, Note: "独家优惠", Color: "#FF0000", Explicit: &explicitTrue},
	}
	rows := buildFeesChannels(models, pricing, []provider.Kind{provider.WorkBuddy}, nil)[0].Models
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].Note != "独家优惠" || rows[0].Color != "#FF0000" {
		t.Errorf("颜色未正确透传: note=%q color=%q", rows[0].Note, rows[0].Color)
	}
}

// TestCreditRefreshSelfHealsSessionDead 本地 token 已失效（上游 401）时，
// 刷新路径必须自动 refresh + 重试，而不是把积分永久记成 0。
// 背景：NeedsRefresh 只比本地 expiresAt，token 在别处被轮换或上次未落盘时
// 本地仍显示有效 → 不重试就永久卡死（面板恒为 0、明细恒为空）。
func TestCreditRefreshSelfHealsSessionDead(t *testing.T) {
	up := &fakeUpstream{remain: 1828}
	up.sessionDead.Store(true)
	a, p := newTestApp(t, provider.TraeWork, up, "uid-dead")

	// 先确保池里账号带 refresh token（否则不会尝试刷新）
	if au := p.AuthByUID("uid-dead"); au != nil {
		au.RefreshToken = "rt"
	}

	got, err := a.RefreshCredits("uid-dead")
	if err != nil {
		t.Fatalf("RefreshCredits 应自愈，却报错: %v", err)
	}
	if got != 1828 {
		t.Errorf("remain=%d want 1828（刷新后应拿到真实余额）", got)
	}
	if up.refreshCalls.Load() == 0 {
		t.Error("未尝试 refresh，说明 401 未被识别为 session dead")
	}
	if st, _ := p.Status("uid-dead"); st.Credits != 1828 {
		t.Errorf("池内积分=%d want 1828", st.Credits)
	}
}

// TestResourceDetailSelfHealsSessionDead 明细接口同样要能自愈，
// 否则前端 hover 永远拿不到 items（tooltip 不显示）。
func TestResourceDetailSelfHealsSessionDead(t *testing.T) {
	up := &fakeUpstream{remain: 4400}
	up.sessionDead.Store(true)
	a, p := newTestApp(t, provider.TraeWork, up, "uid-tip")
	if au := p.AuthByUID("uid-tip"); au != nil {
		au.RefreshToken = "rt"
	}

	remain, items, _, _, err := a.ResourceDetail("uid-tip")
	if err != nil {
		t.Fatalf("ResourceDetail 应自愈，却报错: %v", err)
	}
	if remain != 4400 || len(items) == 0 {
		t.Errorf("remain=%d items=%d，刷新后应拿到真实明细", remain, len(items))
	}
}

// TestResourceDetailNoRefreshTokenStillErrors 没有 refresh token 时不假装成功，
// 仍返回错误让前端提示——避免静默显示错误的 0。
func TestResourceDetailNoRefreshTokenStillErrors(t *testing.T) {
	up := &fakeUpstream{remain: 100}
	up.sessionDead.Store(true)
	a, p := newTestApp(t, provider.TraeWork, up, "uid-nort")
	if au := p.AuthByUID("uid-nort"); au != nil {
		au.RefreshToken = ""
	}

	if _, _, _, _, err := a.ResourceDetail("uid-nort"); err == nil {
		t.Error("无 refresh token 时应报错，而不是静默返回空")
	}
	if up.refreshCalls.Load() != 0 {
		t.Error("无 refresh token 时不应尝试刷新")
	}
}

// TestResourceDetailWritesBackPoolSnapshot 同源修复的守门：ResourceDetail 成功后
// pool 快照必须更新为同一响应的值。否则卡片顶层停留在上次刷新事件的旧值，
// 面板出现「同屏两个数字对不上」（实测为时差，非上游口径问题）。
func TestResourceDetailWritesBackPoolSnapshot(t *testing.T) {
	up := &fakeUpstream{remain: 1234}
	a, p := newTestApp(t, provider.Loomy, up, "uid-rd")

	// 先置旧快照，模拟「上次刷新事件之后又发生了消耗」。
	p.SetCreditDetail("uid-rd", 9999, 0, 0, time.Time{})

	remain, items, expiring, unusable, err := a.ResourceDetail("uid-rd")
	if err != nil {
		t.Fatalf("ResourceDetail: %v", err)
	}
	if remain != 1234 || len(items) != 1 {
		t.Fatalf("remain=%d items=%d, want 1234/1", remain, len(items))
	}
	if expiring != 0 || unusable != 0 {
		t.Fatalf("expiring=%d unusable=%d, want 0/0", expiring, unusable)
	}
	if st, ok := p.Status("uid-rd"); !ok || st.Credits != 1234 {
		t.Fatalf("pool 快照未回写: ok=%v credits=%d, want 1234", ok, st.Credits)
	}
}

// TestBuildFeesChannelsContextWindows 逐模型档位随行透出：可选档位来自上游，
// 当前选择取逐模型配置（key 与 /v1/models 的 id 同格式）。
func TestBuildFeesChannelsContextWindows(t *testing.T) {
	// 旧 Qoder 已从面板下线（buildFeesChannels 跳过它），用 QoderCN 代表档位渠道。
	models := map[provider.Kind][]provider.ModelInfo{
		provider.QoderCN: {
			{ID: "qwen3.8-flash", ContextWindow: 1000000, ContextFromAPI: true,
				ContextOptions: []int64{200000, 400000, 1000000}},
			{ID: "minimax-m2.7", ContextWindow: 200000, ContextFromAPI: true,
				ContextOptions: []int64{200000}},
		},
	}
	got := buildFeesChannels(models, nil, []provider.Kind{provider.QoderCN},
		map[string]int64{"qodercn/qwen3.8-flash": 1000000})
	if len(got) != 1 || len(got[0].Models) != 2 {
		t.Fatalf("want 1 channel / 2 rows, got %+v", got)
	}
	byID := map[string]feesModelRow{}
	for _, r := range got[0].Models {
		byID[r.Model] = r
	}
	if r := byID["qwen3.8-flash"]; r.ContextChoice != 1000000 || len(r.ContextOptions) != 3 {
		t.Errorf("已配置模型应带当前档位与全部选项，得到 %+v", r)
	}
	if r := byID["minimax-m2.7"]; r.ContextChoice != 0 {
		t.Errorf("未配置模型当前档位应为 0，得到 %+v", r)
	}
}

// TestSetContextWindow 逐模型档位写入/清除配置，模型名需为 channel/model 形式。
func TestSetContextWindow(t *testing.T) {
	a, _ := newTestApp(t, provider.Qoder, &fakeUpstream{}, "uid-ctx")

	if err := a.SetContextWindow("qoder/qwen3.8-flash", 1000000); err != nil {
		t.Fatal(err)
	}
	if got := a.cfg.Compat.ContextWindows["qoder/qwen3.8-flash"]; got != 1000000 {
		t.Fatalf("配置未写入，得到 %v", a.cfg.Compat.ContextWindows)
	}

	if err := a.SetContextWindow("qoder/qwen3.8-flash", 0); err != nil {
		t.Fatal(err)
	}
	if _, has := a.cfg.Compat.ContextWindows["qoder/qwen3.8-flash"]; has {
		t.Fatalf("window=0 应清除该项，得到 %v", a.cfg.Compat.ContextWindows)
	}

	if err := a.SetContextWindow("qwen3.8-flash", 1000000); err == nil {
		t.Fatal("缺渠道前缀应报错")
	}
}

// ---------------------------------------------------------------------------
// 自定义渠道（R66）与积分体系的关系：无积分概念，不得产生假失败/假 0
// ---------------------------------------------------------------------------

// TestRefreshAllSkipsCustomChannels RefreshAll 必须跳过自定义渠道：
// 其占位账号无 access/refresh token（凭据本体是渠道级 api_key），纳入统计只会
// 每渠道产生一条「no token」假失败，面板 toast 报「失败 N」纯噪音（issue 用户实测：
// 4 个自定义渠道每次刷新都「失败 4」，但 32 个真实账号全部成功）。
// 对齐 oczen 的跳过逻辑（无积分概念的渠道都不参与）。
func TestRefreshAllSkipsCustomChannels(t *testing.T) {
	up := &fakeUpstream{remain: 500}
	a, _ := newTestApp(t, provider.WorkBuddyAI, up, "uid-real")

	// 挂一个自定义渠道运行时：占位账号故意不带任何 token（与 main.buildCustomRuntimes 一致）
	cp := pool.New(t.TempDir() + "/state-mychannel.json")
	cp.Add(&auth.Auth{Kind: "mychannel", UID: "custom-mychannel", Nickname: "mychannel (自定义)", ExpiresAt: 4102444800})
	a.UpdateCustomRuntimes(map[provider.Kind]*Runtime{
		provider.Kind("mychannel"): {Kind: provider.Kind("mychannel"), Pool: cp, Upstream: up, Custom: true},
	})

	sum := a.RefreshAll()
	if sum.Failed != 0 {
		t.Errorf("自定义渠道不应计入失败：failed=%d（accounts=%v）", sum.Failed, sum.Platforms["mychannel"])
	}
	if sum.OK != 1 {
		t.Errorf("真实账号应正常计入成功：ok=%d", sum.OK)
	}
	if _, ok := sum.Platforms["mychannel"]; ok {
		t.Errorf("自定义渠道不应出现在汇总里")
	}
}

// TestRefreshAllSkipsDisabledCustomChannels 变体：自定义渠道停用态（Enabled=false 时
// buildCustomRuntimes 直接不装配，但热更新窗口内可能出现 Custom+Disabled 组合），同样不崩不进统计。
func TestRefreshCreditsCustomChannelNA(t *testing.T) {
	up := &fakeUpstream{remain: 500}
	a, _ := newTestApp(t, provider.WorkBuddyAI, up, "uid-real")

	cp := pool.New(t.TempDir() + "/state-mychannel.json")
	cp.Add(&auth.Auth{Kind: "mychannel", UID: "custom-mychannel", ExpiresAt: 4102444800})
	a.UpdateCustomRuntimes(map[provider.Kind]*Runtime{
		provider.Kind("mychannel"): {Kind: provider.Kind("mychannel"), Pool: cp, Upstream: up, Custom: true},
	})

	// 单账号刷新必须明确报「不适用」，而不是假装成功把池内余额写成 0。
	remain, err := a.RefreshCredits("custom-mychannel")
	if err == nil {
		t.Fatalf("自定义渠道单账号刷新应报「不适用」，却成功返回 remain=%d", remain)
	}
	if !strings.Contains(err.Error(), "不适用") {
		t.Errorf("错误信息应说明无积分概念，得: %v", err)
	}
	if st, ok := cp.Status("custom-mychannel"); ok && st.Credits != 0 {
		// 未被 creditSnapshot 污染（这里快照本就是 0，重点是没有假装成功）
		t.Logf("pool credits=%d", st.Credits)
	}

	// 积分明细同理：报错而不是返回空 items 让前端弹空白层。
	if _, _, _, _, err := a.ResourceDetail("custom-mychannel"); err == nil {
		t.Error("自定义渠道积分明细应报「不适用」，却成功返回")
	}
}

// TestCreditsNAKindCoversCustomChannels creditsNAKind 对自定义渠道返回 true
// （/api/state 据此给 credits_na=true，前端显示「不适用」）。
func TestCreditsNAKindCoversCustomChannels(t *testing.T) {
	up := &fakeUpstream{remain: 1}
	a, _ := newTestApp(t, provider.WorkBuddyAI, up, "uid-real")

	cp := pool.New(t.TempDir() + "/state-mychannel.json")
	cp.Add(&auth.Auth{Kind: "mychannel", UID: "custom-mychannel", ExpiresAt: 4102444800})
	a.UpdateCustomRuntimes(map[provider.Kind]*Runtime{
		provider.Kind("mychannel"): {Kind: provider.Kind("mychannel"), Pool: cp, Upstream: up, Custom: true},
	})

	if !a.creditsNAKind("mychannel") {
		t.Error("自定义渠道应判定为积分不适用（credits_na=true）")
	}
	if a.creditsNAKind("workbuddyai") {
		t.Error("内建有积分渠道不应被判定为 NA")
	}
}
