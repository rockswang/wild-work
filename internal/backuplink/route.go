// route.go 「地区限制 → 备用上游通道重放」的公共编排 + 学习路由。
//
// 为什么放在 backuplink 而不是各渠道各写一份：这条编排有一套**语义很细**的规则
//
//	（首因不得被掩盖、备用出口同样受限时不记学习结果、备用拨不动要回落直连、
//	 学习结果随配置变更作废）——分散在两处必然改一处漏一处（R52 的教训：判据分裂
//	 就出过真 bug）。渠道只提供两件渠道相关的东西：怎么发一发请求、什么算「地区限制」。
//
// 当前使用方：cline（上游 403 `not available in your region`）与
// oczen（上游 403 `RegionError`，见 R62 的补充）。
package backuplink

import (
	"errors"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// ErrNoBackup 表示「要求走备用出口，但面板未配置备用通道」。
// Attempt 回调应当返回它（而不是普通错误），编排据此区分「未配置」与「备用拨不动」：
// 前者是按设计原样透传，后者是故障但同样不掩盖首因，两者日志文案不同。
var ErrNoBackup = errors.New("备用上游通道未配置")

// routeTTL 学习结果有效期：地区限制是稳定属性（地理 + 账号 + 模型），取长一些，
// 避免每发都先白撞一次直连；到期后重新试探（上游若解除限制即自动回归直连）。
const routeTTL = 6 * time.Hour

// Routes 是「该模型只能走备用出口」的学习路由（每渠道各持一份，零值可用）。
// 与 backuplink 全局配置联动：Generation 一变（用户改/清代理）即整体作废。
type Routes struct {
	mu  sync.RWMutex
	m   map[string]time.Time
	gen uint64 // 写入时的 backuplink.Generation()
}

// Learned 报告该模型是否已学到「走备用出口」，且备用配置仍生效。
func (r *Routes) Learned(model string) bool {
	if !Enabled() || model == "" {
		return false
	}
	g := Generation()
	now := time.Now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	exp, ok := r.m[model]
	return ok && g == r.gen && now.Before(exp)
}

// Mark 记录/续期学习结果；备用配置一变（generation 变）就整体作废重建。
func (r *Routes) Mark(model string) {
	g := Generation()
	if g == 0 || model == "" {
		return
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]time.Time{}
	}
	if r.gen != g {
		clear(r.m)
		r.gen = g
	}
	r.m[model] = now.Add(routeTTL)
	for k, t := range r.m { // 顺带清过期项，防长期碎片
		if now.After(t) {
			delete(r.m, k)
		}
	}
}

// Forget 作废单个模型的学习结果（备用链路拨不动 / 备用出口也受限时回落直连用）。
func (r *Routes) Forget(model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, model)
}

// Replay 执行一次「直连优先、命中地区限制则换备用出口重放」的完整编排。
//
// 调用方只需提供渠道相关的三件事：
//   - Attempt：用给定出口（backup=false 直连 / true 备用）发**一发**请求并返回响应。
//     必须可重放——编排可能调用它 1~3 次，调用方每次都新建请求（body 已在内存，无需 GetBody）。
//     备用出口未配置时返回 ErrNoBackup。
//   - Blocked：该渠道「地区限制」的判据（各上游文案不同，见各渠道实现）。
//   - Wrap：可选，包装成功响应（如流式空闲兜底 / 取消所有权转移）。
//
// 语义保证（均有回归测试）：① 未配/拨不动/备用同样受限 ⇒ 交出**直连那份**原文
// （首因不被第二次尝试掩盖）；② 备用链路自身不可用 ⇒ 作废学习结果并回落直连；
// ③ 只有「备用确实给出了可用答复」才记学习结果；④ 只换出口 IP，不涉及账号。
type Replay struct {
	Kind    string  // 渠道名（日志前缀）
	Model   string  // 学习路由的键（上游模型名）
	Routes  *Routes // 非 nil 时启用学习路由
	Attempt func(useBackup bool) (*http.Response, error)
	Blocked func(status int, body string) bool
	// Wrap 可选：成功分支的响应包装（nil = 直接用 resp.Body）。
	Wrap func(resp *http.Response) io.ReadCloser
	// Finalize 可选：交出「非 200 的上游应答」之前调用（cline 用它标记共享上限冷却）。
	Finalize func(status int, body string)
	// Translate 可选：把上游状态码还原为内层真实码（cline 的网关会把真码压平成 500）。
	// nil 时原样返回。
	Translate func(status int, body string) int
	// Note 可选：交出结果前回调，报告本次答复是否来自**备用出口**。
	// 用途：面板「测试」按钮要如实告诉用户「代理到底有没有被用上」（oczen 的 issue #117）。
	// 注意与 Routes 的区别——Routes 记的是「该模型以后就走备用」的学习结果持久态，
	// 本钩子只描述**这一次**请求。
	Note func(viaBackup bool)
	// MaxErrBody 可选：非 200 响应体读取上限（字节）。0 = 默认 64KB。
	// oczen 旧实现用 1MB（上游错误详情偶尔较长），这里保留其原值以免削弱「原文透传」。
	MaxErrBody int64
}

// errBodyLimit 错误体读取上限默认值（各渠道原实现均不小于此）。
const errBodyLimit = 64 << 10

// limit 返回本次编排的错误体读取上限。
func (r *Replay) limit() int64 {
	if r.MaxErrBody > 0 {
		return r.MaxErrBody
	}
	return errBodyLimit
}

