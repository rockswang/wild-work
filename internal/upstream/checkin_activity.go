// checkin_activity.go WorkBuddy 国内版「签到活动」结构化上报（issue #147）。
//
// 背景：国内版原先只实现 CheckinGranter（直接 POST daily-checkin）。这在
// 「今天已在别处签过」时得到 HTTP 400 + {"code":10001,"msg":"今天已签到，请明天再来"}，
// 经 doJSONBilling → doJSONWith 变成 *Error{Kind:ErrClient}。调度器靠 isAlready()
// 文本匹配兜底（含「已签到」），所以**最终结果**是对的，但有三个实际缺陷：
//
//  1. 面板只能显示一条 error 文案，看不出「活动是否开启 / 连续多少天 / 本周进度」；
//  2. 无法区分「已签到（完成）」与「活动未开启（可重试）」——两者都是非 0 业务码，
//     文本匹配一旦上游改文案就会退化成「签到失败」；
//  3. 上游确有 `checkin-activity-status` 端点（2026-10-17 实测，见下），
//     一次只读请求就能拿到 active / today_checked_in / streak_days / 活动期，
//     比「先写后猜」更安全（写操作只在确认未签时才发）。
//
// 实测回执（2026-10-17，Smurf7788，全程只读）：
//
//	POST /v2/billing/meter/checkin-activity-status {}
//	{"code":0,"data":{"active":true,"activity_name":"Buddy应用","theme_name":"Buddy加油站",
//	 "season":10,"start_time":"2026-09-30 00:00:00","end_time":"2026-10-31 23:59:59",
//	 "daily_credit":100,"today_credit":100,"total_credits":1800,
//	 "today_checked_in":true,"streak_days":18,"week_checkin_days":6,
//	 "is_streak_day":false,"streak_bonus_days":0,"streak_bonus_credit":0,
//	 "claim_button_text":"立即领取","checkin_dates":[...]}}
//
// ⚠️ 与 issue #147 的关系（如实标注，勿把它写成实测）：
// 该 issue 问「10-22 过期可领补偿 500 积分，签到能领吗」。上述回执里**没有任何**
// compensation/bonus 类字段（streak_bonus_credit=0），积分明细里也没有 500 的补偿条目
// ——只有「CodeBuddy个人体验版 500」（周期 10-01~10-31，与 10-22 无关）与
// 每天 100 的签到裂变包。故**结论是：补偿不经过签到端点**，本文件不做补偿领取
// （那需要另一个端点，且未经实测，按 AGENTS.md §0.1 不去穷举探测）。
// 本文件的收益是把签到本身从「写后猜文本」升级为「先只读探测再按需写」。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// checkinActivityPath 签到活动状态端点（国内版与国际版同路径，实测 2026-10-17）。
const checkinActivityPath = "/v2/billing/meter/checkin-activity-status"

// checkinActivity checkin-activity-status 的响应体（只取用到的字段）。
type checkinActivity struct {
	Active          bool   `json:"active"`
	TodayCheckedIn  bool   `json:"today_checked_in"`
	DailyCredit     int64  `json:"daily_credit"`
	TodayCredit     int64  `json:"today_credit"`
	TotalCredits    int64  `json:"total_credits"`
	StreakDays      int64  `json:"streak_days"`
	WeekCheckinDays int64  `json:"week_checkin_days"`
	ActivityName    string `json:"activity_name"`
	ThemeName       string `json:"theme_name"`
	StartTime       string `json:"start_time"`
	EndTime         string `json:"end_time"`
}

// fetchCheckinActivity 只读查询签到活动状态。
// 失败返回 error（调用方回退到「直接领取 + 文本判定」的旧路径）。
func (c *Client) fetchCheckinActivity(a *auth.Auth) (*checkinActivity, error) {
	url := c.billingBase(a) + checkinActivityPath
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	BillingHeaders(req, a)
	data, err := c.doJSONBilling(req)
	if err != nil {
		return nil, err
	}
	var act checkinActivity
	if err := json.Unmarshal(data, &act); err != nil {
		return nil, fmt.Errorf("activity parse: %w", err)
	}
	return &act, nil
}

