// Package login_qwenwork 实现千问办公的「设备授权」登录（与桌面端 1.3.2 同一条流程）。
//
// 链路（2026-11 实测）：
//  1. Start：生成 PKCE(S256) + nonce + machine_id，返回
//     {GatewayBase}/device/selectAccounts?challenge=…&challenge_method=S256&nonce=…&machine_id=…&client_id=…&redirect_uri=…
//  2. 浏览器打开该地址。**未登录时它 302 到 /oauth2/auth，而那条 OAuth 请求的
//     state 与 code_challenge 都是网关自己生成的**；用户登录后回到
//     /oauth/callback（网关自己的 state，故校验通过），网关把该 nonce 标记为已批准。
//  3. Poll：GET {GatewayBase}/api/v1/deviceToken/poll?nonce=…&verifier=…&challenge_method=S256
//     404（x-device-flow-state: pending）表示还没批；200 直接返回 token 对。
//
// 为什么不再自己拼 /oauth2/auth（2026-11 三次实测的结论）：
//   - 官方 client_id 的 redirect_uri 白名单只认 https://gateway.qwenwork.cn/oauth/callback；
//   - 自己拼的 state 不是网关签发的，回调页一律 INVALID_OAUTH_CALLBACK（"invalid or expired"）；
//   - 动态注册（RFC 7591）拿到的 client_id 是 36 位 UUID，登录页把它当 X-OAuth-Source
//     上报，服务端限 32 字符 → bad-request。
//
// 即：**OAuth 是设备流的内层实现**，第三方只能从 /device/selectAccounts 进入。
package login_qwenwork

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
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
)

// 上游常量（取自桌面端 1.3.2 的 auth 模块）。
const (
	// GatewayBase 设备流与令牌轮询所在网关。
	GatewayBase = "https://gateway.qwenwork.cn"
	// DevicePath 设备授权入口：首次 GET 会在服务端登记本次 nonce，并 302 到 OAuth 授权页。
	DevicePath = "/device/selectAccounts"
	// PollPath 令牌轮询端点。
	PollPath = "/api/v1/deviceToken/poll"
	// ClientID 桌面端的 public client（设备流只接受它）。
	ClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"
	// RedirectURI 桌面端注册的回调 scheme（设备流不真正跳回，仅作为登记值）。
	RedirectURI = "qwenwork-cn://"
	// FlowTTL 设备流有效期：与桌面端轮询上限（5 分钟）一致。
	FlowTTL = 5 * time.Minute
	// machineIDFile 稳定机器标识的落盘文件名（与 login state 同目录）。
	machineIDFile = "qwenwork-machine-id"
)

// gatewayBase 轮询用的网关基址。生产恒为 GatewayBase；用例改写到 httptest 服务。
var gatewayBase = GatewayBase

// ErrPending 表示用户尚未在浏览器里完成设备授权。
var ErrPending = errors.New("login pending")

// Result 登录成功后拿到的凭证与账号信息。
type Result struct {
	AccessToken      string
	RefreshToken     string // 上游轮换，必须落盘
	ExpiresIn        int64  // 秒
	RefreshExpiresIn int64  // 秒（0 = 上游未给）
	UID              string
	Nickname         string
}

// state 本次设备流的状态（落盘，供 Poll 使用）。
type state struct {
	Verifier  string `json:"verifier"`
	Challenge string `json:"challenge"`
	Nonce     string `json:"nonce"`
	MachineID string `json:"machine_id"`
	AuthURL   string `json:"auth_url"`
	CreatedAt int64  `json:"created_at"`
}

