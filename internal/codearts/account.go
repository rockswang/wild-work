// account.go CodeArts 账号在内存里的表示，以及它到 auth.Auth 的双向映射。
//
// 凭据形态（协议 §2.7）：CodeArts **不是** OAuth Bearer 渠道，它的凭据是华为云
// 临时 AK/SK + SecurityToken（由 STS 用 OAuth 授权码 / refresh_token 换得，约 2h）：
//
//	access_key_id      参与签名：Authorization 的 Access= 声明，也是账号唯一标识
//	secret_access_key  参与签名：HMAC-SHA256 的 key
//	security_token     参与签名：x-security-token 头
//	expires_at         不参与签名，但决定要不要先续期（落 auth.Auth.ExpiresAt，**单位秒**）
//	refresh_token      不参与签名，用于静默续期（落 auth.Auth.RefreshToken）
//	code_verifier      不参与签名，PKCE 校验器，续期请求体必需
//	dpop_private_key_jwk 不参与签名，ES256 私钥，续期请求头的 DPoP 证明必需
//
// 落盘约定（与 officeace 同款）：
//   - 三件套（AK/SK/SecurityToken）与其余渠道专有字段 → auth.Auth.Extra（键名见 constants.go）；
//   - auth.Auth.AccessToken 承载 **AK**：它既是账号唯一标识，又满足 auth.Parse 对
//     accessToken 非空的硬校验（本渠道没有 Bearer token 可放，绝不用占位串）；
//   - refresh_token → auth.Auth.RefreshToken；到期 → auth.Auth.ExpiresAt（Unix 秒）。
package codearts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"time"

	"wild-work/internal/auth"
)

// Credential 是 STS 换来的临时凭据（协议 §2.7「STS token 响应」）。
type Credential struct {
	AccessKeyID     string
	SecretAccessKey string
	SecurityToken   string
	// ExpiresAt 到期时刻（Unix 秒）；0 表示上游未给（不得当"永不过期"用）。
	ExpiresAt int64
	// RefreshToken 静默续期用；空表示不可续期（旧 ticket 凭据形态）。
	RefreshToken string
	DomainID     string
	UserID       string
	UserName     string
}

// Account 是 CodeArts 账号的内存形态。
type Account struct {
	// ID 账号唯一标识（= auth.Auth.UID）。
	ID string
	// AccessKeyID / SecretAccessKey / SecurityToken 是**签名三件套**（本渠道凭据本体）。
	AccessKeyID     string
	SecretAccessKey string
	SecurityToken   string
	// ExpiresAt 三件套到期时刻（Unix 秒；0 = 未知）。
	ExpiresAt    int64
	RefreshToken string
	// CodeVerifier / DPoPPrivateKeyJWK 续期必需（缺任一 → 不可静默续期 → 会话终态）。
	CodeVerifier      string
	DPoPPrivateKeyJWK string
	// DomainID / UserID / UserName 身份字段（面板展示与 UID 派生用）。
	DomainID string
	UserID   string
	UserName string
	// ProjectID / Region 华为云项目与区域（签名 V4 形态需要；snap-access 的
	// SDK-HMAC-SHA256 不要求，故只做透传保存，不参与签名）。
	ProjectID string
	Region    string
	// MaaSType 该账号默认的 maas_type（"benefit"）；请求级以模型属 benefit 集合为准。
	MaaSType string
	Nickname string
}

// accountFromAuth 从磁盘 auth.Auth 还原 Account。
//
// 读路径持 RLock：Extra 是 map，续期时会整体改写，无锁读会与写并发触发
// Go 的 "concurrent map read and map write" 致命错误（officeace 已实证复现）。
func accountFromAuth(a *auth.Auth) *Account {
	a.RLock()
	defer a.RUnlock()
	return accountFromAuthLocked(a)
}

// accountFromAuthLocked 是 accountFromAuth 的持锁内部版本；调用方必须已持锁。
func accountFromAuthLocked(a *auth.Auth) *Account {
	ex := a.Extra
	if ex == nil {
		ex = map[string]string{}
	}
	ac := &Account{
		ID:                a.UID,
		AccessKeyID:       ex[ExtraAccessKeyID],
		SecretAccessKey:   ex[ExtraSecretAccessKey],
		SecurityToken:     ex[ExtraSecurityToken],
		ExpiresAt:         a.ExpiresAt,
		RefreshToken:      a.RefreshToken,
		CodeVerifier:      ex[ExtraCodeVerifier],
		DPoPPrivateKeyJWK: ex[ExtraDPoPPrivateKeyJWK],
		DomainID:          ex[ExtraDomainID],
		UserID:            ex[ExtraUserID],
		UserName:          ex[ExtraUserName],
		ProjectID:         ex[ExtraProjectID],
		Region:            ex[ExtraRegion],
		MaaSType:          ex[ExtraMaaSType],
		Nickname:          a.Nickname,
	}
	if strings.TrimSpace(ac.AccessKeyID) == "" {
		// 兼容：AccessToken 承载 AK（见文件头）；Extra 缺失时回退顶层字段。
		ac.AccessKeyID = strings.TrimSpace(a.AccessToken)
	}
	if strings.TrimSpace(ac.ID) == "" {
		// 兜底 UID：AK 是账号唯一标识（协议 §2.7「身份反查」）。
		ac.ID = strings.TrimSpace(ac.AccessKeyID)
	}
	return ac
}

