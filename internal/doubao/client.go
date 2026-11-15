package doubao

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// DefaultIdleTimeout 流中空闲看门狗（与其它渠道对齐，R58）。
const DefaultIdleTimeout = 90 * time.Second

// defaultHeaderTimeout 等响应头的上限。
//
// 豆包的对话是**缓冲式**的：首字节可能要等思考跑一会儿。取 120s 与其它渠道同量级，
// 不故意放宽（上游排队导致的慢不属于本渠道特性）。
const defaultHeaderTimeout = 120 * time.Second

// Client 豆包渠道客户端。
type Client struct {
	HTTP       *http.Client
	StreamHTTP *http.Client

	// baseURL 可覆盖的站点根（**仅测试注入**；生产恒为 BaseURL）。
	// 为什么需要：签到/额度/对话的端点都是绝对 URL，测试必须能指向 httptest，
	// 否则会打到真上游（违反 §0.1：绝不能用真账号/真上游做实验）。
	baseURL string
}

// New 出厂客户端。
func New() *Client {
	return &Client{
		HTTP:       &http.Client{Timeout: defaultHeaderTimeout, Transport: newTransport()},
		StreamHTTP: &http.Client{Transport: newTransport()},
		baseURL:    BaseURL,
	}
}

func newTransport() *http.Transport {
	return &http.Transport{
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: defaultHeaderTimeout,
	}
}

// ---------- 上游请求 ----------

// identityURL 给端点拼上桌面客户端身份参数。
//
// 每请求稳定的：device_id / fp / tea_uuid / web_id（按账号固定，见 DeviceParamsOf）；
// 每请求新的：web_tab_id。
//
// ⚠️ 三个设备字段的**形状**上游会校验（2026-11-15 实测：形状不对回
// `710020202 common invalid param`），故一律走 DeviceParamsOf 而不是随手拼。
func (c *Client) identityURL(path, cookie, deviceID, webID, fp string) (string, error) {
	return c.identityURLWith(path, identityQuery, cookie, deviceID, webID, fp)
}

