// Package loopguard 实时「思考链死循环（发癫）」检测：在流式生成过程中逐帧喂入
// 推理增量（reasoning delta），命中机械复读/空转时返回命中信息，由调用方中断 SSE、
// 向客户端下发错误帧，交给下游 agent 自行重试恢复。
//
// 算法移植自 dsh-attention-health 的 lib/guard.js（只取死循环检测，忽略健康评估/
// 工作交接/会话日志）。设计立场与参考实现一致：**宁可漏报，不可误杀**——
// 中断一次正在进行的生成是有代价的，故只抓「正常思考绝不会出现」的形态。
//
// 四条判据（阈值对齐 DSH 在 79+ 真实会话上标定的 GUARD_DEFAULTS）：
//
//  1. repeat-line       同一 trim 后的行连续重复 ≥8 次，且行长 ≥12 字符；
//  2. short-line-repeat 最近 60 个 ≤11 字符的短行里，同一短行 ≥30 次（抓 A/B 交替空转）；
//  3. spinning          最近 40 个非空行里，不同行占比 ≤0.25（滑动窗口空转）；
//  4. repeat-inline     行内周期复读：末尾存在长度 p∈[2,40] 的单元，连续重复 ≥16 次
//     且累计 ≥160 字符，且单元含字母或汉字。
//
// 关键护栏（一并移植，否则误杀）：
//   - 行内周期单元必须含 unicode 字母/汉字：纯符号（─ 长分隔线）、纯数字
//     （命令参数/序号/版本号）是正常结构重复，不算空转；
//   - 十六进制字节周期（0x00,0x01,…）豁免；
//   - 代码围栏（``` 与 ~~~）内的行不进 ②③ 窗口、也不做 ④ 行内判定（2026-10-14
//   - 结构标点行（trim 后无字母/数字，如 `}` `{` `});`）不进短行高频与唯一率窗口——
//     模型在思维链里写代码时闭合块高频产生 `}` 是正常形态，不是空转（2026-10-14
//     误伤现场：agent 正常生成 Go 代码被 short-line-repeat 掐断，60 行里 30 行 `}`）。
//     与行内护栏（纯数字/纯符号豁免）同一哲学：正常结构重复不算发癫；
//     修订：原实现围栏只豁免 ④，围栏内代码的 `}` 短行照样进 ② 窗口被误杀，
//     故 ②③ 同步排除围栏内行；标点豁免兼兜无围栏裸代码（fence 状态机管不到）。
package loopguard

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"unicode"
)

// Kind 命中判据类型。
type Kind string

const (
	RepeatLine      Kind = "repeat_line"       // 同行连续重复
	ShortLineRepeat Kind = "short_line_repeat" // 短行高频交替
	Spinning        Kind = "spinning"          // 滑动窗口空转
	RepeatInline    Kind = "repeat_inline"     // 行内片段周期复读
)

// CodeReasoningLoop 守卫命中时下发给客户端的错误码（OpenAI error 帧的 code 字段）。
// 与 R36 的 upstream_truncated / R40 的 upstream_rate_limited 同族。
const CodeReasoningLoop = "upstream_reasoning_loop"

// 行内复读的止损阈值（对齐 DSH 模块级常量 INLINE_TRIP_REPEATS / INLINE_TRIP_CHARS）。
// inlineKeep 与探测长度必须同源使用 INLINE_TRIP_REPEATS，否则流式分片下长周期
// 会在凑齐止损次数前被裁掉（整段喂命中、分片喂漏检——DSH AY.2 复核抓到的坑）。
const (
	inlineTripRepeats = 16
	inlineTripChars   = 160
)

// Hit 守卫命中结果。
type Hit struct {
	Kind   Kind           `json:"kind"`
	Reason string         `json:"reason"`
	Detail map[string]any `json:"detail"`
}

// Error 守卫命中错误（实现 error）。
//
// ⚠️ 刻意**不**实现 provider.StreamErrorClassifier：死循环是「模型/请求级」问题，
// 不是账号故障——换账号重试必然复现，冷却账号只会把后续请求挡成
// no_healthy_account（R16/R38 的误伤形态）。handler 里 errors.As(serr, &sec)
// 取不到分类就会自然跳过惩罚分支，只记日志并透传，让下游 agent 自己重试。
type Error struct {
	Hit Hit
}

