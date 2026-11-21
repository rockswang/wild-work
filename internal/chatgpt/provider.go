package chatgpt

import (
	"errors"
	"fmt"
	"strings"

	"wild-work/internal/auth"
)

// 本文件是 provider.Upstream 的**账号面**实现（票 01 只落账号面：
// RefreshToken / Me / Models；对话、生图、签到由后续票补齐）。
//
// 账号隔离的落地：Client 按账号缓存 session（TLS 客户端 + cookie jar + device id +
// Bearer），请求 context 携带当前账号的 session。cookie 是账号级凭据，共用一个 jar
// 会让 A 号的 session cookie 串到 B 号的请求上。

// accountSeed 是「建/同步 session」所需的账号取值快照，避免在锁内反复读 *auth.Auth。
type accountSeed struct {
	key         string
	accessToken string
	accountID   string
	deviceID    string
	cookies     map[string]string
}

// seedOf 从凭据取快照。accountId 优先取 Extra（面板/导入写入），缺失时从 JWT 解。
func seedOf(a *auth.Auth) accountSeed {
	if a == nil {
		return accountSeed{}
	}
	tok := strings.TrimSpace(a.AccessTokenValue())
	accID := strings.TrimSpace(a.Extra["accountId"])
	if accID == "" {
		_, _, accID, _, _ = JWTInfo(tok)
	}
	return accountSeed{
		key:         accountKey(a),
		accessToken: tok,
		accountID:   accID,
		deviceID:    strings.TrimSpace(a.DeviceID),
		cookies:     cloneCookies(a.Cookies),
	}
}

// accountKey 是 session 的桶键：优先 UID（重登后仍是同一个号），
// 缺失时退回 cookie/ token 的**哈希**（绝不用明文做键）。
func accountKey(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	if strings.TrimSpace(a.UID) != "" {
		return "uid:" + strings.TrimSpace(a.UID)
	}
	if len(a.Cookies) > 0 {
		return "ck:" + hashKey(CookieString(a.Cookies))
	}
	if tok := a.AccessTokenValue(); tok != "" {
		return "tok:" + hashKey(tok)
	}
	return ""
}

// session 取（必要时创建）账号的出站通道。
func (c *Client) session(a *auth.Auth) (*session, error) {
	seed := seedOf(a)
	if seed.key == "" {
		return nil, errors.New("chatgpt: 账号缺少可用标识（uid / cookie / token 全空）")
	}
	c.mu.Lock()
	if s := c.sessions[seed.key]; s != nil {
		c.mu.Unlock()
		s.sync(seed)
		return s, nil
	}
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("chatgpt: 客户端已关闭")
	}
	s, err := c.newSession(seed)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	c.sessions[seed.key] = s
	c.mu.Unlock()
	return s, nil
}

// RefreshToken 实现 provider.Upstream 的续期面：打 /api/auth/session 触发 90 天
// **滑动续期**，成功后立即落盘。
//
// 不变量 20：会话 cookie 每次续期都会重发新值，不落盘 = 下次启动拿着已作废的旧
// session-token 反复 401。落盘失败必须报错（内存已滚动，磁盘仍是旧值）。
//
// 到期时刻用**上游真实值**（JWT exp 优先，其次上游 expires），**不写远期** ——
// 有刷新能力的渠道写远期，只会在 90 天后静默死掉且没人发现。
func (c *Client) RefreshToken(a *auth.Auth) error {
	if a == nil {
		return errors.New("chatgpt: 缺少账号凭据")
	}
	res, err := c.RefreshSession(a)
	if err != nil {
		return err
	}

	a.Lock()
	a.AccessToken = res.AccessToken
	if res.ExpiresAt > 0 {
		a.ExpiresAt = res.ExpiresAt
	}
	if res.AccountID != "" {
		if a.Extra == nil {
			a.Extra = map[string]string{}
		}
		a.Extra["accountId"] = res.AccountID
	}
	if res.Nickname != "" {
		a.Nickname = res.Nickname
	}
	if strings.TrimSpace(a.UID) == "" && res.UserID != "" {
		a.UID = res.UserID
	}
	a.Cookies = mergeCookies(a.Cookies, res.Cookies)
	a.Unlock()

	if err := a.SaveAtomic(); err != nil {
		return fmt.Errorf("chatgpt: 凭据落盘失败（会话 Cookie 已滚动，务必重试）: %w", err)
	}
	return nil
}

// Cookies 读回该账号 session 的 cookie jar（面板/导入需要把上游 Set-Cookie 的新值
// 单独持久化时用；不返回任何 token）。
func (c *Client) Cookies(a *auth.Auth) (map[string]string, error) {
	s, err := c.session(a)
	if err != nil {
		return nil, err
	}
	return s.Cookies(), nil
}

// mergeCookies 把更新合并进基线（同名取更新值）。
func mergeCookies(base, update map[string]string) map[string]string {
	if len(update) == 0 {
		return cloneCookies(base)
	}
	out := cloneCookies(base)
	if out == nil {
		out = map[string]string{}
	}
	for k, v := range update {
		out[k] = v
	}
	return out
}
