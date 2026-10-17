// Package login_zcode 实现 ZCode（智谱 z.ai）的**服务端中介设备授权流**登录。
//
// 形态（协议事实来源：docs/ref-ofm-渠道-zcode-gemini.md §1.3，逐条落实）：
//
//	① POST {Origin}/api/v1/oauth/cli/init
//	     Authorization: Bearer <自己生成的 32 字节 hex>   ← **会话密钥**，不是用户凭据
//	     body: {"provider": "bigmodel"}
//	   → { code, msg, data: { flow_id, poll_token, authorize_url, expires_at, poll_interval_sec } }
//	② 用户在自己的浏览器里打开 authorize_url 完成授权（授权在**服务端**完成，
//	   回调到 /oauth/cli/callback/<provider>，**不经** zcode:// 自定义协议）
//	③ GET {Origin}/api/v1/oauth/cli/poll/{flow_id}
//	     → data.status = "pending"   继续等
//	     → data.status = "ready"     data.token（zcode JWT）+ data.user.user_id + 第三方 access_token
//	     → data.status = "failed"    用户拒绝/授权失败（终态）
//
// 与内建渠道的形态差异：
//
//   - **不起本地回调端口**（协议 §1.3：「端口：无」）⇒ 本包**没有** Shutdown()；
//   - **不生成 state / 不使用 PKCE**：state 由服务端拼进 authorize_url；
//   - 轮询鉴权只带 flow_id（官方连 Bearer 都不发也返回 200，本包照参考实现带上会话密钥）；
//   - **凭据不可续期**：`zcode_jwt` 的 JWT 无 `exp`，上游无 refresh 端点
//     （参考实现 `ZCODE_REFRESHABLE = false`）⇒ Result.RefreshToken 恒为空，
//     ExpiresAt 写成**远期值**（见 farFutureExpiry）。
//
// 上游纪律（AGENTS.md §0.1）：本包只在用户主动发起登录后才请求上游；每次轮询都按
// 服务端下发的 interval 本地节流；一旦得到**终态**结论就把它固化进 state 文件，
// 后续 Poll 直接短路返回，不再重复打上游。
package login_zcode

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/zcode"
)

// 上游常量：主机与身份头取值的**单一来源在 internal/zcode**（渠道包），
// 这里一律引用而非另写一份——两处各写一份的话，将来改端点必然漂移
// （登录包与渠道包指向不同主机，且编译期毫无感觉）。
const (
	// Origin ZCode 平台 origin（设备授权发起 + 轮询 + 额度 + 推理端点共用）。
	Origin = zcode.Origin
	// AppVersion 登录请求里带的官方客户端版本（与渠道包的兜底值同源）。
	AppVersion = zcode.AppVersionFallback
	// Domain 写进 auth 文件的 domain 字段。
	Domain = zcode.Domain

	initPath = "/api/v1/oauth/cli/init"
	pollPath = "/api/v1/oauth/cli/poll/"

	// loginProvider 登录 provider，固定 `bigmodel`。
	//
	// 依据（协议 §1.3 的 provider 差异表 + 参考实现 `ZCODE_LOGIN_PROVIDER = 'bigmodel'`）：
	//   - `bigmodel` → 授权页 `bigmodel.cn/login?appId=zcode&…`，第三方凭据落 `data.bigmodel`；
	//   - `zai`      → 授权页 `chat.z.ai/api/oauth/authorize?…`，第三方凭据落 `data.zai`。
	// 两者的 init 响应结构完全一致，且**都不影响推理**：推理一律用 `data.token`（zcode JWT），
	// 第三方 token 只作备用身份（协议 §1.3 的 ready 字段说明）。
	// 本包的固定导出签名（`Start(client, statePath)`）没有 provider 入参，故取默认值；
	// 需要 `zai` 时由上层另开入口，不在本包硬编码两条路径。
	loginProvider = "bigmodel"

	// defaultPollIntervalSec 上游未下发 poll_interval_sec 时的缺省轮询间隔（**秒**）。
	defaultPollIntervalSec = 2
)

