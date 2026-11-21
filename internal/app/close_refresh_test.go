// close_refresh_test.go 锁住「Close 必须等积分刷新收尾」这条退出纪律。
//
// 背景（2026-11-04，PR #215 作者在 issue 里点名）：SetExpiringDays 末尾是
// `go a.RefreshAll()`，刷新链路会往回写 pool 快照 / ledger / stats，并在写的过程中
// **重新创建 data 目录**。若 Close 不等它，进程退出（或测试 TempDir 清理）就会与
// 后台 goroutine 抢同一批文件句柄：Windows 上表现为
// `RemoveAll cleanup: ... directory is not empty`，面板侧则可能留下半截 state.json。
package app

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// slowItemsUpstream 在 UserResourceDetail 里阻塞到 release 关闭，
// 用于把「刷新进行中」这个窗口拉长到可观测。
type slowItemsUpstream struct {
	fakeUpstream
	release   chan struct{}
	entered   chan struct{}
	enterOnce sync.Once
}

func (s *slowItemsUpstream) UserResourceDetail(_ *auth.Auth) (int64, []provider.ResourceItem, error) {
	s.entered <- struct{}{}
	<-s.release
	return 100, []provider.ResourceItem{{Name: "pkg", Remain: 100, Usable: true}}, nil
}

// TestCloseWaitsForRunningRefresh Close 必须阻塞到在跑的 RefreshAll 落地。
// 否则刷新会在 Close 关掉句柄之后继续往 data 目录写文件。
func TestCloseWaitsForRunningRefresh(t *testing.T) {
	up := &slowItemsUpstream{release: make(chan struct{}), entered: make(chan struct{}, 4)}
	a, _ := newTestApp(t, provider.WorkBuddyAI, up, "uid-1")

	refreshed := make(chan struct{})
	go func() {
		a.RefreshAll()
		close(refreshed)
	}()
	<-up.entered // 刷新已进入上游调用（正在「进行中」）

	closed := make(chan struct{})
	go func() {
		a.Close()
		close(closed)
	}()

	// Close 不该在刷新还没落地时就返回：给它 100ms，正常实现一定还堵着。
	select {
	case <-closed:
		t.Fatalf("Close 在积分刷新收尾前就返回了（后台会继续写 data 文件）")
	case <-time.After(100 * time.Millisecond):
	}

	close(up.release) // 放行刷新
	select {
	case <-refreshed:
	case <-time.After(3 * time.Second):
		t.Fatalf("RefreshAll 未落地")
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatalf("刷新落地后 Close 应立刻返回")
	}
}

// TestWaitRefreshIdleReturnsImmediately 没有在跑的刷新时不能白等（退出路径要快）。
func TestWaitRefreshIdleReturnsImmediately(t *testing.T) {
	a, _ := newTestApp(t, provider.WorkBuddyAI, &fakeUpstream{remain: 10}, "uid-1")
	start := time.Now()
	a.waitRefresh(time.Second)
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("无刷新在跑时 waitRefresh 应立即返回，实际耗时 %s", d)
	}
}

// gatedPricingUpstream 把费率拉取卡在 release 上（entered/release 为 nil 时不卡），
// 用于把「后台费率刷新进行中」这个窗口拉长到可观测。
type gatedPricingUpstream struct {
	fakeUpstream
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (g *gatedPricingUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	g.calls.Add(1)
	if g.entered != nil {
		g.entered <- struct{}{}
	}
	if g.release != nil {
		<-g.release
	}
	return []provider.ModelPricing{{Channel: "modelscope", Model: "m", Rate: 1}}, nil
}

// newPricingTestApp 造一个「token 远期有效」的单账号 app：费率刷新不会被
// 卡在 token 轮换（那条路径会 SaveAtomic 写 auth 文件，属于另一类落盘）。
func newPricingTestApp(t *testing.T, up provider.Upstream) *App {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuthDir = dir
	p := pool.New(filepath.Join(dir, "state-modelscope.json"))
	p.Add(&auth.Auth{
		Kind: provider.ModelScope.String(), UID: "uid-1",
		AccessToken: "t", RefreshToken: "r",
		ExpiresAt: time.Now().AddDate(80, 0, 0).Unix(),
	})
	a, err := New(Options{
		ConfigPath: filepath.Join(dir, "config.json"),
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			provider.ModelScope: {Kind: provider.ModelScope, Pool: p, Upstream: up},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

// TestPriceRefreshAfterCloseWritesNothing 锁住 `TestPersistModelScopeAccount` 曾经的 flaky 根因。
//
// 登录成功后 `afterAccountAdded` 会 `go RefreshPricing()`；它若在 Close **之后**才被调度到，
// 就会在测试的 TempDir 清理期间往 AuthDir 写出 pricing-cache.json，
// 表现为 `TempDir RemoveAll cleanup: ... directory not empty`（与 waitRefresh 同源）。
// 现在关闭流程置位之后，晚到的刷新既不请求上游也不落盘。
func TestPriceRefreshAfterCloseWritesNothing(t *testing.T) {
	up := &gatedPricingUpstream{}
	a := newPricingTestApp(t, up)
	path := filepath.Join(a.stateDir(), "pricing-cache.json")

	a.Close()          // 关闭流程走完时，后台 goroutine 可能还没被调度到
	a.RefreshPricing() // 它终于在 Close 之后跑起来

	if n := up.calls.Load(); n != 0 {
		t.Fatalf("Close 之后仍请求上游 %d 次", n)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("Close 之后仍写出了 %s（会与退出/TempDir 清理抢文件）", path)
	}
}

// TestPriceRefreshInFlightSkipsWriteAfterClose 已经在跑的刷新（Close 时正卡在上游调用里）
// 同样不得在 Close 之后落盘：判定与写入必须在同一把锁下，否则留着「刚判完就被置位」的窗口。
func TestPriceRefreshInFlightSkipsWriteAfterClose(t *testing.T) {
	up := &gatedPricingUpstream{entered: make(chan struct{}, 1), release: make(chan struct{})}
	a := newPricingTestApp(t, up)
	path := filepath.Join(a.stateDir(), "pricing-cache.json")

	done := make(chan struct{})
	go func() {
		a.RefreshPricing()
		close(done)
	}()
	<-up.entered // 刷新已进入上游调用（进行中）

	a.Close()         // 关闭流程：置 closed、关日志句柄
	close(up.release) // 放行：此时到达落盘点的写入必须被拒

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("RefreshPricing 未收尾（Close 与落盘互锁？）")
	}
	if up.calls.Load() == 0 {
		t.Fatalf("构建失败：刷新根本没走到上游")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("Close 之后仍写出了 %s（会与退出/TempDir 清理抢文件）", path)
	}
}
