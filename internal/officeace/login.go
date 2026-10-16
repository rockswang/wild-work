package officeace

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
	"strings"
	"sync"
	"time"

	"wild-work/internal/platform"
)

// 自助 OAuth 登录：不依赖本机 OfficeAce 桌面端，自己跑完华为云授权码 + PKCE + DPoP。
//
//	① POST {cloud}/v1/claw/auth/state            → {state}
//	② 本地生成 PKCE + P-256 DPoP 密钥对
//	③ 拼 authorizeUrl，让用户在浏览器登录
//	④ GET  {cloud}/v1/claw/auth/code?state=      轮询取 code（202 未就绪 / 404 失效 / 200 成功）
//	⑤ POST {sts}/v1/oauth2/tokens                code+verifier → 临时 AK/SK + refresh_token
//	⑥ GET  {cloud}/v1/claw/client-permission-validate → 网关地址 + model_app_key/secret
//	⑦ 落盘 auths/officeace-<uid>.json

const (
	authBase    = "https://auth.huaweicloud.com"
	redirectURI = "https://agentarts.cn-southwest-2.myhuaweicloud.com/v1/claw/auth/callback"
	bgImgURL    = "https://res.hc-cdn.com/AgentArts-Console/26.4.7/hws/assets/login/bg2.png"
	adImgURL    = "https://res.hc-cdn.com/AgentArts-Console/26.4.7/hws/assets/login/ad2.png"

	loginTTL      = 10 * time.Minute
	pollInterval  = 2500 * time.Millisecond
	maxPollErrors = 60
)

// LoginResult 登录成功结果（供面板展示）。
type LoginResult struct {
	UID      string
	Nickname string
}

// LoginSession 一次自助登录会话。
type LoginSession struct {
	mu      sync.Mutex
	status  string // pending | exchanging | done | failed | expired | canceled
	err     error
	result  LoginResult
	url     string
	cancel  chan struct{}
	stopped bool
	httpc   *http.Client
}

// StartLogin 发起自助登录：返回会话（已含待打开的授权 URL），后台轮询完成登录。
func StartLogin(authDir string, onDone func(LoginResult)) (*LoginSession, error) {
	s := &LoginSession{status: "pending", cancel: make(chan struct{}), httpc: &http.Client{Timeout: 30 * time.Second}}

	// ① 云端签发 state
	state, err := fetchState(s.httpc)
	if err != nil {
		return nil, err
	}
	// ② PKCE + DPoP
	verifier, challenge, err := newPKCE()
	if err != nil {
		return nil, err
	}
	dpop, err := NewDPoPKey()
	if err != nil {
		return nil, err
	}
	// ③ 授权 URL
	s.url = buildAuthorizeURL(state, challenge)

	go s.run(authDir, state, verifier, dpop, onDone)
	return s, nil
}

// URL 返回待用户在浏览器打开的授权地址。
func (s *LoginSession) URL() string { return s.url }

// Status 返回当前状态与错误。
func (s *LoginSession) Status() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.err
}

// Result 在成功后返回结果。
func (s *LoginSession) Result() (LoginResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == "done" {
		return s.result, true
	}
	return LoginResult{}, false
}

// Cancel 取消登录。
func (s *LoginSession) Cancel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopped {
		close(s.cancel)
		s.stopped = true
	}
	if s.status == "pending" || s.status == "exchanging" {
		s.status = "canceled"
	}
}

func (s *LoginSession) setStatus(st string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = st
	s.err = err
}

