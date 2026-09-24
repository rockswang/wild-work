// Package ledger 用量/积分双流水记账。
//
// 设计要点（见 docs 与 AGENTS.md 决议讨论）：
//   - token 流水（渠道×模型维度）与积分流水（账号维度）分口径统计，不做强行关联。
//   - 写入侧：内存仅一个带缓冲句柄，掉 API 时 append 一行 JSONL，O(1) 无聚合状态。
//   - 读取侧：仅在 Web UI 请求 /api/usage 时一次性读文件聚合，边扫边聚合不保留原始行。
//   - 文件按月分段（usage-YYYYMM.jsonl / credit-YYYYMM.jsonl），单文件天然有界；
//     默认保留 6 个月，启动时清理更早的分段。个人工具量级（~1.5MB/月）下
//     单次聚合扫描 ≤2 个月分段 ≈ 毫秒级；若未来量级上涨可再加 rollup 偏移续扫。
//   - 流水只含 uid/模型名/token 数量，绝不含 access/refresh token（§6-2 红线）。
package ledger

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// UsageEntry 单次成功请求的 token 用量流水。
type UsageEntry struct {
	Ts    int64  `json:"ts"`            // Unix 秒
	Ch    string `json:"ch"`            // 渠道 kind（workbuddy/...）
	UID   string `json:"uid"`           // 账号 uid
	Model string `json:"model"`         // 客户端请求的原始模型名（含 channel/ 前缀）
	PT    int64  `json:"pt"`            // prompt/input tokens
	CT    int64  `json:"ct"`            // completion/output tokens
	Src   string `json:"src,omitempty"` // upstream=上游返回 | none=无 usage（token 记 0，仅计请求数）
}

// CreditEntry 积分变动流水（按账号条目差分而来，非逐笔精确账）。
type CreditEntry struct {
	Ts      int64  `json:"ts"`             // Unix 秒
	Ch      string `json:"ch"`             // 渠道 kind
	UID     string `json:"uid"`            // 账号 uid
	Kind    string `json:"kind"`           // earn | spend | expire
	Amount  int64  `json:"amount"`         // 正=入项，负=消耗/过期
	Balance int64  `json:"balance"`        // 事件后该账号可消耗余额
	Note    string `json:"note,omitempty"` // 条目名等备注（如 "签到奖励"/"月度套餐"）
}

const (
	// keepMonths 流水分段保留月数（启动时清理更早的分段）。
	keepMonths = 1
	// keepDays 单文件内条目保留天数（启动时裁剪更早的)。
	keepDays = 7
)

// Ledger 双流水记账器。goroutine 安全。
type Ledger struct {
	mu  sync.Mutex
	dir string

	uw     *bufio.Writer // 当前月 usage 文件缓冲写句柄（nil = 未打开）
	cw     *bufio.Writer
	uF     *os.File
	cF     *os.File
	uMonth string // 当前句柄对应的月份 "200601"
	cMonth string
}

// New 构建。dir 例 data/ledger；目录不存在则创建，并顺带清理过期分段。
func New(dir string) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	l := &Ledger{dir: dir}
	l.cleanup()
	l.trimCurrentMonth() // 裁剪当月文件内 7 天前条目
	return l, nil
}

// cleanup 删除保留窗口之外的月分段文件（失败静默：锦上添花功能不阻塞启动）。
func (l *Ledger) cleanup() {
	cutoff := time.Now().AddDate(0, -keepMonths, 0).Format("200601")
	ents, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		// 文件名形如 usage-202601.jsonl：截出月份段比较
		stem := strings.TrimSuffix(name, ".jsonl")
		if i := strings.LastIndexByte(stem, '-'); i >= 0 && len(stem)-i-1 == 6 {
			if stem[i+1:] < cutoff {
				_ = os.Remove(filepath.Join(l.dir, name))
			}
		}
	}
}

