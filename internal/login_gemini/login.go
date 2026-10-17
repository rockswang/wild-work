// Package login_gemini 实现 Gemini（Google Code Assist / Cloud Code）的**本机 loopback
// 回调**登录：起本机 HTTP 回调服务器收 `code`，随后在令牌端点**带 client_secret** 兑换 token。
//
// 协议依据：事实文档 docs/ref-ofm-渠道-zcode-gemini.md §2.3 与参考实现 gemini-oauth.ts
// （行号在注释里逐条标注）。形态与 login_qwenwork / login_lobsterai 同形：
// `NewClient` / `Start` / `Poll` / `Shutdown` / `SaveAuth`。
//
// 链路：
//  1. Start：生成 `state = randomBytes(16).hex`，**先监听**再拼 URL（先拼会 redirect_uri_mismatch），
//     监听 `127.0.0.1` 与 `[::1]`（浏览器常把 localhost 解析成 IPv6）的**同一个动态端口**，
//     路径 `/oauth-callback`；把 state 落盘后**立即**返回授权 URL。
//     Start 不能阻塞在等回调上：前端 `window.open` 只在用户点击后的瞬态激活窗口内有效。
//  2. 用户在系统浏览器打开授权 URL 完成授权，浏览器被 302 回
//     `http://localhost:<port>/oauth-callback?code=…&state=…`。
//  3. 回调处理器**强制校验 state**（不匹配回 400 并拒绝，协议 §2.3），收到 code 后置入内存。
//  4. Poll：由 `app.pollLogin` 每 2s 调一次；拿到 code 后立即兑换 token，成功即删 state 文件并
//     返回 Result，由调用方 SaveAuth 落盘 `auths/gemini-<uid>.json`。
//
// 三条上游硬约束（gemini-oauth.ts:9-17，原版 oauth.go 实测踩过）：
//
//  1. **token 端点强制校验客户端身份**：只发 `client_id` 会回
//     `invalid_request: client_secret is missing`，症状是「浏览器显示授权成功，
//     但面板里账号一直不出现」⇒ 兑换表单**必须**带 `client_secret`；
//  2. **必须「先监听、再拼 URL」**：端口被占时会换端口；
//  3. **`state` 必须校验**：不匹配一律 400 拒绝（防伪造回调）。
//
// ⚠️ **无 PKCE**（刻意去掉，协议 §2.3：上游用最朴素的 authorization_code + client_secret）。
//
// 纪律：
//   - 主机 / client 凭据 / 回调路径的**单一来源是兄弟包 internal/gemini**（数据面的续期也用
//     同一份取值），本地绝不另写一份常量；
//   - 上游纪律（AGENTS.md §0.1）：本包只在用户主动发起登录后请求上游，每次登录最多一次
//     换码（+ 可能一次 userinfo 兜底）；
//   - 零 token 纪律（AGENTS.md §0.2）：错误文案**不回显响应体原文**——兑换响应体里就是
//     access/refresh token，而这些文案会经 app.pollLogin 进日志。
package login_gemini

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/gemini"
)

// 端点取值：**默认取自 internal/gemini**（单一来源），独立成变量的唯一目的是让回归测试
// 把它们指向本机 httptest 服务。
//
// ⚠️ 严禁把它们指向生产上游做探测（AGENTS.md §0.1）。
var (
	authEndpoint     = gemini.AuthEndpoint
	tokenEndpoint    = gemini.TokenEndpoint
	userInfoEndpoint = gemini.UserInfoEndpoint
)

// flowTTL 整条登录流程的上限：**6 分钟**（参考实现 authFlowTimeout = 360_000 ms）。
//
// ⚠️ 不是 3 分钟：Google 账号常带二次验证，3 分钟会把正常用户掐掉（面板亮「授权失败」，
// 其实人家只是慢）。与 app.pollLogin 的 5 分钟轮询窗口并存且口径不同——app 先到期只会
// 少等 1 分钟，不会让流程出错。
const flowTTL = 6 * time.Minute

// domainConst 写进 auth 文件的 domain 字段。本渠道的协议里**没有**该字段，它纯粹是本工具
// auth 格式的一部分（同 minimax-"minimax.cn" / lobsterai 的口径）。
const domainConst = "cloudcode-pa.googleapis.com"

