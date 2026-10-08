// client.go OpenCodeZen 匿名免费通道客户端，实现 provider.Upstream。
//
// 与其它渠道最大的不同：本渠道「无账号」。匿名凭证是常量 "public"，
// 面板里只有一个虚拟账号（AnonymousUID），不可增删停用、无签到、无积分。
// 所有请求都按 OpenCode 官方 CLI 的形态构造（见 constants.go 的三道校验）。
package oczen

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/idle"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// Client 上游 HTTP 客户端。Base 可覆盖以便测试。
type Client struct {
	HTTP *http.Client
	// StreamHTTP 用于对话流（SSE）：不设整体 Timeout，避免长回答在超时点被掐断
	// 且无终止帧（R35 / issue #42）。
	StreamHTTP *http.Client
	// IdleTimeout 聊天 SSE 流中空闲超时（StreamHTTP 无总时长上限，靠本字段兜底
	// 「上游中途卡死」）。<=0 表示禁用（测试用）。
	IdleTimeout time.Duration
	Base       string

	// mu 保护 apikey 的并发读写（面板热更新与请求路径并发）。
	mu sync.RWMutex
	// apiKey 自定义 API key；空 = 匿名凭证（AnonymousKey）。
	// 由面板设置（config.Proxies 同屏的 oczen_api_key）热更新。
	apiKey string
}

// New 生产默认客户端。
func New() *Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{HTTP: &http.Client{Timeout: requestTimeout, Transport: tr}, Base: DefaultBase,
		StreamHTTP:  &http.Client{Transport: tr}, // 共用 Transport，不设 Timeout
		IdleTimeout: idle.DefaultTimeout,          // 流中空闲监控兜底（见 internal/idle）
	}
}

// NewWithBase 测试用：覆盖上游基址。
func NewWithBase(base string) *Client {
	c := New()
	c.Base = base
	return c
}

func (c *Client) base() string {
	if c.Base == "" {
		return DefaultBase
	}
	return c.Base
}

// AnonymousAuth 构造匿名虚拟账号。唯一合法账号，由 main 装配时注入 pool。
// ExpiresAt 取远期值：使 NeedsRefresh 恒为假，避免走无意义的 token 刷新路径。
func AnonymousAuth() *auth.Auth {
	return &auth.Auth{
		Kind:        string(Kind),
		AccessToken: AnonymousKey,
		ExpiresAt:   noExpiry,
		UID:         AnonymousUID,
		Nickname:    AnonymousName,
	}
}

// StaticModels 上游不可达时的静态兜底清单（仍只含免费模型）。
func StaticModels() []provider.ModelInfo { return staticFree() }

// ---------------------------------------------------------------------------
// 请求构造
// ---------------------------------------------------------------------------

// canonicalSessionID 把任意会话种子映射成 OpenCode 官方格式
// ses_<12位小写hex><14位Base62>。已是该格式则原样返回（保住 prompt cache 亲和）。
// 上游自 2026-09-16 起对非该形状的会话头一律回 403 FreeTierError。
func canonicalSessionID(signal string) string {
	// 官方形状：ses_ + 12 位小写 hex（时间戳）+ 14 位 Base62
	if len(signal) == 4+12+14 && strings.HasPrefix(signal, "ses_") {
		ok := true
		for i := 4; i < 16; i++ {
			c := signal[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				ok = false
				break
			}
		}
		if ok {
			for i := 16; i < len(signal); i++ {
				if !isBase62(signal[i]) {
					ok = false
					break
				}
			}
			if ok {
				return signal
			}
		}
	}
	sum := sha256.Sum256([]byte("ses\x00" + signal))
	timePart := hex.EncodeToString(sum[:6]) // 12 位小写 hex
	n := new(big.Int).SetBytes(sum[6:16])
	return "ses_" + timePart + base62Fixed(n, 14)
}

func isBase62(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func base62Fixed(n *big.Int, width int) string {
	base := big.NewInt(62)
	out := make([]byte, width)
	rem := new(big.Int)
	for i := width - 1; i >= 0; i-- {
		n.DivMod(n, base, rem)
		out[i] = base62Alphabet[rem.Int64()]
	}
	return string(out)
}

func randomID(prefix string, size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return prefix + "_" + hex.EncodeToString([]byte(time.Now().String()))
	}
	return prefix + "_" + hex.EncodeToString(buf)
}

