package provider

import (
	"strings"
	"testing"
	"time"
)

// timeNow 返回 UTC+8 墙钟当前时刻（与 expireLoc 口径一致，测试用）。
func timeNow() time.Time { return time.Now().In(expireLoc) }

func TestExpiringWithin(t *testing.T) {
	today := timeNow().Format("2006-01-02")
	tomorrow := timeNow().AddDate(0, 0, 1).Format("2006-01-02")
	after := timeNow().AddDate(0, 0, 3).Format("2006-01-02")

	items := []ResourceItem{
		{Name: "今日到期", Remain: 100, Usable: true, ExpireAt: today},
		{Name: "明天到期", Remain: 50, Usable: true, ExpireAt: tomorrow},
		{Name: "三天后到期", Remain: 999, Usable: true, ExpireAt: after},
		{Name: "无到期时间", Remain: 70, Usable: true},
		{Name: "不可用池临期", Remain: 300, Usable: false, ExpireAt: today},
	}
	if got := ExpiringWithin(items, 24*time.Hour); got != 150 {
		t.Fatalf("expiring=%d want 150 (今天100+明天50，三天后/无到期/不可用不计)", got)
	}
}

// TestExpireInstant 失效时刻口径：日期粒度是「次日 00:00」，RFC3339 是精确时刻。
// 这是 ledger「是否已过期」与面板「临期」共用的唯一解析入口，两种写法都要覆盖。
func TestExpireInstant(t *testing.T) {
	// 日期粒度：当天仍可用 ⇒ 失效时刻落在次日零点
	got, ok := ExpireInstant("2026-10-18")
	if !ok {
		t.Fatal("日期粒度应可解析")
	}
	want := time.Date(2026, 10, 19, 0, 0, 0, 0, expireLoc)
	if !got.Equal(want) {
		t.Fatalf("日期粒度失效时刻 = %v，want %v（次日 00:00）", got, want)
	}

	// RFC3339：精确时刻，不做「次日」平移
	got, ok = ExpireInstant("2026-10-18T15:00:00+08:00")
	if !ok {
		t.Fatal("RFC3339 应可解析")
	}
	want = time.Date(2026, 10, 18, 15, 0, 0, 0, expireLoc)
	if !got.Equal(want) {
		t.Fatalf("RFC3339 失效时刻 = %v，want %v（原样）", got, want)
	}

	// 空串 / 不可解析：ok=false，调用方按「无到期信息」处理
	for _, s := range []string{"", "  ", "not-a-date", "2026-13-45"} {
		if _, ok := ExpireInstant(s); ok {
			t.Errorf("ExpireInstant(%q) 不应可解析", s)
		}
	}
}

// TestExpiringWithinRFC3339 临期统计要能吃 RFC3339（gemini 的窗口重置时间）。
// 日期粒度的既有口径（今天/明天都算临期）由 TestExpiringWithin 锁住，这里只补新格式。
func TestExpiringWithinRFC3339(t *testing.T) {
	now := timeNow()
	items := []ResourceItem{
		{Name: "两小时后重置", Remain: 40, Usable: true, ExpireAt: now.Add(2 * time.Hour).Format(time.RFC3339)},
		{Name: "两天后重置", Remain: 900, Usable: true, ExpireAt: now.Add(48 * time.Hour).Format(time.RFC3339)},
		{Name: "不可解析", Remain: 500, Usable: true, ExpireAt: "bogus"},
	}
	if got := ExpiringWithin(items, 24*time.Hour); got != 40 {
		t.Fatalf("expiring=%d want 40（只算 24h 内的 RFC3339 条目）", got)
	}
}

