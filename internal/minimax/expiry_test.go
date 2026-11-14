package minimax

// expiry_test.go 锁死积分批次到期时间的读法与筛选口径。
//
// 参考实现（magpie minimax 插件）把账本当「按批的账」：只统计 credit_type 1/2、
// 剩余 > 0 且未过期的批次，取最小 expire_at_ms；账本读不全就整体放弃。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wild-work/internal/provider"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("时间常量非法 %s: %v", s, err)
	}
	return v
}

// TestEarliestLiveExpiryFilters 只认 credit_type 1/2、剩余 > 0、未过期的批次。
func TestEarliestLiveExpiryFilters(t *testing.T) {
	now := mustTime(t, "2026-11-14T12:00:00+08:00")
	details := []map[string]json.RawMessage{
		// 已过期（不参与）
		detail(2, "100", now.Add(-time.Hour).UnixMilli()),
		// 剩余 0（不参与）
		detail(1, "0", now.Add(time.Hour).UnixMilli()),
		// 未知类型（不参与）
		detail(9, "50", now.Add(30*time.Minute).UnixMilli()),
		// 不过期（expire_at_ms=0，不参与）
		detail(2, "50", 0),
		// 有效：两笔，取较早
		detail(1, "200", now.Add(48*time.Hour).UnixMilli()),
		detail(2, "80", now.Add(24*time.Hour).UnixMilli()),
	}
	got, ok := earliestLiveExpiry(details, now)
	if !ok {
		t.Fatal("应取到有效批次")
	}
	if want := now.Add(24 * time.Hour).UnixMilli(); got.UnixMilli() != want {
		t.Fatalf("最早到期 = %d，期望 %d", got.UnixMilli(), want)
	}

	if _, ok := earliestLiveExpiry(details[:4], now); ok {
		t.Fatal("全是无效批次时应返回 ok=false")
	}
}

func detail(creditType int, remaining string, expireMS int64) map[string]json.RawMessage {
	b, _ := json.Marshal(remaining)
	e, _ := json.Marshal(expireMS)
	c, _ := json.Marshal(creditType)
	return map[string]json.RawMessage{
		"credit_type":      c,
		"remaining_amount": b,
		"expire_at_ms":     e,
	}
}

// TestLedgerAmount 金额是十进制字符串，可能带千分位；负数按 0。
func TestLedgerAmount(t *testing.T) {
	cases := []struct {
		raw  string
		want float64
		ok   bool
	}{
		{`"1,200.5"`, 1200.5, true},
		{`"800.00"`, 800, true},
		{`200`, 200, true},
		{`"-3"`, 0, true},
		{`""`, 0, false},
		{`"abc"`, 0, false},
		{`null`, 0, false},
	}
	for _, c := range cases {
		got, ok := ledgerAmount(json.RawMessage(c.raw))
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ledgerAmount(%s) = (%v,%v)，期望 (%v,%v)", c.raw, got, ok, c.want, c.ok)
		}
	}
}

// TestCreditExpiryPaging 账本分两页，取两页里最早的到期；查询参数必须带
// include_expired / page_size / page_number。
func TestCreditExpiryPaging(t *testing.T) {
	now := time.Now()
	page2Expiry := now.Add(10 * 24 * time.Hour).UnixMilli()
	page1Expiry := now.Add(3 * 24 * time.Hour).UnixMilli()
	var pages int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("include_expired") != "true" ||
			r.URL.Query().Get("page_size") != "20" {
			t.Errorf("查询参数不对：%s", r.URL.RawQuery)
		}
		switch r.URL.Query().Get("page_number") {
		case "1":
			atomic.AddInt32(&pages, 1)
			fmt.Fprintf(w, `{"base_resp":{"status_code":0},"total_count":2,
				"details":[{"credit_type":2,"remaining_amount":"100","expire_at_ms":%d}]}`, page1Expiry)
		case "2":
			atomic.AddInt32(&pages, 1)
			fmt.Fprintf(w, `{"base_resp":{"status_code":0},"total_count":2,
				"details":[{"credit_type":1,"remaining_amount":"1,000.5","expire_at_ms":%d}]}`, page2Expiry)
		default:
			t.Errorf("不应翻到第 %s 页", r.URL.Query().Get("page_number"))
		}
	})
	got := c.creditExpiry(testAuth())
	if got.UnixMilli() != page1Expiry {
		t.Fatalf("最早到期 = %d，期望 %d", got.UnixMilli(), page1Expiry)
	}
	if pages != 2 {
		t.Fatalf("应读满两页，实际 %d 页", pages)
	}
}

