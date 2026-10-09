package app

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"wild-work/internal/monkeycode"
	"wild-work/internal/provider"
)

// 本文件实现「导入型」渠道的凭据获取：凭据由官方客户端在本机维护，
// 本工具不去复现它的登录流程，而是直接读取客户端落盘的凭据文件。
//
// MonkeyCode 的凭据来自官方客户端（ohmyagent）的 settings.json：
// 一个账号 = oma_ api_key + omas_ signing_secret，另有控制台侧的会话 Cookie。
//
// 小浣熊的凭据来自官方客户端（%USERPROFILE%\.box-agent\configuth.json 明文 JSON）。
// 面板另有「浏览器授权登录」主路径（见 internal/login_raccoon），导入作为回退。
//
// Loomy 的凭据来自官方客户端的会话文件（auth-session.json）。
//
// 设计要点：
//   - 路径**自适应探测**（多候选 + 环境变量覆盖），不做硬编码单一路径；
//   - Windows/macOS 均生效（Loomy/小浣熊/MonkeyCode 官方客户端 2026 年起均有 macOS 版，
//     凭据路径按平台分别探测；找不到时返回带路径的错误提示）；
//   - 写入走 tmp+rename 原子替换、0600，并与 internal/auth.Parse 的嵌套格式逐字段对齐；
//   - **绝不打印凭据值**（日志只记文件名与 uid）。

// ImportLocalResult 导入结果（供面板展示）。
type ImportLocalResult struct {
	Channel string `json:"channel"`
	UID     string `json:"uid"`
	File    string `json:"file"`
	Note    string `json:"note,omitempty"`
}

// authDoc / authSection / accountSection 与 internal/auth.Parse 的嵌套分支严格对应。
type authDoc struct {
	Auth    authSection    `json:"auth"`
	Account accountSection `json:"account"`
}

type authSection struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	ApiHost      string `json:"apiHost"`
	MachineID    string `json:"machineId"`
	DeviceID     string `json:"deviceId"`
	MachineToken string `json:"machineToken"`
	MachineType  string `json:"machineType"`
	// SigningSecret 只在 MonkeyCode 凭据里非空（omas_ secret，见 internal/monkeycode）
	SigningSecret string `json:"signingSecret"`
	// ConsoleCookie 只在 MonkeyCode 凭据里非空（控制台会话 Cookie，见 internal/auth）
	ConsoleCookie string `json:"consoleCookie"`
	// BaizhiCookie 只在 MonkeyCode 凭据里非空（百智云会话 Cookie，控制台会话的上游来源）
	BaizhiCookie string `json:"baizhiCookie"`
}

type accountSection struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// ImportLocalCredentials 从本机已安装的官方客户端导入指定渠道凭据。
//
// 跨平台口径（issue #75）：不在此处做整体 Windows 门禁——三个导入渠道的
// 路径探测各自尽力而为、找不到客户端凭据时返回明确错误。
//  - Raccoon 用 os.UserHomeDir() + BOX_AGENT_CONFIG_DIR，天然跨平台
//    （macOS/Linux 同样有官方客户端）；
//  - Loomy / MonkeyCode 的路径候选以 POSIX/Win 通用环境变量为主，
//    客户端配置落在标准目录即可命中，非 Windows 上找不到时给出明确报错。
//  注：真正仅 Windows 的是「浏览器授权登录」（raccoon 依赖 HKCU 的
//  office-raccoon:// 协议注册，见 protocol_windows.go）。
func (a *App) ImportLocalCredentials(channel string) (*ImportLocalResult, error) {
	switch provider.Kind(strings.TrimSpace(channel)) {
	case provider.MonkeyCode:
		return a.importMonkeyCode()
	case provider.Raccoon:
		return a.importRaccoon()
	case provider.Loomy:
		return a.importLoomy()
	}
	return nil, fmt.Errorf("渠道 %q 不支持本地导入（该渠道请用面板的登录按钮）", channel)
}

