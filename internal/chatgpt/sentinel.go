package chatgpt

import (
	"encoding/base64"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/chatgpt/sentinel/pow"
	"wild-work/internal/chatgpt/sentinel/vm"
	"wild-work/internal/provider"
)

// sentinel 对抗算法链路（L1→L3）：首页 → prepare → finalize。
//
// 上游对「请求质量不足」的对话请求有两种伪装形态（都不会直说原因）：
//   - 缺失证据：403 `{"detail":"Unusual activity has been detected from your device"}`
//   - 质量偏低：**伪装成** 404 `{"detail":"Model not found"}`（容易被误判为模型不存在）
//
// 三层证据链（缺一层就过不去）：
//  1. TLS 指纹（transport.go）；
//  2. HTTP 头族顺序与取值（transport.go）；
//  3. sentinel 三令牌：requirements(p) + proofofwork(FNV-1a) + turnstile(挑战 VM)。
//
// 本文件只做第 3 层的编排；PoW 与 VM 分别在 sentinel/pow 与 sentinel/vm。
// 全程纯 Go，不需要浏览器 sidecar。
//
// **fail-closed**：dx 出现 Go VM 不认识的 opcode、或指纹结构漂移时立即返回错误，
// **不发业务请求**（否则会拿到伪装 404 或登号风险），且**不落盘新基线**
// （一旦覆盖基线，第二次运行就看不到漂移了——那正是「升级不自知」的最坏形态）。

// sentinel 两级路径前缀（登录 / 匿名）。匿名链路已裁剪（spec §2），只保留登录。
const (
	PathSentinelPrepare  = "/backend-api/sentinel/chat-requirements/prepare"
	PathSentinelFinalize = "/backend-api/sentinel/chat-requirements/finalize"
)

var (
	reDataBuild = regexp.MustCompile(`data-build="([^"]*)"`)
	reScriptSrc = regexp.MustCompile(`script\.src = '([^']*)'`)
	reScriptTag = regexp.MustCompile(`src="([^"]*)"`)
	// reErrShape 匹配 VM 内部报错被 btoa 后的形态（"37: TypeError: …"）。
	reErrShape = regexp.MustCompile(`^\d+: `)
)

// HomeInfo 是首页探测结果（L1）。
//
// 它同时是**升级预警指纹**的第一项：data-build 变了说明前端重新发布过，
// 挑战程序与 Turnstile 模块都可能跟着变。
type HomeInfo struct {
	Status     int
	DataBuild  string   // 首页 data-build（前端构建号）
	Scripts    []string // 首页内联赋值的 script.src 列表
	SDKURL     string   // 第一个 script.src（通常是 sentinel sdk 引导器）
	Cloudflare bool     // 是否被 CF 托管挑战拦住
	HTMLSize   int
}

// FetchHome 抓首页拿 data-build 与 script src（零配额）。
func (c *Client) FetchHome(a *auth.Auth) (*HomeInfo, error) {
	st, body, err := c.Do(a, "GET", "/", nil, nil)
	if err != nil {
		return nil, err
	}
	info := &HomeInfo{Status: st, HTMLSize: len(body)}
	html := string(body)
	if m := reDataBuild.FindStringSubmatch(html); len(m) > 1 {
		info.DataBuild = m[1]
	}
	for _, m := range reScriptSrc.FindAllStringSubmatch(html, -1) {
		info.Scripts = append(info.Scripts, m[1])
	}
	if len(info.Scripts) == 0 { // 兜底：有些版本用普通 src= 属性
		for _, m := range reScriptTag.FindAllStringSubmatch(html, -1) {
			if strings.Contains(m[1], "/sentinel/") || strings.Contains(m[1], "challenge-platform") {
				info.Scripts = append(info.Scripts, m[1])
			}
		}
	}
	if len(info.Scripts) > 0 {
		info.SDKURL = info.Scripts[0]
	}
	// CF 挑战判据（与参考实现同口径）：403 + HTML 页。注意它**不是**对话请求的判据，
	// 只是给运维一个「换出口 IP / 补 cf_clearance」的定位线索。
	info.Cloudflare = st == 403 && strings.Contains(strings.ToLower(html), "<html")
	if st != 200 {
		return info, StatusError("GET", "/", st, body)
	}
	return info, nil
}

// PowInfo 是 prepare 响应里的 proofofwork 段。
type PowInfo struct {
	Required   bool
	Seed       string
	Difficulty string
}

// TurnstileInfo 是 prepare 响应里的 turnstile 段。
type TurnstileInfo struct {
	Required bool
	DX       string
}

// PrepareInfo 是 prepare 的完整结果。
type PrepareInfo struct {
	Status       int
	PrepareToken string
	Pow          PowInfo
	Turnstile    TurnstileInfo
	Arkose       bool
	// So 是 so 段（当前上游不强制；**这是最好的「即将升级」观测点**）。
	So map[string]any
	// Keys 是响应顶层键（升级预警指纹：键集变了说明协议改了）。
	Keys []string
	Raw  map[string]any
}

