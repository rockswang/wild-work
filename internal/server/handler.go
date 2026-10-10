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
	h := &Handler{cfg: cfg, mux: http.NewServeMux(), sticky: make(map[string]*stickyEntry), ledger: cfg.Ledger, stats: cfg.Stats}
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

// stickyKey 粘性路由 key（按渠道独立）
func (h *Handler) stickyKey(kind provider.Kind) string { return kind.String() }

// pickWithSticky 粘性路由选择账号。
// 优先使用上次成功路由的账号，直到：
//   - 账号进入冷却/禁用状态，或对该请求模型处于模型级冷却（429/6004 按模型限额）
//   - 连续成功请求达到 maxReqs 次（默认 50），自动轮换
//
// 任一条件触发则降级为 PickForModel(model) 选新账号并重置粘性记录。
func (h *Handler) pickWithSticky(rt *Runtime, model string) *auth.Auth {
	const defaultMaxReqs = 50

	h.stickyMu.RLock()
	sticky := h.sticky[h.stickyKey(rt.Kind)]
	h.stickyMu.RUnlock()

	// 尝试粘性路由
	if sticky != nil && sticky.uid != "" && sticky.reqCount < sticky.maxReqs {
		acct := rt.Pool.AuthByUID(sticky.uid)
		if acct != nil {
			status, ok := rt.Pool.Status(sticky.uid)
			modelCooled := ok && status.ModelCooling != nil && status.ModelCooling[model].After(time.Now())
			if ok && !status.Cooling && !status.Disabled && !modelCooled {
				log.Printf("sticky route platform=%s uid=%s count=%d/%d",
					rt.Kind, sticky.uid, sticky.reqCount, sticky.maxReqs)
				return acct
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
	h.sticky[h.stickyKey(rt.Kind)] = &stickyEntry{uid: acct.UID, maxReqs: defaultMaxReqs}
	h.stickyMu.Unlock()
	log.Printf("new sticky route platform=%s uid=%s maxReqs=%d", rt.Kind, acct.UID, defaultMaxReqs)
	return acct
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
	if mi.ContextWindow > 0 {
		entry["context_length"] = mi.ContextWindow
	}
	if mi.MaxTokens > 0 {
		entry["max_output_tokens"] = mi.MaxTokens
	}
	entry["architecture"] = map[string]any{
		"input_modalities": mi.InputModalities(),
		"modality":         mi.Modality(),
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
	acct := rt.Pool.Pick()
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
	rt, model, err := h.runtimeForModel(peek.Model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_model", err.Error())
		return
	}
	clientModel := peek.Model // 客户端请求的原始模型名（含 channel/ 前缀），回填进响应
	body, err = rewriteModel(body, model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// 运行统计插桩：每客户端请求一行（最终结果；中间换号尝试不逐行——对齐 sidecar 口径）。
	// TTFB 由 statWriter 记首字节，耗时/吞吐在出口由 defer 收口；
	// Status 未显式赋值 = 成功写回（200）。
	var statRow statsEngine.ReqRow
	statRow.Model = clientModel
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

	tried := map[string]bool{}
	var lastErr error
	// 最后一次上游错误的原始 status+body：候选耗尽时原样透传（而非包装成 503），
	// 客户端才能看到 6004 重置时间等上游细节。内容类错误在循环内已提前透传，不受影响。
	var lastStatus int
	var lastBody []byte
	for i := 0; i < h.cfg.MaxRotate; i++ {
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
				var ue *provider.Error
				if errors.As(err, &ue) && ue.Kind == provider.ErrSessionDead {
					rt.Pool.Disable(acct.UID, "refresh session dead")
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
			if rt.SingleAccount {
				// 单账号渠道：不累计 errCount。网络抖动重试三次就冷却唯一账号，
				// 会让整条渠道下线（且无号可轮换）。直接透传错误，重试交给客户端。
				log.Printf("upstream transport error platform=%s uid=%s（单账号渠道不计错）err=%v",
					rt.Kind, acct.UID, terr)
				statRow.Status = http.StatusBadGateway
				writeOpenAIError(w, http.StatusBadGateway, "upstream_error", terr.Error())
				return
			}
			rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			continue
		}
		if status >= 400 {
			statRow.Status = status // 所有透传出口（单账号/请求级/惩罚后）共用
			h.stickyClear(rt)
			kind := rt.Upstream.Classify(status, string(respBody))
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
				rt.Pool.Cooldown(acct.UID, pool.CoolHard, h.cfg.HardCooldown, "余额/权益不足")
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
				rt.Pool.Disable(acct.UID, "session dead")
			case provider.ErrNotFound:
				rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
			case provider.ErrContentBlocked, provider.ErrPromptTooLong, provider.ErrPassthrough:
				// 内容拦截/上下文超限/请求级拒绝：不冷却不熔断不计错，直接透传原文回客户端。
				// 这些是请求内容或请求形态问题，与账号健康无关，轮转白费时间且浪费好号配额。
				transparentError(w, status, respBody)
				return
			case provider.ErrWafBlock, provider.ErrAccountFault, provider.ErrModelBlocked:
				// 账号级风控/故障/模型不存在：软冷却，不累计错误计数。
				rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, kind.String())
				if kind != provider.ErrAccountFault {
					transparentError(w, status, respBody)
					return
				}
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
			// 全部候选耗尽后才把最后一个上游错误透传给客户端（-p4 前此处直接 return，
			// 导致客户端看到本可避免的 429/402）。内容类错误在上方 case 中已提前返回。
			if kind.Rotatable() {
				log.Printf("rotate platform=%s uid=%s kind=%s status=%d body=%s",
					rt.Kind, acct.UID, kind, status, truncateBody(respBody))
				lastErr = fmt.Errorf("upstream %s (http %d): %s", kind, status, truncateBody(respBody))
				lastStatus = status
				lastBody = respBody
				continue
			}
			transparentError(w, status, respBody)
			return
		}
		defer rc.Close()
		rt.Pool.NoteSuccess(acct.UID)
		h.stickySuccess(rt)
		if peek.Stream {
			usage, serr := rt.Upstream.Stream(w, rc, clientModel)
			// 记 token 流水（成功请求口径；usage 缺失记 0 token 仅计请求数）。
			// 记账失败不干扰主流程——锦上添花功能。
			if h.ledger != nil {
				pt, ct, src := usageTokens(usage)
				h.ledger.AppendUsage(ledger.UsageEntry{Ch: rt.Kind.String(), UID: acct.UID,
					Model: stripChannel(clientModel), PT: pt, CT: ct, Src: src})
			}
			// 运行统计：模型维度记账（有 usage 才计，对齐 sidecar 的 u.found 语义）。
			// 吞吐仅在流式下有意义：genSec = 总耗时 − TTFB；引擎内对 ≤50ms 的样本只计 TTFB。
			if h.stats != nil {
				pt, ct, _ := usageTokens(usage)
				if pt > 0 || ct > 0 {
					gen := time.Since(sw.start).Seconds() - float64(sw.ttfbMs)/1000
					h.stats.AddUsage(clientModel, pt, ct, usageCredit(usage), sw.ttfbMs, gen, time.Now())
					statRow.Tok = ct
					if gen > 0 {
						statRow.TokPerSec = float64(ct) / gen
					}
				}
				statRow.Credit = usageCredit(usage)
			}
			if serr != nil {
				// 上游中断/空流已尽力透传，错误仅在日志可见
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
						if len(rt.Pool.List()) <= 1 && !rt.SingleAccount {
							// 单号渠道：余额/权益不足透传原文，客户端/用户直接看到真实原因（如充值），
							// 而不是被 503 掩盖成「渠道不可用」。
							log.Printf("stream error passthrough platform=%s uid=%s kind=%s（单号渠道不罚账号）",
								rt.Kind, acct.UID, kind)
							break
						}
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
			statRow.Status = http.StatusBadGateway
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			return
		}
		if h.ledger != nil {
			u, _ := resp["usage"].(map[string]any)
			pt, ct, src := usageTokens(u)
			h.ledger.AppendUsage(ledger.UsageEntry{Ch: rt.Kind.String(), UID: acct.UID,
				Model: stripChannel(clientModel), PT: pt, CT: ct, Src: src})
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
				h.stats.AddUsage(clientModel, pt, ct, usageCredit(u), sw.ttfbMs, gen, time.Now())
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
		transparentError(w, lastStatus, lastBody)
		return
	}
	statRow.Status = http.StatusServiceUnavailable
	sts := rt.Pool.List()
	now := time.Now()
	var bound, disabled, cooling, modelAvail int
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
	if bound > 0 && modelAvail == 0 {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			fmt.Sprintf("渠道 %s 的 %d 个账号均健康，但模型 %s 已在全部账号上限流冷却：最早约 %s 解冻（到点自动恢复，无需重新登录），也可先切换其他模型",
				rt.Kind, bound, model, formatUnfreeze(earliest)))
		return
	}
	if bound == 0 {
		// 匿名渠道没有「重新登录」这个概念，给出针对性提示（否则会误导用户去找登录入口）。
		hint := fmt.Sprintf("%s 账号需重新登录（日志会有 refresh token is invalid）", rt.Kind)
		if noLoginChannel(rt.Kind) {
			hint = "该渠道无需登录，稍后重试即可（若持续失败请查看日志）"
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
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
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
// 避免匿名通道（oczen）被误导去找一个不存在的登录按钮。
func noLoginChannel(k provider.Kind) bool { return k == provider.Oczen }

func (h *Handler) runtimeForModel(model string) (*Runtime, string, error) {
	parts := strings.SplitN(strings.TrimSpace(model), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, "", fmt.Errorf("model must use explicit prefix: workbuddy/<model> / workbuddyai/<model> / traework/<model> / qoder/<model> / qodercn/<model> / qodercom/<model> / qwenwork/<model> / glm/<model> / oczen/<model>")
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

// usageCredit 从 usage 对象取上游下发的单次积分（WorkBuddy 系会回；
// TraeWork 系 token_usage 帧无此字段 → 0，引擎侧按渠道余额差值分摊估算）。
func usageCredit(u map[string]any) float64 {
	if v, ok := u["credit"].(float64); ok {
		return v
	}
	return 0
}

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

// normalizeToolTurnContent 把「content 为 null 或缺失」的工具轮消息补成空串。
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
func normalizeToolTurnContent(obj map[string]any) {
	msgs, _ := obj["messages"].([]any)
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "assistant":
			// 只在真的带 tool_calls 时补：普通 assistant 消息 content=null 无证据会出问题。
			if tc, ok := m["tool_calls"].([]any); !ok || len(tc) == 0 {
				continue
			}
		case "tool":
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

// softRateResetMaxCap 解析出的重置时长上限：上游时间戳异常（如错给一年后）时不至于把号冻死。
const softRateResetMaxCap = 24 * time.Hour

// softRateCooldown 从限流响应体提取真实重置时间，冷却到该时刻（而非固定 60s）。
// 命中形态：WorkBuddy 6004「...将在 2026-09-23 14:47:19 UTC+8 重置...」——该类限流
// 按重置点解封，60s 软冷却过期后账号会被反复选中反复 429。解析失败/时长为非正值时
// 回退默认 SoftCooldown。时间戳按 UTC+8 墙钟解析，与到期字段口径一致（AGENTS §6.18）。
// 已知形态「2026-09-23 14:47:19」可能带「UTC+8」后缀，解析前剥掉。
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

// softRateResetTs 提取「将在 <ts> 重置」中的时间戳（UTC+8 墙钟）；未命中返回 false。
func softRateResetTs(body string) (time.Time, bool) {
	const marker = "将在 "
	i := strings.Index(body, marker)
	if i < 0 {
		return time.Time{}, false
	}
	rest := body[i+len(marker):]
	end := strings.Index(rest, " 重置")
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