func (e *Error) Error() string {
	if e == nil {
		return "reasoning loop detected"
	}
	return "reasoning loop detected: " + e.Hit.Reason
}

// ErrorFrameJSON 返回 OpenAI 规范 error 帧的 JSON 载荷（不含 "data: " 前缀）。
// 调用方自行拼 "data: " + json + "\n\n" 写出并 flush。
// message 截断到 200 字符（rune 安全），避免撑大客户端日志；不含任何 token。
func ErrorFrameJSON(hit *Hit) string {
	msg := "reasoning loop detected"
	if hit != nil {
		msg = hit.Reason
	}
	if len([]rune(msg)) > 200 {
		msg = clipRunes(msg, 200)
	}
	b, err := json.Marshal(map[string]any{"error": map[string]any{
		"message": msg,
		"type":    "upstream_error",
		"code":    CodeReasoningLoop,
	}})
	if err != nil {
		return `{"error":{"message":"reasoning loop detected","type":"upstream_error","code":"upstream_reasoning_loop"}}`
	}
	return string(b)
}

// ReasoningDeltaOf 从 OpenAI 形状的 chunk 里提取 reasoning 增量。
// 各渠道 Stream 实现解出的 chunk 均为此形状，统一走这里取值。
//
// 字段名兼容三种（2026-10-13 实抓）：`reasoning_content`（DeepSeek/WorkBuddy 系）、
// `reasoning`（OpenRouter 归一化字段，Cline 免费池实测下发）、
// `reasoning_details`（OpenRouter 结构化块：`reasoning.text` 为原始全文、
// `reasoning.summary` 为摘要，取 text 优先；encrypted 是密文无可读文本）。
// 优先级与 upstream.reasoningDeltaOf 保持一致，否则同一帧两个包取值不同步。
func ReasoningDeltaOf(obj map[string]any) string {
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	c, _ := choices[0].(map[string]any)
	if c == nil {
		return ""
	}
	delta, _ := c["delta"].(map[string]any)
	if delta == nil {
		return ""
	}
	if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
		return rc
	}
	if rc, ok := delta["reasoning"].(string); ok && rc != "" {
		return rc
	}
	items, ok := delta["reasoning_details"].([]any)
	if !ok {
		return ""
	}
	var full, summary strings.Builder
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m == nil {
			continue
		}
		switch m["type"] {
		case "reasoning.text":
			if s, ok := m["text"].(string); ok {
				full.WriteString(s)
			}
		case "reasoning.summary":
			if s, ok := m["summary"].(string); ok {
				summary.WriteString(s)
			}
		}
	}
	if full.Len() > 0 {
		return full.String()
	}
	return summary.String()
}

// Config 守卫阈值。零值字段不可用，必须从 DefaultConfig 起步再覆盖。
type Config struct {
	// StreakLimit 同一行连续出现多少次即判复读（默认 8）。
	StreakLimit int
	// MinLineChars 计入「复读」的行最短字符数（排除 --- / } / ); 这类短行）。
	MinLineChars int
	// WindowLines 空转检测的滑动窗口行数（默认 40）。
	WindowLines int
	// WindowMinLines 窗口内至少要有这么多行才做空转判断（默认 24）。
	WindowMinLines int
	// UniqueRatioMin 窗口内不同行占比下限；低于它判空转（默认 0.25）。
	UniqueRatioMin float64
	// ShortLineWindow 短行高频判据的窗口行数（默认 60）。
	ShortLineWindow int
	// ShortLineMaxChars 判定为「短行」的最大字符数（默认 11）。
	ShortLineMaxChars int
	// ShortLineRepeatLimit 窗口内同一短行出现多少次即判空转（默认 30，勿超）。
	ShortLineRepeatLimit int
	// ShortLineMinCount 窗口内至少要有这么多短行才做判定（默认 30）。
	ShortLineMinCount int
	// InlineMinPeriod / InlineMaxPeriod 行内周期扫描范围（默认 2..40）。
	InlineMinPeriod int
	InlineMaxPeriod int
	// InlineMinPendingChars 未闭合行超过这个长度后才做行内探测（默认 64）。
	InlineMinPendingChars int
}

