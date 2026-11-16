package modelscope

// expiry_test.go —— 票 03：transactions 还原 + FIFO 拆分 + 「临期」口径。
//
// 探针实测形状（.scratch/modelscope-credits/probe-findings.md §2）：
// EARN 行逐笔带 expire_at，格式是 "2006-01-02 15:04:05" 的 UTC+8 墙钟串（非 RFC3339）；
// SPEND_CONFIRM / EXPIRE 行不带 expire_at，本渠道只把它们当作对账信息（不单独成条）。

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// txEnvelope 把若干条流水拼成 transactions 的小写信封（探针 §2 实测形状）。
func txEnvelope(records ...string) string {
	return fmt.Sprintf(`{"success":true,"request_id":"req-tx","data":{"page":1,"page_size":20,"total":%d,"records":[%s]}}`,
		len(records), strings.Join(records, ","))
}

// earnRecord 构造一条核心字段齐全的 EARN 行（expire_at 用上游原格式）。
func earnRecord(expireAt string, amount int64) string {
	return fmt.Sprintf(`{"gmt_created":"%s","type":"EARN","rule_key":"daily_active","category_key":"daily",`+
		`"title":"注册并登录","total_amount":%d,"count":1,"expire_days":1,"expire_at":"%s","days_remaining":1}`,
		time.Now().In(creditLoc).Format("2006-01-02"), amount, expireAt)
}

// txStamp 返回相对当前时刻 offsetDays 天后的上游 expire_at 原文（UTC+8 墙钟、空格分隔）。
// 用相对日期保证「未到期」判定不会因写死过去日期而恒为 0（raccoon 同款踩坑）。
func txStamp(offsetDays int) string {
	return time.Now().In(creditLoc).AddDate(0, 0, offsetDays).Format(magicubeExpireLayout)
}

// wantStamp 把上游原文换算成渠道对外承诺的 RFC3339（UTC+8）形态。
func wantStamp(upstream string) string {
	s, ok := normalizeMagicubeExpire(upstream)
	if !ok {
		panic("测试输入不可解析: " + upstream)
	}
	return s
}

// TestTransactionsFIFOSplit（验收 1）先进先出拆分：最老先烧 ⇒ 余额挂最新几笔，
// 且 ΣRemain == available、输出按到期升序。
func TestTransactionsFIFOSplit(t *testing.T) {
	d3, d4, d5 := txStamp(3), txStamp(4), txStamp(5)
	f := &fakeSite{
		balanceBody: `{"success":true,"data":{"total_balance":500,"available_balance":500,"frozen_amount":0}}`,
		usageBody:   fakeUsageBody,
		txBody:      txEnvelope(earnRecord(d3, 300), earnRecord(d4, 300), earnRecord(d5, 300)),
	}
	c := newSiteClient(provider.ModelScope, f.start(t).URL, "ms-x")

	remain, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "ms-x"})
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if remain != 500 {
		t.Fatalf("remain = %d, want 500（余额口径不变）", remain)
	}
	var split []provider.ResourceItem
	var sum int64
	for _, it := range items {
		if it.Name == itemNameUsable {
			split = append(split, it)
			sum += it.Remain
		}
	}
	if sum != 500 {
		t.Fatalf("拆分后 ΣRemain = %d, want 500（必须等于 available）", sum)
	}
	// 500 分池 + 三笔 300：最老那笔已被烧光（剩 0 ⇒ 不出条目），余量落在最新的两笔。
	if len(split) != 2 {
		t.Fatalf("拆分条目 = %d, want 2（最老一笔余额 0 应省略）: %+v", len(split), split)
	}
	if split[0].Remain != 200 || split[0].ExpireAt != wantStamp(d4) {
		t.Fatalf("次新一笔应承接余量 200: %+v", split[0])
	}
	if split[1].Remain != 300 || split[1].ExpireAt != wantStamp(d5) {
		t.Fatalf("最新一笔应满额: %+v", split[1])
	}
	// 输出按到期升序（账本聚合取首个非空 Expire ⇒ 必须是最早那笔）。
	if !(split[0].ExpireAt < split[1].ExpireAt) {
		t.Fatalf("未按到期升序: %q >= %q", split[0].ExpireAt, split[1].ExpireAt)
	}
	// ledger 差分键：所有拆分条目共用 Name、不设 Key。
	for _, it := range split {
		if it.Key != "" {
			t.Fatalf("拆分条目不得设 Key（会拆散 ledger 差分键）: %+v", it)
		}
	}
	// pool 快照与面板小计不变（InfoOnly 的今日用量不进小计）。
	if usable, _ := provider.Summarize(items); usable != 500 {
		t.Fatalf("Summarize 可用 = %d, want 500", usable)
	}
	// transactions 与 balance 用同一种鉴权形态（Bearer），且带浏览器 UA。
	if len(f.obs.txAuth) != 1 || f.obs.txAuth[0] != "Bearer ms-x" {
		t.Errorf("transactions 鉴权头 = %v, want [Bearer ms-x]", f.obs.txAuth)
	}
	for _, ua := range f.obs.txUA {
		if !strings.Contains(ua, "Mozilla/5.0") {
			t.Errorf("transactions 应带浏览器 UA，实得 %q", ua)
		}
	}
}

