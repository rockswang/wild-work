// Package stats 内存实时统计引擎（「运行统计」面板的数据面，与 internal/ledger 并列、口径互不混用）。
//
// 与 ledger 的分工（勿合并，见 docs/用量积分流水记账备忘.md）：
//   - ledger：磁盘 JSONL 双流水，按需聚合扫描（/api/usage），账号条目级差分；
//   - stats：内存「今日」实时口径——余额下降差值记花费（快照粒度随喂入节奏）、
//     签到日志事件记收入（差值口径漏记「登录即签到」，不用差值，见 events.go）、
//     逐请求 usage 帧记精确积分与 token（见 usage.go）；
//     多天窗口（7/30 日）按日归档落盘 data/stats.json，「今日」计数跨天滚动清零。
//     两套数字天然不同（差值含积分包到期作废、ledger 按 spend/expire 拆分），
//     面板各自标注口径，互不引用。
//
// 数据注入全部为进程内直调（禁止 HTTP 自环，见 AGENTS.md R13）：
//   - ApplyPoll：定时器周期把 App 内存账号快照喂入（差值记账 + 异常态采集）；
//   - RescanEventLog：直读本进程日志文件幂等重建今日签到收入；
//   - AddUsage / AddRequestRow：/v1 转发路径拿到 usage 帧处插桩；
//   - ApplyExpiry / ApplyLedgerExpire / ApplySticky：各定时器喂入（临期、今日作废、粘性）。
//
// 对外只暴露 Snapshot 供 GET /api/stats（前端浏览器轮询；浏览器不是进程内自环）。
package stats

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"wild-work/internal/provider"
)

// Version 引擎载荷语义版本（Snapshot.Version 透出，结构演进时 bump）。
const Version = "1"

// MilestoneStep 里程碑步长（分）：差值花费每满 N 分推一条 Toast。
const MilestoneStep = 100

// milestoneCap 里程碑上限（防刷屏护栏：10 万分/日，正常日消耗远到不了）。
const milestoneCap = 1000

// toastCap Toast 环形队列长度（超出裁最旧）。
const toastCap = 5

// burnMinObserve 预计作废的最小观测时长（小时），不足则返回 -1 不误报。
const burnMinObserve = 0.5

// expiryRiskDays 风险号判定窗口（天）。
const expiryRiskDays = 7

// Account 一份账号状态快照（ApplyPoll 的输入；由调用方从 pool 聚合拼装）。
// Credits/Expiring 为展示与差值口径；Cooling/Disabled/Reason/Until/ErrCount
// 供「异常账号」区块使用。
type Account struct {
	UID      string `json:"uid"`
	Group    string `json:"group"`
	Nickname string `json:"nickname"`
	Credits  int64  `json:"credits"`
	Expiring int64  `json:"expiring_credits"`
	Cooling  bool   `json:"cooling"`
	Disabled bool   `json:"disabled"`
	// Reason/Until 冷却原因与解冻时刻文案（来自 pool.Status；Until 为空串 = 无明确
	// 解冻点，非空时须为「字典序即时间序」的时间文案，供异常行排序）。
	Reason string `json:"reason"`
	Until  string `json:"until"`
	// ErrCount 连续错误计数（达阈值触发冷却前的中间态，异常区块提前预警）。
	ErrCount int `json:"err_count"`
	// CreditsNA 匿名渠道无积分概念（面板显示「不适用」，不参与余额合计）。
	CreditsNA bool `json:"credits_na"`
}

// Toast 里程碑通知事件（带稳定 ID，前端按 id 去重）。
type Toast struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
	At    string `json:"at"`
}

// PlatformRow 单平台汇总行（平台表）。Credits 叠加该平台全部账号当前余额（含
// 冷却/停用，展示口径）；CreditIn 为今日签到事件收入（按 uid 反查渠道）、CreditOut
// 为今日差值消耗；CreditExpired/CreditUsed 把差值花费拆成「作废 + 净值」
// （CreditOut 含积分包到期作废，CreditUsed = max(0, CreditOut−CreditExpired) 才是
// 面板「今日消耗」）；Tokens 取今日按模型消耗里前缀属于该渠道的部分；
// Expire7d/ExpireToday/ExpireTomorrow 叠加 7 日内/今日/明日到期额度
// （数据源同「最近临期」：每账号最早到期日 + 该日各包剩余合计）。
type PlatformRow struct {
	Group          string `json:"group"`
	Accounts       int    `json:"accounts"`
	Credits        int64  `json:"credits"`
	CreditIn       int64  `json:"credit_in"`
	CreditOut      int64  `json:"credit_out"`
	CreditExpired  int64  `json:"credit_expired"`
	CreditUsed     int64  `json:"credit_used"`
	Tokens         int64  `json:"tokens"`
	Expire7d       int64  `json:"expire_7d"`
	ExpireToday    int64  `json:"expire_today"`
	ExpireTomorrow int64  `json:"expire_tomorrow"`
}

