package officeace

import (
	"sync"
	"testing"
	"time"
)

// TestThrottleSerializesWithinBaseURL 同一 base_url 的并发调用必须被串行化到
// 至少 interval 的间隔，否则上游 429（限 1 req/s）。
//
// 这条测试守的是 2026-10-18 实测事故：officeace 无节流 ⇒ 并发稍高即 429
// ModelArts.81101 ⇒ handler 整号软冷却 ⇒ 逐号轮空 ⇒ 整渠道 503。
func TestThrottleSerializesWithinBaseURL(t *testing.T) {
	const baseURL = "https://throttle-test-1.example/officeace"
	const interval = 60 * time.Millisecond
	l := throttleFor(baseURL, interval)

	const n = 4
	var mu sync.Mutex
	var stamps []time.Time
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l.wait()
			mu.Lock()
			stamps = append(stamps, time.Now())
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(stamps) != n {
		t.Fatalf("记录到 %d 次调用，期望 %d", len(stamps), n)
	}
	// 按时间排序后逐对检查间隔。
	for i := 1; i < len(stamps); i++ {
		for j := i; j > 0 && stamps[j-1].After(stamps[j]); j-- {
			stamps[j-1], stamps[j] = stamps[j], stamps[j-1]
		}
	}
	for i := 1; i < len(stamps); i++ {
		gap := stamps[i].Sub(stamps[i-1])
		// 允许少量调度抖动（Go 定时器与调度精度），但不能明显小于 interval。
		if gap < interval*3/4 {
			t.Errorf("❌ 第 %d 与第 %d 次调用间隔 %v < %v —— 节流未生效（会导致上游 429）",
				i-1, i, gap, interval*3/4)
		}
	}
}

// TestThrottleSharedAcrossCallers 节流状态必须按 base_url 跨调用方共享。
//
// 判据：两个不同的 throttleFor 句柄（模拟两个 Client 实例）必须拿到**同一个**限流器，
// 否则「重建 Client 就丢节奏」，三个账号并发时合计仍会超 1 req/s。
func TestThrottleSharedAcrossCallers(t *testing.T) {
	const baseURL = "https://throttle-test-2.example/officeace"
	const interval = time.Second
	a := throttleFor(baseURL, interval)
	b := throttleFor(baseURL, interval)
	if a != b {
		t.Fatal("❌ 同一 base_url 拿到了不同的限流器实例 —— 节流状态不共享，并发仍会 429")
	}
}

// TestThrottleIsolatedAcrossBaseURLs 不同 base_url 必须互不影响。
//
// 否则一个慢网关会拖住所有 officeace 请求。
func TestThrottleIsolatedAcrossBaseURLs(t *testing.T) {
	const interval = time.Second
	a := throttleFor("https://throttle-test-3a.example", interval)
	b := throttleFor("https://throttle-test-3b.example", interval)
	if a == b {
		t.Fatal("❌ 不同 base_url 共用了同一个限流器 —— 一个网关会拖住全部请求")
	}
}

// TestThrottleDisabledByNegative 负间隔关闭节流（逃生门）。
func TestThrottleDisabledByNegative(t *testing.T) {
	l := throttleFor("https://throttle-test-4.example", 0)
	if d := l.wait(); d != 0 {
		t.Errorf("间隔为 0 时应立即通过，实际等待 %v", d)
	}
	c := &Client{ChatThrottleInterval: -1}
	if got := c.throttleInterval(); got != 0 {
		t.Errorf("ChatThrottleInterval<0 应关闭节流，throttleInterval() = %v", got)
	}
	c2 := &Client{}
	if got := c2.throttleInterval(); got != DefaultChatThrottleInterval {
		t.Errorf("零值应取默认 %v，实得 %v", DefaultChatThrottleInterval, got)
	}
}