// TestTransactionsSplitSharesLedgerKeyAndAscending（验收 2）拆分条目同 Name、无 Key，
// 且输出按到期升序 —— 聚合后 ledger 的 Expire 即最早一笔。
func TestTransactionsSplitSharesLedgerKeyAndAscending(t *testing.T) {
	d3, d4, d5 := txStamp(3), txStamp(4), txStamp(5)
	f := &fakeSite{
		balanceBody: `{"success":true,"data":{"total_balance":900,"available_balance":900,"frozen_amount":0}}`,
		usageBody:   fakeUsageBody,
		txBody:      txEnvelope(earnRecord(d5, 300), earnRecord(d3, 300), earnRecord(d4, 300)),
	}
	c := newSiteClient(provider.ModelScope, f.start(t).URL, "ms-x")
	_, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "ms-x"})
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	var split []provider.ResourceItem
	keys := map[string]bool{}
	for _, it := range items {
		if it.Name != itemNameUsable {
			continue
		}
		split = append(split, it)
		if it.Key != "" {
			t.Fatalf("拆分条目不得设 Key: %+v", it)
		}
		keys[it.Name] = true
	}
	if len(split) != 3 || len(keys) != 1 {
		t.Fatalf("拆分条目 = %d / 键数 = %d, want 3 / 1（同 Name 无 Key）", len(split), len(keys))
	}
	want := []string{wantStamp(d3), wantStamp(d4), wantStamp(d5)}
	for i, it := range split {
		if it.ExpireAt != want[i] {
			t.Fatalf("第 %d 条到期 = %q, want %q（升序）", i, it.ExpireAt, want[i])
		}
	}
}

// TestTransactionsEmptyPoolKeepsExpiry（验收 3）池子为空时仍挂「最早一笔在效发放」的
// 到期时刻，否则「有效期」整列会随余额花完而消失（R107 同款行为）。
func TestTransactionsEmptyPoolKeepsExpiry(t *testing.T) {
	d3, d4 := txStamp(3), txStamp(4)
	f := &fakeSite{
		balanceBody: `{"success":true,"data":{"total_balance":0,"available_balance":0,"frozen_amount":0}}`,
		usageBody:   fakeUsageBody,
		txBody:      txEnvelope(earnRecord(d4, 250), earnRecord(d3, 250)),
	}
	c := newSiteClient(provider.ModelScope, f.start(t).URL, "ms-x")
	_, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "ms-x"})
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	var split []provider.ResourceItem
	for _, it := range items {
		if it.Name == itemNameUsable {
			split = append(split, it)
		}
	}
	if len(split) != 1 {
		t.Fatalf("池子为空不拆笔，条目 = %d, want 1: %+v", len(split), split)
	}
	if split[0].ExpireAt != wantStamp(d3) {
		t.Fatalf("到期 = %q, want 最早一笔 %q", split[0].ExpireAt, wantStamp(d3))
	}
	if split[0].Remain != 0 || split[0].Total != 0 {
		t.Fatalf("不得凭空造余额: %+v", split[0])
	}
}

// TestTransactionsIgnoresNonEarn 只认 EARN：SPEND_CONFIRM / EXPIRE 不参与拆分
// （它们用于对账，不单独成条）。
//
// 夹具刻意让非 EARN 行**更早到期**且池子大到需要动用两笔：若类型筛选失效，
// 拆分条目会从 1 条变成 2 条（保证这条断言真能咬住类型筛选）。
func TestTransactionsIgnoresNonEarn(t *testing.T) {
	soon := txStamp(2)
	later := txStamp(4)
	f := &fakeSite{
		balanceBody: `{"success":true,"data":{"total_balance":450,"available_balance":450,"frozen_amount":0}}`,
		usageBody:   fakeUsageBody,
		txBody: txEnvelope(
			fmt.Sprintf(`{"gmt_created":"%s","type":"SPEND_CONFIRM","rule_key":"api_inference","total_amount":150,"count":1,"expire_at":"%s"}`,
				time.Now().In(creditLoc).Format("2006-01-02"), soon),
			fmt.Sprintf(`{"gmt_created":"%s","type":"EXPIRE","rule_key":"expire","total_amount":200,"count":2}`,
				time.Now().In(creditLoc).Format("2006-01-02")),
			earnRecord(later, 300),
		),
	}
	c := newSiteClient(provider.ModelScope, f.start(t).URL, "ms-x")
	_, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "ms-x"})
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	var split []provider.ResourceItem
	for _, it := range items {
		if it.Name == itemNameUsable {
			split = append(split, it)
		}
	}
	if len(split) != 1 {
		t.Fatalf("拆分条目 = %d, want 1（非 EARN 行不得参与拆分）: %+v", len(split), split)
	}
	// 450 分池 + 只有一笔 300 的 EARN：余量 150 挂在这一笔上。
	if split[0].Remain != 450 || split[0].ExpireAt != wantStamp(later) {
		t.Fatalf("余额应全部挂在未到期 EARN 上: %+v", split[0])
	}
}

