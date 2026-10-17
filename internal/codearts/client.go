// client.go 华为云 CodeArts Agent 渠道客户端，实现 provider.Upstream。
//
// 渠道形态：**正常多账号渠道**（AK/SK + SecurityToken，由 PKCE OAuth 登录或
// refresh_token 静默续期得到），因此**不置 SingleAccount**：账号级错误按正常分类
// 走池内轮换（R24/R29），额度用尽由 ErrHardCredit 表达长冷却。
//
// 与其它渠道最不一样的三点（协议 §2.2/§2.3/§2.4）：
//   - 出站请求靠 **SDK-HMAC-SHA256 签名**（不是 Bearer），且 `maas_type` 必须参与签名、
//     `Agent-Type` 必须不参与——两者写反都是 401 APIG.0301；
//   - 出站体恒 stream:true，且 `max_tokens` 有实测上限（>65536 触发空流）；
//   - 错误既可能挂在非 2xx，也可能是 **HTTP 200 + 流内 `error_code`**，
//     两条路径共用 classify.go 的一套判据。
package codearts

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// Client 上游 HTTP 客户端。Base / BenefitBase / STSURL 可覆盖以便测试。
type Client struct {
	// HTTP 带总超时，供控制面（目录 / 积分 / 活动 / 续期）。
	HTTP *http.Client
	// StreamHTTP 专供对话 SSE：**无总超时**（R35），
	// 卡死由首 token / chunk 两段空闲看门狗兜底（见 sse.go 的 phaseReader）。
	StreamHTTP *http.Client
	// Base 对话与控制面基址（默认 APIBase）。
	Base string
	// BenefitBase benefit 模型目录基址（默认 BenefitBase 常量）。
	BenefitBase string
	// STSURL 凭据换取 / 续期端点（默认 STSTokenURL）。
	STSURL string
	// FirstTokenTimeout / ChunkTimeout 两段空闲看门狗阈值；
	// 0 = 用默认值（可由环境变量 DSH_CODEARTS_SSE_FIRST_TOKEN_TIMEOUT_MS /
	// DSH_CODEARTS_SSE_CHUNK_TIMEOUT_MS 覆盖，协议 §2.4）。
	FirstTokenTimeout time.Duration
	ChunkTimeout      time.Duration

	catMu sync.RWMutex // 见 models.go（动态目录 + benefit 集合的进程内缓存）
	cat   *catalogCache
}

// 编译期断言：本包对外的能力契约（Lead 的接线依赖 provider.Upstream 全部 10 个方法；
// 签到另实现 CheckinReporter/CheckinGranter，调度器优先用它们拿到结构化状态）。
var (
	_ provider.Upstream        = (*Client)(nil)
	_ provider.CheckinReporter = (*Client)(nil)
	_ provider.CheckinGranter  = (*Client)(nil)
)

// New 生产默认客户端。
func New() *Client {
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// StreamHTTP 无总超时后**必须**有 ResponseHeaderTimeout，否则连响应头都等不到
		// 就会无限挂住（与 minimax/kilo 同款理由）。
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &Client{
		HTTP:        &http.Client{Transport: tr, Timeout: requestTimeout},
		StreamHTTP:  &http.Client{Transport: tr}, // 共用 Transport，不设 Timeout
		Base:        APIBase,
		BenefitBase: BenefitBase,
		STSURL:      STSTokenURL,
	}
}

// NewWithBase 测试用：覆盖对话/控制面基址（长驻 URL 不变，需要时另设字段）。
func NewWithBase(base string) *Client {
	c := New()
	c.Base = base
	return c
}

// SetTransport 让 main 装配 devdump / 备用通道包壳时统一替换 Transport
// （两个 client 必须一起换，否则流式路径绕过包装）。
func (c *Client) SetTransport(tr http.RoundTripper) {
	c.HTTP.Transport = tr
	c.StreamHTTP.Transport = tr
}

// base 返回生效的对话/控制面基址。
func (c *Client) base() string {
	if s := trimSlash(c.Base); s != "" {
		return s
	}
	return APIBase
}

// benefitBase 返回生效的 benefit 目录基址。
func (c *Client) benefitBase() string {
	if s := trimSlash(c.BenefitBase); s != "" {
		return s
	}
	return BenefitBase
}

// stsURL 返回生效的 STS 端点。
func (c *Client) stsURL() string {
	if s := strings.TrimSpace(c.STSURL); s != "" {
		return s
	}
	return STSTokenURL
}

// httClient 返回非流式客户端（nil 安全回落）。
func (c *Client) httClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: requestTimeout}
}

// streamClient 返回流式客户端（无总超时；nil 安全回落）。
func (c *Client) streamClient() *http.Client {
	if c.StreamHTTP != nil {
		return c.StreamHTTP
	}
	return &http.Client{Transport: New().StreamHTTP.Transport}
}

// chatURL 对话端点。
func (c *Client) chatURL() string { return c.base() + EpChatCompletions }

