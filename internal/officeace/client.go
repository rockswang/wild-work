package officeace

import (
	"io"
	"net"
	"net/http"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// Client 是 OfficeAce（华为云果办模型网关）渠道客户端，实现 provider.Upstream。
//
// 上游本身就是 OpenAI 兼容协议，故 chat 走 {base}/chat/completions；但有三处改写：
//   - 恒以 stream:true 打上游（非流式由本地聚合）；
//   - 按模型补足 max_tokens 推理预算（推理模型 max_tokens 太小 → 正文空）；
//   - 剥离 glm-5.3 系不认的 thinking 参数。
//
// 控制面（积分/签到/续期）另见 quota.go / refresh.go。
type Client struct {
	HTTP       *http.Client // 带总超时，供非流式/控制面
	StreamHTTP *http.Client // 无总超时，供长 SSE 流式（R35）
}

// New 构造 officeace 客户端。
func New() *Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 90 * time.Second,
	}
	return &Client{
		HTTP:       &http.Client{Transport: tr, Timeout: 600 * time.Second},
		StreamHTTP: &http.Client{Transport: tr},
	}
}

// SetTransport 让 main 装配 devdump 包壳时统一替换 Transport。
func (c *Client) SetTransport(tr http.RoundTripper) {
	c.HTTP.Transport = tr
	c.StreamHTTP.Transport = tr
}

// ---------------------------------------------------------------------------
// provider.Upstream 必需接口实现
// ---------------------------------------------------------------------------

// RefreshToken 在 refresh.go 实现（批 3）；此处保留接口位。
func (c *Client) RefreshToken(a *auth.Auth) error { return c.refreshToken(a) }

// FetchModelPricing 返回空（officeace 无本地费率表；实际倍率见 quota 的 model-token-rates）。
func (c *Client) FetchModelPricing(_ *auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

// Classify 按华为网关错误码分类（见 classify.go）。
func (c *Client) Classify(status int, body string) provider.ErrKind {
	return classifyUpstream(status, body)
}

// ChatStream 出站对话请求（实现见 http.go）。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	return c.chatStream(a, body)
}

// Stream 透传并规范化上游 SSE 帧，回填 model 为客户端请求名。
// 同时嗅探流内 error 帧（HTTP 200 内嵌错误）——发现即返回实现 StreamErrorClassifier
// 的错误，供 handler 分类冷却账号（R40）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
	sn := &errorSniffer{src: r}
	err := upstream.StreamCapture(w, rewriteModelReader(sn, model), func(u map[string]any) { usage = u })
	if err != nil {
		return usage, err
	}
	if sn.payload != "" {
		return usage, &StreamError{Status: statusOfPayload(sn.payload), Body: sn.payload}
	}
	return usage, nil
}

// Aggregate 非流式聚合（上游恒 stream=true，故只处理 SSE）。
//
// 与 Stream 同款嗅探：上游同样可能在 HTTP 200 + 流内 error 帧返回业务错误，
// 而 upstream.Aggregate 会把该帧当成「无正文的成功响应」吞掉（实测 resp 有 choices
// 但 content 为空、err=nil）——客户端只看到空回答、账号也不会冷却（R40/R60）。
// 故在聚合器下方同样挂 errorSniffer，命中即返回带分类的错误（优先于聚合结果）。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	sn := &errorSniffer{src: r}
	resp, err := upstream.Aggregate(sn)
	if sn.payload != "" {
		return nil, &StreamError{Status: statusOfPayload(sn.payload), Body: sn.payload}
	}
	if err != nil {
		return nil, err
	}
	if model != "" {
		resp["model"] = model
	}
	return resp, nil
}

// FetchModels 拉取上游模型目录（{base}/models）；失败时回退各账号本地已知模型。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	return c.fetchModels(a)
}
