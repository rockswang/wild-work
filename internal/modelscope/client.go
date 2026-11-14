package modelscope

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// Client 上游 HTTP 客户端，实现 provider.Upstream。
//
// 基址按 Kind 二选一（国内站 / 国际站，见 constants.go）；Base 可覆盖以便测试注入假上游。
type Client struct {
	// Kind 渠道标识：决定出站基址（两个 kind 是独立服务，token 不通用）。
	Kind provider.Kind
	// HTTP 非流式请求（对话非流式、目录、生图 submit/poll），带总超时。
	HTTP *http.Client
	// StreamHTTP 对话 SSE 专用：**不设** Client.Timeout（R35，长回答会在超时点被掐断
	// 且无终止帧）。与 HTTP 共用 Transport 复用连接池。
	StreamHTTP *http.Client
	// Base 上游基址覆盖（测试用）；空 = 按 Kind 取默认值。
	Base string
	// idle 流中空闲上限（拿到响应头之后才计时）；0 = 默认 90s。
	idle time.Duration
}

// New 构造客户端。kind 只接受 provider.ModelScope / provider.ModelScopeAI。
func New(kind provider.Kind) *Client {
	tr := newTransport()
	return &Client{
		Kind: kind,
		// 生产由 main 用 upstream.timeout_seconds 覆盖（与其它渠道同口径）。
		HTTP:       &http.Client{Timeout: requestTimeout, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr},
	}
}

// newTransport 出厂 Transport：禁 h2（SSE 走 h1.1 行为最可预测），响应头兜底。
func newTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     false,
		TLSHandshakeTimeout:   15 * time.Second,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
}

// base 返回生效基址（去尾斜杠）。
func (c *Client) base() string {
	if b := strings.TrimRight(strings.TrimSpace(c.Base), "/"); b != "" {
		return b
	}
	return BaseURL(c.Kind)
}

// newRequest 构造带鉴权的上游请求：基址按 kind 二选一，鉴权恒为 `Authorization: Bearer <token>`
// （票 02 的对话与票 05 的生图共用本方法，避免各写一份头构造）。
//
// ⚠️ 绝不在错误/日志里拼 token（零 token 落盘纪律）。
func (c *Client) newRequest(ctx context.Context, method, path string, a *auth.Auth, body []byte) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if a != nil {
		if tok := strings.TrimSpace(a.AccessToken); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	return req, nil
}

// FetchModelPricing 无费率端点（上游不声明价格）：空实现，不编造。
func (c *Client) FetchModelPricing(_ *auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// 「不适用」额度口径的空能力（D5）：上游无余额/积分端点
// ---------------------------------------------------------------------------

// RefreshToken 空实现：`ms-` token 无 refresh 端点（失效表现为 401，需人工换 token）。
func (c *Client) RefreshToken(_ *auth.Auth) error { return nil }

// UserResource 无余额概念 ⇒ 恒 0 且**不报错**（面板据 credits_na 显示「不适用」，
// 报错会被渲染成「刷新积分失败」）。
func (c *Client) UserResource(_ *auth.Auth) (int64, error) { return 0, nil }

// UserResourceDetail 同上：没有余额明细条目，也绝不给整池补假到期日（R107 的教训）。
func (c *Client) UserResourceDetail(_ *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}

// DailyCheckin 本渠道无签到活动（调度器以 CheckinMinutes: []int{} 关闭，R39/R150）。
func (c *Client) DailyCheckin(_ *auth.Auth) error { return nil }
