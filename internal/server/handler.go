// Package server 暴露 OpenAI 兼容 HTTP 接口，按模型名前缀路由到不同上游。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/ledger"
	"wild-work/internal/loopguard"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	// 别名 statsEngine：与运行统计引擎包名一致（本包无同名局部变量，别名仅为与 app 包对齐可读）
	statsEngine "wild-work/internal/stats"
)

// Runtime 是一个平台的一组运行时资源：pool + upstream + 静态模型兜底。
type Runtime struct {
	Kind         provider.Kind
	Pool         *pool.Pool
	Upstream     provider.Upstream
	StaticModels []provider.ModelInfo

	// NoCooldownOnServerError 声明该渠道的上游 5xx 属于网关/基础设施抖动
	// （账号本身健康），不应计入账号错误触发冷却。
	// WorkBuddy 国际版开启（其 openresty 网关实测间歇性 502/503/504）。
	//
	// ⚠️ 2026-10-18 起本字段**不再决定计数行为**（5xx 已默认不计数，见
	// CountServerErrorAsAccountFault），仅保留作渠道差异的文档标注。
	NoCooldownOnServerError bool

	// CountServerErrorAsAccountFault 显式要求「本渠道的上游 5xx 计入账号错误」。
	//
	// 默认 false（5xx 不计入账号错误）：5xx 的语义是服务端故障，与客户端凭据健康无关。
	// 曾经的默认行为是「计入」，导致一次上游 500 风暴把整池健康账号打成
	// consecutive errors 冷却 10 分钟，整条渠道 503（2026-10-18 lobsterai 实测）。
	//
	// 仅当某渠道**确实用 5xx 表达账号级封禁/欠费**时才置 true —— 当前无一渠道如此。
	CountServerErrorAsAccountFault bool

	// SingleAccount 声明该渠道只有唯一一个、且**无法人工恢复**的账号（当前仅 oczen 匿名）。
	// 语义：任何账号级惩罚（冷却/计数/禁用）都等价于「整条渠道下线」，故一律不适用——
	// 出错就原文透传，把重试交给客户端。区别于 NoCooldownOnServerError（只豁免 5xx）。
	// 生效范围：
	//   - 传输层错误：不累计 errCount（否则 3 次网络抖动即冷却唯一账号）；
	//   - ErrSoftRate（429）：不冷却。单账号无号可轮换，冷却只会把后续请求挡在
	//     挑号阶段（返回 no_healthy_account），连「稍后重试」都做不到；透传 429
	//     才能让客户端按 Retry-After 自行重试；
	//   - 其余分类：统一不罚账号（ErrHardCredit 无余额概念、ErrSessionDead 不可重登）。
	SingleAccount bool

	mu       sync.RWMutex
	models   []provider.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

// Config handler 依赖。
type Config struct {
	Runtimes map[provider.Kind]*Runtime
	APIKey   string // 空 = 不鉴权

	// WebUI 为内嵌的静态 Web UI 文件系统（go:embed 产物）；非 nil 时挂载到 /
	WebUI fs.FS
	// AttachAPI 由调用方注册管理 API 路由（internal/app 的 HandleAPI）
	AttachAPI func(mux *http.ServeMux)

	// 兼容旧调用方：只传 Pool/Upstream 时等价于只启用 workbuddy。
	Pool     *pool.Pool
	Upstream provider.Upstream

	// Ledger 用量/积分流水记账器（非 nil 时成功请求记 token 流水）。
	Ledger *ledger.Ledger

	// Stats 运行统计引擎（非 nil 时转发请求插桩 AddUsage/AddRequestRow；见 internal/stats 包注释）。
	Stats *statsEngine.Stats

	// MaxRequestBody 请求体上限（字节）。0 = 用 DefaultMaxRequestBody（128MiB）。
	MaxRequestBody int64

	// StickyMaxRequests 同渠道粘性账号连续成功使用多少次后轮换。0 = 默认 50。
	StickyMaxRequests int

	HardCooldown time.Duration
	SoftCooldown time.Duration
	ErrThreshold int
	ErrCooldown  time.Duration
	RefreshSkew  time.Duration

	// HardCooldownFor 硬冷却（402 余额/权益不足）时长的计算钩子：nil 时恒用
	// HardCooldown。非 nil 时由装配层注入「截到每日发奖窗口」策略（见
	// config.HardCooldownFor）——放在钩子里而不是 server 直接依赖 config，
	// 是因为 server 包只依赖 provider/pool/ledger，不引入配置层。
	HardCooldownFor func(now time.Time) time.Duration

	// ModelFallbacks 模型降级链初始值（面板保存后经 SetModelFallbacks 热更新）。
	ModelFallbacks []config.ModelFallbackRule

	// ReasoningEfforts 思考强度注入映射初始值（面板保存后经 SetReasoningEfforts 热更新）。
	ReasoningEfforts map[string]string
}

// stickyEntry 粘性路由记录：记录上次路由账号及连续使用次数。
// 不使用 credits（pool 中余额仅在签到/手动刷新时更新，对话后是 stale 数据），
// 改用请求计数：连续请求 maxReqs 次后自动降级换账号。
type stickyEntry struct {
	uid      string
	reqCount int
	maxReqs  int
}

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux

	ledger *ledger.Ledger     // 记账器（cfg.Ledger 透传，nil = 不记账）
	stats  *statsEngine.Stats // 运行统计引擎（cfg.Stats 透传，nil = 不插桩）

	apiMu    sync.RWMutex // 保护 cfg.APIKey（面板可运行时修改）
	stickyMu sync.RWMutex
	sticky   map[string]*stickyEntry // runtimeKind → stickyEntry

	fbMu      sync.RWMutex               // 保护 fallbacks（面板可运行时修改）
	fallbacks []config.ModelFallbackRule // 模型降级链（cfg.ModelFallbacks 初始，热更新经 SetModelFallbacks）
	// aliases 是 fallbacks 的派生索引（别名 → 主力模型全路径），与 fallbacks 同锁同寿命：
	// 任何写入 fallbacks 的地方都必须经 setFallbacks 重建，避免两份状态漂移（R129）。
	aliases map[string]string

	efMu    sync.RWMutex      // 保护 efforts（面板可运行时修改）
	efforts map[string]string // 思考强度注入映射（cfg.ReasoningEfforts 初始，热更新经 SetReasoningEfforts）

	runtimesMu sync.RWMutex               // 保护 customRts 动态热更新（R66）
	customRts  map[provider.Kind]*Runtime // 自定义渠道运行时
}

// SetCustomRuntimes 热更新自定义渠道运行时（面板保存后由 app 层回调）。
func (h *Handler) SetCustomRuntimes(rts map[provider.Kind]*Runtime) {
	h.runtimesMu.Lock()
	h.customRts = rts
	h.runtimesMu.Unlock()
}

// getRuntime 取渠道运行时。内建渠道优先——自定义渠道名不得与内建同名，
// 该约束由 custom.ChannelConfig.Validate 强制（否则两层会指向不同 Runtime：
// 面板/账号操作打到自定义池，而对话请求走内建渠道）。
func (h *Handler) getRuntime(k provider.Kind) *Runtime {
	if rt := h.cfg.Runtimes[k]; rt != nil {
		return rt
	}
	h.runtimesMu.RLock()
	defer h.runtimesMu.RUnlock()
	return h.customRts[k]
}

// SetModelFallbacks 热更新模型降级链（面板保存后由 app 层回调；nil/空 = 清空禁用）。
func (h *Handler) SetModelFallbacks(rules []config.ModelFallbackRule) {
	h.setFallbacks(rules)
}

// setFallbacks 是 fallbacks 的唯一写入点：规则与别名索引在同一把锁内一起换，
// 派生只走 config.AliasIndex（单一来源，R129）。
func (h *Handler) setFallbacks(rules []config.ModelFallbackRule) {
	idx := config.AliasIndex(rules)
	h.fbMu.Lock()
	h.fallbacks = rules
	h.aliases = idx
	h.fbMu.Unlock()
}

// resolveAlias 把客户端请求名解析为别名指向的主力模型全路径。
// 命中时打一行 `alias resolved:` 日志（别名可观测性），未命中原样返回。
// 别名只在入口解析：解析结果就是后续一切行为（路由/容量/降级/记账）的输入。
func (h *Handler) resolveAlias(model string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		return model
	}
	h.fbMu.RLock()
	target, ok := h.aliases[m]
	h.fbMu.RUnlock()
	if !ok || target == "" {
		return model
	}
	log.Printf("alias resolved: %s -> %s", m, target)
	return target
}

// aliasTarget 返回别名指向的主力模型全路径（无别名返回空串），供 /v1/models 现拼条目用。
func (h *Handler) aliasTarget(name string) string {
	h.fbMu.RLock()
	defer h.fbMu.RUnlock()
	return h.aliases[strings.TrimSpace(name)]
}

// aliasEntries 返回按配置顺序排列的「别名 → 主力模型」对（给 /v1/models 现拼条目用）。
func (h *Handler) aliasEntries() [][2]string {
	h.fbMu.RLock()
	defer h.fbMu.RUnlock()
	out := make([][2]string, 0, len(h.aliases))
	for _, rule := range h.fallbacks {
		name := strings.TrimSpace(rule.Name)
		if target, ok := h.aliases[name]; ok && target != "" {
			out = append(out, [2]string{name, target})
		}
	}
	return out
}

// SetReasoningEfforts 热更新思考强度注入映射（面板保存后由 app 层回调；nil/空 = 清空禁用）。
func (h *Handler) SetReasoningEfforts(efforts map[string]string) {
	h.efMu.Lock()
	h.efforts = efforts
	h.efMu.Unlock()
}

func NewHandler(cfg Config) *Handler {
	if cfg.Runtimes == nil && cfg.Pool != nil && cfg.Upstream != nil {
		cfg.Runtimes = map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: cfg.Pool, Upstream: cfg.Upstream, StaticModels: WorkBuddyStaticModels()},
		}
	}
	if cfg.HardCooldown <= 0 {
		cfg.HardCooldown = 12 * time.Hour
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.MaxRequestBody > 0 {
		MaxRequestBody = cfg.MaxRequestBody
	}
	if cfg.StickyMaxRequests <= 0 {
		cfg.StickyMaxRequests = config.DefaultStickyMaxRequests
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), sticky: make(map[string]*stickyEntry), ledger: cfg.Ledger, stats: cfg.Stats, fallbacks: cfg.ModelFallbacks, efforts: cfg.ReasoningEfforts}
	h.aliases = config.AliasIndex(cfg.ModelFallbacks) // 别名索引与规则同源派生（R129）
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("POST /v1/images/generations", h.withAuth(h.imagesGenerations))
	h.mux.HandleFunc("POST /v1/images/edits", h.withAuth(h.imagesEdits))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	if cfg.WebUI != nil {
		// no-cache 强制浏览器每次 revalidate：embed 资源随二进制更新，若不加，
		// 浏览器长期用旧缓存会出现界面与后端版本不匹配（如新字段不渲染）。
		// 文件未变时 FileServer 回 304，开销可忽略。
		webSrv := http.FileServer(http.FS(cfg.WebUI))
		h.mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-cache")
			webSrv.ServeHTTP(w, r)
		}))
	}
	if cfg.AttachAPI != nil {
		cfg.AttachAPI(h.mux)
	}
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// stickyMaxReqs 粘性路由连续成功轮换阈值（默认 50）。
const stickyMaxReqs = 50

// stickyKey 粘性路由 key（按渠道独立）
func (h *Handler) stickyKey(kind provider.Kind) string { return kind.String() }