// NewClient 简单 HTTP 客户端（设备流不需要 cookie）。
func NewClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// Start 发起设备授权：生成 PKCE/nonce/machine_id，返回浏览器入口地址。
//
// 只拼地址、**不发请求**：/device/selectAccounts 首次 GET 即登记 nonce 并消费掉
// 「首次访问」状态，本工具若先探一次，用户浏览器再打开就会拿到 DEVICE_FLOW_REPLAYED。
func Start(client *http.Client, statePath string) (string, error) {
	verifier, challenge := makePKCE()
	nonce := uuid4Hex()
	machineID, err := ensureMachineID(statePath)
	if err != nil {
		return "", err
	}
	authURL := buildDeviceURL(challenge, nonce, machineID)

	st := state{
		Verifier:  verifier,
		Challenge: challenge,
		Nonce:     nonce,
		MachineID: machineID,
		AuthURL:   authURL,
		CreatedAt: time.Now().Unix(),
	}
	if err := writeState(statePath, st); err != nil {
		return "", err
	}
	return authURL, nil
}

// buildDeviceURL 组装设备授权入口。抽成函数便于用例直接断言参数，不必起网络。
func buildDeviceURL(challenge, nonce, machineID string) string {
	q := url.Values{
		"challenge":        {challenge},
		"challenge_method": {"S256"},
		"nonce":            {nonce},
		"machine_id":       {machineID},
		"client_id":        {ClientID},
		"redirect_uri":     {RedirectURI},
	}
	return GatewayBase + DevicePath + "?" + q.Encode()
}

