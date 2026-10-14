package cline

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/backuplink"
	"wild-work/internal/provider"
)

// refreshSkew 是「提前多久认为 access token 需要刷新」。
// 实测上游有效期约 1h，取 5 分钟余量：既避免请求打到临界点，
// 也不至于把还没过期的 token 频繁换掉（换 token 是实打实的上游往返）。
const refreshSkew = 5 * time.Minute

// Client 实现 provider.Upstream。
type Client struct {
	HTTP *http.Client
	// StreamHTTP 专供对话流：**不设 Client.Timeout**（R35）。
	// Go 的 Client.Timeout 覆盖整个请求生命周期（含读 body），对 SSE 意味着
	// 「长回答必被从中间掐断」。空闲兜底改由 IdleReader 承担。
	StreamHTTP *http.Client
	// IdleTimeout 流式空闲上限，0 = DefaultIdleTimeout。
	// ⚠️ 目前**没有配置项**：config.Upstream 只有 timeout_seconds，main 也不注入，
	// 实际生效值恒为 DefaultIdleTimeout（旧注释承诺的 stream_idle_seconds 从未实现）。
	IdleTimeout time.Duration
	// Base 网关基址（默认 APIBase）；测试注入 httptest 假上游用，生产不设。
	Base string

	knownMu sync.RWMutex
	// knownIDs 是已知模型集（ID → 展示名，来自 free[]），也是 ChatStream 的本地校验集。
	// 冷启动（未拉到目录）时校验放行 —— 见 models.go 的 knownModel 注释。
	knownIDs map[string]string
	capMu    sync.RWMutex
	capped   map[string]time.Time
	// routes 是「地区限制→改走备用上游通道」的学习路由（实现在 backuplink.Routes，
	// 与 oczen 共用同一份编排：见 internal/backuplink/route.go）。
	routes backuplink.Routes

	// refreshMu 把「同一账号的并发换 token」收敛成一次网络调用。
	// 理由与第三方网关的 single-flight 相同：并发刷新会互相作废（后发的覆盖先发的），
	// 而这里更糟——每次换 token 都实打实打上游，重复调用纯属浪费且会触发风控。
	refreshMu  sync.Mutex
	refreshing map[string]*sync.Mutex // uid → 该账号的刷新锁
}

// New 构造客户端（非流式 120s 总超时；一问一答需要兜底，故保留 Timeout）。
func New() *Client {
	tr := newTransport()
	return &Client{
		HTTP:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr},
		knownIDs:   map[string]string{},
		capped:     map[string]time.Time{},
	}
}

// newTransport 出厂 Transport：禁 h2 + ResponseHeaderTimeout 兜底。
// StreamHTTP 无总超时后**必须**有 ResponseHeaderTimeout，否则连响应头都等不到会无限挂。
func newTransport() *http.Transport {
	return &http.Transport{
		DisableKeepAlives:     false,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 90 * time.Second,
		DisableCompression:    false,
	}
}

func (c *Client) base() string {
	if b := strings.TrimRight(strings.TrimSpace(c.Base), "/"); b != "" {
		return b
	}
	return defaultBaseURL
}

func (c *Client) streamClient() *http.Client {
	if c.StreamHTTP == nil {
		c.StreamHTTP = &http.Client{Transport: newTransport()}
	}
	return c.StreamHTTP
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 120 * time.Second, Transport: newTransport()}
	}
	return c.HTTP
}

func (c *Client) idleTimeout() time.Duration {
	if c.IdleTimeout <= 0 {
		return DefaultIdleTimeout
	}
	return c.IdleTimeout
}

// applyHeaders 统一出站头。
//
// ★ X-CLIENT-TYPE 必须带且必须非空：缺这个头，所有 `cline-free/*` 一律 403
//
//	API_REQUEST_ERROR_CODE，且**症状是「免费模型全部消失」而不报任何配置错误**。
//	值本身不参与判定（实测 bogus-value 同样放行），故固定写死，勿当优化点删除。
//
// token 由调用方经 Authorization() 产出：**已含 `workos:` 前缀与 `Bearer `**。
// 见 authHeader 注释说明为什么前缀不能省。
func applyHeaders(req *http.Request, authorization string) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set(HeaderClientType, HeaderClientTypeValue)
	if authorization != "" {
		req.Header.Set(HeaderAuth, authorization)
	}
}