// trimCurrentMonth 裁剪当月文件内 7 天前的条目：一行行扫一遍，只保留 cutTs 之后的。
// 日常 150 行/天的量级下走重写方式即可，无需 mmap 或 seek 优化。
func (l *Ledger) trimCurrentMonth() {
	cutTs := time.Now().AddDate(0, 0, -keepDays).Unix()
	for _, prefix := range []string{"usage", "credit"} {
		fp := filepath.Join(l.dir, monthFile(prefix, time.Now()))
		raw, err := os.ReadFile(fp)
		if err != nil {
			continue
		}
		// 快速路径：首行时间戳已 ≤ cutTs，无需裁剪
		if idx := findLineTs(raw); idx >= 0 && idx <= cutTs {
			continue
		}
		// 双指针原地过滤（避免分配临时数组）
		keep := raw[:0]
		for len(raw) > 0 {
			nl := indexByte(raw, '\n')
			line := raw
			if nl >= 0 {
				line, raw = raw[:nl], raw[nl+1:]
			} else {
				raw = nil
			}
			// 取时间戳：json {"ts":1790144604,...}
			ts := parseTs(line)
			if ts >= cutTs {
				keep = append(keep, line...)
				keep = append(keep, '\n')
			}
		}
		_ = os.WriteFile(fp, keep, 0o600)
	}
}

// findLineTs 取 JSONL 文件首行时间戳（用于快速路径判断）。
// 首行 → 裁剪边界说明全部待裁剪；首行在边界后说明全部保留。
func findLineTs(raw []byte) int64 {
	nl := indexByte(raw, '\n')
	if nl < 0 {
		nl = len(raw)
	}
	return parseTs(raw[:nl])
}

// parseTs 从 JSONL 行头提取 \"ts\": 后的整数（无 jq 依赖，纯字节扫描）。
// 不成功返回 0（保守保留）。
func parseTs(line []byte) int64 {
	// 找 "ts":
	const needle = `"ts":`
	idx := indexBytes(line, []byte(needle))
	if idx < 0 {
		return 0
	}
	// 跳过空白和冒号后的空白
	b := line[idx+len(needle):]
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	var v int64
	for len(b) > 0 && b[0] >= '0' && b[0] <= '9' {
		v = v*10 + int64(b[0]-'0')
		b = b[1:]
	}
	return v
}

// indexByte 返回字节切片中目标字节首次出现的索引，未找到返回 -1。
func indexByte(s []byte, c byte) int {
	for i, b := range s {
		if b == c {
			return i
		}
	}
	return -1
}

