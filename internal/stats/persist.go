// 落盘层：data/stats.json（tmp+rename 原子写）。
// 持久内容=跨天历史（token/req/模型按日归档）+ 当日基线（差值 credit_out 与观测
// 起点 burn_since 必须同窗持久化——只存 burn_since 的话，重启后分母是旧值、分子归零，
// burn 外推会系统性低估）+ 今日签到事件收入（重扫日志幂等，双保险）。
// 今日全局 tokens/reqs/exact 不单独落盘，由 today_models 求和重建（AddUsage 是
// 四者唯一写入方，求和与逐项累计天然相等）。
package stats

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// persistJSON stats.json 落盘结构。
type persistJSON struct {
	TokenDays   map[string]int64                   `json:"token_days"`             // YYYY-MM-DD -> tokens（不含今日）
	ReqDays     map[string]int64                   `json:"req_days"`               // YYYY-MM-DD -> reqs（不含今日）
	Accts       map[string]acctPersist             `json:"accts"`                  // 当日差值基线
	Events      map[string]int64                   `json:"events,omitempty"`       // 今日签到事件收入 uid|day -> 数额
	ModelDays   map[string]map[string]modelPersist `json:"model_days,omitempty"`   // 历史日 -> 模型 -> 消耗（跨天归档）
	TodayModels *dayModelsPersist                  `json:"today_models,omitempty"` // 今日模型消耗（day 守卫，重启恢复）
}

// modelPersist 单模型消耗落盘形态（与内存 modelEntry 同口径，字段一一对应）。
// 性能四项缺省为零值 → 均值退化为「无样本」，前端显示 "—"，重启后新一轮请求
// 自然重新采样（丢弃历史样本比显示半截均值更诚实）。
// 缓存三项（Cr/CrCov/CrReqs）必须持久化：曾遗漏导致每次重启丢失今日缓存样本，
// 面板「缓存」列大面积回退成「—」（2026-10-13 线上实测：workbuddyai 98%→—）。
type modelPersist struct {
	Reqs      int64   `json:"reqs"`
	Tokens    int64   `json:"tokens"`
	Credit    float64 `json:"credit"`
	Cr        int64   `json:"cr,omitempty"`
	CrCov     int64   `json:"cr_cov,omitempty"`
	CrReqs    int64   `json:"cr_reqs,omitempty"`
	TTFBSumMs int64   `json:"ttfb_sum_ms,omitempty"`
	PerfCnt   int64   `json:"perf_cnt,omitempty"`
	GenTokens int64   `json:"gen_tokens,omitempty"`
	GenSec    float64 `json:"gen_sec,omitempty"`
}

// toEntry 落盘形态 → 内存形态。
func (mp modelPersist) toEntry() *modelEntry {
	return &modelEntry{
		reqs: mp.Reqs, tokens: mp.Tokens, credit: mp.Credit,
		cr: mp.Cr, crCov: mp.CrCov, crReqs: mp.CrReqs,
		ttfbSumMs: mp.TTFBSumMs, perfCnt: mp.PerfCnt,
		genTokens: mp.GenTokens, genSec: mp.GenSec,
	}
}

// fromEntry 内存形态 → 落盘形态。
func fromEntry(e *modelEntry) modelPersist {
	return modelPersist{
		Reqs: e.reqs, Tokens: e.tokens, Credit: e.credit,
		Cr: e.cr, CrCov: e.crCov, CrReqs: e.crReqs,
		TTFBSumMs: e.ttfbSumMs, PerfCnt: e.perfCnt,
		GenTokens: e.genTokens, GenSec: e.genSec,
	}
}

// cloneEntry 复制一份（跨天归档用，避免与今日 map 共享指针）。
func cloneEntry(e *modelEntry) *modelEntry {
	c := *e
	return &c
}

// dayModelsPersist 今日模型消耗落盘（day 守卫对齐 acctPersist：跨天/换日启动整块失效）。
type dayModelsPersist struct {
	Day    string                  `json:"day"`
	Models map[string]modelPersist `json:"models"`
}

// acctPersist 单账号当日差值基线。
type acctPersist struct {
	Day       string `json:"day"`        // 记账日，跨天/换日启动时整块失效
	BurnSince string `json:"burn_since"` // RFC3339
	CreditOut int64  `json:"credit_out"`
}

