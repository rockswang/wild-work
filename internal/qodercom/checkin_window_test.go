// checkin_window_test.go QoderCOM 签到「活动有效窗口」回归测试。
//
// 为什么要单独测窗口：本仓曾把 qodercom 判成 no_campaign（「以为上游没发活动」）。2026-10-18
// 的单发只读探测（3 个本机 qodercom 账号各 1 次 GET /sash/api/v1/me/campaigns，无重试、
// 无领取）证明本工具**能**拿到 CLAIM_BENEFIT 活动，并顺带暴露了每条活动的有效窗口字段。
// 证据（原样摘自 account 0 的回包，仅去掉无关的 placements 文案）：
//
//	"campaignKey":"act-20261013-119","actionType":"CLAIM_BENEFIT",
//	"startAt":1792202400,"endAt":1792288740,"claimStatus":"CLAIMED"
//
// endAt=1792288740 = 2026-10-18 09:59 +08（= 01:59 UTC），startAt=1792202400 = 2026-10-17 10:00 +08，
// 探测发生在 2026-10-18 01:10 +08 —— 该活动**当时确实仍在有效窗口内**（离 endAt 还有约 8.8 小时），
// 所以「窗口」这条在 10-18 的探测里只是**结构证据**（字段确实存在、取值语义清楚），
// 而桌面端 0.4.8 日志给出了同一条活动的**状态随窗口迁移**：
//
//	07:47:46 claimable=true  → 列表里那条 CLAIM_BENEFIT 的 claimStatus 是 CLAIMABLE；
//	08:47:45 claimable=true  → 同上；
//	15:52:42 claimable=true  → 仍是 CLAIMABLE；14 秒后 forceRefresh 才变 CLAIMED。
//
// 结论（分层标注）：startAt/endAt 是**实测**字段（探测回包里每条活动都带）；
// 「过期 CLAIM_BENEFIT 条目会留在列表里」这一形态本身**尚未直接观察到**，按**推断**对待。
// 正因是推断，本文件测的是**不变量**而不是某次回包：
//
//  1. 窗口已结束的 CLAIMED 活动**绝不能**把当天判成「今日已领取」——否则窗口内不再重试，
//     等于当天漏领（与 AGENTS.md 不变式 23「假成功」同类）。
//  2. 窗口未开始的 CLAIMABLE 活动**绝不能**发 claim（否则把「还没到点」记成一条假失败）。
//  3. 上游不下发 startAt/endAt（0 值）时行为与改动前一致（向后兼容）。
package qodercom

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"wild-work/internal/provider"
)

// windowsAroundNow 返回与「现在」相关的三个时刻（Unix 秒），供夹具构造。
// 用真实的 time.Now 而不是固定时间戳：tryCampaigns 内部取 time.Now，
// 固定值会让测试在几年后自己过期。
func windowsAroundNow() (ended, active, pending int64) {
	now := time.Now().Unix()
	return now - 24*3600, now - 60, now + 24*3600
}

// TestEndedClaimBenefitDoesNotMaskNoCampaign 今日活动尚未刷新、列表里只剩**已结束**的
// CLAIM_BENEFIT(CLAIMED) 时：必须是 no_campaign（可重试），且文案点明「已结束」。
// 反向验证：去掉 campaignWindow 的 ended 分支，本用例会变成 already_claimed → 红。
func TestEndedClaimBenefitDoesNotMaskNoCampaign(t *testing.T) {
	ended, _, _ := windowsAroundNow()
	withCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != EpCampaigns {
			t.Errorf("残留活动不得触发 claim：%s %s", r.Method, r.URL.Path)
			return
		}
		w.Write([]byte(`{"showCampaign":true,"claimable":false,"campaigns":[
		  {"campaignId":"old","campaignKey":"act-20261013-119","actionType":"CLAIM_BENEFIT",` +
			`"startAt":` + itoa(ended-3600) + `,"endAt":` + itoa(ended) + `,"claimStatus":"CLAIMED",` +
			`"benefit":{"kind":"CREDITS","amount":100}}]}`))
	})

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("no_campaign 不是 error：%v", err)
	}
	if rep.Status != provider.CheckinNoCampaign || !rep.Status.Retryable() {
		t.Fatalf("report = %+v, want no_campaign (retryable)：已结束的残留活动不得判成今日已领", rep)
	}
	for _, want := range []string{"已结束", "CLAIM_BENEFIT=CLAIMED[已结束]", "（等上游刷新今日活动）", "本机时间"} {
		if !strings.Contains(rep.Msg, want) {
			t.Errorf("诊断文案缺 %q：%s", want, rep.Msg)
		}
	}
}

