// Package login_trae 实现 TraeWork OAuth 回调登录。
package login_trae

import (
	"context"
	"crypto/rand"
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
	"wild-work/internal/traework"
)

var ErrPending = errors.New("login pending")

// submitMu 串行化 SubmitCallbackURL 对 state 文件的读-改-写。
// 自动回调（listener handler）与 Poll 直读直写不走此锁——它们是既有链路；
// 粘贴入口加锁是为了「连点两次提交」「粘贴与轮询交错」时不出现丢更新。
var submitMu sync.Mutex

type Result struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	Domain       string
	ApiHost      string
	MachineID    string
	DeviceID     string
	UID          string
	EnterpriseID string
	Nickname     string
}

type state struct {
	MachineID    string `json:"machineId"`
	DeviceID     string `json:"deviceId"`
	RefreshToken string `json:"refreshToken,omitempty"`
	AccessToken  string `json:"accessToken,omitempty"`  // userJwt 兜底路径（无 refreshToken 时）
	AuthCode     string `json:"authCode,omitempty"`     // PKCE 新流程：回调 authCodeInfo 里的 AuthCode
	CodeVerifier string `json:"codeVerifier,omitempty"` // 登录 URL 配对的 PKCE verifier（必须保存）
	Host         string `json:"host,omitempty"`
	Err          string `json:"err,omitempty"`
	// CallbackPort 本次登录回调监听端口（Start 随机分配）。traework 的回调 URL 不带
	// state 参数，端口是唯一的会话锚点——SubmitCallbackURL（issue #98 的人工兜底通道）
	// 用它做「这条 URL 是不是本次登录的」自检，拦截上一次登录的陈旧地址。
	CallbackPort int `json:"callbackPort,omitempty"`
}

func NewClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// Start 启动本地一次性回调监听，返回 Trae 授权 URL。
//
// 设备号作用域是「登录会话」，不是「本机」（issue #60/#70，R37 第二次修订）：
// 每次 Start 都新生成一对 machineId/deviceId，随登录流程走完落进
// auths/trae-<uid>.json —— 从此成为该账号的稳定注册设备（#3：x-device-id 必须是
// 注册设备，否则 9074；签到/对话头均从 auth 文件读，重登/刷新不经过这里）。
//   - 旧实现（本机级 device-id.json 全局复用）被 #70 实测证伪：一机挂 N 号 →
//     共用设备号的账号全部撞 4017 风控（15 号对照：共享组 3 号全中、一机一号组 0 中）；
//   - 同时它也是 #60 的根因：同一编造设备号被多账号先后绑定后，新账号授权
//     直接 20401 Device limit reached。随机新会话号即可消解两者，无需读官方
//     客户端真实设备号（需移植 storage.json TC 解密且依赖本机装官方客户端）。
//
// 注意：不能复用 login-state.json 本身——它承载的一次性登录状态（authCode/verifier）
// 在登录成功/取消时会被删除。
func Start(client *http.Client, statePath string) (string, error) {
	machineID := randHex(32)                          // 真实客户端 64 位 hex（32 字节）
	deviceID := randNumericID()                       // 真实客户端 15 位数字设备 ID（首次绑定随机产生）
	codeVerifier, codeChallenge := traework.GenPKCE() // PKCE：verifier 必须保存，交换 AuthCode 时用
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	callback := "http://" + addr + "/authorize"
	// 回调端口随 state 落盘：SubmitCallbackURL 的端口自检要用（见 state.CallbackPort）。
	port := 0
	if _, p, perr := net.SplitHostPort(addr); perr == nil {
		if n, aerr := strconv.Atoi(p); aerr == nil {
			port = n
		}
	}
	st := state{MachineID: machineID, DeviceID: deviceID, CodeVerifier: codeVerifier, CallbackPort: port}
	if err := writeState(statePath, st); err != nil {
		_ = ln.Close()
		return "", err
	}

	srv := &http.Server{ReadHeaderTimeout: 15 * time.Second}
	srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 支持 GET（query 参数）与 POST（JSON/form body）两种回调：
		// 新版授权页登录成功后 redirect=1 走 POST 到 authCallbackURL（body 带 refreshToken）。
		if r.Method == http.MethodPost {
			if body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10)); len(body) > 0 {
				var m map[string]any
				if err := json.Unmarshal(body, &m); err == nil {
					q := r.URL.Query()
					for k, v := range m {
						if s, ok := v.(string); ok && q.Get(k) == "" {
							q.Set(k, s)
						}
					}
					r.URL.RawQuery = q.Encode()
				}
			}
		}
		q := r.URL.Query()
		st.Host = q.Get("host")
		if st.Host == "" {
			st.Host = traework.OAuthHost
		}
		// 解析回调凭证：优先 refreshToken；缺省时从 userJwt（URL 编码 JSON）兜底取 RefreshToken/Token，
		// 再尝试 PKCE 新流程 authCodeInfo.AuthCode
		info := ParseCallback(r.URL.String())
		st.RefreshToken = info.RefreshToken
		st.AccessToken = info.AccessToken
		st.AuthCode = info.AuthCode
		if st.RefreshToken == "" && st.AccessToken == "" && st.AuthCode == "" {
			st.Err = "missing refreshToken in callback"
		}
		_ = writeState(statePath, st)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// 统一回调页：有凭证即成功态（自动关）；无凭证为失败态（展示原因）
		ok := st.Err == ""
		_, _ = w.Write([]byte(callbackPage(ok, st.Err)))
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		}()
	})
	go func() { _ = srv.Serve(ln) }()
	go func() {
		time.Sleep(5 * time.Minute)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	u, _ := url.Parse(traework.ConsoleHost + "/authorization")
	v := u.Query()
	v.Set("login_version", "1")
	v.Set("auth_from", "solo")
	v.Set("login_channel", "native_ide")
	v.Set("plugin_version", traework.PluginVersion)
	v.Set("auth_type", "local")
	v.Set("client_id", traework.ClientID)
	v.Set("redirect", "0")
	v.Set("login_trace_id", newUUID())
	v.Set("auth_callback_url", callback)
	v.Set("machine_id", machineID)
	v.Set("device_id", deviceID)
	v.Set("x_device_id", deviceID)
	v.Set("x_machine_id", machineID)
	v.Set("x_device_brand", traework.DeviceBrand)
	v.Set("x_device_type", "windows")
	v.Set("x_os_version", traework.OSVersion)
	v.Set("x_env", "")
	v.Set("x_app_version", traework.IdeVersion)
	v.Set("x_app_type", "stable")
	v.Set("code_challenge", codeChallenge)
	v.Set("code_challenge_method", "S256")
	v.Set("hide_saas_login", "true")
	v.Set("channel_name", "common")
	v.Set("click_id", "TRAE SOLOSetup-stable-"+traework.PluginVersion)
	u.RawQuery = v.Encode()
	_ = client
	return u.String(), nil
}