// pickWithSticky 粘性路由选择账号。
// 优先使用上次成功路由的账号，直到：
//   - 账号进入冷却/禁用状态，或对该请求模型处于模型级冷却（429/6004 按模型限额）
//   - 连续成功请求达到 maxReqs 次（默认 50），自动轮换
//
// 任一条件触发则降级为 PickForModel(model) 选新账号并重置粘性记录。
func (h *Handler) pickWithSticky(rt *Runtime, model string) *auth.Auth {
	h.stickyMu.RLock()
	sticky := h.sticky[h.stickyKey(rt.Kind)]
	h.stickyMu.RUnlock()

	// 尝试粘性路由
	if sticky != nil && sticky.uid != "" && sticky.reqCount < sticky.maxReqs {
		acct := rt.Pool.AuthByUID(sticky.uid)
		if acct != nil {
			status, ok := rt.Pool.Status(sticky.uid)
			modelCooled := ok && status.ModelCooling != nil && status.ModelCooling[model].After(time.Now())
			// ⚠️ 2026-10-18 补 SessionDead：本判据曾只查 Cooling/Disabled/模型级冷却，
			// 而 `healthy()`（pool.go:103）明确把 sessionDead 也算不健康。
			// 粘性路由直接调 AuthByUID、**绕过了 healthy()**，于是登录态已失效的账号
			// 仍被粘住 ⇒ 每个请求都白打一次必 401 的上游探测。
			// （MonkeyCode 实测：全日志 16 条 session_dead 401，且一次刷新都没发起。）
			if ok && !status.Cooling && !status.Disabled && !status.SessionDead && !modelCooled {
				// R146：粘性账号今天没有额度到期、而池内有账号今天有 ⇒ 立刻让位，
				// 并把粘性改钉到那个账号（会话粘性不丢）。低流量下 50 次阈值可能横跨数小时，
				// 快过期的额度等不起。
				if alt := h.yieldForExpiringToday(rt, model, sticky.uid, time.Now()); alt != nil {
					return alt
				}
				log.Printf("sticky route platform=%s uid=%s count=%d/%d",
					rt.Kind, sticky.uid, sticky.reqCount, sticky.maxReqs)
				return acct
			}
			if ok && status.SessionDead {
				log.Printf("sticky route skipped platform=%s uid=%s reason=session_dead",
					rt.Kind, sticky.uid)
			}
		}
	}

	// 降级：选择对当前模型可用的、余额最高的 healthy 账号
	acct := rt.Pool.PickForModel(model)
	if acct == nil {
		return nil
	}

	// 新建粘性记录
	h.stickyMu.Lock()
	h.sticky[h.stickyKey(rt.Kind)] = &stickyEntry{uid: acct.UID, maxReqs: h.cfg.StickyMaxRequests}
	h.stickyMu.Unlock()
	log.Printf("new sticky route platform=%s uid=%s maxReqs=%d", rt.Kind, acct.UID, h.cfg.StickyMaxRequests)
	return acct
}

// expiresToday unix 到期时刻是否落在「今天」桶（R105 单一口径：自然日 + 次日 00:00 的抖动缓冲）。
// 0 或负值 = 无到期信息，返回 false。
func expiresToday(unixSec int64, now time.Time) bool {
	if unixSec <= 0 {
		return false
	}
	return provider.ExpireBucketOf(time.Unix(unixSec, 0).Format(time.RFC3339), now) == provider.BucketToday
}

// yieldForExpiringToday 粘性让位（R146）：粘性账号今天没有额度到期、而池内最优账号今天有，
// 就把粘性改钉到最优账号并返回它。返回 nil 表示维持粘性。
//
// 必须与「会话粘性」共存：让位只发生一次，之后 sticky 记录指向新账号（reqCount 归零），
// 后续请求继续走同一账号，直到 50 次阈值或它变得不健康。
func (h *Handler) yieldForExpiringToday(rt *Runtime, model, stickyUID string, now time.Time) *auth.Auth {
	best := rt.Pool.PickForModel(model)
	if best == nil || best.UID == stickyUID {
		return nil
	}
	stExp, okSt := rt.Pool.EarliestExpireOf(stickyUID)
	bsExp, okBs := rt.Pool.EarliestExpireOf(best.UID)
	if !okSt || !okBs {
		return nil
	}
	// 两者都今天到期时保持粘性：否则两个账号会按到期时刻反复互抢，会话被拆碎。
	stToday := !stExp.IsZero() && expiresToday(stExp.Unix(), now)
	bsToday := !bsExp.IsZero() && expiresToday(bsExp.Unix(), now)
	if !bsToday || stToday {
		return nil
	}

	h.stickyMu.Lock()
	h.sticky[h.stickyKey(rt.Kind)] = &stickyEntry{uid: best.UID, maxReqs: stickyMaxReqs}
	h.stickyMu.Unlock()
	log.Printf("sticky yield platform=%s from=%s to=%s reason=expiring_today",
		rt.Kind, stickyUID, best.UID)
	return best
}

// stickySuccess 粘性路由成功：递增请求计数。
func (h *Handler) stickySuccess(rt *Runtime) {
	h.stickyMu.Lock()
	defer h.stickyMu.Unlock()
	key := h.stickyKey(rt.Kind)
	if e := h.sticky[key]; e != nil {
		e.reqCount++
	}
}

// stickyClear 粘性路由失败（错误/冷却）：清除粘性记录，下次请求强制重新选号。
func (h *Handler) stickyClear(rt *Runtime) {
	h.stickyMu.Lock()
	defer h.stickyMu.Unlock()
	delete(h.sticky, h.stickyKey(rt.Kind))
}

// StickyState 粘性路由快照片段（运行统计喂入用）。
type StickyState struct {
	UID   string
	Count int // 已连续成功请求数
	Max   int // 轮换阈值（默认 50）
}