// importMonkeyCode 读取 MonkeyCode 官方客户端（ohmyagent）的 settings.json，
// 取出平台托管模型的 api_key 与顶层 signing_secret，落成本工具凭据。
//
// settings.json 结构（2026-09-23 实测）：
//
//	{
//	  "signing_secret": "omas_…",            <- 顶层字段，与 api_key 是**两把不同的密钥**
//	  "models": {                            <- 对象而非数组
//	    "monkeycode-basic/deepseek-flash@monkeycode#<uuid>": {
//	      "api_key": "oma_…", "base_url": "https://proxy.monkeycode-ai.com/v1",
//	      "type": "anthropic", "model": "…"
//	    }, …
//	  }
//	}
//
// 一个账号只有一对 (api_key, signing_secret)，与具体模型无关 → 任取一条托管条目即可
// （按模型名排序取首条，保证同一份配置多次导入结果一致）。
func (a *App) importMonkeyCode() (*ImportLocalResult, error) {
	path, err := monkeyCodeSettingsPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取客户端配置失败（%s）：%w", path, err)
	}
	var payload struct {
		SigningSecret string `json:"signing_secret"`
		Models        map[string]struct {
			APIKey  string `json:"api_key"`
			BaseURL string `json:"base_url"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("解析客户端配置失败（%s）：%w", path, err)
	}
	secret := strings.TrimSpace(payload.SigningSecret)
	if secret == "" {
		return nil, fmt.Errorf("客户端配置里没有 signing_secret（%s）：请先在 MonkeyCode 客户端登录", path)
	}
	names := make([]string, 0, len(payload.Models))
	for name := range payload.Models {
		if strings.HasPrefix(name, "monkeycode-") {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("客户端配置里没有平台托管模型（%s）：请先在 MonkeyCode 客户端登录", path)
	}
	sort.Strings(names)
	entry := payload.Models[names[0]]
	key := strings.TrimSpace(entry.APIKey)
	if key == "" {
		return nil, fmt.Errorf("客户端配置里的托管条目没有 api_key（%s）", path)
	}
	host, path0 := splitBaseURL(strings.TrimSpace(entry.BaseURL))
	// 两侧会话 Cookie 都是**尽力而为**：拿不到不影响导入本身。
	// 控制台会话短寿（≈6 天）但可由百智云会话（≈29 天）自动续期，故两支都取。
	consoleCookie, consoleWhy := monkeyCodeCookie("monkeycode-cookies.json", monkeycode.CookieNameConsole)
	baizhiCookie, baizhiWhy := monkeyCodeCookie("baizhi-cookies.json", monkeycode.CookieNameBaizhi)
	// 上游无 uid：用 api_key 派生稳定标识，避免同一账号重复导入生成多个凭据文件。
	sum := sha256.Sum256([]byte(key))
	uid := "mc_" + hex.EncodeToString(sum[:5])
	doc := authDoc{
		Auth: authSection{
			AccessToken: key,
			// 无 refresh 端点：留空，scheduler 会跳过保活（不产生无意义失败）。
			RefreshToken: "",
			// 远期到期：避免 NeedsRefresh 恒真触发一次必然失败的刷新。
			ExpiresAt:     time.Now().AddDate(50, 0, 0).Unix(),
			Domain:        path0,
			ApiHost:       host,
			SigningSecret: secret,
			ConsoleCookie: consoleCookie,
			BaizhiCookie:  baizhiCookie,
		},
		Account: accountSection{UID: uid, Nickname: "MonkeyCode " + strings.TrimPrefix(uid, "mc_")},
	}
	file, err := a.writeAuthFile("monkeycode", uid, doc)
	if err != nil {
		return nil, err
	}
	a.reloadAccounts()
	a.afterAccountAdded(provider.MonkeyCode)
	// 提示只在有降级时出现（见 monkeyCodeImportNote）。
	note := monkeyCodeImportNote(consoleWhy, baizhiWhy)
	return &ImportLocalResult{
		Channel: "monkeycode", UID: uid, File: filepath.Base(file),
		Note: note,
	}, nil
}

// monkeyCodeImportNote 拼导入提示：**只在有降级时非空**。
//
// 正常导入不必复述凭据来源（面板 toast 已说"已导入 <渠道> 账号 xxx"），
// 而这句会原样进 toast（单行条、默认 3s），所以每句都要短且可行动。
func monkeyCodeImportNote(consoleWhy, baizhiWhy string) string {
	note := ""
	if consoleWhy != "" {
		note += "未取到控制台会话（" + consoleWhy + "），积分暂不可用（不影响对话）。"
	}
	if baizhiWhy != "" {
		note += "未取到百智云会话（" + baizhiWhy + "），控制台会话过期后需重新导入。"
	}
	return note
}

// monkeyCodeCookie 从客户端 cookie 文件里取指定名字的 Cookie 值（尽力而为）。
//
// 为什么要取两支（控制台 + 百智云）：控制台会话短寿（≈6 天）但可由百智云会话
// （≈29 天）自动派生续期。
//
// 与 api_key/signing_secret 不同，控制台接口（积分钱包等）只认 Cookie，而本工具
// 没有登录流程 —— 因此文件缺失、读取失败或已过期都只返回空串 + 一句**原因**，
// 由调用方决定怎么向用户表述（都不影响凭据导入本身）。
//
// 返回的第二个值是简短原因（"文件缺失"/"已过期"…），空串表示取到了值。
func monkeyCodeCookie(file, name string) (string, string) {
	path, err := monkeyCodeCookiePath(file)
	if err != nil {
		return "", "文件缺失"
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "读取失败"
	}
	// 客户端落盘格式（2026-09-24 实测）：
	//   [{"name":"monkeycode_ai_session","value":"…","expires":"2026-10-01T10:17:37Z"}, …]
	var items []struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		Expires string `json:"expires"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return "", "解析失败"
	}
	for _, it := range items {
		if it.Name != name {
			continue
		}
		if exp, perr := time.Parse(time.RFC3339, it.Expires); perr == nil && !exp.IsZero() && time.Now().After(exp) {
			return "", "已过期"
		}
		if v := strings.TrimSpace(it.Value); v != "" {
			return v, ""
		}
	}
	return "", "文件里没有 " + name
}

