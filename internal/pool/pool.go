// Package pool 账号池：内存索引 + 冷却/禁用状态机 + state.json 持久化。
// 挑选策略：healthy 账号中临期积分多者优先，同组内按剩余积分降序。
// 冷却分两层：账号级（entry.until，token/session/余额等与模型无关的惩罚）与
// 模型级（entry.modelUntil，上游按模型独立限流，见 CooldownModel）。
package pool

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolHard       CoolKind = iota // 余额不足 → 长冷却
	CoolSoft                       // 429 → 短冷却
	CoolErr                        // 连续错误 → 中冷却
	CoolLowBalance                 // 余额低于平均 60% → 15min 冷却
)

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	case CoolErr:
		return "error_threshold"
	case CoolLowBalance:
		return "low_balance"
	}
	return "unknown"
}

// ModelCoolStatus 单条模型级冷却（面板展示用）。
// 上游对模型独立计费与限流（WorkBuddy 系 6004 频率限流原文即明说「可换其他模型继续用」），
// 故冷却按 (账号, 模型) 记录：该账号在 Model 上暂不可选，其他模型不受影响。
type ModelCoolStatus struct {
	Model  string    `json:"model"`
	Until  time.Time `json:"until"`
	Reason string    `json:"reason,omitempty"`
}

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	// Credits 本工具可消耗的积分余额（pool 路由依据）。
	Credits int64 `json:"credits"`
	// ExpiringCredits 可消耗余额中 24h 内（实际口径：到期日≤明天）到期的部分。
	// Pick() 先按临期优先消耗快过期的积分，再按总可用余额排序；仅面板展示。
	// 0 表示无临期或渠道不区分（如 Qoder 不下发到期时间）。
	ExpiringCredits int64 `json:"expiring_credits,omitempty"`
	// UnusableCredits 账号名下有、但本工具用不了的积分（如 TraeWork 的 ep=1 专用池）。
	// 仅用于面板展示，不参与路由；0 表示该渠道不区分或没有此类额度。
	UnusableCredits int64 `json:"unusable_credits,omitempty"`
	// CreditsStale 标记余额口径不可信：state 文件是旧版本格式（无 unusable 字段）
	// 或尚未完成首次成功刷新。UI 应显示「待刷新」而非把旧值/0 当真值。
	// 自动刷新循环首次成功写入后即清除。
	CreditsStale bool      `json:"credits_stale,omitempty"`
	Cooling      bool      `json:"cooling"`
	Until        time.Time `json:"until,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	// ModelCooling 该账号上处于冷却的模型（上游按模型独立限流）。
	// 与 Cooling/Reason 分开：非空不代表账号整体不可用，其他模型仍可路由，
	// 合并展示会让用户误以为整个账号被限流。
	ModelCooling   []ModelCoolStatus `json:"model_cooling,omitempty"`
	Disabled       bool              `json:"disabled"`
	ErrCount       int               `json:"err_count,omitempty"`
	LastCheckinOK  bool              `json:"last_checkin_ok,omitempty"`
	LastCheckinAt  time.Time         `json:"last_checkin_at,omitempty"`
	LastCheckinMsg string            `json:"last_checkin_msg,omitempty"`
	// SessionDead 登录态失效（401/登录已失效）：挑号跳过 + 面板「需重新登录」徽标（#134）。
	// 与 Disabled（用户手动停用）分离，避免机器状态混入用户控制项。
	SessionDead       bool      `json:"session_dead,omitempty"`
	SessionDeadReason string    `json:"session_dead_reason,omitempty"`
	SessionDeadAt     time.Time `json:"session_dead_at,omitempty"`
}

// modelCool 模型级冷却记录：该账号在单个模型上的限流/不可用。
type modelCool struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason,omitempty"`
	Kind   CoolKind  `json:"kind,omitempty"`
}