// identityURLWith 用指定的身份参数集拼 URL（对话与签到用不同参数集，见 checkinQuery）。
func (c *Client) identityURLWith(path string, params map[string]string, cookie, deviceID, webID, fp string) (string, error) {
	base := c.baseURL
	if base == "" {
		base = BaseURL
	}
	u, err := url.Parse(base + path)
	if err != nil {
		return "", err
	}
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	q.Set("device_id", deviceID)
	q.Set("fp", fp)
	q.Set("tea_uuid", deviceID)
	if webID != "" {
		q.Set("web_id", webID)
	}
	q.Set("web_tab_id", newUUID())
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// do 发一次上游请求，带上桌面客户端形态的头族。
//
// 出站头刻意**极简**：只有 UA / Referer / Accept / Content-Type / agw-js-conv /
// last-event-id / x-flow-trace。参考实现证明这套足够，多带浏览器头（sec-ch-ua 等）
// 反而与桌面客户端身份自相矛盾。
func (c *Client) do(method, url, cookie string, body []byte) (*http.Response, []byte, error) {
	return c.doWithHeaders(method, url, cookie, body, nil)
}

// doWithHeaders 带额外头的请求（焚化路径要 csrf 头）。
func (c *Client) doWithHeaders(method, url, cookie string, body []byte, extra map[string]string) (*http.Response, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Referer", BaseURL+"/")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("agw-js-conv", "str, str")
	req.Header.Set("last-event-id", "undefined")
	// B3 trace 头：参考实现用它免掉 a_bogus 签名（详见包注释的选型说明）。
	req.Header.Set("x-flow-trace", "04-0000000000000000000000000000000-0000000000000000-01")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if rerr != nil {
		return resp, nil, rerr
	}
	return resp, raw, nil
}

// devOf 从凭据里取 Cookie 与一整套**合法形状**的设备指纹。
//
// 老账号（登录时还没生成这三个字段）会按 Cookie 稳定派生，保证每次请求同一套。
func devOf(a *auth.Auth) (cookie, deviceID, webID, fp string) {
	cookie, deviceID, webID, fp = "", "", "", ""
	if a != nil && a.Extra != nil {
		cookie = a.Extra[ExtraCookie]
		deviceID = a.Extra[ExtraDeviceID]
		webID = a.Extra[ExtraWebID]
		fp = a.Extra[ExtraFP]
	}
	deviceID, webID, fp = DeviceParamsOf(cookie, deviceID, webID, fp)
	return cookie, deviceID, webID, fp
}

// ---------- provider.Upstream ----------

// RefreshToken 上游无 refresh 端点（Cookie 有效期由服务端定）。
//
// 返回 ErrSessionDead 让上层走「标记可自愈 + 面板引导重登」的既有路径
// （R76），**不** Disable（重新扫码即可恢复）。
func (c *Client) RefreshToken(a *auth.Auth) error {
	return &provider.Error{
		Kind: provider.ErrSessionDead, Status: 0,
		Msg: "doubao：上游无 refresh 端点（Cookie 失效后请重新扫码登录）",
	}
}

// chatStreamBody 上游响应体包装：携带本次请求的缓存键与工具门控标志，供
// Stream/Aggregate 捕获 SSE_ACK 时写回会话缓存、以及工具文本协议解析
//（避免改 provider.Upstream 接口签名）。
type chatStreamBody struct {
	io.ReadCloser
	sessionKey string
	hasTools   bool
}

// ChatStream 发一次对话请求，返回上游响应体（豆包私有 SSE）。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	cookie, dev, webID, fp := devOf(a)
	if cookie == "" {
		return nil, 0, nil, &provider.Error{Kind: provider.ErrSessionDead, Msg: "doubao：账号没有 Cookie，请重新扫码登录"}
	}
	// 客户端 OpenAI 请求体 → 上游 /chat/completion 请求体（**必须转换**，
	// 原样转发会被上游按 content_block 形状解析失败，见 outboundBody 的注释）。
	// 会话式复用：按 (账号 UID, 会话键) 查缓存，命中则只发最后一句（增量轮），
	// 未命中则建会话 + 历史压平（首轮）。UID 为空时退回无状态（理论不可达，防御性）。
	uid := ""
	if a != nil {
		uid = a.UID
	}
	out, err := outboundBodyFor(body, dev, uid, globalSessions)
	if err != nil {
		return nil, 0, nil, err
	}
	target, err := c.identityURLWith(ChatPath, chatQuery, cookie, dev, webID, fp)
	if err != nil {
		return nil, 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(out))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Referer", BaseURL+"/")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("agw-js-conv", "str, str")
	req.Header.Set("last-event-id", "undefined")
	req.Header.Set("x-flow-trace", "04-0000000000000000000000000000000-0000000000000000-01")
	req.Header.Set("Cookie", cookie)

	// 流式**不设**整体超时（不变量：R35），只有响应头上限与流中空闲看门狗兜底。
	resp, err := c.StreamHTTP.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, resp.StatusCode, raw, nil
	}
	// 成功响应才携带会话键：SSE_ACK 捕获的 conv id 只应绑定到真正建/续了会话的那次响应。
	// 键在此重算（与 outboundBodyFor 同一输入同一结果）：接口签名不能改，只能这样让
	// 键随响应体流过 Stream/Aggregate。复用轮也带上：豆包同会话多 section 会续发 ACK。
	key := sessionKeyOf(uid, conversationKeyOf(body))
	return &chatStreamBody{ReadCloser: resp.Body, sessionKey: key, hasTools: len(parseTools(body)) > 0}, resp.StatusCode, nil, nil
}

// deleteConversation 删除豆包侧会话（/samantha/thread/delete，焚化路径）。
//
// 协议（temp/doubao-tools-delete-research2.md B2）：body {"thread_id": conv_id}；
// query 身份参数照 kitsch/DoubaoProxy delete.go 全套（aid/version_code/samantha_web/
// 设备三件套/web_id）；头必带 Cookie + x-tt-passport-csrf-token（取 Cookie 里的
// passport_csrf_token，缺失用常量兑底——kitsch 未带头也成功过，但带总是更稳）。
// 成功 = HTTP 200 + {"code":0}。失败不重试（焚化是锦上添花）。
func (c *Client) deleteConversation(a *auth.Auth, convID string) error {
	cookie, dev, webID, fp := devOf(a)
	if cookie == "" {
		return fmt.Errorf("无 Cookie")
	}
	q := url.Values{}
	for k, v := range identityQuery {
		q.Set(k, v)
	}
	q.Set("device_id", dev)
	q.Set("fp", fp)
	q.Set("tea_uuid", dev)
	if webID != "" {
		q.Set("web_id", webID)
	}
	q.Set("web_tab_id", newUUID())
	target := c.baseURL + "/samantha/thread/delete?" + q.Encode()
	body, _ := json.Marshal(map[string]string{"thread_id": convID})
	extra := map[string]string{
		"Origin":                   BaseURL,
		"Referer":                  BaseURL + "/chat/" + convID,
		"x-tt-passport-csrf-token": csrfTokenOf(cookie),
	}
	resp, raw, err := c.doWithHeaders(http.MethodPost, target, cookie, body, extra)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateStr(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
	}
	if json.Unmarshal(raw, &env) == nil && env.Code != 0 {
		return fmt.Errorf("code=%d: %s", env.Code, truncateStr(string(raw), 120))
	}
	return nil
}

