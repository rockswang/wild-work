package chatgpt

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"wild-work/internal/auth"
)

// 凭据：与 ww3 客户端 `auths/<name>.json` **逐字段相同**的嵌套形
//
//	{"auth":{accessToken,expiresAt,deviceId,cookies,…},"account":{uid,nickname}}
//
// 落盘为 v2 惯例的 `auths/chatgpt-<uid>.json`（0600）。零 token 落盘：日志/错误串
// 里只出现掩码，绝不出现 access token 或 cookie 明文。

// ParseAuth 解析 chatgpt 凭据文件（嵌套形 / 扁平形）并校验网页会话必需字段。
//
// 「零转换」：ww3 客户端写在磁盘上的那份文件不需要任何格式转换 —— accessToken /
// expiresAt / deviceId / cookies 段逐字段对上，uid 取 account.uid。
// 「明确报错」：缺 accessToken（auth.Parse 抛）或整组 cookies 为空时返回可读错误，
// 而不是静默产出一个「看起来加上了、每次请求都 403」的空账号。
func ParseAuth(raw []byte) (*auth.Auth, error) {
	a, err := auth.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("chatgpt 凭据解析失败: %w", err)
	}
	a.Kind = "chatgpt"
	if strings.TrimSpace(a.UID) == "" {
		// ww3 的 auth 段也写了一份 userId；account.uid 缺失时回退，避免生成无主账号。
		a.UID = nestedUserID(raw)
	}
	if allEmpty(a.Cookies) {
		return nil, errors.New("chatgpt 凭据缺少 cookies：网页会话需要整组 Cookie（只有 access token 会被上游判为未登录）")
	}
	return a, nil
}