func (s *LoginSession) run(authDir, state, verifier string, dpop *DPoPKey, onDone func(LoginResult)) {
	deadline := time.Now().Add(loginTTL)
	// ④ 轮询取码
	var code string
	fails := 0
	for {
		select {
		case <-s.cancel:
			return
		default:
		}
		if time.Now().After(deadline) {
			s.setStatus("expired", errors.New("登录超时，请重试"))
			return
		}
		c, retry, err := pollCode(s.httpc, state)
		if err != nil {
			fails++
			if fails >= maxPollErrors {
				s.setStatus("failed", err)
				return
			}
		} else if c != "" {
			code = c
			break
		}
		if !retry && err != nil && strings.Contains(err.Error(), "state") {
			s.setStatus("expired", err)
			return
		}
		select {
		case <-s.cancel:
			return
		case <-time.After(pollInterval):
		}
	}

	// ⑤ 换令牌
	s.setStatus("exchanging", nil)
	tok, err := exchangeCode(s.httpc, code, verifier, dpop)
	if err != nil {
		s.setStatus("failed", err)
		return
	}
	// ⑥ 取网关凭据
	base, appKey, appSecret, accountID, userID, err := validateAndGateway(s.httpc, tok, dpop)
	if err != nil {
		s.setStatus("failed", err)
		return
	}
	if accountID == "" {
		accountID = tok.UserID
	}
	if userID == "" {
		userID = tok.UserID
	}
	id := accountID + ":" + userID
	ac := &Account{
		ID:              id,
		AccountID:       accountID,
		UserID:          userID,
		Label:           tok.UserName,
		BaseURL:         trimSlash(base),
		Authorization:   "Basic " + base64.StdEncoding.EncodeToString([]byte(appKey+":"+appSecret)),
		AccessKeyID:     tok.AccessKeyID,
		SecretAccessKey: tok.SecretAccessKey,
		SecurityToken:   tok.SecurityToken,
		ProjectID:       tok.ProjectID,
		CredExpiresAt:   tok.ExpiresAt,
		RefreshToken:    tok.RefreshToken,
		RefreshExpire:   tok.RefreshExpire,
		DPoPPrivateJWK:  dpop.jwkJSON(),
		Region:          regionFromBaseURL(base),
		Source:          "oauth-login",
	}
	if _, err := WriteAuthFile(authDir, ac); err != nil {
		s.setStatus("failed", fmt.Errorf("保存凭据失败：%w", err))
		return
	}
	s.mu.Lock()
	s.result = LoginResult{UID: id, Nickname: tok.UserName}
	s.status = "done"
	s.mu.Unlock()
	if onDone != nil {
		onDone(s.result)
	}
}

// OpenInBrowser 用系统默认浏览器打开授权 URL。
func (s *LoginSession) OpenInBrowser() error { return platform.OpenURL(s.url) }

// ---------------------------------------------------------------------------
// 各步骤实现
// ---------------------------------------------------------------------------

func fetchState(c *http.Client) (string, error) {
	u := agentartsBase + "/v1/claw/auth/state"
	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader("{}"))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("获取 state 失败 HTTP %d: %s", resp.StatusCode, truncate(rb, 200))
	}
	var out struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", err
	}
	if out.State == "" {
		return "", errors.New("云端未返回 state")
	}
	return out.State, nil
}

func newPKCE() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func buildAuthorizeURL(state, challenge string) string {
	inner, _ := url.Parse(authBase + "/authui/v1/oauth2/authorize")
	q := inner.Query()
	q.Set("client_id", oauthClientID)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "SHA-256")
	q.Set("state", state)
	q.Set("scope", "openid")
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	inner.RawQuery = q.Encode()

	outer, _ := url.Parse(authBase + "/authui/login.html")
	oq := outer.Query()
	oq.Set("service", inner.String())
	oq.Set("background_img_url", bgImgURL)
	oq.Set("portal_img_url", adImgURL)
	outer.RawQuery = oq.Encode()
	return outer.String()
}

// pollCode 轮询取授权码：返回 (code, retryable, err)。code 非空即拿到。
func pollCode(c *http.Client, state string) (string, bool, error) {
	u := agentartsBase + "/v1/claw/auth/code?state=" + url.QueryEscape(state)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", true, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return "", true, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	switch resp.StatusCode {
	case http.StatusOK:
		var out struct {
			Code              string `json:"code"`
			AuthorizationCode string `json:"authorization_code"`
		}
		_ = json.Unmarshal(rb, &out)
		c2 := out.Code
		if c2 == "" {
			c2 = out.AuthorizationCode
		}
		return c2, false, nil
	case http.StatusAccepted: // 202：还没登完
		return "", true, nil
	case http.StatusNotFound: // state 失效
		return "", false, errors.New("state 已失效")
	default:
		return "", true, fmt.Errorf("取码 HTTP %d", resp.StatusCode)
	}
}

type tokenResult struct {
	AccessKeyID     string
	SecretAccessKey string
	SecurityToken   string
	ProjectID       string
	ExpiresAt       int64
	RefreshToken    string
	RefreshExpire   int64
	UserID          string
	UserName        string
}

func exchangeCode(c *http.Client, code, verifier string, dpop *DPoPKey) (*tokenResult, error) {
	form := url.Values{}
	form.Set("client_id", oauthClientID)
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", redirectURI)
	return postToken(c, form, dpop)
}