// authHeader 把裸 access token 变成可直接写进 Authorization 头的值。
//
// ★ `workos:` 前缀**必须带**（2026-10-14 逐项实测）：
//
//	裸 token      → 401 Unauthorized（"re-authenticate your Cline account"）
//	workos:<tok>  → 200
//
// 这与官方 SDK 的 WORKOS_TOKEN_PREFIX 一致；Key 模式（sk_…）则是裸 Bearer，
// 故这里按前缀形态分支，不能统一加。
func authHeader(rawToken string) string {
	t := strings.TrimSpace(rawToken)
	if t == "" {
		return ""
	}
	if strings.HasPrefix(t, "sk_") {
		return "Bearer " + t
	}
	if strings.HasPrefix(strings.ToLower(t), workosPrefix) {
		return "Bearer " + t
	}
	return "Bearer " + workosPrefix + t
}

// ---------------------------------------------------------------------------
// 凭据形态归一：把「Key」与「refreshToken」都收敛成一个可用的 access token
// ---------------------------------------------------------------------------

// isKeyMode 报告该凭据是官方 API Key（sk_…）形态 —— 它本身就是 token，无需刷新。
func isKeyMode(a *auth.Auth) bool {
	if a == nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(a.AccessTokenValue()), "sk_")
}

// lockFor 取该账号的刷新锁（懒建）。
func (c *Client) lockFor(uid string) *sync.Mutex {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if c.refreshing == nil {
		c.refreshing = map[string]*sync.Mutex{}
	}
	m, ok := c.refreshing[uid]
	if !ok {
		m = &sync.Mutex{}
		c.refreshing[uid] = m
	}
	return m
}

// ensureAccessToken 返回一个当前有效的裸 access token（**不含** Bearer / workos: 前缀）。
//
// 两种形态：
//   - Key 模式：AccessToken 就是 token，原样返回。
//   - refreshToken 模式：AccessToken 为空或将于 refreshSkew 内过期 ⇒ 换一次。
//     并发调用由 per-uid 锁收敛成一次网络往返（后到者进来时前者的结果已写回，直接命中缓存）。
//
// 换失败时返回错误，由调用方决定归类（ChatStream 走 Classify，probe 直接报错）。
func (c *Client) ensureAccessToken(a *auth.Auth) (string, error) {
	if a == nil {
		return "", fmt.Errorf("cline: nil credential")
	}
	if isKeyMode(a) {
		return strings.TrimSpace(a.AccessTokenValue()), nil
	}
	if strings.TrimSpace(a.RefreshTokenValue()) == "" {
		return "", fmt.Errorf("cline: 凭据既无 sk_ Key 也无 refreshToken")
	}
	// 快路径：现成 token 且未临近过期。
	if tok := strings.TrimSpace(a.AccessTokenValue()); tok != "" && !a.NeedsRefresh(refreshSkew) {
		return tok, nil
	}

	lock := c.lockFor(a.UID)
	lock.Lock()
	defer lock.Unlock()

	// 双重检查：等锁期间可能已被其它 goroutine 刷好。
	if tok := strings.TrimSpace(a.AccessTokenValue()); tok != "" && !a.NeedsRefresh(refreshSkew) {
		return tok, nil
	}
	if err := c.RefreshToken(a); err != nil {
		return "", err
	}
	return strings.TrimSpace(a.AccessTokenValue()), nil
}