func TestLogBody(t *testing.T) {
	// 上游 400 信封（含尾部 extError 明细）必须整段保留——排障看的就是这一段。
	body := `{"code":11133,"msg":"the request parameters were rejected by the model provider",` +
		`"extError":{"code":"integer_below_min_value","message":"Invalid 'max_tokens': integer below minimum value. Expected a value >= 1, but got 0 instead."}}`
	if got := LogBody(body); got != body {
		t.Fatalf("短响应应原样返回，得到 %q", got)
	}

	long := strings.Repeat("a", 40000) + `"extError":{"code":"integer_below_min_value"}`
	got := LogBody(long)
	if len(got) >= len(long) {
		t.Fatalf("超长响应应被压缩，得到 %d 字节（原 %d）", len(got), len(long))
	}
	if !strings.HasPrefix(got, "aaaa") || !strings.HasSuffix(got, `"integer_below_min_value"}`) {
		t.Fatalf("头尾都应保留，得到 %q ... %q", got[:20], got[len(got)-40:])
	}
	if !strings.Contains(got, "log body truncated:") {
		t.Fatalf("省略处应有标记，得到 %q", got)
	}
}

func TestLogParams(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-luna","stream":true,"max_tokens":32000,` +
		`"thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hi"}],` +
		`"tools":[{"type":"function","function":{"name":"t"}}],"user":"u-1","instructions":"` + strings.Repeat("x", 300) + `"}`)
	got := LogParams(body)
	for _, unwanted := range []string{"messages", "tools", "function"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("参数摘要不应含 %q：%s", unwanted, got)
		}
	}
	for _, want := range []string{`"max_tokens":32000`, `"thinking":{"type":"enabled"}`, `"stream":true`} {
		if !strings.Contains(got, want) {
			t.Errorf("参数摘要缺少 %s：%s", want, got)
		}
	}
	if !strings.Contains(got, "...") {
		t.Errorf("超长字符串应截断：%s", got)
	}

	if got := LogParams([]byte("{broken")); got != "{broken" {
		t.Errorf("非 JSON 应原样返回，得到 %q", got)
	}
}

func TestSummarizeExpiring(t *testing.T) {
	items := []ResourceItem{
		{Remain: 100, Usable: true, ExpireAt: timeNow().Format("2006-01-02")},
		{Remain: 200, Usable: false},
	}
	u, un := Summarize(items)
	e := ExpiringWithin(items, 24*time.Hour)
	if u != 100 || un != 200 || e != 100 {
		t.Fatalf("usable=%d unusable=%d expiring=%d want 100/200/100", u, un, e)
	}
}

// TestInfoOnlyExcluded 只展示条目（单位与积分不同，如 MonkeyCode 每日 Token）
// 不得进入任何算术：既不算可消耗，也不算不可消耗，更不算临期。
func TestInfoOnlyExcluded(t *testing.T) {
	items := []ResourceItem{
		{Name: "积分余额", Remain: 55, Usable: true},
		{Name: "每日 Token 额度", Remain: 10000000, Usable: true, InfoOnly: true,
			ExpireAt: timeNow().Format("2006-01-02")},
	}
	u, un := Summarize(items)
	if u != 55 || un != 0 {
		t.Fatalf("usable=%d unusable=%d want 55/0（InfoOnly 两不计）", u, un)
	}
	if e := ExpiringWithin(items, 24*time.Hour); e != 0 {
		t.Fatalf("expiring=%d want 0（InfoOnly 不计临期）", e)
	}
}

// TestIsModelScopedSoftRate 判定软限流是否只作用于当前模型。
// 特征串取自上游 6004 频率限流原文；未命中时返回 false（账号级）是刻意的保守选择——
// 账号级最坏多冷却一个账号（有号可轮换），误判成模型级会让 N 个模型各白撞一次。
func TestIsModelScopedSoftRate(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			"6004 频率限流（原文声明可换其他模型）",
			`{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your usage will reset at 2026-09-28 22:37:01 UTC+8, alternatively, you can switch to the other models to continue using it.","requestId":"r1"}`,
			true,
		},
		{"大小写不敏感", `{"msg":"You Can Switch To The Other Models"}`, true},
		{"账号级限流（无特征串）", `{"code":9999,"msg":"too many requests"}`, false},
		{"14018 积分耗尽（账号级）", `{"code":14018,"msg":"quota exceeded"}`, false},
		{"空 body", ``, false},
	}
	for _, c := range cases {
		if got := IsModelScopedSoftRate(c.body); got != c.want {
			t.Errorf("%s: got=%v want=%v", c.name, got, c.want)
		}
	}
}