// DailyCheckinReport 实现 provider.CheckinReporter：先只读探测活动状态，再决定是否领取。
//
// 相比旧的「直接 POST daily-checkin」：
//   - 已签到 → CheckinAlready（不再发写请求，避免必然 400 的噪音日志）；
//   - 活动未开启 → CheckinNoCampaign（可重试，语义正确）；
//   - 未签到且活动开启 → 调领取端点，成功 CheckinClaimed（带金额），失败 CheckinError。
//
// 探测失败时**降级**为旧的直接领取路径（保持向后兼容：上游若撤掉该端点，签到不会失效）。
func (c *Client) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	act, err := c.fetchCheckinActivity(a)
	if err != nil {
		log.Printf("workbuddy checkin activity probe failed uid=%s err=%v（降级为直接领取）", a.UID, err)
		return c.checkinDirect(a)
	}
	if !act.Active {
		log.Printf("workbuddy checkin activity inactive uid=%s name=%s", a.UID, act.ActivityName)
		return provider.CheckinReport{
			Status: provider.CheckinNoCampaign,
			Msg:    "签到活动未开启（" + act.ActivityName + "）",
		}, nil
	}
	if act.TodayCheckedIn {
		// 已签到：不再发写请求。带上面板可读的进度信息（连续天数/本周进度）。
		msg := fmt.Sprintf("已签到（连续 %d 天，本周 %d 天）", act.StreakDays, act.WeekCheckinDays)
		log.Printf("workbuddy checkin already uid=%s streak=%d week=%d", a.UID, act.StreakDays, act.WeekCheckinDays)
		return provider.CheckinReport{Status: provider.CheckinAlready, Msg: msg}, nil
	}
	// 未签到且活动开启：此时才发写请求。
	granted, gerr := c.DailyCheckinGrant(a)
	if gerr != nil {
		// 领取失败：若错误文本是「已签到」形态（并发场景：别处刚签过），仍按已完成上报。
		if isAlreadyCheckedIn(gerr) {
			return provider.CheckinReport{Status: provider.CheckinAlready, Msg: "已签到"}, nil
		}
		return provider.CheckinReport{Status: provider.CheckinError, Msg: "签到失败：" + truncate(gerr.Error(), 120)}, gerr
	}
	amount := granted
	if amount <= 0 {
		amount = act.DailyCredit // 回执未回填时用活动声明的日额兜底
	}
	return provider.CheckinReport{
		Status: provider.CheckinClaimed,
		Msg:    fmt.Sprintf("签到成功 +%d（连续 %d 天）", amount, act.StreakDays+1),
		Amount: amount,
	}, nil
}

// checkinDirect 旧路径：直接 POST daily-checkin，不做活动探测。
// 仅在活动探测失败时作为降级使用。
func (c *Client) checkinDirect(a *auth.Auth) (provider.CheckinReport, error) {
	granted, err := c.DailyCheckinGrant(a)
	if err != nil {
		if isAlreadyCheckedIn(err) {
			return provider.CheckinReport{Status: provider.CheckinAlready, Msg: "已签到"}, nil
		}
		return provider.CheckinReport{Status: provider.CheckinError, Msg: "签到失败：" + truncate(err.Error(), 120)}, err
	}
	return provider.CheckinReport{
		Status: provider.CheckinClaimed,
		Msg:    fmt.Sprintf("签到成功 +%d", granted),
		Amount: granted,
	}, nil
}

// isAlreadyCheckedIn 判定错误是否为「今天已签到」形态。
// 上游实测文案：「今天已签到，请明天再来」（code=10001）；保留英文变体兜底。
func isAlreadyCheckedIn(err error) bool {
	s := err.Error()
	for _, m := range []string{"今天已签到", "已签到", "already check", "already check"} {
		if containsFold(s, m) {
			return true
		}
	}
	return false
}

// containsFold ASCII 大小写无关的子串判定（中文无需折叠）。
func containsFold(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	// 简单实现：ASCII 小写化后比较（中文不受影响）。
	ls, lsub := toLowerASCII(s), toLowerASCII(sub)
	for i := 0; i+len(lsub) <= len(ls); i++ {
		if ls[i:i+len(lsub)] == lsub {
			return true
		}
	}
	return false
}

func toLowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}