// conversationSeed 取首个 user 消息的 content 作为会话种子：
// 多轮对话中历史不断增长，取首轮可保证同一对话映射到同一会话（prompt cache 亲和），
// 不同对话则自然分开。
func conversationSeed(obj map[string]any) string {
	msgs, _ := obj["messages"].([]any)
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok || m["role"] != "user" {
			continue
		}
		if b, err := json.Marshal(m["content"]); err == nil && len(b) > 0 && string(b) != "null" {
			return string(b)
		}
	}
	return ""
}

// stubTool 免费档要求的桩工具（bash / read）。参数与描述不参与校验，
// 但给出可辨识的名字以便模型/客户端区分。
func stubTool(name string) map[string]any {
	switch name {
	case "bash":
		return map[string]any{"type": "function", "function": map[string]any{
			"name": "bash", "description": "(internal placeholder — do not call)",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{
				"command": map[string]any{"type": "string"}}}}}
	default:
		return map[string]any{"type": "function", "function": map[string]any{
			"name": "read", "description": "(internal placeholder — do not call)",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{
				"filePath": map[string]any{"type": "string"}}}}}
	}
}

// ensureAgentShape 就地改写请求体以通过免费档校验：
//   - 强制 stream=true（上游免费档只接受流式；非流式客户端由 Aggregate 本地聚合）；
//   - tools 内补齐缺失的 bash/read 桩工具（客户端没带 tools 时同时置 tool_choice=none，
//     避免模型真的去调桩工具；客户端自带工具时保留其 tool_choice，否则会破坏工具调用）。
//
// 返回被强制改过的标记仅供测试断言。
func ensureAgentShape(obj map[string]any) {
	obj["stream"] = true

	tools, _ := obj["tools"].([]any)
	have := map[string]bool{}
	for _, raw := range tools {
		t, ok := raw.(map[string]any)
		if !ok || t["type"] != "function" {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		if n, ok := fn["name"].(string); ok {
			have[n] = true
		}
	}
	hadTools := len(tools) > 0
	for _, name := range stubToolNames {
		if !have[name] {
			tools = append(tools, stubTool(name))
		}
	}
	obj["tools"] = tools
	if !hadTools {
		// 纯聊天：客户端没有工具语义，桩工具仅供上游过检，明确禁止调用。
		obj["tool_choice"] = "none"
	}
}

// authKey 返回当前出站凭证：自定义 key 优先，空则回匿名。
func (c *Client) authKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.apiKey != "" {
		return c.apiKey
	}
	return AnonymousKey
}

// SetAPIKey 热更新自定义 API key（空串 = 回匿名凭证）。
func (c *Client) SetAPIKey(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.apiKey = strings.TrimSpace(key)
}

// headered 为上游请求注入 OpenCode CLI 伪装头（含规范会话头）。
func (c *Client) headered(req *http.Request, session string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Authorization", "Bearer "+c.authKey())
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-session", session)
	// OpenCode 1.18.x 的亲和头族，缺一会降低被识别为官方客户端的概率
	req.Header.Set("x-session-affinity", session)
	req.Header.Set("X-Session-Id", session)
	req.Header.Set("x-opencode-request", randomID("req", 16))
	req.Header.Set("x-opencode-project", randomID("prj", 12))
}

// ---------------------------------------------------------------------------
// provider.Upstream 实现
// ---------------------------------------------------------------------------

