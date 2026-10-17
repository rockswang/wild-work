// checkin_variant_test.go QoderCOM 签到「列表响应形态」回归测试。
//
// 取证来源（全部离线/本机，未打上游生产接口）：
//
//  1. 本机 Qoder 桌面端 0.4.8 主进程日志（%APPDATA%\com.qoder.app.stable\logs\<会话>\main.log）
//     的 "[Campaign] 活动状态请求返回" 行 —— 三份会话（1015 / 1017-0747 / 1017-1036）
//     逐字一致：claimStatus 是**扁平字符串**，例如
//     "actionType":"CLAIM_BENEFIT", …, "claimStatus":"CLAIMABLE"。
//     对全部本地日志 grep `"claimStatus":{"status"` = **0 命中**。
//  2. 单发 A/B 只读探测（2026-10-17 18:59 +08，同一 dt- 凭据、只差请求头）：
//     两次 GET /sash/api/v1/me/campaigns 回包**逐字节相同**（HTTP 200，len 1277），
//     内容即下面 realNoClaimablePayload —— 列表里只有 VIEW_DETAILS/CLAIMED，
//     **没有** CLAIM_BENEFIT 条目。⇒ 该账号此刻确实无可领取活动；本工具报
//     no_campaign 是如实反映上游，不是解析缺陷（头部注释详见 checkin.go）。
//
// ⚠️ 历史更正：本文件曾以「claimStatus 为**嵌套对象** {"status":…}」为桌面端真实形态，
// 并据此把 no_campaign 归因为解析失败。该形态在全部本地 0.4.8 日志与本次探测回包中
// **没有任何证据**（疑为早期会话转述时被改写）。现在对象形态只作为**防御性向后兼容**
// 保留（测试 5），不再声称它是线上形态。
package qodercom

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// realNoClaimablePayload 单发探测拿到的**真实响应**（字段顺序与取值原样保留；
// 仅裁剪 placements.content 的长文本，被裁处形状不变）。
const realNoClaimablePayload = `{
  "uid": "01a05725-40bb-74e2-8534-858686c422c6",
  "showCampaign": true,
  "claimable": false,
  "campaignUrl": "https://openapi.qoder.sh/growth-page/activity-iframe",
  "campaigns": [
    {
      "campaignId": "01a05bce-e800-7494-a002-806e4438f483",
      "campaignKey": "act-20260901-493",
      "actionType": "VIEW_DETAILS",
      "startAt": 1788247200,
      "endAt": 1793462340,
      "claimStatus": "CLAIMED",
      "placements": [
        {"type": "POPUP", "campaignUrl": "https://openapi.qoder.sh/growth-page/activity-iframe"},
        {"type": "USAGE"}
      ]
    }
  ]
}`

// desktopClaimOkPayload claim 接口的成功回执（桌面端同族字段形状）。
const desktopClaimOkPayload = `{"campaignId":"c1","status":"CLAIMED","replayed":false,"benefit":{"kind":"CREDITS","amount":100,"validity":{"mode":"RELATIVE_DAYS","days":30}}}`

// withCheckinServer 起一个假上游并把 checkinHost 指过去；handler 收到未知路径即失败。
func withCheckinServer(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := checkinHost
	checkinHost = srv.URL
	t.Cleanup(func() { checkinHost = old })
}

// TestRealPayloadWithoutClaimBenefitIsNoCampaign 真实响应（无 CLAIM_BENEFIT 条目）必须：
//   - 判成 no_campaign 且可重试（不是「今日已完成」，也不是 error）；
//   - **不发** claim 请求（没有可领对象时绝不能瞎领）；
//   - Msg 带上游摘要，让面板能区分「上游没发活动」与「本工具故障」。
func TestRealPayloadWithoutClaimBenefitIsNoCampaign(t *testing.T) {
	var claimCalled bool
	withCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != EpCampaigns {
			claimCalled = true
			t.Errorf("无可领活动时不得发 claim：%s %s", r.Method, r.URL.Path)
			return
		}
		w.Write([]byte(realNoClaimablePayload))
	})

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("no_campaign 不是 error：%v", err)
	}
	if rep.Status != provider.CheckinNoCampaign || !rep.Status.Retryable() {
		t.Fatalf("report = %+v, want no_campaign (retryable)", rep)
	}
	if claimCalled {
		t.Error("无可领活动时发出了 claim")
	}
	for _, want := range []string{"showCampaign=true", "claimable=false", "VIEW_DETAILS=CLAIMED", "1 条活动"} {
		if !strings.Contains(rep.Msg, want) {
			t.Errorf("诊断文案缺 %q：%s", want, rep.Msg)
		}
	}
}

