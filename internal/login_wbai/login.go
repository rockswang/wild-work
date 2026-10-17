// Package loginwbai WorkBuddy 国际版（www.workbuddy.ai）OAuth 登录编排。
//
// 与国内版 internal/login 完全独立实现：路径相同但 host 不同，
// 且国际版在 auth/token 未完成时固定返回 code=11217（"login ing..."），
// 该语义直接作为 pending 判据，无需国内版那套文本启发式判断。
package loginwbai

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wild-work/internal/workbuddyai"
)

// ErrPending 登录尚未完成（浏览器还没登录完）。
var ErrPending = errors.New("login pending")

// Result 登录成功后拿到的凭证与账号信息。
type Result struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	Domain       string
	UID          string
	EnterpriseID string
	Nickname     string
}

// NewClient 每个登录流程独立的 cookie jar（多账号登录互不串会话）。
func NewClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Timeout: 30 * time.Second, Jar: jar}
}

// upstream 返回登录用客户端（独立实例，避免与账密池共享状态）。
func upstream() *workbuddyai.Client { return workbuddyai.NewWithTimeout(30 * time.Second) }

// Start 发起登录流程：POST auth/state 拿 state+授权 URL，state 落盘后返回 URL。
func Start(_ *http.Client, statePath string) (string, error) {
	c := upstream()
	state, authURL, err := c.StartLogin()
	if err != nil {
		return "", err
	}
	raw, _ := json.Marshal(map[string]string{"state": state})
	if dir := filepath.Dir(statePath); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		return "", fmt.Errorf("write state: %w", err)
	}
	return authURL, nil
}

// Poll 单次轮询登录状态；未完成返回 ErrPending；成功返回凭证并删除 state 文件。
func Poll(_ *http.Client, statePath string) (Result, error) {
	raw, err := os.ReadFile(statePath)
	if err != nil {
		return Result{}, fmt.Errorf("read state: %w", err)
	}
	var ls struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(raw, &ls); err != nil || ls.State == "" {
		return Result{}, fmt.Errorf("parse state: %w", err)
	}
	c := upstream()
	r, pending, err := c.PollLogin(ls.State)
	if err != nil {
		return Result{}, err
	}
	if pending {
		return Result{}, ErrPending
	}
	_ = os.Remove(statePath)
	return Result{
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		ExpiresIn:    r.ExpiresIn,
		Domain:       r.Domain,
		UID:          r.UID,
		EnterpriseID: r.EnterpriseID,
		Nickname:     r.Nickname,
	}, nil
}

// SaveAuth 以嵌套形原子写 auth 文件（与 internal/auth.Parse 读取格式一致），
// 文件名前缀 workbuddyai- 以便 LoadWorkBuddyAiDir 识别。
func SaveAuth(authDir string, r Result) (string, error) {
	if r.UID == "" {
		return "", fmt.Errorf("missing uid in result")
	}
	expiresAt := int64(0)
	if r.ExpiresIn > 0 {
		expiresAt = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second).Unix()
	}
	domain := strings.TrimSpace(r.Domain)
	if domain == "" {
		domain = "www.workbuddy.ai"
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  r.AccessToken,
			"refreshToken": r.RefreshToken,
			"expiresAt":    expiresAt,
			"domain":       domain,
		},
		"account": map[string]any{
			"uid":          r.UID,
			"enterpriseId": r.EnterpriseID,
			"nickname":     r.Nickname,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	_ = os.MkdirAll(authDir, 0o755)
	fp := filepath.Join(authDir, "workbuddyai-"+r.UID+".json")
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, fp); err != nil {
		return "", err
	}
	return fp, nil
}
