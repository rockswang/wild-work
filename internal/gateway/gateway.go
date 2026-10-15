// gateway.go 三接口兼容层：OpenAI Chat Completions / OpenAI Responses / Anthropic Messages。
//
// 设计要点（两层结构）：
//   - 内层（internal/server.Handler）保持原样，只提供 POST /v1/chat/completions；
//     账号池、粘性路由、冷却、token 刷新、渠道改写全部留在那里，本层不感知。
//   - 外层（本包）负责协议转换与模型名路由，通过 in-process 调用内层（见 pipe.go），
//     不经过本地 HTTP 自环，避免二次监听/鉴权/超时。
//
// 数据流：客户端原生请求 → 转 OpenAI Chat 请求 → 内层 → Chat 响应 → 转回客户端原生协议。
package gateway

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"wild-work/internal/reasoning"
)

// Config 兼容层配置。
type Config struct {
	// Inner 内层 handler（internal/server.Handler），必须非 nil。
	Inner http.Handler
	// APIKey 内层要求的 API Key；为空时内层不鉴权。
	APIKey string
	// Router 模型名路由规则。
	Router Router
	// MaxTokensCap 上游 max_tokens 上限（0 = 不限制）。
	// Anthropic 客户端的 max_tokens 常远大于上游接受值，需在转发前 clamp。
	MaxTokensCap int
	// ResponsesReasoningSummary Responses 思考摘要下发策略（auto / on / off）。
	// 空串等价于 auto。见 reasoning.ParseSummaryMode。
	ResponsesReasoningSummary string
}

// SystemOneHandler 是 systemone 端点的实际处理函数。
// 签名为 func(w, r, state, questions)。网关负责鉴权、解析 body 后调用。
// Handler 返回上游 JSON 响应体的状态码与原始字节供透传；错误表示内部故障（如网络错）。
type SystemOneHandler func(w http.ResponseWriter, r *http.Request, state string, questions map[string]any) (status int, body []byte, err error)

// Gateway 三接口兼容层。零值不可用，须经 New 构造。
type Gateway struct {
	inner        http.Handler
	apiKey       string
	apiKeySource func() string // 非 nil 时优先于 apiKey（面板热改 Key 后立即跟随）
	router       Router
	maxTokensCap int
	// summaryMode 见 Config.ResponsesReasoningSummary。
	summaryMode string
	sysone      SystemOneHandler // 非 nil 时注册 POST /v1/systemone
	mu          sync.RWMutex     // 保护 router/maxTokensCap/summaryMode 热更新
}

// New 构造兼容层。inner 为 nil 时返回 nil（调用方据此跳过兼容层，保持旧行为）。
func New(cfg Config) *Gateway {
	if cfg.Inner == nil {
		return nil
	}
	return &Gateway{
		inner:        cfg.Inner,
		apiKey:       cfg.APIKey,
		router:       cfg.Router,
		maxTokensCap: cfg.MaxTokensCap,
		summaryMode:  normalizeSummaryMode(cfg.ResponsesReasoningSummary),
	}
}

// normalizeSummaryMode 把策略字符串收敛为合法取值，未知值按 auto 处理
// （配置加载阶段已校验，此处只兜底运行时热更新传入的脏值）。
func normalizeSummaryMode(mode string) string {
	normalized, err := reasoning.ParseSummaryMode(mode)
	if err != nil {
		return reasoning.SummaryAuto
	}
	return normalized
}

// SetAPIKeySource 注入 API Key 实时读取函数（通常为 server.Handler.CurrentAPIKey）。
// 注入后无需在面板改 Key 时同步本层状态。
func (g *Gateway) SetAPIKeySource(fn func() string) {
	if g != nil {
		g.apiKeySource = fn
	}
}

// SetSystemOneHandler 注入 systemone 端点处理器（非 nil 时注册路由 /v1/systemone）。
func (g *Gateway) SetSystemOneHandler(h SystemOneHandler) {
	if g != nil {
		g.sysone = h
	}
}

// withRouter 在读锁内执行 fn（Router 指针语义，不可拷贝）。
func (g *Gateway) withRouter(fn func(rt *Router)) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	fn(&g.router)
}

// SetCompat 热更新路由配置（default_channel / max_tokens_cap / model_map /
// responses_reasoning_summary）。面板保存 compat 时调用；不刷新的话新映射要到
// 下次重启才生效。channels 用于校验映射目标的渠道前缀，调用方应传入当前已接入渠道。
func (g *Gateway) SetCompat(defaultChannel string, maxTokensCap int, modelMap map[string]string, channels []string, reasoningSummary string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.router = Router{
		Default:  defaultChannel,
		Map:      modelMap,
		Channels: SortChannels(channels),
	}
	g.maxTokensCap = maxTokensCap
	g.summaryMode = normalizeSummaryMode(reasoningSummary)
	g.mu.Unlock()
}

// summaryEnabled 判断本次请求是否下发思考摘要事件。
// payload 必须是客户端原始请求体（含 reasoning.summary / include 等字段）；
// 转换后的 Chat 请求体已把这些字段收敛掉，不可用于判定。
func (g *Gateway) summaryEnabled(payload map[string]any) bool {
	g.mu.RLock()
	mode := g.summaryMode
	g.mu.RUnlock()
	switch mode {
	case reasoning.SummaryOff:
		return false
	case reasoning.SummaryOn:
		return true
	default: // auto：客户端显式索要摘要才下发
		return reasoning.WantsSummary(payload)
	}
}

