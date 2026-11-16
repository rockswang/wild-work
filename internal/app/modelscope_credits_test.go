package app

import (
	"testing"

	"wild-work/internal/provider"
)

// TestModelScopeCreditsFailureKeepsSnapshot 5xx / 网络错误（D5）：
//   - 保留上次快照值（**不写 0**）；
//   - 不把账号标成「不适用」（credits_na=false）；
//   - 不产生账号级惩罚（不 Disable、不 Cooling）——一次上游抖动不得让整条渠道下线。
func TestModelScopeCreditsFailureKeepsSnapshot(t *testing.T) {
	up := &fakeUpstream{remain: 350}
	a, p := newTestApp(t, provider.ModelScope, up, "mst-0000000000000001")

	// 先成功一次落快照（真实余额 350）。
	if _, err := a.RefreshCredits("mst-0000000000000001"); err != nil {
		t.Fatalf("首次刷新应成功：%v", err)
	}
	if st, _ := p.Status("mst-0000000000000001"); st.Credits != 350 {
		t.Fatalf("首次快照 credits=%d，期望 350", st.Credits)
	}

	// 上游转为 5xx。
	up.failResource.Store(true)
	if _, err := a.RefreshCredits("mst-0000000000000001"); err == nil {
		t.Fatal("上游 5xx 应报错（面板据此提示刷新失败），却成功了")
	}
	st, _ := p.Status("mst-0000000000000001")
	if st.Credits != 350 {
		t.Errorf("刷新失败后快照被改写：credits=%d，期望保留 350（不得写 0）", st.Credits)
	}
	if st.Disabled || st.Cooling {
		t.Errorf("余额读取失败不得产生账号级惩罚：disabled=%v cooling=%v", st.Disabled, st.Cooling)
	}

	var view *AccountView
	views := a.accountViews()
	for i := range views {
		if views[i].UID == "mst-0000000000000001" {
			view = &views[i]
		}
	}
	if view == nil {
		t.Fatal("账号视图缺失")
	}
	if view.CreditsNA {
		t.Error("两区已有真实魔粒余额端点，不得再标「不适用」")
	}
	if view.Credits != 350 {
		t.Errorf("面板 credits=%d，期望保留 350（不是 0）", view.Credits)
	}
	if view.CreditsTerm != "魔粒" {
		t.Errorf("credits_term=%q，期望「魔粒」（面板文案术语）", view.CreditsTerm)
	}
}

// TestModelScopeSessionDeadNoPenalty 401/403 → ErrSessionDead（可自愈标记）：
// 本渠道没有 refresh 端点（RefreshToken 空实现）⇒ 不做自动 refresh，
// 也**不得** Disable / Cooldown（否则一次失效会把唯一账号永久踢出池子）。
func TestModelScopeSessionDeadNoPenalty(t *testing.T) {
	up := &fakeUpstream{remain: 350}
	up.sessionDead.Store(true)
	a, p := newTestApp(t, provider.ModelScope, up, "mst-dead")
	if au := p.AuthByUID("mst-dead"); au != nil {
		au.RefreshToken = "" // 真实 `ms-` 凭据没有 refresh_token
	}

	if _, err := a.RefreshCredits("mst-dead"); err == nil {
		t.Fatal("401 应报错")
	}
	if n := up.refreshCalls.Load(); n != 0 {
		t.Errorf("无 refresh token 时不得尝试刷新（本渠道也没有 refresh 端点），却调了 %d 次", n)
	}
	st, _ := p.Status("mst-dead")
	if st.Disabled {
		t.Error("401 不得 Disable（R76：登录态失效走可自愈标记，不是禁用）")
	}
	if st.Cooling {
		t.Error("401 不得 Cooldown（凭据问题不是限流，重贴 token 即恢复）")
	}
}
