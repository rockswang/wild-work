package officeace

import (
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"wild-work/internal/auth"
)

// Account 是 OfficeAce 渠道的一个账号（华为云 IAM 账号）在内存里的表示。
//
// 华为云有「两层凭据」：
//   - 网关凭据（不过期）：Authorization: Basic base64(AK:SK)，用来真正收发对话请求。
//     它落在本机 OfficeAce 桌面端的 models.json 里（custom_headers.Authorization）。
//   - 临时凭据（约 2h）：临时 AK/SK + security_token + project_id，用来做
//     V4/V11 签名访问控制面（积分查询、签到、client-permission-validate）。
//     它由 refresh_token 经 DPoP 换得。
//
// 在 wild-work 的存储里，网关 Authorization 放进 auth.Auth.AccessToken，
// base_url 放进 auth.Auth.ApiHost，其余扩展字段放进 auth.Auth.Extra。
type Account struct {
	ID              string // "<accountId>:<userId>"
	AccountID       string
	UserID          string
	Label           string
	BaseURL         string // 网关基址（形如 https://modelgw-0004.<region>.huaweicloud-agentarts.com/v2）
	Authorization   string // 网关 Basic 凭据头值
	ModelNames      []string
	AccessKeyID     string // 临时 AK
	SecretAccessKey string // 临时 SK
	SecurityToken   string
	ProjectID       string
	CredExpiresAt   int64 // 临时凭据到期（Unix 秒；0=未知）
	RefreshToken    string
	RefreshExpire   int64  // refresh_token 到期（Unix 秒）
	DPoPPrivateJWK  string // DPoP 私钥 JWK（JSON 串）
	Region          string
	Source          string // 来源：disk / oauth-login
}

// Extra 键名常量（落盘到 auth.Auth.Extra）。
const (
	keyBaseURL       = "base_url"
	keyAccountID     = "account_id"
	keyUserID        = "user_id"
	keyLabel         = "label"
	keyAccessKey     = "access_key"
	keySecretKey     = "secret_key"
	keySecurityToken = "security_token"
	keyProjectID     = "project_id"
	keyCredExpires   = "cred_expires"
	keyRefreshExpire = "refresh_expire"
	keyDPoPPriv      = "dpop_priv"
	keyRegion        = "region"
	keyModels        = "models" // '\n' 分隔的模型名列表
	keySource        = "source"
)

// accountFromAuth 把磁盘上的 auth.Auth 还原成 Account。
// 读路径持 RLock：a.Extra 是 map，refresh 时会就地/整体改写（addDead / mergeRotationInfo），
// 无锁读会与写并发触发 Go 的 "concurrent map read and map write" **致命错误**
// （不可 recover，直接打死进程）——实证复现。
func accountFromAuth(a *auth.Auth) *Account {
	a.RLock()
	defer a.RUnlock()
	ex := a.Extra
	if ex == nil {
		ex = map[string]string{}
	}
	ac := &Account{
		ID:              a.UID,
		AccountID:       ex[keyAccountID],
		UserID:          ex[keyUserID],
		Label:           ex[keyLabel],
		BaseURL:         firstNonEmpty(ex[keyBaseURL], a.ApiHost),
		Authorization:   a.AccessToken,
		AccessKeyID:     ex[keyAccessKey],
		SecretAccessKey: ex[keySecretKey],
		SecurityToken:   ex[keySecurityToken],
		ProjectID:       ex[keyProjectID],
		CredExpiresAt:   parseInt64(ex[keyCredExpires]),
		RefreshToken:    a.RefreshToken,
		RefreshExpire:   parseInt64(ex[keyRefreshExpire]),
		DPoPPrivateJWK:  ex[keyDPoPPriv],
		Region:          ex[keyRegion],
		Source:          ex[keySource],
	}
	if ms := ex[keyModels]; ms != "" {
		ac.ModelNames = strings.Split(ms, "\n")
	}
	if ac.AccountID == "" && ac.ID != "" {
		if i := strings.IndexByte(ac.ID, ':'); i > 0 {
			ac.AccountID = ac.ID[:i]
			ac.UserID = ac.ID[i+1:]
		}
	}
	return ac
}

