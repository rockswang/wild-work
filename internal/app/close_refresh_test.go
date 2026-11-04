// close_refresh_test.go 锁住「Close 必须等积分刷新收尾」这条退出纪律。
//
// 背景（2026-11-04，PR #215 作者在 issue 里点名）：SetExpiringDays 末尾是
// `go a.RefreshAll()`，刷新链路会往回写 pool 快照 / ledger / stats，并在写的过程中
// **重新创建 data 目录**。若 Close 不等它，进程退出（或测试 TempDir 清理）就会与
// 后台 goroutine 抢同一批文件句柄：Windows 上表现为
// `RemoveAll cleanup: ... directory is not empty`，面板侧则可能留下半截 state.json。
package app

import (
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
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