// AccountFromCredential 用一次 STS 换取结果构造账号（登录包与续期路径共用）。
func AccountFromCredential(uid, nickname string, c Credential, codeVerifier, dpopJWK string) *Account {
	ac := &Account{
		ID:                strings.TrimSpace(uid),
		AccessKeyID:       strings.TrimSpace(c.AccessKeyID),
		SecretAccessKey:   c.SecretAccessKey,
		SecurityToken:     c.SecurityToken,
		ExpiresAt:         c.ExpiresAt,
		RefreshToken:      c.RefreshToken,
		CodeVerifier:      codeVerifier,
		DPoPPrivateKeyJWK: dpopJWK,
		DomainID:          c.DomainID,
		UserID:            c.UserID,
		UserName:          c.UserName,
		MaaSType:          MaaSTypeBenefit,
		Nickname:          strings.TrimSpace(nickname),
	}
	if ac.ID == "" {
		ac.ID = deriveUID(c.UserID, c.DomainID, c.AccessKeyID)
	}
	return ac
}

// deriveUID 派生账号 UID：user_id → domain_id → AK。
//
// ⚠️ 2026-10-17 更正：先前此处写着「协议 §2.7：AK 是 CodeArts 的身份反查字段，故它永远是
// 可用的最后兜底」——**这个假设是错的**。协议 §2.7 已确认 STS 响应只给 `credentials.*`
// （不含任何身份字段），于是本函数实际**总是**落到 AK；而 AK 是每次交换新签发的**临时**凭据
// ⇒ 每次登录 UID 都不同 ⇒ 同一账号不断新增重复条目（用户实测报告）。
//
// 现实处置见 login_codearts.SaveAuth 的「方案 B」：无稳定身份字段时复用已有账号的 UID。
// 本函数保持既有回退顺序（它是纯函数，不应依赖 IO），但**调用方必须知道退化后果**。
func deriveUID(candidates ...string) string {
	for _, c := range candidates {
		if s := strings.TrimSpace(c); s != "" {
			return s
		}
	}
	return ""
}

// extra 组装落进 auth.Auth.Extra 的渠道专有字段（**键名单一来源**：落盘与续期回写共用）。
//
//   - 签名三件套 access_key_id / secret_access_key / security_token：本渠道凭据本体；
//   - code_verifier / dpop_private_key_jwk：续期必需，丢了账号 ~2h 后必然失效；
//   - domain_id / user_id / user_name：身份字段（面板展示、UID 派生）；
//   - project_id / region / maas_type：华为云定位与 benefit 标记。
func (ac *Account) extra() map[string]string {
	ex := map[string]string{
		ExtraAccessKeyID:     ac.AccessKeyID,
		ExtraSecretAccessKey: ac.SecretAccessKey,
		ExtraSecurityToken:   ac.SecurityToken,
	}
	setIfNonEmpty(ex, ExtraDomainID, ac.DomainID)
	setIfNonEmpty(ex, ExtraUserID, ac.UserID)
	setIfNonEmpty(ex, ExtraUserName, ac.UserName)
	setIfNonEmpty(ex, ExtraProjectID, ac.ProjectID)
	setIfNonEmpty(ex, ExtraRegion, ac.Region)
	setIfNonEmpty(ex, ExtraCodeVerifier, ac.CodeVerifier)
	setIfNonEmpty(ex, ExtraDPoPPrivateKeyJWK, ac.DPoPPrivateKeyJWK)
	maasType := strings.TrimSpace(ac.MaaSType)
	if maasType == "" {
		maasType = MaaSTypeBenefit
	}
	ex[ExtraMaaSType] = maasType
	return ex
}

// ToAuth 把 Account 写回 auth.Auth（落盘与池注入共用，保证 Extra 键名单一来源）。
func (ac *Account) ToAuth() *auth.Auth {
	nick := strings.TrimSpace(ac.Nickname)
	if nick == "" {
		nick = ac.Label()
	}
	return &auth.Auth{
		Kind: string(Kind),
		UID:  ac.ID,
		// AccessToken 承载 AK：本渠道没有 Bearer token，AK 是账号唯一标识，
		// 同时满足 auth.Parse 对 accessToken 非空的要求（见文件头说明）。
		AccessToken:  ac.AccessKeyID,
		RefreshToken: ac.RefreshToken,
		ExpiresAt:    ac.ExpiresAt,
		Domain:       domainConst,
		ApiHost:      APIBase,
		Nickname:     nick,
		Extra:        ac.extra(),
	}
}