// ErrPending 表示授权尚未完成（与 login_qwenwork / login_lobsterai 同名同义，
// app.pollLogin 用 errors.Is 判定）。
var ErrPending = errors.New("login pending")

// Result 登录成功后拿到的凭据与账号信息。
//
// 字段比"够用"多几个（Domain / ApiHost / Project / PaidTier*）：SaveAuth 要用它们构造
// auth.Auth 落盘，渠道侧的 project 探测则会在首次请求时把真值回写。
type Result struct {
	AccessToken  string
	RefreshToken string // Google 首次授权（access_type=offline + prompt=consent）必定下发
	// ExpiresAt 是 access token 的到期时刻（**Unix 秒**）。
	// 协议侧只有 `expires_in`（秒）可用：Google 的 access token 是 opaque token（`ya29.` 开头），
	// 不是 JWT，没有 `exp` 可读（故 auth.LoadGeminiDir 刻意不调 AdoptJWTExpiry）。
	ExpiresAt int64
	// ExpiresIn 上游下发的 `expires_in` 原文（秒，缺失时为兜底的 3600），仅诊断用。
	ExpiresIn int64
	// UID 是账号池去重/排序标识：OIDC 的 `sub`（稳定标识，邮箱可改、sub 不变）。
	UID string
	// Nickname 展示名：优先 email，其次 sub。
	Nickname string
	// Domain / ApiHost 写进 auth 文件（自描述字段，协议里没有）。
	Domain  string
	ApiHost string

	// ---- 以下字段是**渠道专有**凭据，落进 auth.Auth.Extra（键名见 internal/gemini）----
	//
	// Project 是 loadCodeAssist 探测到的 `cloudaicompanionProject`。登录环节**不探测**
	// （少发一次上游请求，AGENTS.md §0.1 同源纪律）——首轮对话由渠道侧探测并回写。
	Project       string
	PaidTierID    string
	PaidTierName  string
	PaidTierLabel string
}

// state 是落在 app.loginStateFP 里的一次性登录状态。
//
// 为什么落盘而不是只放内存：与 login_qwenwork / login_lobsterai 同形——app 的
// Start / Poll / CancelLogin 是三个独立入口（CancelLogin 直接删 state 文件即等于放弃），
// 且进程重启后仍能看清上一轮登录卡在哪一步。
type state struct {
	// State 是回调时**逐字比对**的随机串（randomBytes(16).hex，协议 §2.3）。
	State string `json:"state"`
	// RedirectURI / CallbackPort 仅诊断与 Poll 复用（redirect_uri 必须与授权请求逐字一致）。
	RedirectURI  string `json:"redirectUri"`
	CallbackPort int    `json:"callbackPort"`
	AuthURL      string `json:"authUrl"`
	// CreatedAt 是 Start 时刻（Unix 秒），用于 flowTTL 超时判定。
	CreatedAt int64 `json:"createdAt"`
}

// pendingCode 本机回调收到的授权码（内存通道）。
//
// received 的语义是「**已有结论**」而不是「有 code」：state 校验失败 / 用户拒绝都会把它置真
// （code 仍为空），Poll 见到即按终态报错——state 错的回调永远不可能变成对的回调，
// 没有理由让用户再等满 6 分钟。
type pendingCode struct {
	code     string
	errMsg   string
	received bool
}

// loginSession 一次登录在内存里的运行时状态。
// 全部字段由 mu 保护：回调 handler 在 server goroutine 里写，Poll / Shutdown 在主流程读。
type loginSession struct {
	mu       sync.Mutex
	state    state
	pending  pendingCode
	srv      *http.Server
	lns      []net.Listener
	shutdown bool
}

var (
	// curMu 保护 cur（仅 Start / Shutdown / Poll 的入口临界区）。
	curMu sync.Mutex
	cur   *loginSession
)

