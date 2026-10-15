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
	"wild-work/internal/ledger"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/reasoning"
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

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux

	ledger *ledger.Ledger // 记账器（cfg.Ledger 透传，nil = 不记账）

	apiMu    sync.RWMutex // 保护 cfg.APIKey（面板可运行时修改）
	stickyMu sync.RWMutex
	sticky   map[string]*stickyEntry // runtimeKind → stickyEntry

	reasoningMu     sync.RWMutex // 保护 reasoningEffort（面板可运行时修改）
	reasoningEffort string       // 默认思考档，见 Config.ReasoningEffort
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
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), sticky: make(map[string]*stickyEntry), ledger: cfg.Ledger}
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
		rt := h.cfg.Runtimes[k]
		accounts[k.String()] = rt.Pool.List()
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
		rt := h.cfg.Runtimes[k]
		if rt.Pool == nil || len(rt.Pool.List()) == 0 { // 只暴露已接入账号的平台
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
func buildModelEntry(k provider.Kind, mi provider.ModelInfo) map[string]any {
	id := k.String() + "/" + mi.ID
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
	// （改写后的出站参数由渠道层打印），首次 4xx 时一并落日志。
	clientParams := provider.LogParams(body)
	clientUA := r.UserAgent()
	// 档位能力表预热（首次请求才触发目录拉取，见 ensureEffortCaps）
	h.ensureEffortCaps(rt)
	// WILDWORK_DUMP_DIR 开启时落盘原始 body（可直接重放），默认 no-op；
	// 返回的序号把本次请求的入站/出站/错误响应三份 dump 关联起来。
	dumpSeqNo := dumpChatBody(body, clientModel, "client")
	body, err = prepareChatBody(body, model, h.reasoningDefaultFor(rt.Kind))
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

	tried := map[string]bool{}
	var lastErr error
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
					rt.Pool.Disable(acct.UID, "refresh session dead")
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
				log.Printf("upstream error platform=%s uid=%s status=%d kind=%s（单账号渠道不罚账号，原文透传）",
					rt.Kind, acct.UID, status, kind)
				transparentError(w, status, respBody)
				return
			}
			switch kind {
			case provider.ErrHardCredit:
				// 积分耗尽（429+14018 / 402）必须账号级冷却：若误按模型级，
				// 同一账号的 N 个模型会各撞一次长硬冷却，白扔号更久。
				rt.Pool.Cooldown(acct.UID, pool.CoolHard, h.cfg.HardCooldown, "余额/权益不足")
			case provider.ErrSoftRate:
				// 上游按模型独立限流时（WorkBuddy 系 6004 原文声明「可换其他模型继续用」）
				// 只冷却该模型，账号上其他模型仍可路由；否则按账号级冷却（保守选择，
				// 理由见 provider.IsModelScopedSoftRate 注释）。
				if provider.IsModelScopedSoftRate(string(respBody)) {
					rt.Pool.CooldownModel(acct.UID, model, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
				} else {
					rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
				}
			case provider.ErrSessionDead:
				rt.Pool.Disable(acct.UID, "session dead")
			case provider.ErrNotFound:
				rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
			case provider.ErrContentBlocked, provider.ErrPromptTooLong,
				provider.ErrImageInvalid, provider.ErrBadParams, provider.ErrPassthrough:
				// 请求级错误（内容拦截 / 上下文超限 / 图片格式无效 / 出站 body 畸形 / 请求级拒绝）：
				// 不冷却不熔断不计错，直接透传原文回客户端。
				// 这些是请求内容或请求形态问题，与账号健康无关——同一 body 换任何账号结果都一样，
				// 轮转白费时间，NoteError 还会把健康账号喂到冷却。
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
				transparentError(w, status, respBody)
				return
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
			// 上游错误直接透传给客户端，不做包装
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(respBody)
			return
		}
		defer rc.Close()
		rt.Pool.NoteSuccess(acct.UID)
		h.stickySuccess(rt, model)
		if peek.Stream {
			usage, serr := rt.Upstream.Stream(w, rc, clientModel)
			// 记 token 流水（成功请求口径；usage 缺失记 0 token 仅计请求数）。
			// 记账失败不干扰主流程——锦上添花功能。
			if h.ledger != nil {
				pt, ct, src := usageTokens(usage)
				h.ledger.AppendUsage(ledger.UsageEntry{Ch: rt.Kind.String(), UID: acct.UID,
					Model: stripChannel(clientModel), PT: pt, CT: ct, Src: src})
			}
			if serr != nil && upstream.IsEmptyStreamError(serr) {
				// 上游 200 但无有效数据帧：HTTP 头已发出只能 200，客户端会收到我们补的
				// error 帧（部分客户端因此判定「模型不可用」），这里留痕便于对账。
				log.Printf("upstream 空流 platform=%s uid=%s model=%s（已下发 error 帧与 [DONE]）",
					rt.Kind, acct.UID, clientModel)
			} else if serr != nil {
				// 上游中断：已尽力透传，错误仅在日志可见
				log.Printf("stream relay end platform=%s uid=%s err=%v", rt.Kind, acct.UID, serr)
				// 流内业务错误（traework SOLO 的 event:error，如 3004 限流）：
				// HTTP 200 与响应头早已发出，无法在同一请求内换号重试；唯一补救是把
				// 中招账号冷却掉——下次请求挑号会自动换到别的账号（粘性路由见
				// pickWithSticky 对 status.Cooling 的检查，故无需显式 stickyClear）。
				var sec provider.StreamErrorClassifier
				if errors.As(serr, &sec) {
					kind := sec.Kind()
					switch kind {
					case provider.ErrSoftRate:
						rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "stream business error: "+serr.Error())
						log.Printf("stream error cooled platform=%s uid=%s kind=%s cooldown=%s",
							rt.Kind, acct.UID, kind, h.cfg.SoftCooldown)
					case provider.ErrHardCredit:
						rt.Pool.Cooldown(acct.UID, pool.CoolHard, h.cfg.HardCooldown, "stream business error: "+serr.Error())
						log.Printf("stream error cooled platform=%s uid=%s kind=%s cooldown=%s",
							rt.Kind, acct.UID, kind, h.cfg.HardCooldown)
					case provider.ErrSessionDead:
						rt.Pool.Disable(acct.UID, "stream business error: session dead")
						log.Printf("stream error disabled platform=%s uid=%s", rt.Kind, acct.UID)
					default:
						// 其余流内错误（如请求级拒绝）不罚账号，只记日志。
						log.Printf("stream error ignored platform=%s uid=%s kind=%s",
							rt.Kind, acct.UID, kind)
					}
				}
			}
			return
		}
		resp, err := rt.Upstream.Aggregate(rc, clientModel)
		if err != nil {
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
			h.ledger.AppendUsage(ledger.UsageEntry{Ch: rt.Kind.String(), UID: acct.UID,
				Model: stripChannel(clientModel), PT: pt, CT: ct, Src: src})
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	// 渠道没有可用账号：区分「未绑定账号」与「全部冷却/禁用」，给出可操作的引导。
	// 客户端（如 Claude Code/Codex）只看到这条错误，必须足以让用户知道去面板做什么。
	sts := rt.Pool.List()
	var bound, disabled, cooling int
	for _, s := range sts {
		if s.Disabled {
			disabled++
		} else if s.Cooling {
			cooling++
		} else {
			bound++
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
}

// noLoginChannel 报告渠道是否「无登录概念」（不存在凭证文件、也无重登入口）。
// 用于把「去面板重新登录」这类引导文案限定在真正适用的渠道上，
// 避免匿名通道（oczen）被误导去找一个不存在的登录按钮。
func noLoginChannel(k provider.Kind) bool { return k == provider.Oczen }

func (h *Handler) runtimeForModel(model string) (*Runtime, string, error) {
	parts := strings.SplitN(strings.TrimSpace(model), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, "", fmt.Errorf("model must use explicit prefix: %s", h.prefixHint())
	}
	kind := provider.Kind(parts[0])
	rt := h.cfg.Runtimes[kind]
	if rt == nil || rt.Pool == nil || rt.Upstream == nil {
		return nil, "", fmt.Errorf("provider %q is not configured", kind)
	}
	if len(rt.Pool.List()) == 0 {
		return nil, "", fmt.Errorf("provider %q has no account", kind)
	}
	return rt, parts[1], nil
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

// stripChannel 去掉模型名的 channel/ 前缀（"workbuddy/glm-5.2" → "glm-5.2"），
// 供流水按渠道内裸模型名聚合；无前缀时原样返回。
func stripChannel(model string) string {
	if i := strings.IndexByte(model, '/'); i >= 0 {
		return model[i+1:]
	}
	return model
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
	out := make(map[provider.Kind][]provider.ModelInfo, len(h.cfg.Runtimes))
	for _, k := range h.runtimeKinds() {
		rt := h.cfg.Runtimes[k]
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
	out := make(map[provider.Kind][]provider.ModelInfo, len(h.cfg.Runtimes))
	for _, k := range h.runtimeKinds() {
		rt := h.cfg.Runtimes[k]
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
	for _, rt := range h.cfg.Runtimes {
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
	ks := make([]provider.Kind, 0, len(h.cfg.Runtimes))
	for k := range h.cfg.Runtimes {
		ks = append(ks, k)
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
