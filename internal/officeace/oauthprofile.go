package officeace

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// OfficeAce 把每个账号的 OAuth 档案（含临时 AK/SK、refresh_token、DPoP 私钥）加密落盘：
//
//	<安装根>/packages/api/.config/secure-config-nodejs/oauth-<uid>.json
//
// 其密钥来自同目录的安装密钥文件 `.oauth-profile-encryption-key`（该文件本身又被 DPAPI 加密）。
// 档案是 {version, alg:"aes-256-gcm", iv, tag, data}（三者 base64url），
// 解密密钥 = SHA-256(安装密钥明文)。

const secureDirRel = "packages/api/.config/secure-config-nodejs"

// oauthProfileState 是解密后的 providerState 关键字段。
type oauthProfileState struct {
	AuthType   string `json:"authType"`
	AccountID  string `json:"accountId"`
	UserName   string `json:"userName"`
	UserID     string `json:"user_id"`
	Credential struct {
		Access         string `json:"access"`
		Secret         string `json:"secret"`
		STSToken       string `json:"sts_token"`
		ProjectID      string `json:"project_id"`
		ExpiresAt      any    `json:"expires_at"`
		RefreshToken   string `json:"refresh_token"`
		RefreshExpires any    `json:"refresh_expires_at"`
	} `json:"credential"`
	// Dpop 是桌面端为**这个账号**持有的 DPoP 密钥对（P-256，形如
	// {"privateJwk":{"kty":"EC","crv":"P-256","x":…,"y":…,"d":…,"ext":…,"key_ops":[…]}, "publicJwk":{…}}）。
	//
	// **必须解析并复用**（2026-10-17 离线取证定位）：DPoP 的语义是 refresh_token 与签发时
	// 使用的公钥绑定，换一对密钥去签续期请求会被 STS 拒绝。旧实现不解析本字段 ⇒ 导入账号的
	// `Extra["dpop_priv"]` 恒为空 ⇒ refresh.go 的 dpopKey() 回落到 NewDPoPKey() **新建**一把
	// 密钥 ⇒ 档案里那份临时凭据（~2h）一到期续期就失败，表现为「导入当天可用、两小时后积分/
	// 签到又全坏」（与用户报告的 APIG.0303 / no_token 同族，只是延后爆发）。
	// 自助登录路径（login.go）本来就把自己生成的私钥落进 Extra，与此同构。
	Dpop struct {
		PrivateJwk map[string]any `json:"privateJwk"`
		PublicJwk  map[string]any `json:"publicJwk"`
	} `json:"dpop"`
}

// enrichWithProfile 尝试从本机加密档案补齐账号的临时凭据（仅 Windows 可解密）。
func enrichWithProfile(acc *Account, installDirs []string) {
	prof := loadOAuthProfile(acc.ID, installDirs)
	if prof == nil {
		return
	}
	c := prof.Credential
	if acc.AccessKeyID == "" {
		acc.AccessKeyID = strings.TrimSpace(c.Access)
		acc.SecretAccessKey = strings.TrimSpace(c.Secret)
		acc.SecurityToken = strings.TrimSpace(c.STSToken)
		acc.ProjectID = strings.TrimSpace(c.ProjectID)
		acc.CredExpiresAt = anyToUnixSec(c.ExpiresAt)
	}
	if acc.RefreshToken == "" {
		acc.RefreshToken = strings.TrimSpace(c.RefreshToken)
		acc.RefreshExpire = anyToUnixSec(c.RefreshExpires)
	}
	// DPoP 私钥：续期必须复用桌面端那把（理由见 oauthProfileState.Dpop 注释）。
	if acc.DPoPPrivateJWK == "" {
		acc.DPoPPrivateJWK = dpopJWKJSON(prof.Dpop.PrivateJwk)
	}
	if acc.Label == "" {
		acc.Label = strings.TrimSpace(prof.UserName)
	}
	if acc.UserID == "" {
		acc.UserID = strings.TrimSpace(prof.UserID)
	}
	if acc.AccountID == "" {
		acc.AccountID = strings.TrimSpace(prof.AccountID)
	}
}

