// Package scheduler 定时任务：每日签到 + token keepalive。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
// 签到时间支持运行中更新（SetCheckinHours），由 GUI 面板写入 config.json 后调用。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/upstream"
)

// Config 调度器依赖。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	KeepaliveHours []int // 默认 [22]
}

// Scheduler 调度器。
type Scheduler struct {
	mu   sync.Mutex // 保护 cfg 中的小时配置
	cfg  Config
	wake chan struct{} // 配置变更唤醒 Run 循环重算下次触发
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	return &Scheduler{cfg: cfg, wake: make(chan struct{}, 1)}
}

// hours 返回当前签到/保活小时配置的副本。
func (s *Scheduler) hours() (checkin, keepalive []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int{}, s.cfg.CheckinHours...), append([]int{}, s.cfg.KeepaliveHours...)
}

// CheckinHours 当前签到时间（副本）。
func (s *Scheduler) CheckinHours() []int {
	ch, _ := s.hours()
	return ch
}

// KeepaliveHours 当前保活时间（副本）。
func (s *Scheduler) KeepaliveHours() []int {
	_, kh := s.hours()
	return kh
}

// SetCheckinHours 运行中更新签到小时并唤醒调度循环。
func (s *Scheduler) SetCheckinHours(hours []int) {
	s.mu.Lock()
	s.cfg.CheckinHours = append([]int{}, hours...)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// NextFire 返回最近的一次触发时间（签到 + 保活合并）。
func (s *Scheduler) NextFire() time.Time {
	ch, kh := s.hours()
	all := append(append([]int{}, ch...), kh...)
	return nextFire(time.Now(), all)
}

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
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
		ch, kh := s.hours()
		all := append(append([]int{}, ch...), kh...)
		next := nextFire(time.Now(), all)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop() // 配置变更，重算
		case <-timer.C:
			h := time.Now().Hour()
			if contains(ch, h) {
				s.RunCheckinNow()
			}
			if contains(kh, h) {
				s.RunKeepaliveNow()
			}
		}
	}
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
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
func (s *Scheduler) RunCheckinNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		s.checkinOne(st.UID)
	}
}

// CheckinAccount 单个账号立即签到（禁用跳过），返回该账号结果。
func (s *Scheduler) CheckinAccount(uid string) (CheckinResult, error) {
	st, ok := s.cfg.Pool.Status(uid)
	if !ok {
		return CheckinResult{}, fmt.Errorf("unknown account %s", uid)
	}
	if st.Disabled {
		return CheckinResult{}, fmt.Errorf("account %s disabled", uid)
	}
	return s.checkinOne(uid), nil
}

// checkinOne 单账号签到 + 余额刷新 + 解冻 + 记录签到状态。
func (s *Scheduler) checkinOne(uid string) CheckinResult {
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil || a.RefreshToken == "" {
		return CheckinResult{UID: uid, Msg: "no refresh token"}
	}
	r := CheckinResult{UID: uid}
	if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
		log.Printf("checkin %s: %v", uid, err)
		r.Msg = shortErr(err)
		if isAlready(err) {
			r.OK = true // 已签到时视为成功状态
			r.Msg = "已签到"
		}
	} else {
		r.OK = true
		r.Msg = "ok"
	}
	// 无论签到成败都查余额（已签到等业务错误下余额刷新仍有效）
	remain, rerr := s.cfg.Upstream.UserResource(a)
	if rerr != nil {
		log.Printf("user-resource %s: %v", uid, rerr)
		if r.OK {
			r.Msg += "；余额查询失败"
		}
	} else {
		r.Remain, r.HasRemain = remain, true
		s.cfg.Pool.ReenableIfCredits(uid, remain)
	}
	s.cfg.Pool.RecordCheckin(uid, r.OK, r.Msg)
	return r
}

// isAlready 已签判定：code 非 0 且含 "已签到"/"already"/"checkin" 等字样。
func isAlready(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "已签到") ||
		strings.Contains(s, "already") ||
		strings.Contains(s, "checkin") ||
		strings.Contains(s, "code=400")
}

func shortErr(err error) string {
	s := strings.TrimSpace(err.Error())
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
func (s *Scheduler) RunKeepaliveNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", st.UID, err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				s.cfg.Pool.Disable(st.UID, "12153 session dead")
			}
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", st.UID, err)
		}
	}
}
