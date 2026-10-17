// Package login_codearts 实现华为云 CodeArts Agent 的**新式 OAuth 登录**：
// PKCE(SHA-256) + 本机 127.0.0.1 回调 + DPoP 证明换 STS 临时凭据。
//
// 链路（协议 §2.1、§2.2）：
//  1. Start：生成 PKCE verifier/challenge、ES256(DPoP) 私钥、ticket_id，起本机回调
//     server（127.0.0.1 + 随机端口，**端口须 ≥10000**），返回授权页 URL：
//     `/portal/authorize?theme=2&locale=zh-cn&uri_scheme=codearts-agent&client_id=codearts-agent
//     &port=<port>&code_challenge=<>&code_challenge_method=SHA-256&ticket_id=<>
//     &plugin-name=snap_AIIDE&plugin-version=5.2.0`
//  2. 用户在系统浏览器完成登录，portal 跳回 `http://127.0.0.1:<port>/oauth/callback?code=…`；
//  3. Poll 拿到 code → `POST https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens`
//     （form + `DPoP: <jws>`）换临时 AK/SK/SecurityToken + refresh_token；
//  4. 调用方随后 SaveAuth 落盘 `auths/codearts-<uid>.json`（Extra 承载三件套与续期材料）。
//
// ⚠️ `code_challenge_method` 必须写**字面量 `SHA-256`**（不是 RFC 7636 的 `S256`）：
// 写错值 portal 会**静默回退**到旧 ticket 流程（协议 §2.2 末条）。旧流程本包**不实现**
// （新式是默认路径；旧凭据没有 refresh_token，不可静默续期）——这是本包的已知边界。
//
// 与 login_qwenwork / login_lobsterai 同形：ErrPending / Result / NewClient /
// Start / Poll / Shutdown / SaveAuth。
package login_codearts

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/codearts"
	"wild-work/internal/provider"
)

// flowTTL 一次登录流程的最长等待（回调 server 与被轮询状态的共同上限）。
//
// 协议未给该值；取 10 分钟（与 login_lobsterai 一致）：足够用户完成华为云登录，
// 又不至于让一个被放弃的回调口长期挂着。
const flowTTL = 10 * time.Minute

// ErrPending 表示授权尚未完成（用户还没在浏览器里登录完）。
// 与 login_trae / login_qwenwork / login_lobsterai 同名同义，app.pollLogin 用 errors.Is 判定。
var ErrPending = errors.New("login pending")

// stsURL 是 STS 端点（**默认取自 internal/codearts**，单一来源）。
//
// 独立成变量的唯一目的是让回归测试把它指向本机 httptest 服务。
// ⚠️ 严禁把它指向生产上游做探测（AGENTS.md §0.1）。
var stsURL = codearts.STSTokenURL

// Result 登录成功后拿到的凭据与账号信息。
//
// AccessToken 承载 **AK**（与渠道包/落盘约定一致：本渠道没有 Bearer token）。
// ⚠️ 但 **AK 不是账号唯一标识** —— 它是每次交换**新签发**的临时凭据；而上游又不下发任何身份字段，
// 所以「账号身份」只能由用户填的 Label 决定（留空时退回复用已有 UID），见 Label 与 SaveAuth。
// 其余渠道专有字段在 SaveAuth 里进 auth.Auth.Extra。
type Result struct {
	AccessToken  string
	RefreshToken string
	// ExpiresAt 是临时凭据到期时刻（**Unix 秒**）。
	ExpiresAt int64
	// ExpiresIn 是上游下发的原始秒数（0 = 未给），仅诊断用。
	ExpiresIn int64
	UID       string
	Nickname  string
	// Label 用户指定的「账号标识」（可选，由调用方在保存前填入）。
	//
	// 为什么需要它：上游不提供任何稳定身份字段（2026-10-17 实测：STS 响应与
	// statistics/plugin 的字段全集里都没有 user_id/domain_id），UID 只能落到临时 AK
	// ⇒ 既无法区分多个华为账号，重复登录又会被写成新条目。用户填的标识是唯一可靠的区分手段：
	// **填同一标识 = 同一账号（覆盖）；填不同标识 = 多个账号（并存）**。
	// 留空则退回「复用该渠道已有账号的 UID」（见 SaveAuth）。
	Label string

	// 签名三件套 + 续期材料（SaveAuth 落 Extra）。
	AccessKeyID     string
	SecretAccessKey string
	SecurityToken   string
	CodeVerifier    string
	DPoPPrivateJWK  string
	DomainID        string
	UserID          string
	UserName        string
	ProjectID       string
	Region          string

	Domain  string
	ApiHost string
}

