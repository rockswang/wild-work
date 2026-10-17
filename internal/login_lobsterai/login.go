// Package login_lobsterai 实现 LobsterAI（有道龙虾）的**本机回调**登录：
// 起本机 HTTP 回调服务器收 `code`，拿到后**立即** `POST {apiBase}/api/auth/exchange`
// 换 token。**无 PKCE、无 DPoP**（协议事实来源：docs/ref-ofm-渠道-lobsterai-minimax.md
// §3「登录流程」97–156 行，逐条对照）。
//
// 链路：
//  1. Start：生成 state（randomUUID）与登录会话 uuid（randomUUID）/ firstKeyfrom（毫秒串），
//     起本机回调 server（**127.0.0.1** + 随机空闲端口，**绝不监听 0.0.0.0**），
//     路径 `/auth/callback`；**立即**返回登录 URL 并把 state 落盘。
//     Start 不能阻塞在等回调上：前端 `window.open` 只在用户点击后的瞬态激活窗口内有效
//     （同渠道 login_minimax 的注释，备忘 §3）。
//  2. 用户在系统浏览器打开 `{PortalBase}/portal#/login?...`，登录后浏览器被重定向回
//     `http://127.0.0.1:{port}/auth/callback?code=...&state=...`。
//  3. 回调处理器**原样比对 state**（不匹配回 400 并拒绝，备忘 108 行），收到 code 后置入内存。
//  4. Poll：由 `app.pollLogin` 每 2s 调一次；拿到 code 后立即 exchange（**不带 Authorization**），
//     成功即删除 state 文件并返回 Result，由调用方 SaveAuth 落盘 `auths/lobsterai-<uid>.json`。
//
// 纪律：
//   - 主机 / UA / 版本号的**单一来源是兄弟包 internal/lobsterai**（该包尚未落地时
//     `go build ./internal/...` 报 undefined 属**预期**）。本地绝不另写一份常量：
//     上一个渠道因两处各写一份常量而被收口过。
//   - 上游纪律（AGENTS.md §0.1）：本包只在用户主动发起登录后请求上游，且每个登录会话
//     最多打上游一次 exchange（+ 可能一次版本号查询，两次都由 internal/lobsterai 缓存）。
//   - 零 token 纪律（AGENTS.md §0.2）：错误文案**不回显响应体原文**——exchange 响应体里
//     就是 access/refresh token，而这些文案会经 app.pollLogin 进日志。
//
// 本包只负责登录。续期（`POST /api/auth/refresh`，备忘 §3.2 133–151 行）属于
// internal/lobsterai 的 Upstream.RefreshToken —— 那里同样受**不变量 20** 约束：
// refresh token 会轮换，刷新后必须紧跟 `SaveAtomic` 落盘。
package login_lobsterai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/lobsterai"
)

const (
	// exchangePath 是授权码换 token 的路径（备忘 §1.2，lobsterai.ts:32）。
	exchangePath = "/api/auth/exchange"
	// callbackPath 是本机回调路径（备忘 §1.2，lobsterai.ts:47）。
	callbackPath = "/auth/callback"
	// portalLoginPath 是 portal 登录页的 hash 路由（备忘 105 行：
	// `{portalBase}/portal#/login?source=electron&redirect_uri=...&state=...`）。
	portalLoginPath = "/portal#/login"
	// loginSource 固定为 electron（备忘 106 行）。
	loginSource = "electron"
	// flowTTL 是整条登录流程的上限：10 分钟（备忘 110 行）。
	//
	// 与 app.pollLogin 的 loginTimeout（5 分钟）**并存且口径不同**：后者是本工具自己的
	// 轮询窗口，前者是上游会话寿命。app 先到期只会少等 5 分钟，不会让流程出错。
	flowTTL = 10 * time.Minute

	// domainConst 写进 auth 文件的 domain 字段（同 minimax-"minimax.cn" /
	// qwenwork-"qwenwork.cn" 的口径）。本渠道的协议里**没有**该字段，
	// 它纯粹是本工具 auth 格式的一部分，故不取自兄弟包。
	domainConst = "lobsterai"

	// Extra 键名：keyfrom 四元组。**单一来源在 internal/lobsterai**（读取方是渠道包，
	// 故由它导出 Extra* 常量）——这里一律引用而非另写字面量：两处各写一份的话，
	// 键名一旦有一边改动，凭据仍在但读取处会**静默取到空值**（上一个渠道正是这么漂移的）。
	extraKeyUUID          = lobsterai.ExtraUUID
	extraKeyFirstKeyfrom  = lobsterai.ExtraFirstKeyfrom
	extraKeyLatestKeyfrom = lobsterai.ExtraLatestKeyfrom
	extraKeyUserID        = lobsterai.ExtraUserID
)