// TestEmptyCampaignListDiagnostics 列表为空 → no_campaign，文案要如实说明这是**账号资格**问题。
//
// 2026-10-18 定性更新：空列表不必然是「10:00 整点还没创建」。用户实测确认
// **上游按账号下发活动资格**——没有资格的号，连活动体系开关都可能是关的（showCampaign=false），
// 而在官方客户端里同样看不到入口。因此文案必须说「该账号无积分领取活动资格」+「不是本工具的问题」，
// 否则用户会把资格问题读成工具故障（10-17 起反复误判的根源）。
func TestEmptyCampaignListDiagnostics(t *testing.T) {
	withCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpCampaigns || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return
		}
		w.Write([]byte(`{"showCampaign":false,"claimable":false,"campaigns":[]}`))
	})

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if rep.Status != provider.CheckinNoCampaign || !rep.Status.Retryable() {
		t.Fatalf("report = %+v, want no_campaign (retryable)", rep)
	}
	for _, want := range []string{"无积分领取活动资格", "不是本工具的问题", "showCampaign=false", "上游未下发任何活动"} {
		if !strings.Contains(rep.Msg, want) {
			t.Errorf("msg 缺 %q：%s", want, rep.Msg)
		}
	}
}

// TestFlatClaimableClaims 线上实际形态（扁平字符串 CLAIMABLE）必须被识别为可领取，
// 并真正发出 POST claim → claimed + amount。
func TestFlatClaimableClaims(t *testing.T) {
	var claimedPath string
	withCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == EpCampaigns && r.Method == http.MethodGet:
			w.Write([]byte(`{"showCampaign":true,"claimable":true,"campaigns":[{"campaignId":"c1","campaignKey":"act-20261013-119","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS","amount":100}}]}`))
		case strings.HasSuffix(r.URL.Path, "/claim") && r.Method == http.MethodPost:
			claimedPath = r.URL.Path
			w.Write([]byte(desktopClaimOkPayload))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if claimedPath == "" {
		t.Fatalf("列表里有 CLAIMABLE 活动时必须发出 claim；report = %+v", rep)
	}
	if !strings.Contains(claimedPath, "/c1/claim") {
		t.Errorf("claimed wrong campaign: %s", claimedPath)
	}
	if rep.Status != provider.CheckinClaimed || rep.Amount != 100 {
		t.Errorf("report = %+v, want claimed +100", rep)
	}
	if rep.Status.Retryable() {
		t.Error("claimed 不得标记可重试")
	}
}

// TestFlatClaimedIsAlreadyWithAmount 扁平 CLAIMED（今日已领）→ already_claimed，
// 且金额从活动自身 benefit.amount 回填，文案带金额。
func TestFlatClaimedIsAlreadyWithAmount(t *testing.T) {
	withCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpCampaigns || r.Method != http.MethodGet {
			t.Errorf("已领取时不得再发 claim：%s %s", r.Method, r.URL.Path)
			return
		}
		w.Write([]byte(`{"showCampaign":true,"claimable":false,"campaigns":[
		  {"campaignId":"c1","campaignKey":"act-20261013-119","actionType":"CLAIM_BENEFIT",
		   "claimStatus":"CLAIMED","benefit":{"kind":"CREDITS","amount":100}}]}`))
	})

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if rep.Status != provider.CheckinAlready {
		t.Fatalf("report = %+v, want already_claimed", rep)
	}
	if rep.Status.Retryable() {
		t.Error("already_claimed 不应当日重试")
	}
	if rep.Amount != 100 {
		t.Errorf("amount = %d, want 从 campaign.benefit.amount 回填 100", rep.Amount)
	}
	if !strings.Contains(rep.Msg, "100") {
		t.Errorf("msg = %q, want 含金额 100", rep.Msg)
	}
}

