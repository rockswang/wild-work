// client.go 智谱清言上游客户端，实现 provider.Upstream。
//
// 请求形态：所有私有接口都经 signedDo 发（自动注入伪装头 + 签名头）。
// 响应形态：统一信封 {status, message, result, rid}，status != 0 即错误。
package glm

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// Client 上游 HTTP 客户端。Base 可覆盖以便测试。
type Client struct {
	HTTP *http.Client
	Base string

	// mu 保护 accessToken 缓存（多账号并发刷新）。
	mu sync.Mutex
	// accessTokens 按 refresh_token 缓存 access_token，避免同一账号并发重复刷新。
	// 清言每次刷新都会**轮换 refresh_token**，重复刷新会让先落盘的那个失效。
	accessTokens map[string]cachedToken
}

// cachedToken 缓存的 access_token 及其过期时刻。
type cachedToken struct {
	AccessToken string
	ExpiresAt   time.Time
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
		ResponseHeaderTimeout: requestTimeout,
	}
	return &Client{
		HTTP:         &http.Client{Timeout: requestTimeout, Transport: tr},
		Base:         Base,
		accessTokens: map[string]cachedToken{},
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
		return Base
	}
	return c.Base
}

// ---------------------------------------------------------------------------
// 信封与请求
// ---------------------------------------------------------------------------

// envelope 清言统一响应信封。
type envelope struct {
	Status  int             `json:"status"`
	Message string          `json:"message"`
	Result  json.RawMessage `json:"result"`
	Rid     string          `json:"rid"`
}

// errText 从信封提取可读错误文案。
func (e *envelope) errText() string {
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = "unknown error"
	}
	if e.Status != 0 {
		return fmt.Sprintf("status=%d %s", e.Status, msg)
	}
	return msg
}

// signedDo 发一个带签名头的请求并返回原始响应。
// body 为 nil 表示无请求体。accept 传 SSE 用于对话流接口。
func (c *Client) signedDo(ctx context.Context, method, path, token, accept string, body []byte) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rdr)
	if err != nil {
		return nil, err
	}
	applySignedHeaders(req, token, accept)
	return c.HTTP.Do(req)
}

// doJSON 发请求并把信封解成 result。
// token 为空的接口（如用 refresh_token 换 token）由调用方在 path 前自行处理。
func (c *Client) doJSON(ctx context.Context, method, path, token string, body any) (json.RawMessage, error) {
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	resp, err := c.signedDo(ctx, method, path, token, "", raw)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		// 非 JSON 响应：多半是 WAF 拦截页或网关错误页。
		return nil, &provider.Error{
			Kind:   provider.ErrWafBlock,
			Status: resp.StatusCode,
			Msg:    truncate(string(payload), 200),
		}
	}
	if resp.StatusCode >= 400 || env.Status != 0 {
		return nil, classifyEnvelope(resp.StatusCode, &env)
	}
	return env.Result, nil
}

// doJSONEnvelope 与 doJSON 相同，但**把非 0 业务码原样返回**而不转成 error。
//
// 用途：某些接口用非 0 的 `status` 表达**正常业务语义**而非错误。
// 典型例子是 `daily_login_score` 的 `status=10001 "今日已领取"`——
// 这是幂等重复调用的正常结果，若按 doJSON 当错误抛出，
// 调用方就永远看不到「已领取」这个语义（会把重复调用误报成失败）。
//
// 仅 HTTP 层错误（>=400）与非法 JSON 才返回 error；
// 业务码由调用方自行解释。
func (c *Client) doJSONEnvelope(ctx context.Context, method, path, token string, body any) (*envelope, error) {
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	resp, err := c.signedDo(ctx, method, path, token, "", raw)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, &provider.Error{
			Kind:   provider.ErrWafBlock,
			Status: resp.StatusCode,
			Msg:    truncate(string(payload), 200),
		}
	}
	// 仅 HTTP 层错误转成 error；业务码留给调用方
	if resp.StatusCode >= 400 {
		return nil, classifyEnvelope(resp.StatusCode, &env)
	}
	return &env, nil
}