// key 返回当前生效的 API Key。
func (g *Gateway) key() string {
	if g.apiKeySource != nil {
		if k := g.apiKeySource(); k != "" {
			return k
		}
	}
	return g.apiKey
}

// Routes 在 mux 上注册三接口路由 + 可选的 SystemOne 端点。
//
// 说明：仅注册内层没有的路径。POST /v1/chat/completions 与 GET /v1/models 由内层直接服务，
// 无需本层介入，这样旧客户端的调用栈完全不变。
func (g *Gateway) Routes(mux *http.ServeMux) {
	if g == nil {
		return
	}
	mux.HandleFunc("POST /v1/responses", g.withAuth(g.handleResponses))
	mux.HandleFunc("POST /v1/messages", g.withAuth(g.handleAnthropicMessages))
	mux.HandleFunc("POST /v1/messages/count_tokens", g.withAuth(g.handleCountTokens))
	if g.sysone != nil {
		mux.HandleFunc("POST /v1/systemone", g.withAuth(g.handleSystemOne))
	}
}

// withAuth 校验客户端凭据：同时接受 Anthropic 风格的 x-api-key 与标准 Bearer。
// Key 为空（内层不鉴权）时直接放行。
func (g *Gateway) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := g.key()
		if key == "" {
			next(w, r)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if strings.TrimSpace(token) != key {
			if strings.TrimSpace(r.Header.Get("x-api-key")) != key {
				// 错误体形状按协议区分：Anthropic 客户端只认 {"type":"error",...}
				if strings.Contains(r.URL.Path, "/messages") {
					writeAnthropicError(w, http.StatusUnauthorized, "authentication_error",
						"invalid x-api-key or Authorization header")
				} else {
					writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key",
						"missing or invalid API key")
				}
				return
			}
		}
		next(w, r)
	}
}

// ---------------------------------------------------------------------------
// 共用工具
// ---------------------------------------------------------------------------

// chatRequest 是发往内层的请求体（OpenAI Chat Completions 子集 + 透传字段）。
// 用 map 而非结构体：上游渠道会读取各自私有字段（如 reasoning_effort/thinking），
// 结构体能承载的字段有限，map 可原样保真转发。
type chatRequest = map[string]any

// callChat 把 Chat 请求投递给内层，返回其响应（流式 raw body / 非流式已解析）。
func (g *Gateway) callChat(w http.ResponseWriter, r *http.Request, req chatRequest, stream bool) (*innerResult, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	res, err := g.call(r.Context(), "/v1/chat/completions", body, g.key())
	if err != nil {
		return nil, err
	}
	if v := res.Header.Get("X-Model-Fallback"); v != "" {
		w.Header().Set("X-Model-Fallback", v)
	}
	return res, nil
}

// readChatResponse 读取内层非流式响应，返回 (状态码, 原始 body)。
func readChatResponse(res *innerResult) (int, []byte, error) {
	raw, err := res.ReadAll()
	return res.Status, raw, err
}

// logf 统一日志前缀，便于在 data/app.log 中与内层日志区分。
func logf(format string, args ...any) {
	log.Printf("[compat] "+format, args...)
}

// nowUnix 便于测试注入（当前仅用于响应 created 字段）。
var nowUnix = func() int64 { return time.Now().Unix() }

// parseJSONBody 读取并解析请求体，失败时按协议形状回错误。
// anthropicShape=true 时错误体为 Anthropic 形状。
func parseJSONBody(w http.ResponseWriter, r *http.Request, anthropicShape bool) (map[string]any, bool) {
	raw, err := readLimited(r, maxInnerBody)
	if err != nil {
		status, code := http.StatusBadRequest, "invalid_request"
		if strings.Contains(err.Error(), "exceeds limit") {
			status, code = http.StatusRequestEntityTooLarge, "request_too_large"
		}
		if anthropicShape {
			writeAnthropicError(w, status, "invalid_request_error", err.Error())
		} else {
			writeOpenAIError(w, status, code, err.Error())
		}
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		if anthropicShape {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		} else {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
		}
		return nil, false
	}
	return obj, true
}

// resolveModel 解析模型名，失败时按协议形状回错误。
func (g *Gateway) resolveModel(w http.ResponseWriter, model string, anthropicShape bool) (string, bool) {
	var resolved string
	var rerr error
	g.withRouter(func(rt *Router) {
		resolved, rerr = rt.Resolve(model)
	})
	if rerr != nil {
		if anthropicShape {
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", rerr.Error())
		} else {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_model", rerr.Error())
		}
		return "", false
	}
	return resolved, true
}

// handleSystemOne 处理 POST /v1/systemone —— Jev 结构化决策端点。
// 请求体：{"state": "...", "questions": {...}} → 透传到 OpenCode Zen 上游。
// 网关负责 API key 鉴权与解析；model 固定为 jev-1.13-free（无需客户端传）。
func (g *Gateway) handleSystemOne(w http.ResponseWriter, r *http.Request) {
	body, ok := parseJSONBody(w, r, false)
	if !ok {
		return
	}
	state := asString(body["state"])
	if state == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing required field: state")
		return
	}
	questions, ok := body["questions"].(map[string]any)
	if !ok || len(questions) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing required field: questions")
		return
	}
	status, respBody, err := g.sysone(w, r, state, questions)
	if err != nil {
		logf("systemone upstream error: %v", err)
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
}
