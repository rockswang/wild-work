// checkin.go 豆包「每日签到」（工作模式活动）。
//
// 端点：POST /alice/ug/campaign/landing/checkin
// 报文：{"scene":"pc_incentive_task_mvp_v3"}
//
// 三态判定（与 Qoder 的结构化上报同思路，R23）：
//   - claimed：本次真领到（checked_in=true 且非幂等命中）
//   - already：今日已签到（idempotent=true）—— **不是** no_campaign，
//     判成可重试会让调度器整天重敲；
//   - no_campaign：710010702 账号没有可用活动（窗口内可重试）；
//   - error：其它（含 401 → 类型化 ErrSessionDead 供上层自愈，不变量 19）。
package doubao

import (
	"bytes"
	"encoding/json"
	"net/http"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// checkinResp 签到响应。
type checkinResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Msg     string `json:"msg"`
	Data    struct {
		CheckedIn      bool   `json:"checked_in"`
		CheckinDate    string `json:"checkin_date"`
		ContinuousDays int    `json:"continuous_days"`
		Idempotent     bool   `json:"idempotent"`
		RewardAmount   int    `json:"reward_amount"`
		RewardType     string `json:"reward_type"`
	} `json:"data"`
}

// DailyCheckin 实现 provider.Upstream：触发当日签到。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	rep, err := c.DailyCheckinReport(a)
	if err != nil {
		return err
	}
	if rep.Status == provider.CheckinError {
		return &provider.Error{Kind: provider.ErrClient, Msg: "doubao 签到未达成: " + rep.Msg}
	}
	return nil
}

// DailyCheckinReport 实现 provider.CheckinReporter：结构化上报签到结果。
func (c *Client) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	cookie, dev, webID, fp := devOf(a)
	if cookie == "" {
		return provider.CheckinReport{Status: provider.CheckinNoToken, Msg: "doubao：账号没有 Cookie，请重新扫码登录"}, nil
	}
	target, err := c.identityURLWith(CheckinPath, checkinQuery, cookie, dev, webID, fp)
	if err != nil {
		return provider.CheckinReport{}, err
	}
	// biz_params 必须是**对象**（参考实现记载：传字符串会 400）。
	body, _ := json.Marshal(map[string]any{"scene": CheckinScene, "biz_params": map[string]any{}})
	resp, raw, err := c.do(http.MethodPost, target, cookie, body)
	if err != nil {
		return provider.CheckinReport{}, err
	}

	// 401 → 类型化会话失效（调度器走 refresh 自愈；本渠道无 refresh，转面板重登）。
	if resp.StatusCode == http.StatusUnauthorized {
		return provider.CheckinReport{}, &provider.Error{Kind: provider.ErrSessionDead, Status: resp.StatusCode, Msg: "doubao 签到 401: " + truncateStr(string(raw), 200)}
	}
	var out checkinResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: "doubao 签到响应解析失败: " + truncateStr(string(raw), 200)}, nil
	}

	switch {
	case resp.StatusCode == http.StatusOK && out.Code == 0 && out.Data.Idempotent:
		return provider.CheckinReport{Status: provider.CheckinAlready, Amount: int64(out.Data.RewardAmount), Msg: checkinMsg(out)}, nil
	case resp.StatusCode == http.StatusOK && out.Code == 0 && out.Data.CheckedIn:
		return provider.CheckinReport{Status: provider.CheckinClaimed, Amount: int64(out.Data.RewardAmount), Msg: checkinMsg(out)}, nil
	case out.Code == CheckinNoCampaign:
		return provider.CheckinReport{Status: provider.CheckinNoCampaign, Msg: "当前无可用签到活动"}, nil
	default:
		msg := out.Message + out.Msg
		if msg == "" {
			msg = truncateStr(string(raw), 200)
		}
		kind := provider.ErrClient
		if resp.StatusCode == http.StatusUnauthorized {
			kind = provider.ErrSessionDead
		}
		return provider.CheckinReport{Status: provider.CheckinError, Msg: msg}, &provider.Error{Kind: kind, Status: resp.StatusCode, Msg: msg}
	}
}

// checkinMsg 组装人类可读的签到结果文案。
func checkinMsg(r checkinResp) string {
	msg := "签到成功"
	if r.Data.Idempotent {
		msg = "今日已签到"
	}
	if r.Data.RewardAmount > 0 {
		msg += " +" + itoa(int64(r.Data.RewardAmount))
	}
	if r.Data.ContinuousDays > 0 {
		msg += "（连签 " + itoa(int64(r.Data.ContinuousDays)) + " 天）"
	}
	return msg
}

var _ = bytes.MinRead
