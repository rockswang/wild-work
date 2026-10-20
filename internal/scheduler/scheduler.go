// Package scheduler 定时任务：每日签到 + token keepalive。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
// 签到时间支持分钟精度运行中更新（SetCheckinMinutes），由 GUI 面板写入 config.json 后调用。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/ledger"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// Config 调度器依赖。
//
// 三个时间字段的零值语义见 New：nil = 未配置（落默认），
// []int{} = 本渠道没有这类任务（保持为空）。想关掉某类任务却传 nil，
// 会被静默补上默认时间并每天空跑。
type Config struct {
	Pool           *pool.Pool
	Upstream       provider.Upstream
	Name           string // 日志中的平台名
	CheckinHours   []int  // 旧配置兼容，整点小时
	CheckinMinutes []int  // 当天分钟数，优先于 CheckinHours
	KeepaliveHours []int  // 默认 [22]

	// CheckinRetryUntil 签到重试截止（当天分钟数）。
	// 上游机制：每日窗口 10:00 开放，但活动可能在整点后才创建（实测有延迟），
	// 若首次尝试得到 no_campaign/error 就直接标记当日完成，会整整漏领一天。
	// 故在 [CheckinMinutes, CheckinRetryUntil] 区间内每分钟重试，直到
	// 拿到 claimed/already_claimed 或超过截止时间（上游取 12:00）。
	// 0 = 关闭重试（只按 CheckinMinutes 精确触发）。
	CheckinRetryUntil int

	// ActivitiesOnly 表示本渠道无签到活动：定时仍调用 Upstream.DailyCheckin
	// （用于账号活跃保活），但不记录、不上报签到状态，保持对用户透明。
	// WorkBuddy 国际版使用该模式。
	ActivitiesOnly bool

	// ExpiringThreshold 临期阈值（config.schedule.expiring_threshold_hours）。
	// 0 = 24h 兜底。用于签到后计算临期额度（Pick() 的第一排序键）。
	//
	// 注意：自 2026-10-19 起判据改为**自然日**（expiringDays()），本字段只作
	// 换算来源；不要直接拿它跟 ExpireAt 做时长比较（会把「次日 00:00 失效」
	// 在 24h 阈值下漏掉或误判，见 provider.ExpireWithinDays 注释）。
	ExpiringThreshold time.Duration

	// Ledger 积分流水记账器（非 nil 时签到后余额刷新触发差分记账）。
	Ledger *ledger.Ledger
}

// Scheduler 调度器。
type Scheduler struct {
	mu        sync.Mutex // 保护 cfg 中的小时配置
	cfg       Config
	wake      chan struct{}              // 配置变更唤醒 Run 循环重算下次触发
	onCheckin func(CheckinResult)        // 结果观察器，供 GUI 接收自动签到结果
	onRefresh func(string, bool, string) // token 刷新结果观察器

	// refreshOverride 仅测试使用：替换 refreshForCheckin 的刷新动作（模拟「刷新也失败」）。
	refreshOverride func(*auth.Auth, string) error

	// retryMu 保护签到完成状态（自动调度的 Run goroutine 与手动签到并发）。
	retryMu sync.Mutex
	// checkinDone 当日已完成签到的时段集合，键为 "YYYY-MM-DD@起始分钟数"。
	// 语义对齐上游 lastCheckinDay：一旦某时段拿到 claimed/already 就不再重试。
	// 用「日+时段」而非「日」作键：本工具允许一天多个签到时间（如 09:00/21:00），
	// 若只按日标记，早间签到成功会连带吞掉晚间时段。
	checkinDone map[string]bool
}

