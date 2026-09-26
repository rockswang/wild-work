// Package loginglm 智谱清言登录编排（引导式粘贴，方案 B）。
//
// 与其它渠道（OAuth 设备流 + 本地回调端口）不同，清言没有可编程的登录接口：
// 凭据是浏览器 Cookie 里的 chatglm_refresh_token，只能由用户在浏览器里登录后取出。
//
// 故本流程是「引导式」的：
//
//	① 打开系统浏览器到 chatglm.cn
//	② 面板提示用户 F12 → Application → Cookies → 复制 chatglm_refresh_token
//	③ 用户把值粘贴回面板，触发验证（调 user/refresh 换 access_token）
//	④ 验证通过则落盘 auths/glm-<uid>.json
//
// 之所以不做内嵌浏览器自动抓取：wild-work 的 R7 决议明确删除了 WebView2 依赖，
// 为了一个渠道把它加回来是架构倒退，且 Windows 专属（macOS/Linux 要另写）。
package loginglm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wild-work/internal/glm"
)

// LoginURL 用户需要在浏览器里打开的地址。
const LoginURL = "https://chatglm.cn"

// Result 登录成功后的账号信息。
type Result struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	UID          string
	Nickname     string
}

// Validate 用 refresh_token 换取 access_token 并取回账号信息。
// 这一步同时完成「验证凭据有效」与「拿到 uid」两件事。
func Validate(refreshToken string) (Result, error) {
	acct, err := glm.ValidateRefreshToken(context.Background(), refreshToken)
	if err != nil {
		return Result{}, err
	}
	if acct.UID == "" {
		return Result{}, fmt.Errorf("未能取得账号 ID，请确认 refresh_token 完整")
	}
	return Result{
		AccessToken:  acct.AccessToken,
		RefreshToken: acct.RefreshToken,
		ExpiresAt:    acct.ExpiresAt,
		UID:          acct.UID,
		Nickname:     acct.Nickname,
	}, nil
}

// SaveAuth 以嵌套形原子写 auth 文件（与 internal/auth.Parse 读取格式一致），
// 文件名前缀 glm- 以便 LoadGLMDir 识别。
func SaveAuth(authDir string, r Result) (string, error) {
	if r.UID == "" {
		return "", fmt.Errorf("missing uid in result")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  r.AccessToken,
			"refreshToken": r.RefreshToken,
			"expiresAt":    r.ExpiresAt,
			"domain":       "chatglm.cn",
		},
		"account": map[string]any{
			"uid":      r.UID,
			"nickname": r.Nickname,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		return "", err
	}
	fp := filepath.Join(authDir, "glm-"+sanitizeUID(r.UID)+".json")
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, fp); err != nil {
		return "", err
	}
	return fp, nil
}

// sanitizeUID 把 uid 里不适合做文件名的字符替换掉（防御性：uid 通常已是安全字符）。
func sanitizeUID(uid string) string {
	var sb strings.Builder
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			sb.WriteRune(r)
		default:
			sb.WriteByte('_')
		}
	}
	s := sb.String()
	if s == "" {
		s = fmt.Sprintf("%d", time.Now().Unix())
	}
	return s
}