// RefreshToken 用 refreshToken 换 accessToken（provider.Upstream 接口实现）。
//
// Key 模式（sk_…）**不需要**刷新：Key 不过期、不轮换，直接返回 nil
// ——这样调度器/面板的「刷新」动作对两种形态都安全（幂等）。
//
// refreshToken 模式的实测契约（2026-10-14，逐项对照）：
//   - `POST /auth/refresh`，body `{"refreshToken":"…","grantType":"refresh_token"}`
//   - **必须带全套客户端指纹头**（applyHeaders 只会设 Accept + X-CLIENT-TYPE；
//     鉴权类的头这里要自己补齐），否则 401。
//   - 响应 `{success, data:{accessToken, refreshToken?, expiresAt, userInfo}}`；
//     `expiresAt` 是 ISO8601 字符串（实测 `2026-10-14T13:29:11Z`）。
//   - 实测本批 refreshToken **不轮换**（返回同值），但代码按「可能轮换」写：
//     返回值非空且不同就写回，避免上游哪天改成轮换后旧 token 失效。
//
// 失败分类：网络错误/5xx/429 属 transient，**不动**凭据（保留旧 token 继续可用）；
// 400/401/403 属 invalid_grant ⇒ 返回明确错误，由上层决定是否禁用该号。
func (c *Client) RefreshToken(a *auth.Auth) error {
	if a == nil {
		return fmt.Errorf("cline: nil credential")
	}
	if isKeyMode(a) {
		return nil // Key 模式无刷新概念
	}
	rt := strings.TrimSpace(a.RefreshTokenValue())
	if rt == "" {
		return fmt.Errorf("no refreshToken")
	}

	body, _ := json.Marshal(map[string]string{
		"refreshToken": rt,
		"grantType":    "refresh_token",
	})
	req, err := http.NewRequest(http.MethodPost, c.base()+EpRefresh, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	applyHeaders(req, "") // 刷新调用本身不带 Authorization
	resp, err := c.httpClient().Do(req)
	if err != nil {
		// 网络层错误：transient，保留旧凭据
		log.Printf("cline refresh transient uid=%s err=%v", a.UID, err)
		return fmt.Errorf("refresh_request_failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))

	if resp.StatusCode != http.StatusOK {
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
			resp.StatusCode == http.StatusBadRequest:
			// 刷新凭据本身失效 ⇒ 该号需要重新导入，不能自愈。
			log.Printf("cline refresh invalid_grant uid=%s status=%d", a.UID, resp.StatusCode)
			return fmt.Errorf("refresh_invalid_grant: HTTP %d %s", resp.StatusCode, truncate(string(raw), 240))
		default:
			log.Printf("cline refresh transient uid=%s status=%d", a.UID, resp.StatusCode)
			return fmt.Errorf("refresh_transient: HTTP %d %s", resp.StatusCode, truncate(string(raw), 200))
		}
	}

	var r refreshResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("refresh_parse_error: %v", err)
	}
	if !r.Success || strings.TrimSpace(r.Data.AccessToken) == "" {
		return fmt.Errorf("refresh_no_token: %s", truncate(string(raw), 200))
	}

	a.Lock()
	a.AccessToken = strings.TrimSpace(r.Data.AccessToken)
	// 轮换兼容：上游返回了不同的 refreshToken 才写回。
	if nt := strings.TrimSpace(r.Data.RefreshToken); nt != "" && nt != rt {
		a.RefreshToken = nt
	}
	if exp := parseExpiry(r.Data.ExpiresAt); exp > 0 {
		a.ExpiresAt = exp
	} else {
		// 上游没给 expiresAt 时兜底：实测有效期约 1h，取 50 分钟以留余量。
		a.ExpiresAt = time.Now().Add(50 * time.Minute).Unix()
	}
	if nm := strings.TrimSpace(r.Data.UserInfo.ClinetUserID); nm != "" && a.UID == "" {
		a.UID = nm
	}
	a.Unlock()

	// 立刻落盘：进程重启后不必再换一次（也避免旧 refreshToken 若被轮换后丢失）。
	if a.FilePath != "" {
		if err := a.SaveAtomic(); err != nil {
			log.Printf("cline refresh save failed uid=%s err=%v", a.UID, err)
		}
	}
	return nil
}

// refreshResp 是 /auth/refresh 的响应（字段按实测响应体取）。
type refreshResp struct {
	Success bool `json:"success"`
	Data    struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresAt    string `json:"expiresAt"`
		UserInfo     struct {
			ClinetUserID string `json:"clineUserId"`
			Email        string `json:"email"`
		} `json:"userInfo"`
	} `json:"data"`
}