// ExpiryRow 最近临期行。BurnForecast = -1 表示观测不足，无法预估。
// 列表排序（expiryRowsLocked）：到期日升序 → 同日按额度降序 → 同日同额 uid 升序。
type ExpiryRow struct {
	UID          string `json:"uid"`
	Nickname     string `json:"nickname"`
	Group        string `json:"group"`
	ExpireAt     string `json:"expire_at"` // 本地日 "2006-01-02"
	DaysLeft     int    `json:"days_left"`
	Amount       int64  `json:"amount"` // 该到期日各包剩余合计
	TodayOut     int64  `json:"today_out"`
	BurnForecast int64  `json:"burn_forecast"`
	Risk         bool   `json:"risk"`
}

// RotationRow 轮换视图行（使用中 / 接棒顺序）。
type RotationRow struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Group    string `json:"group"`
	Credits  int64  `json:"credits"`
	Expiring int64  `json:"expiring"`
	Sticky   string `json:"sticky,omitempty"` // 仅当前账号：粘性进度 "12/50"
}

// RotationView 单平台轮换视图：当前粘性账号 + 接棒顺序前 3。
type RotationView struct {
	Group   string        `json:"group"`
	Current *RotationRow  `json:"current"`
	Next    []RotationRow `json:"next"`
}

// AbnormalRow 异常账号行：「限流/冻结等不可用或亚健康账号」区块用。
// Level：disabled=已停用 / cool=整号冷却 / err=错误累计中（上游 pool 无模型级
// 冷却状态——模型级 429/11102 仍按整号冷却处置，故无 model 级）。
// Until 为解冻时刻文案（空=无明确解冻点）。
type AbnormalRow struct {
	UID       string `json:"uid"`
	Nickname  string `json:"nickname"`
	Group     string `json:"group"`
	Level     string `json:"level"`
	Reason    string `json:"reason"`
	Until     string `json:"until"`
	ErrCount  int    `json:"err_count,omitempty"`
	Credits   int64  `json:"credits"`
	UpdatedAt string `json:"updated_at"`
}

// TokenUsage 为 Token 多天窗口（窗口=今日+前 N-1 天）。
type TokenUsage struct {
	Today    int64 `json:"today"`
	Days7    int64 `json:"days7"`
	Days30   int64 `json:"days30"`
	All      int64 `json:"all"`
	ReqToday int64 `json:"req_today"`
	ReqAll   int64 `json:"req_all"`
	Days     int   `json:"days"` // 已记录天数（历史日数）
}

// Snapshot 引擎对外只读快照（GET /api/stats 的载荷；今日口径，跨天滚动清零）。
type Snapshot struct {
	Version        string  `json:"version"`
	Today          string  `json:"today"`
	Now            string  `json:"now"`
	TotalCredits   int64   `json:"total_credits"` // 全部账号当前余额合计（含冷却/停用；匿名渠道除外）
	TotalAccounts  int     `json:"total_accounts"`
	CreditIn       int64   `json:"credit_in"`        // 今日签到事件收入
	CreditOut      int64   `json:"credit_out"`       // 今日差值花费合计（含到期作废）
	CreditOutExact float64 `json:"credit_out_exact"` // 今日逐请求精确积分（有 usage 帧积分的渠道）
	// CreditExpired/CreditUsed：差值花费里混着「积分包到期作废」，拆出后 CreditUsed
	// 才是真消耗。ledger 作废额缺失（轮询失败）时 CreditExpired=0 → CreditUsed 退化
	// 为 CreditOut，与拆分前显示一致（降级不报错）。
	CreditExpired   int64           `json:"credit_expired"`
	CreditUsed      int64           `json:"credit_used"`
	Tokens          int64           `json:"tokens"`
	Reqs            int64           `json:"reqs"`
	TokenUsage      TokenUsage      `json:"token_usage"`
	Expiring7d      int64           `json:"expiring_7d"`       // 未来 7 天（含今日）内到期额度合计
	Expiring7dAccts int             `json:"expiring_7d_accts"` // 涉临期额度的账号数
	ExpireToday     int64           `json:"expire_today"`      // 全池今日到期额度合计
	ExpireTomorrow  int64           `json:"expire_tomorrow"`   // 全池明日到期额度合计
	Platforms       []PlatformRow   `json:"platforms"`         // 各平台今日情况（余额降序）
	Models          []ModelStat     `json:"models"`            // 今日按模型消耗（积分降序）
	Expiry          []ExpiryRow     `json:"expiry"`            // 最近临期（到期日升序）
	Rotation        []RotationView  `json:"rotation"`          // 使用中 / 接棒顺序
	Abnormal        []AbnormalRow   `json:"abnormal"`          // 异常账号（无异常为空，前端隐藏区块）
	Fallbacks       []FallbackEvent `json:"fallbacks"`         // 降级记录（最新在前；空 = 无事件，前端隐藏区块）
	MilestoneTotal  int64           `json:"milestone_total"`
	MilestoneCount  int             `json:"milestone_count"`
	ToastQueue      []Toast         `json:"toasts"`
}

