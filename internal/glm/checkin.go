// checkin.go 智谱清言签到（activity-api 族）。
//
// 端点（2026-09-26 实测存在，同前缀错误路径返回 404 可证明非网关通配）：
//
//	GET  /chatglm/activity-api/activity/daily/check_in_info   签到状态
//	POST /chatglm/activity-api/activity/daily/check_in        执行签到
//	GET  /chatglm/activity-api/activity/daily/prize_list      奖品列表
//	POST /chatglm/activity-api/activity/daily/draw_prize      抽奖
//
// 重要机制：签到与**对话联动**。签到页文案有「对话后记得回来领取打卡进度」
// 「今日已对话」「手动打卡」——即当日需先与清言对话，才能领取打卡进度。
// 故本渠道的保活与签到是同一个流程：先发一条最小对话，再签到（见 Keepalive）。
package glm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// CheckinInfo 签到状态。
type CheckinInfo struct {
	// TodayStatus 今日签到状态：0 = 未签，非 0 = 已签。
	TodayStatus int
	// TotalDays 累计打卡天数。
	TotalDays int
	// PrizeDays / NeedDays 抽奖进度（累计天数 / 所需天数）。
	PrizeDays int
	NeedDays  int
	// PrizeChance 可抽奖次数。
	PrizeChance int
	// List 打卡明细列表（日期 → 状态）。
	List []CheckinDay
}

// CheckinDay 单个打卡日。
type CheckinDay struct {
	Date   string `json:"date"`
	Status int    `json:"status"`
}

