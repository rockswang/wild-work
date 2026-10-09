package raccoon

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wild-work/internal/auth"
)

// 本机小浣熊客户端（box-agent）会话文件候选，与 internal/app/import_local.go 的
// raccoonClientAuthPath 同源：客户端支持用 BOX_AGENT_CONFIG_DIR 覆盖配置目录。
// 返回空串表示本机没有客户端会话。
func clientAuthFile() string {
	if dir := strings.TrimSpace(os.Getenv("BOX_AGENT_CONFIG_DIR")); dir != "" {
		p := filepath.Join(dir, "auth.json")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(home, ".box-agent", "config", "auth.json")
	if st, err := os.Stat(p); err == nil && !st.IsDir() {
		return p
	}
	return ""
}

// jwtName 解出 JWT 的 name claim（小浣熊 uid），失败返回空串。
func jwtName(tok string) string {
	parts := strings.Split(strings.TrimSpace(tok), ".")
	if len(parts) < 2 {
		return ""
	}
	seg := strings.NewReplacer("-", "+", "_", "/").Replace(parts[1])
	if m := len(seg) % 4; m != 0 {
		seg += strings.Repeat("=", 4-m)
	}
	raw, err := base64.StdEncoding.DecodeString(seg)
	if err != nil {
		return ""
	}
	var payload struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	return strings.TrimSpace(payload.Name)
}

// RescueFromClientSession 从本机客户端会话文件采纳最新令牌到 a。
//
// 背景：上游对 refresh_token 做轮换（见 RefreshToken），而客户端（box-agent）与本工具
// 共用同一会话——客户端任一次续期都会轮换 refresh_token，令本工具落盘的旧值立刻失效
// （2026-10-09 实测：refresh 401 authorization_verify_error，且失败累积会触发账号自动停用）。
// 客户端会话文件里始终持有唯一活凭证，故以此为本渠道的会话真相源自救。
//
// 安全约束：仅当客户端令牌的 uid 与账号一致才采纳——多账号场景下客户端登着谁的号，
// 就只救谁，绝不把 A 账号的令牌写进 B 账号。采纳只改内存，落盘由调用方按
// AGENTS §6.20（凡 RefreshToken 成功必紧跟 SaveAtomic）完成。
func RescueFromClientSession(a *auth.Auth) error {
	path := clientAuthFile()
	if path == "" {
		return fmt.Errorf("raccoon: 未找到本机客户端会话（~/.box-agent/config/auth.json）")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("raccoon: 读取客户端会话失败: %w", err)
	}
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("raccoon: 解析客户端会话失败（%s）: %w", path, err)
	}
	access := strings.TrimSpace(payload.AccessToken)
	refresh := strings.TrimSpace(payload.RefreshToken)
	if access == "" || refresh == "" {
		return fmt.Errorf("raccoon: 客户端会话缺少令牌（%s），请先在客户端完成登录", path)
	}
	if uid := jwtName(access); uid == "" || uid != a.UID {
		return fmt.Errorf("raccoon: 客户端会话 uid 不匹配（客户端=%q 账号=%q），拒绝采纳", uid, a.UID)
	}
	// 客户端令牌已过期（如客户端登出后残留旧会话）时不采纳——采纳一个过期
	// access 只会把「必然失败的请求」延迟到下一次上游 401，毫无收益。
	if exp := jwtExp(access); exp > 0 && exp <= time.Now().Unix() {
		return fmt.Errorf("raccoon: 客户端会话 access 已过期（exp=%d），拒绝采纳", exp)
	}
	a.Lock()
	a.AccessToken = access
	a.RefreshToken = refresh
	a.ExpiresAt = jwtExp(access)
	a.Unlock()
	return nil
}
