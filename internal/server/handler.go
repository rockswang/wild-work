// Package server 暴露 OpenAI 兼容 HTTP 接口，按模型名前缀路由到不同上游。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/ledger"
	"wild-work/internal/loopguard"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/reasoning"
	statsEngine "wild-work/internal/stats"
	"wild-work/internal/upstream"
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
	// 其余渠道保持 false，行为不变。
	NoCooldownOnServerError bool

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

	MaxRotate    int
	HardCooldown time.Duration
	SoftCooldown time.Duration
	ErrThreshold int
	ErrCooldown  time.Duration
	RefreshSkew  time.Duration

	// ReasoningEffort 思考强度默认档：客户端未表达思考意图时注入的兜底值
	// （标准档位 none/minimal/low/medium/high/xhigh/max/ultra；空串 = 不注入）。
	// 来源 config.Compat.ReasoningEffort，面板保存后经 SetReasoningEffort 热更新。
	ReasoningEffort string

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
	lastUsed time.Time // 最近一次命中/成功时间，供粘性表容量淘汰使用
}

// maxStickyEntries 粘性记录条数上限。
// key 含模型后条目数 = 渠道数 × 客户端请求过的模型数，不再有天然上界（此前最多 = 渠道数），
// 故设上限并在超出时淘汰最久未用者，避免长期运行下 map 无界增长。
const maxStickyEntries = 256

// StickyState 粘性路由快照条目。
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

	efMu    sync.RWMutex      // 保护 efforts（面板可运行时修改）
	efforts map[string]string // 思考强度注入映射（cfg.ReasoningEfforts 初始，热更新经 SetReasoningEfforts）

	runtimesMu sync.RWMutex               // 保护 customRts 动态热更新（R66）
	customRts  map[provider.Kind]*Runtime // 自定义渠道运行时

	reasoningMu     sync.RWMutex // 保护 reasoningEffort（面板可运行时修改）
	reasoningEffort string       // 默认思考档，见 Config.ReasoningEffort
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
	h.fbMu.Lock()
	h.fallbacks = rules
	h.fbMu.Unlock()
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
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
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
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), sticky: make(map[string]*stickyEntry), ledger: cfg.Ledger, stats: cfg.Stats, fallbacks: cfg.ModelFallbacks, efforts: cfg.ReasoningEfforts}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
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

// stickyKey 粘性路由 key：按渠道 + 模型独立。
// 必须含模型：上游按模型独立限流/计费，若只按渠道粘性，A 账号在 flash 上被限流后
// 粘性会整体切到 B，A 上仍可用的 luna 也被一起粘走（A 每次轮换只得到一次请求机会）。
// model 用 runtimeForModel 解析出的「去渠道前缀后的模型名」，它同时也是出站 body 的
// model 字段值（prepareChatBody 覆盖写入），因此同一客户端对同一模型写法稳定。
func (h *Handler) stickyKey(kind provider.Kind, model string) string {
	return kind.String() + "\x00" + model
}

// pickWithSticky 粘性路由选择账号。
// 优先使用上次成功路由的账号，直到：
//   - 账号进入冷却/禁用状态（含本次请求模型上的模型级冷却）
//   - 连续成功请求达到 maxReqs 次（默认 50），自动轮换
//
// 任一条件触发则降级为 Pick(model) 选新账号并重置粘性记录。
func (h *Handler) pickWithSticky(rt *Runtime, model string) *auth.Auth {
	const defaultMaxReqs = 50
	key := h.stickyKey(rt.Kind, model)

	// 粘性记录在锁内取值后即释放：reqCount 会被 stickySuccess 并发递增，
	// 锁外读字段会与写并发（指针本身稳定，字段不稳定）。
	h.stickyMu.RLock()
	var stickyUID string
	var stickyCount, stickyMaxReqs int
	if sticky := h.sticky[key]; sticky != nil {
		stickyUID, stickyCount, stickyMaxReqs = sticky.uid, sticky.reqCount, sticky.maxReqs
	}
	h.stickyMu.RUnlock()

	// 尝试粘性路由
	if stickyUID != "" && stickyCount < stickyMaxReqs {
		acct := rt.Pool.AuthByUID(stickyUID)
		if acct != nil {
			status, ok := rt.Pool.Status(stickyUID)
			// 模型级冷却同样参与判定：A 在 flash 上被限流时，flash 的粘性记录必须失效，
			// 否则会一直粘着打不通的账号；luna 的粘性记录独立，不受影响。
			if ok && !status.Cooling && !status.Disabled && !modelCooling(status, model) {
				log.Printf("sticky route platform=%s model=%s uid=%s count=%d/%d",
					rt.Kind, model, stickyUID, stickyCount, stickyMaxReqs)
				return acct
			}
		}
	}

	// 降级：按临期优先 → 余额降序选号（同时排除在该模型上冷却的账号）
	acct := rt.Pool.Pick(model)
	if acct == nil {
		return nil
	}

	// 新建粘性记录
	h.stickyMu.Lock()
	h.sticky[key] = &stickyEntry{uid: acct.UID, maxReqs: defaultMaxReqs, lastUsed: time.Now()}
	h.evictStickyLocked()
	h.stickyMu.Unlock()
	log.Printf("new sticky route platform=%s model=%s uid=%s maxReqs=%d", rt.Kind, model, acct.UID, defaultMaxReqs)
	return acct
}

// modelCooling 该账号当前是否在 model 上处于模型级冷却。
func modelCooling(st pool.Status, model string) bool {
	for _, mc := range st.ModelCooling {
		if mc.Model == model {
			return true
		}
	}
	return false
}