// New 构建。
//
// 零值（nil）与显式空切片语义不同，不可混用：
//   - nil     = 「未配置」→ 落默认（签到 9:00/21:00，保活 22:00）
//   - []int{} = 「本渠道没有这类定时任务」→ 保持为空，什么都不跑
//
// 用 len(...) == 0 判定会把后者一并兜底成默认时间，导致无签到活动、
// 无 refresh 端点的渠道每天空跑并记录失败。
func New(cfg Config) *Scheduler {
	if cfg.CheckinMinutes == nil {
		if len(cfg.CheckinHours) > 0 {
			cfg.CheckinMinutes = make([]int, 0, len(cfg.CheckinHours))
			for _, h := range cfg.CheckinHours {
				if h >= 0 && h <= 23 {
					cfg.CheckinMinutes = append(cfg.CheckinMinutes, h*60)
				}
			}
		} else {
			cfg.CheckinMinutes = []int{9 * 60, 21 * 60}
		}
	}
	if cfg.KeepaliveHours == nil {
		cfg.KeepaliveHours = []int{22}
	}
	if cfg.ExpiringThreshold <= 0 {
		cfg.ExpiringThreshold = 24 * time.Hour
	}
	return &Scheduler{cfg: cfg, wake: make(chan struct{}, 1), checkinDone: map[string]bool{}}
}

// expiringDays 把时长阈值换算成自然日数（1/2/3；非整日向上取整）。
// 与 config.Config.ExpiringThresholdDays 同算法，保持两侧判据一致。
func (c Config) expiringDays() int {
	h := int(c.ExpiringThreshold / time.Hour)
	if h <= 0 {
		h = 24
	}
	days := h / 24
	if h%24 != 0 {
		days++
	}
	if days < 1 {
		days = 1
	}
	return days
}

// schedule 返回当前签到分钟/保活小时配置的副本。
func (s *Scheduler) schedule() (checkinMinutes, keepaliveHours []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int{}, s.cfg.CheckinMinutes...), append([]int{}, s.cfg.KeepaliveHours...)
}

// CheckinHours 保留旧 API，返回整点签到小时。
func (s *Scheduler) CheckinHours() []int {
	minutes, _ := s.schedule()
	out := make([]int, 0, len(minutes))
	for _, m := range minutes {
		if m%60 == 0 {
			out = append(out, m/60)
		}
	}
	return out
}

// CheckinMinutes 当前签到时间，单位为当天分钟数（0..1439）。
func (s *Scheduler) CheckinMinutes() []int {
	minutes, _ := s.schedule()
	return minutes
}

// CheckinTimes 当前签到时间，格式为 HH:MM。
func (s *Scheduler) CheckinTimes() []string {
	minutes := s.CheckinMinutes()
	out := make([]string, 0, len(minutes))
	for _, m := range minutes {
		out = append(out, fmt.Sprintf("%02d:%02d", m/60, m%60))
	}
	return out
}

// KeepaliveHours 当前保活时间（副本）。
func (s *Scheduler) KeepaliveHours() []int {
	_, kh := s.schedule()
	return kh
}