// Export 返回落盘所需的字段（供 app 层写 auth 文件；避免 app 依赖内部结构）。
type Export struct {
	Kind         string
	UID          string
	AccessToken  string
	RefreshToken string // 必须带上：导入路径靠它续期临时凭据（缺了则 ~2h 后积分/签到永久失效）
	ApiHost      string
	Extra        map[string]string
	Nickname     string
}

// Export 把账号导出为落盘字段集合。
func (ac *Account) Export() Export {
	a := ac.toAuth()
	return Export{
		Kind:         "officeace",
		UID:          a.UID,
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		ApiHost:      a.ApiHost,
		Extra:        a.Extra,
		Nickname:     a.Nickname,
	}
}

// WriteAuthFile 把账号以标准嵌套 auth 格式落盘到 auths/officeace-<uid>.json。
func WriteAuthFile(authDir string, ac *Account) (string, error) {
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return "", err
	}
	a := ac.toAuth()
	a.FilePath = filepath.Join(authDir, "officeace-"+sanitizeFileID(ac.ID)+".json")
	if err := a.SaveAtomic(); err != nil {
		return "", err
	}
	return a.FilePath, nil
}

// sanitizeFileID 过滤文件名危险字符。
func sanitizeFileID(id string) string {
	var sb strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			sb.WriteRune(r)
		}
	}
	out := sb.String()
	if out == "" {
		return "default"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

// toAuth 把 Account 写回 auth.Auth（供落盘与池注入）。
func (ac *Account) toAuth() *auth.Auth {
	ex := map[string]string{
		keyBaseURL:       ac.BaseURL,
		keyAccountID:     ac.AccountID,
		keyUserID:        ac.UserID,
		keyLabel:         ac.Label,
		keyAccessKey:     ac.AccessKeyID,
		keySecretKey:     ac.SecretAccessKey,
		keySecurityToken: ac.SecurityToken,
		keyProjectID:     ac.ProjectID,
		keyRegion:        ac.Region,
		keySource:        ac.Source,
	}
	if ac.CredExpiresAt > 0 {
		ex[keyCredExpires] = strconv.FormatInt(ac.CredExpiresAt, 10)
	}
	if ac.RefreshExpire > 0 {
		ex[keyRefreshExpire] = strconv.FormatInt(ac.RefreshExpire, 10)
	}
	if ac.DPoPPrivateJWK != "" {
		ex[keyDPoPPriv] = ac.DPoPPrivateJWK
	}
	if len(ac.ModelNames) > 0 {
		ex[keyModels] = strings.Join(ac.ModelNames, "\n")
	}
	return &auth.Auth{
		Kind:         "officeace",
		UID:          ac.ID,
		AccessToken:  ac.Authorization,
		RefreshToken: ac.RefreshToken,
		ApiHost:      ac.BaseURL,
		ExpiresAt:    ac.CredExpiresAt,
		Nickname:     ac.Label,
		Extra:        ex,
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func parseInt64(s string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return v
}

// urlPathUnescape 是 url.PathUnescape 的薄封装（便于 discover 复用）。
func urlPathUnescape(s string) (string, error) { return url.PathUnescape(s) }

// trimSlash 去掉 URL 尾部所有 '/'。
func trimSlash(u string) string { return strings.TrimRight(strings.TrimSpace(u), "/") }

// chatURL 返回对话端点（网关 OpenAI 兼容：{base}/chat/completions，不带 /v1）。
func (ac *Account) chatURL() string { return trimSlash(ac.BaseURL) + "/chat/completions" }

// modelsURL 返回模型目录端点（{base}/models）。
func (ac *Account) modelsURL() string { return trimSlash(ac.BaseURL) + "/models" }

// regionFromBaseURL 从网关 host 里猜 region（华为主机形如
// modelgw-0004.officeace.cn-southwest-2.huaweicloud-agentarts.com ⇒ cn-southwest-2）。
func regionFromBaseURL(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	for _, p := range strings.Split(u.Host, ".") {
		if strings.HasPrefix(p, "cn-") || strings.HasPrefix(p, "ap-") ||
			strings.HasPrefix(p, "eu-") || strings.HasPrefix(p, "af-") ||
			strings.HasPrefix(p, "la-") || strings.HasPrefix(p, "sa-") {
			return p
		}
	}
	return ""
}