// ErrPending 表示授权尚未完成（用户还没在浏览器里登录完）。
// 与 login_trae / login_qwenwork / login_minimax 同名同义，app.pollLogin 用 errors.Is 判定。
var ErrPending = errors.New("login pending")

// endpointHost 是 API 主机（**默认取自 internal/lobsterai.APIBase**，单一来源）。
//
// 独立成变量的唯一目的是让回归测试把它指向**本机 httptest 服务**（同 login_minimax
// 的 endpointHost）。⚠️ 严禁把它指向生产上游做探测（AGENTS.md §0.1）。
var endpointHost = lobsterai.APIBase

// clientVersionFn 是客户端版本号的取值入口（默认 internal/lobsterai.ClientVersion：
// 12h 缓存 + 失败回退 FallbackClientVersion，备忘 §2.5）。测试替换它以免触碰真实网络。
var clientVersionFn = lobsterai.ClientVersion

// Result 登录成功后拿到的凭据与账号信息。
//
// 字段比"够用"多几个 Domain / ApiHost / ExpiresIn：app 的 complete*Login 收尾要构造
// auth.Auth 落盘并 reloadAccounts，模型列表 / 续期都从 auth 文件里取主机，故必须带上
// （与 login_minimax.Result 同口径）。
type Result struct {
	AccessToken  string
	RefreshToken string // 允许空串（备忘 §8.1：refresh_token 可为空）
	// ExpiresAt 是 access token 的到期时刻（**Unix 秒**）。见 expiresAtMillis 的注释：
	// 线上凭据形态用毫秒，但本工具 auth.Auth.ExpiresAt 的既有单位是秒，落盘前统一换算。
	ExpiresAt int64
	// ExpiresIn 是上游下发的 `expiresIn`（**秒**）原文，0 表示上游没给。
	// 保留它纯为诊断（app 只打日志，不参与落盘）；真正的到期判据用 ExpiresAt。
	ExpiresIn int64
	UID       string // 账号池去重/排序标识（派生规则见 deriveUID，备忘 153–155 行）
	Nickname  string
	// Domain / ApiHost 写进 auth 文件，供 internal/lobsterai 组装出站请求。
	Domain  string
	ApiHost string

	// ---- 以下四项是**必须持久化**的 keyfrom 四元组（备忘 §8.1、§3.1、§3.2）----
	//
	// 为什么必须落盘：refresh 请求体与模型列表 query 都要 `firstKeyfrom` /
	// `latestKeyfrom` / `version` / `uuid`（`userId` 有才带），少了它们账号直接不可用
	// （refresh 会失败，或模型列表拿不到 kimi-k3 等准入模型）。
	// 落法是 auth.Auth.Extra（见 SaveAuth 的注释）。
	//
	// UUID 是**安装 UUID**（登录会话生成，exchange/refresh 必带且不可每次重生成，
	// 备忘 102–109 行）——与 state 那个 randomUUID 是**两个不同的值**，勿混。
	UUID string
	// FirstKeyfrom 是首次登录时间戳（**毫秒字符串**，备忘 110–111 行）。
	FirstKeyfrom string
	// LatestKeyfrom 是最近活动时间戳（毫秒字符串）。
	// ⚠️ 本实现按外部参考实现刻意**不更新**（备忘 §3.2 150 行：refresh 时也发凭据里的原值），
	// 故登录写一次之后不应再改。这是**刻意**与"当前时刻"分叉，别顺手改成 time.Now()。
	LatestKeyfrom string
	// UserID 是**有道 yid**（备忘 §8.1 98–99 行：`user_id` = 有道 yid，用于 refresh 请求体；
	// exchange 响应里对应 `user.yid`）。
	UserID string
}

