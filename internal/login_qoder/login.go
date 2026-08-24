// Package login_qoder 实现 QoderWork CN OAuth 设备授权登录（PKCE Device Flow）。
// 移植自 qoderwork2api cmd/login：先本地生成 PKCE+nonce+machineID，
// 状态落盘后返回授权 URL；浏览器完成授权后 Poll 用 verifier 换 dt-/drt- 凭据。
package login_qoder

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 与 qoderwork2api cmd/login 一致的常量（CN only）。
const (
	OAuthWebsiteCN = "https://qoder.com.cn"
	OAuthOpenapiCN = "https://openapi.qoder.com.cn"
	OAuthClientID  = "1c5e33e1-364d-4ce6-b02c-acaa81274a5c"
	OAuthRedirect  = "qoder-work-cn://"
	clientUA       = "Go-http-client/2.0"
)

// ErrPending 表示授权尚未完成。
var ErrPending = errors.New("login pending")

// Result 登录成功后拿到的凭证与账号信息。
type Result struct {
	AccessToken  string // dt-
	RefreshToken string // drt-
	ExpiresIn    int64  // 秒
	UID          string
	Nickname     string
}

// state 落盘的 PKCE 状态。
type state struct {
	Verifier  string `json:"verifier"`
	Nonce     string `json:"nonce"`
	MachineID string `json:"machine_id"`
	AuthURL   string `json:"auth_url"`
}

// NewClient 简单 HTTP 客户端（无 cookie 需求）。
func NewClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// makePKCE 生成 verifier + S256 challenge（与 qoderwork2api 一致，含模 66 采样）。
func makePKCE() (verifier, challenge string) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"
	buf := make([]byte, 64)
	_, _ = rand.Read(buf)
	var sb strings.Builder
	sb.Grow(64)
	for _, b := range buf {
		sb.WriteByte(alphabet[int(b)%len(alphabet)])
	}
	verifier = sb.String()
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Start 发起登录：生成 PKCE 状态落盘，返回授权 URL。
func Start(client *http.Client, statePath string) (string, error) {
	verifier, challenge := makePKCE()
	nonce := newUUID()
	machineID := newUUID()
	authURL := fmt.Sprintf("%s/device/selectAccounts?challenge=%s&challenge_method=S256&nonce=%s&machine_id=%s&client_id=%s&redirect_uri=%s",
		OAuthWebsiteCN, challenge, nonce, machineID, OAuthClientID, OAuthRedirect)
	st := state{Verifier: verifier, Nonce: nonce, MachineID: machineID, AuthURL: authURL}
	if err := writeState(statePath, st); err != nil {
		return "", err
	}
	return authURL, nil
}

// Poll 单次轮询设备令牌；未完成返回 ErrPending；成功返回 dt-/drt- 并删除 state。
func Poll(client *http.Client, statePath string) (Result, error) {
	raw, err := os.ReadFile(statePath)
	if err != nil {
		return Result{}, fmt.Errorf("read state: %w", err)
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil || st.Verifier == "" || st.Nonce == "" {
		return Result{}, fmt.Errorf("parse state: %w", err)
	}
	url := fmt.Sprintf("%s/api/v1/deviceToken/poll?nonce=%s&verifier=%s&challenge_method=S256",
		OAuthOpenapiCN, st.Nonce, st.Verifier)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	raw, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusAccepted {
		return Result{}, ErrPending
	}
	if resp.StatusCode >= 400 {
		return Result{}, fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var tok map[string]any
	if err := json.Unmarshal(raw, &tok); err != nil {
		return Result{}, fmt.Errorf("parse: %w", err)
	}
	t := getString(tok, "token")
	if t == "" {
		t = getString(tok, "device_token")
	}
	if t == "" {
		return Result{}, ErrPending
	}
	refresh := getString(tok, "refresh_token")
	uid := getString(tok, "user_id")
	if uid == "" {
		uid = getString(tok, "uid")
	}
	expiresIn := int64(getFloat(tok, "expires_in"))
	_ = os.Remove(statePath)
	return Result{AccessToken: t, RefreshToken: refresh, ExpiresIn: expiresIn, UID: uid}, nil
}

// SaveAuth 以嵌套形原子写 auth 文件（与 internal/auth.Parse 读取格式一致）。
// 文件名 qoder-<uid>.json；机器指纹由调用方在写入前生成并传入。
func SaveAuth(authDir string, r Result, machineID, machineToken, machineType string) (string, error) {
	if r.UID == "" {
		return "", fmt.Errorf("missing uid in result")
	}
	expiresAt := int64(0)
	if r.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second).Unix()
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  r.AccessToken,
			"refreshToken": r.RefreshToken,
			"expiresAt":    expiresAt,
			"domain":       "qoder.com.cn",
			"machineId":    machineID,
			"machineToken": machineToken,
			"machineType":  machineType,
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
	fp := filepath.Join(authDir, "qoder-"+r.UID+".json")
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

func getFloat(m map[string]any, key string) float64 {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f
		}
	case int64:
		return float64(n)
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
