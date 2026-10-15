// 逐请求 usage 记账、请求级日志环形、Token 多天窗口与按模型消耗聚合
// （引擎数据面；喂入方式见包注释，禁止 HTTP 自环）。
package stats

import (
	"sort"
	"time"

	"wild-work/internal/provider"
)

// minGenSecForTput 计入 per-model 吞吐聚合的最小生成耗时：
// 50ms 对应 >20000 tok/s 的瞬时速度，任何真实模型都达不到；低于它的样本
// 只可能是计时口径伪影（非流聚合回包、时钟量化），计入只会拉高平均值。
const minGenSecForTput = 0.05

// ringCap 请求日志环形容量（内存环形，不持久化）。
const ringCap = 128

// ReqRow 请求级日志行（内存环形，不持久化）。
type ReqRow struct {
	Seq       int64   `json:"seq"`
	Time      string  `json:"time"`
	Model     string  `json:"model"`
	Mode      string  `json:"mode"` // "流" / "非流"
	Status    int     `json:"status"`
	TTFBMs    int64   `json:"ttfb_ms"`
	Tok       int64   `json:"tok"` // completion tokens（上游 usage 口径）
	TokPerSec float64 `json:"tok_per_sec"`
	TotalSec  float64 `json:"total_sec"`
	Credit    float64 `json:"credit"` // 本次精确积分（有 usage 帧积分的渠道），0 = 无
}

// ModelStat 今日按模型消耗（Credit 0 由前端区分显示）。
//
// Credit 口径分两类：WorkBuddy 系为 usage 帧透传的**精确值**；TraeWork 系上游不下发
// 积分，由 allocateTraeCreditsLocked 按「渠道真实消耗 × 模型 token 占比」**分摊**
// （估算，前端加 "≈" 前缀）。两类相加要与「花费积分」卡片口径一致时以平台表为准。
//
// 性能三项为「成功记账请求」的实测值（口径见 modelEntry）：
//   - AvgTTFBMs：首字节耗时算术平均（毫秒）；
//   - AvgTokPerSec：生成 token 合计 ÷ 生成耗时合计（按时长加权的平均吞吐）；
//   - PerfSamples：样本数（= 参与 TTFB 平均的请求数，前端做悬停提示用）。
//
// 无样本时 AvgTTFBMs/AvgTokPerSec 为 0，前端显示 "—"（不解为 0 耗时/0 速度）。
type ModelStat struct {
	Model        string  `json:"model"`
	Reqs         int64   `json:"reqs"`
	Tokens       int64   `json:"tokens"`
	Credit       float64 `json:"credit"`
	AvgTTFBMs    int64   `json:"avg_ttfb_ms"`
	AvgTokPerSec float64 `json:"avg_tok_per_sec"`
	PerfSamples  int64   `json:"perf_samples"`

	// 缓存利用率（今日）：CR = 命中 token 合计，CRCov = **上报该维度**请求的
	// prompt 合计（分母），CRReqs = 上报请求数。CRRov==0 表示今日无样本，
	// 前端显示「—」而不显示 0%（与「命中率真的为 0」区分开）。
	// 口径与语义同 ledger.UsageEntry 的 CR/CRC，取数点见 server/cacheusage.go。
	CR     int64 `json:"cr,omitempty"`
	CRCov  int64 `json:"cr_cov,omitempty"`
	CRReqs int64 `json:"cr_reqs,omitempty"`
}

// modelEntry per-model 今日累计（消耗 + 性能采样）。
// 性能采样只统计「usage 记账成功」的请求（与表内「次数」列同口径，失败/截断请求
// 不计，否则秒回错误会把首字节均值拉低成假象）：
//   - ttfbSumMs / perfCnt：首字节耗时的分子与分母（算术平均）；
//   - genTokens / genSec：生成 token 与生成耗时（总时长 − 首字节）合计，两者相除即
//     按时长加权的吞吐，避免短请求把均值拉偏。genSec<50ms 或 completion=0 不计入。
type modelEntry struct {
	reqs   int64
	tokens int64
	credit float64

	// 缓存维度：cr = 命中 token 合计；crCov = **上报该维度**请求的 prompt 合计
	// （分母）；crReqs = 上报请求数。crCov==0 ⇒ 该模型无样本 ⇒ 面板显示「—」。
	// 语义与口径详见 ledger.UsageEntry 的 CR/CRC 注释与 server/cacheusage.go。
	cr     int64
	crCov  int64
	crReqs int64

	ttfbSumMs int64
	perfCnt   int64
	genTokens int64
	genSec    float64
}

