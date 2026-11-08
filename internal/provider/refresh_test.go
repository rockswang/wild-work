package provider

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wild-work/internal/auth"
)

// waitEntered 等所有 goroutine 都已进入 RefreshOnce，再留一点余量让它们落到 flight 表上。
// 计数器加完到真正读表之间只有函数调用开销，50ms 余量足够稳定。
func waitEntered(t *testing.T, entered *int32, n int32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(entered) < n {
		if time.Now().After(deadline) {
			t.Fatalf("只有 %d/%d 个 goroutine 进入 RefreshOnce", atomic.LoadInt32(entered), n)
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
}

// TestRefreshOnceConcurrentSingleFlight 同账号并发刷新只应真打上游一次，所有调用者共享成功结果。
// 这正是 refresh_conflict 的成因：并发 N 次刷新 = 1 成功 + N-1 次冲突。
func TestRefreshOnceConcurrentSingleFlight(t *testing.T) {
	a := &auth.Auth{FilePath: "auths/raccoon-flight.json", UID: "uid-flight"}

	const n = 8
	var calls, entered int32
	release := make(chan struct{})
	errs := make([]error, n)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := int32(0); i < n; i++ {
		wg.Add(1)
		go func(i int32) {
			defer wg.Done()
			<-start
			atomic.AddInt32(&entered, 1)
			errs[i] = RefreshOnce(a, func() error {
				atomic.AddInt32(&calls, 1)
				<-release // 卡住第一次执行，逼出其余调用者的等待路径
				return nil
			})
		}(i)
	}
	close(start)
	waitEntered(t, &entered, n)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("并发刷新真实执行次数 = %d, want 1", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("errs[%d] = %v, want nil", i, err)
		}
	}
}

// TestRefreshOnceDifferentAccountsRunIndependently 不同账号（不同 auth 文件）不得互相阻塞或共享结果。
func TestRefreshOnceDifferentAccountsRunIndependently(t *testing.T) {
	a1 := &auth.Auth{FilePath: "auths/raccoon-a.json", UID: "same-uid"}
	a2 := &auth.Auth{FilePath: "auths/loomy-b.json", UID: "same-uid"} // UID 故意相同：键必须看 FilePath

	var calls int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	for _, a := range []*auth.Auth{a1, a2} {
		wg.Add(1)
		go func(a *auth.Auth) {
			defer wg.Done()
			_ = RefreshOnce(a, func() error {
				atomic.AddInt32(&calls, 1)
				<-release
				return nil
			})
		}(a)
	}
	// 两个账号应各自进入执行（若被错误合并成一次，calls 会停在 1 而此处永远等不到 2）
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&calls) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("两个不同账号被合并成同一次刷新（calls=%d）", atomic.LoadInt32(&calls))
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("不同账号应各执行一次，got %d", got)
	}
}

// TestRefreshOnceSharesError 失败结果同样被共享：等待者不应各自重试打上游。
func TestRefreshOnceSharesError(t *testing.T) {
	a := &auth.Auth{FilePath: "auths/raccoon-err.json", UID: "uid-err"}
	want := errors.New("refresh_conflict")

	const n = 5
	var calls, entered int32
	release := make(chan struct{})
	errs := make([]error, n)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := int32(0); i < n; i++ {
		wg.Add(1)
		go func(i int32) {
			defer wg.Done()
			<-start
			atomic.AddInt32(&entered, 1)
			errs[i] = RefreshOnce(a, func() error {
				atomic.AddInt32(&calls, 1)
				<-release
				return want
			})
		}(i)
	}
	close(start)
	waitEntered(t, &entered, n)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("失败场景真实执行次数 = %d, want 1", got)
	}
	for i, err := range errs {
		if !errors.Is(err, want) {
			t.Fatalf("errs[%d] = %v, want %v", i, err, want)
		}
	}
}

// TestRefreshOnceNoStickyResult 单飞不是缓存：上一轮结束后必须允许重新执行。
// 若实现里忘了删表项，token 过期后将永远刷不动。
func TestRefreshOnceNoStickyResult(t *testing.T) {
	a := &auth.Auth{FilePath: "auths/raccoon-seq.json", UID: "uid-seq"}

	var calls int32
	for i := 0; i < 3; i++ {
		if err := RefreshOnce(a, func() error {
			atomic.AddInt32(&calls, 1)
			return nil
		}); err != nil {
			t.Fatalf("第 %d 次刷新报错: %v", i+1, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("顺序调用 3 次应执行 3 次，got %d（结果被错误缓存）", got)
	}
}

// TestRefreshOnceUnkeyableAccount 既无 FilePath 也无 UID 时退化为直接执行，不得阻塞。
func TestRefreshOnceUnkeyableAccount(t *testing.T) {
	var calls int32
	fn := func() error { atomic.AddInt32(&calls, 1); return nil }

	if err := RefreshOnce(nil, fn); err != nil {
		t.Fatalf("nil auth 应直接执行且无错，got %v", err)
	}
	if err := RefreshOnce(&auth.Auth{}, fn); err != nil {
		t.Fatalf("空 auth 应直接执行且无错，got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("应各执行一次，got %d", got)
	}
}

// TestRefreshOncePanicWakesWaiters fn panic 时等待者必须被唤醒（附 panic 错误）而非永久阻塞。
func TestRefreshOncePanicWakesWaiters(t *testing.T) {
	a := &auth.Auth{FilePath: "auths/raccoon-panic.json", UID: "uid-panic"}

	release := make(chan struct{})
	entered := make(chan struct{})

	go func() {
		defer func() { _ = recover() }() // 吞掉 panic，避免拖垮测试进程
		_ = RefreshOnce(a, func() error {
			close(entered)
			<-release
			panic("boom")
		})
	}()
	<-entered

	waiterDone := make(chan error, 1)
	go func() {
		waiterDone <- RefreshOnce(a, func() error { return nil }) // 等待者：其 fn 不应被执行
	}()

	time.Sleep(50 * time.Millisecond) // 让 waiter 落到 <-c.done 上
	close(release)

	select {
	case err := <-waiterDone:
		if err == nil || !strings.Contains(err.Error(), "panic") {
			t.Fatalf("等待者应拿到 panic 包装错误，got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("等待者未被唤醒（死锁）")
	}
}