// monkeyCodeCookiePath 定位客户端保存 Cookie 的文件。
//
// 这些文件与 settings.json **不同级**：settings.json 在 ohmyagent 子目录，
// cookie 直接落在 bundle 根目录（%APPDATA%\com.chaitin.baizhi.monkeycode\），
// 且按来源分两个文件（monkeycode-cookies.json / baizhi-cookies.json）。
func monkeyCodeCookiePath(file string) (string, error) {
	var cands []string
	if dir := strings.TrimSpace(os.Getenv("MONKEYCODE_CONFIG_DIR")); dir != "" {
		cands = append(cands, filepath.Join(dir, file))
	}
	if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
		cands = append(cands, filepath.Join(appData, "com.chaitin.baizhi.monkeycode", file))
	}
	if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" {
		cands = append(cands, filepath.Join(local, "com.chaitin.baizhi.monkeycode", file))
		cands = append(cands, filepath.Join(local, "MonkeyCode", file))
	}
	// macOS：Electron 数据目录 `~/Library/Application Support/com.chaitin.baizhi.monkeycode/`
	// （2026-10-08 实测 macOS 版客户端目录与 Windows 版同名，cookie 文件格式一致）。
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, "Library", "Application Support", "com.chaitin.baizhi.monkeycode", file))
	}
	for _, p := range cands {
		if fileExists(p) {
			return p, nil
		}
	}
	last := ""
	if len(cands) > 0 {
		last = cands[len(cands)-1]
	}
	return "", fmt.Errorf("未找到 MonkeyCode Cookie 文件 %s（已尝试 %d 个路径，最后一个是 %s）", file, len(cands), last)
}

// splitBaseURL 把客户端 base_url 拆成 (host, path)。
// 例：https://proxy.monkeycode-ai.com/v1 → ("https://proxy.monkeycode-ai.com", "/v1")。
// 空值回落到内置默认端点。
func splitBaseURL(base string) (host, path string) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = monkeycode.DefaultBase
	}
	if i := strings.Index(base, "://"); i >= 0 {
		rest := base[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return base[:i+3+len(rest[:j])], rest[j:]
		}
		return base, "/v1"
	}
	return base, "/v1"
}

// monkeyCodeSettingsPath 定位 MonkeyCode 客户端（ohmyagent）的 settings.json（自适应多候选）。
//
// 客户端是 Go 程序，配置由桌面壳写在 %APPDATA%\<bundle-id>\ohmyagent 下
// （bundle id = com.chaitin.baizhi.monkeycode）；LOCALAPPDATA 作为次候选兜底。
func monkeyCodeSettingsPath() (string, error) {
	var cands []string
	if dir := strings.TrimSpace(os.Getenv("MONKEYCODE_CONFIG_DIR")); dir != "" {
		cands = append(cands, filepath.Join(dir, "settings.json"))
	}
	if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
		cands = append(cands, filepath.Join(appData, "com.chaitin.baizhi.monkeycode", "ohmyagent", "settings.json"))
	}
	if local := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); local != "" {
		cands = append(cands, filepath.Join(local, "com.chaitin.baizhi.monkeycode", "ohmyagent", "settings.json"))
		cands = append(cands, filepath.Join(local, "MonkeyCode", "ohmyagent", "settings.json"))
	}
	// macOS：settings.json 在 `~/Library/Application Support/com.chaitin.baizhi.monkeycode/ohmyagent/`
	// （2026-10-08 实测与 Windows 版同级目录结构一致）。
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, "Library", "Application Support", "com.chaitin.baizhi.monkeycode", "ohmyagent", "settings.json"))
	}
	for _, p := range cands {
		if fileExists(p) {
			return p, nil
		}
	}
	last := ""
	if len(cands) > 0 {
		last = cands[len(cands)-1]
	}
	return "", fmt.Errorf("未找到 MonkeyCode 客户端配置（已尝试 %d 个路径，最后一个是 %s）：请先安装并登录 MonkeyCode 客户端",
		len(cands), last)
}