// nestedUserID 从嵌套形的 auth 段读 userId（ww3 凭据文件的两个 uid 之一）。
func nestedUserID(raw []byte) string {
	var probe struct {
		Auth struct {
			UserID string `json:"userId"`
		} `json:"auth"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return ""
	}
	return strings.TrimSpace(probe.Auth.UserID)
}

// allEmpty 报告 map 是否没有非空值。
func allEmpty(m map[string]string) bool {
	for _, v := range m {
		if strings.TrimSpace(v) != "" {
			return false
		}
	}
	return true
}

// ParseCookieString 把浏览器复制的整条 Cookie 串解析成 map。
// 用户从 DevTools 复制的一般是 `a=1; b=2; __Secure-next-auth.session-token.0=xxx; …`。
func ParseCookieString(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// ParseCookieHeader 解析用户从 DevTools 复制的 Cookie 输入。
//
// 容忍三种常见粘贴形态：裸值、带 `Cookie:` 前缀（复制整行请求头）、带引号。
// **不做宽松兜底**：解析不出条目就是错，让调用方如实回报，而不是产出一个空 Cookie 账号
// （空 Cookie 的账号「看起来加上了」，每次请求都 403，是本渠道最难排障的失败形态）。
func ParseCookieHeader(raw string) (map[string]string, error) {
	s := strings.TrimSpace(raw)
	// 复制整行请求头时会带 `Cookie:` 前缀（大小写不敏感）。
	if i := strings.Index(s, ":"); i > 0 && strings.EqualFold(strings.TrimSpace(s[:i]), "cookie") {
		s = s[i+1:]
	}
	ck := ParseCookieString(strings.Trim(s, "\"'"))
	if allEmpty(ck) {
		return nil, errors.New("Cookie 为空或格式不对：需要从已登录 chatgpt.com 的浏览器里复制**整条** Cookie 头（形如 `a=1; b=2; __Secure-next-auth.session-token.0=…`）")
	}
	return ck, nil
}

// ParseAuthorizationHeader 解析 `Authorization` 输入，返回裸 access token。
//
// 上游要的是 `Authorization: Bearer <token>`，而用户从 DevTools 复制的可能是
// 整行（`authorization: Bearer eyJ…`）、带引号的值，或只有裸 token。
func ParseAuthorizationHeader(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if i := strings.Index(s, ":"); i > 0 && strings.EqualFold(strings.TrimSpace(s[:i]), "authorization") {
		s = s[i+1:]
	}
	s = strings.TrimSpace(strings.Trim(s, "\"'"))
	// `Bearer` 也允许无空格/尾随空格（用户常连空格一起复制或只粘方案名）。
	if len(s) >= 6 && strings.EqualFold(s[:6], "bearer") {
		s = strings.TrimSpace(s[6:])
	}
	if s == "" {
		return "", errors.New("Authorization 为空：需要从已登录 chatgpt.com 的浏览器里复制请求头 `Authorization: Bearer eyJ…`（只给 Cookie 会被上游判未登录）")
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return "", errors.New("Authorization 里含空白字符：请只粘贴 `Bearer` 后面的那串 token")
	}
	return s, nil
}

// NewDeviceID 生成一份网页身份设备号（uuid4 形态）。
//
// 上游只校验形状，不复用旧设备号即可（与 doubao「每次登录生成新指纹」同思路：
// 复用别人的设备号只会把两边的风控记录串在一起）。
func NewDeviceID() string { return newUUID() }

// CookieString 把 map 还原成 Cookie 头（按 key 排序，保证可复现）。
//
// **不用于日常请求**：日常请求靠 tls-client 的 cookie jar 自动管理（手写 Cookie 头
// 会覆盖 jar，导致上游 Set-Cookie 的新值带不回来）。这里只用于持久化与展示。
func CookieString(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, "; ")
}

// Mask 掩码显示（日志/错误用）。长度信息也不外泄。
func Mask(tok string) string {
	if tok == "" {
		return "(empty)"
	}
	if len(tok) <= 8 {
		return "***"
	}
	return tok[:4] + "…(len=" + strconv.Itoa(len(tok)) + ")"
}

// JWTExp 从 JWT 取 exp（Unix 秒）。解析失败返回 0（未知，不臆造）。
func JWTExp(tok string) int64 {
	_, _, _, _, exp := JWTInfo(tok)
	return exp
}

// JWTInfo 解 JWT 的常用字段（只用于展示与判断，不验签）。
//
// 字段有两套位置：老的平铺（acct_id/plan_type/email）与新的命名空间嵌套
// （https://api.openai.com/auth 与 /profile）——实测 Codex 签发的 token 只给后者。
func JWTInfo(tok string) (email, planType, accountID string, iat, exp int64) {
	claims := jwtClaims(tok)
	if claims == nil {
		return
	}
	authNS, _ := claims["https://api.openai.com/auth"].(map[string]any)
	profNS, _ := claims["https://api.openai.com/profile"].(map[string]any)
	pick := func(flatKey, nsKey string) string {
		if s, ok := claims[flatKey].(string); ok && s != "" {
			return s
		}
		if s, ok := authNS[nsKey].(string); ok {
			return s
		}
		return ""
	}
	email, _ = claims["email"].(string)
	if email == "" {
		email, _ = profNS["email"].(string)
	}
	planType = pick("plan_type", "chatgpt_plan_type")
	accountID = pick("acct_id", "chatgpt_account_id")
	if f, ok := claims["iat"].(float64); ok {
		iat = int64(f)
	}
	if f, ok := claims["exp"].(float64); ok {
		exp = int64(f)
	}
	return
}

// JWTUserID 解 user_id（user-xxx），用于展示与账号命名。
func JWTUserID(tok string) string {
	claims := jwtClaims(tok)
	if claims == nil {
		return ""
	}
	if authNS, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if s, ok := authNS["chatgpt_user_id"].(string); ok && s != "" {
			return s
		}
		if s, ok := authNS["user_id"].(string); ok && s != "" {
			return s
		}
	}
	if s, ok := claims["sub"].(string); ok {
		return s
	}
	return ""
}

// JWTName 解 display name（profile.name）。
func JWTName(tok string) string {
	claims := jwtClaims(tok)
	if claims == nil {
		return ""
	}
	if profNS, ok := claims["https://api.openai.com/profile"].(map[string]any); ok {
		if s, ok := profNS["name"].(string); ok {
			return s
		}
	}
	return ""
}

// jwtClaims 解 JWT payload（无填充 base64url）；非 JWT 返回 nil。
func jwtClaims(tok string) map[string]any {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := b64urlDecode(parts[1])
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		return nil
	}
	return m
}

func b64urlDecode(s string) ([]byte, error) {
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	return base64.URLEncoding.DecodeString(s)
}

// parseISOExpiry 解上游 `expires` 字段（ISO8601）为 Unix 秒；解不出返回 0（不臆造）。
func parseISOExpiry(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// newUUID 生成 v4 UUID（不引第三方库）。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", time.Now().UnixNano()&0xFFFFFFFFFFFF)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// cloneCookies 复制 cookie map（快照，避免调用方后续改写影响 session 判断）。
func cloneCookies(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// cookiesEqual 比较两个 cookie map（忽略空值差异）。
func cookiesEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// truncate 截断长串用于错误体/日志（避免把整页 HTML 打进错误消息）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// hashKey 返回短哈希（不含明文），用于派生账号 key / uid 兜底。
func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