// TestPendingClaimBenefitWaits 窗口未开始的 CLAIMABLE 活动：不发 claim，
// 判成 no_campaign 可重试，文案标「未开始」。
// 反向验证：去掉 pending 分支会真的 POST 出去 → 红。
func TestPendingClaimBenefitWaits(t *testing.T) {
	_, _, pending := windowsAroundNow()
	withCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != EpCampaigns {
			t.Errorf("未开始的活动不得发 claim：%s %s", r.Method, r.URL.Path)
			return
		}
		w.Write([]byte(`{"showCampaign":true,"claimable":true,"campaigns":[
		  {"campaignId":"future","campaignKey":"act-20261019-001","actionType":"CLAIM_BENEFIT",` +
			`"startAt":` + itoa(pending) + `,"endAt":` + itoa(pending+86400) + `,"claimStatus":"CLAIMABLE",` +
			`"benefit":{"kind":"CREDITS","amount":100}}]}`))
	})

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if rep.Status != provider.CheckinNoCampaign || !rep.Status.Retryable() {
		t.Fatalf("report = %+v, want no_campaign (retryable)", rep)
	}
	if !strings.Contains(rep.Msg, "未开始") || !strings.Contains(rep.Msg, "CLAIM_BENEFIT=CLAIMABLE[未开始]") {
		t.Errorf("诊断文案缺「未开始」标注：%s", rep.Msg)
	}
}

// TestActiveWindowStillClaims 有效窗口内的 CLAIMABLE 仍要照常领取（别把过滤写成「一律不领」）。
func TestActiveWindowStillClaims(t *testing.T) {
	_, active, _ := windowsAroundNow()
	var claimed bool
	withCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == EpCampaigns && r.Method == http.MethodGet:
			w.Write([]byte(`{"showCampaign":true,"claimable":true,"campaigns":[
			  {"campaignId":"today","campaignKey":"act-20261018-001","actionType":"CLAIM_BENEFIT",` +
				`"startAt":` + itoa(active-3600) + `,"endAt":` + itoa(active+86400) + `,"claimStatus":"CLAIMABLE",` +
				`"benefit":{"kind":"CREDITS","amount":100}}]}`))
		case strings.HasSuffix(r.URL.Path, "/claim") && r.Method == http.MethodPost:
			claimed = true
			w.Write([]byte(desktopClaimOkPayload))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if !claimed || rep.Status != provider.CheckinClaimed || rep.Amount != 100 {
		t.Fatalf("有效窗口内的活动必须照常领取：claimed=%t report=%+v", claimed, rep)
	}
}

// TestActiveClaimedIsAlready 有效窗口内的 CLAIMED 才是「今日已领取」。
func TestActiveClaimedIsAlready(t *testing.T) {
	_, active, _ := windowsAroundNow()
	withCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != EpCampaigns {
			t.Errorf("已领取不得再发 claim：%s %s", r.Method, r.URL.Path)
			return
		}
		w.Write([]byte(`{"showCampaign":true,"claimable":false,"campaigns":[
		  {"campaignId":"today","campaignKey":"act-20261018-001","actionType":"CLAIM_BENEFIT",` +
			`"startAt":` + itoa(active-3600) + `,"endAt":` + itoa(active+86400) + `,"claimStatus":"CLAIMED",` +
			`"benefit":{"kind":"CREDITS","amount":100}}]}`))
	})

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if rep.Status != provider.CheckinAlready || rep.Amount != 100 {
		t.Fatalf("report = %+v, want already +100", rep)
	}
	if rep.Status.Retryable() {
		t.Error("already 不应重试")
	}
}

// TestMissingWindowFieldsKeepLegacyBehavior 上游不下发 startAt/endAt（0 值）时：
// CLAIMED 仍按已领取处理（向后兼容，不因新增字段把旧回包判错）。
func TestMissingWindowFieldsKeepLegacyBehavior(t *testing.T) {
	withCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != EpCampaigns {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return
		}
		w.Write([]byte(`{"showCampaign":true,"claimable":false,"campaigns":[
		  {"campaignId":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED",` +
			`"benefit":{"kind":"CREDITS","amount":100}}]}`))
	})

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if rep.Status != provider.CheckinAlready || rep.Amount != 100 {
		t.Fatalf("report = %+v, want already +100（缺时间字段时不做时间过滤）", rep)
	}
}

