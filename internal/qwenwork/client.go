// client.go 千问办公上游客户端：COSY 签名对话/模型 + Bearer 余额/费率，
// 实现 provider.Upstream 接口。
//
// 双轨鉴权（实测混用必失败，见备忘 §2.7）：
//   - COSY 端点（GatewayBase）：推理、/api/v2/model/list
//   - Bearer 端点（WebBase）  ：/user/balance、/user/wallets、/user/billings、/api/chat-modes
package qwenwork

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// Client 千问办公上游 HTTP 客户端。
type Client struct {
	HTTP    *http.Client
	Gateway string // 推理网关（COSY），默认 https://gateway.qwenwork.cn
	Web     string // 网页域（Bearer），默认 https://qwenwork.cn
}

// New 生产默认。网关对 HTTP/2 不友好（对齐 qoder 渠道经验），强制 HTTP/1.1。
func New() *Client { return NewWithTimeout(180 * time.Second) }

// NewWithTimeout 指定上游 HTTP 超时；配置连接池。
func NewWithTimeout(timeout time.Duration) *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{}, // 强制 HTTP/1.1
	}
	return &Client{
		HTTP:    &http.Client{Timeout: timeout, Transport: tr},
		Gateway: GatewayBase,
		Web:     WebBase,
	}
}

func (c *Client) gateway() string {
	if c.Gateway == "" {
		return GatewayBase
	}
	return c.Gateway
}

func (c *Client) web() string {
	if c.Web == "" {
		return WebBase
	}
	return c.Web
}

// identity 从 auth 取 COSY info 所需字段（全程持读锁快照）。
// auth.Auth 无 email 字段：COSY info 的 email 传空串（实测网关不校验 email 值，
// 只要 uid + security_oauth_token 能对上即可；桌面端 auth-v2.dat 里的 email
// 本就是 phone_xxx@phone.local 占位形）。
func identity(a *auth.Auth) (uid, name, email, token string) {
	a.RLock()
	defer a.RUnlock()
	return a.UID, a.Nickname, "", a.AccessToken
}

// bearerHeaders 网页域请求头（纯 Bearer；实测 Accept 头非必需但保持礼貌）。
func bearerHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Authorization", "Bearer "+a.JWT())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
}

