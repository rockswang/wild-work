// 降级链事件记账（运行统计「降级记录」区块）：handler 在降级命中/恢复时喂入。
// 纯内存环形记录（重启清零）——降级是运行期瞬时状态；app.log 里的
// "model fallback platform=... from=... to=..." 行才是持久留痕，此处只做面板展示。
//
// 事件生命周期：
//   - NoteFallback：降级命中（原模型无可用账号：模型全冷 / 整渠道全灭 → 备胎顶上）时记一条，Active=true；
//   - NoteFallbackRecovery：后续同一原模型的请求在降级判定点测得容量回归
//     （不管该请求自身后续成败）→ 关闭进行中事件，落恢复时刻与持续秒数。
//     健康期每个请求都会喂恢复探针，绝大多数是空转 no-op（线性扫小环形，开销可忽略）。
package stats

import "time"

// fbKeepMax 内存保留的事件上限（超出丢最旧；按 200 条 × 每条几十字节，忽略不计）。
const fbKeepMax = 200

// fbEv 引擎内单条降级事件。
type fbEv struct {
	from  string
	to    string
	at    time.Time
	recAt time.Time // 零值 = 降级进行中
}

// FallbackEvent 降级事件快照行（GET /api/stats 载荷 fallbacks[]，最新在前）。
type FallbackEvent struct {
	From      string `json:"from"`                // 客户端请求的原模型（全路径）
	To        string `json:"to"`                  // 实际顶上的备胎（全路径）
	At        string `json:"at"`                  // 降级时刻（今日 "15:04:05"，跨日 "01-02 15:04"）
	AtUnix    int64  `json:"at_unix"`             // 降级时刻 epoch 秒（前端进行中事件实时计时用）
	Recovered string `json:"recovered,omitempty"` // 恢复时刻；空 = 降级进行中
	DurSec    int64  `json:"dur_sec,omitempty"`   // 降级持续秒数（恢复后固定）
	Active    bool   `json:"active"`              // 进行中（前端高亮 + 每秒重算持续时长）
}

// NoteFallback 记一次降级发生。同一原模型已有进行中事件时忽略——
// 恢复判定与降级判定同源于一次容量检查，正常不会重叠（防御性去重）。
func (s *Stats) NoteFallback(from, to string, now time.Time) {
	if from == "" || to == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.fallbacks {
		if e.recAt.IsZero() && e.from == from {
			return
		}
	}
	s.fallbacks = append(s.fallbacks, &fbEv{from: from, to: to, at: now})
	if len(s.fallbacks) > fbKeepMax {
		s.fallbacks = s.fallbacks[len(s.fallbacks)-fbKeepMax:]
	}
}

// NoteFallbackRecovery 原模型恢复：关闭其进行中事件。无进行中事件时 no-op。
func (s *Stats) NoteFallbackRecovery(from string, now time.Time) {
	if from == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.fallbacks {
		if e.recAt.IsZero() && e.from == from {
			e.recAt = now
			return
		}
	}
}

// fallbackRowsLocked 快照行（最新在前；须在 rollDayLocked 之后调用——跨日文案依赖 s.today）。
func (s *Stats) fallbackRowsLocked() []FallbackEvent {
	rows := make([]FallbackEvent, 0, len(s.fallbacks))
	for i := len(s.fallbacks) - 1; i >= 0; i-- {
		e := s.fallbacks[i]
		row := FallbackEvent{
			From:   e.from,
			To:     e.to,
			At:     fbClock(e.at, s.today),
			AtUnix: e.at.Unix(),
			Active: e.recAt.IsZero(),
		}
		if !row.Active {
			row.Recovered = fbClock(e.recAt, s.today)
			row.DurSec = int64(e.recAt.Sub(e.at).Seconds())
		}
		rows = append(rows, row)
	}
	return rows
}

// fbClock 时刻文案：今日只显时刻；跨日带日期防歧义（200 条环形在极端活跃下可能跨天）。
func fbClock(t time.Time, today string) string {
	if t.Format("2006-01-02") == today {
		return t.Format("15:04:05")
	}
	return t.Format("01-02 15:04")
}