// FetchCheckinInfo 查询签到状态。
func (c *Client) FetchCheckinInfo(a *auth.Auth) (CheckinInfo, error) {
	token, err := c.acquireToken(context.Background(), a)
	if err != nil {
		return CheckinInfo{}, err
	}
	path := EpCheckinInfo + "?event_date=" + eventDate(time.Now())
	raw, err := c.doJSON(context.Background(), http.MethodGet, path, token, nil)
	if err != nil {
		return CheckinInfo{}, err
	}
	var r struct {
		CheckInList        []CheckinDay `json:"check_in_list"`
		CheckInTodayStatus int          `json:"check_in_today_status"`
		CheckInTotalDays   int          `json:"check_in_total_days"`
		PrizeDays          int          `json:"prize_days"`
		NeedDays           int          `json:"need_days"`
		PrizeChance        int          `json:"prize_chance"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return CheckinInfo{}, err
	}
	return CheckinInfo{
		TodayStatus: r.CheckInTodayStatus,
		TotalDays:   r.CheckInTotalDays,
		PrizeDays:   r.PrizeDays,
		NeedDays:    r.NeedDays,
		PrizeChance: r.PrizeChance,
		List:        r.CheckInList,
	}, nil
}

// DailyCheckin 执行一次签到。实现 provider.Upstream。
//
// 返回 nil 表示「已签或本次签成」；返回 error 表示需要重试或上报失败。
// 语义与 CheckinReporter 对齐：只有明确成功/已签才算完成。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := c.DailyCheckinReport(a)
	return err
}

// DailyCheckinReport 结构化上报签到结果，供调度器判定「当日是否完成」。
//
// 流程（签到与对话联动，故合并为一步）：
//
//	① 先发一条最小对话保活（当日需有对话才能领打卡进度）
//	② 查签到状态，已签则直接完成
//	③ 执行签到
//	④ 复查状态确认真的签上（避免「接口 200 但实际没签」的假成功）
//
// 机制说明（2026-09-26 以真实账号核实，与初版假设不同）：
//
//	清言**没有「签到领取」接口**。积分规则实测为：
//	    score_rule = "免费用户，登录赠送200积分/天"
//	即积分由**服务端按天被动发放**，用户无需（也无法）主动领取。
//
//	App 里的「做任务赚积分」（截图）是**任务激励**，其中每日任务为
//	「与伙伴对话」「与群聊对话」——**发一条消息即算完成**，同样没有领取步骤。
//
// 故本渠道的「签到」语义 = **保活对话**（既满足每日任务，又维持账号活跃）。
// 原 activity-api 签到活动（/activity/daily/*）已下线（服务端返回「活动已结束」），
// 保留其调用作为**尽力而为**的额外收益，但**其结果不再决定成败**——
// 因为积分发放与它无关，把它当失败上报只会让面板显示无意义的红字。
//
// 状态映射（对齐 provider.CheckinStatus 语义）：
//   - 保活对话成功         → CheckinClaimed（当日完成）
//   - 会话失效（40102）    → CheckinNoToken（可重试：refresh 后可能恢复）
//   - 对话本身失败         → CheckinError（可重试）
func (c *Client) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	// ① 保活对话：这是本渠道「签到」的**实质动作**
	if err := c.keepaliveChat(a); err != nil {
		// 对话失败才是真失败（账号可能失效）
		return classifyCheckinErr(err)
	}

	// ② 领取每日登录积分（`daily_login_score`）。
	//
	// 这是**真实存在**的领取接口（2026-09-26 从 Web 主包挖出），
	// 网页版打开时自己就会调它（带 errorMessageShow:false 静默失败）。
	//
	// 为什么值得调：`score_rule` 写「免费用户，登录赠送200积分/天」，
	// 而实测该接口返回「今日已领取」/ 成功。**幂等**（重复调用不报错、不重复发放），
	// 故无论上游是「自动发放」还是「需主动调用」，调它都是安全且有益的：
	//   - 若自动发放 → 返回「今日已领取」，无害
	//   - 若需主动调 → 完成领取，这正是我们要的
	//
	// 与旧 activity-api 签到不同：**这个接口是在线的、真实的每日积分来源**，
	// 故它的失败要如实反映（不静默），但不阻断整体成功。
	dailyNote := c.claimDailyLoginScore(a)

	// ③ 读积分余额作为结果展示（清言唯一的积分来源）
	msg := "保活对话完成"
	if info, err := c.FetchMemberInfo(a); err == nil {
		msg = "保活对话完成，当前积分 " + itoa64(info.Score)
		if info.ScoreRule != "" {
			msg += "（" + info.ScoreRule + "）"
		}
	}
	if dailyNote != "" {
		msg += "；" + dailyNote
	}

	// ④ 尽力而为：若旧的每日签到活动仍在线，顺手试一次（纯额外收益，失败不报错）
	if extra := c.tryLegacyCheckin(a); extra != "" {
		msg += "；" + extra
	}

	return provider.CheckinReport{
		Status: provider.CheckinClaimed,
		Msg:    msg,
	}, nil
}

// claimDailyLoginScore 领取每日登录积分。返回可附加到结果文案的说明。
//
// 上游响应语义（实测）：
//
//	{"status":0,     ...}      → 领取成功
//	{"status":10001, "今日已领取"} → 今日已领（幂等，非错误）
//
// 失败时返回带「领取失败」字样的说明——因为这是**真实在线**的接口，
// 与已下线的旧活动不同，它的异常值得让用户看到（但**不影响整体成功状态**：
// 保活对话已成功，积分也可能由服务端自动发放）。
func (c *Client) claimDailyLoginScore(a *auth.Auth) string {
	token, err := c.acquireToken(context.Background(), a)
	if err != nil {
		return "每日登录积分：取 token 失败"
	}
	// 用 doJSONEnvelope 而非 doJSON：`status=10001 "今日已领取"` 是**正常业务语义**，
	// doJSON 会把它当错误抛出，导致重复调用被误报成失败。
	env, err := c.doJSONEnvelope(context.Background(), http.MethodPost, EpDailyLoginScore, token, map[string]any{})
	if err != nil {
		// 404/网络问题：该接口可能尚未全量开放，静默降级（不误导）
		if isNotFoundErr(err) {
			return ""
		}
		return "每日登录积分：领取失败"
	}
	switch {
	case env.Status == 0:
		return "每日登录积分已领取"
	case strings.Contains(env.Message, "已领取"):
		return "每日登录积分今日已领"
	default:
		// 其它业务码（如活动未开放）：如实但简短地反映
		if env.Message != "" {
			return "每日登录积分：" + env.Message
		}
		return "每日登录积分：未领取"
	}
}

// isNotFoundErr 判断错误是否为「接口不存在」（用于静默降级）。
func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	var pe *provider.Error
	if errors.As(err, &pe) {
		return pe.Status == http.StatusNotFound
	}
	return strings.Contains(err.Error(), "404")
}

// tryLegacyCheckin 尝试旧的 activity-api 签到（已下线，故完全静默失败）。
// 返回可附加到结果文案的说明；无事发生返回空串。
//
// 之所以保留：活动可能重新上线，届时自动获得额外收益，无需改代码。
// 之所以静默：该活动与积分发放无关，把它的失败上报成红字会误导用户。
func (c *Client) tryLegacyCheckin(a *auth.Auth) string {
	info, err := c.FetchCheckinInfo(a)
	if err != nil {
		return "" // 活动已下线/未开放：静默忽略
	}
	if info.TodayStatus != 0 {
		return "今日签到已完成"
	}
	token, err := c.acquireToken(context.Background(), a)
	if err != nil {
		return ""
	}
	body := map[string]any{"event_date": eventDate(time.Now())}
	if _, err := c.doJSON(context.Background(), http.MethodPost, EpCheckin, token, body); err != nil {
		return "" // 签到失败：静默忽略（不影响主结果）
	}
	out := "签到成功"
	if info.TotalDays > 0 {
		out += "（累计打卡 " + itoa(info.TotalDays) + " 天）"
	}
	if after, aerr := c.FetchCheckinInfo(a); aerr == nil && after.PrizeChance > 0 {
		if prize, derr := c.DrawPrize(a); derr == nil && prize != "" {
			out += "，抽中：" + prize
		}
	}
	return out
}

// itoa64 int64 → 字符串。
func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// checkinMsg 拼装可读文案（含打卡进度）。
func checkinMsg(info CheckinInfo, prefix string) string {
	msg := prefix
	if info.TotalDays > 0 {
		msg += "（累计打卡 " + itoa(info.TotalDays) + " 天"
		if info.NeedDays > 0 {
			msg += "/" + itoa(info.NeedDays)
		}
		msg += "）"
	}
	if info.PrizeChance > 0 {
		msg += "，可抽奖 " + itoa(info.PrizeChance) + " 次"
	}
	return msg
}

// classifyCheckinErr 把签到错误映射成结构化结果（而不是裸 error）。
// 之所以要分类：调度器需要区分「可重试」与「已完成」，
// 裸 error 无法表达「无签到活动」这类可重试状态（wild-work 不变量 23 的教训）。
func classifyCheckinErr(err error) (provider.CheckinReport, error) {
	var pe *provider.Error
	if asProviderError(err, &pe) {
		switch pe.Kind {
		case provider.ErrSessionDead:
			return provider.CheckinReport{
				Status: provider.CheckinNoToken,
				Msg:    "登录态失效，待刷新后重试",
			}, err
		default:
			return provider.CheckinReport{
				Status: provider.CheckinError,
				Msg:    truncate(pe.Msg, 120),
			}, err
		}
	}
	return provider.CheckinReport{
		Status: provider.CheckinError,
		Msg:    truncate(err.Error(), 120),
	}, err
}

// DrawPrize 抽奖（有次数时调用）。尽力而为，失败不影响签到结果。
func (c *Client) DrawPrize(a *auth.Auth) (string, error) {
	ctx := context.Background()
	token, err := c.acquireToken(ctx, a)
	if err != nil {
		return "", err
	}
	body := map[string]any{"event_date": eventDate(time.Now())}
	raw, err := c.doJSON(ctx, http.MethodPost, EpDrawPrize, token, body)
	if err != nil {
		return "", err
	}
	var r struct {
		HasPrize  bool   `json:"has_prize"`
		PrizeName string `json:"prize_name"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	if !r.HasPrize {
		return "", nil
	}
	return r.PrizeName, nil
}

// keepaliveChat 发一条最小对话以保活账号。
func (c *Client) keepaliveChat(a *auth.Auth) error {
	body, _ := json.Marshal(map[string]any{
		"model": "chatglm",
		"messages": []any{
			map[string]any{"role": "user", "content": "你好"},
		},
		"stream": false,
	})
	rc, status, respBody, err := c.ChatStream(a, body)
	if err != nil {
		return err
	}
	if status >= 400 {
		return &provider.Error{Kind: Classify(status, string(respBody)), Status: status,
			Msg: truncate(string(respBody), 160)}
	}
	defer rc.Close()
	// 读干流即可：目的是让上游产生一次真实对话，内容不重要。
	// 限制读取量避免长回复拖慢保活。
	_, _ = io.CopyN(io.Discard, rc, 64*1024)
	return nil
}

// asProviderError 用 errors.As 取出 *provider.Error。
func asProviderError(err error, target **provider.Error) bool {
	return errors.As(err, target)
}

// itoa 整数转字符串。
func itoa(n int) string { return strconv.Itoa(n) }