// parseExpiry 把上游的 expiresAt 归一成 Unix 秒。
// 实测是 ISO8601 字符串（`2026-10-14T13:29:11Z`）；也兼容纯数字（秒或毫秒）。
func parseExpiry(v string) int64 {
	s := strings.TrimSpace(v)
	if s == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix()
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e12 { // 毫秒
			return n / 1000
		}
		return n
	}
	return 0
}

// ---------------------------------------------------------------------------
// 对话
// ---------------------------------------------------------------------------

// ChatStream 实现 provider.Upstream：返回上游流（200）或已读尽的错误响应。
//
// 状态码会经 effectiveStatus **还原被网关压平的内层码**（实测外层恒 500），
// 让客户端与 handler 看到真正的 429/401/404；响应体保持原文（不变量 8 的偏离已在注释说明）。
//
// 地区限制自动改道（决议 R57）：直连命中 IsRegionBlocked ⇒ 同一次请求内经**备用上游通道**
// 重放一次；未配置备用通道则原样透传失败。重试全部在本方法内完成，handler/账号惩罚/
// 粘性路由对该机制无感（备用链路只是换出口 IP，不是换账号）。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	m := modelOf(body)
	if m == "" {
		return nil, http.StatusBadRequest, []byte(`{"error":{"message":"model is required"}}`), nil
	}
	// 本地校验：集合 == 暴露集 == free[]（构造即一致）；目录未拉到过时放行（fail-open）。
	// R42 的教训：校验过严会误拒上游新模型，故冷启动不拦、集合跟随上游同步。
	if !c.knownModel(m) {
		return nil, http.StatusNotFound,
			[]byte(fmt.Sprintf(`{"error":{"message":"model not found in cline free catalog: %s",
			"code":"model_not_found"}}`, jsonEscape(m))), nil
	}

	out := clampMaxTokens(body)
	out = forceStream(out)

	// 凭据归一：Key 模式直接用，refreshToken 模式按需换（带 single-flight）。
	// 换失败按上游错误口径返回，让 handler 的冷却/禁用状态机照常运转。
	rawTok, err := c.ensureAccessToken(a)
	if err != nil {
		return nil, http.StatusUnauthorized,
			[]byte(fmt.Sprintf(`{"error":{"message":%s,"code":"credential_unavailable"}}`,
				jsonEscape(err.Error()))), nil
	}
	authorization := authHeader(rawTok)

	// 编排抽到 backuplink.Replay（R62）：渠道只提供「怎么发一发」与「什么算地区限制」，
	// 其余语义（首因不被掩盖、备用拨不动则回落直连、学习路由随配置作废）全渠道共用一份，
	// 避免 cline/oczen 各写一遍后改一处漏一处（R52 的教训）。
	rp := &backuplink.Replay{
		Kind:   "cline",
		Model:  m,
		Routes: &c.routes,
		Attempt: func(useBackup bool) (*http.Response, error) {
			return c.attempt(out, authorization, useBackup)
		},
		Blocked:   IsRegionBlocked,
		Wrap:      func(resp *http.Response) io.ReadCloser { return c.openStream(resp) },
		Finalize:  func(_ int, body string) { c.noteCap(m, body) },
		Translate: effectiveStatus,
	}
	rc, status, raw, err := rp.Do()
	if err != nil || rc != nil || status == http.StatusOK {
		return rc, status, raw, err
	}
	return nil, status, raw, nil
}

// noteCap 命中共享出口上限文案时记模型级冷却（不罚账号）。
func (c *Client) noteCap(m, body string) {
	if IsCapBody(body) {
		c.markCapped(m, capCooldown)
		log.Printf("cline: 模型触上限 model=%s（不罚账号，已标记冷却）", m)
	}
}