// doBearer 调网页域 GET 端点并解 {code,data} 信封。
// 网页域错误形如 {"code":"invalid-credential","msg":"..."} 或 {"errorCode":..,"errorMessage":..}。
func (c *Client) doBearer(url string, a *auth.Auth) (json.RawMessage, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	bearerHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &provider.Error{Kind: Classify(resp.StatusCode, string(raw)),
			Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env struct {
		Code string          `json:"code"`
		Data json.RawMessage `json:"data"`
		Msg  string          `json:"msg"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != "ok" && env.Code != "" {
		return nil, &provider.Error{Kind: Classify(resp.StatusCode, env.Msg+env.Code),
			Status: resp.StatusCode, Msg: fmt.Sprintf("code=%s msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// ---------------------------------------------------------------------------
// provider.Upstream 实现
// ---------------------------------------------------------------------------

// RefreshToken 用 refreshToken 走 deviceToken/refresh 轮换（body 带 target:"c"）。
// 成功时更新 a 字段，调用方负责 SaveAtomic（refresh token 会轮换，不落盘=下次失效）。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	rt := strings.TrimSpace(a.RefreshToken)
	if rt == "" {
		return fmt.Errorf("no refreshToken")
	}
	log.Printf("qwenwork refresh start uid=%s", a.UID)
	body, _ := json.Marshal(map[string]string{"refresh_token": rt, "target": "c"})
	req, err := http.NewRequest(http.MethodPost, c.gateway()+EpDTRefresh, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// refresh 用独立短超时客户端
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("qwenwork refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		err := &provider.Error{Kind: provider.ErrSessionDead, Status: resp.StatusCode,
			Msg: truncate(string(raw), 200)}
		log.Printf("qwenwork refresh session-dead uid=%s err=%v", a.UID, err)
		return err
	}
	if resp.StatusCode >= 400 {
		err := fmt.Errorf("deviceToken refresh http %d: %s", resp.StatusCode, truncate(string(raw), 200))
		log.Printf("qwenwork refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	var out struct {
		Token        string `json:"token"`
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    string `json:"expires_at"`
		ExpiresIn    int64  `json:"expires_in"` // 秒（与 access token JWT 寿命同量纲，见下）
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Printf("qwenwork refresh parse failed uid=%s err=%v", a.UID, err)
		return fmt.Errorf("deviceToken refresh parse: %w", err)
	}
	dt := out.DeviceToken
	if dt == "" {
		dt = out.Token
	}
	if dt == "" || out.RefreshToken == "" {
		return fmt.Errorf("deviceToken refresh: incomplete token pair")
	}
	a.AccessToken = dt
	a.RefreshToken = out.RefreshToken
	now := time.Now()
	// 过期时刻优先取绝对字段 expires_at，再回退相对 expires_in。
	// expires_in 单位是【秒】——与 workbuddyai 同口径，且实测 expires_in=604800
	// 恰好等于 access token JWT 的 iat→exp（7 天）。
	// 回归：早期误按毫秒处理（*time.Millisecond），把 7 天压成 604.8 秒 →
	// 落盘 expiresAt 比真实寿命少 ~7 天 → NeedsRefresh 恒为真 → 每次请求都刷 token，
	// 与千问办公 App 高频互踩直至 refresh token 作废、账号被禁用。
	// 佐证：同仓 workbuddy/trae/workbuddyai 的 auth 文件 expiresAt 与 JWT exp 逐秒一致，
	// 仅 qwenwork 偏离 6.99 天（见渠道备忘）。
	if out.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, out.ExpiresAt); err == nil {
			a.ExpiresAt = t.Unix()
		}
	}
	if a.ExpiresAt == 0 && out.ExpiresIn > 0 {
		a.ExpiresAt = now.Add(time.Duration(out.ExpiresIn) * time.Second).Unix()
	}
	if a.ExpiresAt == 0 {
		// 上游两字段都缺失：JWT 观测寿命 7 天，保守取一半。
		a.ExpiresAt = now.Add(84 * time.Hour).Unix()
	}
	log.Printf("qwenwork refresh success uid=%s expires_at=%d (in=%ds)", a.UID, a.ExpiresAt, out.ExpiresIn)
	return nil
}

// ChatStream 发推理请求并返回原始嵌套 SSE body 流（调用方负责 Close）。
// body 是 server 侧改写后的 OpenAI 请求；本渠道透传（补 request_id/session_id + model key 映射）。
// 非 2xx 时 rc=nil、respBody=上游响应体、err=nil；仅传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	prepared, err := c.prepareChatBody(body)
	if err != nil {
		return nil, 0, nil, err
	}
	uid, name, email, token := identity(a)
	if token == "" {
		return nil, 0, nil, fmt.Errorf("no access token")
	}
	sess, err := NewCosySession(uid, name, email, token)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("cosy session: %w", err)
	}
	rawURL := c.gateway() + EpChat
	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(prepared))
	if err != nil {
		return nil, 0, nil, err
	}
	headers := map[string]string{}
	if err := sess.ApplyHeadersWithUID(headers, string(prepared), rawURL, uid); err != nil {
		return nil, 0, nil, fmt.Errorf("cosy headers: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("x-model-key", modelKeyOf(prepared))

	resp, err := c.HTTP.Do(req)
	if err != nil {
		log.Printf("qwenwork chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		log.Printf("qwenwork chat_stream uid=%s: upstream %d body=%s",
			a.UID, resp.StatusCode, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// prepareChatBody 解析客户端 OpenAI body，补 request_id/session_id、映射 model key。
// 服务端无状态：messages 由调用方携带全量历史（本渠道不做改写，developer 角色实测可接受）。
// 注意：server 层正常会把 model 去前缀后传入，但防御性兼容「qwenwork/flash」带前缀形式。
// 另：x-model-key 决定实际路由（优先级高于 body model），缺省或非法档位 → 上游包 403
// "Model is not available for this user"（envelope 内嵌，HTTP 仍 200）。
//
// business 段（2026-09-24 上游 1.0.4 起强校验）：网关按 body.business.{product,type}
// 解析模型目录，缺失时对话恒返回 HTTP 200 + envelope 503 "Model catalog unavailable"
// （模型列表/余额/费率均不受影响，只有推理路径受影响）。仅补 Cosy-Business-* 头
// 不能替代该字段（已实测四组对照：body 缺 business 的头/原生两种 body 均 503，
// 补上后均 200；原生 body 结构、Encode=1 组包、机器指纹均非必要条件）。
func (c *Client) prepareChatBody(body []byte) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("parse chat body: %w", err)
	}
	// model → 上游档位 key：先剥渠道前缀（防御），再查映射表
	m, _ := obj["model"].(string)
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	obj["model"] = ModelKey(m)
	// 必填字段：request_id / session_id（缺失 → envelope 400）
	if s, _ := obj["request_id"].(string); strings.TrimSpace(s) == "" {
		obj["request_id"] = uuid4()
	}
	if s, _ := obj["session_id"].(string); strings.TrimSpace(s) == "" {
		obj["session_id"] = uuid4()
	}
	// 模型目录标识：与 Cosy-Business-Product / Cosy-Business-Type 静态头同源
	obj["business"] = map[string]any{
		"product":          BusinessProduct,
		"type":             BusinessType,
		"version":          "1",
		"feature_switches": map[string]any{},
	}
	// stream 强制 true：上游为 SSE-only 端点（非流式由 Stream/Aggregate 聚合实现）
	obj["stream"] = true
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// modelKeyOf 从已编码 body 提取映射后的 model key（供 x-model-key 头）。
func modelKeyOf(prepared []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(prepared, &probe) == nil && probe.Model != "" {
		return probe.Model
	}
	return "pro"
}

// modelEntry 动态模型接口单条（scene=qwork）。
type modelEntry struct {
	Key            string  `json:"key"`
	DisplayName    string  `json:"display_name"`
	Enable         bool    `json:"enable"`
	IsDefault      bool    `json:"is_default"`
	IsNew          bool    `json:"is_new"`
	PriceFactor    float64 `json:"price_factor"`
	MaxInputTokens int64   `json:"max_input_tokens"`
}

// FetchModels COSY GET /api/v2/model/list，取 scene=qwork 的档位列表。
// GET 无 body，签名用空串 ""（非 "{}"，后者 403 Signature invalid，与 qoder 一致）。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	uid, name, email, token := identity(a)
	if token == "" {
		return nil, fmt.Errorf("no access token")
	}
	sess, err := NewCosySession(uid, name, email, token)
	if err != nil {
		return nil, fmt.Errorf("cosy session: %w", err)
	}
	rawURL := c.gateway() + EpModels
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	headers := map[string]string{}
	if err := sess.ApplyHeadersWithUID(headers, "", rawURL, uid); err != nil {
		return nil, fmt.Errorf("cosy headers: %w", err)
	}
	headers["Accept"] = "application/json" // 覆盖 SSE accept
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &provider.Error{Kind: Classify(resp.StatusCode, string(raw)),
			Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var scenes map[string][]modelEntry
	if err := json.Unmarshal(raw, &scenes); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	rows := scenes["qwork"]
	out := make([]provider.ModelInfo, 0, len(rows))
	for _, m := range rows {
		if m.Key == "" || !m.Enable {
			continue
		}
		// 上游真实上限实测 ≈180K（175K OK / 179K → 424 All models failed，见备忘 §6.9）；
		// 网页 chat-modes 的 1M 是前端档位选择器最大值，非单次请求能力。
		// 上游给的 180000 是准确口径，仅在缺失时兜底同值。
		ctx := m.MaxInputTokens
		if ctx <= 0 {
			ctx = 180_000
		}
		out = append(out, provider.ModelInfo{
			ID:            m.Key,
			Name:          displayName(m),
			ContextWindow: ctx,
			// 能力：/api/chat-modes 声明三档均 is_reasoning/is_vl（pro 声明 is_vl=false），
			// 实测 pro 路由 glm-5.2 思考可选；工具调用三档实测均支持。
			SupportsImages:    m.Key != "pro",
			SupportsReasoning: true,
			SupportsTools:     true,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty qwork scene")
	}
	return out, nil
}

// displayName 档位展示名：i18n 缺失时回退 display_name → key。
func displayName(m modelEntry) string {
	return m.DisplayName
}

// chatMode 网页 /api/chat-modes 单条（qwork scene）。
type chatMode struct {
	Key            string `json:"key"`
	PriceFactor    float64
	Factor         float64 `json:"price_factor"`
	CostFactor     float64 `json:"cost_factor"`
	Enable         bool    `json:"enable"`
	IsNew          bool    `json:"is_new"`
	MaxInputTokens int64   `json:"max_input_tokens"`
}

// FetchModelPricing 费率：优先网页 /api/chat-modes（Bearer，price_factor），
// 失败回退静态表。Rate 语义与 wild-work 一致（相对倍率，pro=1.0 基准）。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	url := c.web() + EpChatModes
	data, err := c.doBearer(url, a)
	if err == nil {
		var scenes map[string][]chatMode
		if json.Unmarshal(data, &scenes) == nil {
			rows := scenes["qwork"]
			if len(rows) > 0 {
				out := make([]provider.ModelPricing, 0, len(rows))
				for _, m := range rows {
					if m.Key == "" || !m.Enable {
						continue
					}
					rate := m.Factor
					if rate == 0 {
						rate = m.CostFactor
					}
					note := ""
					if m.IsNew {
						note = "新档位"
					}
					explicit := true
					out = append(out, provider.ModelPricing{
						Model:    m.Key,
						Channel:  ChannelName,
						Rate:     rate,
						Note:     note,
						Explicit: &explicit,
					})
				}
				if len(out) > 0 {
					return out, nil
				}
			}
		}
	} else {
		log.Printf("qwenwork pricing via chat-modes failed: %v（回退静态表）", truncate(err.Error(), 120))
	}
	// 回退静态表
	out := make([]provider.ModelPricing, 0, len(staticModels))
	for _, m := range staticModels {
		out = append(out, provider.ModelPricing{
			Model:   m.ID,
			Channel: ChannelName,
			Rate:    priceFactorOf(m.ID),
		})
	}
	return out, nil
}

// walletEntry /user/wallets 单条钱包。
type walletEntry struct {
	Balance float64 `json:"balance"`
	ValidTo string  `json:"valid_to"`
}

// walletTotals /user/wallets 三类汇总余额。
type walletTotals struct {
	TotalBalance float64 `json:"total_balance"`
}

// FetchNickname 拉取账号昵称（/user/info 的 nickname/name/username 首个非空字段）。
// 用途：登录时 JWT 解不出 username 时的兑底（OAuth 兑换的 access token 实测
// 偶发无 username，而 deviceToken/refresh 后的 token 才有；详见备忘 §9.10）。
func (c *Client) FetchNickname(a *auth.Auth) (string, error) {
	data, err := c.doBearer(c.web()+EpUserInfo, a)
	if err != nil {
		return "", err
	}
	var info struct {
		Nickname string `json:"nickname"`
		Name     string `json:"name"`
		Username string `json:"username"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return "", fmt.Errorf("userinfo parse: %w", err)
	}
	if info.Nickname != "" {
		return info.Nickname, nil
	}
	if info.Name != "" {
		return info.Name, nil
	}
	return info.Username, nil
}

// UserResource 查询账号当前可花费余额（所有钱包聚合，×100 取整为积分厘单位）。
// 上游余额为小数（如 2094.8294），wild-work 全链路按 int64 积分（厘=1/100）口径，
// 故此处 ×100 取整（与面板显示位数一致：显示 2 位小数）。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	remain, _, err := c.userResource(a)
	return remain, err
}

