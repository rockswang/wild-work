package officeace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// 本文件实现「导入型」路径：从本机已登录的 OfficeAce 桌面端文件里发现网关凭据。
//
// 上游模型网关地址 + 鉴权头由桌面端同步到本机磁盘：
//
//	<dataRoot>/.jiuwenclaw/config/routing_state/users/<uid>/models.json
//
// 每条 entry 形如：
//
//	{"model_client_config":{"api_base":"https://modelgw-.../v2","api_key":"huawei-maas-session",
//	  "model_name":"deepseek-v4.1-flash","custom_headers":{"Authorization":"Basic <b64(AK:SK)>"}}}
//
// dataRoot 默认 %USERPROFILE%\.office-claw（或 $OFFICEACE_HOME，或各盘 OfficeAce\.office-claw）。
// **只读、绝不写桌面端任何文件**。

const (
	placeholderKey = "huawei-maas-session"
)

// placeholderKeys 视为占位符的 api_key（不作为真实凭据）。
var placeholderKeys = map[string]bool{
	"": true, placeholderKey: true, "placeholder-api-key": true,
	"sk-xxxxxxxxx": true, "your-api-key": true,
}

// Discover 扫描本机 OfficeAce 文件，返回发现的账号（可能为空）。
// installDirs 为额外安装根目录候选（可空）。
func Discover(installDirs []string) ([]*Account, error) {
	var out []*Account
	seen := map[string]bool{}
	for _, root := range dataRoots(installDirs) {
		for _, dir := range routingStateDirs(root) {
			entries, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				uid := decodeUID(e.Name())
				mp := filepath.Join(dir, e.Name(), "models.json")
				acc := parseModelsJSON(mp, uid)
				if acc == nil {
					continue
				}
				enrichWithProfile(acc, installDirs)
				if seen[acc.ID] {
					continue
				}
				seen[acc.ID] = true
				out = append(out, acc)
			}
		}
	}
	return out, nil
}

// dataRoots 计算数据根目录候选（默认 home/.office-claw + 各安装根/.office-claw）。
func dataRoots(installDirs []string) []string {
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		for _, x := range out {
			if x == p {
				return
			}
		}
		out = append(out, p)
	}
	home := strings.TrimSpace(os.Getenv("OFFICEACE_HOME"))
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	if home != "" {
		add(filepath.Join(home, ".office-claw"))
	}
	for _, r := range installRoots(installDirs) {
		add(filepath.Join(r, ".office-claw"))
	}
	return out
}

// installRoots 计算安装根目录候选（环境变量 + 各盘常见目录）。
func installRoots(installDirs []string) []string {
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		if st, err := os.Stat(p); err != nil || !st.IsDir() {
			return
		}
		for _, x := range out {
			if x == p {
				return
			}
		}
		out = append(out, p)
	}
	for _, d := range installDirs {
		add(d)
	}
	add(os.Getenv("OFFICEACE_INSTALL_DIR"))
	mids := []string{"软件", "Program Files", "Program Files (x86)", "Apps"}
	// Windows 盘符候选（非 Windows 下 os.Stat 失败，自然跳过）。
	for _, drive := range []string{`C:\`, `D:\`, `E:\`, `F:\`} {
		for _, mid := range mids {
			add(filepath.Join(drive, mid, "OfficeAce"))
		}
	}
	// macOS：应用在 /Applications/OfficeAce.app，配置通常在 home/.office-claw（已在 dataRoots）。
	add("/Applications/OfficeAce.app")
	return out
}

// routingStateDirs 返回数据根下可能存放 models.json 的目录候选。
func routingStateDirs(root string) []string {
	return []string{
		filepath.Join(root, ".jiuwenclaw", "config", "routing_state", "users"),
		filepath.Join(root, "relayclaw", "jiuwenclaw", "config", "routing_state", "users"),
		filepath.Join(root, "plugin-host", "routing_state", "users"),
	}
}

// parseModelsJSON 解析一个 models.json，聚合成一个 Account。
// 目录名是 URL 编码的 "<accountId>:<userId>"；解不出时用原始目录名。
func parseModelsJSON(path, uid string) *Account {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	lists := collectModelLists(doc)
	var baseURL, authorization string
	var models []string
	for _, list := range lists {
		for _, it := range list {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			mcc, ok := m["model_client_config"].(map[string]any)
			if !ok {
				continue
			}
			if baseURL == "" {
				baseURL = firstString(mcc, "api_base", "baseUrl", "base_url")
			}
			if authorization == "" {
				authorization = authFromConfig(mcc)
			}
			if mn := firstString(mcc, "model_name", "modelName"); mn != "" {
				models = append(models, mn)
			}
		}
	}
	if baseURL == "" && authorization == "" {
		return nil
	}
	acc := &Account{
		ID:            uid,
		BaseURL:       bareGateway(baseURL),
		Authorization: authorization,
		ModelNames:    dedupe(models),
		Source:        "disk",
		Region:        regionFromBaseURL(baseURL),
	}
	if i := strings.IndexByte(uid, ':'); i > 0 {
		acc.AccountID = uid[:i]
		acc.UserID = uid[i+1:]
	}
	return acc
}

// collectModelLists 收集 models.json 里的模型条目列表（defaults 数组或各 provider 键）。
func collectModelLists(doc map[string]any) [][]any {
	var lists [][]any
	if d, ok := doc["defaults"].([]any); ok {
		lists = append(lists, d)
	}
	for k, v := range doc {
		if k == "defaults" || k == "_meta" {
			continue
		}
		if arr, ok := v.([]any); ok {
			lists = append(lists, arr)
			continue
		}
		if obj, ok := v.(map[string]any); ok {
			if arr, ok := obj["entries"].([]any); ok {
				lists = append(lists, arr)
			}
		}
	}
	return lists
}

// authFromConfig 从 model_client_config 里取 Authorization 头值（多种键名兼容）。
func authFromConfig(mcc map[string]any) string {
	for _, hk := range []string{"custom_headers", "customHeaders", "headers"} {
		if h, ok := mcc[hk].(map[string]any); ok {
			for k, v := range h {
				if strings.EqualFold(k, "Authorization") {
					if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
						return strings.TrimSpace(s)
					}
				}
			}
		}
	}
	// 无 Authorization 头时，若 api_key 非占位则包成 Bearer。
	if s, ok := mcc["api_key"].(string); ok {
		s = strings.TrimSpace(s)
		if !placeholderKeys[s] {
			return "Bearer " + s
		}
	}
	return ""
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// decodeUID 把目录名（URL 编码）解回真实账号 ID。
func decodeUID(dirName string) string {
	if dec, err := urlPathUnescape(dirName); err == nil {
		return dec
	}
	return dirName
}