func postToken(c *http.Client, form url.Values, dpop *DPoPKey) (*tokenResult, error) {
	req, err := http.NewRequest(http.MethodPost, stsTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	proof, err := dpop.DPoPProof(http.MethodPost, stsTokenURL, time.Now())
	if err != nil {
		return nil, err
	}
	req.Header.Set("DPoP", proof)
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("换令牌失败 HTTP %d: %s", resp.StatusCode, truncate(rb, 300))
	}
	var out struct {
		Credentials struct {
			AccessKeyID     string `json:"access_key_id"`
			SecretAccessKey string `json:"secret_access_key"`
			SecurityToken   string `json:"security_token"`
			ProjectID       string `json:"project_id"`
			Expiration      string `json:"expiration"`
		} `json:"credentials"`
		RefreshToken   string `json:"refresh_token"`
		RefreshExpires string `json:"refresh_expires_at"`
		IDToken        string `json:"id_token"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, err
	}
	tr := &tokenResult{
		AccessKeyID:     out.Credentials.AccessKeyID,
		SecretAccessKey: out.Credentials.SecretAccessKey,
		SecurityToken:   out.Credentials.SecurityToken,
		ProjectID:       out.Credentials.ProjectID,
		RefreshToken:    out.RefreshToken,
	}
	if t, ok := parseISO(out.Credentials.Expiration); ok {
		tr.ExpiresAt = t / 1000
	}
	if t, ok := parseISO(out.RefreshExpires); ok {
		tr.RefreshExpire = t / 1000
	}
	if out.IDToken != "" {
		tr.UserName = jwtClaimStr(out.IDToken, "preferred_username")
		if tr.UserName == "" {
			tr.UserName = jwtClaimStr(out.IDToken, "name")
		}
		tr.UserID = jwtClaimStr(out.IDToken, "sub")
	}
	return tr, nil
}

// validateAndGateway 用临时凭据调 client-permission-validate，取网关地址与 app key/secret。
func validateAndGateway(c *http.Client, tok *tokenResult, dpop *DPoPKey) (base, appKey, appSecret, accountID, userID string, err error) {
	ac := &Account{AccessKeyID: tok.AccessKeyID, SecretAccessKey: tok.SecretAccessKey,
		SecurityToken: tok.SecurityToken, ProjectID: tok.ProjectID}
	u := agentartsBase + "/v1/claw/client-permission-validate"
	hdrs, herr := SignV4(ac.AccessKeyID, ac.SecretAccessKey, ac.SecurityToken, ac.ProjectID,
		http.MethodGet, u, "", "application/json;charset=utf8", nil, time.Now())
	if herr != nil {
		return "", "", "", "", "", herr
	}
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", "", "", "", "", err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", "", "", "", "", fmt.Errorf("校验凭据失败 HTTP %d: %s", resp.StatusCode, truncate(rb, 300))
	}
	var out struct {
		AccountID    string `json:"account_id"`
		PrincipalID  string `json:"principal_id"`
		Subscription struct {
			ModelInfo struct {
				ModelAPIURLBase string `json:"model_api_url_base"`
				ModelAuthInfo   struct {
					ModelAppKey    string `json:"model_app_key"`
					ModelAppSecret string `json:"model_app_secret"`
				} `json:"model_auth_info"`
			} `json:"model_info"`
		} `json:"subscription"`
		ModelInfo struct {
			ModelAPIURLBase string `json:"model_api_url_base"`
			ModelAuthInfo   struct {
				ModelAppKey    string `json:"model_app_key"`
				ModelAppSecret string `json:"model_app_secret"`
			} `json:"model_auth_info"`
		} `json:"model_info"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return "", "", "", "", "", err
	}
	mi := out.ModelInfo
	if mi.ModelAuthInfo.ModelAppKey == "" && out.Subscription.ModelInfo.ModelAuthInfo.ModelAppKey != "" {
		mi = out.Subscription.ModelInfo
	}
	if mi.ModelAuthInfo.ModelAppKey == "" {
		return "", "", "", "", "", errors.New("该账号未开通服务（需先兑换邀请码）")
	}
	base = mi.ModelAPIURLBase
	if !strings.HasPrefix(base, "http") && base != "" {
		base = "https://" + base
	}
	return trimSlash(base), mi.ModelAuthInfo.ModelAppKey, mi.ModelAuthInfo.ModelAppSecret, out.AccountID, out.PrincipalID, nil
}

func jwtClaimStr(tok, key string) string {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// jwkJSON 返回 DPoP 私钥 JWK 的 JSON 串。
func (k *DPoPKey) jwkJSON() string {
	b, _ := json.Marshal(k.privateJWK())
	return string(b)
}
