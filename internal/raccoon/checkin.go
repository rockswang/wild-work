// checkin.go 小浣熊「每日积分」签到（2026-10-15 实测接入）。
//
// 上游没有独立的「签到/领取」端点（`daily_grant` / `checkin` / `claim` 等候选全 404），
// 每日 300 积分的发放是**惰性**的：
//
//	名义记账时间 = 当天 00:00（bills 的 grant_at）
//	实际落账时间 = 账号当天第一次访问「工作台业务接口」的时刻（bills 的 created_at）
//
// 网页端文案原文：「每日积分每天 00:00 发放，默认当日 24:00 清零，当天登录后有效期延长至 7 天。」
//
// 实测（2026-10-15，三账号对照）：
//
//	GET /api/web/office/v3/setting_info  → 三账号各入账 300（point_grant_popups 非空）
//	POST /api/web/llm/v2/chat/completions → 只扣「桌面端 LLM 对话消耗 -2」，不入账
//	GET /api/web/points/v1/balance|bills  → 只读，不入账
//	GET /api/web/auth/v1/user_info        → 不入账
//	POST /api/web/auth/v1/iam/confirm     → 不入账（HTTP 200 但无发放）
//
// 幂等：同一账号当天重复调用 setting_info 不重复发放（第二次 popups 为空数组）。
// 故本实现 ≈ 替用户「打开一次网页」，与网页端行为同构，不是跨端伪装
// （对照 AGENTS R31 的判据：网页端页面加载时自己就会调用的接口 = 安全可调）。
package raccoon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// settingInfoResp /api/web/office/v3/setting_info 的响应（2026-10-15 实测）。
//
// 只解析签到关心的两个字段；其余（空间配额/功能开关/ab_tests 等）与本渠道无关，
// 不建模——上游增删这些字段不应影响签到（同 AGENTS R55 的「爆炸半径」哲学）。
type settingInfoResp struct {
	Code int `json:"code"`
	Data struct {
		AvailablePoints int64 `json:"available_points"`
		// PointGrantPopups 本次调用**刚刚触发**的发放提示列表。
		// 非空 ⇔ 本次调用触发了新发放；空 ⇔ 今天已经领过（幂等语义的判据）。
		PointGrantPopups []struct {
			Source      string `json:"source"`       // daily / monthly / old_user / new_user / desktop_login
			DisplayName string `json:"display_name"` // 中文展示名，如「每日积分发放」
			Points      int64  `json:"points"`
			ExpireAt    string `json:"expire_at"`
		} `json:"point_grant_popups"`
	} `json:"data"`
}

// FetchSettingInfo 拉取工作台设置（副作用：触发当日每日积分落账，幂等）。
//
// 这是本渠道「签到」的**实质动作**——上游没有独立领取端点，发放靠这次访问触发。
// 返回 (可用余额, 本次发放的积分列表)。
func (c *Client) FetchSettingInfo(a *auth.Auth) (int64, []GrantPopup, error) {
	resp, raw, err := c.do(http.MethodGet, c.authBaseURL()+EpOfficeSettingInfo, a, nil)
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, nil, &provider.Error{
			Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncate(string(raw), 300),
		}
	}
	var out settingInfoResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, nil, fmt.Errorf("raccoon setting_info 响应解析失败: %w (%s)", err, truncate(string(raw), 200))
	}
	if out.Code != 0 {
		// 业务码非 0 视为失败（与 points 服务同口径：只有 HTTP 状态码能判会话失效，
		// 业务码多为参数类错误，见 UserResourceDetail 的注释）。
		kind := provider.ErrClient
		if resp.StatusCode == http.StatusUnauthorized {
			kind = provider.ErrSessionDead
		}
		return 0, nil, &provider.Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 300)}
	}
	popups := make([]GrantPopup, 0, len(out.Data.PointGrantPopups))
	for _, p := range out.Data.PointGrantPopups {
		popups = append(popups, GrantPopup{
			Source: p.Source, DisplayName: p.DisplayName, Points: p.Points, ExpireAt: p.ExpireAt,
		})
	}
	return out.Data.AvailablePoints, popups, nil
}