// classifyEnvelope 把信封错误映射成 provider.Error。
// 判据顺序很关键（对齐 wild-work 既有教训）：
//   - 40102 unauthorized user → 登录态失效，可 refresh 自愈
//   - 429 → 软限流
//   - 其余 4xx → 请求级问题，不罚账号
func classifyEnvelope(status int, env *envelope) error {
	kind := provider.ErrClient
	switch {
	case env.Status == 40102 || status == http.StatusUnauthorized:
		kind = provider.ErrSessionDead
	case status == http.StatusTooManyRequests:
		kind = provider.ErrSoftRate
	case status == http.StatusForbidden:
		kind = provider.ErrWafBlock
	case status >= 500:
		kind = provider.ErrServer
	case status >= 400:
		kind = provider.ErrClient
	}
	return &provider.Error{Kind: kind, Status: status, Msg: env.errText()}
}

// ---------------------------------------------------------------------------
// provider.Upstream 实现
// ---------------------------------------------------------------------------

// RefreshToken 用 refresh_token 换 access_token 并**落盘轮换后的新 refresh_token**。
//
// 关键：清言刷新会同时轮换 refresh_token。不落盘 = 下次启动用旧 refresh token，
// 而旧的那个已被上游作废 → 账号永久失效（wild-work 不变量 19/20 的同款陷阱）。
func (c *Client) RefreshToken(a *auth.Auth) error {
	res, err := c.refresh(context.Background(), a.RefreshTokenValue())
	if err != nil {
		return err
	}

	a.Lock()
	a.AccessToken = res.AccessToken
	if res.RefreshToken != "" {
		a.RefreshToken = res.RefreshToken
	}
	if res.ExpiresAt > 0 {
		a.ExpiresAt = res.ExpiresAt
	}
	if res.UID != "" {
		a.UID = res.UID
	}
	a.Unlock()

	// 落盘轮换后的凭证。失败必须报错——否则内存与磁盘不一致，
	// 下次启动会拿着已作废的 refresh token 重启（同不变量 20）。
	if err := a.SaveAtomic(); err != nil {
		return fmt.Errorf("glm: 凭证落盘失败（refresh token 已轮换，务必重试）: %w", err)
	}

	// 刷新成功后更新缓存
	c.mu.Lock()
	c.accessTokens[res.RefreshToken] = cachedToken{
		AccessToken: res.AccessToken,
		ExpiresAt:   time.Unix(res.ExpiresAt, 0),
	}
	c.mu.Unlock()
	return nil
}

// refreshResult 刷新结果。
type refreshResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	UID          string
}

// Account 一次性验证 refresh_token 得到的结果（登录编排用）。
type Account struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	UID          string
	Nickname     string
}

// ValidateRefreshToken 用默认客户端验证一个 refresh_token（生产入口）。
func ValidateRefreshToken(ctx context.Context, refreshToken string) (Account, error) {
	return New().Validate(ctx, refreshToken)
}

// Validate 用本客户端验证 refresh_token 并返回账号信息。
//
// 这是登录流程的唯一入口：清言没有可编程登录接口，凭据只能由用户
// 从浏览器 Cookie 里取出后交进来验证。验证通过即证明凭据有效。
//
// 不落盘、不改动任何已有账号——纯校验。
// 设计为方法而非包级函数，是为了让测试能把 Base 指向 mock 上游。
func (c *Client) Validate(ctx context.Context, refreshToken string) (Account, error) {
	res, err := c.refresh(ctx, strings.TrimSpace(refreshToken))
	if err != nil {
		return Account{}, err
	}
	acct := Account{
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		ExpiresAt:    res.ExpiresAt,
		UID:          res.UID,
	}
	// 取昵称（失败不阻断：refresh 已证明凭据有效）
	if uid, name, uerr := c.fetchUserInfoWithToken(ctx, res.AccessToken); uerr == nil {
		if uid != "" {
			acct.UID = uid
		}
		acct.Nickname = name
	}
	return acct, nil
}