// Poll 单次检查授权结果；未完成返回 ErrPending；成功后返回 token 对并清理状态。
func Poll(client *http.Client, statePath string) (Result, error) {
	if client == nil {
		client = NewClient()
	}
	st, err := readState(statePath)
	if err != nil {
		return Result{}, fmt.Errorf("read state: %w", err)
	}
	if time.Since(time.Unix(st.CreatedAt, 0)) > FlowTTL {
		_ = os.Remove(statePath)
		return Result{}, fmt.Errorf("登录超时（%d 分钟），请重试", int(FlowTTL.Minutes()))
	}

	q := url.Values{
		"nonce":            {st.Nonce},
		"verifier":         {st.Verifier},
		"challenge_method": {"S256"},
	}
	req, err := http.NewRequest(http.MethodGet, gatewayBase+PollPath+"?"+q.Encode(), nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Request-Id", uuid4Hex())
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	// 404 + x-device-flow-state: pending = 用户还没在浏览器里确认。
	if resp.StatusCode == http.StatusNotFound {
		return Result{}, ErrPending
	}
	if resp.StatusCode >= 400 {
		return Result{}, fmt.Errorf("设备授权轮询失败 http %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}

	var out struct {
		Token            string `json:"token"`
		DeviceToken      string `json:"device_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int64  `json:"expires_in"`
		RefreshExpiresIn int64  `json:"refresh_token_expires_in"`
		Nonce            string `json:"nonce"`
		Challenge        string `json:"code_challenge"`
		ChallengeMethod  string `json:"code_challenge_method"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Result{}, fmt.Errorf("设备授权响应解析失败: %w（body: %s）", err, truncate(string(raw), 200))
	}
	access := firstNonEmptyStr(out.Token, out.DeviceToken)
	if access == "" {
		return Result{}, fmt.Errorf("设备授权响应缺少 token（body: %s）", truncate(string(raw), 200))
	}
	// 绑定校验：响应必须属于本次 nonce + challenge，否则可能拿到别人的凭据。
	if out.Nonce != "" && out.Nonce != st.Nonce {
		return Result{}, fmt.Errorf("设备授权响应 nonce 不匹配（期望 %s，实际 %s）", st.Nonce, out.Nonce)
	}
	if out.Challenge != "" && out.Challenge != st.Challenge {
		return Result{}, fmt.Errorf("设备授权响应 code_challenge 不匹配")
	}

	defer func() { _ = os.Remove(statePath) }()
	res := Result{
		AccessToken:      access,
		RefreshToken:     out.RefreshToken,
		ExpiresIn:        out.ExpiresIn,
		RefreshExpiresIn: out.RefreshExpiresIn,
	}
	// token 端点不回 userinfo：uid/nickname 从 JWT 解，nickname 再由调用方兜底。
	res.UID, res.Nickname = parseJWTIdentity(access)
	return res, nil
}

// Shutdown 清理登录状态。设备流没有本机监听端口，取消/超时时只需丢弃状态文件
// （下一次 Start 会覆盖），故这里是空实现——保留该函数是为了与 loginFlows 表同形。
func Shutdown() {}

// ---------------------------------------------------------------------------
// 保存凭证（对齐 qoder 渠道的 SaveAuth 契约）
// ---------------------------------------------------------------------------

// SaveAuth 以嵌套形原子写 auth 文件（与 internal/auth.Parse 读取格式一致）。
// 文件名 qwenwork-<uid>.json。
func SaveAuth(authDir string, r Result) (string, error) {
	if r.UID == "" {
		return "", fmt.Errorf("missing uid in result")
	}
	// 到期时刻优先取 access token 的 JWT exp（服务端真实判据），其次 expires_in。
	expiresAt := jwtExpiry(r.AccessToken)
	if expiresAt == 0 {
		if r.ExpiresIn > 0 {
			expiresAt = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second).Unix()
		} else {
			expiresAt = time.Now().Add(7 * 24 * time.Hour).Unix() // 观测 JWT 寿命 7 天
		}
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  r.AccessToken,
			"refreshToken": r.RefreshToken,
			"expiresAt":    expiresAt,
			"domain":       "qwenwork.cn",
			"apiHost":      GatewayBase,
		},
		"account": map[string]any{
			"uid":      r.UID,
			"nickname": r.Nickname,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	_ = os.MkdirAll(authDir, 0o755)
	fp := filepath.Join(authDir, "qwenwork-"+r.UID+".json")
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, fp); err != nil {
		return "", err
	}
	return fp, nil
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// ensureMachineID 读取（或首次生成并落盘）稳定的机器标识。
// 设备流把它登记为「发起登录的设备」，每次登录换一个会让账号设备列表里堆满重复项。
func ensureMachineID(statePath string) (string, error) {
	dir := filepath.Dir(statePath)
	fp := filepath.Join(dir, machineIDFile)
	if raw, err := os.ReadFile(fp); err == nil {
		if v := strings.TrimSpace(string(raw)); v != "" {
			return v, nil
		}
	}
	id := uuid4Hex()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(fp, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

// makePKCE 生成 verifier + S256 challenge（43 字符 base64url，RFC 7636）。
func makePKCE() (verifier, challenge string) {
	buf := make([]byte, 48)
	_, _ = rand.Read(buf)
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	return verifier, pkceChallenge(verifier)
}

// pkceChallenge 计算 verifier 的 S256 challenge（拆出来便于用例校验配对关系）。
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// uuid4Hex 生成 UUID v4（nonce / machine_id / 请求号）。
func uuid4Hex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// jwtExpiry 取 JWT payload 的 exp；解析失败返回 0。
func jwtExpiry(token string) int64 {
	raw, ok := jwtPayload(token)
	if !ok {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return 0
	}
	return claims.Exp
}

// parseJWTIdentity 从 access token 解 uid 与 username。
func parseJWTIdentity(token string) (uid, name string) {
	raw, ok := jwtPayload(token)
	if !ok {
		return "", ""
	}
	var payload struct {
		Sub      string `json:"sub"`
		UserID   string `json:"user_id"`
		Username string `json:"username"`
		Name     string `json:"name"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return "", ""
	}
	uid = payload.UserID
	if uid == "" {
		uid = payload.Sub
	}
	return uid, firstNonEmptyStr(payload.Username, payload.Name)
}

// jwtPayload 解 JWT 的 payload 段（base64url 无填充 → 补齐到 4 的倍数）。
// 注意：Go 的 % 保留被除数符号，len%4!=0 时 strings.Repeat 负数会 panic
// （login_qwenwork 曾实测踩中：poll panic: strings: negative Repeat count）。
func jwtPayload(token string) ([]byte, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, false
	}
	p := parts[1]
	if r := len(p) % 4; r != 0 {
		p += strings.Repeat("=", 4-r)
	}
	raw, err := base64.URLEncoding.DecodeString(p)
	if err != nil {
		return nil, false
	}
	return raw, true
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

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}
