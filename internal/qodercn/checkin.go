// checkin.go QoderCN 每日签到：仅 campaigns 活动路径（对齐上游 qoder2api 99ab022）。
//   - GET  /sash/api/v1/me/campaigns                  → 找 CLAIMABLE 的 CLAIM_BENEFIT
//   - POST /sash/api/v1/me/campaigns/{campaignId}/claim → 领取（空 body）
//
// 2026-09-21 实测（本项目抓包 + 上游 qoder2api 同款结论）：legacy daily-check-in
// 系统已全局 DISABLED，但其 claim 端点对未领取日也恒返回 409，会被误判成
// 「今日已领取」而跳过真实领取 → 假成功、零积分。故不再走 legacy claim；
// status 端点仅作只读统计（上游 99ab022 亦如此，保留备用）。
// 签到认证：仅需 Bearer dt- + cosy-clienttype:10（桌面端标识），无需 COSY 签名。
// 幂等语义：已签到（409 / CLAIMED / replayed）与无活动均视为成功（返回 nil）。
package qodercn

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// checkinHost 签到 API 域名（抓包确认 openapi.qoder.com.cn，与业务 API 同域）。
// var 而非 const：测试用 httptest 覆盖。
var checkinHost = OpenAPIBase

// checkinHeaders 构造桌面端签到请求头（抓包确认的必需头）。
// ★ cosy-clienttype 必须为 10（桌面端标识），推理链路用的是 5，两者不可混用。
func checkinHeaders(req *http.Request, dt string) {
	req.Header.Set("authorization", "Bearer "+dt)
	req.Header.Set("accept", "application/json")
	req.Header.Set("accept-language", "zh-CN")
	req.Header.Set("user-agent", "Qoder")
	req.Header.Set("cosy-clienttype", "10")
}

// doCheckinRequest 发送签到请求，返回 (httpStatus, rawBody)。
// reqBody 为 nil 时发送空 body（抓包确认 campaigns/claim 即空 body）。
func doCheckinRequest(method, path, dt string, reqBody any) (int, []byte, error) {
	var body io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return 0, nil, err
		}
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, checkinHost+path, body)
	if err != nil {
		return 0, nil, err
	}
	checkinHeaders(req, dt)
	if method == http.MethodPost {
		req.Header.Set("origin", checkinHost)
		if reqBody == nil {
			req.ContentLength = 0
		}
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// checkin 执行签到：campaigns 活动路径（唯一领取路径）。
// 返回 (报告, error)：error 非 nil 时调度器可按 isSessionDead 自愈重试；
// 报告状态供调度器判定当日是否完成、是否需要在窗口内重试。
func checkin(a *auth.Auth) (provider.CheckinReport, error) {
	dt := a.JWT()
	if dt == "" {
		return provider.CheckinReport{Status: provider.CheckinNoToken, Msg: "no dt- available"}, nil
	}
	rep, err := tryCampaigns(dt)
	log.Printf("qodercn checkin campaigns uid=%s status=%s claimed=%t amount=%d msg=%s err=%v",
		a.UID, rep.Status, rep.Status == provider.CheckinClaimed, rep.Amount, rep.Msg, err)
	return rep, err
}

// tryCampaigns 查活动列表 → 领取 CLAIMABLE 的 CLAIM_BENEFIT。
func tryCampaigns(dt string) (provider.CheckinReport, error) {
	status, raw, err := doCheckinRequest(http.MethodGet, EpCampaigns, dt, nil)
	if err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: err.Error()}, err
	}
	// 401 必须按 session dead 类型透传，否则调度器无法自愈（AGENTS.md 不变式 19）。
	if status == http.StatusUnauthorized {
		return provider.CheckinReport{Status: provider.CheckinError}, sessionDead(status, raw)
	}
	if status != http.StatusOK {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: fmt.Sprintf("campaigns http %d: %s", status, truncate(string(raw), 200))}, nil
	}
	var list struct {
		Campaigns []struct {
			CampaignID  string `json:"campaignId"`
			CampaignKey string `json:"campaignKey"`
			ActionType  string `json:"actionType"`
			ClaimStatus string `json:"claimStatus"`
		} `json:"campaigns"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: fmt.Sprintf("campaigns parse: %v", err)}, nil
	}
	var targetID string
	already := false
	for _, cp := range list.Campaigns {
		if cp.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		switch cp.ClaimStatus {
		case "CLAIMABLE":
			targetID = cp.CampaignID
		case "CLAIMED":
			already = true
		}
	}
	if targetID == "" {
		if already {
			return provider.CheckinReport{Status: provider.CheckinAlready, Msg: "今日已领取"}, nil
		}
		// 活动尚未创建（10:00 整点延迟）→ 可重试状态，不可当作完成。
		return provider.CheckinReport{Status: provider.CheckinNoCampaign, Msg: "无可用签到活动"}, nil
	}

	// 领取（空 body，抓包确认）
	status, raw, err = doCheckinRequest(http.MethodPost, EpCampaigns+"/"+targetID+"/claim", dt, nil)
	if err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: err.Error()}, err
	}
	if status == http.StatusConflict { // 409 幂等：今日已领
		return provider.CheckinReport{Status: provider.CheckinAlready, Msg: "今日已领取"}, nil
	}
	if status == http.StatusUnauthorized {
		return provider.CheckinReport{Status: provider.CheckinError}, sessionDead(status, raw)
	}
	if status != http.StatusOK {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: fmt.Sprintf("领取失败 http %d: %s", status, truncate(string(raw), 150))}, nil
	}
	var claim struct {
		Status   string `json:"status"`
		Replayed bool   `json:"replayed"`
		Benefit  *struct {
			Amount int64 `json:"amount"`
		} `json:"benefit"`
	}
	if err := json.Unmarshal(raw, &claim); err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: fmt.Sprintf("claim parse: %v", err)}, nil
	}
	if claim.Status != "CLAIMED" {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: fmt.Sprintf("未知状态 %s", claim.Status)}, nil
	}
	if claim.Replayed {
		return provider.CheckinReport{Status: provider.CheckinAlready, Msg: "今日已领取"}, nil
	}
	amount := int64(0)
	if claim.Benefit != nil {
		amount = claim.Benefit.Amount
	}
	return provider.CheckinReport{Status: provider.CheckinClaimed, Amount: amount, Msg: fmt.Sprintf("签到成功 +%d", amount)}, nil
}

// sessionDead 构造类型化的 401 错误：调度器用 errors.As(*provider.Error) 判定自愈，
// 裸 fmt.Errorf 会让 isSessionDead 恒为 false（AGENTS.md 不变式 19）。
func sessionDead(status int, raw []byte) error {
	return &provider.Error{Kind: provider.ErrSessionDead, Status: status, Msg: truncate(string(raw), 200)}
}
