// Package login WorkBuddy OAuth 登录（国内版 / 国际版，移植自 cmd/login，供 GUI 内嵌使用）。
// 无 PKCE（workbuddy 设备流由服务端签发 state）。
package login

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 上游常量（国内版 / 国际版）。
const (
	UpstreamBaseCN      = "https://copilot.tencent.com"
	UpstreamBaseGlobal  = "https://www.workbuddy.ai"
	// 与聊天链同一客户端身份（官方桌面 5.8.3 内嵌 CLI 2.161.0，R117）。
	clientUA            = "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0"
	originRefererCN     = "https://www.codebuddy.cn"
	originRefererGlobal = "https://www.workbuddy.ai"
)

// Endpoints 一次登录流程用到的上游地址（国内版 / 国际版不同）。
type Endpoints struct {
	Base          string // API 基址
	Origin        string // 请求头 Origin/Referer 与浏览器登录页所在站点
	DefaultDomain string // token 响应缺 domain 时的兜底（国际版必须非空，否则账号会被判为 CN）
}

// EndpointsForRegion 按区域返回登录端点：region 为 "global" 时用国际版
// （www.workbuddy.ai），其余（含空）用国内版（copilot.tencent.com / codebuddy.cn）。
func EndpointsForRegion(region string) Endpoints {
	if strings.EqualFold(strings.TrimSpace(region), "global") {
		return Endpoints{
			Base:          UpstreamBaseGlobal,
			Origin:        originRefererGlobal,
			DefaultDomain: "www.workbuddy.ai",
		}
	}
	return Endpoints{Base: UpstreamBaseCN, Origin: originRefererCN}
}

func (e Endpoints) authStateURL() string { return e.Base + "/v2/plugin/auth/state?platform=CLI" }
func (e Endpoints) loginAcctURL() string { return e.Base + "/v2/plugin/login/account?state=" }
func (e Endpoints) authTokenURL() string { return e.Base + "/v2/plugin/auth/token?state=" }

// ErrPending 表示登录尚未完成（业务 code 非 0，浏览器还没登录完）。
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

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// commonHeaders 与 CLI 实现一致的请求头。
func commonHeaders(req *http.Request, ep Endpoints) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", ep.Origin)
	req.Header.Set("Referer", ep.Origin+"/")
	req.Header.Set("User-Agent", clientUA)
}

// doJSON 发请求并解 {code,msg,data} 信封；code!=0 → error。
// bearer 非空时附带 Authorization 头（login/account 需要）。
func doJSON(client *http.Client, method, fullURL string, ep Endpoints, bearer string, body io.Reader) (json.RawMessage, error) {
	req, err := http.NewRequest(method, fullURL, body)
	if err != nil {
		return nil, err
	}
	commonHeaders(req, ep)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream http %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))
	}
	return env.Data, nil
}

// Start 发起登录流程：POST auth/state 拿 state+授权 URL，state 落盘后返回 URL。
func Start(client *http.Client, statePath string, ep Endpoints) (string, error) {
	data, err := doJSON(client, http.MethodPost, ep.authStateURL(), ep, "", bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", fmt.Errorf("auth state failed: %w", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return "", fmt.Errorf("auth state: missing state or authUrl")
	}
	raw, _ := json.Marshal(map[string]string{"state": st.State})
	if dir := filepath.Dir(statePath); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		return "", fmt.Errorf("write state: %w", err)
	}
	return st.AuthURL, nil
}

// ResolveAuthURL 手动跟随登录页重定向链，返回最终 URL
// （copilot.tencent.com/login → 301 加斜杠 → www.codebuddy.cn/login/...），
// 浏览器可直接打开最终地址，跳过中间跳转。
func ResolveAuthURL(client *http.Client, rawURL string, ep Endpoints) (string, error) {
	noFollow := &http.Client{
		Timeout:   client.Timeout,
		Jar:       client.Jar,
		Transport: client.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse // 自己读 Location
		},
	}
	current := rawURL
	for hop := 0; hop < 5; hop++ {
		req, err := http.NewRequest(http.MethodGet, current, nil)
		if err != nil {
			return "", err
		}
		commonHeaders(req, ep)
		resp, err := noFollow.Do(req)
		if err != nil {
			return "", err
		}
		loc := resp.Header.Get("Location")
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if loc == "" {
			return current, nil
		}
		ref, err := url.Parse(loc)
		if err != nil {
			return "", err
		}
		base, err := url.Parse(current)
		if err != nil {
			return "", err
		}
		current = base.ResolveReference(ref).String()
	}
	return current, nil
}

// Poll 单次轮询登录状态；未完成返回 ErrPending；成功返回凭证并删除 state 文件。
func Poll(client *http.Client, statePath string, ep Endpoints) (Result, error) {
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

	// auth/token 是权威登录状态端点：pending 时业务 code 非 0（"login ing"）
	tokRaw, errTok := doJSON(client, http.MethodGet, ep.authTokenURL()+ls.State, ep, "", nil)
	if errTok != nil {
		if isPending(errTok) {
			return Result{}, ErrPending
		}
		return Result{}, errTok
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.AccessToken == "" {
		return Result{}, ErrPending
	}
	// 国际版 token 响应若缺 domain，按区域兜底，避免账号被判为 CN 而无法被加载
	if tok.Domain == "" {
		tok.Domain = ep.DefaultDomain
	}

	// login/account 拿 uid/nickname（带 Bearer）
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if acctRaw, errAcct := doJSON(client, http.MethodGet, ep.loginAcctURL()+ls.State, ep, tok.AccessToken, nil); errAcct == nil {
		_ = json.Unmarshal(acctRaw, &acct)
	}
	_ = os.Remove(statePath)
	return Result{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresIn:    tok.ExpiresIn,
		Domain:       tok.Domain,
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname:     acct.Nickname,
	}, nil
}

// isPending 业务错误码（如 "login ing"）视为尚未完成。
func isPending(err error) bool {
	s := err.Error()
	return (strings.Contains(s, "code=") || strings.Contains(s, "login")) &&
		!strings.Contains(s, "http ") && !strings.Contains(s, "parse")
}

// SaveAuth 以嵌套形原子写 auth 文件（与 internal/auth.Parse 读取格式一致）。
func SaveAuth(authDir string, r Result) (string, error) {
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
			"domain":       r.Domain,
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
	fp := filepath.Join(authDir, "workbuddy-"+r.UID+".json")
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, fp); err != nil {
		return "", err
	}
	return fp, nil
}

func truncate(s string, n int) string {
	s = string(bytes.TrimSpace([]byte(s)))
	if len(s) > n {
		return string(s[:n])
	}
	return string(s)
}
