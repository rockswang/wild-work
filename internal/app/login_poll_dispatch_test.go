package app

// login_poll_dispatch_test.go 锁定 pollLogin 对 loginFlow.Poll 三态契约的分派
// （回归：9f8f4a95 重构收表时把分派写成 `if perr == nil` 就调 done(a)，而 pending
// 态恒为 (nil, true, nil)——done 是 nil，首个 2s tick 必 nil 闭包 panic；panic 被
// recover 吞掉后 finishLogin 清 loginBusy，表现是**所有**轮询渠道发起登录 2 秒后
// 静默死亡、面板无任何响应，曾连带掩盖 issue #98 的真实症状）。
//
// 白盒：直接驱动 pollLogin，用 stub flow 走「pending → pending → 成功」，
// 断言不 panic、成功态 done 恰被调一次、无终态错误。

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"wild-work/internal/provider"
)

func TestPollLoginPendingDoesNotCallNilDone(t *testing.T) {
	const kind = provider.TraeWork
	orig, had := loginFlows[kind]
	if !had {
		t.Fatalf("loginFlows 未登记 %s", kind)
	}
	t.Cleanup(func() { loginFlows[kind] = orig })

	calls := 0
	doneCalled := 0
	loginFlows[kind] = &loginFlow{
		Poll: func(*http.Client, string) (func(*App) error, bool, error) {
			calls++
			if calls <= 2 {
				// pending 态：done 必须为 nil（wrapPoll 契约），分派不得调它。
				return nil, true, nil
			}
			return func(*App) error { doneCalled++; return nil }, false, nil
		},
	}

	a := &App{}
	a.loginKind = kind
	a.loginStateFP = filepath.Join(t.TempDir(), "login-state.json")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() { a.pollLogin(ctx); close(finished) }()

	// 3 个 tick（loginPollEvery=2s）+ 余量；回归版本会在首个 tick panic 后立即返回。
	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		cancel()
		t.Fatal("pollLogin 未在预期时间内收尾（轮询卡死？）")
	}

	if doneCalled != 1 {
		t.Fatalf("成功态 done 应恰被调用 1 次，实际 %d 次（calls=%d）—— "+
			"若为 0 且立即返回，即 pending 态误调 nil done 的回归", doneCalled, calls)
	}
	if calls != 3 {
		t.Fatalf("stub Poll 应被调用 3 次（两次 pending + 一次成功），实际 %d 次", calls)
	}
	if msg := a.peekLoginError(); msg != "" {
		t.Fatalf("成功收尾不应留下终态错误，实际 %q", msg)
	}
}

// TestPollLoginTerminalErrorEndsPolling 终态错误 (nil, false, err)：立即收尾并落 loginErr，
// 不得继续空转，也不得 panic。
func TestPollLoginTerminalErrorEndsPolling(t *testing.T) {
	const kind = provider.Qoder
	orig, had := loginFlows[kind]
	if !had {
		t.Fatalf("loginFlows 未登记 %s", kind)
	}
	t.Cleanup(func() { loginFlows[kind] = orig })

	loginFlows[kind] = &loginFlow{
		Poll: func(*http.Client, string) (func(*App) error, bool, error) {
			return nil, false, context.DeadlineExceeded
		},
	}

	a := &App{}
	a.loginKind = kind
	a.loginStateFP = filepath.Join(t.TempDir(), "login-state.json")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan struct{})
	go func() { a.pollLogin(ctx); close(finished) }()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("终态错误后 pollLogin 未立即收尾（重复轮询只会刷同一个错）")
	}
	if msg := a.peekLoginError(); msg == "" {
		t.Fatal("终态错误必须经 setLoginError 落到 loginErr 供面板展示")
	}
}
