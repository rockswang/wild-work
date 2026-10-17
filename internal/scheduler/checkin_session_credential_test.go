// checkin_session_credential_test.go 回归：长会话渠道的账号（**无 refresh token**、
// 只有 accessToken）必须能走到渠道的签到实现。
//
// 背景（2026-10-17 冒烟测试发现，属发布级缺陷）：
// checkinOne 的旧守卫是 `a == nil || a.RefreshToken == ""`，把「凭据本体是 accessToken」
// 的渠道一律当 no_token 提前返回。而 loomy / monkeycode / zcode 三个渠道的账号
// refreshToken **刻意留空**（上游没有 refresh 端点，见各自 checkin 实现与 auth 落盘逻辑），
// 于是：
//   - monkeycode 上一轮刚实装的签到（POST /api/v1/users/wallet/checkin）**从未被调用过** ——
//     点面板「签到」只回一句「no refresh token」，看着像上游问题；
//   - loomy 的宠物打工领奖同样到不了实现。
//
// 面板上看不出异常、渠道侧日志也没有记录 —— 典型的「功能写了却不生效」，只有盯住
// 「签名被调用了没有」才能发现。故此处直接断言渠道方法被调用。
package scheduler

import (
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// TestCheckinSessionCredentialReachesImplementation 长会话账号必须真的被签到。
func TestCheckinSessionCredentialReachesImplementation(t *testing.T) {
	p := pool.New("")
	// 关键形态：refreshToken 留空，只有 accessToken（loomy / monkeycode / zcode 的真实样子）。
	p.Add(&auth.Auth{
		UID:         "sess1",
		AccessToken: "session-token",
		ExpiresAt:   time.Now().Add(14 * 24 * time.Hour).Unix(),
	})

	up := &dispatchUpstream{
		report: provider.CheckinReport{Status: provider.CheckinClaimed, Amount: 300},
		remain: 500,
	}
	s := New(Config{Pool: p, Upstream: up, Name: "loomy", Ledger: newDispatchLedger(t)})

	res, err := s.CheckinAccount("sess1")
	if err != nil {
		t.Fatalf("CheckinAccount: %v", err)
	}
	if up.calls == 0 {
		t.Fatalf("渠道签到实现未被调用（结果 %+v）—— 守卫又把长会话账号挡死了；"+
			"这正是「写了却不生效」的形态", res)
	}
	if !res.OK {
		t.Fatalf("claimed 应算成功：%+v", res)
	}
	if res.Retryable {
		t.Fatalf("claimed 不应标可重试：%+v", res)
	}
}

// TestCheckinNoCredentialStillShortCircuits 反向：**两种凭据都为空**才算无凭据，
// 此时仍要提前返回、且不得调用渠道（不去打无谓的上游请求）。
func TestCheckinNoCredentialStillShortCircuits(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "empty1", ExpiresAt: time.Now().Add(time.Hour).Unix()})

	up := &dispatchUpstream{report: provider.CheckinReport{Status: provider.CheckinClaimed}}
	s := New(Config{Pool: p, Upstream: up, Name: "loomy"})

	res, _ := s.CheckinAccount("empty1")
	if up.calls != 0 {
		t.Fatalf("无任何凭据时不应调用渠道，实际 calls=%d", up.calls)
	}
	if res.OK {
		t.Fatalf("无凭据不应算成功：%+v", res)
	}
}

// TestCheckinSessionCredentialSkipsRefresh 长会话账号不得走「签到前先刷新」这一步。
//
// 它没有 refresh token 可刷；若仍进入该分支，渠道的 RefreshToken 会返回错误，
// 结果被写成「refresh: …」——把「今天没领到」误报成「凭据刷新失败」，误导排查方向。
func TestCheckinSessionCredentialSkipsRefresh(t *testing.T) {
	p := pool.New("")
	// 刻意让 accessToken 已进入「2 小时内到期」窗口，触发 NeedsRefresh 判定；
	// 因为是长会话（无 refresh token），该判定必须被跳过。
	p.Add(&auth.Auth{
		UID:         "sess2",
		AccessToken: "session-token",
		ExpiresAt:   time.Now().Add(30 * time.Minute).Unix(),
	})

	up := &refreshRecorder{report: provider.CheckinReport{Status: provider.CheckinClaimed, Amount: 10}}
	s := New(Config{Pool: p, Upstream: up, Name: "loomy"})

	if _, err := s.CheckinAccount("sess2"); err != nil {
		t.Fatalf("CheckinAccount: %v", err)
	}
	if up.refreshCalls != 0 {
		t.Fatalf("长会话账号不应触发 RefreshToken，实际 %d 次", up.refreshCalls)
	}
	if up.checkinCalls == 0 {
		t.Fatal("签到实现仍应被调用")
	}
}

// refreshRecorder 记录 RefreshToken 与签到各被调用几次。
type refreshRecorder struct {
	provider.Upstream
	report       provider.CheckinReport
	refreshCalls int32
	checkinCalls int32
}

func (r *refreshRecorder) RefreshToken(a *auth.Auth) error {
	r.refreshCalls++
	return nil
}

func (r *refreshRecorder) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	r.checkinCalls++
	return r.report, nil
}

func (r *refreshRecorder) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 100, []provider.ResourceItem{{Key: "k", Name: "n", Remain: 100, Usable: true}}, nil
}