// acct 引擎内单账号记账态（credit_out 与 burn_since 同窗持久化，见 acctPersist）。
type acct struct {
	uid      string
	nickname string
	group    string
	credits  int64
	expiring int64 // 上游 24h 临期额度（轮换推算用，纯内存）
	cooling  bool
	disabled bool
	// 异常账号区块：冷却原因/解冻时刻/错误计数（纯内存，每轮喂入覆盖）。
	coolReason string
	coolUntil  string
	errCount   int
	creditsNA  bool
	hasLast    bool      // 是否已确立差值基线（首轮只建基线不记差值）
	creditOut  int64     // 今日差值花费（仅下降记，上涨不计）
	expireAt   string    // 最早到期本地日（ApplyExpiry 喂入，纯内存）
	expireAmt  int64     // 到期日各包剩余合计（纯内存）
	burnSince  time.Time // 今日差值观测起点（持久化，跨天清零重设）
}

// stickyInfo 平台粘性现状（ApplySticky 喂入，纯内存、每轮覆盖）。
type stickyInfo struct {
	uid   string
	count int
	max   int
}

// creditVal 账号的积分口径值：匿名渠道（无积分概念）恒 0，不参与任何余额/差值口径。
// Snapshot.TotalCredits 与平台行 Credits 共用此口径，防两处各自维护漂移。
func (a *acct) creditVal() int64 {
	if a.creditsNA {
		return 0
	}
	return a.credits
}

// Stats 引擎本体。并发安全（单一互斥锁；写入量小，锁竞争可忽略）。
type Stats struct {
	mu   sync.Mutex
	dir  string
	file string
	// logf 由 New 注入，nil 时静默；落盘失败等关键告警走这里。
	logf func(format string, args ...any)

	today  string
	accts  map[string]*acct
	sticky map[string]stickyInfo // platform -> 当前粘性账号
	events map[string]int64      // 今日签到事件收入：uid|day -> 数额（事件口径，见 events.go）
	// fallbacks 降级链事件环形（见 fallbacks.go；纯内存，重启清零，最新在尾部）。
	fallbacks []*fbEv

	creditInTotal  int64
	creditOutTotal int64
	creditOutExact float64
	tokensToday    int64
	reqsToday      int64

	// 今日作废（ledger 周期喂入，纯内存不落盘：ledger 自身持久，重启后下一轮
	// 喂入即自愈）。命名用 expired*（已作废）区别既有 expire*（将到期，见 acct.expireAt）。
	expiredByGroup map[string]int64
	expiredTotal   int64

	tokenDays map[string]int64                  // YYYY-MM-DD -> tokens（不含今日）
	reqDays   map[string]int64                  // YYYY-MM-DD -> reqs（不含今日）
	modelDays map[string]map[string]*modelEntry // 历史日 -> 模型 -> 消耗（跨天归档）

	models    map[string]*modelEntry
	milestone int

	toasts []Toast
	seq    int64
	ring   []ReqRow

	// 签到日志解析健康度（纯内存、不落盘）：形状像签到结果却无一命中严格正则
	// = 日志文案已变、收入统计归零，边沿告警（见 events.go noteEventScan）。
	evStrict   int
	evUnparsed int
	evDegraded bool
	evSample   string

	dirty bool
}