// csrfTokenOf 从 Cookie 里取 passport_csrf_token（缺失回退常量，参考实现两种都出现过）。
func csrfTokenOf(cookie string) string {
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "passport_csrf_token=") {
			return strings.TrimPrefix(part, "passport_csrf_token=")
		}
	}
	return ""
}

// Classify 归类上游错误。
//
// 顺序纪律（不变量 15）：**429 优先于业务码**——限流响应体高频带风控文案，
// 先判业务码会把限流误归成人机验证。
func (c *Client) Classify(status int, body string) provider.ErrKind {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return provider.ErrSessionDead
	case http.StatusTooManyRequests:
		return provider.ErrSoftRate
	}
	if code := riskCodeOf(body); code != 0 {
		switch code {
		case RiskCaptcha:
			// 人机验证：账号级、需人工，短冷却让位给用户操作窗口。
			return provider.ErrSoftRate
		case RiskRateLimited:
			return provider.ErrSoftRate
		case CheckinNoCampaign:
			return provider.ErrPassthrough
		}
	}
	if status >= 500 {
		return provider.ErrServer
	}
	return provider.ErrClient
}

// riskCodeOf 从响应体里挖风控/业务码（豆包把 code 放在 JSON 的 code 字段）。
func riskCodeOf(body string) int {
	var env struct {
		Code int `json:"code"`
	}
	if json.Unmarshal([]byte(body), &env) == nil && env.Code != 0 {
		return env.Code
	}
	// 兜底：文本里找 7100220xx 形态（风控码有时只出现在 message 里）。
	for _, marker := range []string{"710022002", "710022004", "710010702"} {
		if strings.Contains(body, marker) {
			var n int
			for _, ch := range marker {
				n = n*10 + int(ch-'0')
			}
			return n
		}
	}
	return 0
}

// ---------- 模型面 ----------

// FetchModels 静态目录（上游无可匿名拉取的目录端点，见包注释）。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	out := make([]provider.ModelInfo, 0, len(staticModels))
	for id, spec := range staticModels {
		out = append(out, provider.ModelInfo{
			ID:   id,
			Name: spec.Display,
			// 上下文窗口来自参考实现的静态记录，非上游声明、亦非本工具实测：
			// 三个来源标志全部不置，面板据此标注「未知」，不冒充容量（R49/R46）。
			ContextWindow: 0,
		})
	}
	return out, nil
}

// StaticModels 供 server.Runtime 做本地校验的静态表（R42）。
func StaticModels() []provider.ModelInfo {
	c := New()
	ms, _ := c.FetchModels(nil)
	return ms
}

// KnownModel 该公开模型名是否在静态表内。
func KnownModel(id string) bool {
	if id == DefaultModel {
		return true
	}
	_, ok := staticModels[id]
	return ok
}

// FetchModelPricing 上游无费率端点（额度是「窗口限额」不是积分计价），
// 返回空让面板显示「不适用」，**不编造**（R49）。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

// ---------- 转换出口 ----------

// Stream 私有 SSE → OpenAI Chat SSE（复用 internal/upstream 的规范化链路）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
	// 会话回写 + 工具门控：响应体若携带缓存键/hasTools（来自 ChatStream），
	// 捕获 SSE_ACK 时写回缓存、CHUNK_DELTA 进工具文本协议门控（tools.go）。
	var sessions *sessionStore
	var skey string
	var toolsOn bool
	if cb, ok := r.(*chatStreamBody); ok {
		sessions, skey, toolsOn = globalSessions, cb.sessionKey, cb.hasTools
	}
	err := upstream.StreamCapture(w, newDoubaoStreamWithSessions(r, model, skey, sessions, toolsOn), func(u map[string]any) { usage = u })
	return usage, err
}

// Aggregate 非流式出口：走同一条转换链路收敛。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	var sessions *sessionStore
	var skey string
	var toolsOn bool
	if cb, ok := r.(*chatStreamBody); ok {
		sessions, skey, toolsOn = globalSessions, cb.sessionKey, cb.hasTools
	}
	res, err := upstream.Aggregate(newDoubaoStreamWithSessions(r, model, skey, sessions, toolsOn))
	if res != nil {
		res["model"] = model
	}
	return res, err
}

// ---------- 小工具 ----------

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

var _ = fmt.Sprintf
