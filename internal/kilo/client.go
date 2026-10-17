// client.go Kilo AI 免费池客户端，实现 provider.Upstream。
//
// 与 oczen 同属「无账号渠道」（R16）：没有凭据文件、没有登录流程、面板里只有
// 一个虚拟账号（AnonymousUID），不可增删停用，无签到、无积分。差别全在上游协议：
// Kilo 是干净的 OpenAI 兼容面——不需要 oczen 那套会话头门禁与工具注入，
// 只要把对话体转给 /chat/completions 就行。
package kilo

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/idle"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// Client 上游 HTTP 客户端。Base 可覆盖以便测试注入假上游。
type Client struct {
	// HTTP 用于非流式请求（模型清单、凭据测试），带总超时。
	HTTP *http.Client
	// StreamHTTP 用于对话 SSE：**不设**整体 Timeout，避免长回答在超时点被掐断
	// 且无终止帧（R35 / issue #42）。卡死由 IdleTimeout 空闲看门狗兜底。
	StreamHTTP *http.Client
	// IdleTimeout 流中空闲上限。<=0 时回落到 idle.DefaultTimeout。
	IdleTimeout time.Duration
	// Base 上游基址；空 = DefaultBase。测试用 NewWithBase 注入。
	Base string
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
	return &Client{
		HTTP:        &http.Client{Timeout: requestTimeout, Transport: tr},
		StreamHTTP:  &http.Client{Transport: tr}, // 共用 Transport，不设 Timeout
		IdleTimeout: idle.DefaultTimeout,
	}
}

// NewWithBase 测试用：覆盖上游基址。
func NewWithBase(base string) *Client {
	c := New()
	c.Base = base
	return c
}

// base 返回当前出站基址。
func (c *Client) base() string {
	if c.Base != "" {
		return strings.TrimRight(c.Base, "/")
	}
	return DefaultBase
}

// idleTimeout 返回生效的空闲上限。
func (c *Client) idleTimeout() time.Duration {
	if c.IdleTimeout > 0 {
		return c.IdleTimeout
	}
	return idle.DefaultTimeout
}

// streamClient 返回对话用客户端：优先 StreamHTTP（无总超时）。
func (c *Client) streamClient() *http.Client {
	if c.StreamHTTP != nil {
		return c.StreamHTTP
	}
	return c.HTTP
}

// AnonymousAuth 构造匿名虚拟账号。唯一合法账号，由 main 装配时注入 pool。
// ExpiresAt 取远期值：让 NeedsRefresh 恒为假，避免走无意义的 token 刷新路径
// （本渠道没有 token 可刷，走进去只会换来一次无谓的冷却）。
func AnonymousAuth() *auth.Auth {
	return &auth.Auth{
		Kind:        string(Kind),
		AccessToken: "public", // 占位：上游免费池不校验凭据，但 auth.Auth 需要非空
		ExpiresAt:   noExpiry,
		UID:         AnonymousUID,
		Nickname:    AnonymousName,
	}
}

// StaticModels 上游清单拉取失败时的静态兜底（仍只含免费模型）。
func StaticModels() []provider.ModelInfo { return staticFree() }

// ---------------------------------------------------------------------------
// 请求构造
// ---------------------------------------------------------------------------

// headered 补齐出站公共头。免费池不看凭据，故只有内容协商与 UA。
func (c *Client) headered(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", userAgent)
}

// ChatStream 发起一次对话（流式）。
//
// 返回约定（与其它渠道一致，见 server/handler.go:638 的消费方式）：
//   - 传输层错误 → (nil, 0, nil, err)：单账号渠道由 handler 直接透传 502，不计错；
//   - HTTP >= 400 → (nil, status, 错误体, nil)：交给 Classify 分类后透传原文；
//   - 200 → (带空闲看门狗的响应体, 200, nil, nil)。
//
// 强制 stream=true：免费池按流式服务（参考实现同样恒开 stream）。客户端要
// 非流式时由 handler 用 Aggregate 在本地收敛，而不是让上游返回整包 JSON。
func (c *Client) ChatStream(_ *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, 0, nil, fmt.Errorf("kilo: invalid request body: %w", err)
	}
	if obj == nil {
		return nil, 0, nil, fmt.Errorf("kilo: empty request body")
	}
	obj["stream"] = true
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("kilo: marshal body: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.base()+"/chat/completions", bytes.NewReader(out))
	if err != nil {
		return nil, 0, nil, err
	}
	c.headered(req)

	// 用无总超时的 client：长思考/长回答不受整请求上限约束（R35）。
	resp, err := c.streamClient().Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		// 上游错误详情偶尔较长（含付费提示与审核原因），保留 1MB 上限原文透传。
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, resp.StatusCode, raw, nil
	}
	return provider.NewIdleReader(resp.Body, c.idleTimeout()), resp.StatusCode, nil, nil
}

// ---------------------------------------------------------------------------
// 模型清单与定价
// ---------------------------------------------------------------------------