// StickySnapshot 返回粘性路由快照（key = 渠道名，即 provider.Kind.String()）。
// 供运行统计引擎喂 ApplySticky——进程内直读取代 sidecar 对粘性日志行的解析；
// 无粘性记录的渠道不出现在结果里。
func (h *Handler) StickySnapshot() map[string]StickyState {
	h.stickyMu.RLock()
	defer h.stickyMu.RUnlock()
	out := make(map[string]StickyState, len(h.sticky))
	for k, e := range h.sticky {
		if e == nil || e.uid == "" {
			continue
		}
		out[k] = StickyState{UID: e.uid, Count: e.reqCount, Max: e.maxReqs}
	}
	return out
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if key := h.currentAPIKey(); key != "" {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") || strings.TrimPrefix(authz, "Bearer ") != key {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

// SetAPIKey 运行时修改内层 API Key（面板可调用）。
func (h *Handler) SetAPIKey(key string) { h.apiMu.Lock(); defer h.apiMu.Unlock(); h.cfg.APIKey = key }

// CurrentAPIKey 读取当前生效的 API Key（供外层兼容层跟随面板修改）。
func (h *Handler) CurrentAPIKey() string { return h.currentAPIKey() }
func (h *Handler) currentAPIKey() string {
	h.apiMu.RLock()
	defer h.apiMu.RUnlock()
	return h.cfg.APIKey
}
func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	accounts := map[string]any{}
	for _, k := range h.runtimeKinds() {
		rt := h.getRuntime(k)
		if rt != nil && rt.Pool != nil {
			accounts[k.String()] = rt.Pool.List()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

// 静态兜底表无上游能力数据，故不声明能力。
var workbuddyStaticModels = []provider.ModelInfo{
	{ID: "glm-5.2", ContextWindow: 131072}, {ID: "glm-5.1", ContextWindow: 131072}, {ID: "glm-5v-turbo", ContextWindow: 131072},
	{ID: "kimi-k2.7", ContextWindow: 131072}, {ID: "minimax-m3", ContextWindow: 131072}, {ID: "hy3", ContextWindow: 131072},
	{ID: "hy3-preview", ContextWindow: 131072}, {ID: "hy3-preview-agent", ContextWindow: 131072},
	{ID: "deepseek-v4-pro", ContextWindow: 131072}, {ID: "deepseek-v4-flash", ContextWindow: 131072},
}

// traework 静态兜底表同样无上游能力数据，不声明能力。
var traeworkStaticModels = []provider.ModelInfo{
	{ID: "glm-5.2"}, {ID: "glm-5-turbo"}, {ID: "glm-5"}, {ID: "DeepSeek-V4-Pro"}, {ID: "DeepSeek-V4-Flash"},
	{ID: "kimi-k2.6"}, {ID: "kimi-k2.7-code"}, {ID: "minimax-m3"}, {ID: "qwen3-coder"}, {ID: "Doubao-Seed-2.1-Pro"},
}

// traecode 静态兜底表：TraeCode（solo_agent）与 TraeWork（solo_work_lite）
// 下发的模型集**不同**，此表按 solo_agent 分组的实际下发清单整理（2026-09 实测）。
// 与 TraeWork 相比，TraeCode 独有 Doubao-Seed-Code / deepseek-v4.1-flash /
// glm-5.3-flash / kimi-k2.8-preview / qwen3.8-flash 等新版模型。
var traecodeStaticModels = []provider.ModelInfo{
	{ID: "Doubao-Seed-2.1-Pro"}, {ID: "Doubao-Seed-Evolving"}, {ID: "Doubao-Seed-2.1-Turbo"},
	{ID: "Doubao-Seed-Code"}, {ID: "DeepSeek-V4-Flash-Official"}, {ID: "DeepSeek-V4-Pro-Official"},
	{ID: "deepseek-v4.1-flash"}, {ID: "glm-5.3-flash"}, {ID: "glm-5.3"}, {ID: "glm-5.2"},
	{ID: "kimi-k3"}, {ID: "kimi-k2.8-preview"}, {ID: "minimax-m3"}, {ID: "qwen3.8-flash"},
	{ID: "qwen3.8-max"}, {ID: "qwen-3.7-plus"},
}

// dynamicModelsCache 保留给旧测试/旧单平台语义；实际多平台缓存放在 Runtime 内。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []provider.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
	// maxModelsFetchAccounts 限制一次目录拉取最多尝试几个账号。
	// 取值理由：目录是渠道级的，理论上要试到「有一个账号能给出目录」为止；
	// 但一个池若全是坏账号（20 个），逐个串行探测会让 /v1/models 首字节等上
	// 几十秒。3 个候选覆盖绝大多数真实场景（同人多开 / 少数号未开通），
	// 剩下的坏号由 5min 负缓存与下一轮刷新兜底。
	maxModelsFetchAccounts = 3
)

// errEmptyModelCatalog 表示上游正常应答但目录为空（如 WPS Comate 账号
// 未开通模型时 /models 返回 items=[]）。它不是传输/鉴权错误，
// 但对「渠道维度」的目录来说等价于这次尝试失败，需要换号重试。
var errEmptyModelCatalog = errors.New("upstream returned an empty model catalog")

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": h.modelList()})
}

// modelList 构造 /v1/models 的 data 数组。
//
// 别名条目（R129）**排在真实模型之前**，每次请求现拼、不进上游目录缓存：
// 别名表随降级链热更新即时生效，无需任何缓存失效通道。
// 能力字段**逐字段继承主力模型**（不新增任何推断），另带一个 alias 字段指向主力全路径。
// 沿用现有过滤：主力所在渠道未接入任何账号时该别名不出现。
func (h *Handler) modelList() []map[string]any {
	out := []map[string]any{}
	out = append(out, h.aliasModelEntries()...)
	for _, k := range h.runtimeKinds() {
		rt := h.getRuntime(k)
		if rt == nil || rt.Pool == nil || len(rt.Pool.List()) == 0 { // 只暴露已接入账号的平台
			continue
		}
		infos := h.fetchRuntimeModels(rt)
		if len(infos) == 0 {
			infos = rt.StaticModels
		}
		for _, mi := range infos {
			out = append(out, buildModelEntry(k, mi))
		}
	}
	return out
}

// aliasModelEntries 把别名表现拼成 /v1/models 条目（按降级链配置顺序）。
//
// 只做两件事：① 找到主力模型的 ModelInfo（找不到 = 主力所在渠道没接入账号 / 模型不在目录里
// ⇒ 整个条目不出现，沿用现有过滤口径）；② 拿 buildModelEntry 的**同一份输出**换掉 id
// 并加 alias 字段。为什么不另写一份能力拼装：那等于给能力字段开第二个来源，
// 迟早与真实模型条目漂移（R49「不编造」+ 单一来源）。
func (h *Handler) aliasModelEntries() []map[string]any {
	pairs := h.aliasEntries()
	if len(pairs) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(pairs))
	for _, p := range pairs {
		name, target := p[0], p[1]
		kind, upstream, ok := provider.SplitModelID(target)
		if !ok {
			continue
		}
		rt := h.getRuntime(kind)
		if rt == nil || rt.Pool == nil || len(rt.Pool.List()) == 0 {
			continue // 主力渠道没接入账号：别名不出现
		}
		infos := h.fetchRuntimeModels(rt)
		if len(infos) == 0 {
			infos = rt.StaticModels
		}
		var found *provider.ModelInfo
		for i := range infos {
			if infos[i].ID == upstream {
				found = &infos[i]
				break
			}
		}
		if found == nil {
			continue // 主力模型不在当前目录：不凭空造一条条目
		}
		entry := buildModelEntry(kind, *found)
		entry["id"] = name
		entry["alias"] = target
		out = append(out, entry)
	}
	return out
}

// buildModelEntry 构造单条 /v1/models 条目。
// OpenAI 官方仅规定 id/object/created/owned_by，未定义能力字段；
// 多模态能力按 OpenRouter / llama.cpp 通行的 architecture.input_modalities 透传。
// 上游模型列表接口未返回能力信息时，按惯例回退为 ["text"]。
// 思考档位（issue #96）：按 Buddy2api v2.1.11 口径透传 reasoning 对象，
// 上游未声明档位时整个对象不出现（不编造；supports_reasoning 同理不输出）。
func buildModelEntry(k provider.Kind, mi provider.ModelInfo) map[string]any {
	// 对外 ID 与 provider.PublicModelID 同源：上游 ID 自带命名空间的渠道（cline 的 cline-free/*）
	// 直接透传上游 ID，不再拼 `<渠道>/`（决议 R59；旧的 cline/cline-free/* 仍可路由）。
	id := provider.PublicModelID(k, mi.ID)
	entry := map[string]any{"id": id, "object": "model", "created": 1753600000, "owned_by": k.String()}
	if mi.ContextWindow > 0 {
		entry["context_length"] = mi.ContextWindow
	}
	if mi.MaxTokens > 0 {
		entry["max_output_tokens"] = mi.MaxTokens
	}
	if mi.SupportsTools {
		entry["supports_tools"] = true
	}
	entry["architecture"] = map[string]any{
		"input_modalities": mi.InputModalities(),
		"modality":         mi.Modality(),
	}
	var params []string
	if mi.SupportsTools {
		params = append(params, "tools", "tool_choice")
	}

	if len(mi.ReasoningEfforts) > 0 || mi.DefaultReasoningEffort != "" {
		// PR #189 原本还声明 include_reasoning，本仓**不实现**该参数（全仓零处理点：入站只吃
		// reasoning_effort 与 Responses 的 reasoning 对象）⇒ 对外宣告一个我们不接的参数，等于
		// 让客户端按它发而拿到静默忽略，正是 R49「绝不编造」要避免的情形，故剥掉。
		params = append(params, "reasoning", "reasoning_effort")
		reasoning := map[string]any{}
		if len(mi.ReasoningEfforts) > 0 {
			reasoning["supported_efforts"] = mi.ReasoningEfforts
		}
		if mi.DefaultReasoningEffort != "" {
			reasoning["default_effort"] = mi.DefaultReasoningEffort
		}
		if mi.CanDisableThinking != nil {
			reasoning["can_disable_thinking"] = *mi.CanDisableThinking
			reasoning["mandatory"] = !(*mi.CanDisableThinking)
		}
		entry["reasoning"] = reasoning
		entry["supports_reasoning"] = true
	}

	if len(params) > 0 {
		entry["supported_parameters"] = params
	}

	return entry
}

func (h *Handler) fetchRuntimeModels(rt *Runtime) []provider.ModelInfo {
	if rt.Kind == provider.WorkBuddy { // 兼容旧单平台缓存观察点
		dynamicModelsCache.RLock()
		if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
			out := dynamicModelsCache.ids
			dynamicModelsCache.RUnlock()
			return out
		}
		if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
			dynamicModelsCache.RUnlock()
			return nil
		}
		dynamicModelsCache.RUnlock()
	}
	rt.mu.RLock()
	if len(rt.models) > 0 && time.Since(rt.fetched) < dynamicModelsTTL {
		out := rt.models
		rt.mu.RUnlock()
		return out
	}
	if rt.Kind != provider.WorkBuddy && !rt.lastFail.IsZero() && time.Since(rt.lastFail) < modelsFetchFailCooldown {
		rt.mu.RUnlock()
		return nil
	}
	rt.mu.RUnlock()
	// 多账号回退（R133）：模型目录是**渠道维度**的，而 Pick 只取一个账号。
	// 只要池里有一个「拉不到目录」的账号——comate 未开通模型时 /models 返回
	// items=[]，token 失效时返回 401——单次 Pick 抽中它的那一刻，整条渠道的
	// 模型面就归零并进入 5min 负缓存。实测事故：加了第二个 WPS 账号后
	// comate 的 9 个模型瞬间变 0，面板里整条渠道消失，要等负缓存过期或重启。
	// 因此「拉不到」不等于「渠道没有模型」：换下一个账号重试，
	// 只有所有候选账号都失败，才认领渠道级失败（与旧语义一致，进负缓存）。
	tried := make(map[string]bool, 4)
	var (
		lastUID string
		lastErr error
		infos   []provider.ModelInfo
	)
	for attempt := 0; attempt < maxModelsFetchAccounts; attempt++ {
		acct := rt.Pool.PickExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		lastUID = acct.UID
		// 401 自愈：与费率路径（RefreshPricing）同款——拉取前先检查 token 有效性，
		// 过期先 refresh + 落盘。否则冷启动时 qodercn/qodercom 等无静态兑底渠道
		// 拿过期 token 拉 FetchModels → 401 → 静默失败 → 负缓存 5min，
		// 费率面板整组消失，最坏要等 30min ticker 才补拉（不变量 19）。
		if acct.NeedsRefresh(10 * time.Minute) {
			if err := rt.Upstream.RefreshToken(acct); err != nil {
				log.Printf("models fetch token refresh failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, err)
			} else if serr := acct.SaveAtomic(); serr != nil {
				log.Printf("models fetch token save failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, serr)
			}
		}
		got, err := rt.Upstream.FetchModels(acct)
		if err == nil && len(got) > 0 {
			infos = got
			break
		}
		lastErr = err
		if err == nil {
			lastErr = errEmptyModelCatalog
		}
		// 失败不再静默：无静态兑底渠道（qodercn/qodercom）整组消失时，
		// 至少日志里要有原因可查（曾致 issue #40 复发 40 分钟无法定位）。
		// 换号重试时同样逐条留痕，否则用户只看到渠道消失、查不到是谁拖垮的。
		log.Printf("models fetch failed platform=%s uid=%s err=%v n=%d", rt.Kind, acct.UID, lastErr, len(got))
	}
	if len(infos) == 0 {
		now := time.Now()
		rt.mu.Lock()
		rt.lastFail = now
		rt.mu.Unlock()
		if rt.Kind == provider.WorkBuddy {
			dynamicModelsCache.Lock()
			dynamicModelsCache.lastFail = now
			dynamicModelsCache.Unlock()
		}
		if lastUID != "" {
			log.Printf("models fetch failed platform=%s uid=%s err=%v n=0 (no account yielded a catalog)", rt.Kind, lastUID, lastErr)
		}
		return nil
	}
	now := time.Now()
	rt.mu.Lock()
	rt.models = infos
	rt.fetched = now
	rt.lastFail = time.Time{}
	rt.mu.Unlock()
	if rt.Kind == provider.WorkBuddy { // 兼容旧测试观察点
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = infos
		dynamicModelsCache.fetched = now
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	}
	return infos
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := ReadBodyLimited(r)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", err.Error())
		} else {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		}
		return
	}
	var peek struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	// 解析错误不再静默（issue #30）：截断/损坏的 body 若吞掉错误会误报 invalid_model
	if err := json.Unmarshal(body, &peek); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	// clientModel = 客户端请求的原始模型名（可能是别名），用于回填响应体 model 字段、
	// X-Model-Fallback 头与降级记录事件——那几处的语义是「客户端视角发了什么名字」。
	clientModel := peek.Model
	// 别名解析（R129）：别名是主力模型的另一个名字，命中即等价于请求主力模型。
	// 只在这里解析一次，reqModel 是后续一切（路由 / 容量判定 / 降级链 / 记账 / 档位）的输入。
	reqModel := h.resolveAlias(clientModel)
	rt, model, err := h.runtimeForModel(reqModel)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_model", err.Error())
		return
	}
	// 诊断：记录上下文构成摘要（config.body_log 关闭时立即返回，无实际开销）。
	// 必须放在 rewriteModel 之前——要记的是客户端发来的原始体，而非改写后的。
	if h.ledger != nil {
		h.ledger.TraceBody(rt.Kind.String(), clientModel, body)
	}
	// 统计/流水的归一 ID：新旧写法（cline-free/x 与 cline/cline-free/x）归一到同一串，
	// 否则同一模型会在用量榜/请求日志里裂成两行（R59）；其余渠道为恒等变换。
	// 别名已在上面解析成主力全路径，故记账记的是**主力模型的规范对外 ID**，
	// 用量榜永远不会出现别名（R129「账本完全透明」）。
	canonModel := provider.CanonicalModelID(reqModel)
	// 模型降级链：请求模型在本渠道已无账号可承载（no_healthy_account 的两种形态——
	// 「模型全冷」或「整渠道全灭」，pickWithSticky 必败）时，按用户配置的链找第一个
	// 仍有可用账号的备胎模型顶上；备胎可跨渠道，渠道全灭时不至于直接 503。
	// 仅此场景触发，正常路由零开销；不粘滞——每个请求都按原模型重新判定，上游解冻即自动切回。
	// fbApplied=实际备胎全路径（用于把响应 model 字段/X-Model-Fallback 头告知 agent）；
	// dispModel=写进响应的模型名（无降级=客户端请求名，保持官方「回填请求名」语义；降级=备胎全路径，
	// agent 读 chunk.model / resp.model 即可感知实际执行模型）。
	var fbApplied string
	// skipSameChannel=false：入口降级不限制渠道（见请求内重入点的理由）。
	fbID, primaryAvail := h.pickModelFallback(rt, model, false)
	// 口径说明：降级期间 ledger/stats 的 token 流水按**实际执行模型**记账
	// （statRow.Model、AddUsage(acctModel)、ledgerModelOf(acctModel)）——谁实际工作、
	// 谁实际消耗 token/积分，就记在谁名下（2026-10-16 口径变更，推翻原「按客户端
	// 请求名记账」设计）。acctModel=记账归属模型（无降级=canonModel；降级=备胎的
	// 规范对外 ID，归一函数同构，新旧写法仍归同一行）；原请求名→备胎的对照由
	// 日志行 + 面板「降级记录」区块 + X-Model-Fallback 头/响应 model 字段承担。
	acctModel := canonModel
	if fbID != "" {
		if rt2, m2, err := h.runtimeForModel(fbID); err == nil {
			log.Printf("model fallback platform=%s from=%s to=%s", rt.Kind, clientModel, fbID)
			// 响应头标注实际备胎（必须在任何 WriteHeader 之前；sw 透传 Header）
			w.Header().Set("X-Model-Fallback", clientModel+" -> "+fbID)
			rt, model = rt2, m2
			fbApplied = fbID
			acctModel = provider.CanonicalModelID(fbID)
		}
	}
	dispModel := clientModel
	if fbApplied != "" {
		dispModel = fbApplied
	}
	// 降级事件喂入运行统计（面板「降级记录」区块）：降级命中记一条；未降级且
	// 原模型容量在位时拿本请求当恢复探针（本请求后续成败不影响，
	// 恢复语义=「重新有可用账号」而非「重新请求成功」）。
	if h.stats != nil {
		switch {
		case fbApplied != "":
			h.stats.NoteFallback(clientModel, fbApplied, time.Now())
		case primaryAvail:
			h.stats.NoteFallbackRecovery(clientModel, time.Now())
		}
	}
	body, err = rewriteModel(body, model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// 思考强度注入：客户端未传 reasoning_effort 且面板配置命中（按客户端请求模型 ID 匹配，
	// 含 `*` 全局默认）时补上档位。未配置 = 零开销直通（applyEffort 内部快检）；
	// 注入失败仅记日志、原 body 照发，绝不因此中断请求。
	body = h.applyEffort(body, reqModel)

	// 运行统计插桩：每客户端请求一行（最终结果；中间换号尝试不逐行——对齐 sidecar 口径）。
	// TTFB 由 statWriter 记首字节，耗时/吞吐在出口由 defer 收口；
	// Status 未显式赋值 = 成功写回（200）。
	var statRow statsEngine.ReqRow
	statRow.Model = acctModel
	if peek.Stream {
		statRow.Mode = "流"
	} else {
		statRow.Mode = "非流"
	}
	sw := &statWriter{ResponseWriter: w, start: time.Now()}
	w = sw // 此后所有写出（流式/非流式/透传错误）统一经 sw 计时
	if h.stats != nil {
		defer func() {
			now := time.Now()
			statRow.Time = now.Format("01-02 15:04:05")
			statRow.TTFBMs = sw.ttfbMs
			statRow.TotalSec = now.Sub(sw.start).Seconds()
			// 单行 tok/s 用**端到端**口径（token ÷ 总耗时）：这一行要回答
			// 「这个请求我等了多久才等到这些 token」，因此排队与 prefill 必须
			// 计入分母。旧口径（减去 TTFB）会给 33 秒只吐 93 token 的请求标
			// 309 tok/s，与实际感知完全相反（R147）。
			if statRow.Tok > 0 && statRow.TotalSec > 0 {
				statRow.TokPerSec = float64(statRow.Tok) / statRow.TotalSec
			}
			if statRow.Status == 0 {
				statRow.Status = http.StatusOK
			}
			h.stats.AddRequestRow(statRow)
		}()
	}

	// 轮转选号 + 上游交互 + 耗尽出口整体平移至 serveRotation（方案 B：轮转耗尽且最后
	// 错误是积分耗尽时，按降级链当场切备胎、以备胎重入同一轮转段——抽方法使重入=
	// 再次调用；成功/错误出口与记账 defer 全留在本函数，「一个客户端请求一行」口径
	// 不变）。serveRotation 保持纯轮转语义；降级状态同步（模型改写/档位注入/记账
	// 归属/响应头）由本函数在重入前完成，与入口降级完全同构。
	for fbAttempt := 0; ; fbAttempt++ {
		res := h.serveRotation(w, sw, rt, model, body, clientModel, dispModel, acctModel, &statRow, peek.Stream)
		if res.done {
			return
		}
		// 候选耗尽待透传（res.lastStatus>0）：方案 B 判定——对两类「本渠道确实
		// 顶不住」的错误就地换模型：
		//   ① ErrHardCredit（积分耗尽）：账号刚进 12h 硬冷却，本渠道容量已翻 false；
		//   ② ErrSoftRate（模型饱和）：14003「Model busy」/6004「usage exceeds
		//      frequency limit」——上游明确按模型限额并建议「switch to the other
		//      models」，本渠道全部账号对该模型都在模型级冷却，轮转已无可选号。
		// 为什么 ② 必须纳入（2026-10-17 生产实录）：14003 被 Classify 判软限流后
		// 只冷「账号×模型」60s，而请求内降级原先只认 ErrHardCredit ⇒ 饱和时段的
		// **第一个**请求必然把 429 原样抛给客户端，第二个请求才由入口降级链顶上
		// （app.log 20:46:00 吃 429、20:46:01 降级成功，同一分钟后续 8 个请求全绿）。
		// 账号级 429 也会因此换模型：比让客户端直接失败更友好，代价是可能多烧
		// 备胎额度——已由用户裁决接受。
		// 仅重入一次（防备胎再降备胎死循环）；单账号渠道豁免（透传是惩罚豁免
		// 不变量的设计约束）；此刻未写过任何字节（轮转失败路径天然成立——所有
		// 写出都发生在成功流式/非流式分支内），降级头可安全补发；无规则/备胎无
		// 容量 → fb2 为空，维持原样透传，与现状一致。
		if fbAttempt == 0 && fallbackWorthy(res.lastKind) && !rt.SingleAccount {
			// 请求内重入只跨渠道（skipSameChannel=true）：同渠道兄弟模型走的是同一批
			// 账号，若本轮 429 其实是账号级限流（traework/qwenwork/qodercn/qodercom
			// 等渠道把任意 429 都归 ErrSoftRate），再打一遍同一批账号必然二次撞墙；
			// 且该账号第二个模型也撞墙后，pool 会把「模型级冷却」升级为「账号级冷却」
			// ——冷却时长取已有条目的最晚到期时刻（不延长），变的是粒度：可用容量从
			// 「1 个模型格子」缩到「0，整号对所有模型下线」。
			// 入口降级不设此限：那里「该模型已全冷」是既有事实，换同渠道兄弟模型
			// 正是降级链配置的本意（上游配额按模型计，模型 A 冷不代表模型 B 冷）。
			if fb2, _ := h.pickModelFallback(rt, model, true); fb2 != "" {
				if rt2, m2, err2 := h.runtimeForModel(fb2); err2 == nil {
					nb, err3 := rewriteModel(body, m2)
					if err3 == nil {
						log.Printf("model fallback platform=%s from=%s to=%s", rt.Kind, clientModel, fb2)
						// 响应头必须在任何 WriteHeader 之前（此刻未写过任何字节）；
						// 改写成功才发——失败走透传时不得残留降级头（S1）。
						w.Header().Set("X-Model-Fallback", clientModel+" -> "+fb2)
						body = nb
						body = h.applyEffort(body, reqModel) // 与入口同参数；键已存在不碰（双注入守卫）
						rt, model = rt2, m2
						dispModel = fb2
						acctModel = provider.CanonicalModelID(fb2) // 记账归属=备胎（0021/PR #148 口径）
						statRow.Model = acctModel
						statRow.Status = 0 // 429 轮转已污染；出口 defer 靠 Status==0 记 200
						if h.stats != nil {
							h.stats.NoteFallback(clientModel, fb2, time.Now()) // 请求内降级也喂降级记录
						}
						continue
					}
					log.Printf("model fallback body rewrite failed platform=%s to=%s err=%v（维持原样透传）", rt.Kind, fb2, err3)
				}
			}
		}
		if res.lastKind == provider.ErrQueued {
			// 全号排队：给合规的重试信号（与 body 里的 retry_after_seconds 一致）。
			w.Header().Set("Retry-After", "30")
		}
		transparentError(w, res.lastStatus, res.lastBody)
		return
	}
}

