package stats

import "testing"

// TestTokenUnitExcludedFromCreditTotals 守门：credits_unit="token" 的账号与匿名渠道同理 ——
// 计入账号数，但**不进**积分合计、**不进**差值消耗口径（token 与积分是两套计量）。
//
// 背景（2026-10-17 用户要求）：ZCode 的额度是 token 桶（动辄上亿）。若它参与 creditVal
// 或差值口径，「总积分」「今日消耗」会被 token 数污染 —— 这正是用户实测报告的问题。
func TestTokenUnitExcludedFromCreditTotals(t *testing.T) {
	s := newTestStats(t)
	s.ApplyPoll([]Account{
		acct1(1000),
		{UID: "zc", Group: "zcode", Nickname: "ZCode", Credits: 94539275, CreditsUnit: "token"},
	}, tNow(1, 9))
	snap := s.Snapshot(tNow(1, 9))
	if snap.TotalAccounts != 2 {
		t.Fatalf("token 渠道账号应计入账号数: got %d", snap.TotalAccounts)
	}
	if snap.TotalCredits != 1000 {
		t.Fatalf("token 数不得进积分合计: got %d（期望 1000）", snap.TotalCredits)
	}

	// token 数下降不是「积分消耗」：差值门禁必须拦住，否则今日消耗被上亿数字污染。
	s.ApplyPoll([]Account{
		acct1(1000),
		{UID: "zc", Group: "zcode", Nickname: "ZCode", Credits: 90000000, CreditsUnit: "token"},
	}, tNow(1, 10))
	snap = s.Snapshot(tNow(1, 10))
	if snap.CreditOut != 0 {
		t.Fatalf("token 减少不得计入积分消耗: got %d", snap.CreditOut)
	}
}