// state 落在 app 的 login-state 文件里的一次性登录状态。
type state struct {
	Verifier     string `json:"verifier"`
	DPoPJWK      string `json:"dpop_jwk"`
	CallbackPort int    `json:"callback_port"`
	RedirectURI  string `json:"redirect_uri"`
	AuthURL      string `json:"auth_url"`
	TicketID     string `json:"ticket_id"`
	CreatedAt    int64  `json:"created_at"`
}

// session 是一次登录在内存里的运行时状态（回调 server + 待取授权码）。
type session struct {
	mu       sync.Mutex
	state    state
	code     string
	errMsg   string
	received bool
	srv      *http.Server
	ln       net.Listener
}

var (
	curMu sync.Mutex
	cur   *session
)

// NewClient 登录用 HTTP 客户端（30s 总超时兜底，与兄弟登录包同形）。
func NewClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// Start 发起登录：生成 PKCE/DPoP/ticket_id，起本机回调，返回用户要打开的授权页 URL。
func Start(_ *http.Client, statePath string) (string, error) {
	verifier, challenge, err := codearts.NewPKCE()
	if err != nil {
		return "", err
	}
	key, err := codearts.NewDPoPKey()
	if err != nil {
		return "", err
	}
	jwk, err := key.JWK()
	if err != nil {
		return "", err
	}

	ln, port, err := listenCallback()
	if err != nil {
		return "", err
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", port, codearts.OAuthCallbackPath)
	ticketID := codearts.UUID4()

	// 授权页参数照协议原样拼接（顺序也与协议表一致，便于对照排查）。
	authURL := fmt.Sprintf("%s?theme=2&locale=zh-cn&uri_scheme=%s&client_id=%s&port=%d&code_challenge=%s&code_challenge_method=%s&ticket_id=%s&plugin-name=%s&plugin-version=%s",
		codearts.PortalAuthorizeURL, codearts.URIScheme, codearts.OAuthClientID, port,
		challenge, codearts.CodeChallengeMethod, ticketID, codearts.PluginName, codearts.PluginVersion)

	st := state{
		Verifier:     verifier,
		DPoPJWK:      jwk,
		CallbackPort: port,
		RedirectURI:  redirectURI,
		AuthURL:      authURL,
		TicketID:     ticketID,
		CreatedAt:    time.Now().Unix(),
	}
	if err := writeState(statePath, st); err != nil {
		_ = ln.Close()
		return "", err
	}

	sess := &session{state: st}
	mux := http.NewServeMux()
	mux.HandleFunc(codearts.OAuthCallbackPath, sess.handleCallback)
	sess.ln = ln
	sess.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	replaceSession(sess)
	go func() { _ = sess.srv.Serve(ln) }()
	return authURL, nil
}

// Poll 检查一次登录结果：授权未完成返回 ErrPending；拿到 code 则**立即**换凭据。
func Poll(client *http.Client, statePath string) (Result, error) {
	st, err := readState(statePath)
	if err != nil {
		return Result{}, fmt.Errorf("read state: %w", err)
	}
	if st.CreatedAt > 0 && time.Since(time.Unix(st.CreatedAt, 0)) > flowTTL {
		Shutdown()
		_ = os.Remove(statePath)
		return Result{}, errors.New("登录超时（10 分钟未收到授权回调），请重新发起")
	}
	sess := currentSession()
	if sess == nil {
		return Result{}, ErrPending
	}
	code, errMsg, ok := sess.snapshot()
	if !ok {
		return Result{}, ErrPending
	}
	if errMsg != "" {
		// 用户在授权页拒绝 / 回调参数异常：终态，清掉会话与 state。
		Shutdown()
		_ = os.Remove(statePath)
		return Result{}, fmt.Errorf("登录失败: %s", errMsg)
	}

	res, err := exchange(client, st, code)
	if err != nil {
		var pe *provider.Error
		if errors.As(err, &pe) && pe.Kind == provider.ErrSessionDead {
			// 授权码已失效（超时/被用过）——重试同一个 code 没有意义，判终态。
			Shutdown()
			_ = os.Remove(statePath)
			return Result{}, err
		}
		// 其余（网络抖动/上游 5xx）：保留 state 与会话，让上层继续轮询。
		return Result{}, err
	}
	Shutdown()
	_ = os.Remove(statePath)
	return res, nil
}

// Shutdown 关闭本机回调 server（登录结束 / 超时 / 取消时调用；幂等）。
func Shutdown() {
	curMu.Lock()
	sess := cur
	cur = nil
	curMu.Unlock()
	if sess != nil && sess.srv != nil {
		_ = sess.srv.Close()
	}
}

// SaveAuth 以嵌套形原子写盘 `auths/codearts-<uid>.json`。
//
// 刻意**不手写 map**：构造 codearts.Account 后交给渠道包的 WriteAuthFile →
// auth.Auth.SaveAtomic()，保证落盘字节形态与 auth.Parse / auth.LoadCodeArtsDir /
// 续期回写**完全同源**（不变量 3，Extra 键名也只有一个来源）。
func SaveAuth(authDir string, r Result) (string, error) {
	if strings.TrimSpace(r.UID) == "" && strings.TrimSpace(r.Label) == "" {
		return "", errors.New("missing uid in result")
	}
	// ── UID 优先级（2026-10-17 起）──────────────────────────────────────────────
	//   1. **用户指定的 Label**（最可靠：直接表达「这是哪个账号」，也是多账号的唯一手段）；
	//   2. 上游稳定身份字段（当前上游不下发，留着以备将来）；
	//   3. **复用该渠道已有账号的 UID**（= 同一账号重新登录，覆盖同一文件）；
	//   4. 兜底 r.UID（= 临时 AK；仅当目录为空且未填 Label 时才会走到）。
	//
	// 背景：上游 STS 只回 `credentials.*` + `refresh_token`，**不含任何身份字段**（协议 §2.7；
	// 2026-10-17 抓包核对 statistics/plugin 的字段全集，亦无 user_id/domain_id）⇒ 直接用 AK
	// 会导致「点一次新增就多一个账号」（用户实测报告）；而**单纯**复用已有 UID 又让第二个华为账号
	// 无处安放（用户随后实测报告）。故引入 Label 由用户显式区分。
	if label := strings.TrimSpace(r.Label); label != "" {
		r.UID = label
	} else if strings.TrimSpace(r.UserID) == "" && strings.TrimSpace(r.DomainID) == "" {
		if uid := firstExistingUID(authDir); uid != "" {
			r.UID = uid
		}
	}
	if strings.TrimSpace(r.AccessKeyID) == "" || strings.TrimSpace(r.SecretAccessKey) == "" {
		// 防御：exchange 已硬校验，这里再挡一次——半成品凭据落盘后不可用且污染账号池。
		return "", errors.New("missing access_key_id/secret_access_key in result")
	}
	cred := codearts.Credential{
		AccessKeyID:     r.AccessKeyID,
		SecretAccessKey: r.SecretAccessKey,
		SecurityToken:   r.SecurityToken,
		ExpiresAt:       r.ExpiresAt,
		RefreshToken:    r.RefreshToken,
		DomainID:        r.DomainID,
		UserID:          r.UserID,
		UserName:        r.UserName,
	}
	ac := codearts.AccountFromCredential(r.UID, r.Nickname, cred, r.CodeVerifier, r.DPoPPrivateJWK)
	ac.ProjectID = r.ProjectID
	ac.Region = r.Region
	return codearts.WriteAuthFile(authDir, ac)
}

// firstExistingUID 返回该渠道**已落盘账号的第一个 UID**（按文件名序，确定性）。
//
// 无账号 / 读取失败 / UID 全空时返回空串——调用方退回新 UID（与旧行为一致，不会因此失败）。
// 用途见 SaveAuth 的「方案 B」注释：上游没有稳定身份字段时用它避免重复条目。
func firstExistingUID(authDir string) string {
	auths, err := auth.LoadCodeArtsDir(authDir)
	if err != nil {
		return ""
	}
	for _, a := range auths {
		if a != nil {
			if uid := strings.TrimSpace(a.UID); uid != "" {
				return uid
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 交换凭据
// ---------------------------------------------------------------------------

// exchange 用授权码换临时凭据（DPoP + PKCE）。
func exchange(client *http.Client, st state, code string) (Result, error) {
	if client == nil {
		client = NewClient()
	}
	key, err := codearts.ParseDPoPKey(st.DPoPJWK)
	if err != nil {
		return Result{}, fmt.Errorf("DPoP 私钥不可用: %w", err)
	}
	now := time.Now()
	cred, err := codearts.ExchangeAuthCode(client, stsURL, code, st.Verifier, st.RedirectURI, key, now)
	if err != nil {
		return Result{}, err
	}
	res := Result{
		AccessToken:     cred.AccessKeyID, // AK 即本渠道的"访问凭据"
		RefreshToken:    cred.RefreshToken,
		ExpiresAt:       cred.ExpiresAt,
		ExpiresIn:       cred.ExpiresAt - now.Unix(),
		AccessKeyID:     cred.AccessKeyID,
		SecretAccessKey: cred.SecretAccessKey,
		SecurityToken:   cred.SecurityToken,
		CodeVerifier:    st.Verifier,
		DPoPPrivateJWK:  st.DPoPJWK,
		DomainID:        cred.DomainID,
		UserID:          cred.UserID,
		UserName:        cred.UserName,
		Domain:          "codearts.huaweicloud.com",
		ApiHost:         codearts.APIBase,
	}
	res.UID = firstNonEmpty(res.UserID, res.DomainID, res.AccessKeyID)
	res.Nickname = firstNonEmpty(res.UserName, shortLabel(res.AccessKeyID))
	return res, nil
}

// ---------------------------------------------------------------------------
// 回调 server
// ---------------------------------------------------------------------------

// handleCallback 收授权码（或用户的拒绝）。
func (s *session) handleCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	q := r.URL.Query()
	if e := strings.TrimSpace(q.Get("error")); e != "" {
		desc := firstNonEmpty(q.Get("error_description"), e)
		s.fail(desc)
		_, _ = io.WriteString(w, callbackPage(false, desc))
		return
	}
	code := strings.TrimSpace(q.Get("code"))
	if code == "" {
		s.fail("回调缺少 code 参数")
		_, _ = io.WriteString(w, callbackPage(false, "回调缺少 code 参数"))
		return
	}
	s.setCode(code)
	// 此时尚未完成兑换（Poll 每 2s 轮询兑换），页面文案用「已收到授权」而非「登录成功」，
	// 避免兑换失败时误导（与 login_qwenwork 同款处理）。
	_, _ = io.WriteString(w, callbackPage(true, ""))
}

// setCode 记录授权码。
func (s *session) setCode(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.code, s.received = code, true
}

// fail 记录终态错误。
func (s *session) fail(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errMsg, s.received = msg, true
}

// snapshot 取当前结论（received=false 表示还没收到任何回调）。
func (s *session) snapshot() (code, errMsg string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code, s.errMsg, s.received
}

// replaceSession 替换当前会话（同一时刻只允许一条登录流程；顺带关掉上一轮的回调口）。
func replaceSession(sess *session) {
	curMu.Lock()
	old := cur
	cur = sess
	curMu.Unlock()
	if old != nil && old.srv != nil {
		_ = old.srv.Close()
	}
}

// currentSession 取当前会话。
func currentSession() *session {
	curMu.Lock()
	defer curMu.Unlock()
	return cur
}

// listenCallback 在 127.0.0.1 上开一个 **≥10000** 的回调端口（协议 §2.1）。
//
// 为什么不用 `:0`：OS 分配的临时端口不保证 ≥10000，而 portal 明确要求端口 ≥10000。
// 故先在 [10000, 65000] 里随机试，失败才退到 `:0` 并校验下限。
func listenCallback() (net.Listener, int, error) {
	for i := 0; i < 20; i++ {
		port := randomPort()
		if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
			return ln, port, nil
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, fmt.Errorf("listen callback: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if port < codearts.MinCallbackPort {
		_ = ln.Close()
		return nil, 0, fmt.Errorf("拿不到 ≥%d 的回调端口（实际 %d）", codearts.MinCallbackPort, port)
	}
	return ln, port, nil
}

// randomPort 返回 [MinCallbackPort, MaxCallbackPort) 内的随机端口（用 crypto/rand，
// 避免引入 math/rand 的全局种子状态）。
func randomPort() int {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return codearts.MinCallbackPort
	}
	span := uint32(codearts.MaxCallbackPort - codearts.MinCallbackPort)
	return codearts.MinCallbackPort + int(binary.BigEndian.Uint32(b[:])%span)
}

// ---------------------------------------------------------------------------
// 状态文件与文案
// ---------------------------------------------------------------------------

func readState(path string) (state, error) {
	var st state
	raw, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(raw, &st)
}

func writeState(path string, st state) error {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// callbackPage 极简回调结果页（不引外部资源；1.5s 自动关闭）。
func callbackPage(ok bool, detail string) string {
	title := "登录成功"
	color := "#12b76a"
	if !ok {
		title = "登录失败"
		color = "#f04438"
	}
	extra := ""
	if detail != "" {
		extra = `<p class="d">` + htmlEscape(detail) + `</p>`
	}
	return `<!doctype html><html lang="zh-cn"><head><meta charset="utf-8">` +
		`<title>` + title + `</title><style>body{font-family:system-ui,sans-serif;margin:40px;color:#344054}` +
		`h1{font-size:20px;color:` + color + `}.d{color:#667085}small{color:#98a2b3}</style></head><body>` +
		`<h1>` + title + `</h1>` + extra +
		`<p><small>可以关闭本页面返回 wild-work。</small></p>` +
		`<script>setTimeout(function(){window.close()},1500)</script></body></html>`
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;").Replace(s)
}

// firstNonEmpty 返回第一个非空白串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// shortLabel AK 前 8 位（上游不下发昵称时的展示兜底）。
func shortLabel(ak string) string {
	s := strings.TrimSpace(ak)
	if s == "" {
		return "codearts"
	}
	if len(s) > 8 {
		return "codearts-" + s[:8]
	}
	return "codearts-" + s
}