// rotationResult：serveRotation 的判定结果。done=true=响应已完整写回（成功/请求级
// 错误/耗尽诊断引导），调用方直接收尾；done=false 且 lastStatus>0=候选耗尽待透传，
// 由调用方决定原样透传还是按降级链请求内重入（fallbackWorthy 判定）。
type rotationResult struct {
	done       bool
	lastKind   provider.ErrKind
	lastStatus int
	lastBody   []byte
}

// fallbackWorthy 报告「轮转耗尽后是否值得就地换模型」。
//
// 只认两类「本渠道对该模型已无承载能力」的错误：
//   - ErrHardCredit：积分耗尽 → 账号进 12h 硬冷却，本渠道容量已翻 false；
//   - ErrSoftRate：模型饱和（14003 Model busy / 6004 usage exceeds frequency
//     limit）→ 全部账号对该模型都在模型级冷却，轮转已无号可选。
//
// 其余错误不换模型：内容类/请求类错误换模型必然复现（换模型只是白烧备胎额度），
// 5xx/登录态问题换模型也治不了本，原样透传让客户端与运维看到真实原因。
func fallbackWorthy(kind provider.ErrKind) bool {
	return kind == provider.ErrHardCredit || kind == provider.ErrSoftRate
}

// serveRotation：chatCompletions 的轮转选号 + 上游交互 + 耗尽出口段（原代码整体
// 平移，零缩进变化；结果经命名返回值承载——done 默认 true，既有裸 return 语义
// 原样保留：所有提前返回的出口都已把响应写完）。轮转中「账号级可轮换」错误
// （kind.Rotatable()）换号续试并置位 lastKind；请求级/内容类错误在循环内提前
// 透传返回。方案 B 重入时由调用方完成全部降级状态同步后再次调用本方法（tried
// 随调用重建，备胎渠道天然全新）。
func (h *Handler) serveRotation(
	w http.ResponseWriter, sw *statWriter, rt *Runtime, model string, body []byte,
	clientModel, dispModel, acctModel string, statRow *statsEngine.ReqRow, stream bool,
) (res rotationResult) {
	res.done = true // 默认：既有 return 出口都已把响应写完（成功/请求级错误/诊断引导）
	tried := map[string]bool{}
	var lastErr error
	// 最后一次上游错误的原始 status+body：候选耗尽时原样透传（而非包装成 503），
	// 客户端才能看到 6004 重置时间等上游细节。内容类错误在循环内已提前透传，不受影响。
	var lastStatus int
	var lastBody []byte
	// lastKind：轮转中最后一个「账号级可轮换」错误的类别（Rotatable 分支置位）——
	// 候选耗尽透传前交回调用方做方案 B 判定；成功/请求级错误不置位（零值 ErrNone）。
	var lastKind provider.ErrKind
	// attempts：本次请求最多试几个号 = 渠道账号数（原 MaxRotate 已删）。
	// 每个号最多试一次（tried），是否真的换下一个由错误类别决定——
	// kind.Rotatable() 为假、或传输层/5xx 这类「换号必撞同一面墙」的错误直接返回，
	// 这样「试满所有号」与「立即失败」两种语义都由错误类型决定，不再有魔法数字上限。
	attempts := len(rt.Pool.List())
	if attempts <= 0 {
		attempts = 1
	}
	for i := 0; i < attempts; i++ {
		acct := h.pickWithSticky(rt, model)
		if acct == nil {
			break
		}
		if tried[acct.UID] {
			// 粘性路由选回已尝试的账号，清除粘性记录后重试
			h.stickyClear(rt)
			acct = rt.Pool.PickExcludingForModel(tried, model)
			if acct == nil {
				break
			}
		}
		tried[acct.UID] = true
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			log.Printf("refresh start platform=%s uid=%s reason=request", rt.Kind, acct.UID)
			if err := rt.Upstream.RefreshToken(acct); err != nil {
				log.Printf("refresh failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, err)
				lastErr = err
				h.stickyClear(rt)
				// 本机网络/DNS 挂着时（`dial tcp: lookup ...: no such host`、connection refused），
				// 所有账号都刷不出来，换号必撞同一面墙 ⇒ 直接 502，别把 N×30s 超时堆满。
				if isNetworkError(err) {
					log.Printf("refresh failed by network platform=%s uid=%s（不换号）err=%v", rt.Kind, acct.UID, err)
					statRow.Status = http.StatusBadGateway
					writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
					return
				}
				// 账号侧问题（refresh token 失效 / 刷新接口 5xx）：只有「恰好要刷新」的号
				// 受影响，其余号 token 仍然有效 ⇒ 换下一个号，不设次数上限（试满所有号）。
				var ue *provider.Error
				if errors.As(err, &ue) && ue.Kind == provider.ErrSessionDead {
					rt.Pool.MarkSessionDead(acct.UID, "refresh: "+err.Error())
				} else {
					rt.Pool.Cooldown(acct.UID, pool.CoolErr, h.cfg.ErrCooldown, "refresh: "+err.Error())
				}
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				log.Printf("refresh save failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, err)
			}
			log.Printf("refresh success platform=%s uid=%s expires_at=%d", rt.Kind, acct.UID, acct.ExpiresAt)
		}
		rc, status, respBody, terr := rt.Upstream.ChatStream(acct, body)
		if terr != nil {
			lastErr = terr
			h.stickyClear(rt)
			// 传输层错误（连接超时 / 拒连 / 等响应头超时）：同一渠道所有账号打的是**同一个
			// host**，网络层故障与账号无关 ⇒ 换号必然撞同一面墙，而且单次可长达 120s。
			// 直接 502 把重试交给客户端（原逻辑：多号渠道轮 3 次 = 最长 6 分钟白等）。
			// 也不计入账号错误（2026-10-20）：一次本机网络抖动不该把整池账号打成冷却。
			// 单账号渠道（oczen）本来就是这条语义，现在统一。
			log.Printf("upstream transport error platform=%s uid=%s（不换号、不计账号错误）err=%v",
				rt.Kind, acct.UID, terr)
			statRow.Status = http.StatusBadGateway
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", terr.Error())
			return
		}
		if status >= 400 {
			statRow.Status = status // 所有透传出口（单账号/请求级/惩罚后）共用
			h.stickyClear(rt)
			kind := rt.Upstream.Classify(status, string(respBody))
			// 单账号渠道：任何账号级惩罚都等于整条渠道下线，故一律原文透传、不罚账号。
			// 尤其在 429（唯一需要的背压）上：冷却后后续请求会在挑号阶段被挡成
			// no_healthy_account，反而不如透传 429 让客户端按 Retry-After 自行重试。
			if rt.SingleAccount {
				// 402（权益不足）例外：账号本身健康，同号其他模型照常可用，只冷却「账号×该模型」
				// ——下一次请求挑号阶段即「模型全冷」，自动触发降级链顶替；本次仍原文透传。
				if kind == provider.ErrHardCredit {
					rt.Pool.CooldownModel(acct.UID, model, h.hardCooldown(), "402 权益不足")
					log.Printf("model cooldown (single-account 402) platform=%s uid=%s model=%s cooldown=%s",
						rt.Kind, acct.UID, model, h.hardCooldown())
				}
				// 日志点明错误性质与上游原文：旧文案（「不罚账号」）会让用户看到 403 时
				// 误以为「工具把这个错误当成了请求级问题」，掩盖了真实原因（issue #117）。
				log.Printf("upstream error platform=%s uid=%s status=%d kind=%s body=%s（单账号渠道无号可轮换，故原样透传、不冷却）",
					rt.Kind, acct.UID, status, kind, truncateBody(respBody))
				transparentError(w, status, respBody)
				return
			}
			switch kind {
			case provider.ErrHardCredit:
				rt.Pool.Cooldown(acct.UID, pool.CoolHard, h.hardCooldown(), "余额/权益不足")
			case provider.ErrSoftRate:
				// 429/6004 上游按模型限额（「您也可以切换其他模型继续使用」）：
				// 只冷却该账号的该模型，同号其他模型继续参与选号；
				// 短时间内第二个模型也撞墙时 CooldownModel 会升级整号冷却（防上游按账号计时）。
				// 冷却时长按 6004 响应体的真实重置点解析（#82）：固定 60s 与上游「按重置点解封」
				// 不符，解析失败/非正回退 SoftCooldown；升级时整号冷却到最晚重置点。
				escalated := rt.Pool.CooldownModel(acct.UID, model, h.softRateCooldown(string(respBody)), "429 rate limit")
				if escalated {
					log.Printf("model cooldown escalated to account platform=%s uid=%s model=%s", rt.Kind, acct.UID, model)
				} else {
					log.Printf("model cooldown platform=%s uid=%s model=%s", rt.Kind, acct.UID, model)
				}
			case provider.ErrSessionDead:
				// 登录态失效不再混入「停用」：标记 SessionDead（挑号跳过 + 面板徽标 +
				// 重新登录入口），与用户手动停用（Disable）分离（#134）。
				rt.Pool.MarkSessionDead(acct.UID, truncateBody(respBody))
				log.Printf("session dead marked platform=%s uid=%s", rt.Kind, acct.UID)
			case provider.ErrNotFound:
				rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
			case provider.ErrContentBlocked, provider.ErrPromptTooLong, provider.ErrImageInvalid,
				provider.ErrBadParams, provider.ErrPassthrough:
				// 内容拦截 / 上下文超限 / 图片无效 / 出站 body 不合格 / 请求级拒绝：
				// 不冷却不熔断不计错，直接透传原文回客户端。
				// 这些是请求内容或请求形态问题，与账号健康无关，轮转白费时间且浪费好号配额。
				//
				// ⚠️ ErrImageInvalid 与 ErrBadParams 是 2026-10-17 补入的：两者的 ErrKind 注释
				// 早就写着「请求级错误，不罚号不轮转，透传原文」，但本 case 漏了它们 ⇒ 落到
				// default 做 NoteError 计数，等于拿客户端的问题惩罚健康账号。本地拒绝型渠道
				// （monkeycode / loomy / raccoon / minimax）最容易触发这条路径。
				transparentError(w, status, respBody)
				return
			case provider.ErrModelBlocked:
				// (账号,模型) 级负缓存避让 —— 兑现 provider.go:193 的注释承诺。
				//
				// ⚠️ 2026-10-18 修正：本 case 原先与 ErrWafBlock/ErrAccountFault 合并，
				// 走 Pool.Cooldown(uid) **冻结整个账号**。后果是「一个模型在上游不存在」
				// 会拖垮同一账号下的所有模型：officeace 里 glm-5.3-flash 上游回 404，
				// 三个账号接连被 model_blocked 冻结 60s，整条渠道 503。
				// 而 provider.go:193 对 ErrModelBlocked 的定义明明写着「(账号,模型) 负缓存避让」。
				// 现在改为只冻这一个 (账号,模型)；CooldownModel 内部在「≥2 个模型同时冷」
				// 时才升级为整号冷却（见 pool.go 的同款注释），那时才是真正的账号问题。
				escalated := rt.Pool.CooldownModel(acct.UID, model, h.cfg.SoftCooldown, kind.String())
				log.Printf("model cooldown platform=%s uid=%s model=%s escalated=%v reason=%s",
					rt.Kind, acct.UID, model, escalated, kind)
				transparentError(w, status, respBody)
				return
			case provider.ErrWafBlock, provider.ErrAccountFault:
				// 账号级风控/授权故障：软冷却，不累计错误计数。
				rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, kind.String())
				if kind != provider.ErrAccountFault {
					transparentError(w, status, respBody)
					return
				}
			case provider.ErrClient:
				// 请求内容/形态类 4xx（如 workbuddy 11148 工具调用与结果不配对）：
				// 错误跟着请求体走，换号必然同样拒收——不累计错误计数。否则一个
				// 坏会话的客户端重试风暴会在几分钟内把全池账号记满冷却（阈值默认 3），
				// 面板呈现「渠道自己挂了」假象，且诱发无意义的模型降级链触发。
				// 仅轮转以兼容「同模型不同账号权益差异」场景；候选耗尽后透传最后
				// 错误（与上游分类器注释「BadParams：不罚号但仍轮转」对齐）。
			case provider.ErrQueued:
				// 上游把「排队」当 4xx 下发（同 10605 的另一种形态）：只冷却「号×模型」，不罚整号。
				// 冷却时长取响应里的 retryAfter/重置点，换号可能命中不同队列。
				rt.Pool.CooldownModel(acct.UID, model, h.softRateCooldown(string(respBody)), "model queued")
				log.Printf("model queued (http %d) platform=%s uid=%s model=%s", status, rt.Kind, acct.UID, model)
			case provider.ErrServer:
				// 上游 5xx = 服务端故障，与账号健康无关 ⇒ **不计入账号错误**。
				//
				// ⚠️ 2026-10-18 翻转（原先只有声明 NoCooldownOnServerError 的渠道豁免）：
				// 实测事故（lobsterai）—— 上游对带 developer 角色的请求返回 500，
				// ErrServer 可轮换 ⇒ 一次客户端请求打 3 个账号各 +1 errCount，
				// 而 err_threshold 默认 3 ⇒ **重试一次就把三个健康账号全冷却 10 分钟**，
				// 期间整条渠道 503。账号本身完全健康（积分/签到/session 全正常）。
				//
				// ⚠️ 2026-10-20 再进一步：**连换号也不做了**（ErrServer 从 Rotatable 除名）。
				// 实测 workbuddyai 11134「the model provider is temporarily unavailable」
				// 在同一波里 4 个号全 500，且每个号内部客户端已自行重试 3 次（日志
				// 「http 500，800ms 后重试 (2/3)」）⇒ 一次请求实际打 9 次，全部白打。
				// 结论：5xx 直接透传原状态码/原文，把等待与重试交给客户端。
				//
				// 逃生门：NoCooldownOnServerError 字段保留，但语义不变（仍表示「不豁免」），
				// 仅用于审计/文档层面标注渠道差异；真正的计数开关由 CountServerErrorAsAccountFault
				// 单独表达，当前无一渠道开启。
				if rt.CountServerErrorAsAccountFault {
					rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
					log.Printf("upstream server error platform=%s uid=%s status=%d（渠道显式要求计入账号错误）",
						rt.Kind, acct.UID, status)
				} else {
					log.Printf("upstream server error platform=%s uid=%s status=%d（不计入账号错误、不换号，直接透传）",
						rt.Kind, acct.UID, status)
				}
			default:
				rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			}
			// 账号级错误（限流/欠费/登录态死/账号故障/排队等）：换下一个账号重试本请求，
			// 全部候选耗尽后才把最后一个上游错误透传给客户端（-p4 前此处直接 return，
			// 导致客户端看到本可避免的 429/402）。内容类错误在上方 case 中已提前返回；
			// 5xx 与传输层错误也是直接透传（换号必撞同一面墙，见各自分支注释）。
			if kind.Rotatable() {
				log.Printf("rotate platform=%s uid=%s kind=%s status=%d body=%s",
					rt.Kind, acct.UID, kind, status, truncateBody(respBody))
				lastErr = fmt.Errorf("upstream %s (http %d): %s", kind, status, truncateBody(respBody))
				lastStatus = status
				lastBody = respBody
				lastKind = kind
				continue
			}
			transparentError(w, status, respBody)
			return
		}
		defer rc.Close()
		// 轮转内换号成功：清掉前面 status>=400 分支写进统计行的状态码。
		// 否则「A 号 429、B 号成功」的请求会在运行统计/请求日志里记成 429——
		// 出口 defer 只在 Status==0 时补 200，而该字段一旦被写就不会自愈。
		// 模型饱和（14003/6004）是多账号渠道的高频形态，统计失真会让面板看不出
		// 降级是否生效，故与请求内降级路径的 statRow.Status 清零同源处理。
		statRow.Status = 0
		rt.Pool.NoteSuccess(acct.UID)
		h.stickySuccess(rt)
		if stream {
			usage, serr := rt.Upstream.Stream(w, rc, dispModel)
			// 排队提示（qodercn 10605 isQueued）：上游把「模型拥塞」当成 HTTP 200 里的
			// 一帧业务错误下发，此刻 **一字节未写** ⇒ 冷却「号×该模型」后换下一个号。
			//
			// 若不识别：这帧没有 choices，会被当普通 chunk 透传 ⇒ 客户端只有一个空 delta，
			// 界面停在「正在等待模型…」，直到网关 180s 后掐断（2026-10-20 实录：
			// 180.3/180.7/180.8s 三次，每轮扣掉 3 分钟，且零 token 计费）。
			var qe *provider.Queued
			if errors.As(serr, &qe) && !sw.written {
				h.stickyClear(rt)
				rt.Pool.CooldownModel(acct.UID, model, qe.Cooldown(), "model queued")
				clearSSEHeaders(w) // StreamCapture 已写进 header 的 SSE 头必须撤掉
				log.Printf("model queued platform=%s uid=%s model=%s %s（换下一个账号，冷却 %s）",
					rt.Kind, acct.UID, model, qe.Error(), qe.Cooldown())
				lastErr = qe
				lastStatus = http.StatusTooManyRequests
				lastBody = queuedBody(qe)
				lastKind = provider.ErrQueued
				continue
			}
			// 其余「首帧即错误、一字节未写」的分类错误（典型：qodercom 的 envelope 403 里
			// 包着 code 110/112 额度耗尽帧 → ErrHardCredit）：此刻换号还来得及，不应当作
			// 「已写流的残留错误」只罚不换（那样 5 个号全耗尽时客户端只能干等下次请求）。
			// 与写后分支的区别仅在「能不能换」：已写出字节的流无法撤回，只能罚号 + 透传。
			if serr != nil && !sw.written && len(rt.Pool.List()) > 1 {
				var sec provider.StreamErrorClassifier
				if errors.As(serr, &sec) && sec.Kind().Rotatable() {
					kind := sec.Kind()
					h.stickyClear(rt)
					switch kind {
					case provider.ErrHardCredit:
						rt.Pool.Cooldown(acct.UID, pool.CoolHard, h.hardCooldown(), "stream 402 权益不足")
					case provider.ErrSoftRate:
						rt.Pool.CooldownModel(acct.UID, model, h.softRateCooldown(serr.Error()), "stream 429 限流")
					case provider.ErrSessionDead:
						rt.Pool.MarkSessionDead(acct.UID, serr.Error())
					case provider.ErrAccountFault:
						rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "stream account fault")
					default:
						rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
					}
					clearSSEHeaders(w)
					log.Printf("stream error before write platform=%s uid=%s model=%s kind=%s（换下一个账号）err=%v",
						rt.Kind, acct.UID, model, kind, serr)
					lastErr = serr
					lastKind = kind
					// 额度耗尽透 402（付费墙语义，客户端/用户一看就知道要充值/换号），
					// 其余保留上游原始状态码；body 用原文，保留上游错误码与文案。
					lastStatus = http.StatusPaymentRequired
					if pe := (*provider.Error)(nil); errors.As(serr, &pe) && pe.Status >= 400 {
						lastStatus = pe.Status
					}
					lastBody = []byte(serr.Error())
					continue
				}
			}
			cu := cacheUsageOf(usage) // 缓存命中（上游未上报时 reported=false，面板显示「—」）
			// 记 token 流水（成功请求口径；usage 缺失记 0 token 仅计请求数）。
			// 记账失败不干扰主流程——锦上添花功能。
			if h.ledger != nil {
				pt, ct, src := usageTokens(usage)
				h.ledger.AppendUsage(ledger.UsageEntry{Ch: rt.Kind.String(), UID: acct.UID,
					Model: ledgerModelOf(acctModel), PT: pt, CT: ct, CR: cu.cached, CRC: cu.reported, Src: src,
					Credit: usageCredit(usage)})
			}
			// 运行统计：模型维度记账（有 usage 才计，对齐 sidecar 的 u.found 语义）。
			if h.stats != nil {
				pt, ct, _ := usageTokens(usage)
				if pt > 0 || ct > 0 {
					h.stats.AddUsage(acctModel, pt, ct, usageCredit(usage), sw.ttfbMs, cu.cached, cu.reported, time.Now())
					// 滑动窗口吞吐：把 completion token 按「每秒写帧数占比」摊到秒桶，
					// 面板显示窗口内平均 tok/s。为什么不在这里算比值：分母用
					// 「总耗时 − TTFB」会把排队/prefill 整段抹掉（实测小浣熊夸大
					// 3.56×），而单请求比值本身也无法表达「某一秒吐出多少」——
					// 上游吞吐每秒都在变（见 docs/adr/accepted/r132-*.md）。
					h.stats.AddTput(acctModel, ct, sw.chunks, time.Now())
					statRow.Tok = ct
				}
				statRow.Credit = usageCredit(usage)
			}
			if serr != nil {
				// 思考链死循环（发癫）命中：模型/请求级问题，不罚账号（换号必然复现），
				// 错误帧已透传给客户端，由下游 agent 自行重试。仅记日志。
				var lge *loopguard.Error
				if errors.As(serr, &lge) {
					log.Printf("reasoning loop intercepted platform=%s uid=%s model=%s kind=%s reason=%s",
						rt.Kind, acct.UID, clientModel, lge.Hit.Kind, lge.Hit.Reason)
					return
				}
				// 上游中断/空流已尽力透传，错误仅在日志可见。
				// 必须带耗时与 TTFB（R58）：只有这个数字能把几种原因分开——
				// 耗时 ≈ 空闲阈值（idle.DefaultTimeout 300s / 各渠道 IdleReader 90s）
				// 且 TTFB 远小于它 ⇒ 看门狗掐的；耗时远小于阈值 ⇒ 上游自己断的。
				// 原先只记 err，两种完全同形，定位只能靠猜。
				log.Printf("stream relay end platform=%s uid=%s model=%s elapsed=%.1fs ttfb=%dms err=%v",
					rt.Kind, acct.UID, clientModel, time.Since(sw.start).Seconds(), sw.ttfbMs, serr)
				// 流内业务错误（traework SOLO 的 event:error，如 3004 限流）：
				// HTTP 200 与响应头早已发出，无法在同一请求内换号重试；唯一补救是把
				// 中招账号冷却掉——下次请求挑号会自动换到别的账号（粘性路由见
				// pickWithSticky 对 status.Cooling 的检查，故无需显式 stickyClear）。
				var sec provider.StreamErrorClassifier
				if errors.As(serr, &sec) {
					kind := sec.Kind()
					switch kind {
					case provider.ErrSoftRate:
						// 单号渠道（当前仅一个账号，如 loomy/小浣熊）：冷却 = 整条渠道下线，
						// 反不如透传——客户端已通过 SSE 收到上游原始限流错误（3004/429），
						// 会自行退避重试；与非流式「单账号不罚账号」口径一致。
						// SingleAccount 渠道（oczen）官方已有完整豁免语义，保持官方原样不重复覆盖。
						if len(rt.Pool.List()) <= 1 && !rt.SingleAccount {
							log.Printf("stream error passthrough platform=%s uid=%s kind=%s（单号渠道不罚账号，客户端自行重试）",
								rt.Kind, acct.UID, kind)
							break
						}
						rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "stream business error: "+serr.Error())
						log.Printf("stream error cooled platform=%s uid=%s kind=%s cooldown=%s",
							rt.Kind, acct.UID, kind, h.cfg.SoftCooldown)
					case provider.ErrHardCredit:
						if len(rt.Pool.List()) <= 1 {
							// 单号渠道：余额/权益不足透传原文，客户端/用户直接看到真实原因（如充值），
							// 而不是被 503 掩盖成「渠道不可用」。账号本身不动，仅冷却「账号×该模型」，
							// 下一次请求挑号即「模型全冷」，自动走降级链顶替。
							rt.Pool.CooldownModel(acct.UID, model, h.hardCooldown(), "stream 402 权益不足")
							log.Printf("stream error model-cooldown platform=%s uid=%s model=%s kind=%s（单号渠道仅冷却该模型，原文透传）",
								rt.Kind, acct.UID, model, kind)
							break
						}
						rt.Pool.Cooldown(acct.UID, pool.CoolHard, h.hardCooldown(), "stream business error: "+serr.Error())
						log.Printf("stream error cooled platform=%s uid=%s kind=%s cooldown=%s",
							rt.Kind, acct.UID, kind, h.hardCooldown())
					case provider.ErrSessionDead:
						// 登录态失效：标记 SessionDead（挑号跳过 + 面板徽标），不再混入停用（#134）
						rt.Pool.MarkSessionDead(acct.UID, serr.Error())
					default:
						// 其余流内错误（如请求级拒绝）不罚账号，只记日志。
						log.Printf("stream error ignored platform=%s uid=%s kind=%s",
							rt.Kind, acct.UID, kind)
					}
				}
			}
			return
		}
		resp, err := rt.Upstream.Aggregate(rc, dispModel)
		if err != nil {
			// 流内嵌错误（如 qwenwork 402 权益不足）此时已带分类：模型级冷却，
			// 下一次请求挑号即「模型全冷」，自动走降级链顶替；本次仍返回错误。
			var asec provider.StreamErrorClassifier
			if errors.As(err, &asec) && asec.Kind() == provider.ErrHardCredit {
				rt.Pool.CooldownModel(acct.UID, model, h.hardCooldown(), "402 权益不足")
				log.Printf("model cooldown (aggregate 402) platform=%s uid=%s model=%s cooldown=%s",
					rt.Kind, acct.UID, model, h.hardCooldown())
			}
			statRow.Status = http.StatusBadGateway
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			return
		}
		if h.ledger != nil {
			u, _ := resp["usage"].(map[string]any)
			pt, ct, src := usageTokens(u)
			cu := cacheUsageOf(u)
			h.ledger.AppendUsage(ledger.UsageEntry{Ch: rt.Kind.String(), UID: acct.UID,
				Model: ledgerModelOf(acctModel), PT: pt, CT: ct, CR: cu.cached, CRC: cu.reported, Src: src,
				Credit: usageCredit(u)})
		}
		writeJSON(w, http.StatusOK, resp)
		// 运行统计：必须在写回之后——此刻 statWriter 才记到首字节，TTFB 为真值；
		// 一次性写回 gen≈0 由引擎 50ms 守卫兜底（只计 TTFB 不计吞吐——非流式本无吞吐样本，
		// 若在写回前取值会把 ttfb=0/gen=端到端总时长喂进引擎，污染 AvgTTFB 与吞吐均值）。
		if h.stats != nil {
			u, _ := resp["usage"].(map[string]any)
			pt, ct, _ := usageTokens(u)
			if pt > 0 || ct > 0 {
				cu := cacheUsageOf(u)
				h.stats.AddUsage(acctModel, pt, ct, usageCredit(u), sw.ttfbMs, cu.cached, cu.reported, time.Now())
				// 非流式一次性写回：无分帧时序可依，故不产生窗口速率样本
				// （前端显示「—」比显示一个端到端假速率更诚实）。
				statRow.Tok = ct
			}
			statRow.Credit = usageCredit(u)
		}
		return
	}
	// 渠道没有可用账号：区分「未绑定账号」与「全部冷却/禁用」，给出可操作的引导。
	// 客户端（如 Claude Code/Codex）只看到这条错误，必须足以让用户知道去面板做什么。
	// 候选耗尽但轮换中有上游错误：优先透传最后一个上游原始响应（保持上游语义，
	// 如 429+6004 重置时间；本次请求确实逐号尝试过，包装成 503 反而丢信息）。
	if lastStatus > 0 && lastBody != nil {
		// 候选耗尽：不在此透传——交回调用方判定（方案 B 积分耗尽时按降级链重入一次，
		// 否则原样透传；透传语义见上方注释）。
		res = rotationResult{done: false, lastKind: lastKind, lastStatus: lastStatus, lastBody: lastBody}
		return
	}
	statRow.Status = http.StatusServiceUnavailable
	sts := rt.Pool.List()
	now := time.Now()
	var bound, disabled, cooling, sessionDead, modelAvail int
	var earliest time.Time // 全池对该模型最早的解冻时刻（账号级与模型级冷却取最小）
	consider := func(t time.Time) {
		if t.After(now) && (earliest.IsZero() || t.Before(earliest)) {
			earliest = t
		}
	}
	for _, s := range sts {
		if s.Disabled {
			disabled++
			continue
		}
		// ⚠️ 2026-10-18 补 sessionDead：登录态失效的账号原先**被计入 bound（可用账号）**，
		// 于是 503 文案说「其余余额耗尽或出错」，把「需要重新登录」这个唯一可行动的
		// 结论彻底掩盖掉（MonkeyCode 实测：用户看到的是"余额耗尽"，实际要重登）。
		// pool.healthy() 早已把 sessionDead 视为不健康（pool.go:103），此处统计口径对齐它。
		if s.SessionDead {
			sessionDead++
			continue
		}
		if s.Cooling {
			cooling++
			consider(s.Until)
			continue
		}
		bound++
		if t, ok := s.ModelCooling[model]; ok {
			consider(t)
		} else {
			modelAvail++
		}
	}
	if len(sts) == 0 {
		if noLoginChannel(rt.Kind) {
			writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
				fmt.Sprintf("渠道 %s 尚未就绪（虚拟账号未装配）：请重启程序", rt.Kind))
			return
		}
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			fmt.Sprintf("渠道 %s 尚未绑定账号：请在面板「账号管理」中添加 %s 账号（或把模型改为已接入渠道，如 workbuddy/glm-5.2）",
				rt.Kind, rt.Kind))
		return
	}
	// 账号均健康、但请求模型在全部账号上都处于模型级限流冷却：
	// 明确告知解冻时间，避免误以为账号故障去重登（2026-09-23 全池 6004 事故的教训）。
	//
	// 措辞「冷却」而不写「限流」：同一窗口也可能来自上游排队（ErrQueued 给「号×模型」
	// 打 30s 冷却，2026-10-20），写死「限流」会让用户以为被限速了。
	// 同时补 Retry-After：首次请求遇到全号排队时回的是 429 + Retry-After: 30，
	// 冷却窗口内的后续请求应当给同量级的重试信号，而不是让 agent 把 503 当“服务坏了”硬退避。
	if bound > 0 && modelAvail == 0 {
		if !earliest.IsZero() {
			w.Header().Set("Retry-After", formatRetryAfter(earliest))
		}
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			fmt.Sprintf("渠道 %s 的 %d 个账号均健康，但模型 %s 已在全部账号上冷却（上游限流或排队）：最早约 %s 解冻（到点自动恢复，无需重新登录），也可先切换其他模型",
				rt.Kind, bound, model, formatUnfreeze(earliest)))
		return
	}
	if bound == 0 {
		// 匿名渠道没有「重新登录」这个概念，给出针对性提示（否则会误导用户去找登录入口）。
		hint := fmt.Sprintf("%s 账号需重新登录（日志会有 refresh token is invalid）", rt.Kind)
		if noLoginChannel(rt.Kind) {
			hint = "该渠道无需登录，稍后重试即可（若持续失败请查看日志）"
		}
		// 粘贴 Key 型渠道（cline）没有「重新登录」这个动作：Key 不过期也不轮换，
		// 被禁用只会是「Key 无效/已被删除」，唯一补救是换一把有数 Key 粘进来。
		// 照搬「需重新登录」会把用户引到一个不存在的入口（R41 同族的文案误导）。
		if keyPasteChannel(rt.Kind) {
			hint = fmt.Sprintf("请在面板粘贴一把有效的 %s API Key（sk_…，在 app.cline.bot → Account / API Keys 创建）", rt.Kind)
		}
		// 多数是限流冷却而非登录态问题：给出最早解冻时间，重登提示仅在无任何冷却时兜底。
		if cooling > 0 && !earliest.IsZero() {
			hint = fmt.Sprintf("最早约 %s 解冻（多为限流冷却，到点自动恢复；若长时间未恢复再检查登录态）", formatUnfreeze(earliest))
		}
		// 登录态失效优先于限流文案：这是唯一「用户自己动手就能恢复」的原因，
		// 被限流文案盖住会让人白等（2026-10-18 MonkeyCode 实测的误导）。
		if sessionDead > 0 && !noLoginChannel(rt.Kind) && !keyPasteChannel(rt.Kind) {
			hint = fmt.Sprintf("%d 个账号登录态已失效，需重新登录后才能恢复", sessionDead)
		}
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			fmt.Sprintf("渠道 %s 的 %d 个账号当前全部不可用（禁用 %d / 冷却 %d / 登录态失效 %d）：可在面板查看原因；%s",
				rt.Kind, len(sts), disabled, cooling, sessionDead, hint))
		return
	}
	msg := fmt.Sprintf("渠道 %s 暂无可用账号（共 %d 个：禁用 %d / 冷却 %d / 登录态失效 %d；其余余额耗尽或出错）",
		rt.Kind, len(sts), disabled, cooling, sessionDead)
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	return // 裸 return：携带命名结果 res（done=true——诊断引导已把响应写完）
}

