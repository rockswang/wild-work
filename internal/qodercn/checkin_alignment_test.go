// checkin_alignment_test.go QoderCN 与 QoderCOM 签到解析层「对齐」的回归测试。
//
// 背景：本仓对 Qoder 三渠道刻意采用「代码级复制、不共享运行时」的约定，
// 于是两区解析层曾经分叉（COM 改了、CN 落后）。本文件把对齐后的三项契约钉死：
//  1. 线上实际形态（扁平字符串 claimStatus）正常工作；
//  2. 对象形态 {"status":…} 作为**防御性**兼容仍被接受（无线上证据）；
//  3. 未知形态绝不误领，且 no_campaign 的文案带上游响应摘要，能区分
//     「上游没发活动」与「本工具解析没懂」。
package qodercn

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// withCNCheckinServer 起假上游并把 checkinHost 指过去（与 qodercn_test.go 同款收尾）。
func withCNCheckinServer(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	old := checkinHost
	checkinHost = srv.URL
	t.Cleanup(func() { checkinHost = old })
}

// TestCNFlatStatusDiagnostics 真实形态（扁平）但无可领活动：no_campaign + 摘要文案。
func TestCNFlatStatusDiagnostics(t *testing.T) {
	withCNCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != EpCampaigns {
			t.Errorf("无可领活动时不得发 claim：%s %s", r.Method, r.URL.Path)
			return
		}
		w.Write([]byte(`{"showCampaign":true,"claimable":false,"campaigns":[
		  {"campaignId":"c2","campaignKey":"act-20260901-493","actionType":"VIEW_DETAILS","claimStatus":"CLAIMED"}]}`))
	})

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("no_campaign 不是 error：%v", err)
	}
	if rep.Status != provider.CheckinNoCampaign || !rep.Status.Retryable() {
		t.Fatalf("report = %+v, want no_campaign (retryable)", rep)
	}
	for _, want := range []string{"showCampaign=true", "claimable=false", "VIEW_DETAILS=CLAIMED"} {
		if !strings.Contains(rep.Msg, want) {
			t.Errorf("诊断文案缺 %q：%s", want, rep.Msg)
		}
	}
}

// TestCNObjectStatusStillAccepted 对象形态仍是防御性兼容（无线上证据，但不得回归成解析失败）。
func TestCNObjectStatusStillAccepted(t *testing.T) {
	var claimed bool
	withCNCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == EpCampaigns && r.Method == http.MethodGet:
			w.Write([]byte(`{"campaigns":[{"campaignId":"c1","actionType":"CLAIM_BENEFIT","claimStatus":{"status":"CLAIMABLE"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/claim"):
			claimed = true
			w.Write([]byte(`{"status":"CLAIMED","replayed":false,"benefit":{"amount":100}}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if !claimed || rep.Status != provider.CheckinClaimed || rep.Amount != 100 {
		t.Fatalf("对象形态兼容性丢失：claimed=%t report=%+v", claimed, rep)
	}
}

// TestCNUnrecognizedEnumDoesNotClaim 未识别的新枚举：不误领、可重试、文案带出原始取值。
func TestCNUnrecognizedEnumDoesNotClaim(t *testing.T) {
	withCNCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
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

// TestCNMalformedClaimStatusDoesNotClaim 形态本身不认识（非字符串/非对象）：
// 不误领，文案标注「未知 + 原始取值」。
func TestCNMalformedClaimStatusDoesNotClaim(t *testing.T) {
	withCNCheckinServer(t, func(w http.ResponseWriter, r *http.Request) {
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
