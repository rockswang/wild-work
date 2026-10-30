package stats

import (
	"testing"
	"time"
)

// TestPlatformRowCreditExact comate 这类「无余额端点」渠道的今日消耗只能靠
// 逐请求 usage.credits 累计（R130）：差值口径恒 0，若平台行不带 credit_exact，
// 面板「今日消耗」整列恒「—」，用户会以为没用过。
func TestPlatformRowCreditExact(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	s.ApplyPoll([]Account{{UID: "cm", Group: "comate", Credits: 0}}, now)
	s.AddUsage("comate/glm-5.3-flash", 100, 20, 0.01, 0, 0, false, now)
	s.AddUsage("comate/glm-5.3-flash", 200, 40, 0.02, 0, 0, false, now)

	snap := s.Snapshot(now)
	var row *PlatformRow
	for i := range snap.Platforms {
		if snap.Platforms[i].Group == "comate" {
			row = &snap.Platforms[i]
		}
	}
	if row == nil {
		t.Fatal("comate 平台行缺失（全零过滤不应吃掉只有 credit_exact 的行）")
	}
	if row.CreditExact != 0.03 {
		t.Fatalf("credit_exact = %v, want 0.03（0.01+0.02 逐请求累计）", row.CreditExact)
	}
	// 差值口径仍为 0：两个口径并列，不互相污染
	if row.CreditUsed != 0 || row.CreditOut != 0 {
		t.Fatalf("差值口径应仍为 0: out=%d used=%d", row.CreditOut, row.CreditUsed)
	}
	// Token 也必须进表：channelKind 白名单漏了 comate 会让 token 与请求全丢
	if row.Tokens != 360 {
		t.Fatalf("tokens = %d, want 360（100+20+200+40）", row.Tokens)
	}
}

// TestChannelKindCoversComate channelKind 白名单必须含 comate：
// 漏了它 ⇒ 该渠道 token/请求不进平台概览表，且 credit_exact 也无处归属。
func TestChannelKindCoversComate(t *testing.T) {
	if !IsKnownChannel("comate") {
		t.Fatal("channelKind 缺少 comate（会静默丢该渠道全部 token 统计）")
	}
}