// TestObjectFormStillAccepted 防御性向后兼容：对象形态 claimStatus:{"status":…}
// **没有**线上证据（见文件头更正说明），但解析层仍接受它，且未知形态不会被误领。
func TestObjectFormStillAccepted(t *testing.T) {
	var claimed bool
	withCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == EpCampaigns && r.Method == http.MethodGet:
			w.Write([]byte(`{"campaigns":[{"campaignId":"c1","actionType":"CLAIM_BENEFIT","claimStatus":{"status":"CLAIMABLE"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/claim"):
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
	if !claimed || rep.Status != provider.CheckinClaimed {
		t.Fatalf("对象形态兼容性丢失：claimed=%t report=%+v", claimed, rep)
	}
}

// TestUnrecognizedEnumDoesNotClaim claimStatus 是没见过的新枚举时：
//   - 绝不发 claim（不误领）；
//   - 判成 no_campaign 并可重试；
//   - 诊断文案把**原始取值**带出来，让「上游新增了枚举」一眼可见。
//
// 这正是本渠道历史上被判错的盲区：取值变了却只有一句笼统的 no_campaign。
func TestUnrecognizedEnumDoesNotClaim(t *testing.T) {
	withCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != EpCampaigns {
			t.Errorf("未识别状态时不得发 claim：%s %s", r.Method, r.URL.Path)
			return
		}
		w.Write([]byte(`{"campaigns":[{"campaignId":"c9","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE_V2"}]}`))
	})

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if rep.Status != provider.CheckinNoCampaign || !rep.Status.Retryable() {
		t.Fatalf("report = %+v, want no_campaign (retryable)", rep)
	}
	if !strings.Contains(rep.Msg, "CLAIMABLE_V2") {
		t.Errorf("msg = %q, want 带出新枚举原始取值", rep.Msg)
	}
}

// TestMalformedClaimStatusDoesNotClaim claimStatus 形态本身不认识（既不是字符串也不是
// {"status":…}）时：同样不误领，且文案标注「未知 + 原始取值」。
func TestMalformedClaimStatusDoesNotClaim(t *testing.T) {
	withCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != EpCampaigns {
			t.Errorf("形态未知时不得发 claim：%s %s", r.Method, r.URL.Path)
			return
		}
		w.Write([]byte(`{"campaigns":[{"campaignId":"c9","actionType":"CLAIM_BENEFIT","claimStatus":123}]}`))
	})

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if rep.Status != provider.CheckinNoCampaign || !rep.Status.Retryable() {
		t.Fatalf("report = %+v, want no_campaign (retryable)", rep)
	}
	if !strings.Contains(rep.Msg, "未知") || !strings.Contains(rep.Msg, "123") {
		t.Errorf("msg = %q, want 「未知」+ 原始取值", rep.Msg)
	}
}

// TestViewDetailsClaimedDoesNotMaskNoCampaign VIEW_DETAILS 活动的 CLAIMED 不是「今天的签到已领」：
// 列表里没有任何 CLAIM_BENEFIT 时仍必须是 no_campaign（保持窗口重试），不得被误判成已完成。
func TestViewDetailsClaimedDoesNotMaskNoCampaign(t *testing.T) {
	onlyView := `{"showCampaign":true,"claimable":false,"campaigns":[
	  {"campaignId":"c2","campaignKey":"act-20260901-493","actionType":"VIEW_DETAILS",
	   "claimStatus":"CLAIMED"}]}`
	withCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpCampaigns || r.Method != http.MethodGet {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			return
		}
		w.Write([]byte(onlyView))
	})

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if rep.Status != provider.CheckinNoCampaign || !rep.Status.Retryable() {
		t.Fatalf("report = %+v, want no_campaign (retryable)", rep)
	}
}