// fetchUserInfoWithToken 用给定 access_token 拉账号信息。
func (c *Client) fetchUserInfoWithToken(ctx context.Context, token string) (string, string, error) {
	raw, err := c.doJSON(ctx, http.MethodGet, EpUserInfo, token, nil)
	if err != nil {
		return "", "", err
	}
	var u struct {
		UserID   string `json:"user_id"`
		Nickname string `json:"nickname"`
		Phone    string `json:"phone"`
		Email    string `json:"email"`
		IsGuest  bool   `json:"is_guest"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return "", "", err
	}
	if u.IsGuest || strings.Contains(u.Nickname, "访客") || strings.Contains(u.Email, "@guest") {
		return u.UserID, u.Nickname, &provider.Error{
			Kind: provider.ErrSessionDead, Status: 401,
			Msg: "访客账号不可用，请登录真实账号",
		}
	}
	name := u.Nickname
	if name == "" {
		name = u.Phone
	}
	if name == "" {
		name = u.Email
	}
	return u.UserID, name, nil
}

// refresh 调 user/refresh 换 token。refreshToken 为空时报错。
func (c *Client) refresh(ctx context.Context, refreshToken string) (refreshResult, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return refreshResult{}, &provider.Error{
			Kind: provider.ErrSessionDead, Status: 401, Msg: "缺少 refresh_token",
		}
	}
	raw, err := c.doJSON(ctx, http.MethodPost, EpRefresh, refreshToken, map[string]any{})
	if err != nil {
		return refreshResult{}, err
	}
	var r struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		UserID       string `json:"user_id"`
		ExpiresIn    int64  `json:"expires_in"`
		IsGuest      bool   `json:"is_guest"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return refreshResult{}, fmt.Errorf("glm: 解析 refresh 响应失败: %w", err)
	}
	if r.AccessToken == "" {
		return refreshResult{}, &provider.Error{
			Kind: provider.ErrSessionDead, Status: 401, Msg: "refresh 响应缺少 access_token",
		}
	}
	if r.IsGuest {
		return refreshResult{}, &provider.Error{
			Kind: provider.ErrSessionDead, Status: 401,
			Msg: "访客账号不可用，请用真实账号登录智谱清言后重新获取 refresh_token",
		}
	}
	// ⚠️ 上游 refresh **不返回 expires_in**（2026-09-26 实测：响应只有
	// user_id / access_token / refresh_token 三个字段）。
	//
	// 早期实现只用 expires_in，导致 expiresAt 恒为 0 → NeedsRefresh 恒为真
	// → **每次请求都刷 token**（日志实测：同一账号 9 秒内刷两次）。
	// 危害：与 App 端高频互踩，可能触发风控。
	//
	// 修复：优先 expires_in；缺失时用 access_token 的 **JWT exp** 兜底。
	// JWT 由上游签名，其 exp 是权威到期时刻（实测剩余 24h）。
	exp := int64(0)
	if r.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second).Unix()
	} else if jwtExp := jwtExpiry(r.AccessToken); jwtExp > 0 {
		exp = jwtExp
	}
	// 两者都拿不到时给一个保守值，避免 expiresAt=0 造成的刷新风暴。
	// 24h 是实测的 token 寿命；宁可早刷一次，也不要每次请求都刷。
	if exp == 0 {
		exp = time.Now().Add(24 * time.Hour).Unix()
	}
	return refreshResult{
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		ExpiresAt:    exp,
		UID:          r.UserID,
	}, nil
}

// jwtExpiry 解出 access_token 的 JWT payload 里的 exp（Unix 秒）。
// 非 JWT / 无 exp / 解析失败时返回 0。
//
// 不校验签名：用途仅是从本地凭证自身的 payload 读出自报到期时刻。
// 清言的 access_token 实测是标准 JWT（3 段），exp 可信。
func jwtExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	p := parts[1]
	if r := len(p) % 4; r != 0 {
		p += strings.Repeat("=", 4-r)
	}
	raw, err := base64.URLEncoding.DecodeString(p)
	if err != nil {
		return 0
	}
	var payload struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return 0
	}
	return payload.Exp
}