// NewClient 返回登录用 HTTP 客户端（与其它 login_* 同形：30s 总超时兜底）。
func NewClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// ErrOAuthClientMissing 未获取到 Google OAuth 客户端凭据。
//
// 2026-10-17 起源码**不再内嵌**那两枚**公开**凭据（GitHub 的 Secret Scanning Push Protection
// 会把它们判为 secret 并拒绝推送整个分支，见 internal/gemini/oauthclient.go 的说明），
// 因此只能来自环境变量，或从本机安装的官方 CLI 包内提取。
//
// ⚠️ 这条文案是**用户唯一能看到的解释**：前端拿到 /api/login/start 的错误后只弹一个 toast，
// 没有其它排查入口。故必须写出「怎么查 / 怎么补」，而不能只丢两个变量名
// （2026-10-17 现场：用户照着旧文案设了变量仍然失败——既没说值从哪来，也没说回调地址要登记）。
var ErrOAuthClientMissing = errors.New(
	"未获取到 Google OAuth 客户端凭据（点「＋Gemini」失败的直接原因）：" + gemini.EnvClientID +
		" 与 " + gemini.EnvClientSecret + " 均未设置，且本机未找到可提取的官方 CLI：\n" +
		"· 二选一即可：① 安装官方 CLI —— npm install -g @google/gemini-cli（或 @google/antigravity），" +
		"装完点「取消」再重新点「＋Gemini」；② 自建 OAuth 客户端 —— 在 console.cloud.google.com 建「桌面应用」类型客户端，" +
		"并把回调地址登记为 http://" + gemini.RedirectHost + ":" + strconv.Itoa(gemini.DefaultCallbackPort) + gemini.CallbackPath +
		"（⚠️ 必须写 localhost，不能写 127.0.0.1；默认用随机端口，该端口是端口被占时的兜底），" +
		"再把拿到的两枚值设为上述环境变量后**重启本工具**（凭据只在进程启动时读取一次）。\n" +
		"· 已查过的本机位置：" + strings.Join(gemini.OAuthClientSearchRoots(), "；") + "\n" +
		"· 若要核对环境变量是否真的生效，可设置 " + EnvDiagnoseOnStart + "=1 后重启查看日志。")

// EnvDiagnoseOnStart 置为 "1" 时，本工具在每次发起的登录里记一行诊断日志
// （凭据是否就绪、取自 env 还是本机 CLI、扫描过哪些位置）。**不含任何凭据值**。
//
// 为什么需要它：凭据解析结果被 sync.Once 缓存，用户改了环境变量却没重启时，
// 面板上只会重复同一条「缺凭据」——没有日志就无法区分「没设置」与「设置了但没生效」。
const EnvDiagnoseOnStart = "WILDWORK_GEMINI_DIAG"

// logOAuthDiagnosis 在 EnvDiagnoseOnStart=1 时记一行凭据诊断（**绝不含凭据值**）。
func logOAuthDiagnosis(status string) {
	if strings.TrimSpace(os.Getenv(EnvDiagnoseOnStart)) != "1" {
		return
	}
	source := "本机 CLI"
	if strings.TrimSpace(os.Getenv(gemini.EnvClientID)) != "" ||
		strings.TrimSpace(os.Getenv(gemini.EnvClientSecret)) != "" {
		source = "环境变量（或环境变量 + 本机 CLI 互补）"
	}
	log.Printf("gemini 登录诊断：OAuth 客户端凭据%s；来源=%s；client_id 长度=%d，client_secret 长度=%d；已扫描位置=%s",
		status, source, len(gemini.ClientID()), len(gemini.ClientSecret()),
		strings.Join(gemini.OAuthClientSearchRoots(), " | "))
}

// ErrGoogleUnreachable Google OAuth 端点连接层不可达（DNS 失败 / 超时 / 连接被拒）。
//
// 为什么要与「缺凭据」「授权码被拒」分开：三者的处置完全不同 ——
// 缺凭据要装 CLI 或设环境变量；授权码被拒要重新授权；**网络不可达只能改网络**
// （换网络 / 开代理）。修复前这类错误被原样抛成 `dial tcp …: i/o timeout`，
// 用户看到系统报文，既不知原因也不知该做什么。
var ErrGoogleUnreachable = errors.New(
	"无法连接 Google 授权服务器（" + hostOfURL(gemini.TokenEndpoint) + "）：本机网络到 Google 不可达。" +
		"这不是账号或授权码的问题 —— 请先确认能访问 Google（需要代理时先让本工具走代理），再重新点「＋Gemini」")

// networkError 把连接层失败包装成可由 errors.Is 识别的 ErrGoogleUnreachable。
func networkError(host string, cause error) error {
	return fmt.Errorf("%w（%s：%v）", ErrGoogleUnreachable, host, cause)
}