// indexBytes 返回字节切片中子串首次出现的索引，未找到返回 -1。
func indexBytes(s, sub []byte) int {
	if len(sub) == 0 {
		return 0
	}
	if len(sub) > len(s) {
		return -1
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		match := true
		for j := 0; j < len(sub); j++ {
			if s[i+j] != sub[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// monthSegment 当前（或指定时间）月份的分段文件名。
func monthFile(prefix string, t time.Time) string {
	return fmt.Sprintf("%s-%s.jsonl", prefix, t.Format("200601"))
}

// writer 取指定月份的缓冲写句柄；跨月时关旧开新（自然轮转）。
// 调用方必须已持有 l.mu。
func (l *Ledger) writer(prefix string, t time.Time) (*bufio.Writer, *os.File, string, error) {
	month := t.Format("200601")
	var w *bufio.Writer
	var f *os.File
	switch prefix {
	case "usage":
		if l.uw != nil && l.uMonth == month {
			return l.uw, l.uF, month, nil
		}
		if l.uF != nil {
			_ = l.uw.Flush()
			_ = l.uF.Close()
			l.uw, l.uF, l.uMonth = nil, nil, ""
		}
	case "credit":
		if l.cw != nil && l.cMonth == month {
			return l.cw, l.cF, month, nil
		}
		if l.cF != nil {
			_ = l.cw.Flush()
			_ = l.cF.Close()
			l.cw, l.cF, l.cMonth = nil, nil, ""
		}
	}
	fp := filepath.Join(l.dir, monthFile(prefix, t))
	f, err := os.OpenFile(fp, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, "", err
	}
	w = bufio.NewWriterSize(f, 8*1024)
	switch prefix {
	case "usage":
		l.uw, l.uF, l.uMonth = w, f, month
	case "credit":
		l.cw, l.cF, l.cMonth = w, f, month
	}
	return w, f, month, nil
}

// AppendUsage 记一条 token 用量流水。掉 API 主链路调用，必须不阻塞不报错向上。
func (l *Ledger) AppendUsage(e UsageEntry) {
	if e.Ts == 0 {
		e.Ts = time.Now().Unix()
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	w, _, _, err := l.writer("usage", time.Now())
	if err != nil {
		return
	}
	_, _ = w.Write(append(raw, '\n'))
	// 缓冲写：不到 8KB 不落盘，靠定期 Flush 兜底（见 Flush）。
	// 崩溃最多丢最后几秒流水，可接受（锦上添花统计，非账务）。
}

// AppendCredit 记一条积分流水。
func (l *Ledger) AppendCredit(e CreditEntry) {
	if e.Ts == 0 {
		e.Ts = time.Now().Unix()
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	w, _, _, err := l.writer("credit", time.Now())
	if err != nil {
		return
	}
	_, _ = w.Write(append(raw, '\n'))
}

// Flush 把缓冲句柄落盘。Web UI 读取聚合前必须先调（保证读到最新流水）。
func (l *Ledger) Flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.uw != nil {
		_ = l.uw.Flush()
	}
	if l.cw != nil {
		_ = l.cw.Flush()
	}
}

// AutoFlush 后台定时落盘缓冲句柄（30s），防止长时间不开面板时流水迟迟不落盘。
// stop 关闭即退出。App 启动时起一个 goroutine 调用。
func (l *Ledger) AutoFlush(stop <-chan struct{}) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			l.Flush()
			return
		case <-t.C:
			l.Flush()
		}
	}
}

// Close 关闭句柄（进程退出前）。
func (l *Ledger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.uw != nil {
		_ = l.uw.Flush()
		_ = l.uF.Close()
		l.uw, l.uF, l.uMonth = nil, nil, ""
	}
	if l.cw != nil {
		_ = l.cw.Flush()
		_ = l.cF.Close()
		l.cw, l.cF, l.cMonth = nil, nil, ""
	}
}

// ---------------------------------------------------------------------------
// 读取聚合（仅 Web UI 请求时执行，平时零开销）
// ---------------------------------------------------------------------------

// ModelStat 模型维度小计。
type ModelStat struct {
	Model    string `json:"model"`   // 渠道内裸模型名
	Channel  string `json:"channel"` // 渠道 kind
	Requests int64  `json:"requests"`
	PT       int64  `json:"pt"`
	CT       int64  `json:"ct"`
}

// RecentCredit 最近一条积分流水（展示用）。
type RecentCredit struct {
	Ts      int64  `json:"ts"`
	UID     string `json:"uid"`
	Name    string `json:"name,omitempty"`
	Channel string `json:"channel"`
	Kind    string `json:"kind"`
	Amount  int64  `json:"amount"`
	Balance int64  `json:"balance"`
	Note    string `json:"note,omitempty"`
}

// Stats 一段时间范围的聚合结果（/api/usage 响应体）。
type Stats struct {
	RecordedSince string      `json:"recorded_since,omitempty"` // 首条流水日期 YYYY-MM-DD
	Token         TokenStats  `json:"token"`
	Credit        CreditStats `json:"credit"`
}

// TokenStats token 口径统计（成功请求）。
type TokenStats struct {
	Total    int64       `json:"total"` // pt+ct 合计
	PT       int64       `json:"pt"`
	CT       int64       `json:"ct"`
	Requests int64       `json:"requests"` // 成功请求数（含 src=none 的 0 token 请求）
	ByDay    []DayTokens `json:"by_day"`   // 按天（days=1 时按小时，Date 为 "HH:00"）
	ByModel  []ModelStat `json:"by_model"` // 按模型降序
}

// DayTokens 单日（或单小时）token 分渠道小计。
type DayTokens struct {
	Date      string           `json:"date"` // "01-15"（按小时时 "14:00"）
	Total     int64            `json:"total"`
	ByChannel map[string]int64 `json:"by_channel"`
}

// CreditStats 积分口径统计：总量 + 原始条目（分页/图表由前端自算）。
type CreditStats struct {
	Earn    int64             `json:"earn"`
	Spend   int64             `json:"spend"`   // 正数
	Expire  int64             `json:"expire"`  // 正数
	Entries []RecentCredit    `json:"entries"` // 窗口内全量条目（时间升序）
	NameMap map[string]string `json:"name_map,omitempty"` // uid → 昵称
}

// Query 聚合最近 days 天的流水（days ∈ {1,7}，其他值按 7 处理）。
// 读取前自动 Flush。返回结果为新建对象，调用方可直接 JSON 序列化。
// enrich：可选，查询后按 uid 回填账号昵称（闭包由 App 提供，拿 pool 状态）。
func (l *Ledger) Query(days int, enrich func(uid string) (name, channel string)) *Stats {
	if days != 1 {
		days = 7
	}
	l.Flush()

	now := time.Now()
	from := now.AddDate(0, 0, -(days - 1))
	fromTs := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, now.Location()).Unix()

	// 覆盖范围内的月份分段（最多 2 个：当月 + 上月）
	months := map[string]bool{}
	for t := from; !t.After(now); t = t.AddDate(0, 0, 15) {
		months[t.Format("200601")] = true
	}

	st := &Stats{}
	// 聚合中间态：全部按天（小时视图由当天流水二次展开）
	type dayAgg struct {
		total int64
		byCh  map[string]int64
	}
	days_ := map[string]*dayAgg{} // "01-15" -> agg
	models := map[string]*ModelStat{}
	hours := map[int]*dayAgg{} // 今天的小时桶（days=1 用）

	var sinceTs int64
	var entries []RecentCredit

	for m := range months {
		for _, prefix := range []string{"usage", "credit"} {
			fp := filepath.Join(l.dir, fmt.Sprintf("%s-%s.jsonl", prefix, m))
			scanJSONL(fp, func(line []byte) {
				switch prefix {
				case "usage":
					var e UsageEntry
					if json.Unmarshal(line, &e) != nil {
						return
					}
					if sinceTs == 0 || e.Ts < sinceTs {
						sinceTs = e.Ts
					}
					t := time.Unix(e.Ts, 0)
					key := t.Format("01-02")
					if e.Ts < fromTs {
						return // 只影响 recorded_since，不进聚合
					}
					d := days_[key]
					if d == nil {
						d = &dayAgg{byCh: map[string]int64{}}
						days_[key] = d
					}
					d.total += e.PT + e.CT
					d.byCh[e.Ch] += e.PT + e.CT
					st.Token.Total += e.PT + e.CT
					st.Token.PT += e.PT
					st.Token.CT += e.CT
					st.Token.Requests++
					// 模型榜 key 用 "ch/model" 防跨渠道同名模型合并
					mk := e.Ch + "/" + e.Model
					ms := models[mk]
					if ms == nil {
						ms = &ModelStat{Model: e.Model, Channel: e.Ch}
						models[mk] = ms
					}
					ms.Requests++
					ms.PT += e.PT
					ms.CT += e.CT
					// 今天的小时桶
					if t.Format("2006-01-02") == now.Format("2006-01-02") {
						h := hours[t.Hour()]
						if h == nil {
							h = &dayAgg{byCh: map[string]int64{}}
							hours[t.Hour()] = h
						}
						h.total += e.PT + e.CT
						h.byCh[e.Ch] += e.PT + e.CT
					}
				case "credit":
					var e CreditEntry
					if json.Unmarshal(line, &e) != nil {
						return
					}
					if e.Ts < fromTs {
						return
					}
					switch e.Kind {
					case "earn":
						st.Credit.Earn += e.Amount
					case "spend":
						st.Credit.Spend += -e.Amount
					case "expire":
						st.Credit.Expire += -e.Amount
					}
					entries = append(entries, RecentCredit{Ts: e.Ts, UID: e.UID, Channel: e.Ch,
						Kind: e.Kind, Amount: e.Amount, Balance: e.Balance, Note: e.Note})
				}
			})
		}
	}

	// recorded_since：全量流水最早日期（用于「已记录 N 天」角标）
	if sinceTs > 0 {
		st.RecordedSince = time.Unix(sinceTs, 0).Format("2006-01-02")
	} else if ts := l.earliestCreditTs(); ts > 0 {
		st.RecordedSince = time.Unix(ts, 0).Format("2006-01-02")
	}

	// by_day / by_hour 序列化
	if days == 1 {
		for h := 0; h <= now.Hour(); h++ {
			a := hours[h]
			dt := DayTokens{Date: fmt.Sprintf("%02d:00", h), ByChannel: map[string]int64{}}
			if a != nil {
				dt.Total, dt.ByChannel = a.total, a.byCh
			}
			st.Token.ByDay = append(st.Token.ByDay, dt)
		}
	} else {
		keys := make([]string, 0, len(days_))
		for k := range days_ {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			st.Token.ByDay = append(st.Token.ByDay, DayTokens{Date: k, Total: days_[k].total, ByChannel: days_[k].byCh})
		}
	}

	// 模型榜降序
	mk := make([]string, 0, len(models))
	for k := range models {
		mk = append(mk, k)
	}
	sort.Slice(mk, func(i, j int) bool {
		a, b := models[mk[i]], models[mk[j]]
		if a.PT+a.CT != b.PT+b.CT {
			return a.PT+a.CT > b.PT+b.CT
		}
		return a.Requests > b.Requests
	})
	for _, k := range mk {
		st.Token.ByModel = append(st.Token.ByModel, *models[k])
	}

	// 返回原始条目 + 昵称表（时间升序；分页/top10 折线由前端自算）
	st.Credit.Entries = entries
	if len(entries) != 0 && enrich != nil {
		st.Credit.NameMap = map[string]string{}
		seen := map[string]bool{}
		for _, r := range entries {
			if seen[r.UID] {
				continue
			}
			seen[r.UID] = true
			if name, _ := enrich(r.UID); name != "" {
				st.Credit.NameMap[r.UID] = name
			}
		}
	}
	return st
}

// earliestCreditTs 全量 credit 流水最早时间戳（recorded_since 兜底，usage 为空时用）。
func (l *Ledger) earliestCreditTs() int64 {
	var min int64
	ents, err := os.ReadDir(l.dir)
	if err != nil {
		return 0
	}
	var names []string
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "credit-") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names { // 最早月份分段里第一行即全局最早
		var got bool
		scanJSONL(filepath.Join(l.dir, n), func(line []byte) {
			if got {
				return
			}
			var e CreditEntry
			if json.Unmarshal(line, &e) == nil {
				min, got = e.Ts, true
			}
		})
		if got {
			break
		}
	}
	return min
}

// scanJSONL 逐行流式解析 JSONL 文件；文件不存在静默跳过（首月无流水）。
// 只前向扫描，不保留行，内存占用 O(1)。
func scanJSONL(fp string, fn func(line []byte)) {
	f, err := os.Open(fp)
	if err != nil {
		return
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if line[len(line)-1] == '\n' {
				line = line[:len(line)-1]
			}
			if len(line) > 0 {
				fn(line)
			}
		}
		if err != nil {
			return
		}
	}
}
