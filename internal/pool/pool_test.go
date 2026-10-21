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
	p.SetCreditDetail("u1", 100, 0, 0, time.Time{})
	p.SetCreditDetail("u2", 500, 0, 0, time.Time{})
	p.SetCreditDetail("u3", 300, 0, 0, time.Time{})
	got := p.Pick("")
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
	p.SetCreditDetail("u1", 100, 0, 0, time.Time{})
	p.SetCreditDetail("u2", 50, 0, 0, time.Time{})
	p.Cooldown("u1", CoolHard, time.Hour, "test")
	got := p.Pick("")
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickExpiredCooldownReturnsToHealthy(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	p.Add(a1)
	p.SetCreditDetail("u1", 100, 0, 0, time.Time{})
	p.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	time.Sleep(5 * time.Millisecond)
	got := p.Pick("")
	if got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v want u1 after cooldown expiry", got)
	}
}

func TestPickNilWhenAllCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "x")
	if got := p.Pick(""); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestPickExcluding(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCreditDetail("u1", 100, 0, 0, time.Time{})
	p.SetCreditDetail("u2", 50, 0, 0, time.Time{})
	tried := map[string]bool{"u1": true}
	got := p.PickExcluding(tried, "")
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
	tried["u2"] = true
	if got := p.PickExcluding(tried, ""); got != nil {
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
	if p2.Pick("") != nil {
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
	if p2.Pick("") != nil {
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
	p.ReenableIfCredits("u1", 500, 0, 120, time.Time{})
	got := p.Pick("")
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
	p.ReenableIfCredits("u1", 0, 0, 0, time.Time{})
	if p.Pick("") != nil {
		t.Fatal("zero credits should stay cooling")
	}
}

func TestReenableDoesNotTouchDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.ReenableIfCredits("u1", 500, 0, 0, time.Time{})
	if p.Pick("") != nil {
		t.Fatal("disabled must not auto-reenable")
	}
}

func TestNoteErrorThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i := 0; i < 2; i++ {
		p.NoteError("u1", 3, 10*time.Minute)
		if p.Pick("") == nil {
			t.Fatalf("cooling too early at %d", i+1)
		}
	}
	p.NoteError("u1", 3, 10*time.Minute)
	if p.Pick("") != nil {
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
	if p.Pick("") == nil {
		t.Fatal("success should reset error counter")
	}
}

func TestList(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "nick1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCreditDetail("u1", 42, 0, 0, time.Time{})
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
	if p.Pick("") == nil || p.Pick("").UID != "u2" {
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
	p.SetCreditDetail("u1", 2710, 0, 2600, time.Time{})
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

// TestPickEarliestExpireFirst 锁定 R112 的选号主键：**最早到期时刻升序**，
// 余额高低不参与比较（u3 余额最低但只要最快过期就先烧 u3）。
func TestPickEarliestExpireFirst(t *testing.T) {
	now := time.Now()
	p := New("")
	p.Add(&auth.Auth{UID: "u1"}) // 余额最高，但 3 天后才到期
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})
	p.SetCreditDetail("u1", 5000, 0, 0, now.Add(72*time.Hour))
	p.SetCreditDetail("u2", 100, 50, 0, now.Add(24*time.Hour))
	p.SetCreditDetail("u3", 200, 200, 0, now.Add(6*time.Hour))
	if got := p.Pick(""); got == nil || got.UID != "u3" {
		t.Fatalf("pick=%+v want u3（最早到期）", got)
	}
	// u3 烧完换时刻 → u2 接管（仍然不看余额）
	p.SetCreditDetail("u3", 200, 0, 0, now.Add(96*time.Hour))
	if got := p.Pick(""); got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2（次早到期）", got)
	}
	// u2 也推远 → 轮到 u1
	p.SetCreditDetail("u2", 100, 0, 0, now.Add(120*time.Hour))
	if got := p.Pick(""); got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v want u1", got)
	}
}

// TestPickSameExpireTieByUID 到期时刻完全相同时按 uid 的 ASCII 升序（用户口径：
// 「时间点都一样就按 ASCII 排序」），**不再看余额**——旧实现会在这里按 credits 降序，
// 让「余额最多但到期时刻一模一样」的账号排队首，与「先烧快过期的」目标无关。
func TestPickSameExpireTieByUID(t *testing.T) {
	same := time.Now().Add(6 * time.Hour)
	p := New("")
	p.Add(&auth.Auth{UID: "z-199"}) // 余额最多
	p.Add(&auth.Auth{UID: "a-158"}) // 余额最少，但 uid 字典序最小
	p.SetCreditDetail("z-199", 100, 100, 0, same)
	p.SetCreditDetail("a-158", 100, 100, 0, same)
	if got := p.Pick(""); got == nil || got.UID != "a-158" {
		t.Fatalf("pick=%+v want a-158（同到期时刻按 uid ASCII）", got)
	}
}

// TestPickNoExpireLastByCredits 无到期信息的账号（渠道不下发 expire_at，如 Qoder）
// 必须垫在「有到期时刻」的后面；同类之间按余额降序，否则一个池子会一直钉在同一个号上。
func TestPickNoExpireLastByCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "noexp-rich"})
	p.Add(&auth.Auth{UID: "noexp-poor"})
	p.Add(&auth.Auth{UID: "exp-small"})
	p.SetCreditDetail("noexp-rich", 9000, 0, 0, time.Time{})
	p.SetCreditDetail("noexp-poor", 10, 0, 0, time.Time{})
	p.SetCreditDetail("exp-small", 5, 5, 0, time.Now().Add(time.Hour))
	if got := p.Pick(""); got == nil || got.UID != "exp-small" {
		t.Fatalf("pick=%+v want exp-small（有到期信息恒优先）", got)
	}
	p.SetCreditDetail("exp-small", 5, 5, 0, time.Time{})
	if got := p.Pick(""); got == nil || got.UID != "noexp-rich" {
		t.Fatalf("pick=%+v want noexp-rich（都无到期信息 → 按余额）", got)
	}
}