// hostOfURL 取 URL 的 host，用于错误文案（解析失败时回落为原文）。
func hostOfURL(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// Start 发起登录：起本机回调 server 并**立即**返回用户需要打开的授权 URL。
func Start(_ *http.Client, statePath string) (string, error) {
	// 先确认拿到了 OAuth 客户端凭据。**放在监听之前**——否则会白占一个回调端口再报错。
	if gemini.ClientID() == "" || gemini.ClientSecret() == "" {
		logOAuthDiagnosis("缺失")
		return "", ErrOAuthClientMissing
	}
	logOAuthDiagnosis("就绪")
	// ⚠️ 顺序：先监听、再拼 URL（协议 §2.3 第 2 条：先拼 URL 会在换端口时 redirect_uri_mismatch）。
	lns, port, err := listenCallback(0)
	if err != nil {
		return "", err
	}
	stateVal := randomState()
	redirectURI := fmt.Sprintf("http://%s:%d%s", gemini.RedirectHost, port, gemini.CallbackPath)
	st := state{
		State:        stateVal,
		RedirectURI:  redirectURI,
		CallbackPort: port,
		AuthURL:      buildAuthURL(stateVal, redirectURI),
		CreatedAt:    time.Now().Unix(),
	}
	if err := writeState(statePath, st); err != nil {
		closeAll(lns)
		return "", err
	}

	sess := &loginSession{state: st}
	mux := http.NewServeMux()
	mux.HandleFunc(gemini.CallbackPath, sess.handleCallback)
	sess.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	sess.lns = lns

	// 上一轮若还挂着回调口，先关掉（同一时刻只允许一条登录流程；app.loginBusy 已保证，
	// 这里只是防御，避免端口泄漏）。
	replaceSession(sess)
	for _, ln := range lns {
		l := ln
		go func() { _ = sess.srv.Serve(l) }()
	}
	return st.AuthURL, nil
}

// Poll 检查一次登录结果：授权未完成返回 ErrPending；拿到 code 则立即兑换，成功返回 Result
// 并删除 state 文件。
func Poll(client *http.Client, statePath string) (Result, error) {
	st, err := readState(statePath)
	if err != nil {
		return Result{}, fmt.Errorf("read state: %w", err)
	}
	if st.CreatedAt > 0 && time.Since(time.Unix(st.CreatedAt, 0)) > flowTTL {
		Shutdown()
		_ = os.Remove(statePath)
		return Result{}, errors.New("登录超时（6 分钟未收到授权回调），请重新发起")
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
		// 用户在授权页拒绝、state 校验失败、回调缺 code：终态，清掉会话与 state。
		Shutdown()
		_ = os.Remove(statePath)
		return Result{}, fmt.Errorf("登录失败: %s", errMsg)
	}

	res, terminal, err := exchange(client, st, code)
	if err != nil {
		if terminal {
			// 已经拿到 HTTP 响应：授权码很可能已被消费，重试没有意义。
			Shutdown()
			_ = os.Remove(statePath)
		}
		// 传输层失败（可能只是抖动）：保留 state，轮询继续；app 侧 6 分钟窗口兜底。
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
	if sess != nil {
		sess.close()
	}
}

// ---------------------------------------------------------------------------
// 令牌兑换与身份
// ---------------------------------------------------------------------------

// exchange 用授权码换 token（`POST https://oauth2.googleapis.com/token`）。
//
// 表单 5 字段（gemini-oauth.ts:285-291）：
// `client_id`、**`client_secret`（必须）**、`code`、`grant_type=authorization_code`、`redirect_uri`。
//
// 返回 (结果, 是否终态, 错误)：传输层失败算**非终态**（可能是抖动，Poll 继续轮询）；
// 一旦拿到 HTTP 响应就算终态（授权码很可能已被消费，重试没有意义）。
func exchange(client *http.Client, st state, code string) (Result, bool, error) {
	if client == nil {
		client = NewClient()
	}
	form := url.Values{}
	form.Set("client_id", gemini.ClientID())
	form.Set("client_secret", gemini.ClientSecret())
	form.Set("code", code)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", st.RedirectURI)

	req, err := http.NewRequest(http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Result{}, false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// ⚠️ 刻意不设 User-Agent：令牌端点是标准 Google OAuth，伪装只用在 Cloud Code 端点。
	resp, err := client.Do(req)
	if err != nil {
		// 连接层失败（DNS / 超时 / 拒绝）：归为 ErrGoogleUnreachable，让用户看到
		// 「网络不可达」而不是一行 `dial tcp …: i/o timeout`。
		// 仍是非终态：Poll 继续轮询，网络恢复后同一次登录还能走完。
		return Result{}, false, networkError(hostOfURL(tokenEndpoint), err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		// 错误文案**只透出信封字段**（error / error_description），不回显响应体原文。
		return Result{}, true, fmt.Errorf("授权码兑换失败（HTTP %d）：%s", resp.StatusCode, oauthErrorText(raw))
	}

	grant, err := gemini.ParseTokenGrant(raw, "")
	if err != nil {
		// ParseTokenGrant 的错误文案同样不含 token 原文（只含 error / error_description）。
		return Result{}, true, fmt.Errorf("授权码兑换失败：%w", err)
	}

	sub, email := identityOf(client, grant)
	uid := strings.TrimSpace(sub)
	if uid == "" {
		// 授权 URL 带 openid ⇒ id_token 必然返回身份；真拿不到时用 access_token 的哈希兜底
		// （同 lobsterai 的第四级回退）。代价：重复登录会得到**新** uid（access_token 每次
		// 都是新签发的）→ 写成新文件而非覆盖旧文件；旧文件由面板「删除账号」清理。
		sum := sha256.Sum256([]byte(grant.AccessToken))
		uid = hex.EncodeToString(sum[:])[:16]
	}
	nick := strings.TrimSpace(email)
	if nick == "" {
		nick = uid
	}
	return Result{
		AccessToken:  grant.AccessToken,
		RefreshToken: grant.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(grant.ExpiresIn) * time.Second).Unix(),
		ExpiresIn:    grant.ExpiresIn,
		UID:          uid,
		Nickname:     nick,
		Domain:       domainConst,
		ApiHost:      gemini.DailyHost,
	}, true, nil
}

// identityOf 解析账号身份：优先 `id_token`（与 access_token 同一次响应自带，零额外请求、
// 无失败面），缺席才兜底打一次 userinfo（协议 §2.3）。
func identityOf(client *http.Client, grant gemini.TokenGrant) (sub, email string) {
	sub, email = identityFromIDToken(grant.IDToken)
	if sub != "" && email != "" {
		return sub, email
	}
	us, ue, err := fetchUserInfo(client, grant.AccessToken)
	if err != nil {
		// 身份只是展示信息，不该让整个授权失败；但要留一行日志（历史上正是这里静默吞掉
		// 一切异常，导致「昵称退化成 id」在现场零日志可查）。
		log.Printf("gemini 登录：userinfo 兜底失败（%v）", err)
	}
	if sub == "" {
		sub = us
	}
	if email == "" {
		email = ue
	}
	return sub, email
}

// identityFromIDToken 从 `id_token` 的 payload 解出 `sub` / `email`。
//
// ⚠️ **不验签**：该值只用于展示名与稳定标识，不作为鉴权依据；令牌端点的响应本身已由 TLS
// 保护（与 auth.jwtExpiry 同口径）。任何解析失败返回空串而不抛错——身份不该让授权失败。
func identityFromIDToken(idToken string) (sub, email string) {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return "", ""
	}
	p := parts[1]
	// base64url 无填充 → 补到 4 的倍数。
	// ⚠️ Go 的 % 保留被除数符号，直接写 -len(p)%4 在 len%4!=0 时为负，
	// strings.Repeat 负数会 panic（login_qwenwork 曾实测踩中）。
	if r := len(p) % 4; r != 0 {
		p += strings.Repeat("=", 4-r)
	}
	raw, err := base64.URLEncoding.DecodeString(p)
	if err != nil {
		if raw, err = base64.RawURLEncoding.DecodeString(parts[1]); err != nil {
			return "", ""
		}
	}
	var payload struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return "", ""
	}
	return strings.TrimSpace(payload.Sub), strings.TrimSpace(payload.Email)
}

// fetchUserInfo 拉账号身份（`id_token` 缺席时的兜底）。
//
// ⚠️ 用 `id`（= `sub`）当账号主键：邮箱用户可以随时改，`sub` 不会变。
// 失败返回 error 由调用方记日志（不抛给用户）。
func fetchUserInfo(client *http.Client, accessToken string) (sub, email string, err error) {
	if client == nil {
		client = NewClient()
	}
	if strings.TrimSpace(accessToken) == "" {
		return "", "", errors.New("没有 access_token")
	}
	req, err := http.NewRequest(http.MethodGet, userInfoEndpoint, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("userinfo HTTP %d", resp.StatusCode)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var payload struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", "", err
	}
	return strings.TrimSpace(payload.ID), strings.TrimSpace(payload.Email), nil
}

// oauthErrorText 从 OAuth 错误响应里只取信封字段（error / error_description），
// **绝不回显 data / 响应体原文**（那里就是 token）。
func oauthErrorText(raw []byte) string {
	var e struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &e); err != nil || (e.Error == "" && e.ErrorDescription == "") {
		return "（响应体非 OAuth 错误信封，未记录：避免凭据进入日志）"
	}
	if e.ErrorDescription == "" {
		return e.Error
	}
	if e.Error == "" {
		return e.ErrorDescription
	}
	return e.Error + ": " + e.ErrorDescription
}

