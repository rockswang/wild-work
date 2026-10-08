package scheduler

// checkin_grant_test.go 回归测试（issue #67 第二道防线）：渠道实现
// provider.CheckinGranter（当前 workbuddy）时，调度器走 Grant 变体执行签到
// 并把回执里的权威发放额直接记进 ledger——不再完全依赖快照差分
// （差分在「发放即消耗」场景会把当日 earn 抵消为 0，面板今日收入恒 0）。

import (
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/ledger"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// granterUpstream 内嵌 Upstream，只实现 CheckinGranter（对齐 reporterUpstream 的嵌入模式）。
type granterUpstream struct {
	provider.Upstream
	granted     int64
	err         error
	legacyCalls int // DailyCheckin 被直接调用的次数（应恒 0：Grant 是唯一执行路径）
}

func (g *granterUpstream) DailyCheckin(a *auth.Auth) error {
	g.legacyCalls++
	return nil
}

func (g *granterUpstream) DailyCheckinGrant(a *auth.Auth) (int64, error) {
	return g.granted, g.err
}

func (g *granterUpstream) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 3428, []provider.ResourceItem{{Name: "套餐", Remain: 3428, Usable: true}}, nil
}

// TestCheckinGranterRecordsAuthoritativeEarn granter 渠道签到成功：
// 回执发放额 100 直接落账 earn，不依赖差分；DailyCheckin 不被重复调用。
func TestCheckinGranterRecordsAuthoritativeEarn(t *testing.T) {
	dir := t.TempDir()
	lg, err := ledger.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	up := &granterUpstream{granted: 100}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := New(Config{Pool: p, Upstream: up, Name: "workbuddy", Ledger: lg, CheckinHours: []int{9}})

	s.RunCheckinNow()

	if up.legacyCalls != 0 {
		t.Fatalf("Grant 应是唯一执行路径，DailyCheckin 被调 %d 次", up.legacyCalls)
	}
	st := lg.Query(1, func(uid string) (name, channel string) { return "测试", "workbuddy" })
	// earn 总额 = 3428（首刷 baseline，既有设计）+ 100（权威回执，issue #67 修复点）
	if st.Credit.Earn != 3528 {
		t.Fatalf("earn=%d, want 3528（baseline 3428 + 权威回执 100）", st.Credit.Earn)
	}
	var grantEarned bool
	for _, e := range st.Credit.Entries {
		if e.Kind == "earn" && e.Note == "签到奖励" && e.Amount == 100 {
			grantEarned = true
		}
	}
	if !grantEarned {
		t.Fatal("应存在一条 Note=签到奖励 Amount=100 的权威 earn 条目")
	}
}

// TestCheckinGranterZeroGrantFallsBack 回执未回填金额（granted=0）：
// 不记假账，签到照常完成（走差分口径兜底）。
func TestCheckinGranterZeroGrantFallsBack(t *testing.T) {
	dir := t.TempDir()
	lg, err := ledger.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	up := &granterUpstream{granted: 0} // 上游未回填 credit 字段
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s := New(Config{Pool: p, Upstream: up, Name: "workbuddy", Ledger: lg, CheckinHours: []int{9}})

	s.RunCheckinNow()

	st := lg.Query(1, func(uid string) (name, channel string) { return "测试", "workbuddy" })
	for _, e := range st.Credit.Entries {
		if e.Kind == "earn" && e.Note == "签到奖励" {
			t.Fatalf("granted=0 不得记权威 earn: %+v", e)
		}
	}
	st2, _ := p.Status("u1")
	if !st2.LastCheckinOK {
		t.Fatal("granted=0 时签到本身应照常成功")
	}
}
