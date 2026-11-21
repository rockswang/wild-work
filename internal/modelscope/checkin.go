package modelscope

// checkin.go 每日魔粒自动领取（票 04 / D4）。
//
// 上游**没有**独立的签到端点：每日魔粒由 `rule_key=daily_active` 的「每日登录」活动发放，
// 「领取动作」就是调用一次幂等的 `GET /api/v1/users/login/info`（探针 §4 实测：连调两次
// 响应除 RequestId 外逐字段相同，无破坏性）。
//
// ⚠️ `login/info` 的响应体里**没有任何魔粒字段**（无三态、无金额，探针 §4.1 实证），
// 所以三态与金额只能借道 `transactions` 判定（owner 拍板的 D4 选项 A）：
//
//	① 领取前先复读一次流水：当日已有 EARN ⇒ 上游今天早已发放 ⇒ `already_claimed`
//	   （这正是「已存在当日 EARN 但未再调 login/info」的场景，也是同一天连调两次
//	   第二次返回 already_claimed 的判据来源）。
//	② 当日无 EARN ⇒ 调 login/info（领取）⇒ 复读一次流水拿本次发放金额 ⇒ `claimed`。
//	③ login/info 成功但复读仍无当日 EARN ⇒ `no_campaign`（上游这天没有可领的东西）。
//
// 请求代价：当日首次领取最多 3 次主站 GET（流水 + login/info + 流水），之后每个签到
// 时段 1 次 —— 全是只读/幂等请求，不产生对话、不扣费。
//
// 金额只出现在 `Msg`（`CheckinReport.Amount` 恒为 0）：本渠道的积分入账走
// `ledger.DiffCredits` 差分（scheduler 签到后再刷一次余额），若同时用
// `RecordCheckinEarn(report.Amount)` 入账会**重复记一笔 earn**（见 scheduler 对
// CheckinReporter 的 `report.Amount > 0` 分支），故这里只透出文案、不报 Amount。

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// EpLoginInfo 幂等的「每日登录/领取」动作（Cookie: m_session_id）。
// 它是本渠道唯一的领取入口——上游没有 `/checkin` 之类的端点（探针 §4）。
const EpLoginInfo = "/api/v1/users/login/info"

// DailyCheckin 实现 provider.Upstream：执行一次幂等的每日领取。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := c.dailyCheckinReport(a)
	return err
}

// DailyCheckinReport 实现 provider.CheckinReporter：如实上报 claimed / already_claimed /
// no_campaign 三态，Msg 透出金额（D4）。
func (c *Client) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	return c.dailyCheckinReport(a)
}

// dailyCheckinReport 领取主流程（DailyCheckin 与 DailyCheckinReport 共用，照抄 lingxi）。
func (c *Client) dailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	now := time.Now()

	// ① 领取前复读：当日 EARN 已存在 ⇒ 今天已经发过了。
	if tx, err := c.fetchMagicubeTransactions(a); err != nil {
		if isSessionDead(err) {
			return provider.CheckinReport{Status: provider.CheckinError}, err
		}
		// 流水只是「判三态」的附加维度，读不到不阻塞领取动作本身（login/info 才是领取）。
		log.Printf("modelscope 领取前流水读取失败（继续 login/info）platform=%s err=%v", c.Kind, err)
	} else if amount, n := magicubeTodayEarn(tx, now); n > 0 {
		return provider.CheckinReport{
			Status: provider.CheckinAlready,
			Msg:    fmt.Sprintf("今日已领取 %d 魔粒", amount),
		}, nil
	}

	// ② 领取动作 = 幂等 login/info（D4）。失败原文直接上抛：401/403 已是 ErrSessionDead。
	if err := c.loginInfo(a); err != nil {
		return provider.CheckinReport{Status: provider.CheckinError}, err
	}

	// ③ 复读一次拿本次发放金额并区分「无活动」。
	tx, err := c.fetchMagicubeTransactions(a)
	if err != nil {
		if isSessionDead(err) {
			return provider.CheckinReport{Status: provider.CheckinError}, err
		}
		// 领取已成功但金额读不到：如实说「已领取、金额待刷新」，不伪造数字。
		log.Printf("modelscope 领取后流水读取失败（金额待刷新）platform=%s err=%v", c.Kind, err)
		return provider.CheckinReport{
			Status: provider.CheckinClaimed,
			Msg:    "今日已登录领取，魔粒发放金额待刷新",
		}, nil
	}
	if amount, n := magicubeTodayEarn(tx, now); n > 0 {
		return provider.CheckinReport{
			Status: provider.CheckinClaimed,
			Msg:    fmt.Sprintf("领到 %d 魔粒", amount),
		}, nil
	}
	// login/info 成功但当日仍无 EARN：上游这天没有可领的（R150：这不是「签到失败」）。
	return provider.CheckinReport{
		Status: provider.CheckinNoCampaign,
		Msg:    "上游还没有发放（今日暂无魔粒活动）",
	}, nil
}

// loginInfo 调一次幂等的 login/info（领取动作）。
func (c *Client) loginInfo(a *auth.Auth) error {
	_, err := c.fetchSite(a, EpLoginInfo, authCookie)
	return err
}

// magicubeTodayEarn 汇总「UTC+8 今天」的 EARN 发放：返回合计金额与行数。
//
// `gmt_created` 是**日期粒度**（"2026-11-16"，探针 §2.1），故直接比较日期串即可。
// 时区按 UTC+8 墙钟解读（与 `expire_at` 同源，探针 §2.2 的判别依据）；
// ⚠️ 「gmt_created 的时区」本身未单独实证（探针那一刻 UTC 与 UTC+8 同日期，无法分辨），
// 取与到期字段一致的墙钟口径是唯一自洽的选择。
func magicubeTodayEarn(tx magicubesTransactionsData, now time.Time) (int64, int) {
	day := now.In(creditLoc).Format("2006-01-02")
	var amount int64
	var rows int
	for _, r := range tx.Records {
		if !strings.EqualFold(strings.TrimSpace(r.Type), "EARN") || r.TotalAmt <= 0 {
			continue
		}
		if strings.TrimSpace(r.GMTCreated) != day {
			continue
		}
		amount += r.TotalAmt
		rows++
	}
	return amount, rows
}

// isSessionDead 报告错误是否是「凭据失效」类型（401/403）。
// 必须走类型断言（不变式 19）：错误文案匹配既不可靠、也会把别的失败误判成失效。
func isSessionDead(err error) bool {
	var pe *provider.Error
	if errors.As(err, &pe) {
		return pe.Kind == provider.ErrSessionDead
	}
	return false
}

// 确认实现签到上报接口（编译期断言）。
var _ provider.CheckinReporter = (*Client)(nil)