type entry struct {
	a        *auth.Auth
	credits  int64
	expiring int64 // 临期（快到期）的可消耗积分，Pick() 第一排序键
	unusable int64
	// creditsStale 余额口径不可信（旧版 state 或从未成功刷新过）。
	// 仅影响面板展示，不参与 Pick() 路由。
	creditsStale bool
	disabled     bool
	reason       string
	until        time.Time
	errCount     int
	// modelUntil 模型级冷却：模型名（去渠道前缀后、与出站 body 的 model 同值）→ 该模型上的冷却。
	// 与 until 的分工见 CooldownModel；nil 表示当前无任何模型级冷却。
	modelUntil map[string]modelCool

	lastCheckinOK  bool
	lastCheckinAt  time.Time
	lastCheckinMsg string
	// sessionDead 登录态失效（401/登录已失效），与 disabled 分离（#134）。
	sessionDead       bool
	sessionDeadReason string
	sessionDeadAt     time.Time
}

// healthy 判定账号在给定模型下当前是否可用。
// model == "" 表示只判账号级状态（拉模型目录、查余额等与具体模型无关的调用）。
func (e *entry) healthy(now time.Time, model string) bool {
	if e.disabled || e.sessionDead {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	if model != "" {
		// 读 nil map 安全：无模型级冷却时直接返回零值。
		if mc, ok := e.modelUntil[model]; ok && now.Before(mc.Until) {
			return false
		}
	}
	return true
}

// stateFile 持久化格式。
// version 用于识别旧版本状态文件：v2.2.0 及之前无 unusable 字段，
// 读入后这些账号的余额口径不可信（pool 会置 creditsStale）。
// 缺失/零值视为 1（保持向后兼容：老文件不报错）。
type stateFile struct {
	Version  int                     `json:"version,omitempty"`
	Accounts map[string]accountState `json:"accounts"`
}

// stateVersion 当前状态文件格式版本。
// v2: unusable（可用/不可用拆分）；v3: expiring（临期额度）；v4: model_until（模型级冷却）；
// v5: session_dead（登录态失效标记，纯新增可选字段，不改变余额口径）。
const stateVersion = 5

// creditsLayoutVersion 余额口径版本：v3 起含 expiring（临期）字段，口径与更早版本不同。
// 只有低于此版本的文件才置 creditsStale —— v4 仅新增 model_until（模型级冷却），
// 不改变余额口径，旧文件读入后余额仍可信，不该白触发一次「待刷新」。
const creditsLayoutVersion = 3

type accountState struct {
	Credits           int64                `json:"credits"`
	Expiring          int64                `json:"expiring,omitempty"`
	Unusable          int64                `json:"unusable,omitempty"`
	ModelUntil        map[string]modelCool `json:"model_until,omitempty"`
	Disabled          bool                 `json:"disabled"`
	Reason            string               `json:"reason,omitempty"`
	Until             time.Time            `json:"until,omitempty"`
	LastCheckinOK     bool                 `json:"last_checkin_ok,omitempty"`
	LastCheckinAt     time.Time            `json:"last_checkin_at,omitempty"`
	LastCheckinMsg    string               `json:"last_checkin_msg,omitempty"`
	SessionDead       bool                 `json:"session_dead,omitempty"`
	SessionDeadReason string               `json:"session_dead_reason,omitempty"`
	SessionDeadAt     time.Time            `json:"session_dead_at,omitempty"`
}

// Pool 账号池。
type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
}

// New 构建池；stateFp 非空时尝试加载旧状态。
func New(stateFp string) *Pool {
	p := &Pool{byUID: map[string]*entry{}, stateFp: stateFp}
	if stateFp != "" {
		p.load()
	}
	return p
}

// Add 加入账号；已存在则保留原状态、更新凭证。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range auths {
		seen[a.UID] = true
		if e, ok := p.byUID[a.UID]; ok {
			e.a = a
		} else {
			p.byUID[a.UID] = &entry{a: a}
		}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
	p.clearReloggedLocked(auths)
}