// ---------------------------------------------------------------------------
// 保存凭证（对齐其它渠道的 SaveAuth 契约）
// ---------------------------------------------------------------------------

// SaveAuth 以嵌套形原子写盘 `auths/gemini-<uid>.json`。
//
// 这里刻意**不手写 map**，而是构造 auth.Auth 后调 auth.SaveAtomic()：落盘字节形态与
// `auth.Parse` / `auth.LoadGeminiDir` / 续期路径的写回**完全同源**（不变量 3）。
//
// Extra 承载渠道专有字段（键名见 internal/gemini 的 ExtraKey*）：project 是出站信封与配额
// 请求的必需载荷（协议 §2.7），档位三件套是面板「账号规格」的落点。
// 注意：`access_token` / `refresh_token` / `uid` / `nickname` / `expires_at`
// **不**进 Extra（它们已有顶层字段，重复存两处必然漂移）。
func SaveAuth(authDir string, r Result) (string, error) {
	if strings.TrimSpace(r.UID) == "" {
		return "", errors.New("missing uid in result")
	}
	if strings.TrimSpace(r.AccessToken) == "" {
		// 防御：兑换路径已硬校验，这里再挡一次——半成品凭据落盘后不可用且会污染账号池。
		return "", errors.New("missing accessToken in result")
	}
	nick := strings.TrimSpace(r.Nickname)
	if nick == "" {
		nick = r.UID
	}
	domain := strings.TrimSpace(r.Domain)
	if domain == "" {
		domain = domainConst
	}
	a := &auth.Auth{
		Kind:         gemini.Kind.String(),
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		ExpiresAt:    r.ExpiresAt,
		Domain:       domain,
		ApiHost:      r.ApiHost,
		UID:          r.UID,
		Nickname:     nick,
		Extra:        extraOf(r),
	}
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return "", err
	}
	a.FilePath = filepath.Join(authDir, "gemini-"+sanitizeFileID(r.UID)+".json")
	if err := a.SaveAtomic(); err != nil {
		return "", err
	}
	return a.FilePath, nil
}