// state 是落在 app.loginStateFP（login-state.json）里的一次性登录状态。
//
// 为什么落盘而不是只放内存：与 login_trae/login_qwenwork/login_minimax 同形——app 的
// Start / Poll / CancelLogin 是三个独立入口（CancelLogin 直接删 state 文件即等于放弃），
// 且进程重启后仍能看清上一轮登录卡在哪一步。
type state struct {
	// State 是回调时**原样比对**的随机串（randomUUID，备忘 108 行）。落盘是为了让
	// "回调收到的 state"有权威比对源——回调 server 与 Start 不保证同 goroutine 时序。
	State string `json:"state"`
	// LoginUUID 是登录会话生成的安装 UUID，随凭据持久化。
	LoginUUID string `json:"loginUuid"`
	// FirstKeyfrom / LatestKeyfrom 是登录时刻的毫秒字符串（登录时两者相同）。
	FirstKeyfrom  string `json:"firstKeyfrom"`
	LatestKeyfrom string `json:"latestKeyfrom"`
	// CallbackPort / RedirectURI / AuthURL 仅诊断与 Poll 复用。
	CallbackPort int    `json:"callbackPort"`
	RedirectURI  string `json:"redirectUri"`
	AuthURL      string `json:"authUrl"`
	// CreatedAt 是 Start 时刻（Unix 秒），用于 flowTTL 超时判定。
	CreatedAt int64 `json:"createdAt"`
}

// pendingCode 是本机回调 server 收到的授权码（内存通道）。
//
// received 的语义是「**已有结论**」而不是「有 code」：state 校验失败 / 用户拒绝都会
// 把它置真（code 仍为空），Poll 见到即按终态报错——state 错的回调永远不可能变成对的
// 回调，没有理由让用户再等满 10 分钟。
type pendingCode struct {
	code     string
	errMsg   string
	received bool
}

// loginSession 是一次登录在内存里的运行时状态（回调 server + 待取授权码）。
// 全部字段由 mu 保护：回调 handler 在 server goroutine 里写，Poll / Shutdown 在主流程读。
type loginSession struct {
	mu       sync.Mutex
	state    state
	pending  pendingCode
	srv      *http.Server
	ln       net.Listener
	shutdown bool
}

var (
	// curMu 保护 cur（仅 Start / Shutdown / Poll 的入口临界区）。
	curMu sync.Mutex
	cur   *loginSession
)

// NewClient 返回登录用 HTTP 客户端（与 login_trae/login_minimax 同形：30s 总超时兜底）。
func NewClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// Start 发起登录：起本机回调 server 并**立即**返回用户需要打开的登录 URL。
//
// 生成物（备忘 108–111 行）：
//   - state = randomUUID()：回调时原样比对；
//   - uuid = randomUUID()：登录会话的安装 UUID，随凭据持久化；
//   - firstKeyfrom = latestKeyfrom = 当前毫秒字符串（登录时刻两者相同）。
func Start(_ *http.Client, statePath string) (string, error) {
	// 本机回调：127.0.0.1 + 随机空闲端口。**绝不监听 0.0.0.0**（备忘 104 行）：
	// 监听非环回等于把"收 code 即换 token"的回调口暴露到局域网。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("listen callback: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	stateVal := randomUUID()
	now := time.Now()
	keyfrom := strconv.FormatInt(now.UnixMilli(), 10)
	st := state{
		State:         stateVal,
		LoginUUID:     randomUUID(),
		FirstKeyfrom:  keyfrom,
		LatestKeyfrom: keyfrom,
		CallbackPort:  port,
		RedirectURI:   fmt.Sprintf("http://127.0.0.1:%d%s", port, callbackPath),
		CreatedAt:     now.Unix(),
	}
	// 登录 URL：{PortalBase}/portal#/login?source=electron&redirect_uri=...&state=...
	// ⚠️ 文档只写"登录 URL 形态"，**未确认**登录页是否要求参数位于 `#` 之前；
	// 这里照文档字面拼接（参数在 `#` 之后），与参考实现同源，不做"看起来更规范"的改写。
	st.AuthURL = fmt.Sprintf("%s%s?source=%s&redirect_uri=%s&state=%s",
		lobsterai.PortalBase, portalLoginPath, loginSource,
		urlQueryEscape(st.RedirectURI), urlQueryEscape(stateVal))
	if err := writeState(statePath, st); err != nil {
		_ = ln.Close()
		return "", err
	}

	sess := &loginSession{state: st}
	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, sess.handleCallback)
	sess.ln = ln
	sess.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	// 上一轮若还挂着回调口，先关掉（同一时刻只允许一条登录流程；app.loginBusy 已保证，
	// 这里只是防御，避免端口泄漏）。
	replaceSession(sess)
	go func() { _ = sess.srv.Serve(ln) }()
	return st.AuthURL, nil
}