// dpopJWKJSON 把桌面端档案里的 `dpop.privateJwk` 收敛成 wild-work 落盘用的最小 JWK JSON。
//
// **只保留 kty/crv/x/y/d 五个字符串键**，刻意丢掉 `ext` / `key_ops` 等非字符串字段：落盘值会被
// refresh.go 的 dpopKey() 按 `map[string]string` 反序列化，混入 bool/数组会让整次解析失败并
// **静默回落到新建密钥** —— 那正是本函数要避免的失败形态（没有日志、只有两小时后的续期失败）。
// 私钥三分量（x/y/d）缺任何一项都返回空串（宁可不填，也不要填一份解不开的 JWK）。
func dpopJWKJSON(in map[string]any) string {
	if len(in) == 0 {
		return ""
	}
	out := map[string]string{}
	for _, k := range []string{"kty", "crv", "x", "y", "d"} {
		if s, ok := in[k].(string); ok && strings.TrimSpace(s) != "" {
			out[k] = strings.TrimSpace(s)
		}
	}
	if out["x"] == "" || out["y"] == "" || out["d"] == "" {
		return ""
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

// loadOAuthProfile 定位并解密某账号的 OAuth 档案；不存在/解不开时返回 nil。
//
// 档案名里的 uid 段是**桌面端的 URL 编码形式**（即 routing_state/users 下的目录名原样，
// 如 01a1...c028%3A01a1...5761），而 acc.ID 是 decodeUID 后的含 ':' 真 uid ⇒ 必须两种写法都试
// （编码优先，见 oauthProfileNameCandidates）。只认解码形式会永远 miss：Windows 文件名不允许 ':'。
//
// 平台能力由 readProtectedTextFn 决定：非 Windows 上密钥文件是 DPAPI blob，
// 读取/解密必然报错 ⇒ 继续试下一候选根，最终返回 nil（不报错，不影响其它账号）。
func loadOAuthProfile(uid string, installDirs []string) *oauthProfileState {
	names := oauthProfileNameCandidates(uid)
	for _, root := range installRoots(installDirs) {
		dir := filepath.Join(root, filepath.FromSlash(secureDirRel))
		var profPath string
		for _, name := range names {
			p := filepath.Join(dir, name)
			if fileExistsS(p) {
				profPath = p
				break
			}
		}
		if profPath == "" {
			continue
		}
		keyPath := filepath.Join(dir, ".oauth-profile-encryption-key")
		secret, err := readProtectedTextFn(keyPath)
		if err != nil || strings.TrimSpace(secret) == "" {
			continue
		}
		raw, err := os.ReadFile(profPath)
		if err != nil {
			continue
		}
		prof, err := decryptOAuthProfile(secret, raw)
		if err != nil {
			continue
		}
		return prof
	}
	return nil
}

// oauthProfileNameCandidates 返回某 uid 对应的**档案文件名**候选（按尝试顺序，已去重）。
//
// 桌面端落盘用 URL 编码（'%3A' 大写），缺失时退回真 uid 内的 ':' 形式，
// 两种历史写法都能命中（解码名对一个不存在的路径无害，仅多一次 os.Stat）。
func oauthProfileNameCandidates(uid string) []string {
	esc := urlEscapeUID(uid)
	dec := urlPathUnescapeOrRaw(uid)
	var out []string
	for _, cand := range []string{esc, dec} {
		if cand == "" {
			continue
		}
		name := "oauth-" + cand + ".json"
		dup := false
		for _, x := range out {
			if x == name {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, name)
		}
	}
	return out
}

// urlEscapeUID 把 uid 编码成桌面端落盘用的形式（'%3A' 而非小写 '%3a'）。
//
// 注意第 2 步必须在**已转义串** `esc` 上替换：若退回原串 `s` 上做 ReplaceAll，会丢掉
// PathEscape 对 `/`、`%`、空格等字符的转义（2026-10-17 独立复核指出）。当前真实 uid 是
// `<hex>:<hex>`，两种写法等价，但形状一旦变化（例如含 `%`）就会算出与桌面端不同的文件名。
func urlEscapeUID(s string) string {
	if s == "" {
		return ""
	}
	esc := url.PathEscape(s)
	if !strings.Contains(esc, ":") {
		// 不含 ':' 的 uid 两种写法一致（PathEscape 对字母数字与 '-'/'_'/'.' 恒等）。
		return esc
	}
	// PathEscape 不转义 ':'，这里显式补成桌面端的大写编码形式。
	return strings.ReplaceAll(esc, ":", "%3A")
}

// urlPathUnescapeOrRaw 解码失败时原样返回（保持 decodeUID 的既有语义）。
func urlPathUnescapeOrRaw(s string) string {
	if dec, err := urlPathUnescape(s); err == nil {
		return dec
	}
	return s
}

// readProtectedTextFn 是 readProtectedText 的包内间接层（测试可注入桩，避开 DPAPI 平台依赖）。
var readProtectedTextFn = readProtectedText

// decryptOAuthProfile 用安装密钥解密档案（AES-256-GCM，密钥 = SHA-256(secret)，iv/tag/data base64url）。
func decryptOAuthProfile(installSecret string, raw []byte) (*oauthProfileState, error) {
	var env struct {
		Version int    `json:"version"`
		Alg     string `json:"alg"`
		IV      string `json:"iv"`
		Tag     string `json:"tag"`
		Data    string `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if env.Version != 1 || env.Alg != "aes-256-gcm" {
		return nil, errUnsupportedProfile
	}
	key := sha256.Sum256([]byte(installSecret))
	iv, err := base64.RawURLEncoding.DecodeString(env.IV)
	if err != nil {
		return nil, err
	}
	tag, err := base64.RawURLEncoding.DecodeString(env.Tag)
	if err != nil {
		return nil, err
	}
	data, err := base64.RawURLEncoding.DecodeString(env.Data)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, iv, append(data, tag...), nil)
	if err != nil {
		return nil, err
	}
	var prof oauthProfileState
	if err := json.Unmarshal(plain, &prof); err != nil {
		return nil, err
	}
	return &prof, nil
}

// anyToUnixSec 把 JSON 的 expires_at（可能是字符串 ISO / 数字秒/毫秒）归一为 Unix 秒。
func anyToUnixSec(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case float64:
		n := int64(t)
		if n > 1e12 { // 毫秒
			return n / 1000
		}
		return n
	case string:
		if ms, ok := parseISO(t); ok {
			return ms / 1000
		}
		if n := parseInt64(t); n > 0 {
			if n > 1e12 {
				return n / 1000
			}
			return n
		}
	}
	return 0
}

func fileExistsS(p string) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

var errUnsupportedProfile = os.ErrInvalid