// FetchModels 拉取模型清单并筛出免费池。
//
// 失败时**返回静态兜底且不报错**：清单拉取失败不该让整个渠道在面板上消失，
// 也不该被当成账号故障（本渠道没有账号）。这与 oczen 的处理一致。
func (c *Client) FetchModels(_ *auth.Auth) ([]provider.ModelInfo, error) {
	rows, err := c.fetchListing()
	if err != nil || len(rows) == 0 {
		return StaticModels(), nil
	}
	free := filterFree(rows)
	if len(free) == 0 {
		return StaticModels(), nil
	}
	return free, nil
}

// fetchListing 请求 GET /models 并解析 OpenRouter 风格清单。
func (c *Client) fetchListing() ([]listingRow, error) {
	ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/models", nil)
	if err != nil {
		return nil, err
	}
	c.headered(req)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("kilo: models http %d", resp.StatusCode)
	}
	var out struct {
		Data []listingRow `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// FetchModelPricing 免费池一律 0 倍率。Explicit=true 让面板显示「免费」
// 而不是「未知」——这里确实知道它是 0，因为它根本不在计费面上。
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

// ---------------------------------------------------------------------------
// 无账号渠道的空能力
// ---------------------------------------------------------------------------

// RefreshToken 空实现：免费池没有 token 可刷。不返回错误，避免上层把它
// 当作会话失效；虚拟账号 ExpiresAt 是远期值，正常路径走不到这里。
func (c *Client) RefreshToken(_ *auth.Auth) error { return nil }

// UserResource 免费池无积分概念，恒 0 且不报错（面板显示「不适用」）。
func (c *Client) UserResource(_ *auth.Auth) (int64, error) { return 0, nil }

// UserResourceDetail 同上：没有余额明细条目。
func (c *Client) UserResourceDetail(_ *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}

// DailyCheckin 免费池无签到活动。渠道在 main 里声明为 noExplicitCheckin，
// 调度器不会调用本方法；这里返回错误只是防呆（若被调用说明接线漏了）。
func (c *Client) DailyCheckin(_ *auth.Auth) error {
	return fmt.Errorf("kilo 免费池无签到活动")
}

// Classify 上游错误分类。
//
// 本渠道是单账号（且虚拟账号不可重登），handler 对 SingleAccount 渠道一律
// 原文透传、不做账号级惩罚，故这里的返回值主要用于**日志语义**与
// 「模型级冷却」那条例外（402）。分三类：
//   - 429 → ErrSoftRate：真限流；
//   - 5xx → ErrServer：上游故障；
//   - 其余 4xx → 付费 id 与内容审核归 ErrClient（请求方问题，换号无从规避），
//     其它一律 ErrPassthrough（形态/地域问题，原文透传）。
//
// 特别地**不得**归 ErrSessionDead：那会让 pool.Disable 永久禁用唯一账号，
// 而本渠道没有重新登录这条路。
func (c *Client) Classify(status int, body string) provider.ErrKind {
	switch {
	case status == http.StatusTooManyRequests:
		return provider.ErrSoftRate
	case status >= 500:
		return provider.ErrServer
	case status >= 400:
		low := strings.ToLower(body)
		if strings.Contains(low, "paid_model_auth_required") ||
			strings.Contains(low, "moderation") ||
			strings.Contains(low, "flagged") ||
			strings.Contains(low, "filtered") {
			return provider.ErrClient
		}
		return provider.ErrPassthrough
	default:
		return provider.ErrNone
	}
}

// ---------------------------------------------------------------------------
// 流式透传与本地聚合
// ---------------------------------------------------------------------------

// Stream 透传上游 SSE，并把 model 字段回填成客户端请求的原始模型名（R14）。
// 返回值为末帧捕获的 usage（供记账；上游未返回时为 nil）。
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

// modelRewriter 逐行把 SSE 帧里的 "model":"<上游裸名>" 换成客户端模型名。
//
// 上游帧经 internal/upstream 的白名单规范化重建，`model` 字段的序列化形态
// 固定为紧凑的 `"model":"xxx"`（无空格），所以按字节替换即可；**按行**处理
// 是关键——不能把整条流攒起来再替换，否则流式就退化成一次性响应。
type modelRewriter struct {
	src     *bufio.Reader
	pending []byte // 已读入但尚未吐出的当前行
	model   string
}

func rewriteModelReader(r io.Reader, model string) io.Reader {
	if model == "" {
		return r
	}
	return &modelRewriter{src: bufio.NewReaderSize(r, 64*1024), model: model}
}

// Read 一次吐一行（含尾换行），保持 SSE 帧边界完整。
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

// rewriteModelLine 替换单行内所有 `"model":"…"` 的值。
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
		// 已替换段不会再次命中 needle（模型名里不含 `"model":"`），从替换点之后继续扫。
		repl := make([]byte, 0, len(out)-(end-start)+len(model))
		repl = append(repl, out[:start]...)
		repl = append(repl, model...)
		repl = append(repl, out[end:]...)
		out = repl
		from = start + len(model)
	}
}
