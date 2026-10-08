package pool

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"wild-work/internal/auth"
)

func TestPickHighestCredits(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	a2 := &auth.Auth{UID: "u2"}
	a3 := &auth.Auth{UID: "u3"}
	p.Add(a1)
	p.Add(a2)
	p.Add(a3)
	p.SetCreditDetail("u1", 100, 0, 0)
	p.SetCreditDetail("u2", 500, 0, 0)
	p.SetCreditDetail("u3", 300, 0, 0)
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickSkipsCooling(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	a2 := &auth.Auth{UID: "u2"}
	p.Add(a1)
	p.Add(a2)
	p.SetCreditDetail("u1", 100, 0, 0)
	p.SetCreditDetail("u2", 50, 0, 0)
	p.Cooldown("u1", CoolHard, time.Hour, "test")
	got := p.Pick()
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickExpiredCooldownReturnsToHealthy(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	p.Add(a1)
	p.SetCreditDetail("u1", 100, 0, 0)
	p.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	time.Sleep(5 * time.Millisecond)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v want u1 after cooldown expiry", got)
	}
}

func TestPickNilWhenAllCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "x")
	if got := p.Pick(); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestPickExcluding(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCreditDetail("u1", 100, 0, 0)
	p.SetCreditDetail("u2", 50, 0, 0)
	tried := map[string]bool{"u1": true}
	got := p.PickExcluding(tried)
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
	tried["u2"] = true
	if got := p.PickExcluding(tried); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestCooldownPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("cooldown lost after reload")
	}
	st, ok := p2.Status("u1")
	if !ok || st.Reason != "余额不足" {
		t.Errorf("status=%+v ok=%v", st, ok)
	}
}

func TestDisablePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick() != nil {
		t.Fatal("disabled account picked after reload")
	}
	st, _ := p2.Status("u1")
	if !st.Disabled || st.Reason != "12153 session dead" {
		t.Errorf("status=%+v", st)
	}
}

func TestReenableIfCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 500, 0, 120)
	got := p.Pick()
	if got == nil || got.UID != "u1" {
		t.Fatalf("should reenable, pick=%+v", got)
	}
	// 不可消耗额度应被记录供面板展示，但不影响可消耗余额。
	st, _ := p.Status("u1")
	if st.Credits != 500 || st.UnusableCredits != 120 {
		t.Errorf("credits=%d unusable=%d, want 500/120", st.Credits, st.UnusableCredits)
	}
}

func TestReenableZeroCreditsKeepsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 0, 0, 0)
	if p.Pick() != nil {
		t.Fatal("zero credits should stay cooling")
	}
}

func TestReenableDoesNotTouchDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.ReenableIfCredits("u1", 500, 0, 0)
	if p.Pick() != nil {
		t.Fatal("disabled must not auto-reenable")
	}
}

func TestNoteErrorThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i := 0; i < 2; i++ {
		p.NoteError("u1", 3, 10*time.Minute)
		if p.Pick() == nil {
			t.Fatalf("cooling too early at %d", i+1)
		}
	}
	p.NoteError("u1", 3, 10*time.Minute)
	if p.Pick() != nil {
		t.Fatal("threshold 3 should cool the account")
	}
}

func TestNoteSuccessResetsCounter(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteError("u1", 3, time.Hour)
	p.NoteError("u1", 3, time.Hour)
	p.NoteSuccess("u1")
	p.NoteError("u1", 3, time.Hour)
	p.NoteError("u1", 3, time.Hour)
	if p.Pick() == nil {
		t.Fatal("success should reset error counter")
	}
}

func TestList(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "nick1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCreditDetail("u1", 42, 0, 0)
	p.Cooldown("u2", CoolSoft, time.Minute, "429")
	list := p.List()
	if len(list) != 2 {
		t.Fatalf("list=%d", len(list))
	}
	var s1, s2 Status
	for _, s := range list {
		if s.UID == "u1" {
			s1 = s
		}
		if s.UID == "u2" {
			s2 = s
		}
	}
	if s1.Credits != 42 || s1.Nickname != "nick1" || s1.Disabled || s1.Cooling {
		t.Errorf("s1=%+v", s1)
	}
	if !s2.Cooling || s2.Reason != "429" {
		t.Errorf("s2=%+v", s2)
	}
}

