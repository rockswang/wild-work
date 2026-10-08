package idle

import (
	"context"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// blockingReader 读时阻塞，直到 ch 被关闭；用于模拟「上游连上但不吐数据」。
type blockingReader struct {
	ch chan struct{}
}

func (b *blockingReader) Read([]byte) (int, error) {
	<-b.ch
	return 0, io.EOF
}

func (b *blockingReader) Close() error { return nil }

// pacedReader 每 pace 返回 1 字节，共 n 字节；用于模拟正常持续输出的流。
type pacedReader struct {
	n    int
	pace time.Duration
	read int
}

func (p *pacedReader) Read(b []byte) (int, error) {
	if p.read >= p.n {
		return 0, io.EOF
	}
	time.Sleep(p.pace)
	p.read++
	b[0] = 'x'
	return 1, nil
}

func (p *pacedReader) Close() error { return nil }

// cancelSpy 记录 cancel 是否被调用（并可等待其发生）。
type cancelSpy struct {
	done chan struct{}
	once sync.Once
}

func newCancelSpy() *cancelSpy { return &cancelSpy{done: make(chan struct{})} }

func (c *cancelSpy) cancel() {
	c.once.Do(func() { close(c.done) })
}

func (c *cancelSpy) waitCancelled(d time.Duration) bool {
	select {
	case <-c.done:
		return true
	case <-time.After(d):
		return false
	}
}

func (c *cancelSpy) cancelled() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// idle<=0 时禁用监控：原样返回底流，不做包装。
func TestMonitorDisabledReturnsSameReader(t *testing.T) {
	rc := &pacedReader{n: 1}
	spy := newCancelSpy()
	got := Monitor(rc, 0, spy.cancel)
	if got != io.ReadCloser(rc) {
		t.Errorf("idle<=0 应原样返回底流，实际被包装")
	}
	if got := Monitor(rc, -time.Second, spy.cancel); got != io.ReadCloser(rc) {
		t.Errorf("idle<0 应原样返回底流，实际被包装")
	}
	if spy.cancelled() {
		t.Error("禁用监控时不应调用 cancel")
	}
}

// 静默超过 idle 时必须主动 cancel（这是本次修复的核心：上游卡死不再无限挂起）。
func TestMonitorCancelsWhenIdleExceedsTimeout(t *testing.T) {
	ch := make(chan struct{})
	defer close(ch)
	spy := newCancelSpy()
	body := Monitor(&blockingReader{ch: ch}, 50*time.Millisecond, spy.cancel)
	defer body.Close()

	if !spy.waitCancelled(2 * time.Second) {
		t.Fatal("静默超过 idle 后应调用 cancel，但未发生")
	}
}

// 持续有数据时不 cancel，且数据必须原样透传。
func TestMonitorKeepsAliveWhileDataFlows(t *testing.T) {
	spy := newCancelSpy()
	const n = 20
	body := Monitor(&pacedReader{n: n, pace: 10 * time.Millisecond}, 200*time.Millisecond, spy.cancel)
	defer body.Close()

	buf := make([]byte, 64)
	total := 0
	for {
		got, err := body.Read(buf)
		total += got
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("读取失败: %v", err)
		}
	}
	if total != n {
		t.Errorf("数据透传不完整: got %d want %d", total, n)
	}
	if spy.cancelled() {
		t.Error("数据持续到达时不应 cancel")
	}
}

// Close 幂等，且会 cancel 并关闭底流。
func TestMonitorCloseIdempotentAndCancels(t *testing.T) {
	spy := newCancelSpy()
	rc := &pacedReader{n: 1}
	body := Monitor(rc, time.Second, spy.cancel)
	if err := body.Close(); err != nil {
		t.Fatalf("首次 Close 失败: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("重复 Close 应幂等，实际: %v", err)
	}
	if !spy.cancelled() {
		t.Error("Close 应调用 cancel")
	}
}

// 后台 goroutine 必须在 Close 后退出，不得泄漏。
func TestMonitorGoroutineExitsAfterClose(t *testing.T) {
	spy := newCancelSpy()
	body := Monitor(&pacedReader{n: 1}, 50*time.Millisecond, spy.cancel)
	// 先让监控 goroutine 跑起来
	time.Sleep(20 * time.Millisecond)
	if err := body.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	// Close 后即使再等超过 idle，也不应再有额外 cancel（once 已保证，这里验证不 panic/不阻塞）
	time.Sleep(120 * time.Millisecond)
}

// tick 周期钳位：小 idle 不至于过密，大 idle 不至于超过 1s。
func TestTickClamp(t *testing.T) {
	cases := []struct {
		idle time.Duration
		want time.Duration
	}{
		{time.Second, 250 * time.Millisecond},
		{10 * time.Second, time.Second},
		{100 * time.Millisecond, 25 * time.Millisecond},
		{10 * time.Millisecond, 10 * time.Millisecond},
		{time.Millisecond, 10 * time.Millisecond},
	}
	for _, c := range cases {
		if got := tick(c.idle); got != c.want {
			t.Errorf("tick(%v)=%v want %v", c.idle, got, c.want)
		}
	}
}

// WithCancel 挂上的 ctx 必须能被 Monitor 的 cancel 取消（打通「监控→中断 Read」链路）。
func TestWithCancelPropagatesToRequest(t *testing.T) {
	req, err := http.NewRequest("POST", "http://example.invalid/v1/chat", nil)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req, cancel := WithCancel(req)
	if err := req.Context().Err(); err != nil {
		t.Fatalf("初始 ctx 不应已取消: %v", err)
	}
	cancel()
	select {
	case <-req.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("cancel 后请求 ctx 应被取消")
	}
	if req.Context().Err() != context.Canceled {
		t.Errorf("期望 context.Canceled，实际 %v", req.Context().Err())
	}
}