// Prepare 走 sentinel prepare（零配额）。
//
// pToken 是 requirements token（pow.BuildLegacyRequirementsToken 产出）。
func (c *Client) Prepare(a *auth.Auth, pToken string) (*PrepareInfo, error) {
	st, m, raw, err := c.DoJSON(a, "POST", PathSentinelPrepare,
		map[string]string{"p": pToken}, map[string]string{"content-type": "application/json"})
	if err != nil {
		return nil, err
	}
	pi := &PrepareInfo{Status: st, Raw: m}
	if st != 200 {
		return pi, StatusError("POST", PathSentinelPrepare, st, raw)
	}
	pi.PrepareToken, _ = m["prepare_token"].(string)
	if pw, ok := m["proofofwork"].(map[string]any); ok {
		pi.Pow.Required, _ = pw["required"].(bool)
		pi.Pow.Seed, _ = pw["seed"].(string)
		pi.Pow.Difficulty, _ = pw["difficulty"].(string)
	}
	if ts, ok := m["turnstile"].(map[string]any); ok {
		pi.Turnstile.Required, _ = ts["required"].(bool)
		pi.Turnstile.DX, _ = ts["dx"].(string)
	}
	if ak, ok := m["arkose"].(map[string]any); ok {
		pi.Arkose, _ = ak["required"].(bool)
	}
	if so, ok := m["so"].(map[string]any); ok {
		pi.So = so
	}
	for k := range m {
		pi.Keys = append(pi.Keys, k)
	}
	sort.Strings(pi.Keys)
	if pi.Arkose {
		return pi, precheckErr("上游要求 arkose token（未实现）：本渠道无法满足该挑战")
	}
	if pi.PrepareToken == "" {
		return pi, precheckErr("prepare 未下发 prepare_token")
	}
	return pi, nil
}

// FinalizeResult 是 finalize 的结果。
type FinalizeResult struct {
	Token       string // 对话用的 sentinel token（一次性！复用即 403）
	SoToken     string // 当前恒空（上游未强制）
	ExpireAfter int64
	ExpireAt    int64
	Keys        []string
}

// Finalize 提交 PoW + Turnstile 换 token（零配额）。
//
// **两道挑战互为前置**：PoW 无解或 Turnstile 校验不过时，调用方**不得**把另一道
// 的「成功」当作整体通过——本方法只负责把两道结果一起提交，任一为空上游即拒。
func (c *Client) Finalize(a *auth.Auth, pi *PrepareInfo, proof, turnstile string) (*FinalizeResult, error) {
	body := map[string]string{
		"prepare_token": pi.PrepareToken,
		"proofofwork":   proof,
		"turnstile":     turnstile,
	}
	st, m, raw, err := c.DoJSON(a, "POST", PathSentinelFinalize, body,
		map[string]string{"content-type": "application/json"})
	if err != nil {
		return nil, err
	}
	if st != 200 {
		return nil, StatusError("POST", PathSentinelFinalize, st, raw)
	}
	res := &FinalizeResult{}
	res.Token, _ = m["token"].(string)
	res.SoToken, _ = m["so_token"].(string)
	if f, ok := m["expire_after"].(float64); ok {
		res.ExpireAfter = int64(f)
	}
	if f, ok := m["expire_at"].(float64); ok {
		res.ExpireAt = int64(f)
	}
	for k := range m {
		res.Keys = append(res.Keys, k)
	}
	sort.Strings(res.Keys)
	if res.Token == "" {
		return res, precheckErr("finalize 未下发 token（多半是 sentinel 质量不足）")
	}
	return res, nil
}

// Sentinel 是一次完整的 L1→L3 预检结果。
type Sentinel struct {
	Home        *HomeInfo
	ReqToken    string // finalize 下发的 token（一次性）
	Proof       string
	Turnstile   string
	DXOps       int      // dx 指令数
	DXAlias     int      // 浮点别名寄存器数
	DXUnknown   []string // 未知 opcode（非空 = 上游升级，VM 需要更新）
	VMSteps     int
	VMElapsedMs int64
	PowElapsed  int64
	// 分段耗时（毫秒）：定位「首字慢」到底慢在哪一步，也是缓存收益的依据。
	HomeMs, PrepareMs, FinalizeMs int64
	HomeCached                    bool // 首页是否走了缓存
	// TokenTTL 是 finalize 下发的 token 寿命（秒）；TokenExpire 是绝对时间（Unix 秒）。
	TokenTTL    int64
	TokenExpire int64
	// PrepareKeys / FinalizeKeys / Difficulty / SoPresent 是升级预警指纹。
	PrepareKeys  []string
	FinalizeKeys []string
	Difficulty   string
	SoPresent    bool
	// TokenCached / CacheAgeSec 表示本次 token 来自预取缓存（复刻浏览器并行预取）。
	TokenCached bool
	CacheAgeSec int64
	// Drift 是跨轮指纹对比结果（见 fingerprint.go）；nil 表示未启用或首轮无基线。
	Drift *Drift
}