// Poll 检查一次登录结果：授权未完成返回 ErrPending；拿到 code 则**立即** exchange，
// 成功返回 Result 并删除 state 文件。
func Poll(client *http.Client, statePath string) (Result, error) {
	st, err := readState(statePath)
	if err != nil {
		return Result{}, fmt.Errorf("read state: %w", err)
	}
	// 流程超时 10 分钟（备忘 110 行）：到点即终态，关掉回调口并清掉 state。
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
		// 用户在登录页点了拒绝，或回调参数异常：终态，清掉会话与 state。
		Shutdown()
		_ = os.Remove(statePath)
		return Result{}, fmt.Errorf("登录失败: %s", errMsg)
	}

	res, err := exchange(client, st, code)
	if err != nil {
		// 不删 state、不 Shutdown：轮询继续（可能是网络抖动），app 侧 5 分钟窗口兜底。
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

// SaveAuth 以嵌套形原子写盘 `auths/lobsterai-<uid>.json`。
//
// 这里刻意**不手写 map**，而是构造 auth.Auth 后调 auth.SaveAtomic()：这样落盘字节形态
// 与 `auth.Parse` / `auth.LoadLobsterAIDir` / 将来刷新路径的写回**完全同源**
// （不变式 3：auth 文件嵌套格式必须与 Parse 一致）。
//
// Extra 承载 keyfrom 四元组（键名见 extraKey* 常量），理由：
//   - auth.Auth 已有 access/refresh/expiresAt/uid/nickname 字段，够存"通用"部分；
//   - 但 `uuid` / `first_keyfrom` / `latest_keyfrom` / `user_id` 是本渠道**专有**且
//     refresh 请求体与模型列表 query 的必需载荷（备忘 §3.1/§3.2/§1.3），丢了账号直接不可用；
//   - auth.Auth.Extra（map[string]string，只增不减、向后兼容）正是为这种"渠道扩展凭据"
//     设的口子（首例 officeace），故不再新开顶层字段。
//
// 注意：`nickname` / `access_token` / `uid` / `expires_at` **不**进 Extra（它们已有
// 顶层字段），避免同一事实两处存储、日后漂移。
func SaveAuth(authDir string, r Result) (string, error) {
	if strings.TrimSpace(r.UID) == "" {
		return "", errors.New("missing uid in result")
	}
	if strings.TrimSpace(r.AccessToken) == "" {
		// 防御：exchange 已硬校验，这里再挡一次——半成品凭据落盘后不可用且会污染账号池。
		return "", errors.New("missing accessToken in result")
	}
	nick := strings.TrimSpace(r.Nickname)
	if nick == "" {
		nick = r.UID // 上游不下发 nickname 时用 uid 兜底，面板至少有可读标识（同 minimax/raccoon）
	}
	a := &auth.Auth{
		Kind:         "lobsterai",
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		ExpiresAt:    r.ExpiresAt,
		Domain:       r.Domain,
		ApiHost:      r.ApiHost,
		UID:          r.UID,
		Nickname:     nick,
		Extra:        keyfromExtra(r),
	}
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return "", err
	}
	a.FilePath = filepath.Join(authDir, "lobsterai-"+sanitizeFileID(r.UID)+".json")
	if err := a.SaveAtomic(); err != nil {
		return "", err
	}
	return a.FilePath, nil
}