// DefaultConfig 与 DSH GUARD_DEFAULTS 对齐的默认阈值。
var DefaultConfig = Config{
	StreakLimit:           8,
	MinLineChars:          12,
	WindowLines:           40,
	WindowMinLines:        24,
	UniqueRatioMin:        0.25,
	ShortLineWindow:       60,
	ShortLineMaxChars:     11,
	ShortLineRepeatLimit:  30,
	ShortLineMinCount:     30,
	InlineMinPeriod:       2,
	InlineMaxPeriod:       40,
	InlineMinPendingChars: 64,
}

// enabled 全局开关（默认开；面板可运行时切换）。atomic 保证热更新线程安全。
var enabled atomic.Bool

func init() { enabled.Store(true) }

// SetEnabled 运行时切换全局开关（面板保存 loop_guard 时调用）。
func SetEnabled(v bool) { enabled.Store(v) }

// Enabled 报告当前是否启用。
func Enabled() bool { return enabled.Load() }

// Stats 守卫观测数据（用于日志回溯误报）。
type Stats struct {
	TotalChars    int // 喂入的推理字符总数（rune）
	LineCount     int // 已结算行数
	LastStreak    int // 当前同行连续重复计数
	WindowLines   int // 滑动窗口内行数
	WindowUnique  int // 滑动窗口内不同行数
	SkippedInline int // 被代码围栏豁免的行内检查次数
}

// Guard 每个流式请求一个实例（用完即弃）。
type Guard struct {
	cfg Config

	pending []rune // 未见到换行的尾巴
	// 代码围栏状态（CommonMark 行结构，按完整行判定，跨分片安全）：
	// 开 = 行首(≤3 空格)后跟 ≥3 个同类字符；关 = 行首后跟不少于开启长度的同类字符，
	// 且其后只有空白。fenceChar 0 = 不在围栏内。
	fenceChar rune
	fenceLen  int

	totalChars    int
	lineCount     int
	lastLine      string
	streak        int
	window        []string // 最近 windowLines 个非空行（全行参与，含短行）
	shortWindow   []string // 最近 shortLineWindow 个 ≤11 字符的行
	skippedInline int
}

// New 构造守卫。
func New(cfg Config) *Guard {
	return &Guard{cfg: cfg}
}

// NewDefault 用默认阈值构造守卫。
func NewDefault() *Guard { return New(DefaultConfig) }

// inlineKeep 未闭合行的保留长度（rune）。与 checkInline 探测长度同源，
// 二者必须使用同一个表达式（DSH AY.2）。
func (g *Guard) inlineKeep() int {
	k := g.cfg.InlineMaxPeriod * inlineTripRepeats
	if k < 256 {
		k = 256
	}
	return k
}

// Push 喂入一个 reasoning 增量片段。命中返回 *Hit（此时调用方应中断流），否则 nil。
// 已全局禁用或空串时直接返回 nil。
func (g *Guard) Push(text string) *Hit {
	if !Enabled() || text == "" {
		return nil
	}
	g.totalChars += runeLen(text)
	for _, ch := range text {
		if ch == '\n' {
			g.updateFence(g.pending)
			hit := g.addLine(string(g.pending))
			g.pending = g.pending[:0]
			if hit != nil {
				return hit
			}
			continue
		}
		g.pending = append(g.pending, ch)
		if len(g.pending) < g.cfg.InlineMinPendingChars {
			continue
		}
		// 围栏内不做行内中止（代码/字符串字面量有明确用途）。① 同行连续仍按原规则
		// 中止（对齐 DSH AZ.2/BA.3：连续 8 行相同长行即使围栏内也几乎必是发癫，
		// 且 MinLineChars 已排除短行）；②③ 的围栏豁免在 addLine 侧生效。
		exempt := g.fenceChar != 0
		if exempt {
			g.skippedInline++
		} else if hit := g.checkInline(g.pending); hit != nil {
			return hit
		}
		// 只在尾巴保留探测所需长度，避免 pending 无界增长。
		if keep := g.inlineKeep(); len(g.pending) > keep {
			g.pending = g.pending[len(g.pending)-keep:]
		}
	}
	return nil
}

// Stats 观测快照。
func (g *Guard) Stats() Stats {
	return Stats{
		TotalChars:    g.totalChars,
		LineCount:     g.lineCount,
		LastStreak:    g.streak,
		WindowLines:   len(g.window),
		WindowUnique:  uniqueCount(g.window),
		SkippedInline: g.skippedInline,
	}
}