// New 创建引擎并加载 dataDir/stats.json（多天窗口与今日基线随存随恢复）。
// 文件缺失/首次启动按零值起步，不算错误。logf 可为 nil。
func New(dataDir string, logf func(format string, args ...any)) (*Stats, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &Stats{
		dir:            dataDir,
		file:           filepath.Join(dataDir, "stats.json"),
		today:          todayKey(time.Now()),
		accts:          map[string]*acct{},
		sticky:         map[string]stickyInfo{},
		events:         map[string]int64{},
		expiredByGroup: map[string]int64{},
		tokenDays:      map[string]int64{},
		reqDays:        map[string]int64{},
		modelDays:      map[string]map[string]*modelEntry{},
		models:         map[string]*modelEntry{},
		ring:           make([]ReqRow, 0, ringCap),
		logf:           logf,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func todayKey(t time.Time) string { return t.Format("2006-01-02") }

// rollDayLocked 跨天归档：今日 tokens/reqs/模型消耗转入历史 map，重置全部今日计数。
// 已知边界：跨天后首轮喂入的差值（含 23:45~00:00 之间的消耗）会记入新一天，
// 幅度 ≤ 一个喂入周期——差值记账的固有权衡，维持现状。
func (s *Stats) rollDayLocked(now time.Time) {
	key := todayKey(now)
	if s.today == key {
		return
	}
	// 零值不入档：不产生空历史键（Days = 有消耗的天数，且首启当天未跨天不占位）
	if s.tokensToday > 0 {
		s.tokenDays[s.today] += s.tokensToday
	}
	if s.reqsToday > 0 {
		s.reqDays[s.today] += s.reqsToday
	}
	if len(s.models) > 0 {
		mm := make(map[string]*modelEntry, len(s.models))
		for name, e := range s.models {
			mm[name] = cloneEntry(e)
		}
		s.modelDays[s.today] = mm
	}
	s.today = key
	s.creditInTotal, s.creditOutTotal = 0, 0
	s.creditOutExact = 0
	s.tokensToday, s.reqsToday = 0, 0
	s.expiredByGroup = map[string]int64{}
	s.expiredTotal = 0
	s.models = map[string]*modelEntry{}
	s.milestone = 0
	s.events = map[string]int64{} // 签到事件收入：跨天作废（重扫日志自动重建）
	s.toasts = nil
	for _, a := range s.accts {
		a.creditOut = 0
		a.burnSince = time.Time{} // 跨天清零，下次喂入重新确立
	}
	// 归档即时落盘：跨天是低频事件（一天一次），同步写成本可忽略；
	// 只置 dirty 的话，滚动后 ≤5s（RunFlusher 周期）内崩溃会让昨日归档
	// 只存在于内存——重启后 load 的 day 守卫把磁盘上的前日态整块丢弃，昨日数据永久丢失。
	s.flushLocked()
}

// ApplyPoll 应用一份账号状态快照（进程内定时器喂入；等价于轮询 /api/state 的差值记账）。
//
// 口径：仅记余额下降差为今日花费；余额上涨不计收入（收入走签到事件口径，见 events.go
// ——「登录即签到」等场景下差值恒 0 会漏记）。
// 空快照视为上游瞬时空响应，跳过本轮（按空清空会把差值基线全抹掉；真实全删号
// 极罕见，让下一轮正常数据自行收敛）。
func (s *Stats) ApplyPoll(list []Account, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDayLocked(now)
	if len(list) == 0 {
		s.logf("状态快照为空，跳过本轮记账")
		return
	}

	seen := map[string]bool{}
	for _, u := range list {
		seen[u.UID] = true
		a, ok := s.accts[u.UID]
		if !ok {
			a = &acct{uid: u.UID}
			s.accts[u.UID] = a
		}
		a.nickname, a.group = u.Nickname, u.Group
		a.expiring, a.cooling, a.disabled = u.Expiring, u.Cooling, u.Disabled
		a.coolReason, a.coolUntil, a.errCount = u.Reason, u.Until, u.ErrCount
		a.creditsNA = u.CreditsNA
		if a.hasLast && !a.creditsNA && u.Credits < a.credits {
			a.creditOut += a.credits - u.Credits // 仅记消耗；余额上涨不是收入事件
		}
		if a.burnSince.IsZero() {
			a.burnSince = now // 本日首次确立差值观测起点（持久化，burn 外推用）
		}
		a.credits, a.hasLast = u.Credits, true
	}
	for uid := range s.accts {
		if !seen[uid] { // 已删号剔除，防差值虚高
			delete(s.accts, uid)
		}
	}
	// 重算全池合计（防漂移）；收入合计归事件口径维护（recordEvent）。
	var out int64
	for _, a := range s.accts {
		out += a.creditOut
	}
	s.creditOutTotal = out
	s.milestoneLocked(now)
	s.dirty = true
}

// milestoneLocked 差值花费每满 MilestoneStep 推一条 Toast；单轮跨多台阶合并为一条
// （防一次喂入弹出积压）。文案用「花费」（基数含到期作废，与净值「消耗」区分）。
func (s *Stats) milestoneLocked(now time.Time) {
	if s.creditOutTotal < int64((s.milestone+1)*MilestoneStep) || s.milestone >= milestoneCap {
		return
	}
	added := 0
	for s.creditOutTotal >= int64((s.milestone+1)*MilestoneStep) && s.milestone < milestoneCap {
		s.milestone++
		added++
	}
	body := fmt.Sprintf("今日花费 %d 分（第 %d 个 %d 分）", s.creditOutTotal, s.milestone, MilestoneStep)
	if added > 1 {
		body = fmt.Sprintf("今日花费 %d 分（+%d 个 %d 分，累计第 %d 个）", s.creditOutTotal, added, MilestoneStep, s.milestone)
	}
	s.toasts = append(s.toasts, Toast{
		ID:    fmt.Sprintf("%s-m%d", s.today, s.milestone),
		Title: "今日情况",
		Body:  body,
		At:    now.Format("15:04:05"),
	})
	if len(s.toasts) > toastCap {
		s.toasts = s.toasts[len(s.toasts)-toastCap:]
	}
}

// ApplyExpiry 写入单账号临期数据（expireDay 为本地日 "2006-01-02"，amount 为该日
// 各包剩余合计）。day=="" 时写入空值即清除旧数据——否则账号余额耗尽后面板会一直
// 显示过期残留的临期行直到跨天。
func (s *Stats) ApplyExpiry(uid, expireDay string, amount int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.accts[uid]; ok {
		a.expireAt, a.expireAmt = expireDay, amount
	}
}

// ApplyLedgerExpire 写入一轮 ledger 的「今日作废」合计（1.18.0 同款拆分）。
// 与 ApplyExpiry 无关：后者是**将到期**的临期预测，本方法是**已作废**的既成事实。
//
// byGroup 的键为 ledger 条目 channel 原值（内部 normalizePlatform 归一），
// 语义是**本轮全量**——整体替换而非累加：ledger 是持久账本、每轮重算今日合计，
// 累加会随轮次膨胀。传空 map 表示今日无作废（置 0）；拉取失败时调用方**不应调用**
// 本方法，保留上一轮有效值（降级为「作废额暂时陈旧」，好过退回含作废的旧口径
// 把假花费当真消耗）。
//
// day 守卫：条目归属日须等于当前记账日，否则整批丢弃——跨零点时上游可能仍返回
// 昨日数据，误记会让新的一天开局就背上一笔假作废。
func (s *Stats) ApplyLedgerExpire(day string, byGroup map[string]int64) {
	if day == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDayLocked(time.Now())
	if day != s.today {
		return
	}
	grouped := make(map[string]int64, len(byGroup))
	var total int64
	for ch, v := range byGroup {
		if v <= 0 {
			continue
		}
		name := normalizePlatform(ch)
		if name == "" {
			name = "unknown"
		}
		grouped[name] += v
		total += v
	}
	s.expiredByGroup = grouped
	s.expiredTotal = total
}

// ApplySticky 写入平台当前粘性账号（server 粘性路由访问器喂入，纯内存、每轮覆盖）。
func (s *Stats) ApplySticky(platform, uid string, count, max int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sticky[platform] = stickyInfo{uid: uid, count: count, max: max}
}

// AcctList 返回当前账号列表（临期数据喂入方迭代用）。
func (s *Stats) AcctList() []Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Account, 0, len(s.accts))
	for _, a := range s.accts {
		out = append(out, Account{UID: a.uid, Group: a.group, Nickname: a.nickname})
	}
	return out
}