// UserResourceDetail 查询余额明细（当日钱包 + 长期钱包 + 月度钱包）。
// 返回的 remain 口径与 UserResource 一致。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return c.userResource(a)
}

// userResource 单次请求同时产出总额与明细。
// 积分单位：上游 /user/balance 直接下发「界面同源」的积分数（如 2090.38），
// 与其他渠道一致地 int64 直转（截断小数）；不做 ×100 缩放 —— 否则面板会虚高 100 倍。
// 小数部分（对话消耗的零头，如 -0.0014）在截断后不可见，可接受。
func (c *Client) userResource(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	// 1) 总余额（单请求最轻量）
	data, err := c.doBearer(c.web()+EpBalance, a)
	if err != nil {
		return 0, nil, err
	}
	var bal struct {
		Balance      float64 `json:"balance"`
		FreezeCredit float64 `json:"freeze_credit"`
	}
	if err := json.Unmarshal(data, &bal); err != nil {
		return 0, nil, fmt.Errorf("balance parse: %w", err)
	}
	remainTotal := truncCredits(bal.Balance)

	// 2) 钱包分账（含到期时间）
	data, err = c.doBearer(c.web()+EpWallets, a)
	if err != nil {
		// 明细失败不影响主流程：返回总额 + 单条聚合条目
		return remainTotal, []provider.ResourceItem{{
			Name: "全部钱包", Total: remainTotal, Used: 0, Remain: remainTotal, Usable: true,
		}}, nil
	}
	var wallets struct {
		ActiveWallets struct {
			Wallets []walletEntry `json:"wallets"`
		} `json:"active_wallets"`
		DailyCredits    walletTotals `json:"daily_credits"`
		LongtermCredits walletTotals `json:"longterm_credits"`
		MonthlyCredits  walletTotals `json:"monthly_credits"`
	}
	if err := json.Unmarshal(data, &wallets); err != nil {
		return remainTotal, []provider.ResourceItem{{
			Name: "全部钱包", Total: remainTotal, Used: 0, Remain: remainTotal, Usable: true,
		}}, nil
	}
	items := make([]provider.ResourceItem, 0, 4)
	add := func(name, key string, bal float64, expireAt string) {
		remain := truncCredits(bal)
		items = append(items, provider.ResourceItem{
			Name:     name,
			Total:    remain,
			Used:     0,
			Remain:   remain,
			ExpireAt: expireAt,
			Key:      key,  // 钱包类别伪键（daily/longterm/monthly），ledger 差分对账用
			Usable:   true, // 千问办公无端点分区，所有钱包均可被本工具消耗
		})
	}
	// 当日钱包（每日 00:00 发放、23:59:59 过期）
	if d := wallets.DailyCredits.TotalBalance; d > 0 {
		exp := ""
		// 取当日钱包的 valid_to（active_wallets 中最临近的过期时间）
		for _, w := range wallets.ActiveWallets.Wallets {
			if w.ValidTo != "" {
				exp = expireDate(w.ValidTo)
				break
			}
		}
		add("每日奖励", "daily", d, exp)
	}
	// 长期钱包（欢迎奖励/充值）
	if l := wallets.LongtermCredits.TotalBalance; l > 0 {
		add("长期积分", "longterm", l, "")
	}
	// 月度钱包（订阅权益）
	if m := wallets.MonthlyCredits.TotalBalance; m > 0 {
		add("月度积分", "monthly", m, "")
	}
	if len(items) == 0 {
		items = append(items, provider.ResourceItem{
			Name: "全部钱包", Total: remainTotal, Used: 0, Remain: remainTotal, Usable: true,
		})
	}
	return remainTotal, items, nil
}