// AddUsage 记录一次代理请求的 usage（全局精确口径 + per-model 消耗与性能采样）。
// prompt/completion 为上游 usage 帧的 token 数；credit 为本次精确积分（无则 0）。
// cacheReported 表示上游**是否上报**了缓存命中字段；cacheHit 为命中 token 数
// （prompt 的子集）。仅 cacheReported 时才进入缓存利用率的分母/分子。
// ttfbMs 为首字节耗时（毫秒）；genSec 为生成耗时（总时长 − 首字节，秒），
// <=50ms 或 completion=0 时只计入 TTFB 平均、不计入吞吐（与请求日志 tok/s 判定
// 同口径）。异常负值帧钳 0 拒记，防止污染今日口径与跨天后的历史 map。
func (s *Stats) AddUsage(model string, prompt, completion int64, credit float64, ttfbMs int64, genSec float64, cacheHit int64, cacheReported bool, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rollDayLocked(now)
	if prompt < 0 {
		prompt = 0
	}
	if completion < 0 {
		completion = 0
	}
	if credit < 0 {
		credit = 0
	}
	if ttfbMs < 0 {
		ttfbMs = 0
	}
	if cacheHit < 0 {
		cacheHit = 0
	}

	s.tokensToday += prompt + completion
	s.reqsToday++
	s.creditOutExact += credit
	if model == "" {
		model = "unknown"
	}
	m, ok := s.models[model]
	if !ok {
		m = &modelEntry{}
		s.models[model] = m
	}
	m.reqs++
	m.tokens += prompt + completion
	m.credit += credit
	// 缓存：只有上游明确上报该维度时才计入（未上报的请求不稀释命中率）
	if cacheReported {
		m.cr += cacheHit
		m.crCov += prompt
		m.crReqs++
	}
	m.ttfbSumMs += ttfbMs
	m.perfCnt++
	if genSec >= minGenSecForTput && completion > 0 {
		m.genTokens += completion
		m.genSec += genSec
	}
	s.dirty = true
}

// AddRequestRow 追加请求级日志行（内存环形，不持久化、不置 dirty）。
func (s *Stats) AddRequestRow(row ReqRow) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	row.Seq = s.seq
	s.ring = append(s.ring, row)
	if len(s.ring) > ringCap {
		s.ring = s.ring[len(s.ring)-ringCap:]
	}
}

// Logs 返回最近 limit 行（按时间正序，最新在末尾）。
func (s *Stats) Logs(limit int) []ReqRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := s.ring
	if len(rows) > limit {
		rows = rows[len(rows)-limit:]
	}
	out := make([]ReqRow, len(rows))
	copy(out, rows)
	return out
}

// channelOfModel 取模型名的渠道前缀，返回 (渠道 kind, 是否在白名单内)。
// 解析与 provider.SplitModelID 同源：上游命名空间写法（cline-free/x）归到其渠道（cline），
// 否则统计会把 cline 的 token 漏掉（R59）。
func channelOfModel(model string) (string, bool) {
	if k, _, ok := provider.SplitModelID(model); ok {
		g := k.String()
		return g, IsKnownChannel(g)
	}
	return "", false
}