// WriteAuthFile 把账号以标准嵌套 auth 格式落盘到 auths/codearts-<uid>.json。
func WriteAuthFile(authDir string, ac *Account) (string, error) {
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return "", err
	}
	a := ac.ToAuth()
	a.FilePath = filepath.Join(authDir, "codearts-"+SanitizeFileID(ac.ID)+".json")
	if err := a.SaveAtomic(); err != nil {
		return "", err
	}
	return a.FilePath, nil
}

// Label 面板展示名兜底（上游不下发昵称时用 user_name / AK 前 8 位）。
func (ac *Account) Label() string {
	if s := strings.TrimSpace(ac.UserName); s != "" {
		return s
	}
	if s := strings.TrimSpace(ac.AccessKeyID); s != "" {
		if len(s) > 8 {
			return "codearts-" + s[:8]
		}
		return "codearts-" + s
	}
	return "codearts"
}

// domainConst 写入 auth.Auth.Domain（渠道域，仅展示/诊断用）。
const domainConst = "codearts.huaweicloud.com"

// SanitizeFileID 把 uid 消毒成安全文件名片段（uid 是外部数据，落文件名前必须消毒）。
//
// 规则：
//   - 保留 **Unicode 字母与数字**（含中文）+ `-` `_` + 非首位的 `.`；
//   - 其余（Windows 禁用字符 `\ / : * ? " < > |`、控制字符、空格、全角斜杠等）一律替换成 `_`
//     （**替换而非删除**，保留长度信息，避免 "../evil" 被删成 "evil" 这种"看起来合法"的名字）；
//   - 首位 `.` 也替换成 `_`，杜绝 `..` 与隐藏文件；
//   - 空结果或 `unknown` 兜底。
//
// ⚠️ 为什么必须放行中文（2026-10-17 实测踩到）：用户填的「账号标识」通常是中文（如"工作号"）；
// 若只放行 ASCII，每个汉字都会变成一个 `_`，于是「工作号」与「生活号」会**撞成同一个文件名**
// （`codearts-___.json`），两个账号互相覆盖。
//
// ⚠️ 安全性说明：路径穿越靠「不允许 `/`、`\`、`..` 与首位 `.`」阻断，**与字符集宽窄无关**；
// 放宽到 Unicode 不引入穿越面 —— 视觉上像斜杠的字符（`／` U+FF0F、`∕` U+2215）都不是 Unicode 字母，
// 依然会被替换；Windows 保留设备名（CON/PRN/…）也因文件名统一带 `codearts-` 前缀而不成立。
func SanitizeFileID(id string) string {
	// 按 **rune** 计数截断：中文 3 字节/字，按字节截会把最后一个字切碎。
	const maxRunes = 48
	var sb strings.Builder
	n := 0
	for _, r := range id {
		if n >= maxRunes {
			break
		}
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == '-', r == '_':
			sb.WriteRune(r)
		case r == '.' && sb.Len() > 0:
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
		n++
	}
	out := sb.String()
	if out == "" || out == "." || out == ".." {
		return "unknown"
	}
	return out
}

// setIfNonEmpty 只在值非空时写 Extra（保持旧形态干净：空值不落盘）。
func setIfNonEmpty(m map[string]string, k, v string) {
	if s := strings.TrimSpace(v); s != "" {
		m[k] = v
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// snippet 截断响应体用于错误文本（**不得**用于含凭据的响应体：STS 响应里就是凭据，
// 那种场合只透出上游自己的 error/error_code 字段，见 sts.go）。
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > maxSnippet {
		return s[:maxSnippet] + "…"
	}
	return s
}

// parseInt64 宽松解析十进制整数。
func parseInt64(s string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return v
}

// firstNonEmpty 返回第一个非空白串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// trimSlash 去掉 URL 尾部所有 '/'。
func trimSlash(u string) string { return strings.TrimRight(strings.TrimSpace(u), "/") }

// jsonStringOf 把可能是数字或字符串的 JSON 值归一成字符串（协议 §2.6(c)：
// campaignId 服务端下发为**数字**，必须转字符串再回传）。
func jsonStringOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return strconv.FormatInt(int64(f), 10)
	}
	return ""
}

// parseISOTime 解析上游的 ISO8601 时间为 Unix 秒。
//
// 华为云在时间形态上不统一（`2026-10-16T10:30:00Z` / 带毫秒 / 无时区的
// `2026-10-16 10:30:00` 都出现过），故按候选布局逐个尝试；全部失败返回 ok=false
// ——调用方**不得**把失败当成「永不过期」（见 defaultCredTTL）。
func parseISOTime(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Unix(), true
		}
	}
	return 0, false
}