// ---------------------------------------------------------------------------
// exchange（授权码换 token）
// ---------------------------------------------------------------------------

// exchange 调 `POST {apiBase}/api/auth/exchange` 换凭据（备忘 §3.1，113–131 行）。
//
// 请求要点（照抄，别"优化"）：
//   - 路径 `/api/auth/exchange`；**不带 `Authorization`**（备忘 115 行）；
//   - 体 5 字段：authCode / firstKeyfrom / latestKeyfrom / uuid / version；
//     `latestKeyfrom` 取**当前毫秒**（备忘 122 行；与 refresh 刻意不更新它相反）；
//   - `version` 取 internal/lobsterai.ClientVersion()（12h 缓存 + 失败回退常量）。
func exchange(client *http.Client, st state, code string) (Result, error) {
	if client == nil {
		client = NewClient()
	}
	body, err := json.Marshal(map[string]string{
		"authCode":      code,
		"firstKeyfrom":  st.FirstKeyfrom,
		"latestKeyfrom": strconv.FormatInt(time.Now().UnixMilli(), 10),
		"uuid":          st.LoginUUID,
		"version":       clientVersion(),
	})
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequest(http.MethodPost, endpointHost+exchangePath, strings.NewReader(string(body)))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// UA 取自兄弟包常量（唯一来源），与三处各写一份字面量的做法相反。
	req.Header.Set("User-Agent", lobsterai.UserAgent)
	// 刻意**不**设置 Authorization（备忘 §2.1：exchange 不带它）。

	resp, err := client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return parseExchange(resp.StatusCode, raw, st)
}