// TestPickStaleExpireFallsBack 快照里的到期时刻已经过去（刷新间隔内那个包失效了）：
// 该账号必须退回「无到期信息」组，否则旧时间戳恒为最小 ⇒ 它永远排第一，
// 每次请求都白打一次「余额不足」。
func TestPickStaleExpireFallsBack(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "stale"}) // 时间戳已过去
	p.Add(&auth.Auth{UID: "fresh"})
	p.SetCreditDetail("stale", 100, 100, 0, time.Now().Add(-time.Minute))
	p.SetCreditDetail("fresh", 10, 10, 0, time.Now().Add(30*time.Minute))
	if got := p.Pick(""); got == nil || got.UID != "fresh" {
		t.Fatalf("pick=%+v want fresh（过期旧快照不得抢占队首）", got)
	}
}

// TestEarliestExpirePersists earliestExpire 必须落盘：重启后选号顺序不能变。
func TestEarliestExpirePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	now := time.Now()
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCreditDetail("u1", 100, 0, 0, now.Add(48*time.Hour))
	p.SetCreditDetail("u2", 100, 0, 0, now.Add(2*time.Hour))

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.Add(&auth.Auth{UID: "u2"})
	if got := p2.Pick(""); got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2（重载后仍按最早到期）", got)
	}
}

// TestLoadLegacyStateNoEarliest 旧 state（无 earliest_expire 字段）不得当成「有到期时刻」：
// 读成零值 → 退回按余额排序，与升级前行为等价（不 bump stateVersion 的前提就是这个）。
func TestLoadLegacyStateNoEarliest(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"version":3,"accounts":{"u1":{"credits":100}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	e, ok := p.byUID["u1"]
	if !ok {
		t.Fatal("账号应存在")
	}
	if !e.earliestExpire.IsZero() {
		t.Errorf("旧 state 应读成无到期信息，got %v", e.earliestExpire)
	}
	if e.expireOK(time.Now()) {
		t.Error("零值不得被判为有效到期时刻")
	}
}

// TestCooldownModelKeepsOtherModelsUsable 429/6004 按模型限额：
// 冷却 u1 的 glm 后，u1 对 deepseek 仍可被选中；不带模型过滤的 Pick() 也不受影响。
func TestCooldownModelKeepsOtherModelsUsable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditDetail("u1", 100, 0, 0, time.Time{})
	p.CooldownModel("u1", "glm-5.3", CoolSoft, time.Hour, "429 rate limit")
	if got := p.Pick("glm-5.3"); got != nil {
		t.Fatalf("glm-5.3 已模型级冷却，不应被选中: %+v", got)
	}
	if got := p.Pick("deepseek-v4.1-flash"); got == nil || got.UID != "u1" {
		t.Fatalf("同号其他模型应可用: %+v", got)
	}
	if got := p.Pick(""); got == nil || got.UID != "u1" {
		t.Fatalf("账号整体仍健康（Pick 无模型过滤）: %+v", got)
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Errorf("单模型冷却不应把整号标记为 cooling: %+v", st)
	}
	cooled := false
	for _, mc := range st.ModelCooling {
		if mc.Model == "glm-5.3" {
			cooled = true
		}
	}
	if !cooled {
		t.Errorf("Status 应暴露模型级冷却: %+v", st.ModelCooling)
	}
}

// TestCooldownModelTwoModelsStayModelLevel 两个模型同时冷却 **不** 升级整号。
// 本仓 CooldownModel 恒不做「N 个模型冷就升级整号」的升级（模型级不连坐是硬约束），
// 故本条同时锁住「上游若改回升级策略，本仓仍保持模型级」。
func TestCooldownModelTwoModelsStayModelLevel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditDetail("u1", 100, 0, 0, time.Time{})
	p.CooldownModel("u1", "deepseek-flash", CoolSoft, 30*time.Second, "model queued")
	p.CooldownModel("u1", "glm-5.3", CoolSoft, time.Hour, "429 rate limit")
	if got := p.Pick("kimi-k2.7"); got == nil || got.UID != "u1" {
		t.Fatalf("两个模型冷却后同号其他模型应仍可用: %+v", got)
	}
	st, _ := p.Status("u1")
	if st.Cooling || len(st.ModelCooling) != 2 {
		t.Errorf("应保持模型级冷却（2 条）而非整号冷却: %+v", st)
	}
}

// TestCooldownModelPersists 模型级冷却随 state.json 持久化：重启后仍生效（6004 重置窗口可达数小时）。
func TestCooldownModelPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownModel("u1", "glm-5.3", CoolSoft, time.Hour, "429 rate limit")

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if got := p2.Pick("glm-5.3"); got != nil {
		t.Fatalf("重启后模型级冷却应保留: %+v", got)
	}
	if got := p2.Pick("deepseek-v4.1-flash"); got == nil || got.UID != "u1" {
		t.Fatalf("重启后同号其他模型应可用: %+v", got)
	}
}