func TestRemoveMissingFromDir(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SyncToDir([]*auth.Auth{{UID: "u2"}})
	if p.Pick() == nil || p.Pick().UID != "u2" {
		t.Fatal("u1 should be removed")
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 should not exist")
	}
}

func TestRecordCheckinAndRemove(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.RecordCheckin("u1", true, "ok")
	st, _ := p.Status("u1")
	if !st.LastCheckinOK || st.LastCheckinAt.IsZero() {
		t.Errorf("status=%+v", st)
	}
	p.Remove("u1")
	if _, ok := p.Status("u1"); ok {
		t.Error("account should be removed")
	}
}

// TestLoadLegacyStateMarksCreditsStale v2.2.0 及之前的 state 文件无 version/unusable 字段，
// 读入后必须置 creditsStale，避免面板把旧口径余额当真值（需删除 data 目录才能恢复的兼容缺陷）。
// 自动刷新首刷成功（SetCreditDetail）后即清除标记。
func TestLoadLegacyStateMarksCreditsStale(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state-traework.json")
	legacy := `{"accounts":{"u1":{"credits":5320,"disabled":false}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应存在")
	}
	if !st.CreditsStale {
		t.Errorf("旧格式 state 读入后应标记 credits_stale: %+v", st)
	}
	if st.Credits != 5320 {
		t.Errorf("credits=%d want 5320（旧值保留，仅标记不可信）", st.Credits)
	}

	// 首刷成功 → 标记清除，数字变为真实拆分
	p.SetCreditDetail("u1", 2710, 0, 2600)
	st, _ = p.Status("u1")
	if st.CreditsStale {
		t.Error("SetCreditDetail 后不应再标记 stale")
	}
	if st.Credits != 2710 || st.UnusableCredits != 2600 {
		t.Errorf("credits=%d unusable=%d want 2710/2600", st.Credits, st.UnusableCredits)
	}
}

// TestLoadCurrentStateNotStale 新版格式（version>=stateVersion=3）读入后不标记 stale。
// v2 文件（无 expiring）按旧版处理置 stale——可用/不可用/临期三者口径需同批刷新。
func TestLoadCurrentStateNotStale(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state-traework.json")
	cur := `{"version":3,"accounts":{"u1":{"credits":2710,"expiring":130,"unusable":2600}}}`
	if err := os.WriteFile(fp, []byte(cur), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	st, _ := p.Status("u1")
	if st.CreditsStale {
		t.Errorf("新版格式不应标记 stale: %+v", st)
	}
	if st.Credits != 2710 || st.ExpiringCredits != 130 || st.UnusableCredits != 2600 {
		t.Errorf("credits=%d expiring=%d unusable=%d want 2710/130/2600", st.Credits, st.ExpiringCredits, st.UnusableCredits)
	}
}

// TestPickPrefersExpiring 临期优先：临期>0 的账号即使总余额更低也先被选中，
// 同组内仍按总可用余额排序；临期烧完（重置为 0）后退回总余额排序。
func TestPickPrefersExpiring(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"}) // 总余额最高，但无临期
	p.Add(&auth.Auth{UID: "u2"}) // 有临期
	p.Add(&auth.Auth{UID: "u3"}) // 临期更多
	p.SetCreditDetail("u1", 5000, 0, 0)
	p.SetCreditDetail("u2", 100, 50, 0)
	p.SetCreditDetail("u3", 200, 200, 0)
	if got := p.Pick(); got == nil || got.UID != "u3" {
		t.Fatalf("pick=%+v want u3 (临期最多)", got)
	}
	// u3 临期烧完 → u2 接管
	p.SetCreditDetail("u3", 200, 0, 0)
	if got := p.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2 (次多临期)", got)
	}
	// 全部临期清零 → 退回总余额排序
	p.SetCreditDetail("u2", 100, 0, 0)
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v want u1 (纯总余额排序)", got)
	}
}

// TestPickExpiringZeroBalance 确保临期统计的约束 expiring ≤ credits：
// credits=0 的账号即使误传 expiring>0 也不会被选中（防御性用例，正常调用方不会这样传）。
func TestPickExpiringZeroBalance(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCreditDetail("u1", 0, 0, 0)
	p.SetCreditDetail("u2", 10, 0, 0)
	if got := p.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

// TestCooldownModelKeepsOtherModelsUsable 429/6004 按模型限额：
// 冷却 u1 的 glm 后，u1 对 deepseek 仍可被选中；不带模型过滤的 Pick() 也不受影响。
func TestCooldownModelKeepsOtherModelsUsable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditDetail("u1", 100, 0, 0)
	if escalated := p.CooldownModel("u1", "glm-5.3", time.Hour, "429 rate limit"); escalated {
		t.Fatal("首次单模型冷却不应升级")
	}
	if got := p.PickForModel("glm-5.3"); got != nil {
		t.Fatalf("glm-5.3 已模型级冷却，不应被选中: %+v", got)
	}
	if got := p.PickForModel("deepseek-v4.1-flash"); got == nil || got.UID != "u1" {
		t.Fatalf("同号其他模型应可用: %+v", got)
	}
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("账号整体仍健康（Pick 无模型过滤）: %+v", got)
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("单模型冷却不应把整号标记为 cooling: %+v", st)
	}
	if st.ModelCooling["glm-5.3"].IsZero() {
		t.Errorf("Status 应暴露模型级冷却: %+v", st.ModelCooling)
	}
}

// TestCooldownModelEscalatesOnSecondActiveModel 短时间内第二个模型也撞墙 →
// 升级整号冷却到最晚重置点（防上游实际按账号计时逐模型撞墙）。
func TestCooldownModelEscalatesOnSecondActiveModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditDetail("u1", 100, 0, 0)
	if escalated := p.CooldownModel("u1", "glm-5.3", time.Hour, "429 rate limit"); escalated {
		t.Fatal("首次不应升级")
	}
	if escalated := p.CooldownModel("u1", "deepseek-v4.1-flash", 2*time.Hour, "429 rate limit"); !escalated {
		t.Fatal("第二个模型仍冷却中时再次撞墙应升级整号")
	}
	if got := p.Pick(); got != nil {
		t.Fatalf("升级后整号应冷却: %+v", got)
	}
	if got := p.PickForModel("kimi-k2.7"); got != nil {
		t.Fatalf("升级后第三个模型也不应选中该号: %+v", got)
	}
	st, _ := p.Status("u1")
	if !st.Cooling || len(st.ModelCooling) != 0 {
		t.Errorf("升级后应为整号冷却且清空模型级条目: %+v", st)
	}
}

// TestCooldownModelNoEscalateAfterExpiry 第一个模型冷却已过期时不算「短时间内第二次」，
// 不触发升级（重置点已过，说明那次限流确实只针对单模型）。
func TestCooldownModelNoEscalateAfterExpiry(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditDetail("u1", 100, 0, 0)
	if escalated := p.CooldownModel("u1", "glm-5.3", -time.Minute, "429 rate limit"); escalated {
		t.Fatal("过期时长不应升级")
	}
	if escalated := p.CooldownModel("u1", "deepseek-v4.1-flash", time.Hour, "429 rate limit"); escalated {
		t.Fatal("首个模型冷却已过期，第二次不应升级")
	}
	if got := p.PickForModel("glm-5.3"); got == nil || got.UID != "u1" {
		t.Fatalf("过期模型应恢复可用: %+v", got)
	}
}

// TestCooldownModelPersists 模型级冷却随 state.json 持久化：重启后仍生效（6004 重置窗口可达数小时）。
func TestCooldownModelPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownModel("u1", "glm-5.3", time.Hour, "429 rate limit")

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if got := p2.PickForModel("glm-5.3"); got != nil {
		t.Fatalf("重启后模型级冷却应保留: %+v", got)
	}
	if got := p2.PickForModel("deepseek-v4.1-flash"); got == nil || got.UID != "u1" {
		t.Fatalf("重启后同号其他模型应可用: %+v", got)
	}
}