// TestCampaignWindowBoundaries 边界：endAt == now 视为已结束（<= 而非 <），
// startAt == now 视为已开始（可领）。这两个判据写反会让「刚好到点/刚好过点」各错一次。
func TestCampaignWindowBoundaries(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		name string
		cp   campaignNode
		want string
	}{
		{"无时间字段", campaignNode{}, "active"},
		{"刚结束", campaignNode{StartAt: now - 100, EndAt: now}, "ended"},
		{"已结束", campaignNode{StartAt: now - 100, EndAt: now - 1}, "ended"},
		{"刚开放", campaignNode{StartAt: now, EndAt: now + 100}, "active"},
		{"未开始", campaignNode{StartAt: now + 1, EndAt: now + 100}, "pending"},
		{"只有 endAt 且在期内", campaignNode{EndAt: now + 100}, "active"},
		{"只有 startAt 且未到点", campaignNode{StartAt: now + 100}, "pending"},
	}
	for _, c := range cases {
		if got := campaignWindow(c.cp, now); got != c.want {
			t.Errorf("%s: campaignWindow = %s, want %s", c.name, got, c.want)
		}
	}
}

// and 让夹具里的时间戳保持十进制，不引入 fmt（本文件只做字符串拼接）。
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// TestNoEligibilityMessage 上游**完全没有**下发领积分活动时：
// 必须直说「该账号无积分领取活动资格」，并点明「不是本工具的问题」——
// 这是 2026-10-18 用户实测定性（上游按账号下发活动资格）之后的核心判据。
// 实测依据：同一个号在官方客户端里也没有该入口；本机 3 个账号里只有 1 个有资格，
// 工具端与桌面端逐账号结果完全一致（见 checkin.go 头部）。
func TestNoEligibilityMessage(t *testing.T) {
	withCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != EpCampaigns {
			t.Errorf("无资格时不得发 claim：%s %s", r.Method, r.URL.Path)
			return
		}
		// 真实形态：只有 VIEW_DETAILS（推广活动），没有任何 CLAIM_BENEFIT
		w.Write([]byte(`{"showCampaign":true,"claimable":false,"campaigns":[
		  {"campaignId":"c2","campaignKey":"act-20260901-493","actionType":"VIEW_DETAILS",
		   "startAt":1788247200,"endAt":1793462340,"claimStatus":"CLAIMED"}]}`))
	})

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if rep.Status != provider.CheckinNoCampaign || !rep.Status.Retryable() {
		t.Fatalf("report = %+v, want no_campaign (retryable)", rep)
	}
	for _, want := range []string{"无积分领取活动资格", "不是本工具的问题"} {
		if !strings.Contains(rep.Msg, want) {
			t.Errorf("无资格文案缺 %q：%s", want, rep.Msg)
		}
	}
	// 不得再说成「上游未下发可领取活动」那种含糊表述（历史误判的措辞）
	if strings.Contains(rep.Msg, "上游未下发可领取活动") {
		t.Errorf("无资格文案不得用含糊措辞「上游未下发可领取活动」：%s", rep.Msg)
	}
}

// TestEligibleButOutOfWindowMessage 有资格但窗口不合时，文案要说「有资格」，
// 且**不得**说成「无资格」——两者必须可区分，否则用户会误以为自己的号被判定成不合格。
func TestEligibleButOutOfWindowMessage(t *testing.T) {
	ended, _, _ := windowsAroundNow()
	withCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != EpCampaigns {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return
		}
		w.Write([]byte(`{"showCampaign":true,"claimable":false,"campaigns":[
		  {"campaignId":"old","campaignKey":"act-20261013-119","actionType":"CLAIM_BENEFIT",` +
			`"startAt":` + itoa(ended-3600) + `,"endAt":` + itoa(ended) + `,"claimStatus":"CLAIMED",` +
			`"benefit":{"kind":"CREDITS","amount":100}}]}`))
	})

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if !strings.Contains(rep.Msg, "有资格") {
		t.Errorf("窗口不合时文案应说「有资格」：%s", rep.Msg)
	}
	if strings.Contains(rep.Msg, "无积分领取活动资格") {
		t.Errorf("有活动的账号不得说成「无资格」：%s", rep.Msg)
	}
}
