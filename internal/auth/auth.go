// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 region 判定与 refresh 后的原子写回。
package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或 CPA 面板扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic/JWT 读，防止并发读写 token。
	mu sync.RWMutex

	Kind         string // workbuddy | traework（由文件名前缀或保存逻辑设置）
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	ApiHost      string // TraeWork: https://api.trae.com.cn
	MachineID    string // TraeWork: x-machine-id
	DeviceID     string // TraeWork: x-device-id
	// QoderWork COSY 机器指纹（登录/刷新时生成，持久化到 auth 文件）
	MachineToken string // Qoder: cosy-machinetoken
	MachineType  string // Qoder: cosy-machinetype
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处
}

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// RLock 供读路径持有读锁。
func (a *Auth) RLock() { a.mu.RLock() }

// RUnlock 释放读锁。
func (a *Auth) RUnlock() { a.mu.RUnlock() }

// JWT 返回当前 access token 快照（TraeWork 头也叫 Cloud-IDE-JWT）。
func (a *Auth) JWT() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

// AccessTokenValue 锁内快照：出站请求头用，防止与 keepalive 刷新并发读写 token。
func (a *Auth) AccessTokenValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}

// RefreshTokenValue 锁内快照：防止调度器锁外直读 RefreshToken 与 refresh 写回竞争。
func (a *Auth) RefreshTokenValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.RefreshToken
}