// ChatStream 改写客户端请求体后转发到 Zen。上游恒为流式，非流式客户端由 Aggregate 收敛。
func (c *Client) ChatStream(_ *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, 0, nil, fmt.Errorf("oczen: invalid request body: %w", err)
	}
	ensureAgentShape(obj)
	// 会话标识由对话首轮稳定派生；缺 seed 时随机（单次会话，无亲和需求）。
	seed := conversationSeed(obj)
	if seed == "" {
		seed = randomID("fallback", 16)
	}
	session := canonicalSessionID(seed)
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("oczen: marshal body: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.base()+"/chat/completions", bytes.NewReader(out))
	if err != nil {
		return nil, 0, nil, err
	}
	c.headered(req, session)
	req, cancel := idle.WithCancel(req)
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		cancel()
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		cancel()
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, resp.StatusCode, raw, nil
	}
	// 成功分支：cancel 所有权交给 idle.Monitor（其 Close 会 cancel；静默超时也会 cancel）。
	return idle.Monitor(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

// TestKey 用 big-pickle 模型发一次最小对话验证凭证可用性。
// 返回 (status, 上游原文片段, error)。判定口径（面板展示用）：
//   - 200：key 有效且配额正常；
//   - 429：key 有效但触发限流（匿名共享配额或临时背压）——也算「连通」；
//   - 401/403：key 无效或被风控；402：付费模型余额不足（key 本身有效，但免费模型不该出现此错）；
//   - 503：上游端点故障（key 无法判定，连接是通的）。
func (c *Client) TestKey() (int, string, error) {
	obj := map[string]any{
		"model":      "big-pickle",
		"stream":     true,
		"max_tokens": 8,
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
	}
	ensureAgentShape(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequest(http.MethodPost, c.base()+"/chat/completions", bytes.NewReader(out))
	if err != nil {
		return 0, "", err
	}
	c.headered(req, canonicalSessionID(randomID("testkey", 8)))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	return resp.StatusCode, string(raw), nil
}

// FetchModels 拉取上游模型列表并只保留免费集合。
// 上游不可达时不报错、回静态兜底清单：匿名渠道无账号可轮换，
// 报错只会让面板显示空白，不如给出已知可用清单。
func (c *Client) FetchModels(_ *auth.Auth) ([]provider.ModelInfo, error) {
	ids, err := c.fetchModelIDs()
	if err != nil || len(ids) == 0 {
		return StaticModels(), nil
	}
	free := filterFree(ids)
	if len(free) == 0 {
		return StaticModels(), nil
	}
	return free, nil
}

// fetchModelIDs 请求 GET /models（同样需要伪装头）。
func (c *Client) fetchModelIDs() ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, c.base()+"/models", nil)
	if err != nil {
		return nil, err
	}
	c.headered(req, canonicalSessionID(randomID("catalog", 8)))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("oczen: models http %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		ids = append(ids, d.ID)
	}
	return ids, nil
}

// FetchModelPricing 免费模型一律 0 倍率（Explicit=true 让面板显示「免费」而非 unknown）。
func (c *Client) FetchModelPricing(_ *auth.Auth) ([]provider.ModelPricing, error) {
	models, _ := c.FetchModels(nil)
	yes := true
	out := make([]provider.ModelPricing, 0, len(models))
	for _, m := range models {
		out = append(out, provider.ModelPricing{
			Model:    m.ID,
			Channel:  string(Kind),
			Rate:     0,
			Explicit: &yes,
		})
	}
	return out, nil
}

// RefreshToken 空实现：匿名通道无 token 可刷。
// 不返回错误以免上层把它当作会话失效；虚拟账号 ExpiresAt 为远期值，
// 正常路径不会走到这里。
func (c *Client) RefreshToken(_ *auth.Auth) error { return nil }

// UserResource 匿名通道无积分概念，恒为 0 且不报错（面板显示「不适用」）。
func (c *Client) UserResource(_ *auth.Auth) (int64, error) { return 0, nil }

// UserResourceDetail 同上：无明细条目。
func (c *Client) UserResourceDetail(_ *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}

// DailyCheckin 匿名通道无签到活动（渠道声明为 noExplicitCheckin，调度器不会调用）。
func (c *Client) DailyCheckin(_ *auth.Auth) error {
	return fmt.Errorf("opencodezen 匿名通道无签到活动")
}