// Poll 检查回调是否已写入 state，完成后 ExchangeToken + GetUserInfo。
func Poll(client *http.Client, statePath string) (Result, error) {
	var st state
	raw, err := os.ReadFile(statePath)
	if err != nil {
		return Result{}, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return Result{}, err
	}
	if st.Err != "" {
		return Result{}, errors.New(st.Err)
	}
	if st.RefreshToken == "" && st.AccessToken == "" && st.AuthCode == "" {
		return Result{}, ErrPending
	}
	if st.Host == "" {
		st.Host = traework.OAuthHost
	}
	c := traework.New()
	c.HTTP = client
	a := &auth.Auth{Kind: "traework", RefreshToken: st.RefreshToken, AccessToken: st.AccessToken, ApiHost: st.Host, MachineID: st.MachineID, DeviceID: st.DeviceID, Domain: "trae.cn"}
	if st.AuthCode != "" {
		// PKCE 新流程：AuthCode + codeVerifier + 设备公钥交换
		res, err := c.ExchangeAuthCode(a, st.AuthCode, st.CodeVerifier)
		if err != nil {
			// 终端性 4xx 拒绝：authCode 已一次性耗尽，重试无意义。把错误固化到 state.Err，
			// 并清空 AuthCode，让后续轮询经顶部 `st.Err != ""` 短路直接返回可行动错误
			// （而不再每 2s 重复消耗同一次性 authCode 刷屏日志）。
			// 注意 ExchangeAuthCode 对 429/408/5xx/传输层还是会换 origin 重试并在全部失败
			// 后返回普通 error——此处仅对 *AuthCodeRejectedError 判断为可固化终态。
			var rej *traework.AuthCodeRejectedError
			if errors.As(err, &rej) {
				st.AuthCode = ""
				st.Err = err.Error()
				if traework.IsDeviceLimitReached(rej) {
					st.Err = "设备数量已达上限：请到其它设备登出该账号或设备列表「下线」释放名额（同一账号最多 10 台设备）"
				}
				_ = writeState(statePath, st)
			}
			return Result{}, err
		}
		a.AccessToken = res.AccessToken
		a.RefreshToken = res.RefreshToken
		a.ExpiresAt = res.ExpiresAt
		if res.Host != "" {
			a.ApiHost = res.Host
			st.Host = res.Host
		}
	} else if st.RefreshToken != "" {
		// 标准路径：ExchangeToken 换 access token
		if err := c.RefreshToken(a); err != nil {
			return Result{}, err
		}
	} else {
		// 兜底路径：回调只给了 userJwt.Token，无 refreshToken（后续 refresh 会失败，但本轮可用）
		if a.AccessToken == "" {
			return Result{}, errors.New("no token in callback")
		}
	}
	uid, nick, ent, err := c.GetUserInfo(a)
	if err != nil {
		return Result{}, err
	}
	_ = os.Remove(statePath)
	// 老用户自愈：R37 旧版遗留的本机级 device-id.json 已废弃（#60/#70：一机多号
	// 共享设备号 → 4017/20401）。新号不再读它；删掉可避免误解，也无凭据价值。
	_ = os.Remove(filepath.Join(filepath.Dir(statePath), "device-id.json"))
	return Result{AccessToken: a.AccessToken, RefreshToken: a.RefreshToken, ExpiresAt: a.ExpiresAt, Domain: "trae.cn", ApiHost: st.Host, MachineID: st.MachineID, DeviceID: st.DeviceID, UID: uid, EnterpriseID: ent, Nickname: nick}, nil
}

func SaveAuth(authDir string, r Result) (string, error) {
	if r.UID == "" {
		return "", fmt.Errorf("missing uid in result")
	}
	doc := map[string]any{
		"auth":    map[string]any{"accessToken": r.AccessToken, "refreshToken": r.RefreshToken, "expiresAt": r.ExpiresAt, "domain": r.Domain, "apiHost": r.ApiHost, "machineId": r.MachineID, "deviceId": r.DeviceID},
		"account": map[string]any{"uid": r.UID, "enterpriseId": r.EnterpriseID, "nickname": r.Nickname},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	_ = os.MkdirAll(authDir, 0o755)
	fp := filepath.Join(authDir, "trae-"+r.UID+".json")
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, fp); err != nil {
		return "", err
	}
	return fp, nil
}

func writeState(path string, st state) error {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	raw, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(path, raw, 0o600)
}

// CallbackInfo 回调解析结果（脱敏前的原始凭证，仅登录流程内部使用）。
type CallbackInfo struct {
	RefreshToken string
	AccessToken  string
	AuthCode     string // PKCE 新流程：authCodeInfo.AuthCode
}