// load 读取并恢复持久数据（文件缺失=首启，零值起步）。
func (s *Stats) load() error {
	raw, err := os.ReadFile(s.file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("读取 %s: %w", s.file, err)
	}
	// 容忍 UTF-8 BOM：遇 BOM 直接 Unmarshal 会报 invalid character 'ï'，
	// 且会误走「旧格式迁移」分支把 accts 全丢（实测踩坑）
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	var p persistJSON
	if err := json.Unmarshal(raw, &p); err != nil {
		// 兼容旧格式（accts 曾为 uid->burnSince 字符串 map）：丢弃 accts 段保留历史，
		// 一次落盘后即转为新格式。accts 只是当日基线，丢失后首轮喂入自动重建。
		var legacyOnly struct {
			TokenDays map[string]int64 `json:"token_days"`
			ReqDays   map[string]int64 `json:"req_days"`
		}
		if err2 := json.Unmarshal(raw, &legacyOnly); err2 != nil {
			return fmt.Errorf("解析 %s: %w", s.file, err)
		}
		p.TokenDays, p.ReqDays, p.Accts = legacyOnly.TokenDays, legacyOnly.ReqDays, nil
	}
	for day, v := range p.TokenDays {
		s.tokenDays[day] = v
	}
	for day, v := range p.ReqDays {
		s.reqDays[day] = v
	}
	for uid, ap := range p.Accts {
		if t, perr := time.Parse(time.RFC3339, ap.BurnSince); perr == nil && ap.Day == s.today && !t.IsZero() {
			s.accts[uid] = &acct{uid: uid, burnSince: t, creditOut: ap.CreditOut, hasLast: false}
			s.creditOutTotal += ap.CreditOut // 首轮喂入前 Snapshot 也口径一致
		}
	}
	// 恢复今日签到事件收入（键含日期后缀，跨天残留键丢弃）；全局收入合计据此重建。
	for k, v := range p.Events {
		if i := strings.LastIndexByte(k, '|'); i >= 0 && k[i+1:] == s.today {
			s.events[k] = v
			s.creditInTotal += v
		}
	}
	// 历史每日模型消耗（跨天归档）
	for day, mm := range p.ModelDays {
		m2 := make(map[string]*modelEntry, len(mm))
		for name, mp := range mm {
			m2[name] = mp.toEntry()
		}
		s.modelDays[day] = m2
	}
	// 恢复今日模型消耗（day 守卫：跨天残留丢弃）；今日全局精确计数从 per-model
	// 求和重建。
	if p.TodayModels != nil && p.TodayModels.Day == s.today {
		for name, mp := range p.TodayModels.Models {
			s.models[name] = mp.toEntry()
			s.tokensToday += mp.Tokens
			s.reqsToday += mp.Reqs
			s.creditOutExact += mp.Credit
		}
	}
	return nil
}

// flushLocked 落盘（tmp+rename 原子写；调用方须持锁）。
// 失败时保留 dirty 标记（下轮重试）并经 logf 上报——静默清 dirty 会让
// 停机前的最后一次 flush 也被跳过（Windows 上文件被占用时 rename 会失败）。
func (s *Stats) flushLocked() {
	p := persistJSON{TokenDays: s.tokenDays, ReqDays: s.reqDays, Accts: map[string]acctPersist{}, Events: s.events}
	// 今日模型消耗（day 守卫随存随恢复）
	if len(s.models) > 0 {
		tm := make(map[string]modelPersist, len(s.models))
		for name, e := range s.models {
			tm[name] = fromEntry(e)
		}
		p.TodayModels = &dayModelsPersist{Day: s.today, Models: tm}
	}
	// 历史每日模型消耗
	if len(s.modelDays) > 0 {
		md := make(map[string]map[string]modelPersist, len(s.modelDays))
		for day, mm := range s.modelDays {
			pm := make(map[string]modelPersist, len(mm))
			for name, e := range mm {
				pm[name] = fromEntry(e)
			}
			md[day] = pm
		}
		p.ModelDays = md
	}
	for uid, a := range s.accts {
		if !a.burnSince.IsZero() {
			p.Accts[uid] = acctPersist{
				Day:       s.today,
				BurnSince: a.burnSince.Format(time.RFC3339),
				CreditOut: a.creditOut,
			}
		}
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		s.logf("stats 落盘 marshal 失败: %v", err)
		return
	}
	tmp := s.file + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		s.logf("stats 落盘创建临时文件失败: %v", err)
		return
	}
	if _, werr := f.Write(raw); werr != nil {
		_ = f.Close()
		s.logf("stats 落盘写入失败: %v", werr)
		return
	}
	// 先 Sync 再 rename：rename 只保证文件名原子，不保证内容已持久；断电时防半截文件。
	if serr := f.Sync(); serr != nil {
		s.logf("stats 落盘 sync 失败: %v", serr)
	}
	_ = f.Close()
	if rerr := os.Rename(tmp, s.file); rerr != nil {
		s.logf("stats 落盘 rename 失败: %v", rerr)
		return
	}
	s.dirty = false
}

// FlushNow 同步落盘（有脏数据时）。进程主动退出路径必须用它，不能依赖
// RunFlusher 的 stop 异步落盘——os.Exit 会抢在另一 goroutine 完成前终止。
func (s *Stats) FlushNow() {
	s.mu.Lock()
	if s.dirty {
		s.flushLocked()
	}
	s.mu.Unlock()
}

// RunFlusher 周期检查 dirty 并落盘；stop 关闭时做最后一次 flush 后返回。
func (s *Stats) RunFlusher(stop <-chan struct{}) {
	tk := time.NewTicker(5 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-stop:
			s.mu.Lock()
			if s.dirty {
				s.flushLocked()
			}
			s.mu.Unlock()
			return
		case <-tk.C:
			s.mu.Lock()
			if s.dirty {
				s.flushLocked()
			}
			s.mu.Unlock()
		}
	}
}