// formatRetryAfter 把解冻时刻换算成整数秒（至少 1）。
// 用于 Retry-After 头：与首次全号排队时的 Retry-After: 30 同一量级信号。
func formatRetryAfter(until time.Time) string {
	d := time.Until(until)
	if d < time.Second {
		d = time.Second
	}
	return strconv.Itoa(int(d.Seconds() + 0.5))
}

// formatUnfreeze 解冻时刻的可读格式：24h 内只报时刻（HH:MM），更远带日期。
// 零值返回占位文案（调用方一般已排除零值，防御性兜底）。
func formatUnfreeze(t time.Time) string {
	if t.IsZero() {
		return "稍后（请查看面板冷却原因）"
	}
	if time.Until(t) < 24*time.Hour {
		return t.Format("15:04")
	}
	return t.Format("2006-01-02 15:04")
}

// noLoginChannel 报告渠道是否「无登录概念」（不存在凭证文件、也无重登入口）。
// 用于把「去面板重新登录」这类引导文案限定在真正适用的渠道上，
// 避免免密渠道（oczen / kilo）被误导去找一个不存在的登录按钮。
func noLoginChannel(k provider.Kind) bool { return k == provider.Oczen || k == provider.Kilo }

// keyPasteChannel 报告渠道凭据是否「由用户从官方站点自行创建后粘贴进来」（而非登录编排取得）。
// 这类渠道的引导文案必须指向「粘贴新 Key」而不能说「重新登录」：
// cline 的官方 API Key 不过期、不轮换，被禁用只会是 Key 本身无效/被删。
func keyPasteChannel(k provider.Kind) bool { return k == provider.Cline }