// extraOf 组装落进 auth.Auth.Extra 的渠道专有字段（全空则不写空 map，保持旧形态干净）。
func extraOf(r Result) map[string]string {
	ex := map[string]string{}
	if v := strings.TrimSpace(r.Project); v != "" {
		ex[gemini.ExtraKeyProject] = v
	}
	if v := strings.TrimSpace(r.PaidTierID); v != "" {
		ex[gemini.ExtraKeyPaidTier] = v
	}
	if v := strings.TrimSpace(r.PaidTierName); v != "" {
		ex[gemini.ExtraKeyPaidTierName] = v
	}
	if v := strings.TrimSpace(r.PaidTierLabel); v != "" {
		ex[gemini.ExtraKeyPaidTierLabel] = v
	}
	if len(ex) == 0 {
		return nil
	}
	return ex
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// buildAuthURL 拼授权页 URL（gemini-oauth.ts:102-115）。
//
// 参数：`client_id`、`response_type=code`、`redirect_uri`、`scope`（空格分隔的 6 项）、
// `state`、`access_type=offline`、`include_granted_scopes=true`、`prompt=consent`。
// `access_type=offline` + `prompt=consent` 是 refresh_token 的必要条件。
//
// ⚠️ **无 PKCE**：不产生 `code_challenge` / `code_verifier`（协议 §2.3 明确「刻意去掉」）。
func buildAuthURL(stateVal, redirectURI string) string {
	q := url.Values{}
	q.Set("client_id", gemini.ClientID())
	q.Set("response_type", "code")
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", strings.Join(gemini.Scopes, " "))
	q.Set("state", stateVal)
	q.Set("access_type", "offline")
	q.Set("include_granted_scopes", "true")
	q.Set("prompt", "consent")
	return authEndpoint + "?" + q.Encode()
}

// randomState 生成 state：randomBytes(16) 的 hex（**32 个 hex 字符**，协议 §2.3）。
func randomState() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 随机源失败：退到时间纳秒（仍然不可预测到"能被外部猜中"的程度，
		// 但这是降级路径，正常永不发生）。
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// listenCallback 在**同一个端口**上同时监听 127.0.0.1 与 [::1]。
//
// 端口选择顺序（协议 §2.3）：
//  1. preferred == 0 → 动态端口（默认路径）；
//  2. 动态分配失败 → 退到固定兜底端口 DefaultCallbackPort(8845)；
//  3. 指定端口被占用 → 换一个空闲端口（**不能**先拼 URL，否则 redirect_uri_mismatch）。
//
// ⚠️ 必须两个栈都听：浏览器把 `localhost` 解析成 `::1` 时只绑 v4 会「页面打不开 /
// 授权完没反应」。`[::1]` 失败可容忍（机器没开 IPv6），`127.0.0.1` 失败就是真失败。
func listenCallback(preferred int) ([]net.Listener, int, error) {
	try := func(port int) ([]net.Listener, int, error) {
		primary, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			return nil, 0, err
		}
		assigned := primary.Addr().(*net.TCPAddr).Port
		lns := []net.Listener{primary}
		if secondary, err := net.Listen("tcp", fmt.Sprintf("[::1]:%d", assigned)); err == nil {
			lns = append(lns, secondary)
		}
		return lns, assigned, nil
	}

	lns, port, err := try(preferred)
	if err == nil {
		return lns, port, nil
	}
	if preferred != 0 {
		// 指定端口被占用 → 换一个空闲端口。
		if lns, port, err2 := try(0); err2 == nil {
			return lns, port, nil
		}
		return nil, 0, fmt.Errorf("监听本机回调端口失败：%w", err)
	}
	// 动态端口都失败（极罕见）→ 退到固定兜底端口。
	if lns, port, err2 := try(gemini.DefaultCallbackPort); err2 == nil {
		return lns, port, nil
	}
	return nil, 0, fmt.Errorf("监听本机回调端口失败（动态端口与 %d 都不可用）：%w", gemini.DefaultCallbackPort, err)
}