// daysUntil 计算「本地日历日」差值：今天到期=0、明天=1。
// 不可用 hours/24 截断——那会把「明天 00:00 到期」算成 0 天。
func daysUntil(expDay, now time.Time) int {
	expDate := time.Date(expDay.Year(), expDay.Month(), expDay.Day(), 0, 0, 0, 0, expDay.Location())
	nowDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	days := int(expDate.Sub(nowDate).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

// burnForecastLocked 预计作废：按今日差值速率外推距到期时段的消耗。
// 返回 -1 = 观测不足/无消耗（宁可不预估也不误报）；已到期返回 0。
func (s *Stats) burnForecastLocked(a *acct, now time.Time) int64 {
	if a.expireAt == "" || a.burnSince.IsZero() || a.creditOut <= 0 {
		return -1
	}
	hours := now.Sub(a.burnSince).Hours()
	if hours < burnMinObserve {
		return -1
	}
	expDay, err := time.ParseInLocation("2006-01-02", a.expireAt, time.Local)
	if err != nil {
		return -1
	}
	hoursTo := expDay.Add(24 * time.Hour).Sub(now).Hours() // 到期日全天结束
	if hoursTo <= 0 {
		return 0
	}
	return int64(float64(a.creditOut) / hours * hoursTo)
}

// expiryRowsLocked 生成最近临期行（到期日升序、同日按额降序、同日同额 uid 升序
// 稳定锚点，全量下发，前端限高滚动展示）。
func (s *Stats) expiryRowsLocked(now time.Time) []ExpiryRow {
	rows := make([]ExpiryRow, 0, len(s.accts))
	for _, a := range s.accts {
		if a.expireAt == "" || a.expireAmt <= 0 {
			continue
		}
		expDay, err := time.ParseInLocation("2006-01-02", a.expireAt, time.Local)
		if err != nil {
			continue
		}
		daysLeft := daysUntil(expDay, now)
		forecast := s.burnForecastLocked(a, now)
		rows = append(rows, ExpiryRow{
			UID:          a.uid,
			Nickname:     a.nickname,
			Group:        a.group,
			ExpireAt:     a.expireAt,
			DaysLeft:     daysLeft,
			Amount:       a.expireAmt,
			TodayOut:     a.creditOut,
			BurnForecast: forecast,
			Risk:         daysLeft <= expiryRiskDays && forecast >= 0 && forecast < a.expireAmt,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ExpireAt != rows[j].ExpireAt {
			return rows[i].ExpireAt < rows[j].ExpireAt // 到期日升序
		}
		if rows[i].Amount != rows[j].Amount {
			return rows[i].Amount > rows[j].Amount // 同日按额度降序
		}
		return rows[i].UID < rows[j].UID // 同日同额：uid 升序稳定锚点（防刷新间跳动）
	})
	return rows
}

// channelKind 渠道前缀白名单（= provider.Kind，也等于 /api/state 的 group 值），
// 用于把模型名 "<渠道>/<模型>" 归属到平台行。新增渠道须同步此表——遗漏会让该渠道
// 的 token 不进平台表；不在表内（含无前缀的裸模型名——那类请求由 compat 层映射，
// 本地看不到真实渠道）一律不归属，宁可少计也不误计。
var (
	channelKindMu sync.RWMutex
	channelKind   = map[string]bool{
		string(provider.WorkBuddy): true, string(provider.WorkBuddyAI): true,
		string(provider.TraeWork): true, string(provider.TraeCode): true,
		string(provider.Qoder): true, string(provider.QoderCN): true, string(provider.QoderCOM): true,
		string(provider.QwenWork): true, string(provider.MonkeyCode): true,
		string(provider.Raccoon): true, string(provider.Loomy): true,
		string(provider.Oczen): true, string(provider.GLM): true,
		string(provider.Cline):     true, // R59：cline 的模型名是 cline-free/*，漏了它则该渠道 token 不进平台表
		string(provider.OfficeAce): true,
	}
)

// RegisterChannelKind 注册动态/自定义渠道前缀（R66），使其 token 与请求进平台概览表。
func RegisterChannelKind(k string) {
	channelKindMu.Lock()
	defer channelKindMu.Unlock()
	channelKind[k] = true
}

// IsKnownChannel 检查渠道前缀是否已知。
func IsKnownChannel(k string) bool {
	channelKindMu.RLock()
	defer channelKindMu.RUnlock()
	return channelKind[k]
}

// traeKinds 为 Trae 系渠道集合（TraeWork + TraeCode 共用账号与积分池——上游只为
// TraeWork 建池，但两个 function 的模型前缀独立，统计口径必须合并）。
var traeKinds = map[string]bool{
	string(provider.TraeWork): true,
	string(provider.TraeCode): true,
}

// isTraeKind 判定渠道标识是否属于 Trae 系（共用账号积分池，积分按 token 占比分摊）。
func isTraeKind(g string) bool { return traeKinds[g] }

// normalizePlatform 把渠道标识归一为面板平台归属：TraeCode 归入 TraeWork
// （二者共用账号池，上游不为其单列账号组；面板按平台一行展示）。
// 其余渠道原样返回；空串保持空串（由调用方决定是否落 "unknown"）。
func normalizePlatform(g string) string {
	if g == string(provider.TraeCode) {
		return string(provider.TraeWork)
	}
	return g
}

// noExpiryChannel 判定渠道是否无临期/到期概念（oczen 匿名免费：上游对其
// resource_detail 恒返拒绝，临期数据喂入方须跳过，否则每轮恒报失败噪音）。
func noExpiryChannel(g string) bool { return g == string(provider.Oczen) }

// platformRowsLocked 生成平台表行（调用方须持锁）：按 group 聚合账号数/余额/今日
// 收入/今日花费/作废与净值/今日 Token，并按各账号 expireAt 归入 7 日内/今日/明日
// 到期额。排序=余额降序（再按渠道名升序稳定）。
// 过滤规则：八个数值字段全为 0 的平台整行剔除（有号但余额与临期皆空、今日也无
// 流量的小渠道不占版面）。被剔除的行对本表任何数值列的贡献都是 0，
// 故「显示行之和 == 全池合计」恒成立。
//
// 归并：TraeCode 与 TraeWork 共用同一批账号与积分池，模型流量按前缀归一到
// traework 一行（账号侧本就没有 traecode 组，合并不产生重复计数）。
func (s *Stats) platformRowsLocked(now time.Time) []PlatformRow {
	deadline := now.AddDate(0, 0, 7).Format("2006-01-02")
	today := now.Format("2006-01-02")
	tomorrow := now.AddDate(0, 0, 1).Format("2006-01-02")
	agg := make(map[string]*PlatformRow, 8)
	rowOf := func(g string) *PlatformRow {
		g = normalizePlatform(g)
		if g == "" {
			g = "unknown"
		}
		row, ok := agg[g]
		if !ok {
			row = &PlatformRow{Group: g}
			agg[g] = row
		}
		return row
	}
	// 存量与今日花费（账号维度；匿名渠道不计余额但计账号数）
	for _, a := range s.accts {
		row := rowOf(a.group)
		row.Accounts++
		row.Credits += a.creditVal()
		row.CreditOut += a.creditOut
		if a.expireAt == "" || a.expireAmt <= 0 {
			continue
		}
		if a.expireAt <= deadline {
			row.Expire7d += a.expireAmt
		}
		if a.expireAt == today {
			row.ExpireToday += a.expireAmt
		}
		if a.expireAt == tomorrow {
			row.ExpireTomorrow += a.expireAmt
		}
	}
	// 今日收入：事件键 "uid|day"，按 uid 反查渠道。已删号的事件仅进全局合计，不归属平台。
	for k, v := range s.events {
		i := strings.LastIndexByte(k, '|')
		if i <= 0 {
			continue
		}
		if a, ok := s.accts[k[:i]]; ok {
			rowOf(a.group).CreditIn += v
		}
	}
	// 今日 Token：模型名带渠道前缀（与 /v1/models 的 id 同构）
	for name, m := range s.models {
		if g, ok := channelOfModel(name); ok {
			rowOf(g).Tokens += m.tokens
		}
	}
	// 今日作废（ledger 周期喂入的全池按渠道值，非账号维度，账号已删也归属渠道）
	for g, v := range s.expiredByGroup {
		rowOf(g).CreditExpired += v
	}
	// 净值 = 差值花费 − 今日作废，钳 0：两源刷新窗口不同、差值口径本身滞后，
	// 短暂倒挂时显示 0 而不是负的「今日消耗」。
	for _, r := range agg {
		if r.CreditUsed = r.CreditOut - r.CreditExpired; r.CreditUsed < 0 {
			r.CreditUsed = 0
		}
	}

	out := make([]PlatformRow, 0, len(agg))
	for _, r := range agg {
		if r.Credits <= 0 && r.CreditIn <= 0 && r.CreditOut <= 0 && r.CreditExpired <= 0 &&
			r.Tokens <= 0 && r.Expire7d <= 0 && r.ExpireToday <= 0 && r.ExpireTomorrow <= 0 {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Credits != out[j].Credits {
			return out[i].Credits > out[j].Credits
		}
		return out[i].Group < out[j].Group
	})
	return out
}

// rotationLocked 生成「使用中 / 接棒顺序」视图（调用方须持锁）。
// 排序复刻上游 pool 选号两级排序：临期额度降序 → 余额降序（再以 uid 升序保证稳定）；
// 排除冷却/停用；接棒顺序剔除当前粘性账号。粘性账号被删/冷却后 Current 自动为
// null（上游下一请求会重建粘性记录，下一轮喂入自然纠正）。
// 只下发有使用记录（Current 非 null）的平台——无粘性记录=今日未用过，
// 展示「暂无使用记录」行没有信息量。
func (s *Stats) rotationLocked() []RotationView {
	groups := map[string]bool{}
	for _, a := range s.accts {
		if a.group != "" {
			groups[normalizePlatform(a.group)] = true
		}
	}
	out := make([]RotationView, 0, len(groups))
	for g := range groups {
		view := RotationView{Group: g, Next: []RotationRow{}}

		st, hasSticky := s.sticky[g]
		if !hasSticky || st.uid == "" {
			continue
		}
		cur, ok := s.accts[st.uid]
		if !ok || normalizePlatform(cur.group) != g {
			continue
		}
		view.Current = &RotationRow{
			UID: cur.uid, Nickname: cur.nickname, Group: normalizePlatform(cur.group),
			Credits: cur.credits, Expiring: cur.expiring,
			Sticky: fmt.Sprintf("%d/%d", st.count, st.max),
		}

		cands := make([]*acct, 0, len(s.accts))
		for _, a := range s.accts {
			if normalizePlatform(a.group) == g && !a.cooling && !a.disabled {
				cands = append(cands, a)
			}
		}
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].expiring != cands[j].expiring {
				return cands[i].expiring > cands[j].expiring
			}
			if cands[i].credits != cands[j].credits {
				return cands[i].credits > cands[j].credits
			}
			return cands[i].uid < cands[j].uid
		})

		for _, a := range cands {
			if a.uid == view.Current.UID {
				continue
			}
			view.Next = append(view.Next, RotationRow{
				UID: a.uid, Nickname: a.nickname, Group: normalizePlatform(a.group),
				Credits: a.credits, Expiring: a.expiring,
			})
			if len(view.Next) >= 3 {
				break
			}
		}
		out = append(out, view)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Group < out[j].Group })
	return out
}