// aliasHint 拼给「未知模型名」错误的可行动提示（未配置别名时为空串，不产生噪声）。
// 别名是裸名（不含 `/`），所以只会在「没写渠道前缀」这一分支上被需要——
// 让用户知道「打错别名」与「忘了写前缀」可以去 /v1/models 自查（R129）。
func (h *Handler) aliasHint() string {
	h.fbMu.RLock()
	n := len(h.aliases)
	h.fbMu.RUnlock()
	if n == 0 {
		return ""
	}
	return "（可用别名见 /v1/models 中带 alias 标记的条目）"
}

func (h *Handler) runtimeForModel(model string) (*Runtime, string, error) {
	// 解析与 provider.PublicModelID 同源：既接受 `<渠道>/<上游ID>`，也接受上游 ID 自带命名空间的
	// 直通形态（`cline-free/*`）——后者首段不是渠道名，必须整串当上游模型名（决议 R59）。
	kind, upstream, ok := provider.SplitModelID(model)
	if !ok {
		return nil, "", fmt.Errorf("model must use explicit prefix: workbuddy/<model> / workbuddyai/<model> / traework/<model> / qoder/<model> / qodercn/<model> / qodercom/<model> / qwenwork/<model> / glm/<model> / oczen/<model> / kilo/<model> / minimax/<model> / lobsterai/<model> / zcode/<model> / gemini/<model> / codearts/<model>；Cline 免费池直接用上游 ID（cline-free/<model>）%s", h.aliasHint())
	}
	rt := h.getRuntime(kind)
	if rt == nil || rt.Pool == nil || rt.Upstream == nil {
		return nil, "", fmt.Errorf("provider %q is not configured", kind)
	}
	if len(rt.Pool.List()) == 0 {
		return nil, "", fmt.Errorf("provider %q has no account", kind)
	}
	return rt, upstream, nil
}

