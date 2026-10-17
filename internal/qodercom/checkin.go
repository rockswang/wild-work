// checkin.go QoderCOM 每日签到：仅 campaigns 活动路径。
// 代码级复制自 internal/qodercn/checkin.go 后裁剪：
// COM 区无 legacy daily-check-in 系统（实测 GET .../daily-check-in/status 404），
// 故只有 campaigns 链路，无回退分支。
// 参考证据：misc 抓包 sid=3（IDE 国际版同端点，cosy-clienttype=10 + 机器头）。
// 认证：仅需 Bearer dt- + cosy-clienttype:10（桌面端标识），无需 COSY 签名。
// 幂等语义：已签到（409 / CLAIMED / replayed）与无活动均视为成功（返回 nil）。
package qodercom

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

// checkinHost 签到 API 域名（与业务 API 同域 openapi.qoder.sh）。
// var 而非 const：测试用 httptest 覆盖。
var checkinHost = OpenAPIBase

// checkinHeaders 构造桌面端签到请求头（与 qodercn 同款；抓包确认必需头）。
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

// checkin 执行签到：campaigns 活动路径（COM 唯一路径）。
// 返回 (报告, error)：error 非 nil 时调度器可按 isSessionDead 自愈重试。
func checkin(a *auth.Auth) (provider.CheckinReport, error) {
	dt := a.JWT()
	if dt == "" {
		return provider.CheckinReport{Status: provider.CheckinNoToken, Msg: "no dt- available"}, nil
	}
	rep, err := tryCampaigns(dt)
	log.Printf("qodercom checkin campaigns uid=%s status=%s claimed=%t amount=%d msg=%s err=%v",
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
			CampaignID  string           `json:"campaignId"`
			CampaignKey string           `json:"campaignKey"`
			ActionType  string           `json:"actionType"`
			ClaimStatus claimStatusNode  `json:"claimStatus"`
			Benefit     *campaignBenefit `json:"benefit"`
		} `json:"campaigns"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: fmt.Sprintf("campaigns parse: %v", err)}, nil
	}
	var targetID string
	var alreadyAmount int64
	already := false
	for _, cp := range list.Campaigns {
		// 只有 CLAIM_BENEFIT 是签到领积分；VIEW_DETAILS 等其它动作的 CLAIMED 与「今日已领」无关，
		// 不能用来把 no_campaign 掩成已领取（否则当天漏领）。
		if cp.ActionType != "CLAIM_BENEFIT" {
			continue
		}
		switch cp.ClaimStatus.mode() {
		case "CLAIMABLE":
			targetID = cp.CampaignID
		case "CLAIMED":
			already = true
			if cp.Benefit != nil {
				alreadyAmount = cp.Benefit.Amount
			}
		}
	}
	if targetID == "" {
		if already {
			// 已领取时活动里只有 CLAIMED 记录，本次不会再拿到 claim 回执，
			// 金额只能取活动自身的 benefit.amount（桌面端已领后仍会保留该字段）。
			return provider.CheckinReport{Status: provider.CheckinAlready, Amount: alreadyAmount, Msg: alreadyMsg(alreadyAmount)}, nil
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
		return provider.CheckinReport{Status: provider.CheckinAlready, Msg: alreadyMsg(0)}, nil
	}
	if status == http.StatusUnauthorized {
		return provider.CheckinReport{Status: provider.CheckinError}, sessionDead(status, raw)
	}
	if status != http.StatusOK {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: fmt.Sprintf("领取失败 http %d: %s", status, truncate(string(raw), 150))}, nil
	}
	var claim struct {
		Status   string           `json:"status"`
		Replayed bool             `json:"replayed"`
		Benefit  *campaignBenefit `json:"benefit"`
	}
	if err := json.Unmarshal(raw, &claim); err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: fmt.Sprintf("claim parse: %v", err)}, nil
	}
	if claim.Status != "CLAIMED" {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: fmt.Sprintf("未知状态 %s", claim.Status)}, nil
	}
	if claim.Replayed {
		return provider.CheckinReport{Status: provider.CheckinAlready, Msg: alreadyMsg(claim.Benefit.amount())}, nil
	}
	amount := claim.Benefit.amount()
	return provider.CheckinReport{Status: provider.CheckinClaimed, Amount: amount, Msg: fmt.Sprintf("签到成功 +%d", amount)}, nil
}

// campaignBenefit 活动/回执里的权益描述（签到只有 CREDITS 一种，故只关心金额）。
type campaignBenefit struct {
	Kind   string `json:"kind"`
	Amount int64  `json:"amount"`
}

// amount 空指针安全地取金额：活动未配置 benefit 或上游未回填时为 0。
func (b *campaignBenefit) amount() int64 {
	if b == nil {
		return 0
	}
	return b.Amount
}

// claimStatusNode 兼容 claimStatus 的两种线上形态：
//
//	"CLAIMABLE"              扁平字符串（早期抓包/本地 qoder2api 采样）
//	{"status":"CLAIMABLE"}   嵌套对象（2026-10-17 桌面端 0.4.8 实测）
//
// 之前按 string 反序列化：对象形态会让**整个列表**解析失败（campaigns parse: …），
// 而 wild-work 的日志只看到 no_campaign，桌面端却显示「可领取」——两者从此对不上。
// 这里自行解码，两种形态都接受，未知形态降级为空串（= 不可领取，不误领）。
type claimStatusNode struct {
	state string // 归一化后的状态串
	raw   string // 原始取值（日志排查用）
}

func (n *claimStatusNode) UnmarshalJSON(b []byte) error {
	n.raw = string(b)
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		n.state = s
		return nil
	}
	var obj struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(b, &obj); err == nil {
		n.state = obj.Status
		return nil
	}
	n.state = "" // 未知形态：当作不可领取，让上层按 no_campaign/已领判定
	return nil
}

// mode 返回归一化状态（大写、去空白）；未知形态返回空串，调用方按「不可领取」处理。
func (n claimStatusNode) mode() string { return strings.ToUpper(strings.TrimSpace(n.state)) }

// alreadyMsg 已领取的展示文案：有金额时带上，避免面板显示「今日已领取」却不知领了多少。
func alreadyMsg(amount int64) string {
	if amount > 0 {
		return fmt.Sprintf("今日已领取 +%d", amount)
	}
	return "今日已领取"
}

// sessionDead 构造类型化的 401 错误：调度器用 errors.As(*provider.Error) 判定自愈，
// 裸 fmt.Errorf 会让 isSessionDead 恒为 false（AGENTS.md 不变式 19）。
func sessionDead(status int, raw []byte) error {
	return &provider.Error{Kind: provider.ErrSessionDead, Status: status, Msg: truncate(string(raw), 200)}
}