// ParseCallback 解析 TRAE 登录回调链接，提取凭证字段。
// 回调形如：
//
//	http://127.0.0.1:PORT/authorize?refreshToken=...&userInfo={...}&userJwt={...}
//	或新流程：?authCodeInfo={AuthCode,...}&host=...&userInfo={...}
//
// refreshToken 优先；缺失时回退 userJwt.RefreshToken（URL 编码 JSON），
// 再兜底 userJwt.Token 作为 accessToken，最后尝试 authCodeInfo.AuthCode（PKCE 新流程）。
func ParseCallback(rawURL string) CallbackInfo {
	info := CallbackInfo{}
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return info
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return info
	}
	q := u.Query()
	info.RefreshToken = q.Get("refreshToken")
	if info.RefreshToken != "" {
		return info
	}
	userJwt := parseJSONParam(q.Get("userJwt"))
	info.RefreshToken = getString(userJwt, "RefreshToken")
	if info.RefreshToken != "" {
		return info
	}
	info.AccessToken = getString(userJwt, "Token")
	if info.AccessToken != "" {
		return info
	}
	// PKCE 新流程：authCodeInfo 是 URL 编码 JSON {AuthCode, ExpireAt, ExpireDuration}
	if raw := q.Get("authCodeInfo"); raw != "" {
		info.AuthCode = authCodeFromInfo(raw)
	}
	return info
}

// SubmitCallbackURL 手动提交浏览器跳转后的回调地址（远程/局域网场景的兜底入口，issue #98）。
//
// 为什么必须有它：本渠道的回调地址写死 `http://127.0.0.1:<随机端口>/authorize`，隐含
// 「浏览器与网关同一台机器」的前提。从局域网另一台电脑打开面板时，授权完成后浏览器
// 跳到的是**他自己机器**的 127.0.0.1 ⇒ 连接被拒、凭证到不了网关（#98 的主场景）；
// 此外回调监听只有 5 分钟寿命，超时/多开场景下「真回调」也会错过，表现同样是
// 「主界面无任何响应」。
//
// 本函数不改 Trae 侧约定（改不了），只补一条入口：用户把地址栏里那条打不开的 URL
// 原样粘回来，解析出凭证后写入 pending 的 state 文件 —— 与真实回调走**同一条结论通道**
// （Poll 每 2s 读一次 state，兑换/取用户信息零分叉）。
//
// 会话绑定（防串号/防陈旧）：traework 的回调 URL 不带 state 参数，回调端口是唯一的
// 会话锚点 —— Start 把本次监听端口写进 state（CallbackPort），粘贴端口不一致立刻拒绝
// （多半是上一次登录遗留的地址）。PKCE verifier 本就只存在 state 文件里，authCode
// 兑换天然只配当前会话。
//
// statePath 接受调用方传入（app 传 a.loginStateFP）：本包状态是文件而非内存会话，
// 由调用方持有路径比包内藏一个「最近一次 Start 的路径」更可测、也不怕多实例错位。
func SubmitCallbackURL(statePath, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return errors.New("回调地址为空：请复制登录完成后浏览器地址栏里的完整 URL")
	}
	var q url.Values
	pastePort := 0
	switch {
	case strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://"):
		u, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("回调地址无法解析: %w", err)
		}
		q = u.Query()
		pastePort, _ = strconv.Atoi(u.Port())
	case strings.Contains(raw, "="):
		// 只贴了查询串（浏览器「复制链接地址」有时带不带 host 都有）：容忍。
		qq, perr := url.ParseQuery(strings.TrimPrefix(raw, "?"))
		if perr != nil && len(qq) == 0 {
			return fmt.Errorf("回调地址无法解析: %w", perr)
		}
		q = qq
	default:
		return errors.New("无法识别的回调内容：请复制完整 URL（含 ? 之后的全部参数），不要只贴单个参数值")
	}

	submitMu.Lock()
	defer submitMu.Unlock()
	b, err := os.ReadFile(statePath)
	if err != nil {
		return errors.New("没有进行中的 TraeWork 登录流程（可能已完成或已超时），请重新发起登录")
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("登录状态损坏: %w", err)
	}
	// 已收到凭证的会话不接受二次提交：与自动回调的「保留先到者」一致，避免后到的覆盖先到的。
	if st.RefreshToken != "" || st.AccessToken != "" || st.AuthCode != "" {
		return errors.New("本次登录已收到凭证，无需再提交")
	}
	// 端口自检：多开/重试场景很容易粘到上一次登录的回调地址，放过去只会得到语焉不详的
	// 兑换失败，在这里拦下并说清原因。
	if pastePort != 0 && st.CallbackPort != 0 && pastePort != st.CallbackPort {
		return fmt.Errorf("回调地址的端口 %d 与本次登录的回调口 %d 不一致——多半粘贴的是上一次登录的地址，请用本次登录后跳转的那条 URL",
			pastePort, st.CallbackPort)
	}
	info := ParseCallback(raw)
	if info.RefreshToken == "" && info.AccessToken == "" && info.AuthCode == "" {
		st.Err = "粘贴的回调里没有可用的凭证参数（refreshToken / userJwt / authCodeInfo 均缺失）：请确认授权已完成，复制地址栏里含参数的整条 URL"
		_ = writeState(statePath, st)
		return errors.New(st.Err)
	}
	st.RefreshToken = info.RefreshToken
	st.AccessToken = info.AccessToken
	st.AuthCode = info.AuthCode
	// host 参数与真实回调同源：回调 URL 上带 host= 时覆盖默认网关（自动回调同款逻辑）。
	if h := q.Get("host"); h != "" {
		st.Host = h
	} else {
		st.Host = traework.OAuthHost
	}
	// 成功粘贴清掉此前失败标记（如自动回调先到过一次「缺 refreshToken」）：兜底入口的
	// 意义就是把一次坏回调救回来。
	st.Err = ""
	if err := writeState(statePath, st); err != nil {
		return err
	}
	// 取证日志（零 token：只记结论，不记任何凭证内容）。
	log.Printf("traework 手动回调已提交（端口自检通过，等待兑换）")
	return nil
}