// clearReloggedLocked 重新登录后（auth 文件被登录流程重写）清除 SessionDead 标记。
// 必要性（#134）：旧版对 session dead 走 Disable，标记被持久化到 state 文件；
// 用户重新登录、RefreshToken 也刷成功了，但池里只换 e.a，标记仍在 ⇒ 账号永不被挑中。
// 判据：auth 文件修改时间晚于「标记建立时间」。调用方须持有 p.mu。
func (p *Pool) clearReloggedLocked(auths []*auth.Auth) {
	for _, a := range auths {
		e, ok := p.byUID[a.UID]
		if !ok || !e.sessionDead {
			continue
		}
		// FilePath 为空（内存构造的凭证）无从判断，保守保留标记。
		if a.FilePath == "" {
			continue
		}
		st, err := os.Stat(a.FilePath)
		if err != nil {
			continue
		}
		if st.ModTime().After(e.sessionDeadAt) {
			e.sessionDead = false
			e.sessionDeadReason = ""
			e.sessionDeadAt = time.Time{}
			log.Printf("session dead cleared by relogin uid=%s", a.UID)
		}
	}
}

// Pick 返回 healthy 中积分最高的账号；无可用返回 nil。
// model 非空时同时排除在该模型上处于冷却的账号（模型级冷却）；传 "" 表示只按
// 账号级状态挑选（拉模型目录、查余额等与具体模型无关的调用）。
// 排序两级：先按临期额度（消耗快过期的），同组内再按总可用余额。
// 临期与总可用均为 0 视为无额度，排序仍正确。
func (p *Pool) Pick(model string) *auth.Auth {
	return p.PickExcluding(nil, model)
}

// PickExcluding 同上，但跳过 tried 中的 uid（请求级轮换）。
// 比较键：expiring 降序 → credits 降序。临期>0 的账号恒排在临期=0 之前，
// 即使后者总余额更高——目的是优先烧掉快过期的积分，避免浪费。
// expiring 仅统计可消耗额度，且 expiring ≤ credits，不会出现虚高。
func (p *Pool) PickExcluding(tried map[string]bool, model string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	var best *entry
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now, model) {
			continue
		}
		if best == nil || entryBetter(e, best) {
			best = e
		}
	}
	if best == nil {
		return nil
	}
	return best.a
}

// entryBetter e 是否应排在 b 之前（路由优先级更高）：临期降序 → 总可用降序。
func entryBetter(e, b *entry) bool {
	if e.expiring != b.expiring {
		return e.expiring > b.expiring
	}
	return e.credits > b.credits
}

// SetCreditDetail 更新账号积分：usable 为可消耗余额，expiring 为其中临期部分，
// unusable 为账号名下有但本工具用不了的额度（如 TraeWork 的 ep=1 专用池），仅面板展示。
// usable+expiring 是 Pick() 的排序依据。写入即视为余额口径可信，清除 creditsStale。
func (p *Pool) SetCreditDetail(uid string, usable, expiring, unusable int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = usable
		e.expiring = expiring
		e.unusable = unusable
		e.creditsStale = false
	}
	p.saveLocked()
}

// Cooldown 账号级冷却至 now+d：与具体模型无关的惩罚（token/session 故障、
// 余额耗尽、WAF 拦截、账号级风控等）。会让账号在所有模型上不可选。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.reason = reason
		e.errCount = 0
	}
	p.saveLocked()
}

// CooldownModel 模型级冷却：只让该账号在 model 上不可选，同账号的其他模型不受影响。
// 用于上游按模型独立限流的场景（WorkBuddy 系 429/6004 原文即声明「可换其他模型继续用」），
// 避免一个模型撞到上限把账号上仍可用的模型一起连坐。
//
// 刻意不写 e.reason：面板上「账号被限流」会让人误以为整个账号不可用；模型级冷却
// 单独经 Status.ModelCooling 暴露。model 为空时退化为账号级冷却。
// 写入时顺带清理已过期条目，避免 map 随请求过的模型数无限增长。
func (p *Pool) CooldownModel(uid, model string, kind CoolKind, d time.Duration, reason string) {
	if model == "" {
		p.Cooldown(uid, kind, d, reason)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if e.modelUntil == nil {
			e.modelUntil = map[string]modelCool{}
		}
		now := time.Now()
		for m, mc := range e.modelUntil {
			if !now.Before(mc.Until) {
				delete(e.modelUntil, m)
			}
		}
		e.modelUntil[model] = modelCool{Until: now.Add(d), Reason: reason, Kind: kind}
	}
	p.saveLocked()
}