// closeAll 关闭一组监听器。
func closeAll(lns []net.Listener) {
	for _, ln := range lns {
		_ = ln.Close()
	}
}

// snapshot 读出当前 pending（code / 错误 / 是否已有结论）。
func (s *loginSession) snapshot() (code, errMsg string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending.code, s.pending.errMsg, s.pending.received
}

// close 关闭回调 server 与监听口（幂等）。
func (s *loginSession) close() {
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return
	}
	s.shutdown = true
	srv := s.srv
	s.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
}

// replaceSession 安装新会话并关掉旧会话（同一时刻只跑一条登录流程）。
func replaceSession(sess *loginSession) {
	curMu.Lock()
	old := cur
	cur = sess
	curMu.Unlock()
	if old != nil {
		old.close()
	}
}

// currentSession 返回当前会话（无则 nil）。
func currentSession() *loginSession {
	curMu.Lock()
	defer curMu.Unlock()
	return cur
}

func readState(path string) (state, error) {
	var st state
	raw, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(raw, &st)
	return st, err
}

func writeState(path string, st state) error {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	raw, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(path, raw, 0o600)
}

// sanitizeFileID 把 uid 里可能出现的文件系统危险字符换成 '_'（uid 是外部数据，
// 落到文件名前必须消毒；同 officeace / lobsterai 的做法）。
func sanitizeFileID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return "unknown"
	}
	return out
}