// SetCheckinMinutes 运行中更新签到分钟并唤醒调度循环。
func (s *Scheduler) SetCheckinMinutes(minutes []int) {
	clean := make([]int, 0, len(minutes))
	seen := map[int]bool{}
	for _, m := range minutes {
		if m >= 0 && m < 24*60 && !seen[m] {
			seen[m] = true
			clean = append(clean, m)
		}
	}
	sort.Ints(clean)
	s.mu.Lock()
	s.cfg.CheckinMinutes = clean
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// SetCheckinHours 保留旧 API，将整点小时转换为当天分钟数。
func (s *Scheduler) SetCheckinHours(hours []int) {
	minutes := make([]int, 0, len(hours))
	for _, h := range hours {
		minutes = append(minutes, h*60)
	}
	s.SetCheckinMinutes(minutes)
}

// SetCheckinObserver 设置签到结果观察器；用于把定时任务结果推送到 GUI。
func (s *Scheduler) SetCheckinObserver(fn func(CheckinResult)) {
	s.mu.Lock()
	s.onCheckin = fn
	s.mu.Unlock()
}

func (s *Scheduler) notifyCheckin(r CheckinResult) {
	s.mu.Lock()
	fn := s.onCheckin
	s.mu.Unlock()
	if fn != nil {
		fn(r)
	}
}

// SetRefreshObserver 设置 token 刷新结果观察器。
func (s *Scheduler) SetRefreshObserver(fn func(uid string, ok bool, msg string)) {
	s.mu.Lock()
	s.onRefresh = fn
	s.mu.Unlock()
}

func (s *Scheduler) notifyRefresh(uid string, ok bool, msg string) {
	s.mu.Lock()
	fn := s.onRefresh
	s.mu.Unlock()
	if fn != nil {
		fn(uid, ok, msg)
	}
}

// NextFire 返回最近的一次触发时间（签到 + 保活合并）。
// 签到部分与 Run 循环同源（含窗口重试分钟、排除已完成时段），
// 否则面板显示的「下次签到」会与实际触发时机不一致。
func (s *Scheduler) NextFire() time.Time {
	ch, kh := s.schedule()
	all := append(s.fireMinutes(ch), hoursToMinutes(kh)...)
	return nextFireMinutes(time.Now(), all)
}

func hoursToMinutes(hours []int) []int {
	out := make([]int, 0, len(hours))
	for _, h := range hours {
		out = append(out, h*60)
	}
	return out
}

// nextFireMinutes 返回 now 之后最近的触发时间；输入为当天分钟数。
func nextFireMinutes(now time.Time, minutes []int) time.Time {
	var earliest time.Time
	for _, m := range minutes {
		if m < 0 || m >= 24*60 {
			continue
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), m/60, m%60, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		ch, kh := s.schedule()
		// 该渠道没有任何定时任务（CheckinMinutes/KeepaliveHours 均为显式空切片）：
		// 直接阻塞等取消或配置变更。此时 nextFireMinutes 返回零值，靠下面的
		// IsZero 兜底会变成每分钟唤醒一次的空转。
		if len(ch) == 0 && len(kh) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			}
			continue
		}
		all := append(s.fireMinutes(ch), hoursToMinutes(kh)...)
		next := nextFireMinutes(time.Now(), all)
		if next.IsZero() {
			// 无任何待触发时刻（列表非空但全部越界时才会走到这里）：
			// 兜底睡一分钟，避免零值时间导致 time.NewTimer 立即返回造成忙循环。
			next = time.Now().Add(time.Minute)
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop() // 配置变更，重算
		case <-timer.C:
			now := time.Now()
			minute := now.Hour()*60 + now.Minute()
			if s.checkinDue(ch, minute) {
				// 命中整点 = 该时段起点；否则是窗口内重试，两者都归属同一 slot，
				// 这样整点失败与后续重试共享同一个「当日完成」标记。
				if start, ok := s.retryWindowOpen(minute); ok {
					s.runCheckin(start)
				} else if start, ok := exactSlot(ch, minute); ok {
					s.runCheckin(start)
				} else {
					s.RunCheckinNow()
				}
			}
			if contains(kh, now.Hour()) {
				s.RunKeepaliveNow()
			}
		}
	}
}

// exactSlot 命中配置签到时刻时返回该时刻（分钟数）。
func exactSlot(ch []int, minute int) (int, bool) {
	for _, start := range ch {
		if start == minute {
			return start, true
		}
	}
	return -1, false
}

// fireMinutes 把签到时刻展开为定时器触发分钟列表：
// 配置时刻 + 窗口重试区间内的每一分钟。nextFireMinutes 只能按给定分钟集算最近触发点，
// 若只给配置时刻，窗口内就不会醒过来重试。
// 已完成的时段（当日已领到）不再展开，避免白醒与面板「下次签到」显示成 10:01。
func (s *Scheduler) fireMinutes(ch []int) []int {
	until := s.retryUntil()
	out := make([]int, 0, len(ch))
	for _, start := range ch {
		if until > 0 && s.checkinSlotDone(start) {
			continue // 该时段当日已完成：连同起点一起跳过（下次触发交给明天）
		}
		out = append(out, start)
		for m := start + 1; m <= until && m < 24*60; m++ {
			out = append(out, m)
		}
	}
	return out
}