// ClearPenalty 清除账号的冷却与错误计数（不动 disabled，停用由用户控制）。
// 用途：单账号渠道（oczen）在启动时自愈历史脏数据——旧版本会把唯一账号
// 因网络抖动/429 冷却，而该渠道无号可轮换，冷却即等于整条渠道下线。
func (p *Pool) ClearPenalty(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		if e.until.IsZero() && e.errCount == 0 && len(e.modelUntil) == 0 {
			return // 无惩罚可清：不触发无意义的落盘
		}
		e.until = time.Time{}
		e.errCount = 0
		e.modelUntil = nil
		if !e.disabled {
			e.reason = ""
		}
	}
	p.saveLocked()
}

// Disable 永久禁用（session 死亡），需人工重登后手工恢复或文件替换。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
	}
	p.saveLocked()
}

// SetDisabled 设置账号禁用/启用状态。
func (p *Pool) SetDisabled(uid string, d bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = d
		if d {
			e.reason = "手动停用"
		} else {
			e.reason = ""
		}
	}
	p.saveLocked()
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号处于冷却（非禁用）时恢复。
// expiring 为临期额度小计，unusable 为不可消耗额度小计（仅面板展示）。写入即视为余额口径可信。
func (p *Pool) ReenableIfCredits(uid string, remain, expiring, unusable int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = remain
		e.expiring = expiring
		e.unusable = unusable
		e.creditsStale = false
		if remain > 0 && !e.disabled {
			// 签到解冻：账号级与模型级冷却一并清除。模型级冷却若残留，该账号会
			// 在特定模型上继续不可选，而签到成功已证明账号整体可用。
			e.until = time.Time{}
			e.reason = ""
			e.errCount = 0
			e.modelUntil = nil
		}
	}
	p.saveLocked()
}

// RecordCheckin 记录一次签到结果（含错误信息），随 state.json 持久化。
func (p *Pool) RecordCheckin(uid string, ok bool, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.lastCheckinOK = ok
		e.lastCheckinAt = time.Now()
		e.lastCheckinMsg = msg
		if ok {
			// 签到成功 = 登录态有效：清除失效标记（#134）
			e.sessionDead = false
			e.sessionDeadReason = ""
			e.sessionDeadAt = time.Time{}
		} else if sessionDeadLike(msg) {
			// 401/登录失效形态：标记待重登（面板徽标 + 重新登录入口）
			e.sessionDead = true
			e.sessionDeadReason = msg
			e.sessionDeadAt = time.Now()
		}
	}
	p.saveLocked()
}

// sessionDeadLike 判断签到失败信息是否为登录态失效形态（401/登录已失效/网关 401 页）。
func sessionDeadLike(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "401") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(msg, "登录已失效") ||
		strings.Contains(msg, "登录已过期")
}

// MarkSessionDead 标记账号登录态失效（401/登录已失效）：挑号阶段跳过、面板显示
// 「需重新登录」徽标与一键重新登录入口（#134）。与 Disable（用户手动停用）分离，
// 避免机器状态混入用户控制项后用户看不出该做什么。
func (p *Pool) MarkSessionDead(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || e.sessionDead {
		return
	}
	e.sessionDead = true
	e.sessionDeadReason = reason
	e.sessionDeadAt = time.Now()
	p.saveLocked()
	log.Printf("session dead marked uid=%s reason=%s", uid, reason)
}