// NeedsRefreshLocked 是 NeedsRefresh 的持锁内部版本；调用方必须已持有读/写锁。
func (a *Auth) NeedsRefreshLocked(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Region 返回 "cn" 或 "global"。domain 为空视为 CN（向后兼容）。
func (a *Auth) Region() string {
	d := strings.ToLower(strings.TrimSpace(a.Domain))
	if d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai") {
		return "global"
	}
	return "cn"
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.NeedsRefreshLocked(within)
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （CPA 面板手建）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
				ApiHost      string `json:"apiHost"`
				MachineID    string `json:"machineId"`
				DeviceID     string `json:"deviceId"`
				MachineToken string `json:"machineToken"`
				MachineType  string `json:"machineType"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  n.Auth.AccessToken,
			RefreshToken: n.Auth.RefreshToken,
			ExpiresAt:    n.Auth.ExpiresAt,
			Domain:       n.Auth.Domain,
			ApiHost:      n.Auth.ApiHost,
			MachineID:    n.Auth.MachineID,
			DeviceID:     n.Auth.DeviceID,
			MachineToken: n.Auth.MachineToken,
			MachineType:  n.Auth.MachineType,
			UID:          n.Account.UID,
			EnterpriseID: n.Account.EnterpriseID,
			Nickname:     n.Account.Nickname,
		}
	} else {
		var f struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Domain       string `json:"domain"`
			ApiHost      string `json:"apiHost"`
			MachineID    string `json:"machineId"`
			DeviceID     string `json:"deviceId"`
			MachineToken string `json:"machineToken"`
			MachineType  string `json:"machineType"`
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:  f.AccessToken,
			RefreshToken: f.RefreshToken,
			ExpiresAt:    f.ExpiresAt,
			Domain:       f.Domain,
			ApiHost:      f.ApiHost,
			MachineID:    f.MachineID,
			DeviceID:     f.DeviceID,
			MachineToken: f.MachineToken,
			MachineType:  f.MachineType,
			UID:          f.UID,
			EnterpriseID: f.EnterpriseID,
			Nickname:     f.Nickname,
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持 CPA 插件可读格式。
// 加锁外壳：防止与 RefreshToken 并发读写 token 字段导致写回半更新。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveAtomicLocked()
}

// saveAtomicLocked 是 SaveAtomic 的持锁内部版本；调用方必须已持有 a.mu。
// 供持锁路径（如 RefreshToken 后写回）使用，避免重复加锁。
func (a *Auth) saveAtomicLocked() error {
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"apiHost":      a.ApiHost,
			"machineId":    a.MachineID,
			"deviceId":     a.DeviceID,
			"machineToken": a.MachineToken,
			"machineType":  a.MachineType,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// LoadDir 扫描 dir 下 workbuddy*.json，只收 wantRegion（"cn"/"global"）。
// 解析失败与 region 不符的文件静默跳过（启动日志由调用方统计）。
func LoadDir(dir, wantRegion string) ([]*Auth, error) { return LoadWorkBuddyDir(dir, wantRegion) }

// LoadWorkBuddyDir 扫描 WorkBuddy 凭证。
func LoadWorkBuddyDir(dir, wantRegion string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil || a.Region() != wantRegion {
			continue
		}
		a.Kind, a.FilePath = "workbuddy", f
		out = append(out, a)
	}
	return out, nil
}

// LoadWorkBuddyAiDir 扫描 WorkBuddy 国际版凭证（workbuddyai-*.json）。
// 不按 region 过滤：国际版凭证的 domain 天然为 www.workbuddy.ai，
// 路由由每个 Auth 自身的 domain 决定，与全局 region 配置无关。
func LoadWorkBuddyAiDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "workbuddyai-*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.Kind, a.FilePath = "workbuddyai", f
		out = append(out, a)
	}
	return out, nil
}

// LoadTraeDir 扫描 TraeWork 凭证（trae-*.json）。
func LoadTraeDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "trae-*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.Kind, a.FilePath = "traework", f
		out = append(out, a)
	}
	return out, nil
}

// LoadQoderDir 扫描 QoderWork 凭证（qoder-*.json / qoderwork-*.json）。
// 注意：qodercn-*.json / qodercom-*.json 也被 qoder*.json 的 glob 命中，需排除（各自归独立加载器）。
func LoadQoderDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "qoder*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		// 排除 QoderCN / QoderCOM 渠道的凭证（独立渠道，不混入）
		base := filepath.Base(f)
		if strings.HasPrefix(base, "qodercn-") || strings.HasPrefix(base, "qodercom-") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.Kind, a.FilePath = "qoder", f
		out = append(out, a)
	}
	return out, nil
}

// LoadQoderCNDir 扫描 QoderCN 凭证（qodercn-*.json，独立渠道）。
// 注意 glob 边界：qodercn- 前缀不会被 LoadQoderCOMDir（qodercom-*）命中，互不干扰。
func LoadQoderCNDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "qodercn-*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.Kind, a.FilePath = "qodercn", f
		out = append(out, a)
	}
	return out, nil
}

// LoadQoderCOMDir 扫描 QoderCOM 凭证（qodercom-*.json，独立渠道，国际版）。
func LoadQoderCOMDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "qodercom-*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.Kind, a.FilePath = "qodercom", f
		out = append(out, a)
	}
	return out, nil
}

// LoadQwenWorkDir 扫描千问办公凭证（qwenwork-*.json）。
// 文件名前缀不能与 qoder*.json 冲突（LoadQoderDir 的 glob 会先吞掉 qwenwork- 前缀，
// 故前缀必须以 q 开头但不含 qoder 字样 —— 取 qwenwork- 无冲突）。
//
// 加载后修正 expiresAt：历史版本把 refresh 响应的 expires_in（秒）误当毫秒，
// 落盘的 expiresAt 比 access token 真实寿命少 ~7 天（实测 607s vs 7 天）。
// 后果是 NeedsRefresh 几乎恒为真 → 每次请求都刷 token → 与千问办公 App 互踩。
// access token 的 JWT exp 由上游签名、权威可信，故以其为准原地校正（仅内存，
// 不写盘：校正后不再触发刷新路径，也就无需回写；token 真过期或刷新后自然落盘正确值）。
func LoadQwenWorkDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "qwenwork-*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.Kind, a.FilePath = "qwenwork", f
		a.AdoptJWTExpiry()
		out = append(out, a)
	}
	return out, nil
}

// AdoptJWTExpiry 用 access token 的 JWT exp 校正本地 expiresAt（仅当 JWT 更晚时）。
// 用于修复历史上 expires_in 单位误判造成的偏短 expiresAt（见 LoadQwenWorkDir）。
// 只在 exp 可解析且确实晚于当前值时才覆盖，避免把正常值改坏。
func (a *Auth) AdoptJWTExpiry() {
	exp := jwtExpiry(a.JWT())
	if exp <= 0 {
		return
	}
	a.Lock()
	defer a.Unlock()
	if exp > a.ExpiresAt {
		a.ExpiresAt = exp
	}
}

// jwtExpiry 解出 JWT payload 的 exp（Unix 秒）；非 JWT/无 exp 时返回 0。
// 不校验签名：用途仅是从本地凭证自身的 payload 读出自报到期时刻。
func jwtExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	p := parts[1]
	if r := len(p) % 4; r != 0 {
		p += strings.Repeat("=", 4-r)
	}
	raw, err := base64.URLEncoding.DecodeString(p)
	if err != nil {
		return 0
	}
	var payload struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return 0
	}
	return payload.Exp
}