// TestCreditExpiryIncompleteLedgerDegrades total_count 大于实际可读条数时不填到期，
// 避免用半份账本算出偏早的日期。
func TestCreditExpiryIncompleteLedgerDegrades(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// 第一页只回 1 条，但 total_count=5；后续页空 ⇒ 账本读不全。
		if r.URL.Query().Get("page_number") == "1" {
			_, _ = io.WriteString(w, `{"base_resp":{"status_code":0},"total_count":5,
				"details":[{"credit_type":2,"remaining_amount":"100","expire_at_ms":1800000000000}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"base_resp":{"status_code":0},"total_count":5,"details":[]}`)
	})
	if got := c.creditExpiry(testAuth()); !got.IsZero() {
		t.Fatalf("账本读不全时应返回零值，得到 %v", got)
	}
}

// TestCreditExpiryDegradesOnError 账本接口失败 / 缺 total_count ⇒ 零值，且不影响余额。
func TestCreditExpiryDegradesOnError(t *testing.T) {
	balance := `{"base_resp":{"status_code":0},"total_count":1,
		"details":[{"credit_type":2,"remaining_amount":"100","expire_at_ms":1800000000000}]}`
	for _, h := range []http.HandlerFunc{
		// 余额请求成功、账本请求 500（账本多带 page_number，可据此区分）。
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page_number") != "" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w, balance)
		},
		// 余额成功、账本响应缺 total_count。
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page_number") != "" {
				_, _ = io.WriteString(w, `{"base_resp":{"status_code":0},"details":[]}`)
				return
			}
			_, _ = io.WriteString(w, balance)
		},
	} {
		c := newTestClient(t, h)
		remain, items, err := c.UserResourceDetail(testAuth())
		if err != nil {
			t.Fatalf("余额读取不该失败：%v", err)
		}
		if remain != 100 || len(items) != 1 || items[0].ExpireAt != "" {
			t.Fatalf("降级结果不对：%d %+v", remain, items)
		}
	}
}

// TestUserResourceDetailSetsExpireAt 余额与到期同源于一次账本读取；
// EarliestExpire 能取到该时刻。
func TestUserResourceDetailSetsExpireAt(t *testing.T) {
	now := time.Now()
	soon := now.Add(6 * 24 * time.Hour).UnixMilli()
	later := now.Add(20 * 24 * time.Hour).UnixMilli()
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"base_resp":{"status_code":0},"total_count":3,"details":[
			{"credit_type":2,"remaining_amount":"120.00","expire_at_ms":%d},
			{"credit_type":1,"remaining_amount":"880.00","expire_at_ms":%d},
			{"credit_type":2,"remaining_amount":"0.00","expire_at_ms":%d}]}`, soon, later, now.Add(time.Hour).UnixMilli())
	})
	remain, items, err := c.UserResourceDetail(testAuth())
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if remain != 1000 {
		t.Fatalf("余额 = %d，期望 1000", remain)
	}
	if len(items) != 1 || items[0].ExpireAt == "" {
		t.Fatalf("应填到期时间：%+v", items)
	}
	// ExpireAt 写 RFC3339（秒级），毫秒会被截断，比较到秒。
	earliest, ok := provider.EarliestExpire(items, now)
	if !ok || earliest.Unix() != time.UnixMilli(soon).Unix() {
		t.Fatalf("EarliestExpire = (%v,%v)，期望 unix=%d", earliest, ok, time.UnixMilli(soon).Unix())
	}
	if !strings.HasSuffix(items[0].ExpireAt, "+08:00") {
		t.Fatalf("ExpireAt 应为 UTC+8 的 RFC3339：%q", items[0].ExpireAt)
	}
}

// TestCreditExpiryCache 5 分钟内复用结果，不再翻账本。
func TestCreditExpiryCache(t *testing.T) {
	now := time.Now()
	var calls int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		fmt.Fprintf(w, `{"base_resp":{"status_code":0},"total_count":1,
			"details":[{"credit_type":2,"remaining_amount":"10","expire_at_ms":%d}]}`,
			now.Add(24*time.Hour).UnixMilli())
	})
	a := testAuth()
	first := c.creditExpiry(a)
	second := c.creditExpiry(a)
	if first.IsZero() || !first.Equal(second) {
		t.Fatalf("缓存前后应一致：%v / %v", first, second)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("5 分钟内应只读一次账本，实际 %d 次", n)
	}
}