// ClearSessionDead 清除登录态失效标记（重新登录成功/签到或请求成功后回到可用态）。
func (p *Pool) ClearSessionDead(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok && e.sessionDead {
		e.sessionDead = false
		e.sessionDeadReason = ""
		e.sessionDeadAt = time.Time{}
		p.saveLocked()
		log.Printf("session dead cleared uid=%s", uid)
	}
}

// Remove 从池中移除账号（内存 + 状态文件）；auth 文件删除由调用方负责。
func (p *Pool) Remove(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.byUID, uid)
	p.saveLocked()
}

// NoteError 记录一次非余额/非 429 错误；达到 threshold 自动冷却 d 时长。
func (p *Pool) NoteError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount++
		if e.errCount >= threshold {
			e.until = time.Now().Add(d)
			e.reason = "consecutive errors"
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteSuccess 成功请求重置错误计数。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount = 0
	}
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	return Status{
		UID:               uid,
		Nickname:          e.a.Nickname,
		Credits:           e.credits,
		ExpiringCredits:   e.expiring,
		UnusableCredits:   e.unusable,
		CreditsStale:      e.creditsStale,
		Cooling:           !e.until.IsZero() && now.Before(e.until),
		Until:             e.until,
		Reason:            e.reason,
		ModelCooling:      modelCoolingOf(e, now),
		Disabled:          e.disabled,
		ErrCount:          e.errCount,
		LastCheckinOK:     e.lastCheckinOK,
		LastCheckinAt:     e.lastCheckinAt,
		LastCheckinMsg:    e.lastCheckinMsg,
		SessionDead:       e.sessionDead,
		SessionDeadReason: e.sessionDeadReason,
		SessionDeadAt:     e.sessionDeadAt,
	}
}

// modelCoolingOf 汇总该账号当前仍生效的模型级冷却（按模型名排序，输出稳定）。
func modelCoolingOf(e *entry, now time.Time) []ModelCoolStatus {
	if len(e.modelUntil) == 0 {
		return nil
	}
	out := make([]ModelCoolStatus, 0, len(e.modelUntil))
	for m, mc := range e.modelUntil {
		if now.Before(mc.Until) {
			out = append(out, ModelCoolStatus{Model: m, Until: mc.Until, Reason: mc.Reason})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	// 旧格式（v2.2.0 及之前无 unusable；v2 无 expiring）：余额口径与新版不一致（可用/不可用/临期拆分），
	// 读入后置 creditsStale，待自动刷新循环首刷时重算并清除。
	// 判据是 creditsLayoutVersion 而非 stateVersion：v4 只加了 model_until，余额口径没变。
	stale := sf.Version < creditsLayoutVersion
	for uid, s := range sf.Accounts {
		p.byUID[uid] = &entry{
			a:                 &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:           s.Credits,
			expiring:          s.Expiring,
			unusable:          s.Unusable,
			creditsStale:      stale,
			disabled:          s.Disabled,
			reason:            s.Reason,
			until:             s.Until,
			modelUntil:        s.ModelUntil,
			lastCheckinOK:     s.LastCheckinOK,
			lastCheckinAt:     s.LastCheckinAt,
			lastCheckinMsg:    s.LastCheckinMsg,
			sessionDead:       s.SessionDead,
			sessionDeadReason: s.SessionDeadReason,
			sessionDeadAt:     s.SessionDeadAt,
		}
	}
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Version: stateVersion, Accounts: map[string]accountState{}}
	for uid, e := range p.byUID {
		sf.Accounts[uid] = accountState{
			Credits:           e.credits,
			Expiring:          e.expiring,
			Unusable:          e.unusable,
			ModelUntil:        e.modelUntil,
			Disabled:          e.disabled,
			Reason:            e.reason,
			Until:             e.until,
			LastCheckinOK:     e.lastCheckinOK,
			LastCheckinAt:     e.lastCheckinAt,
			LastCheckinMsg:    e.lastCheckinMsg,
			SessionDead:       e.sessionDead,
			SessionDeadReason: e.sessionDeadReason,
			SessionDeadAt:     e.sessionDeadAt,
		}
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.stateFp)
}