// endpointOrigin 是平台 origin。独立成变量（默认等于 Origin，而非直接引用常量）的唯一
// 目的是让回归测试能把它指向**本机 httptest 服务**。
// ⚠ 严禁把它指向生产上游做探测（AGENTS.md §0.1）。
var endpointOrigin = Origin

// ErrPending 表示授权尚未完成（用户还没在浏览器里确认），调用方应继续轮询。
// 与 login_minimax / login_trae 同名同义，app.pollLogin 用 errors.Is 判定。
var ErrPending = errors.New("login pending")

// Result 登录成功后拿到的凭据与账号信息。
type Result struct {
	// AccessToken 是 `data.token`（zcode JWT），免费额度通道的 `Authorization: Bearer`。
	AccessToken string
	// RefreshToken **恒为空**：本渠道不可续期（协议 §1.8：JWT 无 exp、
	// `ZCODE_REFRESHABLE = false`、凭据里 refresh_token 恒 undefined）。
	// 保留该字段是为了与其它渠道的 Result 同形（调用方按同一形状消费）。
	RefreshToken string
	// ExpiresAt 是 access token 的到期时刻（Unix **秒**），写成远期值（见 farFutureExpiry）。
	ExpiresAt int64
	// UID 是账号标识（`data.user.user_id`）——协议 §1.8：★**唯一稳定的账号标识**
	// （device_mid 每次登录都变、不是账号标识）。
	UID string
	// Nickname 是展示名；上游不下发时由 SaveAuth 用 UID 兜底。
	Nickname string
	// DeviceMid 是 `X-Device-Mid`（缺它 billing 全家桶回 400 code 3001）。
	// 服务端**不校验它的值**，只需稳定；本包登录时生成一次并随凭据持久化。
	DeviceMid string
	// BigmodelAccessToken / ZaiAccessToken 第三方备用身份（**不参与推理**）。
	BigmodelAccessToken string
	ZaiAccessToken      string
	// BigmodelRefreshToken / ZaiRefreshToken 第三方 refresh token（服务端若下发）。
	BigmodelRefreshToken string
	ZaiRefreshToken      string
}

// state 是落在 login-state.json 里的一次性登录状态。
//
// 为什么状态要落盘而不是只放内存：与 login_minimax / login_trae 保持同形——app 的
// Start / Poll / CancelLogin 是三个独立入口（CancelLogin 直接删 state 文件即等于放弃），
// 且进程重启后仍能看清上一轮登录卡在哪一步。
type state struct {
	FlowID string `json:"flowId"`
	// FlowSecret 是本次流程的 CLI 会话密钥（随 Authorization: Bearer 发送）。
	// ⚠ 它不是用户凭据，且只在流程存活期内有意义；仍按 0600 落盘。
	FlowSecret string `json:"flowSecret"`
	Provider   string `json:"provider"`
	// DeviceMid 本次登录生成的设备标识（SaveAuth 时写进凭据 Extra）。
	DeviceMid string `json:"deviceMid"`
	// AppVersion 发起流程时用的版本（轮询与落盘的 app_version 与它一致）。
	AppVersion string `json:"appVersion"`
	// IntervalSec 当前有效的轮询间隔（**秒**）。
	IntervalSec int `json:"intervalSec"`
	// ExpiresAt 是授权流程的截止时刻（Unix 秒）= Start 时刻依据服务端 expires_at。
	ExpiresAt int64 `json:"expiresAt"`
	// NextPollAt 是下一次**允许**向上游轮询的时刻（Unix 秒）。
	// app.pollLogin 固定每 2s 调一次 Poll，比 interval 密，必须本地拦掉多余请求。
	NextPollAt int64 `json:"nextPollAt"`
	// Err 是固化的终态错误：非空时后续 Poll 直接返回它，不再触碰上游（§0.1）。
	Err string `json:"err,omitempty"`
}