// GrantPopup 一次发放提示（setting_info 的 point_grant_popups 条目）。
type GrantPopup struct {
	Source      string `json:"source"`
	DisplayName string `json:"display_name"`
	Points      int64  `json:"points"`
	ExpireAt    string `json:"expire_at"`
}

// DailyCheckin 实现 provider.Upstream：走 setting_info 触发当日积分落账。
//
// 语义与 CheckinReporter 一致（claimed/already 视为成功），供未走结构化通道的
// 调用方（如旧 API）使用；调度器优先用 DailyCheckinReport。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	rep, err := c.DailyCheckinReport(a)
	if err != nil {
		return err
	}
	if rep.Status.Retryable() {
		return fmt.Errorf("raccoon 签到未达成: %s", rep.Msg)
	}
	return nil
}

// DailyCheckinReport 实现 provider.CheckinReporter：结构化上报签到结果。
//
// 状态映射（本渠道的语义）：
//   - point_grant_popups 含 source=daily 且 points>0 → CheckinClaimed（本次真正领到）
//   - popups 为空（今天已领过，幂等）              → CheckinAlready（当日完成）
//   - 401 会话失效                                → 返回 *provider.Error{ErrSessionDead}
//     （调度器据此走「refresh + 重试」，见 AGENTS 不变量 19）
//   - 其它 HTTP 错误                              → CheckinError（可重试）
//
// 为什么用 popups 而非余额差分判定：余额会同时受消耗影响（同期可能有对话扣费），
// 而 popups 是「本次调用刚触发的发放」的**直接回执**，语义无歧义。
//
// 日志行形状与 qoder 系结构化行同构（`<p> checkin <action> uid=… status=… claimed=…
// amount=… msg=… err=…`）——stats 的签到收入解析器按该形状取「实发金额」入账，
// 故这里的字段顺序/命名不得随意改动（改则面板「今日签到收入」静默归零）。
func (c *Client) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	remain, popups, err := c.FetchSettingInfo(a)
	if err != nil {
		rep := provider.CheckinReport{Status: provider.CheckinError, Msg: truncate(err.Error(), 120)}
		var pe *provider.Error
		if asProviderError(err, &pe) && pe.Kind == provider.ErrSessionDead {
			rep = provider.CheckinReport{Status: provider.CheckinNoToken, Msg: "登录态失效，待刷新后重试"}
		}
		log.Printf("raccoon checkin setting_info uid=%s status=%s claimed=false amount=0 msg=%s err=%v",
			a.UID, rep.Status, rep.Msg, err)
		return rep, err
	}
	var granted int64
	var names []string
	for _, p := range popups {
		granted += p.Points
		if p.DisplayName != "" {
			names = append(names, p.DisplayName)
		}
	}
	rep := provider.CheckinReport{Status: provider.CheckinAlready, Msg: "今日已领取"}
	if granted > 0 {
		// 注意：不要把它判成「无活动」——本渠道的发放是惰性的，空 popups 即代表已完成。
		msg := fmt.Sprintf("签到成功 +%d", granted)
		if len(names) > 0 {
			msg = fmt.Sprintf("签到成功 +%d（%s）", granted, names[0])
		}
		rep = provider.CheckinReport{Status: provider.CheckinClaimed, Amount: granted, Msg: msg}
	}
	log.Printf("raccoon checkin setting_info uid=%s status=%s claimed=%t amount=%d msg=%s err=%v remain=%d",
		a.UID, rep.Status, rep.Status == provider.CheckinClaimed, granted, rep.Msg, nil, remain)
	return rep, nil
}

// asProviderError 用 errors.As 取出 *provider.Error（与 glm 包同名助手一致）。
func asProviderError(err error, target **provider.Error) bool {
	return errors.As(err, target)
}