// attempt 发一发出站对话请求：useBackup 决定出口（直连 or 备用上游通道）。
// 重放时重新构造请求（body 已在内存，无需 GetBody），两个出口共用同一构造器
// ⇒ 头完全一致：X-CLIENT-TYPE 缺失会恒回 403，重放少了它会把「代理也没用」误判成代理故障。
//
// authorization 是已拼好的头值（含 `Bearer ` 与 `workos:` 前缀）——由 ChatStream 在
// 进重放编排**之前**解析一次，避免重放时二次刷新（两次 attempt 必须用同一个 token）。
func (c *Client) attempt(out []byte, authorization string, useBackup bool) (*http.Response, error) {
	cli := c.streamClient()
	if useBackup {
		bc := c.backupClient()
		if bc == nil {
			return nil, backuplink.ErrNoBackup
		}
		cli = bc
	}
	req, err := http.NewRequest(http.MethodPost, c.base()+EpChat, bytes.NewReader(out))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyHeaders(req, authorization)
	return cli.Do(req)
}

// openStream 把 200 响应包成带空闲兜底的流（R35：不用 Client.Timeout，靠 IdleReader）。
func (c *Client) openStream(resp *http.Response) io.ReadCloser {
	return provider.NewIdleReader(resp.Body, c.idleTimeout())
}

// backupClient 返回绑定备用上游通道的 client（未配置返回 nil = 只能直连）。
// base 取流式 client 的 Transport：只改出口，其余超时/协议参数与出厂一致。
func (c *Client) backupClient() *http.Client {
	tr, _ := c.streamClient().Transport.(*http.Transport)
	return backuplink.Client(tr)
}

// regionLearned 报告该模型是否已学到「直连被地区拒、只能走备用」（测试与日志用）。
func (c *Client) regionLearned(m string) bool { return c.routes.Learned(m) }