// modelStatsLocked 生成今日按模型消耗行（调用方须持锁；积分降序）。
func (s *Stats) modelStatsLocked() []ModelStat {
	models := make([]ModelStat, 0, len(s.models))
	for name, m := range s.models {
		st := ModelStat{Model: name, Reqs: m.reqs, Tokens: m.tokens, Credit: m.credit}
		// 缓存透出（0 值省略在 JSON 里，前端据 cr_cov 判定样本量）
		if m.crCov > 0 {
			st.CR, st.CRCov, st.CRReqs = m.cr, m.crCov, m.crReqs
		}
		if m.perfCnt > 0 {
			st.AvgTTFBMs = m.ttfbSumMs / m.perfCnt
			st.PerfSamples = m.perfCnt
		}
		if m.genSec > 0 {
			st.AvgTokPerSec = float64(m.genTokens) / m.genSec
		}
		models = append(models, st)
	}
	// TraeWork 无精确积分：上游 usage 帧不含积分字段，改用该渠道今日真实消耗
	// （账号余额差值口径）按各模型 token 占比分摊。须在排序前写入，否则积分列
	// 降序与实际显示值不一致。
	s.allocateTraeCreditsLocked(models)
	sort.Slice(models, func(i, j int) bool {
		if models[i].Credit != models[j].Credit {
			return models[i].Credit > models[j].Credit
		}
		if models[i].Tokens != models[j].Tokens {
			return models[i].Tokens > models[j].Tokens
		}
		return models[i].Reqs > models[j].Reqs
	})
	return models
}

// allocateTraeCreditsLocked 把 Trae 系渠道（TraeWork + TraeCode）今日真实消耗按各模型
// token 占比分摊到模型行（调用方须持锁）。
//
// 背景：TraeWork 的 usage 帧只有 prompt/completion token，**没有积分字段**，故无逐请求
// 精确值；「token × 渠道费率」折算与上游实际扣费对不上（各模型分别计价且带倍率）。
// 唯一可靠的真实数是账号余额差值（creditOut，快照粒度）——因此本函数给出「渠道合计
// 真实、模型间为估算」的分摊：各行之和恒等于平台表该渠道的 CreditUsed（净值），
// 与「今日消耗」口径闭合。前端以 "≈" 前缀标注为估算值。
//
// TraeCode（`traecode/*`）与 TraeWork 共用同一批账号与积分池（一个 trPool 服务两个
// 渠道，见 cmd/wild-work 装配），其消耗同样落在这批账号的余额差值上，故**必须并入
// 同一分摊池**——只认 traework 会让 traecode 的消耗在模型表里凭空消失。
//
// 无消耗或无 token（无从定权重）时保持原值，避免除零与伪造 0。
// 分摊基数为**净值**：差值总额里先扣掉 trae 系今日作废（ledger expire）——
// 差值口径分不出「真消耗」与「积分包到期作废」，不扣就会把作废摊成模型消耗。
// 作废额缺失时基数退回差值总额，即拆分前的行为，不会算出负数。
func (s *Stats) allocateTraeCreditsLocked(models []ModelStat) {
	var channelOut int64
	for _, a := range s.accts {
		if isTraeKind(a.group) {
			channelOut += a.creditOut
		}
	}
	if channelOut > 0 {
		var traeExpired int64
		for g := range traeKinds {
			traeExpired += s.expiredByGroup[g]
		}
		channelOut -= traeExpired
		if channelOut < 0 {
			channelOut = 0
		}
	}
	if channelOut <= 0 {
		return
	}
	var tokens int64
	for _, m := range models {
		if g, ok := channelOfModel(m.Model); ok && isTraeKind(g) {
			tokens += m.Tokens
		}
	}
	if tokens <= 0 {
		return
	}
	perToken := float64(channelOut) / float64(tokens)
	for i := range models {
		if g, ok := channelOfModel(models[i].Model); ok && isTraeKind(g) {
			models[i].Credit = float64(models[i].Tokens) * perToken
		}
	}
}

// windowLocked 返回历史 map（不含今日）中窗口=前 N-1 天的 tokens 和。
// YYYY-MM-DD 字典序即时序，可直接字符串比较。
func (s *Stats) windowLocked(now time.Time, days int) int64 {
	start := now.AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	var sum int64
	for k, v := range s.tokenDays {
		if k >= start {
			sum += v
		}
	}
	return sum
}

// reqSumLocked 历史（不含今日）请求总数（全量累计）。
func (s *Stats) reqSumLocked() int64 {
	var sum int64
	for _, v := range s.reqDays {
		sum += v
	}
	return sum
}