// usageTokens 从 OpenAI usage 对象提 prompt/completion tokens；
// 缺失/异常时返回 (0,0,"none")，上游正常时 src="upstream"。
// 兼容 Anthropic 字段名（input_tokens/output_tokens）——traework 的 SOLO token_usage
// 可能带非 OpenAI 字段，做一次别名兼容更稳妥。
func usageTokens(u map[string]any) (pt, ct int64, src string) {
	if u == nil {
		return 0, 0, "none"
	}
	pt = numField(u, "prompt_tokens", "input_tokens")
	ct = numField(u, "completion_tokens", "output_tokens")
	if pt == 0 && ct == 0 {
		// 有 usage 对象但无有效字段：按缺失处理（不算失败）
		return 0, 0, "none"
	}
	return pt, ct, "upstream"
}

// numField 取 usage 里首个存在且可转数值的字段。
func numField(u map[string]any, keys ...string) int64 {
	for _, k := range keys {
		if v, ok := u[k].(float64); ok {
			return int64(v)
		}
	}
	return 0
}

// ledgerModelOf 把客户端模型 ID 归一为流水里的模型名（= 上游模型名）。
// 与 provider.UpstreamModelOf 同源：新旧写法（cline-free/x 与 cline/cline-free/x）
// 必须落同一行，否则同一模型会在用量榜上裂成两行；裸名原样返回。
func ledgerModelOf(clientModel string) string { return provider.UpstreamModelOf(clientModel) }

// modelHasCapacity 报告 rt 渠道此刻是否有账号能承载 model。
//
// 一次 Pool.List() 得出答案，口径与 no_healthy_account 的「模型全冷」分支一致：
// 跳过禁用/整号冷却的账号后，看是否还有账号不在该模型的模型级冷却中
// （pool.statusOf 已剔掉过期条目，此处再比一次到期时间属防御）。
//
// 注意本函数对「整渠道全灭」（无任何健康账号）同样返回 false——
// 这正是降级链该顶上的两种形态（模型全冷 / 整渠道全灭），调用方不得
// 再额外要求「渠道内有健康账号」才降级（R54，PR #127 修正的旧判据）。
func modelHasCapacity(rt *Runtime, model string) bool {
	now := time.Now()
	for _, s := range rt.Pool.List() {
		if s.Disabled || s.Cooling {
			continue
		}
		if t, ok := s.ModelCooling[model]; ok && t.After(now) {
			continue
		}
		return true
	}
	return false
}

// pickModelFallback 模型降级链解析：请求模型在本渠道已无账号可承载时
// （两种形态——①「模型全冷」：渠道内有健康账号，但该模型在全部账号上处于
// 模型级限流冷却；②「整渠道全灭」：渠道内已无任何健康账号，含全部禁用与
// pool 的「多模型接连限流升级整号冷却」）时，按规则顺序（规则间按配置顺序、
// 链内按 To 顺序）找第一个有容量的备胎模型全路径 ID；无规则/无可用备胎返回空串。
// 备胎允许跨渠道（To 为 "channel/model" 全路径，经 runtimeForModel 解析）——
// 整渠道全灭时正需要跨渠道备胎顶上，而非直接 503。
//
// skipSameChannel=true：跳过与当前渠道相同的备胎候选（仍会继续往链后找跨渠道的）。
// 只给「请求内重入」用——那里 429 可能只是账号级限流，同渠道兄弟模型共享同一批
// 账号，重打一遍会二次撞墙，并可能把冷却粒度从模型级升级为账号级；入口降级传 false
// （入口已知「该模型在本渠道全冷」，换同渠道兄弟模型正是链配置的本意）。
//
// 第二返回值 primaryAvail = 「原模型此刻有可用账号」，与降级与否无关，只给恢复探针用：
// 旧实现「未降级就算恢复」会把两种情形谎报成已恢复——① 原模型仍全冷但备胎也没容量，
// ② 整渠道无健康账号。仅面板事件记账受影响，但谎报会令「持续时长」失真。
// 未配置规则时不做探测（守住「正常路由零开销」）：此时不会新增事件，
// 代价是删规则后残留的进行中事件不会自动关闭（环形 200 条自然淘汰，重启即清）。
func (h *Handler) pickModelFallback(rt *Runtime, model string, skipSameChannel bool) (string, bool) {
	h.fbMu.RLock()
	rules := h.fallbacks
	h.fbMu.RUnlock()
	if len(rules) == 0 {
		return "", false
	}
	if modelHasCapacity(rt, model) {
		return "", true // 未全冷：走原路由，并告知探针「容量已在」
	}
	// 未全冷之外（模型全冷 / 整渠道全灭）都继续往下遍历规则找备胎：
	// 链内无容量的候选由 modelHasCapacity 跳过；同渠道候选是否跳过由入参决定。
	for _, rule := range rules {
		// 规则里的 from 与请求模型都要按同一套 ID 解析（R59：cline 的 `cline-free/x`
		// 与旧写法 `cline/cline-free/x` 必须都能命中同一条规则）。
		rk, rUp, rok := provider.SplitModelID(rule.From)
		if !rok || rk != rt.Kind || rUp != model {
			continue
		}
		for _, to := range rule.To {
			to = strings.TrimSpace(to)
			if to == "" {
				continue
			}
			rt2, m2, err := h.runtimeForModel(to)
			if err != nil || rt2 == nil {
				continue
			}
			if skipSameChannel && rt2.Kind == rt.Kind {
				// 同渠道备胎共享同一批账号：请求内 429 可能只是账号级限流，
				// 重打一遍必然二次撞墙（详见调用点注释）。继续往链后找跨渠道候选。
				continue
			}
			if modelHasCapacity(rt2, m2) {
				return to, false
			}
		}
	}
	return "", false
}

// usageCredit 从 usage 对象取上游下发的单次积分（WorkBuddy 系会回 `credit`；
// comate 回 `credits`（复数，如 0.01）→ 两种都接；
// TraeWork 系 token_usage 帧无此字段 → 0，引擎侧按渠道余额差值分摊估算）。
//
// 为什么必须接复数：`internal/comate/constants.go` 实测上游下发
// `{"prompt_tokens":…,"completion_tokens":…,"credits":0.01}`，而该渠道
// **没有余额端点**（UserResource 恒 0）⇒ 只认单数就等于“comate 的积分永远不入账”，
// README 承诺的“按每次回答的 usage.credits 记账”落空（2026-10-28 修正）。
func usageCredit(u map[string]any) float64 {
	for _, k := range []string{"credit", "credits"} {
		if v, ok := u[k].(float64); ok {
			return v
		}
	}
	return 0
}

// statWriter 记录首字节写出时刻（TTFB 口径：客户端等到的第一个响应字节，
// 含状态行/响应头——WriteHeader/Write/Flush 三者先到先记），并顺带按秒统计
// 写出的帧数（流式吞吐滑动窗口的唯一数据源）。
type statWriter struct {
	http.ResponseWriter
	start   time.Time
	ttfbMs  int64
	written bool
	// chunks unix 秒 -> 该秒写出的非空 Write 次数。
	// 一次 SSE 帧 = 一次 Write（streamCore/各渠道 Stream 都是逐帧 Write），
	// 故这是「token 投递按秒分布」的代理量：token 精确值只有末帧 usage 才有，
	// 完成后按本表占比把 completion token 摊到各秒（见 stats.AddTput）。
	chunks map[int64]int
}

func (sw *statWriter) noteFirstByte() {
	if !sw.written {
		sw.written = true
		sw.ttfbMs = time.Since(sw.start).Milliseconds()
	}
}

func (sw *statWriter) WriteHeader(code int) {
	sw.noteFirstByte()
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statWriter) Write(p []byte) (int, error) {
	sw.noteFirstByte()
	// 空写不计帧：JSON 空帧与心跳不该算成一次 token 投递。
	if len(p) > 0 {
		if sw.chunks == nil {
			sw.chunks = map[int64]int{}
		}
		sw.chunks[time.Now().Unix()]++
	}
	return sw.ResponseWriter.Write(p)
}

// Flush 透传给内层 ResponseWriter（流式 SSE 依赖；首帧 flush 也算首字节时机）。
func (sw *statWriter) Flush() {
	sw.noteFirstByte()
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// rewriteModel 改写发往上游的请求体：修补工具轮的空 content（见
// normalizeToolTurnContent），并把 model 覆盖为路由后的真实模型名。
func rewriteModel(body []byte, model string) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	normalizeToolTurnContent(obj)
	obj["model"] = model
	return json.Marshal(obj)
}

// applyEffort 思考强度档位注入收口：未传且命中面板配置（lookupEffort 精确未命中回退
// 通配 `*`）时填入 reasoning_effort；键已存在（任意值含 null，客户端显式意图）不碰；
// 未配置（空 map，未配置 = 常态）零开销直通。注入失败不中断——原样返回旧 body，
// 让上游给出权威结果（同模型降级链「仅触发才干活」哲学，也与 rewriteModel 失败
// 处理同套路）。
func (h *Handler) applyEffort(body []byte, model string) []byte {
	h.efMu.RLock()
	ef, ok := lookupEffort(h.efforts, model)
	h.efMu.RUnlock()
	if !ok {
		return body
	}
	nb, changed, err := injectEffortField(body, ef)
	if err != nil {
		log.Printf("effort 注入失败 %s: %v", model, err)
		return body
	}
	if changed {
		log.Printf("effort inject: %s -> %s", model, ef)
		return nb
	}
	return body
}

// lookupEffort 思考强度映射查询：精确命中优先，未命中回退 `*` 全局默认；皆无 = 不注入。
// 空 map / 空模型名一律未命中（调用方无需再加前置判断）。
func lookupEffort(efforts map[string]string, model string) (string, bool) {
	if len(efforts) == 0 || model == "" {
		return "", false
	}
	if ef, ok := efforts[model]; ok {
		return ef, true
	}
	ef, ok := efforts["*"]
	return ef, ok
}

// injectEffortField 思考强度档位注入：请求体顶层**无** reasoning_effort 键时填入配置
// 档位。整体 map 解析 → 增键 → Marshal，保留其余全部字段；顶层键已存在（任意值含
// null——客户端显式意图）不碰，返回 changed=false。失败返回 error，调用方保持原样
// 转发（绝不因注入失败中断请求）。
func injectEffortField(body []byte, ef string) (nb []byte, changed bool, err error) {
	var obj map[string]any
	if err = json.Unmarshal(body, &obj); err != nil {
		return nil, false, err
	}
	if obj == nil {
		return nil, false, errors.New("body 不是 JSON 对象")
	}
	if _, ok := obj["reasoning_effort"]; ok {
		return nil, false, nil
	}
	obj["reasoning_effort"] = ef
	nb, err = json.Marshal(obj)
	if err != nil {
		return nil, false, err
	}
	return nb, true, nil
}