// NewClient 返回登录用 HTTP 客户端（30s 总超时兜底；上游 init 自身声明 30s 超时）。
func NewClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// Start 发起设备授权流，返回用户需要打开的授权链接（authorize_url）。
//
// 注意：
//   - **立即返回**，不阻塞在轮询上：前端 `window.open` 只在用户点击的瞬态激活窗口内
//     有效（参考实现特意把轮询拆到 Poll 里，本包同形）；
//   - `authorize_url` 必须是 https（协议 §1.3：授权页由服务端拼好，含 state）。
func Start(client *http.Client, statePath string) (string, error) {
	flowSecret := randomHex(32) // 官方 `UH(32).toString("hex")`
	deviceMid := newDeviceMid()

	body, _ := json.Marshal(map[string]string{"provider": loginProvider})
	req, err := http.NewRequest(http.MethodPost, endpointOrigin+initPath, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	// ① 的请求头（协议 §1.3）：会话密钥当 Bearer + 官方客户端来源头。
	req.Header.Set("Authorization", "Bearer "+flowSecret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(zcode.HdrUserAgent, zcode.UserAgentPrefix+AppVersion)
	req.Header.Set(zcode.HdrReferer, Origin)
	req.Header.Set(zcode.HdrAppVersion, AppVersion)
	req.Header.Set(zcode.HdrPlatform, zcode.Platform)

	raw, status, err := do(client, req)
	if err != nil {
		return "", fmt.Errorf("无法连接 ZCode 授权服务：%w", err)
	}
	if status >= 400 {
		// 零 token 纪律：不回显响应体原文（init 的响应体含 flow_id/poll_token），
		// 只透出上游自己声明的结构化错误字段。
		return "", fmt.Errorf("授权初始化失败（HTTP %d）：%s", status, structuredErr(raw))
	}

	var env struct {
		Code int64 `json:"code"`
		Msg  string
		Data struct {
			FlowID          string `json:"flow_id"`
			PollToken       string `json:"poll_token"`
			AuthorizeURL    string `json:"authorize_url"`
			ExpiresAt       int64  `json:"expires_at"` // Unix **秒**
			PollIntervalSec int64  `json:"poll_interval_sec"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("授权初始化响应不是 JSON：%w", err)
	}
	if env.Code != 0 {
		return "", fmt.Errorf("授权初始化失败：业务码 %d %s", env.Code, strings.TrimSpace(env.Msg))
	}
	flowID := strings.TrimSpace(env.Data.FlowID)
	if flowID == "" {
		return "", fmt.Errorf("授权初始化响应缺 flow_id（业务码 %d %s）", env.Code, strings.TrimSpace(env.Msg))
	}
	authorizeURL := strings.TrimSpace(env.Data.AuthorizeURL)
	if !strings.HasPrefix(authorizeURL, "https://") {
		return "", errors.New("授权初始化响应缺合法的 authorize_url（必须是 https）")
	}
	interval := int(env.Data.PollIntervalSec)
	if interval < 1 {
		// 官方校验 interval 必须 ≥1 秒且 < 总时长；越界时保守兜底（协议 §1.3）。
		interval = defaultPollIntervalSec
	}

	now := time.Now()
	st := state{
		FlowID:      flowID,
		FlowSecret:  flowSecret,
		Provider:    loginProvider,
		DeviceMid:   deviceMid,
		AppVersion:  AppVersion,
		IntervalSec: interval,
		ExpiresAt:   now.Add(time.Duration(env.Data.ExpiresAt) * time.Second).Unix(),
		// 首发也要等一个 interval：两次轮询之间至少间隔 interval。
		NextPollAt: now.Add(time.Duration(interval) * time.Second).Unix(),
	}
	if env.Data.ExpiresAt <= 0 {
		// 服务端没给有效期 ⇒ 算不出截止时刻。用一个**保守**的本地上限（5 分钟，
		// 与官方 `zH` 的默认总超时一致），而不是让流程永不过期。
		st.ExpiresAt = now.Add(5 * time.Minute).Unix()
	}
	if err := writeState(statePath, st); err != nil {
		return "", err
	}
	return authorizeURL, nil
}

// Poll 轮询一次授权结果：未完成返回 ErrPending；成功返回 Result 并删除 state 文件；
// 终态失败返回可行动的错误（同时把错误固化进 state，避免反复打上游）。
func Poll(client *http.Client, statePath string) (Result, error) {
	st, err := readState(statePath)
	if err != nil {
		return Result{}, fmt.Errorf("read state: %w", err)
	}
	// 终态短路：错误已固化，本轮登录不会再有别的结果（也不该再问上游）。
	if st.Err != "" {
		return Result{}, errors.New(st.Err)
	}
	if st.FlowID == "" || st.FlowSecret == "" {
		return Result{}, errors.New("login state 不完整（缺 flowId/flowSecret），请重新发起登录")
	}
	now := time.Now()
	if st.ExpiresAt > 0 && now.Unix() >= st.ExpiresAt {
		return Result{}, st.fail(statePath, "授权流程已过期，请重新发起登录")
	}
	// 本地节流：没到 NextPollAt 就什么都不做（上游 interval 是秒级的，app 是 2s 一跳）。
	if st.NextPollAt > 0 && now.Unix() < st.NextPollAt {
		return Result{}, ErrPending
	}

	req, err := http.NewRequest(http.MethodGet, endpointOrigin+pollPath+url.PathEscape(st.FlowID), nil)
	if err != nil {
		return Result{}, err
	}
	// ③ 的请求头（协议 §1.3）：参考实现带会话密钥 + UA + Referer + 版本头
	// （**不带** X-Platform）；官方其实连 Bearer 都不发也返回 200，这里照参考实现带上。
	req.Header.Set("Authorization", "Bearer "+st.FlowSecret)
	req.Header.Set("Accept", "application/json")
	v := strings.TrimSpace(st.AppVersion)
	if v == "" {
		v = AppVersion
	}
	req.Header.Set(zcode.HdrUserAgent, zcode.UserAgentPrefix+v)
	req.Header.Set(zcode.HdrReferer, Origin)
	req.Header.Set(zcode.HdrAppVersion, v)

	raw, status, err := do(client, req)
	if err != nil {
		// 网络抖动**不算失败**：与参考实现一致，返回 pending 继续轮询
		// （不改 state：这不是终态）。但要把下次轮询推后一个 interval。
		return Result{}, st.backoff(statePath, now, st.intervalOr(), ErrPending)
	}
	// ⚠ HTTP 4xx（除 408/429）才是**终态**失败；5xx 与网络错误继续重试
	// （官方原实现：`status >= 400 && status < 500 && status !== 408 && status !== 429`）。
	if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
		return Result{}, st.fail(statePath, fmt.Sprintf("轮询被拒（HTTP %d）：%s", status, structuredErr(raw)))
	}
	if status >= 400 {
		return Result{}, st.backoff(statePath, now, st.intervalOr(), ErrPending)
	}

	var env struct {
		Code int64 `json:"code"`
		Msg  string
		Data struct {
			Status string `json:"status"`
			Token  string `json:"token"`
			User   struct {
				UserID string `json:"user_id"`
				ID     string `json:"id"`
				Name   string `json:"name"`
				Email  string `json:"email"`
			} `json:"user"`
			Bigmodel thirdPartyBlock `json:"bigmodel"`
			Zai      thirdPartyBlock `json:"zai"`
		} `json:"data"`
		// Status 兜底：少数形态把 status 放在顶层而不是 data 里。
		Status string `json:"status"`
	}
	parseErr := json.Unmarshal(raw, &env)
	if parseErr != nil {
		// 2xx 但 body 不是 JSON：多半是代理/网关插进来的页面，属可重试异常，
		// 但仍退避一个 interval，免得 2s 一跳把上游打满。
		return Result{}, st.backoff(statePath, now, st.intervalOr(), ErrPending)
	}
	// 业务码非 0 或没有 data：按 pending 处理（参考实现同款口径——服务端在流程未就绪时
	// 也可能回业务码非 0，把它当失败会让正常等待被判死）。
	if env.Code != 0 || (env.Data.Status == "" && env.Status == "") {
		return Result{}, st.backoff(statePath, now, st.intervalOr(), ErrPending)
	}
	statusStr := strings.ToLower(strings.TrimSpace(firstNonEmpty(env.Data.Status, env.Status)))
	switch statusStr {
	case "pending":
		return Result{}, st.backoff(statePath, now, st.intervalOr(), ErrPending)
	case "failed", "denied", "rejected":
		// 终态：用户拒绝或授权失败。
		return Result{}, st.fail(statePath, "用户取消了授权或授权失败")
	case "expired", "timeout":
		return Result{}, st.fail(statePath, "授权流程已过期，请重新发起登录")
	case "ready":
		// 继续按 ready 解析（下方）。
	default:
		return Result{}, st.fail(statePath, fmt.Sprintf("轮询响应状态无法识别：%s", statusStr))
	}

	// ---- ready：字段硬校验（协议 §1.3）----
	zcodeJWT := strings.TrimSpace(env.Data.Token)
	userID := strings.TrimSpace(firstNonEmpty(env.Data.User.UserID, env.Data.User.ID))
	if zcodeJWT == "" || userID == "" {
		return Result{}, st.fail(statePath, "轮询响应缺关键字段（token / user_id）")
	}
	bigmodelToken := env.Data.Bigmodel.accessToken()
	zaiToken := env.Data.Zai.accessToken()
	if bigmodelToken == "" && zaiToken == "" {
		// 第三方 access_token 不参与推理，但它是「凭据完整性」的一部分：
		// 两者都空说明 ready 响应不是我们认识的形态（协议 §1.3 的 ready 必需字段）。
		return Result{}, st.fail(statePath, "轮询响应缺第三方 access_token（data.zai 与 data.bigmodel 均为空）")
	}
	displayName := strings.TrimSpace(env.Data.User.Name)
	if displayName == "" {
		displayName = strings.TrimSpace(env.Data.User.Email)
	}
	if displayName == "" {
		displayName = userID
	}
	res := Result{
		AccessToken:          zcodeJWT,
		RefreshToken:         "", // 本渠道不可续期（见 Result 字段注释）
		ExpiresAt:            farFutureExpiry(),
		UID:                  userID,
		Nickname:             displayName,
		DeviceMid:            st.DeviceMid,
		BigmodelAccessToken:  bigmodelToken,
		ZaiAccessToken:       zaiToken,
		BigmodelRefreshToken: env.Data.Bigmodel.refreshToken(),
		ZaiRefreshToken:      env.Data.Zai.refreshToken(),
	}
	_ = os.Remove(statePath) // 一次性状态用完即删（同 login_trae/login_minimax）
	return res, nil
}

// SaveAuth 以嵌套形原子写盘 `auths/zcode-<uid>.json`。
//
// 这里刻意**不手写 map**，而是构造 auth.Auth 后调 auth.SaveAtomic()：
// 这样落盘字节形态与 auth.Parse / auth.LoadZCodeDir / 将来刷新路径的写回**完全同源**，
// 不会再出现「登录写的字段名和刷新写的字段名不一致」这类漂移。
//
// Extra 约定（跨包硬性约定）：
//   - 渠道专有字段进 Extra 且用协议原文 snake_case：`device_mid`（必需）、`app_version`、
//     `source`，以及第三方备用身份 `bigmodel_access_token` / `zai_access_token`
//     （+ 可选的第三方 refresh token）；
//   - `access_token` / `uid` / `nickname` / `expires_at` **不重复塞进 Extra**
//     （它们已在 Auth 的一等字段上，重复存放必然漂移）。
//
// ⚠ 刻意**不**把 `user_id` 也塞进 Extra：Auth.UID 就是 `data.user.user_id`（账号标识），
// 同一事实存两处是漂移的来源（协议 §1.8 也只把它当账号标识用）。
func SaveAuth(authDir string, r Result) (string, error) {
	if strings.TrimSpace(r.UID) == "" {
		return "", fmt.Errorf("missing uid in result")
	}
	if r.ExpiresAt <= 0 {
		// 不可续期渠道没有「事后校正」的机会：ExpiresAt 落成 0 会让 auth.NeedsRefresh
		// 恒为真 → 每轮调度都去调 RefreshToken（本渠道是空实现）= 纯空转 + 账号被反复标记。
		return "", fmt.Errorf("missing expiresAt in result（ZCode 不可续期，到期时间必须是远期值）")
	}
	nick := strings.TrimSpace(r.Nickname)
	if nick == "" {
		nick = r.UID // 上游不下发昵称时用 uid 兜底，面板至少有可读标识（同 minimax）
	}
	extra := map[string]string{
		// device_mid 是**硬需求**：缺它 billing 全家桶回 400 code 3001（协议 §1.2.2）。
		"device_mid":  strings.TrimSpace(r.DeviceMid),
		"app_version": AppVersion,
		// source 标记凭据来源（参考实现里新凭据恒为 plugin）。
		"source": "plugin",
	}
	if v := strings.TrimSpace(r.BigmodelAccessToken); v != "" {
		extra["bigmodel_access_token"] = v
	}
	if v := strings.TrimSpace(r.ZaiAccessToken); v != "" {
		extra["zai_access_token"] = v
	}
	if v := strings.TrimSpace(r.BigmodelRefreshToken); v != "" {
		extra["bigmodel_refresh_token"] = v
	}
	if v := strings.TrimSpace(r.ZaiRefreshToken); v != "" {
		extra["zai_refresh_token"] = v
	}
	a := &auth.Auth{
		// Kind 取渠道包的单一真源（provider.ZCode），不另抄一份字面量。
		Kind: zcode.Kind.String(),
		// AccessToken = zcode_jwt（免费额度通道的 Authorization Bearer）。
		AccessToken: r.AccessToken,
		// RefreshToken 恒空：本渠道不可续期（协议 §1.8）。
		RefreshToken: "",
		ExpiresAt:    r.ExpiresAt,
		Domain:       Domain,
		// ApiHost 记平台 origin，供渠道侧与排障使用（本渠道的两条腿端点不同，
		// 具体走哪条由凭据形态决定，见 internal/zcode/client.go 的 resolveChannel）。
		ApiHost:  Origin,
		UID:      r.UID,
		Nickname: nick,
		Extra:    extra,
	}
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		return "", err
	}
	a.FilePath = filepath.Join(authDir, "zcode-"+safeFileToken(r.UID)+".json")
	if err := a.SaveAtomic(); err != nil {
		return "", err
	}
	return a.FilePath, nil
}

// ---------------------------------------------------------------------------
// 内部实现
// ---------------------------------------------------------------------------

// thirdPartyBlock ready 响应里的第三方凭据块（`data.bigmodel` / `data.zai`）。
//
// 两种命名都认：参考实现实测上游用 snake_case，但同一份源码里也保守地兼容了
// camelCase（`accessToken` / `refreshToken`）。
type thirdPartyBlock struct {
	AccessTokenSnake  string `json:"access_token"`
	AccessTokenCamel  string `json:"accessToken"`
	RefreshTokenSnake string `json:"refresh_token"`
	RefreshTokenCamel string `json:"refreshToken"`
}

func (b thirdPartyBlock) accessToken() string {
	return strings.TrimSpace(firstNonEmpty(b.AccessTokenSnake, b.AccessTokenCamel))
}

func (b thirdPartyBlock) refreshToken() string {
	return strings.TrimSpace(firstNonEmpty(b.RefreshTokenSnake, b.RefreshTokenCamel))
}

// farFutureExpiry 返回凭据的到期时刻（Unix 秒）——**远期值**。
//
// 「远期」不是矫情，而是硬需求：本渠道的 JWT **没有 `exp`**、上游**也没有续期端点**
// （协议 §1.8：`ZCODE_REFRESHABLE = false`、`refresh_token` 恒 undefined）。
// 若把 ExpiresAt 写成 0 或近值，`auth.NeedsRefresh` 会恒为真 → 调度器每一轮都去调
// `RefreshToken`（本渠道是空实现）→ 纯空转，还会让账号被反复标记刷新。
// 真失效的唯一可信判据是上游的 401 / code 1002（见 zcode.Classify）。
func farFutureExpiry() int64 { return time.Now().AddDate(100, 0, 0).Unix() }

// newDeviceMid 生成设备标识（UUIDv4 形态）。
//
// 依据（协议 §1.8）：`X-Device-Mid` 的**值不被服务端绑定校验**（实测同一 JWT 换任意随机
// UUID 都返回 200，只有「不带」才会 400 code 3001），但生成后**必须持久化**——同一账号
// 换 mid 会让服务端的用量归集看起来像换了一台设备（不报错，但不自然）。
func newDeviceMid() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "mid-" + randomHex(16)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40 // version 4
	buf[8] = (buf[8] & 0x3f) | 0x80 // variant 10xx
	h := hex.EncodeToString(buf)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// randomHex 返回 n 字节的随机十六进制串。
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// do 发送请求并读回（上限 1 MiB；非 2xx 也要读 body——错误信封就在 body 里）。
func do(client *http.Client, req *http.Request) ([]byte, int, error) {
	if client == nil {
		client = NewClient()
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, nil
}

// intervalOr 返回当前有效 interval（秒），非法值回退缺省。
func (st state) intervalOr() int {
	if st.IntervalSec <= 0 {
		return defaultPollIntervalSec
	}
	return st.IntervalSec
}

// backoff 按给定 interval 退避并落盘，返回 ret（正常是 ErrPending）。
func (st state) backoff(statePath string, now time.Time, intervalSec int, ret error) error {
	st.IntervalSec = intervalSec
	st.NextPollAt = now.Add(time.Duration(intervalSec) * time.Second).Unix()
	_ = writeState(statePath, st)
	return ret
}

// fail 把终态错误固化进 state 并返回它。
//
// 固化是刻意的：app.pollLogin 对非 ErrPending 错误是「记日志继续轮询」（同 trae），
// 不固化就会每 2s 重复问上游同一个必然失败的请求（§0.1 最小请求数）。
func (st state) fail(statePath, msg string) error {
	st.Err = msg
	_ = writeState(statePath, st)
	return errors.New(msg)
}

// structuredErr 从响应体里只取结构化错误字段（code / msg / error / error_description）
// 用于错误文案。
//
// 为什么不直接回显响应体：init 的响应体含 flow_id / poll_token，ready 响应体更含
// access/refresh token，而这些错误会进日志与面板（AGENTS.md §0.2「零 token 落盘」）。
// 取不到结构化字段时明确说「响应体未记录」，不退回原文。
func structuredErr(raw []byte) string {
	var e struct {
		Code      json.RawMessage `json:"code"`
		Msg       string          `json:"msg"`
		Error     string          `json:"error"`
		ErrorDesc string          `json:"error_description"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &e) != nil {
		return "（响应体非 JSON，未记录：避免凭据进入日志）"
	}
	parts := make([]string, 0, 3)
	if s := strings.TrimSpace(string(e.Code)); s != "" && s != "null" && s != "0" {
		parts = append(parts, "code="+s)
	}
	for _, v := range []string{e.Error, e.Msg, e.ErrorDesc} {
		if s := strings.TrimSpace(v); s != "" {
			parts = append(parts, truncate(s, 200))
		}
	}
	if len(parts) == 0 {
		return "（上游未给出结构化错误字段，响应体未记录：避免凭据进入日志）"
	}
	return strings.Join(parts, " | ")
}

// safeFileToken 把账号标识清洗成文件名安全的片段。
//
// user_id 实测是 17 位数字（协议 §1.8：手机号前 11 位派生），但**不假设**它一定是数字：
// 一旦上游改成带 `:` / `/` 的形态，未清洗就会写出到 auths 目录之外（路径穿越）。
func safeFileToken(s string) string {
	s = strings.TrimSpace(s)
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
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
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

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}