// importLoomy 读取讯飞 Loomy 客户端的 auth-session.json 并转换成本工具格式。
func (a *App) importLoomy() (*ImportLocalResult, error) {
	path, err := loomyClientSessionPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取客户端凭据失败（%s）：%w", path, err)
	}
	var payload struct {
		Session   string `json:"session"`
		UserID    string `json:"userid"`
		Phone     string `json:"phone"`
		UpdatedAt int64  `json:"updatedAt"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("解析客户端凭据失败（%s）：%w", path, err)
	}
	session := strings.TrimSpace(payload.Session)
	if session == "" {
		return nil, fmt.Errorf("客户端凭据里没有 session（%s）：请先在客户端完成登录", path)
	}
	uid := strings.TrimSpace(payload.UserID)
	if uid == "" {
		uid = "default"
	}
	// Loomy 无 refresh 端点，且 session 是客户端登录时申请 14 天有效期的短期凭据。
	// 用 updatedAt + 14d 估算到期时刻：避免 NeedsRefreshLocked 因 ExpiresAt<=0 恒真
	// 而触发一个必然失败的刷新。
	base := payload.UpdatedAt
	if base > 1e12 { // 毫秒 → 秒
		base /= 1000
	}
	if base <= 0 {
		base = time.Now().Unix()
	}
	doc := authDoc{
		Auth: authSection{
			AccessToken: session,
			// 故意留空：scheduler 对 refreshToken 为空的账号会跳过保活（不会产生无意义失败）。
			RefreshToken: "",
			ExpiresAt:    base + 14*24*3600,
			Domain:       "/api/v1",
			ApiHost:      "https://loomyad.xunfei.cn",
		},
		Account: accountSection{UID: uid},
	}
	file, err := a.writeAuthFile("loomy", uid, doc)
	if err != nil {
		return nil, err
	}
	a.reloadAccounts()
	a.afterAccountAdded(provider.Loomy)
	return &ImportLocalResult{
		Channel: "loomy", UID: uid, File: filepath.Base(file),
		Note: "Loomy 无 refresh 端点（session 约 14 天），到期后需在客户端重新登录并再次导入",
	}, nil
}

// loomyClientSessionPath 定位 Loomy 客户端会话文件（自适应多候选）。
//
// Loomy 的配置根目录是 `C:\Users\Public\Loomy\<sha256(用户名)[:12]>`（见客户端 config-path.js），
// 因此这里**枚举该目录下所有 hash 子目录**，而不是自己算 hash ——
// 这样跨账户、跨版本都能命中。
func loomyClientSessionPath() (string, error) {
	var cands []string
	publicRoot := filepath.Join(`C:\Users\Public`, "Loomy")
	if entries, err := os.ReadDir(publicRoot); err == nil {
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			cands = append(cands, filepath.Join(publicRoot, e.Name(), "userData", "auth-session.json"))
		}
	}
	if appData := strings.TrimSpace(os.Getenv("APPDATA")); appData != "" {
		cands = append(cands, filepath.Join(appData, "Loomy", "auth-session.json"))
	}
	// macOS：客户端数据目录 `~/Library/Application Support/loomy/`（2026-10-08 实测
	// 桌面版凭据即该目录下的 auth-session.json，字段与 Windows 版一致）。
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, "Library", "Application Support", "loomy", "auth-session.json"))
	}
	for _, p := range cands {
		if fileExists(p) {
			return p, nil
		}
	}
	if len(cands) == 0 {
		return "", errors.New("未找到 Loomy 客户端目录（C:\\Users\\Public\\Loomy 或 ~/Library/Application Support/loomy 不存在）：请先安装并登录 Loomy 客户端")
	}
	return "", fmt.Errorf("未找到 Loomy 会话文件（已尝试 %d 个路径）：请先在客户端完成登录", len(cands))
}

// writeAuthFile 原子写 auth 文件（tmp + rename，0600），文件名前缀即渠道 Kind。
func (a *App) writeAuthFile(prefix, uid string, doc authDoc) (string, error) {
	if err := os.MkdirAll(a.cfg.AuthDir, 0o700); err != nil {
		return "", fmt.Errorf("创建凭据目录失败：%w", err)
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	raw = append(raw, '\n')
	file := filepath.Join(a.cfg.AuthDir, prefix+"-"+sanitizeUID(uid)+".json")
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", fmt.Errorf("写入凭据失败：%w", err)
	}
	if err := os.Rename(tmp, file); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("保存凭据失败：%w", err)
	}
	return file, nil
}

// sanitizeUID 过滤文件名里的危险字符（uid 来自 token，可能含路径分隔符等）。
func sanitizeUID(uid string) string {
	var sb strings.Builder
	for _, r := range uid {
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

// importRaccoon 读取小浣熊客户端的 auth.json 并转换成本工具格式。
func (a *App) importRaccoon() (*ImportLocalResult, error) {
	path, err := raccoonClientAuthPath()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取客户端凭据失败（%s）：%w", path, err)
	}
	var payload struct {
		AccessToken    string `json:"access_token"`
		RefreshToken   string `json:"refresh_token"`
		OfficeIdentity string `json:"office_identity"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("解析客户端凭据失败（%s）：%w", path, err)
	}
	access := strings.TrimSpace(payload.AccessToken)
	if access == "" {
		return nil, fmt.Errorf("客户端凭据里没有 access_token（%s）：请先在客户端完成登录", path)
	}
	uid := strings.TrimSpace(jwtClaim(access, "name"))
	if uid == "" {
		uid = "default"
	}
	doc := authDoc{
		Auth: authSection{
			AccessToken:  access,
			RefreshToken: strings.TrimSpace(payload.RefreshToken),
			ExpiresAt:    jwtExp(access),
			Domain:       "/api/web/llm/v2",
			ApiHost:      "https://xiaohuanxiong.com",
		},
		Account: accountSection{
			UID:          uid,
			EnterpriseID: strings.TrimSpace(payload.OfficeIdentity),
			Nickname:     uid,
		},
	}
	file, err := a.writeAuthFile("raccoon", uid, doc)
	if err != nil {
		return nil, err
	}
	a.reloadAccounts()
	a.afterAccountAdded(provider.Raccoon)
	return &ImportLocalResult{
		Channel: "raccoon", UID: uid, File: filepath.Base(file),
		Note: "access_token 约 2 小时有效，wild-work 会用 refresh_token 自动续期（上游会轮换 refresh_token，新值已随刷新落盘）",
	}, nil
}

