package chatgpt

import (
	"encoding/json"
	"fmt"
	"strings"

	"wild-work/internal/auth"
)

// 账号面：当前账号信息 / 模型目录 / web 会话滑动续期。
//
// 续期只有一条路：`GET /api/auth/session`。它会 Set-Cookie 重发
// `__Secure-next-auth.session-token.0/.1`（Max-Age=7776000 = 90 天，每次请求往后推），
// **业务请求（/backend-api/*）一个 Set-Cookie 都不下发** ⇒「正常对话就能保活」是错的，
// 必须显式调它，并且调用后立即落盘（不变量 20：不落盘 = 下次启动用旧凭据）。

// UserInfo 是 /backend-api/me 的结果。
type UserInfo struct {
	Email       string `json:"email"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	PhoneNumber string `json:"phone_number,omitempty"`
}

// Me 取当前用户信息（零配额端点）。
//
// 面板「粘贴凭据」与「从 ww3 导入」都用它做**零费用校验**：失败即不落盘。
func (c *Client) Me(a *auth.Auth) (*UserInfo, error) {
	st, m, raw, err := c.DoJSON(a, "GET", PathMe, nil, nil)
	if err != nil {
		return nil, err
	}
	if st != 200 {
		return nil, StatusError("GET", PathMe, st, raw)
	}
	u := &UserInfo{}
	if b, err := json.Marshal(m); err == nil {
		_ = json.Unmarshal(b, u)
	}
	return u, nil
}

// SessionResult 是 /api/auth/session 的结果。
type SessionResult struct {
	Status      int
	AccessToken string
	// Expires 是上游原样的 `expires`（ISO8601，通常为会话 90 天滚动期）。
	Expires string
	// ExpiresAt 是**真实**到期时刻（Unix 秒）：优先 access token 的 JWT exp，
	// 否则解上游 expires。两者都拿不到时保持 0（未知，不臆造，更不写远期）。
	ExpiresAt int64
	PlanType  string
	AccountID string
	UserID    string
	Email     string
	Nickname  string
	// Cookies 是响应里的 Set-Cookie 合并结果（session-token.0/.1 等）。
	// 必须全量持久化，否则下次启动用旧 session-token 会 401。
	Cookies map[string]string
	// Raw 便于排查（不含凭据明文时使用）。
	Raw map[string]any
}

// RefreshSession 打 /api/auth/session，触发滑动续期并拿最新 access token。
//
// 该端点不带 Authorization（只靠 cookie）——带 Bearer 的 session 请求会被上游判异常。
func (c *Client) RefreshSession(a *auth.Auth) (*SessionResult, error) {
	s, err := c.session(a)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(a, c.HTTP, "GET", PathSession, nil, nil, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := readAllMaybeGzip(resp)

	r := &SessionResult{Status: resp.StatusCode, Cookies: map[string]string{}}
	// Set-Cookie：同名多条取最后一条（.0/.1 是不同名，都要）。
	for _, ck := range resp.Header.Values("Set-Cookie") {
		if name, val := cookieNameValue(ck); name != "" {
			r.Cookies[name] = val
		}
	}
	if resp.StatusCode != 200 {
		return r, StatusError("GET", PathSession, resp.StatusCode, body)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return r, fmt.Errorf("chatgpt: /api/auth/session 返回非 JSON: %s", truncate(string(body), 300))
	}
	r.Raw = m
	r.AccessToken = str(m["accessToken"])
	r.Expires = str(m["expires"])
	if acc, ok := m["account"].(map[string]any); ok {
		r.PlanType = str(acc["planType"])
		r.AccountID = str(acc["id"])
	}
	if u, ok := m["user"].(map[string]any); ok {
		r.UserID = str(u["id"])
		r.Email = str(u["email"])
		r.Nickname = str(u["name"])
	}
	if r.AccessToken == "" {
		return r, fmt.Errorf("chatgpt: /api/auth/session 未返回 accessToken（会话已失效，需重新登录）")
	}
	// 真实到期时刻：JWT exp 优先，其次上游 expires。不写远期（有刷新能力的渠道写远期
	// 会在 90 天后静默死掉）。
	if exp := JWTExp(r.AccessToken); exp > 0 {
		r.ExpiresAt = exp
	} else {
		r.ExpiresAt = parseISOExpiry(r.Expires)
	}
	// 顶部会话 cookie 直接反映在 jar 里：把 Set-Cookie 灌回去，后续 business 请求即用新值。
	if len(r.Cookies) > 0 {
		s.seedCookies(r.Cookies)
		// 让内存视图（auth 与 session 的「已灌入」快照）与 jar 一致，
		// 否则下一次 session 同步会拿旧 cookie 把刚滚动的新值冲掉。
		a.Lock()
		a.Cookies = mergeCookies(a.Cookies, r.Cookies)
		s.mu.Lock()
		s.seeded = cloneCookies(a.Cookies)
		s.mu.Unlock()
		a.Unlock()
	}
	s.ApplySession(r)
	return r, nil
}

// ApplySession 把续期结果写回 session（后续请求用新 token / 新 account 头）。
func (s *session) ApplySession(r *SessionResult) {
	if r == nil {
		return
	}
	s.mu.Lock()
	if r.AccessToken != "" {
		s.accessToken = r.AccessToken
	}
	if r.AccountID != "" {
		s.accountID = r.AccountID
	}
	s.mu.Unlock()
}

// ModelInfo 是上游模型清单的一项。
type ModelInfo struct {
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	MaxTokens   int64    `json:"max_tokens"`
	Tags        []string `json:"tags"`
	// Categories 归属（如 gpt_5_auto）。
	Categories []string `json:"-"`
}

// Models 拉登录态模型目录。
//
// 实测（free 号）清单里只有部分 slug 是真模型，其余会被上游静默路由到默认模型 ——
// 真实路由结果只有对话响应的 server_ste_metadata.model_slug 能证实，清单本身不可信。
// 因此这里只做「如实转述清单」，**不编造**上下文窗口与能力声明（R49）。
func (c *Client) Models(a *auth.Auth) ([]ModelInfo, error) {
	path := PathModels + modelsQuery
	st, m, raw, err := c.DoJSON(a, "GET", path, nil, nil)
	if err != nil {
		return nil, err
	}
	if st != 200 {
		return nil, StatusError("GET", path, st, raw)
	}
	var out []ModelInfo
	// 形状一：{"models":[{slug,…}]}
	if arr, ok := m["models"].([]any); ok {
		for _, it := range arr {
			if im, ok := it.(map[string]any); ok {
				out = append(out, modelOf(im))
			}
		}
	}
	// 形状二：{"categories":[{id,models:[…]}]}（新形状，带分组）
	if cats, ok := m["categories"].([]any); ok {
		seen := map[string]bool{}
		for _, it := range cats {
			cm, ok := it.(map[string]any)
			if !ok {
				continue
			}
			catID := str(cm["id"])
			if catID == "" {
				catID = str(cm["slug"])
			}
			arr, _ := cm["models"].([]any)
			for _, mi := range arr {
				mm, ok := mi.(map[string]any)
				if !ok {
					continue
				}
				one := modelOf(mm)
				if one.Slug == "" || seen[one.Slug] {
					continue
				}
				seen[one.Slug] = true
				one.Categories = append(one.Categories, catID)
				out = append(out, one)
			}
		}
	}
	return out, nil
}

// modelOf 把上游一条模型记录转成 ModelInfo（字段名宽容）。
func modelOf(m map[string]any) ModelInfo {
	mi := ModelInfo{
		Slug:        firstStr(m, "slug", "id", "model_slug"),
		Title:       firstStr(m, "title", "name"),
		Description: str(m["description"]),
	}
	if f, ok := m["max_tokens"].(float64); ok {
		mi.MaxTokens = int64(f)
	}
	if arr, ok := m["tags"].([]any); ok {
		for _, t := range arr {
			if s, ok := t.(string); ok {
				mi.Tags = append(mi.Tags, s)
			}
		}
	}
	return mi
}

// cookieNameValue 从一条 Set-Cookie 头里抠 name=value。
func cookieNameValue(setCookie string) (string, string) {
	s := strings.TrimSpace(setCookie)
	if i := strings.IndexByte(s, ';'); i >= 0 {
		s = s[:i]
	}
	k, v, ok := strings.Cut(s, "=")
	if !ok {
		return "", ""
	}
	return strings.TrimSpace(k), strings.TrimSpace(v)
}

// str 取 map 里的字符串值（缺失/类型不符返回空串）。
func str(v any) string {
	s, _ := v.(string)
	return s
}

// firstStr 返回第一个存在的非空字符串值。
func firstStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}