// Classify 上游错误分类。本渠道是单账号（且不可人工恢复），分类原则是
// 「绝不因请求级问题惩罚账号」，否则唯一账号一冷却就等于整条渠道下线：
//   - 429 → ErrSoftRate：真限流，短冷却 —— 单账号渠道唯一需要的背压；
//   - 其它 4xx（含 401/402/403 的 FreeTierError/RegionError）→ ErrPassthrough：
//     原文透传、不计错不冷却 —— 这些是请求形态/地域限制，换账号无从规避；
//     尤其不得归 ErrSessionDead：那会 pool.Disable 永久禁用，而匿名账号不可重登；
//   - 5xx → ErrServer（server.Runtime 已置 NoCooldownOnServerError，同样不罚账号）。
func (c *Client) Classify(status int, body string) provider.ErrKind {
	switch {
	case status == http.StatusTooManyRequests:
		return provider.ErrSoftRate
	case status >= 500:
		return provider.ErrServer
	case status >= 400:
		return provider.ErrPassthrough
	default:
		return provider.ErrNone
	}
}

// SystemOne 调用 jev 决策模型端点（POST /v1/systemone）。
// 透传 state+questions，自动注入 model、规范 criteria 并附加伪装头。
// 返回上游原始 JSON 响应体（非流式）；status>=400 时 body 为错误详情。
func (c *Client) SystemOne(state string, questions map[string]any) (int, []byte, error) {
	body := map[string]any{
		"model":     "jev-1.13-free",
		"state":     state,
		"questions": questions,
	}
	out, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("oczen systemone marshal: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.base()+"/systemone", bytes.NewReader(out))
	if err != nil {
		return 0, nil, err
	}
	c.headered(req, canonicalSessionID(randomID("sysone", 8)))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// Stream 透传上游 SSE，并把 model 字段回填成客户端请求的原始模型名（R14）。
// 返回值为末帧捕获的 usage（供记账，上游未返回时为 nil）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
	err := upstream.StreamCapture(w, rewriteModelReader(r, model), func(u map[string]any) { usage = u })
	return usage, err
}

// Aggregate 把上游 SSE 收敛成单个 OpenAI 响应，同样回填客户端模型名。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	resp, err := upstream.Aggregate(r)
	if err != nil {
		return nil, err
	}
	if model != "" {
		resp["model"] = model
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// model 字段回填
// ---------------------------------------------------------------------------

// rewriteModelReader 逐行把 SSE 帧里的 "model":"<上游裸名>" 替换成客户端模型名。
// 上游帧经规范化白名单重建，model 字段格式固定为 `"model":"xxx"`（紧凑无空格），
// 故按字节替换即可；按行处理保证流式不被缓冲（每行一到即吐，不攒整包）。
type modelRewriter struct {
	src     *bufio.Reader
	pending []byte // 已读入但尚未吐出的当前行缓冲
	model   string
}

func rewriteModelReader(r io.Reader, model string) io.Reader {
	if model == "" {
		return r
	}
	return &modelRewriter{src: bufio.NewReaderSize(r, 64*1024), model: model}
}

// Read 以「行」为单位吐数据：一次吐一行（含尾部换行），保持 SSE 帧边界完整。
func (m *modelRewriter) Read(p []byte) (int, error) {
	for len(m.pending) == 0 {
		line, err := m.src.ReadBytes('\n')
		if len(line) > 0 {
			m.pending = rewriteModelLine(line, m.model)
		}
		if err != nil {
			if len(m.pending) == 0 {
				return 0, err
			}
			break
		}
	}
	n := copy(p, m.pending)
	m.pending = m.pending[n:]
	return n, nil
}

// rewriteModelLine 替换单行内所有 "model":"..." 的值。
func rewriteModelLine(line []byte, model string) []byte {
	needle := []byte(`"model":"`)
	out := line
	from := 0
	for {
		i := bytes.Index(out[from:], needle)
		if i < 0 {
			return out
		}
		start := from + i + len(needle)
		end := bytes.IndexByte(out[start:], '"')
		if end < 0 {
			return out
		}
		end += start
		// 已替换段不会再次命中 needle（模型名不含 `"model":"`），从替换点之后继续扫描
		repl := make([]byte, 0, len(out)-(end-start)+len(model))
		repl = append(repl, out[:start]...)
		repl = append(repl, model...)
		repl = append(repl, out[end:]...)
		out = repl
		from = start + len(model)
	}
}