// raccoonClientAuthPath 定位小浣熊客户端凭据（自适应多候选）。
func raccoonClientAuthPath() (string, error) {
	var cands []string
	// 客户端支持用 BOX_AGENT_CONFIG_DIR 覆盖配置目录（见 boxAgentAuthFile.js）。
	if dir := strings.TrimSpace(os.Getenv("BOX_AGENT_CONFIG_DIR")); dir != "" {
		cands = append(cands, filepath.Join(dir, "auth.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, ".box-agent", "config", "auth.json"))
	}
	for _, p := range cands {
		if fileExists(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("未找到小浣熊客户端凭据（已尝试 %d 个路径，最后一个是 %s）：请先安装并登录「小浣熊」客户端",
		len(cands), cands[len(cands)-1])
}

// jwtPayload 解出 JWT 的 payload（失败返回 nil）。
func jwtPayload(tok string) map[string]any {
	parts := strings.Split(strings.TrimSpace(tok), ".")
	if len(parts) < 2 {
		return nil
	}
	seg := strings.NewReplacer("-", "+", "_", "/").Replace(parts[1])
	if m := len(seg) % 4; m != 0 {
		seg += strings.Repeat("=", 4-m)
	}
	raw, err := base64.StdEncoding.DecodeString(seg)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

func jwtClaim(tok, key string) string {
	m := jwtPayload(tok)
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func jwtExp(tok string) int64 {
	m := jwtPayload(tok)
	if m == nil {
		return 0
	}
	if f, ok := m["exp"].(float64); ok {
		return int64(f)
	}
	return 0
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
