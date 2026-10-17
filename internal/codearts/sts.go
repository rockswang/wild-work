// sts.go 华为云 STS 凭据换取：OAuth 授权码（登录）与 refresh_token（静默续期）。
//
// 协议事实（§2.1、§2.2）：
//   - 端点 `POST https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens`，**form 编码**；
//   - 授权码：client_id=codearts-agent、code、code_verifier、grant_type=authorization_code、
//     redirect_uri=http://127.0.0.1:<port>/oauth/callback；
//   - 续期：client_id、code_verifier、grant_type=refresh_token、refresh_token；
//   - 鉴权靠 **DPoP**（请求头 `DPoP: <jws>`），不是 AK/SK 签名；
//   - 终态判据只认 `error === 'invalid_grant'` 或 `error_code` 含 `ExpiredRefreshToken`；
//     **`InvalidDPoPHeader` 不算终态**（时钟偏差/重放/抖动都是一次性拒绝）。
//
// 零 token 落盘：STS 的成功响应体里就是凭据本体，故所有错误文案**只**透出上游自己声明的
// error/error_code/error_msg/error_description 字段，绝不回显响应体原文（AGENTS.md §0.2）。
package codearts

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// PKCEVerifierBytes PKCE code_verifier 的随机字节数（协议 §2.2：48 字节 base64url）。
const PKCEVerifierBytes = 48

// defaultCredTTL 上游未给 expiration 时的保守存活时间。
//
// 绝不能留 0：auth.NeedsRefresh 对 ExpiresAt<=0 恒返回真 ⇒ 每次请求都刷凭据，
// 等于用 STS 做压力测试。宁可取一个偏短的值（提前续期是安全的，反之后果是
// 用过期凭据打上游拿 401）。
const defaultCredTTL = 30 * time.Minute

// NewPKCE 生成 PKCE 对（verifier + S256 challenge，均无填充 base64url）。
//
// 注意：**challenge 的算法是 SHA-256**（RFC 7636 的 S256），而授权页的参数值要写字面量
// `SHA-256`（见 CodeChallengeMethod）——两者是不同的东西，别混。
func NewPKCE() (verifier, challenge string, err error) {
	buf := make([]byte, PKCEVerifierBytes)
	if _, err = rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("codearts: 生成 PKCE verifier 失败: %w", err)
	}
	verifier = b64url(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = b64url(sum[:])
	return verifier, challenge, nil
}

// stsEnvelope STS 响应（成功取 credentials/refresh_token，失败取 error* 字段）。
type stsEnvelope struct {
	Credentials struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
		SecurityToken   string `json:"security_token"`
		// Expiration 是 ISO8601 串（协议 §2.7）。
		Expiration string `json:"expiration"`
	} `json:"credentials"`
	RefreshToken     string `json:"refresh_token"`
	Error            string `json:"error"`
	ErrorCode        string `json:"error_code"`
	ErrorMsg         string `json:"error_msg"`
	ErrorDescription string `json:"error_description"`
}

// ExchangeAuthCode 用授权码换临时凭据（登录流程最后一步）。
func ExchangeAuthCode(client *http.Client, stsURL, code, codeVerifier, redirectURI string, key *DPoPKey, now time.Time) (Credential, error) {
	form := url.Values{}
	form.Set("client_id", OAuthClientID)
	form.Set("code", code)
	form.Set("code_verifier", codeVerifier)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", redirectURI)
	return doSTS(client, stsURL, form, key, now)
}

// RefreshCredential 用 refresh_token 静默续期。
func RefreshCredential(client *http.Client, stsURL, refreshToken, codeVerifier string, key *DPoPKey, now time.Time) (Credential, error) {
	form := url.Values{}
	form.Set("client_id", OAuthClientID)
	form.Set("code_verifier", codeVerifier)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	return doSTS(client, stsURL, form, key, now)
}

