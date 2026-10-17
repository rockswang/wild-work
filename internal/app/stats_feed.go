package app

// 运行统计引擎（internal/stats）的喂入器：把上游各数据源周期性喂进引擎，
// 数据面即面板「运行统计」tab。数据注入全部进程内直调（无 HTTP 自环，
// 见 internal/stats 包注释与 AGENTS.md R13）。
//
// 与 sidecar 版轮询器的差异（语义对齐、通道直连）：
//   - 账号状态差值：直读 pool（allStatuses），不再 GET /api/state；
//   - 粘性路由：直读 Handler.StickySnapshot()，不再解析粘性日志行；
//   - 今日作废：直读进程内 ledger（Query(1)），不再 GET /api/usage；
//   - 签到事件：启动全量重扫 app.log + logWriter 实时逐行喂（双通道
//     按 uid|day 幂等去重），不再周期性重扫；
//   - 临期/到期额度：挂 creditTotals（30min 积分自动刷新已在拉
//     UserResourceDetail，items 直接复用），不再单独轮询 resource_detail。

import (
	"context"
	"path/filepath"
	"time"

	"wild-work/internal/provider"
	statsEngine "wild-work/internal/stats"
)

// StatsFeedInterval 喂入周期：进程内直调成本极低，30s 与前端轮询节奏对齐
// （sidecar 版 state 轮询同为 30s，对比口径不变）。
const StatsFeedInterval = 30 * time.Second

// StartStatsFeeders 启动喂入器：启动先重扫日志 + 喂一轮，之后每 interval 一轮。
// 取消 ctx 即停止（main 退出路径）；a.stats 为 nil 时静默不启动。
func (a *App) StartStatsFeeders(ctx context.Context) {
	if a.stats == nil {
		return
	}
	go func() {
		// 启动重扫：今日已产生的签到事件先收进来（幂等；与 logWriter 的
		// 实时喂入按 uid|day 键去重，不会双计）。
		a.stats.RescanEventLog(a.logFilePath())
		feed := func() {
			if a.stats == nil {
				return
			}
			now := time.Now()
			a.stats.ApplyPoll(a.statsAccounts(), now)
			if a.handler != nil {
				for ch, st := range a.handler.StickySnapshot() {
					a.stats.ApplySticky(ch, st.UID, st.Count, st.Max)
				}
			}
			a.feedLedgerExpire(now)
		}
		feed() // 启动即喂：面板打开即有数（差值首轮只建基线，消耗从第二轮起记）
		t := time.NewTicker(StatsFeedInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				feed()
			}
		}
	}()
}

// logFilePath 运行日志路径（data/app.log，与 /api/logs、OpenLogFile 同源）。
func (a *App) logFilePath() string {
	return filepath.Join(filepath.Dir(a.cfg.StateFile), "app.log")
}

// statsAccounts 把 pool 状态映射成引擎账号视图（口径与面板 accountViews 一致：
// 分组按账号文件名判定、匿名渠道标不适用、时间零值转空串）。
func (a *App) statsAccounts() []statsEngine.Account {
	statuses := a.allStatuses()
	out := make([]statsEngine.Account, 0, len(statuses))
	for _, s := range statuses {
		group := a.accountGroup(s.UID)
		out = append(out, statsEngine.Account{
			UID:       s.UID,
			Group:     group,
			Nickname:  s.Nickname,
			Credits:   s.Credits,
			Expiring:  s.ExpiringCredits,
			Cooling:   s.Cooling,
			Until:     fmtTime(s.Until),
			Reason:    s.Reason,
			Disabled:  s.Disabled,
			ErrCount:  s.ErrCount,
			CreditsNA:   a.creditsNAKind(group),
			CreditsUnit: a.creditsUnit(group),
		})
	}
	return out
}

// feedLedgerExpire 今日作废喂入：ledger 当日窗口的 expire 条目按渠道求和后
// 交给引擎整体替换（引擎侧 day 守卫 + 全量替换）。空 map = 今日无作废 → 置 0；
// ledger 未启用时不喂（引擎保留旧值，与 sidecar「拉取失败保留旧值」一致）。
func (a *App) feedLedgerExpire(now time.Time) {
	if a.ledger == nil {
		return
	}
	byGroup := map[string]int64{}
	if q := a.ledger.Query(1, nil); q != nil {
		for _, e := range q.Credit.Entries {
			if e.Kind == "expire" && e.Amount < 0 {
				byGroup[e.Channel] += -e.Amount
			}
		}
	}
	a.stats.ApplyLedgerExpire(now.Format("2006-01-02"), byGroup)
}

// applyExpiryFromItems 把 UserResourceDetail 的资源条目喂引擎（账号维度的
// 最近临期日 + 当日额度）。挂在 creditTotals（积分自动刷新已拉到 items，
// 复用免二次调用）；无可喂条目时写空值清除残留。oczen 由调用方天然排除
// （积分自动刷新的 kinds 不含它）。
func (a *App) applyExpiryFromItems(uid string, items []provider.ResourceItem) {
	if a.stats == nil {
		return
	}
	day, amount := expiryDayAmount(items)
	a.stats.ApplyExpiry(uid, day, amount)
}

// expiryDayAmount 取（最早到期日, 该日可用额度合计）：Usable 且 Remain>0 且
// 带到期日的条目按日归组求和——sidecar fetchExpiry 同款口径。
func expiryDayAmount(items []provider.ResourceItem) (day string, amount int64) {
	byDay := map[string]int64{}
	for _, it := range items {
		if !it.Usable || it.Remain <= 0 || len(it.ExpireAt) < 10 {
			continue
		}
		byDay[it.ExpireAt[:10]] += it.Remain
	}
	for d := range byDay {
		if day == "" || d < day {
			day = d
		}
	}
	if day == "" {
		return "", 0
	}
	return day, byDay[day]
}