// evictStickyLocked 粘性表容量控制（调用方须持写锁）：淘汰最久未用者，使条数回到上限内。
// 命中条目的 lastUsed 由 stickySuccess 刷新；只被失败清掉的条目不会留下。
func (h *Handler) evictStickyLocked() {
	if len(h.sticky) <= maxStickyEntries {
		return
	}
	type kv struct {
		key string
		at  time.Time
	}
	all := make([]kv, 0, len(h.sticky))
	for k, e := range h.sticky {
		all = append(all, kv{k, e.lastUsed})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for i := 0; i < len(all)-maxStickyEntries; i++ {
		delete(h.sticky, all[i].key)
	}
}

// recoverSession 遇上游 ErrSessionDead 时先试一次 token refresh（不变量 19）。
//
// 为什么不能在 handler 里直接 Disable：本地 expiresAt 与上游真实状态可能不同步——
// 同一账号在另一实例/官方客户端刷新过（refresh token 轮换）但本进程未落盘时，
// 本地看起来完全健康，上游却已拒绝旧 access token。此时 refresh 一步就能救回，
// 直接 Disable 会把一个完全正常的账号永久停掉（需人工重登）。
// 只有 refresh 也失败（会话真被吊销，如 invalid_grant）才回到 Disable 路径。
//
// 成功必须 SaveAtomic：refresh token 会轮换，不落盘则下次启动重回死锁（不变量 20）。
func (h *Handler) recoverSession(rt *Runtime, acct *auth.Auth, respBody []byte) bool {
	if acct == nil || acct.RefreshTokenValue() == "" {
		return false
	}
	log.Printf("session dead, refreshing platform=%s uid=%s body=%s", rt.Kind, acct.UID, truncateBody(respBody))
	if err := rt.Upstream.RefreshToken(acct); err != nil {
		log.Printf("session dead refresh failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, err)
		return false
	}
	if serr := acct.SaveAtomic(); serr != nil {
		log.Printf("session dead refresh save failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, serr)
	}
	log.Printf("session dead recovered platform=%s uid=%s expires_at=%d", rt.Kind, acct.UID, acct.ExpiresAt)
	return true
}

// stickySuccess 粘性路由成功：递增请求计数并刷新最近使用时间。
func (h *Handler) stickySuccess(rt *Runtime, model string) {
	h.stickyMu.Lock()
	defer h.stickyMu.Unlock()
	key := h.stickyKey(rt.Kind, model)
	if e := h.sticky[key]; e != nil {
		e.reqCount++
		e.lastUsed = time.Now()
	}
}

// stickyClear 粘性路由失败（错误/冷却）：清除该 (渠道,模型) 的粘性记录，下次请求强制重新选号。
// 只清当前模型：其他模型的粘性记录各自独立，不应被本次失败牵连。
func (h *Handler) stickyClear(rt *Runtime, model string) {
	h.stickyMu.Lock()
	defer h.stickyMu.Unlock()
	delete(h.sticky, h.stickyKey(rt.Kind, model))
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

// SetReasoningEffort 热更新默认思考档（面板保存 compat 后调用）。
// 取值已由 config 层归一化：空串 = 不注入。
func (h *Handler) SetReasoningEffort(v string) {
	h.reasoningMu.Lock()
	defer h.reasoningMu.Unlock()
	h.reasoningEffort = v
}

func (h *Handler) currentReasoningEffort() string {
	h.reasoningMu.RLock()
	defer h.reasoningMu.RUnlock()
	return h.reasoningEffort
}

// reasoningDefaultFor 返回该渠道可用的默认思考档。
// WorkBuddy 国内版/国际版与 Qoder 支持：Qoder 的档位来自模型目录的
// thinking_config ladder，由 qoder 渠道层按模型能力就近降级（Clamp），
// 因此这里只需把面板配置的默认档原样注入，具体能不能用交给渠道层判断。
// TraeWork 协议没有可验证的思考控制字段，注入默认档只会被上游忽略或报错。
func (h *Handler) reasoningDefaultFor(k provider.Kind) string {
	if reasoning.SupportsEffortKind(k.String()) {
		return h.currentReasoningEffort()
	}
	return ""
}

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
)

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": h.modelList()})
}

func (h *Handler) modelList() []map[string]any {
	out := []map[string]any{}
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
	// 只透出上游声明的容量：硬编码估算值（ContextFromAPI=false）不当作真实值，
	// 与面板「未知」的口径保持一致，避免客户端按错误预算裁剪上下文。
	if mi.ContextFromAPI && mi.ContextWindow > 0 {
		entry["context_length"] = mi.ContextWindow
	}
	if mi.ContextFromAPI && mi.MaxTokens > 0 {
		entry["max_output_tokens"] = mi.MaxTokens
	}
	// 上游声明的可选上下文档位（Qoder context_config），供客户端/面板选择。
	if len(mi.ContextOptions) > 0 {
		entry["context_options"] = mi.ContextOptions
	}
	if mi.SupportsTools {
		entry["supports_tools"] = true
	}
	entry["architecture"] = map[string]any{
		"input_modalities": mi.InputModalities(),
		"modality":         mi.Modality(),
	}
	// 思考能力与可选档位：三个渠道有可验证的档位能力（WorkBuddy 双面 + Qoder），
	// 远端声明优先、静态兜底表补齐、皆无则省略字段（不输出空数组）。
	if mi.SupportsReasoning {
		entry["supports_reasoning"] = true
	}
	if efforts, def := reasoning.ListingForKind(k.String(), mi.ID, mi.SupportedEfforts, mi.DefaultEffort); len(efforts) > 0 {
		entry["reasoning_supported_efforts"] = efforts
		if def != "" {
			entry["reasoning_default_effort"] = def
		}
	}
	if len(mi.ReasoningEfforts) > 0 || mi.DefaultReasoningEffort != "" {
		reasoning := map[string]any{}
		if len(mi.ReasoningEfforts) > 0 {
			reasoning["supported_efforts"] = mi.ReasoningEfforts
		}
		if mi.DefaultReasoningEffort != "" {
			reasoning["default_effort"] = mi.DefaultReasoningEffort
		}
		if mi.CanDisableThinking != nil {
			reasoning["can_disable_thinking"] = *mi.CanDisableThinking
		}
		entry["reasoning"] = reasoning
		entry["supports_reasoning"] = true
	}
	return entry
}

// publishEffortCaps 把目录接口返回的档位能力写入 internal/reasoning 的能力表。
// 远端值为权威（投影与 /v1/models 共用同一份表）；空结果不覆盖既有能力。
// 只处理有档位能力的渠道（WorkBuddy 双面 + Qoder）：TraeWork 协议没有该字段。
func publishEffortCaps(kind provider.Kind, infos []provider.ModelInfo) {
	if !reasoning.SupportsEffortKind(kind.String()) {
		return
	}
	caps := make(map[string]reasoning.Cap, len(infos))
	for _, mi := range infos {
		if len(mi.SupportedEfforts) == 0 && mi.DefaultEffort == "" && !mi.ReasoningCanDisable {
			continue
		}
		caps[mi.ID] = reasoning.Cap{
			Efforts:         mi.SupportedEfforts,
			DefaultEffort:   mi.DefaultEffort,
			SupportsDisable: mi.ReasoningCanDisable,
		}
	}
	reasoning.Caps.SetRemote(reasoning.RealmForKind(kind.String()), caps)
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
	// 拉模型目录与具体模型无关，故不排除任何模型级冷却（model 传空）。
	acct := rt.Pool.Pick("")
	if acct == nil {
		return nil
	}
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
	infos, err := rt.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 失败不再静默：无静态兑底渠道（qodercn/qodercom）整组消失时，
		// 至少日志里要有原因可查（曾致 issue #40 复发 40 分钟无法定位）。
		now := time.Now()
		rt.mu.Lock()
		rt.lastFail = now
		rt.mu.Unlock()
		if rt.Kind == provider.WorkBuddy {
			dynamicModelsCache.Lock()
			dynamicModelsCache.lastFail = now
			dynamicModelsCache.Unlock()
		}
		log.Printf("models fetch failed platform=%s uid=%s err=%v n=%d", rt.Kind, acct.UID, err, len(infos))
		return nil
	}
	now := time.Now()
	rt.mu.Lock()
	rt.models = infos
	rt.fetched = now
	rt.lastFail = time.Time{}
	rt.mu.Unlock()
	publishEffortCaps(rt.Kind, infos)
	if rt.Kind == provider.WorkBuddy { // 兼容旧测试观察点
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = infos
		dynamicModelsCache.fetched = now
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	}
	return infos
}

// ensureEffortCaps 请求路径的档位能力表预热。
// 远端目录此前只在「列模型」（/v1/models 与面板费率表）时拉取，纯 API 调用方
// 从不列模型，档位投影会一直在无远端能力的状态下进行。这里只在尚无远端能力时
// 触发一次拉取——fetchRuntimeModels 自带 TTL 与失败冷却，且远端一旦成功下发
// 即长期保留，因此正常情况每个进程只多付一次目录请求。
func (h *Handler) ensureEffortCaps(rt *Runtime) {
	if rt == nil || !reasoning.SupportsEffortKind(rt.Kind.String()) {
		return
	}
	realm := reasoning.RealmForKind(rt.Kind.String())
	if reasoning.Caps.HasRemote(realm) {
		return
	}
	h.fetchRuntimeModels(rt)
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
	rt, model, err := h.runtimeForModel(peek.Model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_model", err.Error())
		return
	}
	clientModel := peek.Model // 客户端请求的原始模型名（含 channel/ 前缀），回填进响应
	// 上游报「参数被拒」时，客户端原始参数是唯一能定位「谁发的、发了什么」的依据
	// （改写后的出站参数由渠道层打印）——在 serveRotation 内按 body 现算。
	clientUA := r.UserAgent()
	// 档位能力表预热（首次请求才触发目录拉取，见 ensureEffortCaps）
	h.ensureEffortCaps(rt)
	// WILDWORK_DUMP_DIR 开启时落盘原始 body（可直接重放），默认 no-op；
	// 返回的序号把本次请求的入站/出站/错误响应三份 dump 关联起来。
	dumpSeqNo := dumpChatBody(body, clientModel, "client")
	// 诊断：记录上下文构成摘要（config.body_log 关闭时立即返回，无实际开销）。
	// 必须放在 rewriteModel 之前——要记的是客户端发来的原始体，而非改写后的。
	if h.ledger != nil {
		h.ledger.TraceBody(rt.Kind.String(), clientModel, body)
	}
	// 统计/流水的归一 ID：新旧写法（cline-free/x 与 cline/cline-free/x）归一到同一串，
	// 否则同一模型会在用量榜/请求日志里裂成两行（R59）；其余渠道为恒等变换。
	canonModel := provider.CanonicalModelID(clientModel)
	// 模型降级链：请求模型在本渠道已无账号可承载（no_healthy_account 的两种形态——
	// 「模型全冷」或「整渠道全灭」，pickWithSticky 必败）时，按用户配置的链找第一个
	// 仍有可用账号的备胎模型顶上；备胎可跨渠道，渠道全灭时不至于直接 503。
	// 仅此场景触发，正常路由零开销；不粘滞——每个请求都按原模型重新判定，上游解冻即自动切回。
	// fbApplied=实际备胎全路径（用于把响应 model 字段/X-Model-Fallback 头告知 agent）；
	// dispModel=写进响应的模型名（无降级=客户端请求名，保持官方「回填请求名」语义；降级=备胎全路径，
	// agent 读 chunk.model / resp.model 即可感知实际执行模型）。
	var fbApplied string
	fbID, primaryAvail := h.pickModelFallback(rt, model)
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
		if reasoning.IsInvalid(err) {
			// 客户端把思考强度写错（取值非法或自相矛盾）：明确回 400，
			// 不要当上游故障去轮转账号、冷却账号。
			writeOpenAIError(w, http.StatusBadRequest, "invalid_reasoning_control", err.Error())
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// 思考强度注入：客户端未传 reasoning_effort 且面板配置命中（按客户端请求模型 ID 匹配，
	// 含 `*` 全局默认）时补上档位。未配置 = 零开销直通（applyEffort 内部快检）；
	// 注入失败仅记日志、原 body 照发，绝不因此中断请求。
	body = h.applyEffort(body, clientModel)

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
		res := h.serveRotation(w, sw, rt, model, body, clientModel, dispModel, acctModel, &statRow, clientUA, dumpSeqNo, peek.Stream)
		if res.done {
			return
		}
		// 候选耗尽待透传（res.lastStatus>0）：方案 B 判定——只对积分耗尽
		// （ErrHardCredit：账号刚进 12h 硬冷却，本渠道容量已翻 false，此刻
		// pickModelFallback 必然命中规则或返回空）；仅重入一次（防备胎再降备胎
		// 死循环）；单账号渠道豁免（透传是惩罚豁免不变量的设计约束）；此刻未写过
		// 任何字节（轮转失败路径天然成立——所有写出都发生在成功流式/非流式分支
		// 内），降级头可安全补发；无规则/备胎无容量 → fb2 为空，维持原样透传，
		// 与现状一致。
		if fbAttempt == 0 && res.lastKind == provider.ErrHardCredit && !rt.SingleAccount {
			if fb2, _ := h.pickModelFallback(rt, model); fb2 != "" {
				if rt2, m2, err2 := h.runtimeForModel(fb2); err2 == nil {
					nb, err3 := rewriteModel(body, m2)
					if err3 == nil {
						log.Printf("model fallback platform=%s from=%s to=%s", rt.Kind, clientModel, fb2)
						// 响应头必须在任何 WriteHeader 之前（此刻未写过任何字节）；
						// 改写成功才发——失败走透传时不得残留降级头（S1）。
						w.Header().Set("X-Model-Fallback", clientModel+" -> "+fb2)
						body = nb
						body = h.applyEffort(body, clientModel) // 与入口同参数；键已存在不碰（双注入守卫）
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
		transparentError(w, res.lastStatus, res.lastBody)
		return
	}
}

// rotationResult：serveRotation 的判定结果。done=true=响应已完整写回（成功/请求级
// 错误/耗尽诊断引导），调用方直接收尾；done=false 且 lastStatus>0=候选耗尽待透传，
// 由调用方决定原样透传还是按降级链请求内重入（方案 B，只认 ErrHardCredit）。
type rotationResult struct {
	done       bool
	lastKind   provider.ErrKind
	lastStatus int
	lastBody   []byte
}

// serveRotation：chatCompletions 的轮转选号 + 上游交互 + 耗尽出口段（原代码整体
// 平移，零缩进变化；结果经命名返回值承载——done 默认 true，既有裸 return 语义
// 原样保留：所有提前返回的出口都已把响应写完）。轮转中「账号级可轮换」错误
// （kind.Rotatable()）换号续试并置位 lastKind；请求级/内容类错误在循环内提前
// 透传返回。方案 B 重入时由调用方完成全部降级状态同步后再次调用本方法（tried
// 随调用重建，备胎渠道天然全新）。
func (h *Handler) serveRotation(
	w http.ResponseWriter, sw *statWriter, rt *Runtime, model string, body []byte,
	clientModel, dispModel, acctModel string, statRow *statsEngine.ReqRow,
	clientUA string, dumpSeqNo int, stream bool,
) (res rotationResult) {
	res.done = true // 默认：既有 return 出口都已把响应写完（成功/请求级错误/诊断引导）
	// 上游报「参数被拒」时，客户端原始参数是唯一能定位「谁发的、发了什么」的依据
	// （改写后的出站参数由渠道层打印），首次 4xx 时一并落日志。
	clientParams := provider.LogParams(body)
	tried := map[string]bool{}
	var lastErr error
	var lastStatus int
	var lastBody []byte
	// lastKind：轮转中最后一个「账号级可轮换」错误的类别（Rotatable 分支置位）——
	// 候选耗尽透传前交回调用方做方案 B 判定；成功/请求级错误不置位（零值 ErrNone）。
	var lastKind provider.ErrKind
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.pickWithSticky(rt, model)
		if acct == nil {
			break
		}
		if tried[acct.UID] {
			// 粘性路由选回已尝试的账号，清除粘性记录后重试
			h.stickyClear(rt, model)
			acct = rt.Pool.PickExcluding(tried, model)
			if acct == nil {
				break
			}
		}
		tried[acct.UID] = true
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			log.Printf("refresh start platform=%s uid=%s reason=request", rt.Kind, acct.UID)
			// 单飞 + 内层重检：同一账号的并发请求只放一次刷新出去。这一步最关键 ——
			// 同一 refresh_token 被并发消费必然一方拿到 refresh_conflict，而失败方
			// 会被下面这个分支推进冷却（默认 10 分钟），表现为「一并发就 503」。
			if err := provider.RefreshOnce(acct, func() error {
				if !acct.NeedsRefresh(h.cfg.RefreshSkew) {
					return nil // 已被并发的另一次刷新刷过
				}
				if rerr := rt.Upstream.RefreshToken(acct); rerr != nil {
					return rerr
				}
				if serr := acct.SaveAtomic(); serr != nil {
					log.Printf("refresh save failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, serr)
				}
				return nil
			}); err != nil {
				log.Printf("refresh failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, err)
				lastErr = err
				h.stickyClear(rt, model)
				var ue *provider.Error
				if errors.As(err, &ue) && ue.Kind == provider.ErrSessionDead {
					rt.Pool.MarkSessionDead(acct.UID, "refresh: "+err.Error())
				} else {
					rt.Pool.Cooldown(acct.UID, pool.CoolErr, h.cfg.ErrCooldown, "refresh: "+err.Error())
				}
				continue
			}
			log.Printf("refresh success platform=%s uid=%s expires_at=%d", rt.Kind, acct.UID, acct.ExpiresAt)
		}
		rc, status, respBody, terr := rt.Upstream.ChatStream(acct, body)
		if terr != nil {
			lastErr = terr
			h.stickyClear(rt, model)
			if rt.SingleAccount {
				// 单账号渠道：不累计 errCount。网络抖动重试三次就冷却唯一账号，
				// 会让整条渠道下线（且无号可轮换）。直接透传错误，重试交给客户端。
				log.Printf("upstream transport error platform=%s uid=%s（单账号渠道不计错）err=%v",
					rt.Kind, acct.UID, terr)
				writeOpenAIError(w, http.StatusBadGateway, "upstream_error", terr.Error())
				return
			}
			rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			continue
		}
		if status >= 400 {
			h.stickyClear(rt, model)
			kind := rt.Upstream.Classify(status, string(respBody))
			log.Printf("client request platform=%s model=%s status=%d ua=%q params=%s",
				rt.Kind, clientModel, status, clientUA, clientParams)
			// WILDWORK_DUMP_DIR 开启时落盘「出站 body + 上游错误响应」对，默认 no-op。
			// 上游按出站 body 校验/计数（如 Kimi 的 position N），仅凭入站 dump 定位不了。
			// 保真度说明：部分渠道 client 在发送前还有渠道级改写（如 raccoon 删顶层
			// reasoning_effort），但均不触碰 messages——按消息定位类故障此形态即等价。
			dumpUpstreamFailure(dumpSeqNo, body, respBody, clientModel, status, rt.Kind.String(), acct.UID)
			// 单账号渠道：任何账号级惩罚都等于整条渠道下线，故一律原文透传、不罚账号。
			// 尤其在 429（唯一需要的背压）上：冷却后后续请求会在挑号阶段被挡成
			// no_healthy_account，反而不如透传 429 让客户端按 Retry-After 自行重试。
			if rt.SingleAccount {
				// 402（权益不足）例外：账号本身健康，同号其他模型照常可用，只冷却「账号×该模型」
				// ——下一次请求挑号阶段即「模型全冷」，自动触发降级链顶替；本次仍原文透传。
				if kind == provider.ErrHardCredit {
					rt.Pool.CooldownModel(acct.UID, model, pool.CoolHard, h.hardCooldown(), "402 权益不足")
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
				// 429/6004：上游声明「按模型限额」（「您也可以切换其他模型继续使用」）时
				// 只冷却该账号的该模型，同号其他模型继续参与选号；否则按账号级冷却
				//（保守选择，理由见 provider.IsModelScopedSoftRate 注释）。
				// 冷却时长按 6004 响应体的真实重置点解析（#82）：固定 60s 与上游「按重置点解封」
				// 不符，解析失败/非正回退 SoftCooldown。
				if provider.IsModelScopedSoftRate(string(respBody)) {
					rt.Pool.CooldownModel(acct.UID, model, pool.CoolSoft, h.softRateCooldown(string(respBody)), "429 rate limit")
					log.Printf("model cooldown platform=%s uid=%s model=%s", rt.Kind, acct.UID, model)
				} else {
					rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.softRateCooldown(string(respBody)), "429 rate limit")
					log.Printf("account cooldown platform=%s uid=%s model=%s", rt.Kind, acct.UID, model)
				}
			case provider.ErrSessionDead:
				// 先试一次 refresh：本地 token 可能只是在别处被轮换（不变量 19）。
				// 成功 ⇒ 解除本轮的「已尝试」标记并重试本号（单号渠道若不清标记，
				// pick 会直接空手而归 → 白变 503），且不施加任何标记。
				// 失败 ⇒ 上游已吊销会话（invalid_grant 之类），才落 SessionDead 标记
				// （R76：标记而非 Disable，与用户手动停用分离、重登后按 mtime 自动清除）。
				if h.recoverSession(rt, acct, respBody) {
					delete(tried, acct.UID)
					lastErr = fmt.Errorf("upstream %s (http %d): 已刷新 token，重试本号", kind, status)
					lastStatus = 0 // 不再透传这条已自愈的响应
					lastBody = nil
					continue
				}
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
				// 11102 该后端无此模型：兑现 ErrKind 注释承诺的「(账号,模型) 负缓存避让」。
				// 此前落到账号级软冷却 —— 一个模型不存在就把账号整体下线，与该语义不符。
				rt.Pool.CooldownModel(acct.UID, model, pool.CoolSoft, h.cfg.SoftCooldown, kind.String())
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
			case provider.ErrServer:
				if rt.NoCooldownOnServerError {
					// 该渠道声明 5xx 为上游网关抖动（账号本身健康），
					// 不计入账号错误，避免一夜抖动把所有账号冷却。
					log.Printf("upstream server error platform=%s uid=%s status=%d（不计入账号错误）",
						rt.Kind, acct.UID, status)
				} else {
					rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
				}
			default:
				rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			}
			// 账号级错误（限流/欠费/登录态死/5xx/账号故障等）：换下一个账号重试本请求，
			// 全部候选耗尽后才把最后一个上游错误透传给客户端（此前此处直接 return，
			// 导致客户端看到本可避免的 429/402）。内容类错误在上方 case 中已提前返回。
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
		rt.Pool.NoteSuccess(acct.UID)
		h.stickySuccess(rt, model)
		if stream {
			usage, serr := rt.Upstream.Stream(w, rc, dispModel)
			cu := cacheUsageOf(usage) // 缓存命中（上游未上报时 reported=false，面板显示「—」）
			// 记 token 流水（成功请求口径；usage 缺失记 0 token 仅计请求数）。
			// 记账失败不干扰主流程——锦上添花功能。
			if h.ledger != nil {
				pt, ct, src := usageTokens(usage)
				h.ledger.AppendUsage(ledger.UsageEntry{Ch: rt.Kind.String(), UID: acct.UID,
					Model: ledgerModelOf(acctModel), PT: pt, CT: ct, CR: cu.cached, CRC: cu.reported, Src: src})
			}
			if serr != nil && upstream.IsEmptyStreamError(serr) {
				// 上游 200 但无有效数据帧：HTTP 头已发出只能 200，客户端会收到我们补的
				// error 帧（部分客户端因此判定「模型不可用」），这里留痕便于对账。
				log.Printf("upstream 空流 platform=%s uid=%s model=%s（已下发 error 帧与 [DONE]）",
					rt.Kind, acct.UID, clientModel)
			}
			// 运行统计：模型维度记账（有 usage 才计，对齐 sidecar 的 u.found 语义）。
			// 吞吐仅在流式下有意义：genSec = 总耗时 − TTFB；引擎内对 ≤50ms 的样本只计 TTFB。
			if h.stats != nil {
				pt, ct, _ := usageTokens(usage)
				if pt > 0 || ct > 0 {
					gen := time.Since(sw.start).Seconds() - float64(sw.ttfbMs)/1000
					h.stats.AddUsage(acctModel, pt, ct, usageCredit(usage), sw.ttfbMs, gen, cu.cached, cu.reported, time.Now())
					statRow.Tok = ct
					if gen > 0 {
						statRow.TokPerSec = float64(ct) / gen
					}
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
							rt.Pool.CooldownModel(acct.UID, model, pool.CoolHard, h.hardCooldown(), "stream 402 权益不足")
							log.Printf("stream error model-cooldown platform=%s uid=%s model=%s kind=%s（单号渠道仅冷却该模型，原文透传）",
								rt.Kind, acct.UID, model, kind)
							break
						}
						rt.Pool.Cooldown(acct.UID, pool.CoolHard, h.hardCooldown(), "stream business error: "+serr.Error())
						log.Printf("stream error cooled platform=%s uid=%s kind=%s cooldown=%s",
							rt.Kind, acct.UID, kind, h.hardCooldown())
					case provider.ErrSessionDead:
						// 登录态失效：标记 SessionDead（挑号跳过 + 面板徽标 + 一键重登），不再混入停用（#134）。
						// 单号渠道也因此不会被「整条渠道下线」——重新登录成功后 clearReloggedLocked 自动解除；
						// 客户端已通过 error 帧收到上游原文（如「登录已失效，请重新登录」），可操作性由客户端承担。
						rt.Pool.MarkSessionDead(acct.UID, serr.Error())
						log.Printf("stream session dead platform=%s uid=%s（已下发 error 帧，err=%v）", rt.Kind, acct.UID, serr)
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
				rt.Pool.CooldownModel(acct.UID, model, pool.CoolHard, h.hardCooldown(), "402 权益不足")
				log.Printf("model cooldown (aggregate 402) platform=%s uid=%s model=%s cooldown=%s",
					rt.Kind, acct.UID, model, h.hardCooldown())
			}
			statRow.Status = http.StatusBadGateway
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			return
		}
		// 空回答兜底：思考型模型的 max_tokens 含思考预算，客户端给小预算时正文可能被
		// 思考吃光（finish_reason=length + content 空）。放大预算重跑一次，成功则替换结果。
		// 会多花一次额度，故只在四条判据同时命中时触发（见 emptyAnswerRetry）。
		if asked := requestedMaxTokens(body); asked > 0 {
			if bumped, need := emptyAnswerRetry(resp, asked); need {
				if retried, ok := h.retryEmptyAnswer(rt, acct, body, bumped, clientModel); ok {
					log.Printf("空回答放大重试成功 platform=%s uid=%s model=%s max_tokens=%d→%d",
						rt.Kind, acct.UID, clientModel, asked, bumped)
					resp = retried
				} else {
					log.Printf("空回答放大重试未改善 platform=%s uid=%s model=%s max_tokens=%d→%d（保留原结果）",
						rt.Kind, acct.UID, clientModel, asked, bumped)
				}
			}
		}
		if h.ledger != nil {
			u, _ := resp["usage"].(map[string]any)
			pt, ct, src := usageTokens(u)
			cu := cacheUsageOf(u)
			h.ledger.AppendUsage(ledger.UsageEntry{Ch: rt.Kind.String(), UID: acct.UID,
				Model: ledgerModelOf(acctModel), PT: pt, CT: ct, CR: cu.cached, CRC: cu.reported, Src: src})
		}
		writeJSON(w, http.StatusOK, resp)
		// 运行统计：必须在写回之后——此刻 statWriter 才记到首字节，TTFB 为真值；
		// 一次性写回 gen≈0 由引擎 50ms 守卫兜底（只计 TTFB 不计吞吐——非流式本无吞吐样本，
		// 若在写回前取值会把 ttfb=0/gen=端到端总时长喂进引擎，污染 AvgTTFB 与吞吐均值）。
		if h.stats != nil {
			u, _ := resp["usage"].(map[string]any)
			pt, ct, _ := usageTokens(u)
			if pt > 0 || ct > 0 {
				gen := time.Since(sw.start).Seconds() - float64(sw.ttfbMs)/1000
				cu := cacheUsageOf(u)
				h.stats.AddUsage(acctModel, pt, ct, usageCredit(u), sw.ttfbMs, gen, cu.cached, cu.reported, time.Now())
				statRow.Tok = ct
			}
			statRow.Credit = usageCredit(u)
		}
		return
	}
	// 候选全部耗尽：透传最后一个上游错误（而非包装成 503）。
	if lastStatus > 0 && lastBody != nil {
		// 候选耗尽：不在此透传——交回调用方判定（方案 B 积分耗尽时按降级链重入一次，
		// 否则原样透传；透传语义见上方注释）。
		res = rotationResult{done: false, lastKind: lastKind, lastStatus: lastStatus, lastBody: lastBody}
		return
	}

	// 渠道没有可用账号：区分「未绑定账号」与「全部冷却/禁用」，给出可操作的引导。
	// 客户端（如 Claude Code/Codex）只看到这条错误，必须足以让用户知道去面板做什么。
	sts := rt.Pool.List()
	var bound, disabled, cooling int
	var earliest time.Time // 全池对该模型最早的解冻时刻（账号级与模型级冷却取最小）
	nowTs := time.Now()
	for _, s := range sts {
		if s.Disabled {
			disabled++
		} else if s.Cooling {
			cooling++
		} else {
			bound++
		}
		if s.Cooling && s.Until.After(nowTs) && (earliest.IsZero() || s.Until.Before(earliest)) {
			earliest = s.Until
		}
		for _, mc := range s.ModelCooling {
			if mc.Until.After(nowTs) && (earliest.IsZero() || mc.Until.Before(earliest)) {
				earliest = mc.Until
			}
		}
	}
	if len(sts) == 0 {
		log.Printf("503 no_healthy_account platform=%s reason=no_account", rt.Kind)
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
	if bound == 0 {
		log.Printf("503 no_healthy_account platform=%s accounts=%d disabled=%d cooling=%d reason=all_unavailable",
			rt.Kind, len(sts), disabled, cooling)
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
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			fmt.Sprintf("渠道 %s 的 %d 个账号当前全部不可用（禁用 %d / 冷却 %d）：可在面板查看原因；%s",
				rt.Kind, len(sts), disabled, cooling, hint))
		return
	}
	msg := fmt.Sprintf("渠道 %s 暂无可用账号（共 %d 个：禁用 %d / 冷却 %d；其余余额耗尽或出错）",
		rt.Kind, len(sts), disabled, cooling)
	// 这一支最需要日志：调用方只看到 503，而账号并非禁用/冷却，原因全在 lastErr 里
	// （2026-09-23 验收时 405a1c 的瞬时 503 因无日志而查不到任何线索，只能靠复测反推）。
	if lastErr != nil {
		log.Printf("503 no_healthy_account platform=%s accounts=%d disabled=%d cooling=%d err=%v",
			rt.Kind, len(sts), disabled, cooling, lastErr)
		msg += ": " + lastErr.Error()
	} else {
		log.Printf("503 no_healthy_account platform=%s accounts=%d disabled=%d cooling=%d reason=no_remaining",
			rt.Kind, len(sts), disabled, cooling)
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
	return // 裸 return：携带命名结果 res（done=true——诊断引导已把响应写完）
}

// noLoginChannel 报告渠道是否「无登录概念」（不存在凭证文件、也无重登入口）。
// 用于把「去面板重新登录」这类引导文案限定在真正适用的渠道上，
// 避免免密渠道（oczen / kilo）被误导去找一个不存在的登录按钮。
func noLoginChannel(k provider.Kind) bool { return k == provider.Oczen || k == provider.Kilo }

// keyPasteChannel 报告渠道凭据是否「由用户从官方站点自行创建后粘贴进来」（而非登录编排取得）。
// 这类渠道的引导文案必须指向「粘贴新 Key」而不能说「重新登录」：
// cline 的官方 API Key 不过期、不轮换，被禁用只会是 Key 本身无效/被删。
func keyPasteChannel(k provider.Kind) bool { return k == provider.Cline }

func (h *Handler) runtimeForModel(model string) (*Runtime, string, error) {
	// 解析与 provider.PublicModelID 同源：既接受 `<渠道>/<上游ID>`，也接受上游 ID 自带命名空间的
	// 直通形态（`cline-free/*`）——后者首段不是渠道名，必须整串当上游模型名（决议 R59）。
	kind, upstream, ok := provider.SplitModelID(model)
	if !ok {
		return nil, "", fmt.Errorf("model must use explicit prefix: %s", h.prefixHint())
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

// prefixHint 列出当前已配置渠道的模型前缀，供「必须带前缀」的报错文案使用。
// 动态生成而非手写：手写列表会随渠道增加而滞后 —— 2026-09-23 发现它漏掉了
// workbuddyai / qodercom / raccoon / loomy 四个渠道（计划 §5 的触点清单项）。
func (h *Handler) prefixHint() string {
	kinds := make([]string, 0, len(h.cfg.Runtimes))
	for k := range h.cfg.Runtimes {
		kinds = append(kinds, string(k))
	}
	if len(kinds) == 0 {
		return "<channel>/<model>"
	}
	sort.Strings(kinds)
	for i, k := range kinds {
		kinds[i] = k + "/<model>"
	}
	return strings.Join(kinds, " / ")
}

// emptyAnswerRetry 判断「非流式聚合结果是否属于『预算被思考吃光』的空回答」。
//
// 思考型模型的 max_tokens 是**输出总预算**（含思考 token）。客户端给小预算时，
// 预算可能全部耗在思考上：正文为空、finish_reason=length。此时把预算放大重跑一次
// 通常就能拿到正文（参考 workbuddy-openai-proxy/src/openai.mjs:193 的同款兜底）。
//
// 判据（四条同时满足才算，避免误伤正常的空回答）：
//   - 正文为空且无 tool_calls（有工具调用就不是「被思考吃光」）
//   - finish_reason == "length" 或流里有帧（排除上游本身没吐任何东西的情况）
//   - 客户端确实给了 max_tokens，且值偏小（< 下限）
func emptyAnswerRetry(resp map[string]any, askedMax int64) (bumped int64, ok bool) {
	if askedMax <= 0 || askedMax >= emptyAnswerMinBudget {
		return 0, false
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		return 0, false
	}
	first, _ := choices[0].(map[string]any)
	if first == nil {
		return 0, false
	}
	errObj, _ := first["error"].(map[string]any)
	if errObj != nil {
		return 0, false // 上游在非流式响应里带了 error：那是错误而非空回答
	}
	msg, _ := first["message"].(map[string]any)
	if msg == nil {
		return 0, false
	}
	if s, _ := msg["content"].(string); s != "" {
		return 0, false // 有正文
	}
	if tcs, _ := msg["tool_calls"].([]any); len(tcs) > 0 {
		return 0, false // 有工具调用，不属于「预算被思考吃光」
	}
	// finish_reason 缺失也放行：部分渠道聚合时该字段可能为空，
	// 而「预算被吃光」的典型形态恰好是 length 被上游省略。
	if fr, _ := first["finish_reason"].(string); fr != "" && fr != "length" {
		return 0, false
	}
	return emptyAnswerBumped(askedMax), true
}

// emptyAnswerMinBudget 触发放大的预算下限：max_tokens 达到该值即认为客户端已给足预算。
// 1024 与参考实现一致（workbuddy-openai-proxy 的 Math.max(1024, asked*4)）。
const emptyAnswerMinBudget = 1024

// emptyAnswerBumped 计算放大后的预算：四倍与下限取大者，保证确实变大。
func emptyAnswerBumped(askedMax int64) int64 {
	if b := askedMax * 4; b > emptyAnswerMinBudget {
		return b
	}
	return emptyAnswerMinBudget
}

// requestedMaxTokens 取请求体里的输出预算（OpenAI 的 max_tokens 或新名 max_completion_tokens）。
// 无值/非法/非正数返回 0，表示「客户端未表达预算」——此时不做空回答重试
// （没有可比对的放大基准，且放大可能违背客户端本意）。
func requestedMaxTokens(body []byte) int64 {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return 0
	}
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		if v, ok := obj[k].(float64); ok && v > 0 {
			return int64(v)
		}
	}
	return 0
}

// retryEmptyAnswer 把 max_tokens 放大到 bumped 后重跑一次非流式请求。
// 仅在结果确实改善（拿到正文或工具调用）时返回 ok=true，否则调用方保留原结果。
// 复用同一账号：空回答与账号健康无关（换号也是同样结果）。
func (h *Handler) retryEmptyAnswer(rt *Runtime, acct *auth.Auth, body []byte, bumped int64, clientModel string) (map[string]any, bool) {
	bumpedBody, err := withMaxTokens(body, bumped)
	if err != nil {
		return nil, false
	}
	rc, status, _, terr := rt.Upstream.ChatStream(acct, bumpedBody)
	if terr != nil || status >= 400 {
		return nil, false
	}
	defer rc.Close()
	retried, err := rt.Upstream.Aggregate(rc, clientModel)
	if err != nil {
		return nil, false
	}
	// 只有真的拿到内容才算改善：再跑一次仍为空则保留原结果（避免用更差的响应替换）
	choices, _ := retried["choices"].([]any)
	if len(choices) == 0 {
		return nil, false
	}
	first, _ := choices[0].(map[string]any)
	if first == nil {
		return nil, false
	}
	msg, _ := first["message"].(map[string]any)
	if msg == nil {
		return nil, false
	}
	if s, _ := msg["content"].(string); s != "" {
		return retried, true
	}
	if tcs, _ := msg["tool_calls"].([]any); len(tcs) > 0 {
		return retried, true
	}
	return nil, false
}

// withMaxTokens 返回把输出预算改写为 bumped 的新 body。
// max_completion_tokens 一并删除，避免与 max_tokens 并存时上游取到旧值。
func withMaxTokens(body []byte, bumped int64) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	obj["max_tokens"] = float64(bumped)
	delete(obj, "max_completion_tokens")
	return json.Marshal(obj)
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
// （过期条目由 pool 侧剔除，此处按存在性判断）。
//
// 注意本函数对「整渠道全灭」（无任何健康账号）同样返回 false——
// 这正是降级链该顶上的两种形态（模型全冷 / 整渠道全灭），调用方不得
// 再额外要求「渠道内有健康账号」才降级（R54，PR #127 修正的旧判据）。
func modelHasCapacity(rt *Runtime, model string) bool {
	for _, s := range rt.Pool.List() {
		if s.Disabled || s.Cooling {
			continue
		}
		if modelCooling(s, model) {
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
// 第二返回值 primaryAvail = 「原模型此刻有可用账号」，与降级与否无关，只给恢复探针用：
// 旧实现「未降级就算恢复」会把两种情形谎报成已恢复——① 原模型仍全冷但备胎也没容量，
// ② 整渠道无健康账号。仅面板事件记账受影响，但谎报会令「持续时长」失真。
// 未配置规则时不做探测（守住「正常路由零开销」）：此时不会新增事件，
// 代价是删规则后残留的进行中事件不会自动关闭（环形 200 条自然淘汰，重启即清）。
func (h *Handler) pickModelFallback(rt *Runtime, model string) (string, bool) {
	h.fbMu.RLock()
	rules := h.fallbacks
	h.fbMu.RUnlock()
	if len(rules) == 0 {
		return "", false
	}
	if modelHasCapacity(rt, model) {
		return "", true // 未全冷：走原路由，并告知探针「容量已在」
	}
	// 未全冷之外（模型全冷 / 整渠道全灭）都继续往下遍历规则找备胎；
	// 链路里落在同渠道的备胎会被 modelHasCapacity 自然跳过。
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
			if modelHasCapacity(rt2, m2) {
				return to, false
			}
		}
	}
	return "", false
}

// prepareChatBody 改写发往上游的 Chat 请求体：
//  1. 归一化客户端的思考控制为顶层 reasoning_effort（见 internal/reasoning）；
//     取值非法或同一对象内自相矛盾时返回 reasoning.InvalidError；
//  2. 修补「带 tool_calls 的 assistant / tool 消息 content 为空」这种上游会整请求
//     拒绝的形态（见 normalizeToolTurnContent）；
//  3. 客户端未表达思考意图时注入渠道默认档（defaultEffort 为空则跳过）；
//  4. 覆盖 model 为路由后的真实模型名。
//
// 渠道层（internal/upstream 等）只负责把标准档位投影成自己协议的方言，
// 不重复做归一化，避免同一套规则散落多处。
func prepareChatBody(body []byte, model, defaultEffort string) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	normalized, control, err := reasoning.NormalizeChat(obj, false)
	if err != nil {
		return nil, err
	}
	normalizeToolTurnContent(normalized)
	if defaultEffort != "" && control.IsDefault() {
		normalized["reasoning_effort"] = defaultEffort
	}
	normalized["model"] = model
	return json.Marshal(normalized)
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
// 危害在于**中毒进历史**：WorkBuddy 客户端在「模型只回工具调用、没输出正文」
// 时会把该 assistant 消息的 content 落成 null，之后这一轮永远留在 messages 里，
// 于是该会话的每一发请求都被上游拒（重启客户端也不恢复，只能新建会话）。
// 客户端侧的 jsonl 记录可验证：第 1/2 轮工具正常，第 3 轮起 assistant 出现
// content=null，同一时刻其它渠道（如 qwen3.8-flash）正常。
//
// 放宽到 tool 角色一并处理：工具返回空内容时客户端同样可能给 null，
// 上游对工具轮是同一套校验。
//
// 2026-10-11 追加（traecode/SOLO 实测）：**普通 assistant 消息**（无 tool_calls）
// content 缺失同样会被整请求拒答，错误是 SOLO 流内 event:error 的
// `4001 the param is invalid`。成因是 CodeBuddy 客户端在「思考中途被打断」时，
// 会把半截思考存成 `{"role":"assistant","reasoning":"…"}`（无 content、无
// tool_calls）；此后该会话每轮重放都带上这条空壳，于是每发必拒（同「中毒进
// 历史」）。dump 0060/0066 重放对照坐实：原样 → 0.6s 即 4001；删掉该条或补
// content:"" → 均正常出流。故 assistant 一律补空串，不再以 tool_calls 为前提。
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

// MaxRequestBody 请求体上限（32MiB）：server 与 gateway 共用，避免两处硬编码不同步。
// 原为 8MiB，实测多模态大图 base64 后（体积 ×~1.37）很宐易超限（issue #30 反馈），提至 32MiB。
// 超限直接回 413 说真话，不做静默截断——截断后 JSON 解析失败会被误报成
// invalid_model（issue #30），比直接拒绝更误导排查。
const MaxRequestBody = 32 << 20

// ReadBodyLimited 读取请求体：超过 MaxRequestBody 时回 413 并返回错误。
// 用 LimitReader(max+1) 多读 1 字节以区分「恰好 max」与「超限」。
func ReadBodyLimited(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > MaxRequestBody {
		_ = r.Body.Close()
		return nil, errTooLarge
	}
	return raw, nil
}

// errTooLarge 超限哨兵错误（调用方据此回 413）。MiB 数字直接用常量推导，避免改上限后文案不同步。
var errTooLarge = fmt.Errorf("request body exceeds limit of %d bytes (%d MiB); please reduce request size", MaxRequestBody, MaxRequestBody>>20)

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": "api_error", "code": code}})
}

// transparentError 上游错误原文透传：status+body 原样写回，不包装。
func transparentError(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
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

// statWriter 记录首字节写出时刻（TTFB 口径：客户端等到的第一个响应字节，
// 含状态行/响应头——WriteHeader/Write/Flush 三者先到先记）。
type statWriter struct {
	http.ResponseWriter
	start   time.Time
	ttfbMs  int64
	written bool
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

// usageCredit 从 usage 对象取上游下发的单次积分（WorkBuddy 系会回；
// TraeWork 系 token_usage 帧无此字段 → 0，引擎侧按渠道余额差值分摊估算）。
func usageCredit(u map[string]any) float64 {
	if v, ok := u["credit"].(float64); ok {
		return v
	}
	return 0
}

// formatUnfreeze 把解冻时刻格式化为面板提示文案。
func formatUnfreeze(t time.Time) string {
	if t.IsZero() {
		return "稍后（请查看面板冷却原因）"
	}
	if time.Until(t) < 24*time.Hour {
		return t.Format("15:04")
	}
	return t.Format("2006-01-02 15:04")
}