// trip 构造命中结果。
func trip(kind Kind, reason string, detail map[string]any) *Hit {
	return &Hit{Kind: kind, Reason: reason, Detail: detail}
}

// addLine 判定一条完整行（已 trim 过的非空行）。
func (g *Guard) addLine(raw string) *Hit {
	line := strings.TrimSpace(raw)
	if line == "" {
		return nil
	}
	g.lineCount++

	// ① 同一行连续重复
	if line == g.lastLine {
		g.streak++
	} else {
		g.lastLine = line
		g.streak = 1
	}
	if runeLen(line) >= g.cfg.MinLineChars && g.streak >= g.cfg.StreakLimit {
		return trip(RepeatLine, fmt.Sprintf("同一行连续重复 %d 次", g.streak), map[string]any{
			"streak":    g.streak,
			"lineChars": runeLen(line),
			"sample":    clipRunes(line, 80),
		})
	}

	// ② 短行高频交替（抓「做。」/「**（Go.）**」这类 A/B 交替空转，
	// 它躲开 ①（短行被 minLineChars 挡掉）与 ③（短行不进旧窗口））。
	// 豁免两类行：围栏内行（代码/字面量，行级重复是正常形态）与结构标点行
	// （`}` 等闭合块高频形态，且兜住无围栏裸代码——DeepSeek 系思维链常不挂围栏）。
	punct := isStructuralLine(line)
	inFence := g.fenceChar != 0
	if !punct && !inFence && runeLen(line) <= g.cfg.ShortLineMaxChars {
		g.shortWindow = append(g.shortWindow, line)
		if len(g.shortWindow) > g.cfg.ShortLineWindow {
			g.shortWindow = g.shortWindow[1:]
		}
		if len(g.shortWindow) >= g.cfg.ShortLineMinCount {
			counts := map[string]int{}
			best, bestLine := 0, ""
			for _, l := range g.shortWindow {
				n := counts[l] + 1
				counts[l] = n
				if n > best {
					best, bestLine = n, l
				}
			}
			if best >= g.cfg.ShortLineRepeatLimit {
				return trip(ShortLineRepeat, fmt.Sprintf("最近 %d 个短行里有 %d 行是「%s」",
					len(g.shortWindow), best, clipRunes(bestLine, 40)), map[string]any{
					"windowLines": len(g.shortWindow),
					"repeat":      best,
					"sample":      clipRunes(bestLine, 40),
				})
			}
		}
	}

	// ③ 滑动窗口空转（全部含内容的行参与，含短行——原来只收 ≥12 字符的行，
	// 于是「短行占绝大多数」的空转窗口反而唯一率很高、永远测不出来）。
	// 围栏内行与标点行同样排除：否则 40 行里 30+ 个 `}` 会把唯一率稀释到 ≤0.25，
	// 修完 ② 后 spinning 会接力误伤同一段正常代码。
	if !punct && !inFence {
		g.window = append(g.window, line)
		if len(g.window) > g.cfg.WindowLines {
			g.window = g.window[1:]
		}
		if len(g.window) >= g.cfg.WindowMinLines {
			uniq := uniqueCount(g.window)
			ratio := float64(uniq) / float64(len(g.window))
			if ratio <= g.cfg.UniqueRatioMin {
				return trip(Spinning, fmt.Sprintf("最近 %d 行只有 %d 种内容", len(g.window), uniq), map[string]any{
					"windowLines": len(g.window),
					"unique":      uniq,
					"ratio":       float64(int(ratio*1000)) / 1000,
				})
			}
		}
	}
	return nil
}