// truncCredits 浮点积分 → int64（直接截断小数，与其他渠道 int64(Remaining) 口径一致）。
// 千问办公余额是浮点（2090.38），截断后 2090 与界面显示一致。
func truncCredits(v float64) int64 {
	if v < 0 {
		return 0
	}
	return int64(v)
}

// softRateResetLoc 上游墙钟时间口径：固定按 UTC+8 解释（对齐 qoder/workbuddyai）。
// wallet valid_to 形如 "2026-09-19T00:00:00+08:00"（带时区偏移），
// 用 ParseInLocation + RFC3339 即可正确解析；fallback 到固定 UTC+8 墙钟。
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// expireDate 把上游时间串转为 YYYY-MM-DD；缺失/不可解析时返回空串。
func expireDate(ts string) string {
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.In(softRateResetLoc).Format("2006-01-02")
	}
	// 无时区后缀时按 UTC+8 墙钟解析
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, ts, softRateResetLoc); err == nil {
			return t.Format("2006-01-02")
		}
	}
	return ""
}

// DailyCheckin 千问办公无签到活动：每日积分由服务端 00:00（UTC+8）被动发放，
// 无 claim 接口、无需客户端保活（实测证据见备忘 §6.5）。
// 直接返回错误语义（对齐 qoder 渠道），调度器不应再对该渠道轮询签到。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	return fmt.Errorf("qwenwork 无签到活动（每日积分服务端自动发放，无需领取）")
}