// AdoptJWTExpiry 用 access_token 的 JWT exp 校正本地 ExpiresAt（仅当 JWT 更晚时）。
//
// 用于**自愈历史脏数据**：早期版本因上游不返回 expires_in 而把 expiresAt 落成 0，
// 导致每次请求都刷 token。加载凭证时调用本方法即可原地修好（仅内存，不写盘——
// 校正后不再触发刷新路径，下次真正刷新时自然落盘正确值）。
//
// 对应 wild-work 不变式 19（401 必须自愈）与 R21（expires_in 单位陷阱）的同族问题。
func AdoptJWTExpiry(a *auth.Auth) {
	if a == nil {
		return
	}
	exp := jwtExpiry(a.AccessTokenValue())
	if exp <= 0 {
		return
	}
	a.Lock()
	defer a.Unlock()
	if exp > a.ExpiresAt {
		a.ExpiresAt = exp
	}
}

// acquireToken 返回可用的 access_token。
// 若本地 token 尚未过期（留 10 分钟余量）直接用；否则刷新并落盘。
func (c *Client) acquireToken(ctx context.Context, a *auth.Auth) (string, error) {
	if a == nil {
		return "", errors.New("glm: nil auth")
	}
	// 本地 token 仍新鲜 → 直接用（避免与 App 端互踩 refresh）
	if !a.NeedsRefresh(10 * time.Minute) {
		if tok := a.AccessTokenValue(); tok != "" {
			return tok, nil
		}
	}
	// 刷新。注意 refresh 会轮换 refresh_token 并落盘。
	if err := c.RefreshToken(a); err != nil {
		return "", err
	}
	return a.AccessTokenValue(), nil
}

// FetchUserInfo 拉取账号信息（昵称/手机/邮箱/访客标记）。
// 返回 (uid, nickname, err)。
func (c *Client) FetchUserInfo(a *auth.Auth) (string, string, error) {
	token, err := c.acquireToken(context.Background(), a)
	if err != nil {
		return "", "", err
	}
	raw, err := c.doJSON(context.Background(), http.MethodGet, EpUserInfo, token, nil)
	if err != nil {
		return "", "", err
	}
	var u struct {
		UserID   string `json:"user_id"`
		Nickname string `json:"nickname"`
		Phone    string `json:"phone"`
		Email    string `json:"email"`
		IsGuest  bool   `json:"is_guest"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return "", "", fmt.Errorf("glm: 解析 user/info 失败: %w", err)
	}
	// 访客过滤：昵称含「访客」或邮箱为 @guest 一律拒绝（参考 Chat2API 判据）。
	if u.IsGuest || strings.Contains(u.Nickname, "访客") || strings.Contains(u.Email, "@guest") {
		return u.UserID, u.Nickname, &provider.Error{
			Kind: provider.ErrSessionDead, Status: 401,
			Msg: "访客账号不可用，请登录真实账号",
		}
	}
	name := u.Nickname
	if name == "" {
		name = u.Phone
	}
	if name == "" {
		name = u.Email
	}
	return u.UserID, name, nil
}

// FetchModels 返回静态模型表。
// 清言无公开模型列表接口（官方 App 是客户端硬编码的），故静态表即权威清单。
func (c *Client) FetchModels(_ *auth.Auth) ([]provider.ModelInfo, error) {
	return StaticModels(), nil
}

// FetchModelPricing 清言按「额度」而非按 token 计费，无公开费率表。
// 返回空列表：面板对空费率显示「不适用」，不伪造价格。
func (c *Client) FetchModelPricing(_ *auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

// MemberInfo 会员与积分信息（来自 EpMemberInfo）。
type MemberInfo struct {
	// Score 积分余额，**已换算成「积分」单位**（上游返回的 left_score 单位是「分」，除以 100）。
	Score int64
	// RawScore 上游原始值（单位「分」），保留以便排查。
	RawScore int64
	// LeftToken 剩余 token 额度。
	LeftToken int64
	// ScoreRule 积分规则文案（如「免费用户，登录赠送200积分/天」）。
	ScoreRule string
	// IsMember 是否会员。
	IsMember bool
}

// scoreUnit 上游 left_score 的单位换算：1 积分 = 100 分。
//
// 依据（2026-09-26 实测）：接口返回 left_score=300000，而清言 App 界面显示 3000 积分，
// 两者恰好差 100 倍，故「分」是最小单位，除以 100 得积分。
const scoreUnit = 100

// FetchMemberInfo 拉取会员与积分信息。
func (c *Client) FetchMemberInfo(a *auth.Auth) (MemberInfo, error) {
	token, err := c.acquireToken(context.Background(), a)
	if err != nil {
		return MemberInfo{}, err
	}
	raw, err := c.doJSON(context.Background(), http.MethodGet, EpMemberInfo, token, nil)
	if err != nil {
		return MemberInfo{}, err
	}
	var m struct {
		LeftScore    int64  `json:"left_score"`
		LeftToken    int64  `json:"left_token"`
		ScoreRule    string `json:"score_rule"`
		IsMember     bool   `json:"is_member"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return MemberInfo{}, fmt.Errorf("glm: 解析 member_info 失败: %w", err)
	}
	return MemberInfo{
		Score:     m.LeftScore / scoreUnit,
		RawScore:  m.LeftScore,
		LeftToken: m.LeftToken,
		ScoreRule: m.ScoreRule,
		IsMember:  m.IsMember,
	}, nil
}