// abnormalRowsLocked 生成「异常账号」行（调用方须持锁）。收录范围（按严重度排序）：
//   - disabled：已停用（session dead 等，需人工处理）；
//   - cool：整号冷却中（429/欠费/账号故障），Until 为解冻时刻；
//   - err：连续错误累计中（达阈值触发冷却前的中间态，提前预警）。
//
// 排序：severity 升序 → 解冻时刻升序（空视为最远）→ uid 升序保证稳定。
// 无异常时返回空切片（前端隐藏区块）。
func (s *Stats) abnormalRowsLocked(now time.Time) []AbnormalRow {
	rows := make([]AbnormalRow, 0, 4)
	for _, a := range s.accts {
		level := ""
		switch {
		case a.disabled:
			level = "disabled"
		case a.cooling:
			level = "cool"
		case a.errCount > 0:
			level = "err"
		default:
			continue
		}
		row := AbnormalRow{
			UID: a.uid, Nickname: a.nickname, Group: normalizePlatform(a.group), Level: level,
			Reason: a.coolReason, Credits: a.credits, ErrCount: a.errCount,
			UpdatedAt: now.Format("15:04:05"),
		}
		if level == "cool" {
			row.Until = a.coolUntil
		}
		rows = append(rows, row)
	}
	severity := map[string]int{"disabled": 0, "cool": 1, "err": 2}
	sort.Slice(rows, func(i, j int) bool {
		if severity[rows[i].Level] != severity[rows[j].Level] {
			return severity[rows[i].Level] < severity[rows[j].Level]
		}
		if rows[i].Until != rows[j].Until {
			if rows[i].Until == "" {
				return false // 无解冻时刻视为最远，排后
			}
			if rows[j].Until == "" {
				return true
			}
			return rows[i].Until < rows[j].Until
		}
		return rows[i].UID < rows[j].UID
	})
	return rows
}