// Classify 实现 provider.Upstream。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// Stream 实现 provider.Upstream（嵌套 SSE → 标准 OpenAI SSE 透传）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	return StreamCapture(w, r, model, nil)
}

// Aggregate 实现 provider.Upstream（嵌套 SSE 聚合）。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	return aggregate(r, model)
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

// hardMarkers 余额/权益不足关键词。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit", "credit is not enough",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
// 429 优先于 hardRule（限流 body 高频带 "quota exceeded"，见 AGENTS.md §6.15）。
func Classify(status int, body string) provider.ErrKind {
	if status == http.StatusPaymentRequired {
		return provider.ErrHardCredit
	}
	lower := strings.ToLower(body)
	if status == http.StatusUnauthorized {
		return provider.ErrSessionDead
	}
	if status == http.StatusForbidden && strings.Contains(lower, "signature invalid") {
		// COSY 签名失败通常是本地状态问题（token 过期/被轮换），按会话失效处理
		return provider.ErrSessionDead
	}
	// 429 优先于 hardRule
	if status == http.StatusTooManyRequests {
		return provider.ErrSoftRate
	}
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return provider.ErrHardCredit
		}
	}
	if strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "usage limit") || strings.Contains(lower, "请求过于频繁") {
		return provider.ErrSoftRate
	}
	if status == http.StatusNotFound {
		return provider.ErrNotFound
	}
	if status >= 500 {
		return provider.ErrServer
	}
	if status >= 400 {
		if strings.Contains(lower, "blocked by security policy") ||
			strings.Contains(lower, "content filter") ||
			strings.Contains(lower, "检测到敏感内容") {
			return provider.ErrContentBlocked
		}
		if strings.Contains(lower, "prompt is too long") || strings.Contains(lower, "context length") {
			return provider.ErrPromptTooLong
		}
		return provider.ErrClient
	}
	return provider.ErrNone
}

// clientUA 上游 UA（实测不校验，保留 qoderwork 形态以贴近桌面端）。
const clientUA = "qoderwork/0.1.8"
