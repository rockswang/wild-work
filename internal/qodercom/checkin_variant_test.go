// checkin_variant_test.go QoderCOM 签到「列表响应变体」回归测试。
//
// 取证来源（离线，未打上游）：本机 Qoder 桌面端日志
// %APPDATA%\com.qoder.app.stable\logs\<session>\main.log 的
// "[Campaign] 活动状态请求返回" 行，2026-10-17 07:47:46Z 起连续三次
// GET /sash/api/v1/me/campaigns 的 payload 一致为：
//
//	{"showCampaign":true,"claimable":true,
//	 "campaigns":[{"campaignId":"01a130ef-…","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE", …},
//	              {"campaignId":"01a05bce-…","actionType":"VIEW_DETAILS","claimStatus":"CLAIMED"}]}
//
// 同一账号在 07:29/07:59 的自查余额恒为 100。wild-work 的 app.log 在该账号上
// 3537 次签到全部落到 no_campaign，从未出现 claimed —— 说明解析层把「可领取」
// 判成了「无活动」。本文件用上述真实 payload 形状驱动 httptest 假上游，锁定该契约。
package qodercom

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// desktopCampaignsPayload 桌面端 main.log 中裁剪出的真实响应（字段顺序与取值原样保留）。
// 关键点：claim 状态在**嵌套对象** claimStatus:{status:…} 里，不是扁平字符串。
const desktopCampaignsPayload = `{
  "uid": "01a05725-40bb-74e2-8534-858686c422c6",
  "showCampaign": true,
  "claimable": true,
  "campaignUrl": "https://openapi.qoder.sh/growth-page/activity-iframe",
  "campaigns": [
    {
      "campaignId": "01a130ef-2c3d-7ee6-a290-90c3207b536d",
      "campaignKey": "act-20261013-119",
      "actionType": "CLAIM_BENEFIT",
      "startAt": 1792202400,
      "endAt": 1792288740,
      "claimStatus": {"status": "CLAIMABLE"},
      "benefit": {"kind": "CREDITS", "amount": 100,
                  "validity": {"mode": "RELATIVE_DAYS", "days": 30}},
      "placements": [{"type": "POPUP", "campaignUrl": "https://openapi.qoder.sh/growth-page/activity-iframe"}]
    },
    {
      "campaignId": "01a05bce-e800-7494-a002-806e4438f483",
      "campaignKey": "act-20260901-493",
      "actionType": "VIEW_DETAILS",
      "startAt": 1788247200,
      "endAt": 1793462340,
      "claimStatus": {"status": "CLAIMED"},
      "placements": [{"type": "POPUP", "campaignUrl": "https://openapi.qoder.sh/growth-page/activity-iframe"}]
    }
  ]
}`

// desktopClaimOkPayload claim 接口的成功回执（桌面端同族字段形状；benefit 为嵌套对象）。
const desktopClaimOkPayload = `{"campaignId":"01a130ef-2c3d-7ee6-a290-90c3207b536d","status":"CLAIMED","replayed":false,"benefit":{"kind":"CREDITS","amount":100,"validity":{"mode":"RELATIVE_DAYS","days":30}}}`

// withCheckinServer 起一个假上游并把 checkinHost 指过去；handler 收到未知路径即失败。
func withCheckinServer(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := checkinHost
	checkinHost = srv.URL
	t.Cleanup(func() { checkinHost = old })
}

// TestDesktopClaimStatusObjectIsClaimable 桌面端真实 payload（claimStatus 为对象）必须识别为可领取，
// 并真正发出 POST claim → claimed + amount。
// 修复前：扁平 string 反序列化失败被吞，claimStatus 恒为空串 → targetID 为空 → no_campaign。
func TestDesktopClaimStatusObjectIsClaimable(t *testing.T) {
	var claimedPath string
	withCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == EpCampaigns && r.Method == http.MethodGet:
			w.Write([]byte(desktopCampaignsPayload))
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
	if !strings.Contains(claimedPath, "01a130ef-2c3d-7ee6-a290-90c3207b536d") {
		t.Errorf("claimed wrong campaign: %s", claimedPath)
	}
	if rep.Status != provider.CheckinClaimed || rep.Amount != 100 {
		t.Errorf("report = %+v, want claimed +100", rep)
	}
	if rep.Status.Retryable() {
		t.Error("claimed 不得标记可重试")
	}
}

// TestClaimStatusObjectClaimedIsAlready 桌面端 payload 中活动已领（claimStatus:{status:CLAIMED}）时，
// 结果必须是 already_claimed（展示「今日已领取」），而不是 no_campaign。
func TestClaimStatusObjectClaimedIsAlready(t *testing.T) {
	claimedPayload := `{"showCampaign":true,"claimable":false,"campaigns":[
	  {"campaignId":"c1","campaignKey":"act-20261013-119","actionType":"CLAIM_BENEFIT",
	   "claimStatus":{"status":"CLAIMED"},"benefit":{"kind":"CREDITS","amount":100}}]}`
	withCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpCampaigns || r.Method != http.MethodGet {
			t.Errorf("已领取时不得再发 claim：%s %s", r.Method, r.URL.Path)
			return
		}
		w.Write([]byte(claimedPayload))
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

// TestViewDetailsClaimedDoesNotMaskNoCampaign VIEW_DETAILS 活动的 CLAIMED 不是「今天的签到已领」：
// 列表里没有任何 CLAIM_BENEFIT 时仍必须是 no_campaign（保持窗口重试），不得被误判成已完成。
func TestViewDetailsClaimedDoesNotMaskNoCampaign(t *testing.T) {
	onlyView := `{"showCampaign":true,"claimable":false,"campaigns":[
	  {"campaignId":"c2","campaignKey":"act-20260901-493","actionType":"VIEW_DETAILS",
	   "claimStatus":{"status":"CLAIMED"}}]}`
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

// TestClaimBenefitWithoutUsableObject 线上同时观察到 `"claimStatus":"CLAIMABLE"`（扁平）
// 与 `"claimStatus":{…}`（对象）两种形态，两种都必须被接受（向后兼容）。
func TestClaimBenefitFlatStringStillWorks(t *testing.T) {
	var claimed bool
	withCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/claim") {
			claimed = true
			w.Write([]byte(`{"status":"CLAIMED","replayed":false,"benefit":{"amount":100}}`))
			return
		}
		w.Write([]byte(`{"campaigns":[{"campaignId":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE"}]}`))
	})

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if !claimed || rep.Status != provider.CheckinClaimed {
		t.Fatalf("扁平 claimStatus 兼容性丢失：claimed=%t report=%+v", claimed, rep)
	}
}