// checkinDue 判定当前分钟是否应触发签到：
//   - 命中配置的签到时刻（精确相等）；或
//   - 处于某签到时刻的「窗口重试区间」内（CheckinRetryUntil > 0），且该时段尚未完成。
//
// 上游机制：10:00 活动可能尚未创建，若只触发一次就会漏领一天，故在窗口内每分钟重试。
func (s *Scheduler) checkinDue(ch []int, minute int) bool {
	for _, start := range ch {
		if minute == start {
			return true
		}
		// 窗口内重试：严格晚于起点（起点已由上面分支处理），不超过截止时间。
		if s.retryUntil() > 0 && minute > start && minute <= s.retryUntil() && !s.checkinSlotDone(start) {
			return true
		}
	}
	return false
}

// retryUntil 重试截止分钟（0 = 关闭）。
func (s *Scheduler) retryUntil() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.CheckinRetryUntil
}

// checkinSlotDone 该时段当日是否已完成（键为 "YYYY-MM-DD@起始分钟"）。
func (s *Scheduler) checkinSlotDone(startMinute int) bool {
	key := slotKey(time.Now(), startMinute)
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	return s.checkinDone[key]
}

// markSlotDone 标记该时段当日完成；同时清理过期条目避免 map 无界增长。
func (s *Scheduler) markSlotDone(startMinute int) {
	key := slotKey(time.Now(), startMinute)
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	s.checkinDone[key] = true
	prefix := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	for k := range s.checkinDone { // 只保留今天与昨天（跨零点时昨日窗口可能仍在重试）
		if len(k) >= 10 && k[:10] < prefix {
			delete(s.checkinDone, k)
		}
	}
}

func slotKey(now time.Time, startMinute int) string {
	return fmt.Sprintf("%s@%d", now.Format("2006-01-02"), startMinute)
}