// Snapshot 生成对外只读快照（now 由调用方传入；GET /api/stats 传 time.Now()）。
// 平台行余额降序；全池「今日消耗」= 各平台净值逐行相加，而非 max(0, 总差值−总作废)
// ——逐行钳 0 之后再求和，「合计行 == 显示行之和」才恒成立。
func (s *Stats) Snapshot(now time.Time) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDayLocked(now)

	tu := TokenUsage{
		Today:    s.tokensToday,
		Days7:    s.windowLocked(now, 7) + s.tokensToday,
		Days30:   s.windowLocked(now, 30) + s.tokensToday,
		ReqToday: s.reqsToday,
		ReqAll:   s.reqSumLocked() + s.reqsToday,
		Days:     len(s.tokenDays),
	}
	tu.All = tu.Days30 // 累计口径：30 日窗口 + 更早历史
	for day, v := range s.tokenDays {
		if day < now.AddDate(0, 0, -29).Format("2006-01-02") {
			tu.All += v
		}
	}

	models := s.modelStatsLocked()
	platforms := s.platformRowsLocked(now)

	var totalCredits int64
	for _, a := range s.accts {
		totalCredits += a.creditVal()
	}

	// 今/明日到期与 7 日临期（全池口径，与平台各行之和必然相等）
	deadline := now.AddDate(0, 0, 7).Format("2006-01-02")
	today := now.Format("2006-01-02")
	tomorrow := now.AddDate(0, 0, 1).Format("2006-01-02")
	var expiring7d int64
	var expiring7dAccts int
	var expireToday int64
	var expireTomorrow int64
	for _, a := range s.accts {
		if a.expireAt == "" || a.expireAmt <= 0 {
			continue
		}
		if a.expireAt == today {
			expireToday += a.expireAmt
		}
		if a.expireAt == tomorrow {
			expireTomorrow += a.expireAmt
		}
		if a.expireAt > deadline {
			continue
		}
		expiring7d += a.expireAmt
		expiring7dAccts++
	}

	var creditUsed int64
	for _, r := range platforms {
		creditUsed += r.CreditUsed
	}

	toasts := make([]Toast, len(s.toasts))
	copy(toasts, s.toasts)

	return Snapshot{
		Version:         Version,
		Today:           s.today,
		Now:             now.Format("2006-01-02 15:04:05"),
		TotalCredits:    totalCredits,
		TotalAccounts:   len(s.accts),
		CreditIn:        s.creditInTotal,
		CreditOut:       s.creditOutTotal,
		CreditOutExact:  s.creditOutExact,
		CreditExpired:   s.expiredTotal,
		CreditUsed:      creditUsed,
		Tokens:          s.tokensToday,
		Reqs:            s.reqsToday,
		TokenUsage:      tu,
		Expiring7d:      expiring7d,
		Expiring7dAccts: expiring7dAccts,
		ExpireToday:     expireToday,
		ExpireTomorrow:  expireTomorrow,
		Platforms:       platforms,
		Models:          models,
		Expiry:          s.expiryRowsLocked(now),
		Rotation:        s.rotationLocked(),
		Abnormal:        s.abnormalRowsLocked(now),
		Fallbacks:       s.fallbackRowsLocked(),
		MilestoneTotal:  s.creditOutTotal,
		MilestoneCount:  s.milestone,
		ToastQueue:      toasts,
	}
}
