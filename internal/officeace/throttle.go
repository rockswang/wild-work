package officeace

import (
	"sync"
	"time"
)

// upstreamRateLimiter 按**上游 base_url** 串行化对话请求，把出站速率压在上游允许的
// 1 req/s 以内。
//
// 为什么需要（2026-10-18 实测）：
//
//	ModelArts 对 officeace 的对话端点限流 1 req/s，超限返回
//	`429 ModelArts.81101 "Too many requests, the rate limit is 1 per second."`
//
// 而本渠道此前**没有任何节流**：client → http.go 直发。后果是
//
//	并发一高 → 429 → handler 走 ErrSoftRate 整号软冷却 → 逐号轮空
//	→ 503 no_healthy_account（修复前还是「3 次轮空」）⇒ 表现为「整条渠道一抖就没」。
//
// **限流维度的判据**：实测两个**不同账号**在同一秒内各自 429（日志 03:47:10 与
// 03:47:11，13:29:58 与 13:29:59），而三个凭据共用同一个 base_url ⇒
// 限的是「网关 × 调用方」，不是「账号」。因此节流器必须按 base_url 共享，
// 不能按账号各持一个（那样三个账号并发时合计仍是 3 req/s，照样 429）。
//
// 实现取「最小间隔串行化」而不是令牌桶：上限恒为 1 req/s，桶容量 1 与直接
// 保证相邻两次出站间隔 ≥ interval 等价，而后者不需要处理突发额度，逻辑更少。
// 多等一点没有代价——429 的代价（整号冷却 + 轮转耗尽）远高于延迟。
type upstreamRateLimiter struct {
	interval time.Duration

	mu   sync.Mutex
	next time.Time // 下一次可以发请求的最早时刻
}

// officeaceThrottle 全包共享的节流表：key = base_url。
//
// 进程级共享（而非 per-Client）是刻意的：Client 可能被重新构造，而「网关限流」
// 是上游的全局属性，节流状态必须跨 Client 存活，否则重建一次就丢一次节奏。
var (
	throttleMu sync.Mutex
	throttles  = map[string]*upstreamRateLimiter{}
)

// throttleFor 取（或创建）某 base_url 的节流器。
func throttleFor(baseURL string, interval time.Duration) *upstreamRateLimiter {
	throttleMu.Lock()
	defer throttleMu.Unlock()
	l, ok := throttles[baseURL]
	if !ok || l.interval != interval {
		l = &upstreamRateLimiter{interval: interval}
		throttles[baseURL] = l
	}
	return l
}

// wait 阻塞到允许发下一个请求的时刻，返回实际等待时长。
//
// 语义：调用方在**发请求前**调用；内部用「预定下一个时刻」而不是「睡完再看」，
// 这样连按顺序到达的并发 caller 会各自拿到错开的时刻（串行化），
// 而不会一起醒来再撞车。
func (l *upstreamRateLimiter) wait() time.Duration {
	if l == nil || l.interval <= 0 {
		return 0
	}
	now := time.Now()
	l.mu.Lock()
	slot := l.next
	if slot.Before(now) {
		slot = now
	}
	l.next = slot.Add(l.interval)
	l.mu.Unlock()

	d := slot.Sub(now)
	if d > 0 {
		time.Sleep(d)
	}
	return d
}