// forceStream 强制上游 stream:true（不变量 1）。
//
// **为什么必须有**（2026-10-12 实测踩坑）：本渠道把客户端 body 原样转发，而 handler 对上游
// 响应**一律按 SSE 解析** —— 流式走 Stream、非流式客户端走 Aggregate，二者都只认 `data:` 帧。
// 客户端发 `stream:false`（或干脆不带 stream）时，上游会回**非流式 JSON 信封**，解析器找不到
// 任何 data 帧 ⇒ `errEmptyStream` ⇒ 502 `upstream_parse`，且该错误被当作账号连续错误
// ⇒ 冷却整条渠道（实测：一次非流式请求后，四个免费模型全部 503 no_healthy_account 约 60s）。
//
// 讽刺的是 sse.go 的 Aggregate 注释早已假定「本渠道永远请求 stream=true（不变量 1）」，
// 但 ChatStream 此前只做了 clampMaxTokens，并未落实该假定 —— 这就是 bug 本身。
// 非流式客户端由 handler 本地聚合（Aggregate），与 oczen/monkeycode 同款做法。
func forceStream(body []byte) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	if v, _ := obj["stream"].(bool); v {
		return body // 已是 true，零改动原样返回（不重新 marshal，避免无谓的键序变化）
	}
	obj["stream"] = true
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// clampMaxTokens 治理输出上限（实测：8→上游 400；16→500 empty response content；
// 64~200000 全通；1e6→拒）。**客户端未给时一律不传** —— 不传最稳，实测可自由跑 3242 token
// 且正常出内容；强行补一个小值反而会制造「空回答」。
func clampMaxTokens(body []byte) []byte {
	var obj map[string]any
	if json.Unmarshal(body, &obj) != nil {
		return body
	}
	if v, ok := obj["max_tokens"]; ok {
		n := toInt(v)
		switch {
		case n <= 0:
			delete(obj, "max_tokens") // 0/负数/非数值：视同未给
		case n < minUpstreamMaxTokens:
			obj["max_tokens"] = minUpstreamMaxTokens
		case n > maxUpstreamMaxTokens:
			obj["max_tokens"] = maxUpstreamMaxTokens
		}
	}
	// 同步治理 OpenAI 新字段名（上游只认 max_tokens；带 max_completion_tokens 时按同规则夹）
	if v, ok := obj["max_completion_tokens"]; ok {
		n := toInt(v)
		if n > 0 {
			if n < minUpstreamMaxTokens {
				n = minUpstreamMaxTokens
			}
			if n > maxUpstreamMaxTokens {
				n = maxUpstreamMaxTokens
			}
			obj["max_completion_tokens"] = n
		} else {
			delete(obj, "max_completion_tokens")
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// ---------------------------------------------------------------------------
// 凭据探针（面板添加 Key 用：零对话成本，只读自己的 Key 列表）
// ---------------------------------------------------------------------------

type apiKeyInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	KeyPrefix string `json:"key_prefix"`
}

type apiKeysResp struct {
	Data struct {
		Keys []apiKeyInfo `json:"keys"`
	} `json:"data"`
	Success bool `json:"success"`
}

// ProbeResult 是 ValidateKey 的产出。
type ProbeResult struct {
	UID         string   // 稳定的 apk- id（Key 模式）或 usr- id（refreshToken 模式）；取不到时退化为 sha256 前缀
	KeyPrefix   string   // 本 Key 的官方回显前缀（仅 Key 模式）
	SiblingKeys []string // 同一账号下的其它 Key 前缀（用于「同账号不扩额度」告警；仅 Key 模式）
	AccountName string   // 展示名（Key 的 name / 账号邮箱）
	// Kind 报告这次探针用的是哪种凭据：`key` 或 `refresh`。
	// 调用方据此决定落盘形态（AccessToken vs RefreshToken）与过期策略。
	Kind string
	// ExpiresAt 仅 refresh 模式有意义（换到的 access token 过期时刻，Unix 秒）。
	ExpiresAt int64
}

// 凭据形态常量（ProbeResult.Kind 的取值）。
const (
	CredKindKey     = "key"
	CredKindRefresh = "refresh"
)

// ValidateKey 校验 API Key 并取回身份（面板添加账号入口，见 app.SubmitClineKey）。
//
// 只调 GET /api/v1/api-keys —— **零 token 消耗、零副作用**，避免「添加账号即烧额度」。
// uid 取该端点里 key_prefix 与本 Key 相符的那条 id（实测 key_prefix == key 前 10 字符）。
// 同账号判定：该端点返回**本账号全部 Key**，故能列出兄弟 Key 的前缀 ——
// 由于配额键名是 `…UsageCostUSDPerUser`（按账号计），同账号多 Key **不会**扩额度，调用方据此告警。
func ValidateKey(key string) (*ProbeResult, error) { return New().probeKey(key) }

// ValidateRefreshToken 校验 refreshToken 并取回身份（面板「粘贴 refreshToken」入口）。
//
// 走 **换一次 token**：POST /auth/refresh，成功即证明凭据有效，同时拿到 clineUserId 作 uid。
// 与 Key 模式的区别：这里有真实的 token 消耗（一次换 token，不消耗对话额度）。
// 失败分类同 RefreshToken：invalid_grant ⇒ 凭据失效；transient ⇒ 稍后重试。
func ValidateRefreshToken(refreshToken string) (*ProbeResult, error) {
	return New().probeRefreshToken(refreshToken)
}

// probeRefreshToken 是 ValidateRefreshToken 的可注入实现。
func (c *Client) probeRefreshToken(refreshToken string) (*ProbeResult, error) {
	rt := strings.TrimSpace(refreshToken)
	if rt == "" {
		return nil, fmt.Errorf("refreshToken 不能为空")
	}
	// 用一个临时 Auth 走同一份刷新实现，保证「面板验证」与「运行时刷新」行为完全一致。
	a := &auth.Auth{Kind: "cline", RefreshToken: rt}
	if err := c.RefreshToken(a); err != nil {
		return nil, err
	}
	uid := strings.TrimSpace(a.UID)
	if uid == "" {
		// 上游没回 userInfo.clineUserId ⇒ 用 hash 兜底（稳定且不含明文）
		sum := sha256.Sum256([]byte(rt))
		uid = "r" + hex.EncodeToString(sum[:])[:16]
	}
	return &ProbeResult{
		UID:         uid,
		Kind:        CredKindRefresh,
		AccountName: "refreshToken " + rt[:min(len(rt), 8)] + "…",
		ExpiresAt:   a.ExpiresAt,
	}, nil
}

// probeKey 是 ValidateKey 的可注入实现（Base 指向 httptest 假上游即可单测）。
func (c *Client) probeKey(key string) (*ProbeResult, error) {
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, "sk_") {
		return nil, fmt.Errorf("Cline API Key 应以 sk_ 开头（在 app.cline.bot → Account / API Keys 创建）")
	}
	req, err := http.NewRequest(http.MethodGet, c.base()+EpAPIKeys, nil)
	if err != nil {
		return nil, err
	}
	applyHeaders(req, authHeader(key))
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 128<<10))
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("Key 无效或被拒（HTTP %d）：%s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		return nil, fmt.Errorf("校验 Key 失败 HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var r apiKeysResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("api-keys 响应无法解析：%v", err)
	}
	prefix := key
	if len(prefix) > KeyPrefixLen {
		prefix = prefix[:KeyPrefixLen]
	}
	res := &ProbeResult{KeyPrefix: prefix, Kind: CredKindKey}
	for _, k := range r.Data.Keys {
		kp := strings.TrimSpace(k.KeyPrefix)
		if kp == "" {
			continue
		}
		if kp == prefix {
			res.UID = strings.TrimSpace(k.ID)
			res.AccountName = strings.TrimSpace(k.Name)
			continue
		}
		res.SiblingKeys = append(res.SiblingKeys, kp)
	}
	if res.UID == "" {
		// 列表里没有自己（上游形态变化）⇒ 用 hash 兜底，仍保证稳定且不含明文。
		sum := sha256.Sum256([]byte(key))
		res.UID = "k" + hex.EncodeToString(sum[:])[:16]
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// Upstream 其余方法
// ---------------------------------------------------------------------------

// RefreshToken 见文件上方的实现（支持 Key / refreshToken 两种凭据形态）。
// 不变量 20（refresh 后必须 SaveAtomic）在该实现内已满足。

// UserResource 恒 0 且**不得返回 error**：上游确无额度/余额端点
// （/credits /usage /billing /account /quota /me 等 12 个候选实测全 404）。
// 返回 error 会让面板与池逻辑报错，而"0"才是这里的正确事实。
func (c *Client) UserResource(a *auth.Auth) (int64, error) { return 0, nil }

// UserResourceDetail 同上：可消耗余额 0、无明细。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}

// DailyCheckin 无签到活动。调度器侧 CheckinHours 传 []int{}（R39），
// 且 noExplicitCheckin 会挡掉面板手工签到，故本方法正常情况下不会被调用。
func (c *Client) DailyCheckin(a *auth.Auth) error { return fmt.Errorf("cline 无签到活动") }

// FetchModelPricing 上游无费率端点；单价只在逐请求 usage 里（cost/cost_details），
// 由 ledger 记账，不进费率表。返回空不报错（面板据此显示「不适用」）。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}

// Classify 实现 provider.Upstream。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

var modelRE = regexp.MustCompile(`"model"\s*:\s*"([^"]+)"`)

// modelOf 从请求体里取模型名（不做完整反序列化，省一次分配）。
func modelOf(body []byte) string {
	if m := modelRE.FindSubmatch(body); len(m) == 2 {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

func uidOf(a *auth.Auth) string {
	if a == nil {
		return "-"
	}
	return a.UID
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		var i int
		_, _ = fmt.Sscanf(n, "%d", &i)
		return i
	}
	return 0
}

func jsonEscape(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `"?"`
	}
	return string(b)
}

// getJSON 发一个 GET 并回原始 body（带 Bearer，虽然两个目录端点实测无需鉴权）。
//
// 用 ensureAccessToken 而非直接读 AccessToken：refreshToken 模式下首启时 token 还是空的，
// 直接发会拿 401（目录端点虽然不校验，但带上有效凭证更保险，也让「首次拉目录」顺带完成换 token）。
func (c *Client) getJSON(a *auth.Auth, path string) (json.RawMessage, error) {
	req, err := http.NewRequest(http.MethodGet, c.base()+path, nil)
	if err != nil {
		return nil, err
	}
	tok, _ := c.ensureAccessToken(a) // 失败时不阻断：目录端点本就无需鉴权
	applyHeaders(req, authHeader(tok))
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, &provider.Error{Kind: Classify(resp.StatusCode, string(raw)),
			Status: resp.StatusCode, Msg: truncate(string(raw), 240)}
	}
	return json.RawMessage(raw), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