// Headers 把 sentinel 三令牌渲染成对话请求要带的头。
//
// **位置固定**（上游按头名读）：requirements 恒有；proof / turnstile 只有上游要求时才带。
// 缺一即被判「请求质量不足」（403 或伪装 404）。conduit 预取那一步只带 requirements
// （与真实网页一致），由对话层按需取用 Headers() 的子集。
func (s *Sentinel) Headers() map[string]string {
	if s == nil {
		return nil
	}
	h := map[string]string{}
	if s.ReqToken != "" {
		h["openai-sentinel-chat-requirements-token"] = s.ReqToken
	}
	if s.Proof != "" {
		h["openai-sentinel-proof-token"] = s.Proof
	}
	if s.Turnstile != "" {
		h["openai-sentinel-turnstile-token"] = s.Turnstile
	}
	return h
}

// sentinelEnv 返回 VM 宿主环境参数（与 session 的身份一致，否则指纹自相矛盾）。
func sentinelEnv(ua string) vm.Env {
	return vm.Env{
		UserAgent:      ua,
		Language:       "en-US",
		Languages:      "en,en-GB,en-US",
		HardwareConcur: 16,
		ScreenW:        2560,
		ScreenH:        1440,
		HeapLimit:      4395630592,
		TimezoneOffset: -480,
	}
}

// RunSentinel 取一个可用 token：优先消费预取缓存，否则完整跑 L1→L3。
func (c *Client) RunSentinel(a *auth.Auth) (*Sentinel, error) {
	if s, ok := c.takeTokenCache(a); ok {
		return s, nil
	}
	return c.runSentinelFull(a)
}

// runSentinelFull 无条件跑完整 L1→L3（预取也走这条）。
//
// **fail-closed**：dx 里出现 Go VM 不认识的 opcode、或指纹结构漂移时立即返回错误，
// 不发业务请求（否则会拿到伪装 404 或封号）。
func (c *Client) runSentinelFull(a *auth.Auth) (*Sentinel, error) {
	s := &Sentinel{}
	sess, err := c.session(a)
	if err != nil {
		return s, err
	}
	ua := sess.ua

	// L1 首页：拿 data-build 与 script src（p token 的 [5] 字段要一个真实 script src）。
	// 首页是唯一可缓存的步骤（data-build 在几小时内不会变），见 homecache.go。
	t0home := time.Now()
	home, cached, err := c.fetchHomeCached(a)
	s.Home, s.HomeCached = home, cached
	s.HomeMs = time.Since(t0home).Milliseconds()
	if err != nil {
		return s, err
	}
	if home.Cloudflare {
		return s, precheckErr("首页被 Cloudflare 托管挑战拦住（换出口 IP 或带 cf_clearance cookie）")
	}

	// L2 prepare：p token 由 25 字段 config 拼出（PoW 与 VM 的 key 都是它）。
	pToken := pow.BuildLegacyRequirementsToken(ua, home.Scripts, home.DataBuild, nil)
	t0prep := time.Now()
	pi, err := c.Prepare(a, pToken)
	s.PrepareMs = time.Since(t0prep).Milliseconds()
	if err != nil {
		return s, err
	}

	// PoW（FNV-1a，纯 Go，6 位 difficulty 也在毫秒级）。
	if pi.Pow.Required {
		t0 := time.Now()
		ans, ok := pow.Solve(pi.Pow.Seed, pi.Pow.Difficulty, ua, nil)
		s.PowElapsed = time.Since(t0).Milliseconds()
		if !ok {
			return s, precheckErr("PoW 无解（seed=%q difficulty=%q）", pi.Pow.Seed, pi.Pow.Difficulty)
		}
		s.Proof = ans
	}

	// Turnstile（挑战 VM：先离线枚举 opcode 做 fail-closed，再真跑）。
	var unknown []string
	if pi.Turnstile.Required {
		n, direct, alias, err := vm.Analyze(pi.Turnstile.DX, pToken)
		if err != nil {
			return s, precheckErr("dx 解码失败: %v", err)
		}
		s.DXOps, s.DXAlias = n, len(alias)
		for k := range direct {
			if !vm.Supported(k) {
				unknown = append(unknown, strconv.FormatFloat(k, 'g', -1, 64))
			}
		}
		sort.Strings(unknown)
		s.DXUnknown = unknown
		if len(unknown) > 0 {
			return s, c.blockDrift(s, "fatal", []string{"dx 出现 Go VM 不认识的 opcode " + strings.Join(unknown, ", ") + "（上游升级，需更新 VM）"})
		}
		t0 := time.Now()
		tok, steps, err := vm.Solve(pi.Turnstile.DX, pToken, 500, vm.NewWindow(sentinelEnv(ua)))
		s.VMSteps, s.VMElapsedMs = steps, time.Since(t0).Milliseconds()
		if err != nil {
			return s, precheckErr("Turnstile VM 求解失败: %v", err)
		}
		if bad := validateTurnstileToken(tok, pToken); bad != "" {
			return s, precheckErr("Turnstile token 校验不通过: %s（token=%s）", bad, truncate(tok, 120))
		}
		s.Turnstile = tok
	}

	// L3 finalize。
	t0fin := time.Now()
	fr, err := c.Finalize(a, pi, s.Proof, s.Turnstile)
	s.FinalizeMs = time.Since(t0fin).Milliseconds()
	if err != nil {
		return s, err
	}
	s.ReqToken = fr.Token
	s.TokenTTL, s.TokenExpire = fr.ExpireAfter, fr.ExpireAt
	s.PrepareKeys, s.FinalizeKeys = pi.Keys, fr.Keys
	s.Difficulty, s.SoPresent = pi.Pow.Difficulty, len(pi.So) > 0

	// L4 升级预警：与上次快照对比（零额外请求，全用刚拿到的字段）。
	cur := fpOf(s)
	drift := diffFingerprint(c.loadFingerprint(a), cur, unknown)
	s.Drift = drift
	if drift.Level == "block" || drift.Level == "fatal" {
		// **不能在这里落盘新快照**：一旦覆盖基线，第二次运行就「看不到漂移」了，
		// 门禁只生效一次——这恰好是「升级不自知」的最坏形态。
		return s, c.blockDrift(s, drift.Level, drift.Reason)
	}
	c.saveFingerprint(a, cur)
	return s, nil
}