// normalizeToolTurnContent 把「content 为 null 或缺失」的 assistant / tool 消息补成空串。
//
// 上游（实测 qoder/deepseek-flash，2026-09-24）要求带 tool_calls 的 assistant
// 消息必须携带**字符串** content，null 或字段缺失会让它整请求拒答，并且报一个
// 完全误导的错：
//
//	Messages with role 'tool' must be a response to a preceding message with 'tool_calls'
//
// 实测矩阵（同一份客户端真实请求，只改这一处）：
//
//	assistant.content = null  → 200 + provider_error 帧（空流）
//	assistant.content = ""    → 正常出流
//	content 字段缺失          → 200 + provider_error 帧（空流）
//
// 危害在于**中毒进历史**：客户端在「模型只回工具调用、没输出正文」时会把该
// assistant 消息的 content 落成 null，之后这一轮永远留在 messages 里，于是该
// 会话的每一发请求都被上游拒（重启客户端也不恢复，只能新建会话）。
//
// 放宽到 tool 角色一并处理：工具返回空内容时客户端同样可能给 null，上游对工具
// 轮是同一套校验。
//
// 2026-10-11 追加（traecode/SOLO 实测）：**普通 assistant 消息**（无 tool_calls）
// content 缺失同样会被整请求拒答，错误是 SOLO 流内 event:error 的
// `4001 the param is invalid`。成因是 CodeBuddy 客户端在「思考中途被打断」时，
// 会把半截思考存成 `{"role":"assistant","reasoning":"…"}`（无 content、无
// tool_calls）；此后该会话每轮重放都带上这条空壳，于是每发必拒（同「中毒进
// 历史」）。重放对照坐实：原样 → 0.6s 即 4001；删掉该条或补 content:"" → 均
// 正常出流。故 assistant 一律补空串，不再以 tool_calls 为前提。
func normalizeToolTurnContent(obj map[string]any) {
	msgs, _ := obj["messages"].([]any)
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "assistant", "tool":
		default:
			continue
		}
		if c, exists := m["content"]; !exists || c == nil {
			m["content"] = ""
		}
	}
}

// ChannelModels 返回每个渠道当前生效的模型列表，与 /v1/models 同源
// （含动态拉取与静态兜底），只包含已接入账号的渠道。
// 供管理端（费率面板）复用，保证「模型列表」与「模型费率」基于同一份清单。
// 注意：会触发上游网络请求（fetchRuntimeModels），费控面板场景用 CachedChannelModels 避免阻塞。
func (h *Handler) ChannelModels() map[provider.Kind][]provider.ModelInfo {
	ks := h.runtimeKinds()
	out := make(map[provider.Kind][]provider.ModelInfo, len(ks))
	for _, k := range ks {
		rt := h.getRuntime(k)
		if rt == nil || rt.Pool == nil || len(rt.Pool.List()) == 0 {
			continue
		}
		infos := h.fetchRuntimeModels(rt)
		if len(infos) == 0 {
			infos = rt.StaticModels
		}
		out[k] = infos
	}
	return out
}

// CachedChannelModels 返回各渠道生效的模型列表，仅用内存缓存/静态兜底，不触发上游网络请求。
// 供费控面板等需要即时返回的场景使用（后台刷新比用户点面板快）。
func (h *Handler) CachedChannelModels() map[provider.Kind][]provider.ModelInfo {
	ks := h.runtimeKinds()
	out := make(map[provider.Kind][]provider.ModelInfo, len(ks))
	for _, k := range ks {
		rt := h.getRuntime(k)
		if rt == nil || rt.Pool == nil || len(rt.Pool.List()) == 0 {
			continue
		}
		// 取缓存：优先「上次成功拉取」的动态表，**哪怕已过 TTL**（issue #40）。
		// 过期数据比空数据好——尤其 qodercn/qodercom 没有静态兜底表，
		// 若在这里回退 StaticModels(nil)，整个渠道分组会从面板消失。
		// 陈旧性由后台 StartPricingAutoRefresh（30min）收敛，TTL 只决定
		// 下次是否重新拉取，不决定「要不要展示」。
		rt.mu.RLock()
		models := rt.models
		rt.mu.RUnlock()
		if len(models) > 0 {
			out[k] = models
			continue
		}
		out[k] = rt.StaticModels
	}
	return out
}

// InvalidateModels 清空各渠道的动态模型缓存，使下次读取重新拉取上游。
// 新增账号后调用：否则新渠道/新模型要等 TTL 到期才出现。
func (h *Handler) InvalidateModels() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	for _, k := range h.runtimeKinds() {
		rt := h.getRuntime(k)
		if rt == nil {
			continue
		}
		rt.mu.Lock()
		rt.models = nil
		rt.fetched = time.Time{}
		rt.lastFail = time.Time{}
		rt.mu.Unlock()
	}
}

func (h *Handler) runtimeKinds() []provider.Kind {
	h.runtimesMu.RLock()
	customCopy := make(map[provider.Kind]bool, len(h.customRts))
	for k := range h.customRts {
		customCopy[k] = true
	}
	h.runtimesMu.RUnlock()

	ks := make([]provider.Kind, 0, len(h.cfg.Runtimes)+len(customCopy))
	seen := map[provider.Kind]bool{}
	for k := range h.cfg.Runtimes {
		ks = append(ks, k)
		seen[k] = true
	}
	for k := range customCopy {
		if !seen[k] {
			ks = append(ks, k)
		}
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i] < ks[j] })
	return ks
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// DefaultMaxRequestBody 请求体默认上限（128MiB）。
//
// 沿革：8MiB → 32MiB（issue #30，多模态大图 base64 后易超限）→ 128MiB（issue #176）。
// 提不动的原因不是「图大」，而是**无状态客户端每轮重放整段历史**：Codex 的一个会话
// 累积 12 张 2048px 截图（33.01MiB base64，文本仅 0.43MiB），请求体 33.44MiB 直接撞
// 32MiB 硬限回 413，会话报废。gateway 侧另有 `imageURLBlock` 在转发前把图缩到 1568px
// / JPEG（同 12 张 → 约 2.9MiB），所以上游实际收到的仍很小；此处上限只需容纳
// **客户端原始体**。仍可经 config.json 的 `server.max_request_body_mb` 调整。
const DefaultMaxRequestBody = 128 << 20

// MaxRequestBody 当前生效的请求体上限。默认 DefaultMaxRequestBody，
// 启动时按配置覆盖（见 NewHandler）。保留为包级变量是为兼容既有测试与调用方。
var MaxRequestBody int64 = DefaultMaxRequestBody

// ReadBodyLimited 读取请求体：超过 MaxRequestBody 时回 413 并返回错误。
// 用 LimitReader(max+1) 多读 1 字节以区分「恰好 max」与「超限」。
func ReadBodyLimited(r *http.Request) ([]byte, error) {
	limit := MaxRequestBody
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		_ = r.Body.Close()
		return nil, tooLargeError(limit)
	}
	return raw, nil
}

// errTooLarge 超限哨兵（调用方用 errors.Is 判定后回 413）。
var errTooLarge = errors.New("request body too large")

// IsBodyTooLarge 供 gateway 层判定「入站请求体超限」（跨包无法引用未导出哨兵）。
func IsBodyTooLarge(err error) bool { return errors.Is(err, errTooLarge) }

// tooLargeError 构造超限错误：包住哨兵以便 errors.Is 命中，文案带实际上限。
// MiB 数字由生效上限推导，避免改上限后文案不同步。
func tooLargeError(limit int64) error {
	return fmt.Errorf("%w: exceeds limit of %d bytes (%d MiB); please reduce request size",
		errTooLarge, limit, limit>>20)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": "api_error", "code": code}})
}

// transparentError 上游错误原文透传：status+body 原样写回，不包装。
func transparentError(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// clearSSEHeaders 撑掉渠道 StreamCapture 预先写进 header 的 SSE 头。
// 排队/业务错误帧发现于**未写出任何字节**之时，此时撤掉这些头，后续才能干净地
// 写回 JSON 错误（否则客户端会按 text/event-stream 解析一个 JSON 体）。
func clearSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Del("Content-Type")
	h.Del("Cache-Control")
	h.Del("Connection")
	h.Del("X-Accel-Buffering")
}

// queuedBody 构造「全号排队」的出口错误体：OpenAI 错误信封 + 排队元数据。
// 客户端/agent 读 code=model_queued 即知“不是渠道挂了，是模型拥塞，等 retry_after 再试”。
func queuedBody(q *provider.Queued) []byte {
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message":             q.Error(),
		"type":                "upstream_error",
		"code":                "model_queued",
		"retry_after_seconds": int(q.Cooldown().Seconds()),
		"queue_count":         q.QueueCount,
		"wait_seconds":        q.WaitSeconds,
	}})
	return payload
}

// isNetworkError 判定是否**本机传输层/DNS**故障（与账号无关）。
// 判据：http.Client.Do 包装的 *url.Error / net.Error，或文案里出现 DNS/拒连特征。
// 用途：refresh 失败要区分「号坏了（换号有效）」与「网络坏了（换号无用）」。
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	s := err.Error()
	for _, m := range []string{"dial tcp", "no such host", "connection refused",
		"connection reset", "i/o timeout", "timeout awaiting response headers", "network is unreachable"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// softRateResetMaxCap 解析出的重置时长上限：上游时间戳异常（如错给一年后）时不至于把号冻死。
const softRateResetMaxCap = 24 * time.Hour

// softRateCooldown 从限流响应体提取真实重置时间，冷却到该时刻（而非固定 60s）。
// 命中形态：WorkBuddy 6004「...将在 2026-09-23 14:47:19 UTC+8 重置...」——该类限流
// 按重置点解封，60s 软冷却过期后账号会被反复选中反复 429。解析失败/时长为非正值时
// 回退默认 SoftCooldown。时间戳按 UTC+8 墙钟解析，与到期字段口径一致（AGENTS §6.18）。
// 已知形态「2026-09-23 14:47:19」可能带「UTC+8」后缀，解析前剥掉。
// hardCooldown 本次硬冷却（402 余额/权益不足）应持续多久。
// 默认恒为 HardCooldown；装配层注入 HardCooldownFor 时按其计算（当前策略：
// 截到下一个每日发奖窗口，见 config.HardCooldownFor）——qwenwork 每日 00:00
// UTC+8 自动发额度，固定 12h 会把 19:07 的 402 锁到次日 07:07，白锁 7 小时。
func (h *Handler) hardCooldown() time.Duration {
	if h.cfg.HardCooldownFor != nil {
		if d := h.cfg.HardCooldownFor(time.Now()); d > 0 {
			return d
		}
	}
	return h.cfg.HardCooldown
}

func (h *Handler) softRateCooldown(body string) time.Duration {
	ts, ok := softRateResetTs(body)
	if !ok {
		return h.cfg.SoftCooldown
	}
	d := time.Until(ts)
	if d <= 0 {
		return h.cfg.SoftCooldown
	}
	if d > softRateResetMaxCap {
		d = softRateResetMaxCap
	}
	return d
}

// softRateResetTs 提取限流响应体中的重置时间戳（UTC+8 墙钟）；未命中返回 false。
// 命中形态：国内版「...将在 2026-09-23 14:47:19 UTC+8 重置...」；国际版
// workbuddyai 6004「...usage will reset at 2026-10-14 12:00:00 UTC+8, ...」。
func softRateResetTs(body string) (time.Time, bool) {
	if ts, ok := softRateResetAt(body, "将在 ", " 重置"); ok {
		return ts, true
	}
	return softRateResetAt(body, "will reset at ", ",")
}

// softRateResetAt 定位 marker 之后、endMarker 之前的时间戳并按 UTC+8 墙钟解析。
func softRateResetAt(body, marker, endMarker string) (time.Time, bool) {
	i := strings.Index(body, marker)
	if i < 0 {
		return time.Time{}, false
	}
	rest := body[i+len(marker):]
	end := strings.Index(rest, endMarker)
	if end < 0 {
		return time.Time{}, false
	}
	s := strings.TrimSpace(rest[:end])
	// 「UTC+8」是时区说明而非时间一部分（layout 无法解析），剥掉
	if suffix := " UTC+8"; strings.HasSuffix(s, suffix) {
		s = strings.TrimSuffix(s, suffix)
	}
	const layout = "2006-01-02 15:04:05"
	loc := time.FixedZone("UTC+8", 8*3600)
	reset, err := time.ParseInLocation(layout, s, loc)
	if err != nil {
		return time.Time{}, false
	}
	return reset, true
}

// truncateBody 错误日志/聚合 lastErr 用的响应体摘要。
func truncateBody(body []byte) string {
	const max = 200
	s := string(body)
	if len(s) > max {
		return s[:max]
	}
	return s
}

func WorkBuddyStaticModels() []provider.ModelInfo {
	return append([]provider.ModelInfo{}, workbuddyStaticModels...)
}
func TraeWorkStaticModels() []provider.ModelInfo {
	return append([]provider.ModelInfo{}, traeworkStaticModels...)
}
func TraeCodeStaticModels() []provider.ModelInfo {
	return append([]provider.ModelInfo{}, traecodeStaticModels...)
}

// WorkBuddyAIStaticModels 国际版静态模型表实际定义在 internal/workbuddyai 包，
// 由 cmd 直接引用 workbuddyai.StaticModels()，此处不再重复。