// doSTS 发出一次 DPoP 认证的 STS 请求并解析凭据。
func doSTS(client *http.Client, stsURL string, form url.Values, key *DPoPKey, now time.Time) (Credential, error) {
	if client == nil {
		client = &http.Client{Timeout: oauthTimeout}
	}
	if key == nil {
		return Credential{}, &provider.Error{Kind: provider.ErrSessionDead, Msg: "codearts：缺 DPoP 私钥，无法续期/换凭据"}
	}
	proof, err := key.Proof(http.MethodPost, stsURL, now)
	if err != nil {
		return Credential{}, err
	}
	req, err := http.NewRequest(http.MethodPost, stsURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Credential{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("DPoP", proof)

	resp, err := client.Do(req)
	if err != nil {
		return Credential{}, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var env stsEnvelope
	_ = json.Unmarshal(raw, &env)
	if resp.StatusCode >= 400 {
		return Credential{}, stsError(resp.StatusCode, env)
	}
	if strings.TrimSpace(env.Credentials.AccessKeyID) == "" || strings.TrimSpace(env.Credentials.SecretAccessKey) == "" {
		// HTTP 200 但没有凭据：上游把错误放在 error* 字段里（协议 §2.7 同款宽容解析）。
		if e := stsError(resp.StatusCode, env); e != nil {
			return Credential{}, e
		}
		return Credential{}, &provider.Error{Kind: provider.ErrClient, Status: resp.StatusCode,
			Msg: "codearts：STS 响应缺 credentials.access_key_id/secret_access_key"}
	}
	exp := now.Add(defaultCredTTL).Unix()
	if t, ok := parseISOTime(env.Credentials.Expiration); ok {
		exp = t
	}
	return Credential{
		AccessKeyID:     env.Credentials.AccessKeyID,
		SecretAccessKey: env.Credentials.SecretAccessKey,
		SecurityToken:   env.Credentials.SecurityToken,
		ExpiresAt:       exp,
		RefreshToken:    strings.TrimSpace(env.RefreshToken),
	}, nil
}

// stsError 把 STS 失败映射成带分类的错误。
//
// ⚠️ 只透出上游自报的错误字段：成功路径的响应体就是凭据，错误路径也可能带上，
// 故**绝不**拼接响应体原文。
func stsError(status int, env stsEnvelope) error {
	text := firstNonEmpty(env.Error, env.ErrorCode, env.ErrorMsg, env.ErrorDescription)
	lowErr := strings.ToLower(env.Error)
	lowCode := strings.ToLower(env.ErrorCode)
	msg := "codearts：STS 请求失败"
	if text != "" {
		msg += "（" + snippet([]byte(text)) + "）"
	}
	switch {
	case lowErr == "invalid_grant" || strings.Contains(lowCode, "expiredrefreshtoken"):
		// 唯一两种终态（协议 §2.7）：refresh_token 已死，需要用户重新登录。
		return &provider.Error{Kind: provider.ErrSessionDead, Status: status, Msg: msg}
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return &provider.Error{Kind: provider.ErrSessionDead, Status: status, Msg: msg}
	case strings.Contains(lowErr, "invaliddpopheader") || strings.Contains(lowCode, "invaliddpopheader"):
		// 刻意**不**算终态：时钟偏差/重放/抖动都会命中，属一次性拒绝。
		return &provider.Error{Kind: provider.ErrClient, Status: status, Msg: msg + "（DPoP 被拒，可重试）"}
	case status >= 500:
		return &provider.Error{Kind: provider.ErrServer, Status: status, Msg: msg}
	default:
		return &provider.Error{Kind: provider.ErrClient, Status: status, Msg: msg}
	}
}

// ---------------------------------------------------------------------------
// Client.RefreshToken（provider.Upstream）
// ---------------------------------------------------------------------------

// RefreshToken 用 refresh_token + DPoP 静默续期（协议 §2.7「续期（静默）」）。
//
// 三类前置缺失都直接判**会话终态**（ErrSessionDead）：没有 refresh_token
// （旧 ticket 凭据不可静默续期）、没有 code_verifier、没有 DPoP 私钥——
// 这三样任一缺失都不可能靠重试或换号补救。
//
// ⚠️ 调用方**必须**在返回 nil 之后立刻落盘（auth.Auth.SaveAtomic，不变量 20）：
// 上游会轮换 refresh_token，不落盘 = 下次启动用旧值，直接失效。
func (c *Client) RefreshToken(a *auth.Auth) error {
	if a == nil {
		return &provider.Error{Kind: provider.ErrSessionDead, Msg: "codearts：账号为空"}
	}
	a.Lock()
	defer a.Unlock()

	ac := accountFromAuthLocked(a)
	switch {
	case strings.TrimSpace(ac.RefreshToken) == "":
		return &provider.Error{Kind: provider.ErrSessionDead,
			Msg: "codearts：账号没有 refresh_token（旧 ticket 凭据无法静默续期，需重新登录）"}
	case strings.TrimSpace(ac.CodeVerifier) == "":
		return &provider.Error{Kind: provider.ErrSessionDead, Msg: "codearts：凭据缺 code_verifier，无法续期"}
	case strings.TrimSpace(ac.DPoPPrivateKeyJWK) == "":
		return &provider.Error{Kind: provider.ErrSessionDead, Msg: "codearts：凭据缺 DPoP 私钥，无法续期"}
	}
	key, err := ParseDPoPKey(ac.DPoPPrivateKeyJWK)
	if err != nil {
		// 私钥损坏同样不可自愈：没有它换不出新凭据。
		return &provider.Error{Kind: provider.ErrSessionDead, Msg: "codearts：" + err.Error()}
	}
	cred, err := RefreshCredential(c.httClient(), c.stsURL(), ac.RefreshToken, ac.CodeVerifier, key, time.Now())
	if err != nil {
		return err
	}
	// 上游未回新 refresh_token 时保留原值（协议 §2.7：refresh_token 会轮换，但缺失
	// 不代表作废——照 officeace 同款容差处理）。
	if strings.TrimSpace(cred.RefreshToken) == "" {
		cred.RefreshToken = ac.RefreshToken
	}
	ac.AccessKeyID = cred.AccessKeyID
	ac.SecretAccessKey = cred.SecretAccessKey
	ac.SecurityToken = cred.SecurityToken
	ac.ExpiresAt = cred.ExpiresAt
	ac.RefreshToken = cred.RefreshToken
	applyAccountLocked(a, ac)
	return nil
}

// applyAccountLocked 把 Account 写回 auth.Auth（含 Extra 整体重建）。
// 调用方必须已持 a 的写锁（本函数内不再加锁）。
func applyAccountLocked(a *auth.Auth, ac *Account) {
	a.AccessToken = ac.AccessKeyID
	a.RefreshToken = ac.RefreshToken
	a.ExpiresAt = ac.ExpiresAt
	if strings.TrimSpace(a.UID) == "" {
		a.UID = ac.ID
	}
	if strings.TrimSpace(a.Nickname) == "" {
		a.Nickname = ac.Label()
	}
	a.Domain = domainConst
	a.ApiHost = APIBase
	a.Extra = ac.extra()
}