// TestTransactionsDropsExpiredGrants 已到期的 EARN 必须剔除（否则「按到期升序」的
// 第一笔永远是过期笔，今日到期恒为 0）。全部发放都已过期时退回「无到期信息」口径。
func TestTransactionsDropsExpiredGrants(t *testing.T) {
	f := &fakeSite{
		balanceBody: `{"success":true,"data":{"total_balance":0,"available_balance":0,"frozen_amount":0}}`,
		usageBody:   fakeUsageBody,
		txBody:      txEnvelope(earnRecord(txStamp(-1), 300)),
	}
	c := newSiteClient(provider.ModelScope, f.start(t).URL, "ms-x")
	_, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "ms-x"})
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	for _, it := range items {
		if it.Name == itemNameUsable && it.ExpireAt != "" {
			t.Fatalf("已过期的发放不得成为到期判据（ExpireAt = %q）", it.ExpireAt)
		}
	}
}

// TestTransactionsFailureDegrades（验收 5，R107 教训）transactions 失败只退回
// 「无到期信息」口径：余额仍返回、明细仍在，不得整体报错。
func TestTransactionsFailureDegrades(t *testing.T) {
	f := &fakeSite{
		balanceBody: fakeBalanceBody,
		usageBody:   fakeUsageBody,
		txStatus:    http.StatusInternalServerError,
		txBody:      `{"success":false,"code":"boom"}`,
	}
	c := newSiteClient(provider.ModelScope, f.start(t).URL, "ms-x")
	logs := captureLog(t)
	remain, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "ms-x"})
	if err != nil {
		t.Fatalf("transactions 失败不得让余额刷新失败: %v", err)
	}
	if remain != 350 {
		t.Fatalf("remain = %d, want 350", remain)
	}
	found := false
	for _, it := range items {
		if it.Name == itemNameUsable {
			found = true
			if it.ExpireAt != "" {
				t.Fatalf("降级后应为原聚合条目（无到期时刻）: %+v", it)
			}
			if it.Remain != 350 {
				t.Fatalf("降级后余额不得变: %+v", it)
			}
		}
	}
	if !found {
		t.Fatal("降级后可用魔粒条目丢失")
	}
	if !strings.Contains(logs.String(), "魔粒到期查询失败") {
		t.Errorf("降级必须留日志，实际：%q", logs.String())
	}
}

// TestExpiringWindowShowsMagicubes（验收 4）「今日到期 / N 日内到期」出现魔粒数字，
// 且「最近临期」按剩余时长升序 —— 用固定 now 保证与墙上时钟无关。
func TestExpiringWindowShowsMagicubes(t *testing.T) {
	loc := creditLoc
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, loc)
	expToday := "2026-11-16T20:00:00+08:00" // 今晚到期
	expLater := "2026-11-19T20:00:00+08:00" // 3 天后到期
	if got := provider.ExpireBucketOf(expToday, now); got != provider.BucketToday {
		t.Fatalf("夹具前提不成立：expToday 不在今日桶（bucket=%d）", got)
	}

	items := balanceItems(magicubesBalance{TotalBalance: 500, AvailableBalance: 500})
	split := withMagicubeExpiry(items, []magicubeGrant{
		{Points: 250, ExpireAt: expToday},
		{Points: 250, ExpireAt: expLater},
	})
	if got := provider.ExpiringWithinDays(split, 1, now); got != 250 {
		t.Fatalf("今日到期 = %d, want 250（「今日到期」必须出现魔粒数字）", got)
	}
	if got := provider.ExpiringWithinDays(split, 7, now); got != 500 {
		t.Fatalf("7 日内到期 = %d, want 500", got)
	}
	// EarliestExpire（选号判据）据此自动生效，无需改 pool 逻辑。
	if earliest, ok := provider.EarliestExpire(split, now); !ok || earliest.Format(time.RFC3339) != expToday {
		t.Fatalf("EarliestExpire = %v (ok=%v), want %s", earliest, ok, expToday)
	}
}

// TestNormalizeMagicubeExpire 上游格式换算：空格分隔的 UTC+8 墙钟串 → RFC3339 精确时刻。
// 纯日期写法不得用于本渠道（魔粒是「发放后 24h 精确过期」）。
func TestNormalizeMagicubeExpire(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"2026-11-17 10:00:36", "2026-11-17T10:00:36+08:00", true},
		{" 2026-11-17 10:00:36 ", "2026-11-17T10:00:36+08:00", true},
		{"2026-11-17T10:00:36Z", "2026-11-17T18:00:36+08:00", true}, // 容错：上游改发 RFC3339
		{"", "", false},
		{"2026-11-17", "", false}, // 纯日期不接受（会丢时分秒）
		{"not-a-timestamp", "", false},
	}
	for _, c := range cases {
		got, ok := normalizeMagicubeExpire(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("normalizeMagicubeExpire(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