// UserResource 返回可用积分余额。
//
// 清言积分 = 登录即送的免费额度（服务端按天被动发放，无「领取」接口），
// 从 member_info.left_score 读取并换算成「积分」单位。
//
// 注意口径：这里返回的是**账号可用额度**，会随对话消耗递减，
// 与「签到获得」无关——清言没有签到领取机制（详见 constants.go 的说明）。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	info, err := c.FetchMemberInfo(a)
	if err != nil {
		return 0, err
	}
	return info.Score, nil
}

// UserResourceDetail 返回积分明细。
//
// 清言不提供积分条目明细（无「哪笔积分何时到期」这类接口），
// 故只回一条汇总条目：把当前余额作为一个无到期日的条目，
// 让面板能显示数字而不是空白。ExpireAt 留空 = 上游未下发到期时间（前端据此隐藏该列）。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	info, err := c.FetchMemberInfo(a)
	if err != nil {
		return 0, nil, err
	}
	name := "积分余额"
	if info.ScoreRule != "" {
		name = "积分余额（" + info.ScoreRule + "）"
	}
	items := []provider.ResourceItem{{
		Name:   name,
		Total:  info.Score,
		Used:   0,
		Remain: info.Score,
		Usable: true,
	}}
	return info.Score, items, nil
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
func (c *Client) Classify(status int, body string) provider.ErrKind {
	return Classify(status, body)
}

// Classify 错误分类（独立函数便于测试）。
//
// 判据顺序（对齐 wild-work 不变量 15：429 必须优先于余额类判据）：
//   - 40102 / 401 → ErrSessionDead（可 refresh 自愈，调度器会重试）
//   - 429 → ErrSoftRate
//   - 403 → ErrWafBlock（WAF 拦截页，账号软冷却）
//   - 5xx → ErrServer
//   - 其余 4xx → ErrClient
func Classify(status int, body string) provider.ErrKind {
	lower := strings.ToLower(body)
	switch {
	case status == http.StatusUnauthorized || strings.Contains(body, "40102"):
		return provider.ErrSessionDead
	case status == http.StatusTooManyRequests:
		return provider.ErrSoftRate
	case status == http.StatusForbidden:
		return provider.ErrWafBlock
	case status >= 500:
		return provider.ErrServer
	case status >= 400:
		// 内容审核拦截（清言会对敏感内容返回业务错误码）
		if strings.Contains(lower, "sensitive") || strings.Contains(body, "内容") ||
			strings.Contains(body, "审核") {
			return provider.ErrContentBlocked
		}
		return provider.ErrClient
	default:
		return provider.ErrNone
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

// truncate 字符串截断（rune 安全）。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}