// blockDrift 记录可检索日志并构造 fail-closed 错误（未知 opcode 与结构漂移共用）。
func (c *Client) blockDrift(s *Sentinel, level string, reasons []string) error {
	reasonText := strings.Join(reasons, "; ")
	log.Printf("chatgpt sentinel drift level=%s data_build=%s dx_ops=%d unknown=%v reasons=%s",
		level, homeBuild(s), s.DXOps, s.DXUnknown, reasonText)
	return &SentinelDriftError{Level: level, Reasons: reasons}
}

func homeBuild(s *Sentinel) string {
	if s == nil || s.Home == nil {
		return ""
	}
	return s.Home.DataBuild
}

// validateTurnstileToken 复刻参考实现的四道拒绝。
//
// 这四道都是**静默失效**的形态：VM 没算出真 token 时不会报错，而是回一个
// 「看起来像 base64」的东西。不校验就会把它当 token 发给上游，换来伪装 404。
func validateTurnstileToken(tok, seed string) string {
	if tok == "" {
		return "空 token"
	}
	if tok == seed {
		return "token == seed（回显了 p，WeakMap 钩子坏了）"
	}
	if isAllDigits(tok) {
		return "纯数字（VM 超时返回步数）"
	}
	if raw, err := base64.StdEncoding.DecodeString(tok); err == nil && reErrShape.MatchString(string(raw)) {
		return "VM 内部报错（base64 解出 'N: …'）"
	}
	return ""
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// precheckErr 是预检链路自身的失败（PoW 无解 / VM 出错 / 上游未下发令牌等）。
//
// 归 ErrUpstream：不换号、不罚账号（spec §10）——这类失败与账号健康度无关，
// 换号只会把同一面墙再撞一次。
func precheckErr(format string, args ...any) *provider.Error {
	return &provider.Error{Kind: provider.ErrUpstream, Msg: fmt.Sprintf(format, args...)}
}

// SentinelDriftError 表示上游对抗算法已升级（fail-closed 门禁触发）。
//
// 与 precheckErr 的区别：这不是「一次请求失败」，而是「本渠道的实现假设已失效」，
// 需要更新 VM / 协议对齐。对外文案必须让用户看懂该做什么，而不是一串莫名其妙的 404。
type SentinelDriftError struct {
	Level   string // "fatal"（未知 opcode）| "block"（结构漂移）
	Reasons []string
}

func (e *SentinelDriftError) Error() string {
	return "上游对抗算法已变化（" + e.Level + "）：" + strings.Join(e.Reasons, "; ") +
		"；本渠道需要升级，已拒绝发送对话请求"
}

// Unwrap 让 errors.As 仍能取到 provider 类别（ErrUpstream：不换号不罚号）。
func (e *SentinelDriftError) Unwrap() error { return precheckErr("%s", e.Error()) }