// retryWindowOpen 报告当前分钟落在哪个签到时段的窗口重试区间内（无则返回 -1,false）。
// Run 循环用它把「窗口内重试」与「整点首次触发」区分开，以便正确维护完成状态。
func (s *Scheduler) retryWindowOpen(minute int) (int, bool) {
	until := s.retryUntil()
	if until <= 0 {
		return -1, false
	}
	ch, _ := s.schedule()
	for _, start := range ch {
		if minute > start && minute <= until {
			return start, true
		}
	}
	return -1, false
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// CheckinResult 单账号签到结果（GUI 面板展示）。
type CheckinResult struct {
	UID       string `json:"uid"`
	OK        bool   `json:"ok"`
	Msg       string `json:"msg"`
	Remain    int64  `json:"remain"`
	HasRemain bool   `json:"has_remain"`
	// Retryable 报告本次签到未达成（no_campaign/error 等），应在当日窗口内继续重试。
	// 仅由支持结构化结果的渠道（provider.CheckinReporter）给出；其它渠道恒为 false。
	Retryable bool `json:"retryable"`
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻。
// 禁用的账号也签到（停用仅影响 API 路由，签到/保活仍需执行）。
// 不参与「当日完成」判定（面板手动签到/旧 API 入口）。
func (s *Scheduler) RunCheckinNow() {
	s.runCheckin(-1)
}

// runCheckin 签到批次主逻辑；start>=0 时按结果决定是否标记该时段完成。
func (s *Scheduler) runCheckin(start int) {
	name := s.name()
	log.Printf("checkin batch start platform=%s accounts=%d slot=%d", name, len(s.cfg.Pool.List()), start)
	retryable := 0
	for _, st := range s.cfg.Pool.List() {
		r := s.checkinOne(st.UID)
		if r.Retryable {
			retryable++
		}
		log.Printf("checkin result platform=%s uid=%s ok=%t retryable=%t msg=%s remain=%d has_remain=%t", name, st.UID, r.OK, r.Retryable, r.Msg, r.Remain, r.HasRemain)
	}
	if start >= 0 && retryable == 0 {
		s.markSlotDone(start)
		log.Printf("checkin batch done platform=%s slot=%d finished", name, start)
		return
	}
	if start >= 0 {
		log.Printf("checkin batch done platform=%s slot=%d retryable=%d -> retry next tick", name, start, retryable)
		return
	}
	log.Printf("checkin batch done platform=%s", name)
}

// CheckinAccount 单个账号立即签到（停用不受影响，仅路由受限），返回该账号结果。
func (s *Scheduler) CheckinAccount(uid string) (CheckinResult, error) {
	st, ok := s.cfg.Pool.Status(uid)
	if !ok {
		return CheckinResult{}, fmt.Errorf("unknown account %s", uid)
	}
	_ = st // 不论停用与否都签到
	return s.checkinOne(uid), nil
}

// finishCheckin 统一收尾：记录签到状态并上报观察者。
// ActivitiesOnly 渠道跳过这两步（无签到语义，且需对用户透明）。
func (s *Scheduler) finishCheckin(uid string, r CheckinResult) CheckinResult {
	if !s.cfg.ActivitiesOnly {
		s.cfg.Pool.RecordCheckin(uid, r.OK, r.Msg)
		s.notifyCheckin(r)
	}
	return r
}

// checkinOne 单账号签到 + 余额刷新 + 解冻 + 记录签到状态。
func (s *Scheduler) checkinOne(uid string) CheckinResult {
	name := s.name()
	log.Printf("checkin start platform=%s uid=%s", name, uid)
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil {
		return s.finishCheckin(uid, CheckinResult{UID: uid, Msg: "unknown account", Retryable: true})
	}
	// ⚠️「无凭据」判定**不能只看 refresh token**（2026-10-17 修）。
	//
	// loomy / monkeycode / zcode 是**长会话渠道**：凭据本体是 accessToken（monkeycode 的
	// 签到还另用 ConsoleCookie），上游没有 refresh 端点，故 auth 文件里 refreshToken 刻意留空。
	// 旧判据 `a.RefreshToken == ""` 把这类账号一律当 no_token 提前返回，于是：
	//   - 渠道里已实现的签到（monkeycode / loomy）**根本不会被调用** —— 点「签到」只回
	//     「no refresh token」，看着像上游问题，实际是本地守卫把路挡死了（静默失效、无日志）；
	//   - 表现为「功能写了却不生效」，是最难查的一类缺陷。
	// 正确判据：两种凭据都为空，才算真的没有可用凭据。
	if a.RefreshToken == "" && a.AccessToken == "" {
		// 仍标可重试（期间用户重新登录即可恢复），面板会以失败状态显示，不会静默漏领。
		return s.finishCheckin(uid, CheckinResult{UID: uid, Msg: "no credential", Retryable: true})
	}
	// 签到前保证 access token 有效；否则仅依赖晚间 keepalive 时，早上的签到可能拿过期 token。
	// 但**只有存在 refresh token 才谈得上「刷新」**：长会话渠道没有可刷的东西，跳过这一步
	//（其 accessToken 若已过期，由渠道自己的 401 / ErrSessionDead 如实报出，比在这里先报
	// 一个「refresh 失败」更准确）。
	if a.RefreshToken != "" && a.NeedsRefresh(2*time.Hour) {
		if err := s.refreshForCheckin(a, uid); err != nil {
			// 上游把 no_token/刷新失败归为可重试（窗口内再试一次往往就好了）；
			// 只有 session dead 才不再重试——那需要人工重新登录。
			return s.finishCheckin(uid, CheckinResult{UID: uid, Msg: "refresh: " + shortErr(err), Retryable: !isSessionDead(err)})
		}
	}
	r := CheckinResult{UID: uid}
	structured, checkinErr := s.dailyCheckin(a, &r)
	// 签到接口本身就是令牌有效性验证；若返回 session dead，刷新一次后重试整套签到。
	// 要求渠道返回类型化的 provider.Error（见 qodercn/qodercom 的 sessionDead）。
	// sessionDeadSeen 记录「本账号本次 401 自愈也救不回来」：此时才把结果定为终态。
	// 只判 checkinErr 的类型不够——刷新失败后被 fmt.Errorf %w 包成普通错误，
	// 会掉进下方「非 session dead 一律可重试」分支，导致 401 在窗口内被反复重试（无效请求）。
	sessionDeadSeen := false
	if checkinErr != nil && isSessionDead(checkinErr) {
		log.Printf("checkin token invalid platform=%s uid=%s, refreshing and retrying", name, uid)
		if err := s.refreshForCheckin(a, uid); err != nil {
			sessionDeadSeen = true
			checkinErr = fmt.Errorf("登录已失效，请重新登录（refresh failed: %v）", err)
		} else {
			structured, checkinErr = s.dailyCheckin(a, &r)
			sessionDeadSeen = checkinErr != nil && isSessionDead(checkinErr)
		}
	}
	if checkinErr != nil && sessionDeadSeen {
		// 401 是终态（不变式 19：刷新一次仍失效 ⇒ 需人工重新登录）。
		// 不得透出渠道的中间态文案（如「无可用签到活动」），那会把用户引向错误的重试方向。
		log.Printf("checkin failed platform=%s uid=%s err=%v (session dead, terminal)", name, uid, checkinErr)
		r.OK = false
		r.Retryable = false
		r.Msg = shortErr(checkinErr)
	} else if checkinErr != nil {
		log.Printf("checkin failed platform=%s uid=%s err=%v", name, uid, checkinErr)
		// 结构化渠道的失败可能伴随一条有用的渠道文案（如 503 的原始响应体），
		// 未给出时退回错误文本，保证面板永远有可读原因。
		if r.Msg == "" || !structured {
			r.Msg = shortErr(checkinErr)
		}
		if isAlready(checkinErr) {
			r.OK = true // 已签到时视为成功状态
			r.Msg = "已签到"
			r.Retryable = false // 已签到即当日完成，不得继续重试
		} else {
			// 未达成（no_campaign / error 等）：失败且窗口内可重试。
			// OK 必须与 Retryable 取反——两者同时为真会让面板同时显示
			// 「签到成功」与「暂未领到：无可用签到活动」（2026-10-17 现场 toast）。
			r.Retryable = true
			r.OK = false
		}
	} else if !structured {
		// 渠道未给出结构化状态（非 CheckinReporter）：保持旧行为，视为成功。
		r.OK = true
		if r.Msg == "" {
			r.Msg = "ok"
		}
		if s.cfg.ActivitiesOnly {
			r.Msg = "活跃保活"
		}
	}
	// 无论签到成败都查余额（已签到等业务错误下余额刷新仍有效）。
	// 用 Detail 而非 UserResource：一次请求同时拿到可消耗余额与不可消耗额度小计。
	usable, items, rerr := s.cfg.Upstream.UserResourceDetail(a)
	// 401 再刷一次：DailyCheckin 可能因业务错误（已签到/无活动）提前返回而没走到刷新分支，
	// 此时本地 token 可能已失效，不重试就会把余额记成 0。
	if rerr != nil && isSessionDead(rerr) {
		log.Printf("checkin credits token invalid platform=%s uid=%s, refreshing and retrying", name, uid)
		if err := s.refreshForCheckin(a, uid); err == nil {
			usable, items, rerr = s.cfg.Upstream.UserResourceDetail(a)
		}
	}
	if rerr != nil {
		log.Printf("checkin credits failed platform=%s uid=%s err=%v", name, uid, rerr)
		r.OK = false // 签到后的积分确认失败，整次操作向 GUI 报告失败
		if r.Msg == "" {
			r.Msg = "余额查询失败"
		} else {
			r.Msg += "；余额查询失败"
		}
	} else {
		_, unusable := provider.Summarize(items)
		// 临期额度：自然日口径（与面板三列、pool 路由同一判据）。
		expiring := provider.ExpiringWithinDays(items, s.cfg.expiringDays(), time.Now())
		r.Remain, r.HasRemain = usable, true
		log.Printf("checkin credits platform=%s uid=%s remain=%d expiring=%d unusable=%d", name, uid, usable, expiring, unusable)
		s.cfg.Pool.ReenableIfCredits(uid, usable, expiring, unusable)
		// 差分记账：与上次快照对比产出 earn/spend/expire 流水
		if s.cfg.Ledger != nil {
			s.cfg.Ledger.DiffCredits(name, uid, usable, items)
		}
	}
	return s.finishCheckin(uid, r)
}

// dailyCheckin 执行一次签到并回填结果。
// 优先走 provider.CheckinReporter（结构化状态，可区分 no_campaign 与 error）；
// 未实现的渠道回退 DailyCheckin，按「已成功」处理（旧行为）。
// structured 报告渠道是否给出了结构化结果（决定调用方能否信任 r.OK 的 false 值）。
// 结构化渠道的 OK 语义：claimed/already 为成功；no_campaign/no_token/error 为未达成。
func (s *Scheduler) dailyCheckin(a *auth.Auth, r *CheckinResult) (structured bool, err error) {
	rep, ok := s.cfg.Upstream.(provider.CheckinReporter)
	if !ok {
		// 回执自带发放额的渠道（workbuddy，issue #67）：走 Grant 变体执行签到并拿权威发放额，
		// 直接记 earn——这是唯一执行路径，不会重复请求（勿在此之后再调 DailyCheckin）。
		if g, isG := s.cfg.Upstream.(provider.CheckinGranter); isG {
			granted, gerr := g.DailyCheckinGrant(a)
			if gerr == nil && granted > 0 && s.cfg.Ledger != nil {
				note := "签到奖励"
				if s.cfg.ActivitiesOnly {
					note = "活跃保活奖励"
				}
				if s.cfg.Ledger.RecordCheckinEarn(s.name(), a.UID, granted, r.Remain, note) {
					log.Printf("checkin earn recorded platform=%s uid=%s amount=%d (来自签到回执)", s.name(), a.UID, granted)
				}
			}
			return false, gerr
		}
		return false, s.cfg.Upstream.DailyCheckin(a)
	}
	report, err := rep.DailyCheckinReport(a)
	r.OK = !report.Status.Retryable()
	r.Retryable = report.Status.Retryable()
	if report.Msg != "" {
		r.Msg = report.Msg
	}
	// 结构化渠道也可能在回执里给出权威发放额（如 WorkBuddy 的 CheckinReporter 实现，
	// issue #147）：与 CheckinGranter 同口径记 earn——否则 issue #67 的「发放即消耗」
	// 差分归零问题会在 reporter 路径上重现（同一渠道两条路径不能不一致）。
	if report.Status == provider.CheckinClaimed && report.Amount > 0 && s.cfg.Ledger != nil {
		note := "签到奖励"
		if s.cfg.ActivitiesOnly {
			note = "活跃保活奖励"
		}
		if s.cfg.Ledger.RecordCheckinEarn(s.name(), a.UID, report.Amount, r.Remain, note) {
			log.Printf("checkin earn recorded platform=%s uid=%s amount=%d (来自结构化回执)", s.name(), a.UID, report.Amount)
		}
	}
	return true, err
}

// isAlready 只匹配明确的“今日已签到”，不能因错误文本包含 checkin 就判成功。
func isAlready(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "已签到") ||
		strings.Contains(s, "already check") ||
		strings.Contains(s, "already checked") ||
		strings.Contains(s, "code=9095")
}

func shortErr(err error) string {
	s := strings.TrimSpace(err.Error())
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

func (s *Scheduler) name() string {
	if s.cfg.Name != "" {
		return s.cfg.Name
	}
	return "unknown"
}

func (s *Scheduler) refreshForCheckin(a *auth.Auth, uid string) error {
	name := s.name()
	log.Printf("refresh start platform=%s uid=%s reason=checkin", name, uid)
	if s.refreshOverride != nil { // 仅测试注入
		return s.refreshOverride(a, uid)
	}
	// 单飞：签到与请求路径/保活/credit 循环并发，同账号并发刷新会互相作废 refresh_token。
	if err := provider.RefreshOnce(a, func() error {
		if rerr := s.cfg.Upstream.RefreshToken(a); rerr != nil {
			return rerr
		}
		if serr := a.SaveAtomic(); serr != nil {
			log.Printf("refresh save failed platform=%s uid=%s err=%v", name, uid, serr)
			return fmt.Errorf("refresh save: %w", serr)
		}
		return nil
	}); err != nil {
		log.Printf("refresh failed platform=%s uid=%s err=%v", name, uid, err)
		return err
	}
	log.Printf("refresh success platform=%s uid=%s expires_at=%d", name, uid, a.ExpiresAt)
	return nil
}

func isSessionDead(err error) bool {
	var ue *provider.Error
	return errors.As(err, &ue) && ue.Kind == provider.ErrSessionDead
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
// 禁用的账号也保活（停用仅影响 API 路由）。
func (s *Scheduler) RunKeepaliveNow() {
	name := s.name()
	log.Printf("refresh batch start platform=%s", name)
	for _, st := range s.cfg.Pool.List() {
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			msg := "no refresh token"
			log.Printf("refresh skip platform=%s uid=%s reason=%s", name, st.UID, msg)
			s.notifyRefresh(st.UID, false, msg)
			continue
		}
		log.Printf("refresh start platform=%s uid=%s reason=keepalive", name, st.UID)
		// 单飞：保活与请求路径/签到/credit 循环并发，同账号并发刷新会互相作废 refresh_token。
		// saveErr 由真正执行刷新的那个 goroutine 写入；等待者不执行 fn，其 saveErr 保持 nil
		// 是正确语义 —— 落盘已由执行者完成。
		var saveErr error
		if err := provider.RefreshOnce(a, func() error {
			if rerr := s.cfg.Upstream.RefreshToken(a); rerr != nil {
				return rerr
			}
			saveErr = a.SaveAtomic()
			return nil
		}); err != nil {
			log.Printf("refresh failed platform=%s uid=%s err=%v", name, st.UID, err)
			var ue *provider.Error
			if errors.As(err, &ue) && ue.Kind == provider.ErrSessionDead {
				s.cfg.Pool.Disable(st.UID, "12153 session dead")
				log.Printf("refresh disabled platform=%s uid=%s reason=session_dead", name, st.UID)
			}
			s.notifyRefresh(st.UID, false, err.Error())
			continue
		}
		if saveErr != nil {
			log.Printf("refresh save failed platform=%s uid=%s err=%v", name, st.UID, saveErr)
			s.notifyRefresh(st.UID, false, "refresh save: "+saveErr.Error())
			continue
		}
		log.Printf("refresh success platform=%s uid=%s expires_at=%d", name, st.UID, a.ExpiresAt)
		s.notifyRefresh(st.UID, true, "ok")
	}
	log.Printf("refresh batch done platform=%s", name)
}