// checkInline 行内周期复读：末尾存在长度 p∈[min,max] 的单元，连续重复 ≥16 次
// 且累计 ≥160 字，且单元含字母/汉字（纯符号分隔线/纯数字命令参数豁免）。
func (g *Guard) checkInline(pending []rune) *Hit {
	probe := pending
	if keep := g.inlineKeep(); len(probe) > keep {
		probe = probe[len(probe)-keep:]
	}
	for p := g.cfg.InlineMinPeriod; p <= g.cfg.InlineMaxPeriod; p++ {
		if len(probe) < p*inlineTripRepeats {
			continue
		}
		unit := probe[len(probe)-p:]
		if !hasNonSpace(unit) || !hasWordChar(unit) || isHexByteCycle(unit) {
			continue
		}
		// 快速预检：末尾两段不同 → 连 2 次都不到，直接试下一周期。
		if !equalRunes(probe[len(probe)-p:], probe[len(probe)-2*p:len(probe)-p]) {
			continue
		}
		n := 2 // 末尾两段已确认相同
		for i := len(probe) - 2*p; i-p >= 0; i -= p {
			if equalRunes(probe[i-p:i], unit) {
				n++
			} else {
				break
			}
		}
		if n >= inlineTripRepeats && n*p >= inlineTripChars {
			return trip(RepeatInline, fmt.Sprintf("同一片段连续重复 %d 次（单元 %d 字符）", n, p), map[string]any{
				"repeat":    n,
				"unitChars": p,
				"sample":    clipRunes(string(unit), 60),
			})
		}
	}
	return nil
}

// updateFence 按 CommonMark 行结构维护代码围栏状态（开/关）。
func (g *Guard) updateFence(line []rune) {
	m := fenceRe.FindStringSubmatch(string(line))
	if m == nil {
		return
	}
	marker, rest := m[1], m[2]
	if g.fenceChar == 0 {
		// 开启：反引号围栏的 info string 里不允许再出现反引号（CommonMark）
		if marker[0] == '`' && strings.Contains(rest, "`") {
			return
		}
		g.fenceChar = rune(marker[0])
		g.fenceLen = len(marker)
	} else if marker[0] == byte(g.fenceChar) && len(marker) >= g.fenceLen && strings.TrimSpace(rest) == "" {
		g.fenceChar = 0
		g.fenceLen = 0
	}
}

// fenceRe 代码围栏开/关行：行首(≤3 空格)后跟 ≥3 个 ``` 或 ~~~。
var fenceRe = regexp.MustCompile(`^ {0,3}(` + "`{3,}|~{3,}" + `)(.*)$`)

// hexByteRe 十六进制字节序列：0x00,0x01,…（大小写不敏感）。
var hexByteRe = regexp.MustCompile(`(?i)^(?:0x[0-9a-f]{2}[,;\s]*)+$`)

// isHexByteCycle 判断周期单元是否为十六进制字节表示（含任意循环位移）。
func isHexByteCycle(unit []rune) bool {
	if len(unit) < 4 || !strings.ContainsAny(string(unit), "xX") {
		return false
	}
	for offset := 0; offset < len(unit); offset++ {
		rot := string(unit[offset:]) + string(unit[:offset])
		if hexByteRe.MatchString(rot) {
			return true
		}
	}
	return false
}

// hasWordChar 单元是否含字母或汉字（DSH INLINE_WORD_RE = /[\p{L}]/u）。
func hasWordChar(unit []rune) bool {
	for _, r := range unit {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// hasNonSpace 单元是否含非空白字符。
func hasNonSpace(unit []rune) bool {
	for _, r := range unit {
		if !unicode.IsSpace(r) {
			return true
		}
	}
	return false
}

// isStructuralLine 结构行豁免：① trim 后无字母/数字（`}`、`{`、`});`、`"""`）；
// ② 连续词行（无空白/标点，如 break、return、continue）——控制流关键字在重复
// 样板代码里高频出现是正常形态。发癫短句（「做。」/ "ok then."）必含标点或空白，
// 照常进窗口。已知残留风险：`else {` 这类带结构符号的关键字组合仍计入窗口
// （密度低、需 30 个同构分支才触发，遇实锤再收）。
// 与围栏豁免互补：围栏状态机覆盖不到无围栏裸代码，结构行豁免兜底。
func isStructuralLine(s string) bool {
	var hasWord, hasOther bool // hasWord: 字母/数字（含汉字）；hasOther: 空白/标点
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			hasWord = true
		} else {
			hasOther = true
		}
	}
	return !hasWord || !hasOther
}

func runeLen(s string) int { return len([]rune(s)) }

// clipRunes rune 安全截断（不切碎多字节字符）。
func clipRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}

func equalRunes(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func uniqueCount(ss []string) int {
	seen := make(map[string]struct{}, len(ss))
	for _, s := range ss {
		seen[s] = struct{}{}
	}
	return len(seen)
}