// authCodeFromInfo 从 authCodeInfo（原始 code 或 JSON 对象）提取 AuthCode。
func authCodeFromInfo(raw string) string {
	raw = strings.TrimSpace(raw)
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) == nil {
		for _, container := range []any{m, m["Result"], m["result"]} {
			mm, ok := container.(map[string]any)
			if !ok {
				continue
			}
			if s := getString(mm, "AuthCode"); s != "" {
				return s
			}
		}
		return ""
	}
	return raw // 非 JSON：视为 code 本身
}

// parseJSONParam 解回调里 URL 编码的 JSON 参数（parse_qs 已解一层 percent-encoding，这里再容错解一层）。
func parseJSONParam(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	candidates := []string{raw}
	if uq, err := url.QueryUnescape(raw); err == nil && uq != raw {
		candidates = append(candidates, uq)
	}
	for _, c := range candidates {
		var obj map[string]any
		if json.Unmarshal([]byte(c), &obj) == nil && obj != nil {
			return obj
		}
	}
	return nil
}

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

func randHex(n int) string { b := make([]byte, n); _, _ = rand.Read(b); return hex.EncodeToString(b) }

// randNumericID 生成 15 位数字 ID，对齐官方客户端 device_id 格式（纯数字）。
// 每次登录会话新生成，登录成功后随 auth 文件持久化，后续签到沿用同一 ID（#3）。
func randNumericID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	n := uint64(b[0])<<24 | uint64(b[1])<<16 | uint64(b[2])<<8 | uint64(b[3])
	// 14 位数字 + 校验：落在 1e14~1e15 之间，凑 15 位
	return fmt.Sprintf("%015d", n%900000000000000+100000000000000)
}

// newUUID 生成 v4 格式 UUID（真实客户端 login_trace_id 形如 0f9b52e9-a63f-48e5-9bcf-888c822a5a8e）。
func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