// Do 执行编排，返回 (流, 状态码, 错误体, 传输层错误)。
// 200 分支返回可读流且 body 为 nil；非 200 分支返回已读尽的原文。
//
// 所有出口路径统一收口到 viaBackup := false + defer 回调（见 Note）：本函数有 8 处 return，
// 逐处手工标记「这次答复来自哪个出口」必会漏（R52 教训：判据分裂就出过真 bug），
// 故只在「交出备用答复」的两处把 via 置 true，由 defer 统一上报。
func (r *Replay) Do() (io.ReadCloser, int, []byte, error) {
	via := false
	defer func() {
		if r.Note != nil {
			r.Note(via)
		}
	}()
	logf := func(format string, args ...any) { log.Printf("%s: "+format, append([]any{r.Kind}, args...)...) }

	useBackup := r.Routes != nil && r.Routes.Learned(r.Model)
	resp, err := r.Attempt(useBackup)
	if err != nil {
		// 备用拨不动（代理挂了/地址错）⇒ 作废学习结果并回落直连一次，
		// 否则代理一故障就把「本来能直连」的模型钉死不可用。
		link := "直连"
		if useBackup {
			link = "备用"
		}
		logf("首选链路(%s)不可用 model=%s err=%v（作废学习结果，回落直连）", link, r.Model, err)
		if r.Routes != nil {
			r.Routes.Forget(r.Model)
		}
		if resp, err = r.Attempt(false); err != nil {
			return nil, 0, nil, err
		}
		useBackup = false
	}
	if resp.StatusCode == http.StatusOK {
		via = useBackup
		if useBackup {
			r.Routes.Mark(r.Model) // 备用出口确认可用，续期学习结果
		}
		return r.wrap(resp), resp.StatusCode, nil, nil
	}
	status, raw := r.drain(resp)

	// (A) 走备用仍被地区拒 ⇒ 上游策略或代理出口已变 ⇒ 作废学习结果并回落直连；
	// 不做这层的话最长要等 TTL 到期才回到直连，期间该模型一直报错。
	if useBackup && r.Blocked(status, string(raw)) {
		if r.Routes != nil {
			r.Routes.Forget(r.Model)
		}
		r2, e2 := r.Attempt(false)
		if e2 != nil {
			logf("备用被地区拒且直连拨不动 model=%s backup=%d err=%v（交出备用那份）", r.Model, status, e2)
		} else if r2.StatusCode == http.StatusOK {
			logf("直连已恢复可用 model=%s（学习结果已作废）", r.Model)
			return r.wrap(r2), r2.StatusCode, nil, nil
		} else {
			status, raw = r.drain(r2) // 交出直连那份（主链路口径），不再乒乓重试
		}
		r.finalize(status, raw)
		return nil, r.translate(status, string(raw)), raw, nil
	}

	// (B) 直连被上游按地区拒 ⇒ 同请求内经备用通道重放一次。
	if r.Blocked(status, string(raw)) {
		r2, e2 := r.Attempt(true)
		switch {
		case errors.Is(e2, ErrNoBackup):
			// 未配置备用通道：原样透传（不罚账号）。日志点名，避免用户以为功能生效了。
			logf("上游按地区拒绝 model=%s（未配置备用上游代理，原样透传）：%s", r.Model, truncate(string(raw), 120))
		case e2 != nil:
			// R37 同族：不得用第二次尝试的错误掩盖首因 ⇒ 仍透传直连那份原文，代理故障只进日志。
			logf("备用通道重放失败 model=%s err=%v（透传直连原始响应）", r.Model, e2)
		case r2.StatusCode == http.StatusOK:
			r.Routes.Mark(r.Model)
			via = true // 答复来自备用出口
			logf("地区限制已改走备用通道 model=%s（后续同模型直接走备用）", r.Model)
			return r.wrap(r2), r2.StatusCode, nil, nil
		default:
			s2, raw2 := r.drain(r2)
			if r.Blocked(s2, string(raw2)) {
				// 备用出口同样受限 ⇒ **不**记学习结果（钉上去只会长期失败），仍透传直连首因。
				logf("备用通道出口同样被地区拒绝 model=%s（透传直连原始响应）", r.Model)
			} else {
				// 链路可达、上游给了**不同**的权威答复 ⇒ 以它为准，并记学习结果。
				r.Routes.Mark(r.Model)
				via = true // 答复来自备用出口（即使非 200，用户也需要知道「代理被用上了」）
				r.finalize(s2, raw2)
				// 日志附答复前 160 字符：排障时「备用出口到底说了什么」是最关键的信息
				// （实盘例子：muse 经某出口回 400 ModelProtocolUnsupported）。
				logf("经备用通道拿到上游答复 model=%s status=%d body=%s", r.Model, s2, truncate(string(raw2), 160))
				return nil, r.translate(s2, string(raw2)), raw2, nil
			}
		}
	}

	r.finalize(status, raw)
	return nil, r.translate(status, string(raw)), raw, nil
}

func (r *Replay) wrap(resp *http.Response) io.ReadCloser {
	if r.Wrap != nil {
		return r.Wrap(resp)
	}
	return resp.Body
}

func (r *Replay) finalize(status int, raw []byte) {
	if r.Finalize != nil {
		r.Finalize(status, string(raw))
	}
}

func (r *Replay) translate(status int, body string) int {
	if r.Translate != nil {
		return r.Translate(status, body)
	}
	return status
}

// drain 读尽并关闭非 200 响应体，返回 (状态码, 原文)。
func (r *Replay) drain(resp *http.Response) (int, []byte) {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, r.limit()))
	_ = resp.Body.Close()
	return resp.StatusCode, raw
}

// truncate 截断日志正文（日志里不落完整上游错误体）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
