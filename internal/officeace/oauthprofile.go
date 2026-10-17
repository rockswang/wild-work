package officeace

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
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

// loadOAuthProfile 定位并解密某账号的 OAuth 档案；不支持/不存在时返回 nil。
func loadOAuthProfile(uid string, installDirs []string) *oauthProfileState {
	if !dpapiSupported() {
		return nil
	}
	for _, root := range installRoots(installDirs) {
		dir := filepath.Join(root, filepath.FromSlash(secureDirRel))
		keyPath := filepath.Join(dir, ".oauth-profile-encryption-key")
		profPath := filepath.Join(dir, "oauth-"+uid+".json")
		if !fileExistsS(profPath) {
			continue
		}
		secret, err := readProtectedText(keyPath)
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
