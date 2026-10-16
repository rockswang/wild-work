package officeace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
)

// refreshToken 用 refresh_token 经 DPoP 向华为 STS 换新的临时凭据，并**落盘**。
//
// refresh_token 是**一次性**的：用一次即废（重放会 STS5.1806 "the refresh token has been used"）。
// 本渠道的持久化策略：
//   - 最新可用 token 恒存 Auth.RefreshToken；
//   - 服务器明确拒过（STS5.1806）的 token 记入 Extra["dead_tokens"]，绝不再发；
//   - 单账号续期单飞（single-flight），防止并发把同一一次性 token 用两次。
//
// 与参考实现的差异（如实说明）：未实现「读桌面端只读档案那条线（diskTokenUsed）与
// 本工具轮换线并存」的多源挑选——wild-work 的 auth 文件就是唯一来源，磁盘档案不再参与，
// 故无需多线取舍。若同一账号的 refresh_token 被本工具与 OfficeAce 桌面端**共用**，
// 仍可能互踢；此时需在本工具重新登录一次以获得独立链路。
func (c *Client) refreshToken(a *auth.Auth) error {
	uid := a.UID
	mu := refreshLockFor(uid)
	mu.Lock()
	defer mu.Unlock()

	ac := accountFromAuth(a)
	rt := strings.TrimSpace(a.RefreshTokenValue())
	if rt == "" {
		return fmt.Errorf("officeace: no refresh_token")
	}
	if containsCSV(acDeadTokens(a), rt) {
		return errors.New("officeace: refresh_token 已失效（被上游拒绝过），请重新登录")
	}

	key, err := c.dpopKey(ac)
	if err != nil {
		return err
	}
	form := url.Values{}
	form.Set("client_id", oauthClientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", rt)

	req, err := http.NewRequest(http.MethodPost, stsTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	proof, err := key.DPoPProof(http.MethodPost, stsTokenURL, time.Now())
	if err != nil {
		return err
	}
	req.Header.Set("DPoP", proof)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		// STS5.1806/1807：refresh_token 已被使用/失效 → 记入死名单并落盘，避免重放。
		if strings.Contains(string(rb), "STS5.1806") || strings.Contains(string(rb), "STS5.1807") ||
			strings.Contains(strings.ToLower(string(rb)), "has been used") {
			addDeadToken(a, rt)
			_ = a.SaveAtomic()
			return errors.New("officeace: refresh_token 已被消费（可能被别处使用），请重新登录")
		}
		return fmt.Errorf("officeace: refresh HTTP %d: %s", resp.StatusCode, truncate(rb, 300))
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
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return err
	}
	if out.Credentials.AccessKeyID == "" {
		return errors.New("officeace: refresh 响应缺少凭据")
	}

	ac.AccessKeyID = out.Credentials.AccessKeyID
	ac.SecretAccessKey = out.Credentials.SecretAccessKey
	ac.SecurityToken = out.Credentials.SecurityToken
	ac.ProjectID = out.Credentials.ProjectID
	if t, ok := parseISO(out.Credentials.Expiration); ok {
		ac.CredExpiresAt = t / 1000
	}
	// 先落盘轮换 token，再判凭据完整（顺序反了会「旧 token 废了、新 token 丢了」永久断链）。
	if out.RefreshToken != "" {
		ac.RefreshToken = out.RefreshToken
		addDead(a, rt) // 旧 token 已消费
	}
	if t, ok := parseISO(out.RefreshExpires); ok {
		ac.RefreshExpire = t / 1000
	}
	// DPoP 私钥首次生成时落盘（refresh 场景此处通常已存在）。
	if ac.DPoPPrivateJWK == "" {
		ac.DPoPPrivateJWK = key.jwkJSON()
	}

	// 写回内存 + 落盘（不变量 20）
	a.Lock()
	a.RefreshToken = ac.RefreshToken
	a.Extra = mergeRotationInfo(a.Extra, ac)
	a.ExpiresAt = ac.CredExpiresAt
	a.Unlock()
	if a.FilePath != "" {
		if err := a.SaveAtomic(); err != nil {
			return fmt.Errorf("officeace: save refreshed credential: %w", err)
		}
	}
	return nil
}

// ensureCredential 返回带新鲜临时凭据的账号；临时凭据缺失/将过期时先续期一次。
// 续期失败不阻断对话（对话走不过期的网关凭据），仅使积分/签到降级。
func (c *Client) ensureCredential(a *auth.Auth) *Account {
	ac := accountFromAuth(a)
	needRefresh := ac.AccessKeyID == "" || ac.SecretAccessKey == "" ||
		(ac.CredExpiresAt > 0 && time.Now().Unix() > ac.CredExpiresAt-120)
	if needRefresh && strings.TrimSpace(a.RefreshTokenValue()) != "" {
		if err := c.refreshToken(a); err == nil {
			return accountFromAuth(a)
		}
	}
	return ac
}

// dpopKey 从账号里还原 DPoP 私钥；没有则新建（首次续期时生成，随落盘持久化）。
func (c *Client) dpopKey(ac *Account) (*DPoPKey, error) {
	if ac.DPoPPrivateJWK != "" {
		var jwk map[string]string
		if err := json.Unmarshal([]byte(ac.DPoPPrivateJWK), &jwk); err == nil {
			if k, err := dpopKeyFromJWK(jwk); err == nil {
				return k, nil
			}
		}
	}
	return NewDPoPKey()
}

// ---------------------------------------------------------------------------
// 死名单 / 单飞（Extra 承载，随 SaveAtomic 持久化）
// ---------------------------------------------------------------------------

const (
	keyDeadTokens = "dead_tokens" // 被上游拒过的一次性 refresh_token（CSV）
)

// 单账号续期单飞：防止并发把同一一次性 refresh_token 用两次。
var (
	refreshMu    sync.Mutex
	refreshLocks = map[string]*sync.Mutex{}
)

func refreshLockFor(uid string) *sync.Mutex {
	refreshMu.Lock()
	defer refreshMu.Unlock()
	m := refreshLocks[uid]
	if m == nil {
		m = &sync.Mutex{}
		refreshLocks[uid] = m
	}
	return m
}

func acDeadTokens(a *auth.Auth) []string {
	a.RLock()
	defer a.RUnlock()
	return splitCSV(a.Extra[keyDeadTokens])
}

func addDead(a *auth.Auth, tok string) {
	// 写路径持 Lock：a.Extra 会被 accountFromAuth 无锁读（map 并发读写 = 致命错误）。
	a.Lock()
	defer a.Unlock()
	if a.Extra == nil {
		a.Extra = map[string]string{}
	}
	a.Extra[keyDeadTokens] = appendCSV(a.Extra[keyDeadTokens], tok)
}

func addDeadToken(a *auth.Auth, tok string) { addDead(a, tok) }

// mergeRotationInfo 用续期后的账号字段覆盖 Extra（保留已有 dead_tokens 等）。
func mergeRotationInfo(prev map[string]string, ac *Account) map[string]string {
	extra := ac.toAuth().Extra
	if extra == nil {
		extra = map[string]string{}
	}
	if prev != nil {
		if dt := prev[keyDeadTokens]; dt != "" {
			extra[keyDeadTokens] = dt
		}
	}
	return extra
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func containsCSV(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// maxDeadTokens 死名单上限：refresh_token 一次性轮换会不断累积，长期运行需裁剪，
// 避免 auth 文件单调变大。只保留最近 N 个（更早的已被服务器遗忘，留着无用）。
const maxDeadTokens = 20

func appendCSV(s, v string) string {
	list := splitCSV(s)
	if v == "" || containsCSV(list, v) {
		return s
	}
	list = append(list, v)
	if len(list) > maxDeadTokens {
		list = list[len(list)-maxDeadTokens:]
	}
	return strings.Join(list, ",")
}