// ---------------------------------------------------------------------------
// provider.Upstream：对话
// ---------------------------------------------------------------------------

// ChatStream 发一次对话请求（恒流式）。
//
// 返回约定（与其它渠道一致，handler 依赖它分类与透传）：
//   - 传输错误                     → (nil, 0, nil, err)
//   - 本地出站体不可构造 / 缺凭据   → (nil, 4xx, 本地错误体, nil)
//   - HTTP ≥ 400                   → (nil, status, 上游原始响应体, nil)  ← 原文透传
//   - 200                          → (带两段空闲看门狗的 body, 200, nil, nil)
//
// `InferHub.4004.200`（该账号无 benefit 包）按协议「去掉 maas_type 头重试**一次**」处理：
// 仅针对**非 2xx**响应（流内出现时 HTTP 200 与响应头早已发出，无法重发，见 sse.go 注释）。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	ac := accountFromAuth(a)
	if ac.AccessKeyID == "" || ac.SecretAccessKey == "" {
		// 本地 401：Classify 归 ErrSessionDead（凭据不完整 = 会话需重登），
		// 消息里点明缺什么，便于用户在面板上定位。
		return nil, http.StatusUnauthorized, localErrorBody("codearts：凭据不完整（缺 access_key_id / secret_access_key），请重新登录"),
			nil
	}
	model := modelOf(body)
	sess := newSessionIDs()
	out, berr := buildChatBody(body, model, sess.sessionID)
	if berr != nil {
		return nil, http.StatusBadRequest, badParamsBody(berr), nil
	}
	benefit := c.isBenefitModel(model)

	rc, status, raw, err := c.doChat(ac, out, sess, benefit)
	if err != nil {
		return nil, 0, nil, err
	}
	if status >= 400 && benefit && reNoBenefitPackage.MatchString(string(raw)) {
		// 该账号没有 benefit 包：去掉 maas_type 重试一次（协议 §2.4 表末行）。
		rc2, status2, raw2, err2 := c.doChat(ac, out, sess, false)
		if err2 != nil {
			return nil, 0, nil, err2
		}
		return rc2, status2, raw2, nil
	}
	return rc, status, raw, nil
}

// doChat 发一次对话请求。benefit=true 时 maas_type 参与签名并随请求发出。
func (c *Client) doChat(ac *Account, body []byte, sess sessionIDs, benefit bool) (io.ReadCloser, int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, c.chatURL(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	var signedExtra map[string]string
	if benefit {
		signedExtra = map[string]string{HeaderMaaSType: MaaSTypeBenefit}
	}
	if err := signAndApply(req, ac, body, "application/json", signedExtra, time.Now()); err != nil {
		return nil, 0, nil, err
	}
	// 以下头**必须**在签名之后追加（协议 §2.2：放进 canonical 就是 401 APIG.0301）。
	req.Header.Set("User-Agent", userAgent)
	applyUnsigned(req, sess.headers())

	resp, err := c.streamClient().Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, resp.StatusCode, raw, nil
	}
	return c.watch(resp.Body), resp.StatusCode, nil, nil
}

// userAgent 出站 UA。上游不校验指纹，这里如实标识本工具，便于上游侧排查异常流量。
const userAgent = "WildWork/1.0 (codearts)"

// ---------------------------------------------------------------------------
// provider.Upstream：不需要上游交互的能力
// ---------------------------------------------------------------------------

// FetchModelPricing CodeArts 没有本地费率表：benefit 模型的额度是按日免费额度
// （不是倍率），积分账户的计量单位是 package credit，两者都不构成「模型倍率」。
// 返回 nil 而不是编一份 0 倍率表——面板显示「未知」好过显示「免费」（R46/R49 同向）。
func (c *Client) FetchModelPricing(_ *auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// randomHex 生成 n 字节随机数的 hex（合 2n 个字符）。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// 随机源失败时退到时间戳（仅影响 id/缓存键的唯一性，不影响安全性）。
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// UUID4 生成 UUIDv4（x-snap-traceid、授权页 ticket_id 等"只要求唯一"的场景）。
func UUID4() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return randomHex(16)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// modelOf 取请求体里的 model。
func modelOf(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	return strings.TrimSpace(probe.Model)
}

// badParamsBody 本地出站体拒绝的错误体（OpenAI 风格）。
// code=bad_params 让上层 Classify 归请求级错误：不罚号、不轮换、原文透传。
func badParamsBody(err error) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": "codearts：" + err.Error(),
		"type":    "invalid_request_error",
		"code":    "bad_params",
	}})
	return b
}

// localErrorBody 构造本地错误体（缺凭据等本地前置失败）。
func localErrorBody(msg string) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": msg,
		"type":    "authentication_error",
		"code":    "session_dead",
	}})
	return b
}

// envDurationMS 读毫秒级环境变量覆盖（非法值忽略，回落 def）。
func envDurationMS(name string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ms <= 0 {
		return def
	}
	return time.Duration(ms) * time.Millisecond
}