// envelope 是上游统一信封 `{code,msg,data}`（备忘 128 行：`code` 必须为 0 且 `data` 必须是对象）。
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// parseExchange 解析并**硬校验** exchange 响应。
//
// 错误文案一律**不带响应体原文**：exchange 响应体里就是 access/refresh token，
// 而这些文案会经 app.pollLogin 进日志（AGENTS.md §0.2 零 token 落盘）。
// 只透出上游自己声明的信封 `code` / `msg`。
func parseExchange(status int, raw []byte, st state) (Result, error) {
	if status >= 400 {
		return Result{}, fmt.Errorf("exchange 失败 http %d：%s", status, envelopeErr(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return Result{}, fmt.Errorf("exchange 响应不是 JSON（%v）", err)
	}
	if env.Code != 0 {
		return Result{}, fmt.Errorf("exchange 被拒：code=%d msg=%s", env.Code, truncate(env.Msg, 200))
	}
	// `data` 必须是**对象**：null（字面量 null / 空 / 缺失）一律拒——按"缺 accessToken"处理，
	// 绝不落半成品凭据（备忘 130 行）。
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return Result{}, errors.New("exchange 响应缺 data 对象")
	}
	var data struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"` // 单位：**秒**（备忘 131 行）
		User         struct {
			// 三个 ID 字段都可能是字符串或数字（同族接口常见），用 RawMessage 再归一，
			// 避免把数字形态的 id 静默丢成空串（那就白白降级到 uid 回退的下一级）。
			ID       json.RawMessage `json:"id"`
			YID      json.RawMessage `json:"yid"`
			UserID   json.RawMessage `json:"userId"`
			Nickname string          `json:"nickname"`
		} `json:"user"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		return Result{}, fmt.Errorf("exchange 响应 data 不是对象（%v）", err)
	}
	if strings.TrimSpace(data.AccessToken) == "" {
		return Result{}, errors.New("exchange 响应缺 accessToken（不落半成品凭据）")
	}

	userID := jsonIDString(data.User.YID)
	// expires_at 三级：优先 expiresIn（秒）→ JWT exp → 留空（0 表示未知）。
	expiresIn := data.ExpiresIn
	if expiresIn < 0 {
		expiresIn = 0
	}
	return Result{
		AccessToken:  data.AccessToken,
		RefreshToken: data.RefreshToken,
		ExpiresAt:    expiresAtSeconds(expiresIn, data.AccessToken),
		ExpiresIn:    expiresIn,
		UID:          deriveUID(data.User.ID, data.User.UserID, data.User.YID, json.RawMessage(data.AccessToken)),
		Nickname:     data.User.Nickname,
		Domain:       domainConst,
		ApiHost:      lobsterai.APIBase,
		UUID:         st.LoginUUID,
		FirstKeyfrom: st.FirstKeyfrom,
		// latestKeyfrom 用**凭据里的原值**（= 登录时刻），不是 exchange 请求体里那个
		// 当前时刻：请求体那一份是一次性载荷，持久化这份要与 refresh 的语义一致
		// （备忘 150 行：refresh 刻意不更新 latestKeyfrom）。
		LatestKeyfrom: st.LatestKeyfrom,
		UserID:        userID,
	}, nil
}

// ---------------------------------------------------------------------------
// 派生与解析工具
// ---------------------------------------------------------------------------

// deriveUID 是 uid 的四级回退（备忘 §3.3，153–155 行）：
//
//	user.id → user.userId → user.yid → sha256(accessToken) 的 hex 前 16 字符
//
// ⚠️ 刻意**不插入** JWT `sub` 一级：参考实现明确不插，以免与外部参考实现分叉
// （备忘 155 行）。别因为"sub 看起来更权威"就顺手加一级。
//
// 已知代价（如实标注）：只有落到第 4 级时，"同一账号重复登录"会得到**新的** uid
// （access_token 每次都是新签发的）→ 写成新文件而不是覆盖旧文件；旧文件由面板
// 「删除账号」清理即可（旧 refresh token 轮换后本已失效）。前三级是上游稳定 ID，不受影响。
func deriveUID(ids ...json.RawMessage) string {
	// 最后一项是 accessToken（裸字节，不是 JSON 字符串），单独处理。
	accessToken := ""
	if n := len(ids); n > 0 {
		accessToken = string(ids[n-1])
		ids = ids[:n-1]
	}
	for _, raw := range ids {
		if s := jsonIDString(raw); s != "" {
			return s
		}
	}
	sum := sha256.Sum256([]byte(accessToken))
	return hex.EncodeToString(sum[:])[:16]
}

// expiresAtSeconds 计算 auth.Auth.ExpiresAt（**Unix 秒**），拿不到就返回 0（留空）。
//
// 单位说明（备忘 131 行 vs 综合约定）：协议侧的 `expires_at` 是**毫秒字符串**，
// 而本工具 auth.Auth.ExpiresAt 的既有单位是**秒**（auth.go 字段注释 + NeedsRefresh/
// jwtExpiry 都按秒）。落盘到 auth 文件的是本工具自己的格式，故这里统一按秒给出，
// 换算只发生在"读上游 expiresIn 秒 → now+expiresIn*1000 毫秒"这一步的内部表达里。
// ⚠️ 若将来 internal/lobsterai 要对外暴露协议形状的毫秒 expires_at，
// 请在**那一侧**做换算，不要把这个字段改成毫秒（会与 auth 包全面冲突）。
func expiresAtSeconds(expiresIn int64, accessToken string) int64 {
	if expiresIn > 0 {
		return time.Now().Add(time.Duration(expiresIn) * time.Second).Unix()
	}
	// 回退：解析 access token 的 JWT `exp`（不校验签名，只读 payload 自报到期）。
	if ms := jwtExpiryMillis(accessToken); ms > 0 {
		return ms / 1000
	}
	return 0 // 都拿不到 → 留空（0=未知；面板据此不显示到期时间）
}

// jwtExpiryMillis 解出 JWT payload 的 `exp`（毫秒）；非 JWT / 无 exp 时返回 0。
// 不校验签名：用途仅是从本地凭据自身读出上游自报的到期时刻（同 auth.jwtExpiry 的定位）。
func jwtExpiryMillis(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	p := parts[1]
	// base64url 无填充 → 补到 4 的倍数。注意 Go 的 % 保留被除数符号，
	// 直接写 -len(p)%4 在 len%4!=0 时为负，strings.Repeat 负数会 panic
	// （login_qwenwork 实测踩过：poll panic: strings: negative Repeat count）。
	if r := len(p) % 4; r != 0 {
		p += strings.Repeat("=", 4-r)
	}
	raw, err := base64URLDecode(p)
	if err != nil {
		return 0
	}
	var payload struct {
		Exp json.Number `json:"exp"`
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if dec.Decode(&payload) != nil {
		return 0
	}
	f, err := payload.Exp.Float64()
	if err != nil || f <= 0 {
		return 0
	}
	// 秒级 JWT（绝大多数）与毫秒级都认：>=1e12 视为毫秒。
	if f >= 1e12 {
		return int64(f)
	}
	return int64(f * 1000)
}

// jsonIDString 把上游可能给字符串/数字的 ID 字段归一成**非空字符串**（空/非法返回 ""）。
func jsonIDString(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if json.Unmarshal(raw, &v) != nil {
			return ""
		}
		return strings.TrimSpace(v)
	}
	// 数字形态：原样保留（去掉可能的科学计数写法带来的空白）。
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return ""
	}
	return strings.TrimSpace(n.String())
}

// keyfromExtra 组装落进 auth.Auth.Extra 的 keyfrom 四元组。
// 键名用**协议原文的 snake_case**（uuid / first_keyfrom / latest_keyfrom / user_id）：
// internal/lobsterai 与本包共享同一份协议词汇，谁都能一眼对上备忘 §8.1 的字段表。
func keyfromExtra(r Result) map[string]string {
	ex := map[string]string{}
	if v := strings.TrimSpace(r.UUID); v != "" {
		ex[extraKeyUUID] = v
	}
	if v := strings.TrimSpace(r.FirstKeyfrom); v != "" {
		ex[extraKeyFirstKeyfrom] = v
	}
	if v := strings.TrimSpace(r.LatestKeyfrom); v != "" {
		ex[extraKeyLatestKeyfrom] = v
	}
	if v := strings.TrimSpace(r.UserID); v != "" {
		ex[extraKeyUserID] = v
	}
	if len(ex) == 0 {
		return nil // 全空就别写空 map（保持旧形态干净，Parse 后 Extra 为 nil）
	}
	return ex
}

// envelopeErr 从响应体里只取信封字段（code / msg）用于错误文案，**绝不回显 data**
// （data 里就是 token）。取不到结构化字段时明确说明"响应体未记录"。
func envelopeErr(raw []byte) string {
	var env envelope
	if len(raw) == 0 || json.Unmarshal(raw, &env) != nil {
		return "（响应体非 JSON，未记录：避免凭据进入日志）"
	}
	msg := truncate(env.Msg, 200)
	if msg == "" {
		msg = "（上游未给出 msg）"
	}
	return fmt.Sprintf("code=%d msg=%s", env.Code, msg)
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

// sanitizeFileID 把 uid 里可能出现的文件系统危险字符换成 '_'（同 officeace 的做法）。
// uid 正常情况下是上游 ID 或 hex，但上游 ID 是外部数据，落到文件名前必须消毒。
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

func truncate(s string, n int) string {
	// 顺手把换行压成空格：错误文案会进单行日志，多行会把后续日志搅乱。
	s = strings.Join(strings.Fields(s), " ")
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}

// clientVersion 取客户端版本号（默认走兄弟包 12h 缓存 + 失败回退）。
func clientVersion() string {
	if clientVersionFn == nil {
		return lobsterai.FallbackClientVersion
	}
	if v := strings.TrimSpace(clientVersionFn()); v != "" {
		return v
	}
	return lobsterai.FallbackClientVersion
}
