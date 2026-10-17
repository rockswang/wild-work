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
	"strconv"
	"strings"
	"time"

	"wild-work/internal/monkeycode"
	"wild-work/internal/officeace"
	"wild-work/internal/provider"
	"wild-work/internal/raccoon"
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
	// Extra 渠道扩展凭据袋（officeace 用；其余渠道空）
	Extra map[string]string `json:"extra,omitempty"`
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
//   - Raccoon 用 os.UserHomeDir() + BOX_AGENT_CONFIG_DIR，天然跨平台
//     （macOS/Linux 同样有官方客户端）；
//   - Loomy / MonkeyCode 的路径候选以 POSIX/Win 通用环境变量为主，
//     客户端配置落在标准目录即可命中，非 Windows 上找不到时给出明确报错。
//     注：真正仅 Windows 的是「浏览器授权登录」（raccoon 依赖 HKCU 的
//     office-raccoon:// 协议注册，见 protocol_windows.go）。
func (a *App) ImportLocalCredentials(channel string) (*ImportLocalResult, error) {
	switch provider.Kind(strings.TrimSpace(channel)) {
	case provider.MonkeyCode:
		return a.importMonkeyCode()
	case provider.Raccoon:
		return a.importRaccoon()
	case provider.Loomy:
		return a.importLoomy()
	case provider.OfficeAce:
		return a.importOfficeAce()
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

// importOfficeAce 扫描本机已登录的 OfficeAce 桌面端文件，发现华为云果办网关凭据。
//
// 与其它导入渠道不同，本渠道一个桌面端可能挂多个华为账号（多份 routing_state/users/<uid>），
// 因此逐账号导入。网关凭据（Basic AK/SK）只是对话入口；积分/签到所需的临时凭据需后续续期获取。
//
// ⚠️ 同一个 uid 的 auth 文件也是**本工具自助登录**的落盘位置（internal/officeace/login.go），
// 而导入侧通常没有临时 AK/SK / refresh_token / dpop_priv（只从桌面端文件发现网关 Basic）。
// 因此写入前若目标文件已存在，必须走 writeOfficeAceAuth 的**按字段合并**：否则一次导入就会把
// 浏览器登录的成果清空 —— 实证后果是 extra.access_key 与 auth.refreshToken 变空，积分查询用
// **空 AK** 签名（订阅接口 401 APIG.0303 "app not found, appkey "），签到退化「无临时凭据」。
func (a *App) importOfficeAce() (*ImportLocalResult, error) {
	accs, err := officeace.Discover(nil)
	if err != nil {
		return nil, err
	}
	if len(accs) == 0 {
		return nil, fmt.Errorf("未找到本机 OfficeAce 客户端凭据：请先安装并登录 OfficeAce 桌面端，且至少发起过一次对话")
	}
	firstUID, firstFile := "", ""
	keptCred, noRT := 0, 0
	for _, acc := range accs {
		ex := acc.Export()
		doc := authDoc{
			Auth: authSection{
				AccessToken: ex.AccessToken,
				// refresh_token 必须落盘（否则 ~2h 后临时凭据到期，积分/签到永久无法续期，
				// 而对话不受影响 ⇒ 极隐蔽）。缺它时为空串，面板会以「无临时凭据」提示；
				// 若本工具已登录过该账号，writeOfficeAceAuth 会保留既有的 refresh_token。
				RefreshToken: ex.RefreshToken,
				// 网关 Basic 凭据不过期；续期是临时凭据的事，故这里用远期到期避免误触刷新。
				// （目标文件已存在且留有 OAuth 临时凭据/refresh_token 时，合并会改用与它们匹配的到期时间；
				// 该哨兵只对「纯网关 Basic 导入」生效——见 mergeOfficeAceDoc 的三级优先级。）
				ExpiresAt: officeAceImportedSentinel(),
				ApiHost:   ex.ApiHost,
				Extra:     ex.Extra,
			},
			Account: accountSection{UID: ex.UID, Nickname: ex.Nickname},
		}
		file, info, merr := a.writeOfficeAceAuth(ex.UID, doc)
		if merr != nil {
			return nil, merr
		}
		if info.KeptCredential {
			keptCred++
		}
		if !info.HasRefreshToken {
			noRT++
		}
		if firstUID == "" {
			firstUID, firstFile = ex.UID, filepath.Base(file)
		}
	}
	a.reloadAccounts()
	a.afterAccountAdded(provider.OfficeAce)
	note := fmt.Sprintf("已导入 %d 个 OfficeAce 账号", len(accs))
	if keptCred > 0 {
		// 区分「导入本来就没有」与「已保留既有凭据」：保留下来的账号通常不需要重新登录
		// （其 refresh_token/临时凭据仍在）。措辞精确到「凭据」，因为 expiresAt 会被**纠正**为
		// 那批凭据自己的到期时间（而不是原样沿用），这点差异对排障有意义（见 mergeOfficeAceDoc）。
		note += fmt.Sprintf("（其中 %d 个保留了本工具已保存的 OAuth 凭据，未被本次导入的网关凭据覆盖；其到期时间已按保留凭据自己的到期对齐）", keptCred)
	}
	if noRT > 0 {
		// 无 refresh_token 的账号只能对话，积分/签到会退化成「无临时凭据」（需在本工具重新登录）。
		// 判据是**合并后**的结果（info.HasRefreshToken），故「本次导入没带、但旧文件里有」的账号不计入。
		note += fmt.Sprintf("（其中 %d 个未取得 refresh_token，积分/签到需在本工具重新登录一次）", noRT)
	}
	note += "；每日签到的临时凭据约 2 小时有效，wild-work 会自动续期"
	return &ImportLocalResult{Channel: "officeace", UID: firstUID, File: firstFile, Note: note}, nil
}

// officeAceMergeInfo 描述一次 OfficeAce 凭据写入中「保留旧值」的情况（供提示文案与回归测试断言）。
type officeAceMergeInfo struct {
	Merged           bool // 目标文件已存在，本次走了按字段合并（而非全新写入）
	KeptCredential   bool // 保留了既有的 OAuth 临时凭据（见 officeAceOAuthCredKeys）
	KeptRefreshToken bool // 保留了既有的 refresh_token（导入侧为空）
	KeptExpiresAt    bool // 最终 expiresAt ≠ 导入侧给的值（即被合并规则改写过；仅用于测试断言与提示）
	HasRefreshToken  bool // 写盘后该账号是否仍有 refresh_token
}

// officeAceOAuthCredKeys 是 OAuth 登录产物里的**凭据类**扩展字段。
//
// 说明：所有 extra 字段都遵循同一条规则（导入值非空 → 用导入值；导入值为空 → 保留旧值），
// 这个列表只用于判定「本次是否保留了 OAuth 凭据」——它决定 expiresAt 的取值口径与提示文案。
// 刻意**不含死名单** dead_tokens：它是黑名单不是凭据，保留它本身不构成「已保留凭据」。
var officeAceOAuthCredKeys = []string{
	"access_key",     // 临时 AK：积分/签到签名用（空 AK ⇒ 订阅接口 401 APIG.0303 "app not found, appkey "）
	"secret_key",     // 临时 SK
	"security_token", // 临时 STS token
	"project_id",     // 控制面项目
	"cred_expires",   // 临时凭据到期（与 auth.expiresAt 配对）
	"refresh_expire", // refresh_token 到期
	"dpop_priv",      // DPoP 私钥 JWK（续期必需，丢了只能重新登录）
}

// writeOfficeAceAuth 写 OfficeAce 凭据：目标文件已存在时**按字段合并**后原子写回，否则全新写入。
//
// 合并规则（仅在旧文件存在时生效；字段 → 优先级）：
//
//	字段                                       优先级
//	auth.accessToken（网关 Basic）              导入值；导入为空 → 保留旧值
//	auth.refreshToken                          导入值；导入为空 → 保留旧值
//	auth.domain/apiHost/machine*               导入值；导入为空 → 保留旧值
//	auth.expiresAt                             见下（不是简单的导入优先）
//	extra.base_url / models / account_id /
//	  user_id / label / region / source        导入值（导入路径是这些字段的权威来源）；
//	                                           导入为空 → 保留旧值
//	extra 的 OAuth 凭据类字段（见
//	  officeAceOAuthCredKeys）与 dead_tokens   导入值；导入为空/缺失 → 保留旧值
//	account.uid / enterpriseId / nickname      导入值；导入为空 → 保留旧值
//
// **空值绝不覆盖已有值**；expiresAt 的特殊规则（它必须与**本次实际生效的那批凭据**自洽，
// 不能沿用导入侧的 +50 年远期哨兵）—— 四级优先，与 mergeOfficeAceDoc 里的实现逐条对应：
//   - ① 本次生效的 extra 里有临时凭据（access_key 非空）**且有** cred_expires
//     ⇒ 取 cred_expires（唯一与手里这批 AK/SK 真实匹配的值，约 2h）；
//   - ② 有临时凭据但**缺** cred_expires ⇒ 取一个 1 小时的保守值，让它尽快续期去补出真值
//     （**不得**落哨兵：那等于宣称一个约 2h 就过期的凭据永不过期）；
//   - ③ 无临时凭据但有 refresh_token ⇒ 取 min(extra.refresh_expire, 旧的非哨兵 expiresAt)，
//     两者都是真值口径时取更早的（宁早刷新，不把真值往后推）；
//   - ④ 都没有（纯网关 Basic 导入）⇒ 采用导入侧的远期值（网关凭据确实不过期）。
//
// 目标文件存在但读不出/解析不了时返回错误**中止导入**（绝不静默覆盖）：宁可让用户看见失败，
// 也不能把登录成果悄悄清掉。
func (a *App) writeOfficeAceAuth(uid string, imported authDoc) (string, officeAceMergeInfo, error) {
	info := officeAceMergeInfo{}
	path := a.authFilePath("officeace", uid)
	if fileExists(path) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", info, fmt.Errorf("已有 OfficeAce 凭据不可读（%s），为避免覆盖登录成果已中止导入：%w", filepath.Base(path), err)
		}
		// 0 字节/纯空白文件没有任何凭据可保留（例如上次写盘被中断留下的空壳），
		// 按「目标不存在」处理 —— 否则用户会被一个空文件永久挡住导入。
		if len(strings.TrimSpace(string(raw))) > 0 {
			prev, perr := parseAuthDoc(raw)
			if perr != nil {
				return "", info, fmt.Errorf("已有 OfficeAce 凭据无法解析（%s），为避免覆盖登录成果已中止导入（确认无需保留后可手动删除该文件再导入）：%w", filepath.Base(path), perr)
			}
			imported, info = mergeOfficeAceDoc(prev, imported)
			info.Merged = true
		}
	}
	info.HasRefreshToken = strings.TrimSpace(imported.Auth.RefreshToken) != ""
	file, err := a.writeAuthFile("officeace", uid, imported)
	if err != nil {
		return "", info, err
	}
	return file, info, nil
}

// parseAuthDoc 解析磁盘上的 auth 文件（嵌套形 {"auth":{…},"account":{…}} 为准，兼容扁平形）。
//
// 刻意不复用 auth.Parse：它对 accessToken 为空**直接报错**，而被导入清空过的文件正是这种形态 ——
// 用它会让人既丢了凭据又无法再导入（必须先手动删文件）。这里只有「JSON 本身不可解析」才报错，
// 由调用方据此中止导入（不静默覆盖）。
func parseAuthDoc(raw []byte) (authDoc, error) {
	var doc authDoc
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return doc, err
	}
	if _, nested := probe["auth"]; nested {
		if err := json.Unmarshal(raw, &doc); err != nil {
			return doc, err
		}
		return doc, nil
	}
	// 扁平形（面板手建/旧版）：字段与 auth 段同级，account 字段同理。
	var flat struct {
		authSection
		accountSection
	}
	if err := json.Unmarshal(raw, &flat); err != nil {
		return doc, err
	}
	doc.Auth, doc.Account = flat.authSection, flat.accountSection
	return doc, nil
}

// mergeOfficeAceDoc 按字段合并「本工具已有凭据」与「从客户端导入的凭据」（规则见 writeOfficeAceAuth）。
func mergeOfficeAceDoc(prev, imported authDoc) (authDoc, officeAceMergeInfo) {
	info := officeAceMergeInfo{}
	out := imported
	p, im := prev.Auth, imported.Auth

	// ---- auth 段：逐字段「导入非空则采用导入值，否则保留旧值」 ----
	out.Auth.AccessToken = pickNonEmpty(im.AccessToken, p.AccessToken) // 网关 Basic：导入为准
	if strings.TrimSpace(im.RefreshToken) == "" && strings.TrimSpace(p.RefreshToken) != "" {
		info.KeptRefreshToken = true
	}
	out.Auth.RefreshToken = pickNonEmpty(im.RefreshToken, p.RefreshToken)
	out.Auth.Domain = pickNonEmpty(im.Domain, p.Domain)
	out.Auth.ApiHost = pickNonEmpty(im.ApiHost, p.ApiHost)
	out.Auth.MachineID = pickNonEmpty(im.MachineID, p.MachineID)
	out.Auth.DeviceID = pickNonEmpty(im.DeviceID, p.DeviceID)
	out.Auth.MachineToken = pickNonEmpty(im.MachineToken, p.MachineToken)
	out.Auth.MachineType = pickNonEmpty(im.MachineType, p.MachineType)
	out.Auth.SigningSecret = pickNonEmpty(im.SigningSecret, p.SigningSecret)
	out.Auth.ConsoleCookie = pickNonEmpty(im.ConsoleCookie, p.ConsoleCookie)
	out.Auth.BaizhiCookie = pickNonEmpty(im.BaizhiCookie, p.BaizhiCookie)

	// ---- extra 段：导入非空覆盖，空值保留旧值 ----
	out.Auth.Extra = mergeOfficeAceExtra(p.Extra, im.Extra)
	for _, k := range officeAceOAuthCredKeys {
		if strings.TrimSpace(p.Extra[k]) != "" && strings.TrimSpace(im.Extra[k]) == "" {
			info.KeptCredential = true
			break
		}
	}
	// source 是**取证标记**：本次保留了 OAuth 登录产物时不得把它改写成 "disk"，
	// 否则事后诊断会把「登录得来的凭据」误读成「从客户端导入的凭据」。
	if info.KeptCredential && strings.TrimSpace(p.Extra["source"]) == "oauth-login" {
		out.Auth.Extra["source"] = p.Extra["source"]
	}

	// ---- account 段 ----
	out.Account.UID = pickNonEmpty(imported.Account.UID, prev.Account.UID)
	out.Account.EnterpriseID = pickNonEmpty(imported.Account.EnterpriseID, prev.Account.EnterpriseID)
	out.Account.Nickname = pickNonEmpty(imported.Account.Nickname, prev.Account.Nickname)

	// ---- expiresAt：与**本次实际生效的那批凭据**对齐（2026-10-17 独立复核修订 + 用户实测补强）----
	//
	// 只要合并结果里带着临时凭据（无论它来自导入值还是被保留的旧值），expiresAt 就必须取
	// 该凭据自己的 `extra.cred_expires`（约 2h），**不能**沿用导入侧的 +50 年哨兵 —— 否则
	// 落盘值会与 cred_expires 自相矛盾，而通用自动续期路径（`auth.NeedsRefresh()` 只看
	// `expiresAt`）对 officeace 将永不触发（积分/签到路径因为 refresh.go 的 `ensureCredential`
	// 另行按 cred_expires 判定才没立刻坏，等于把「2 小时后失效」悄悄推到 refresh_token 到期那天）。
	// 旧写法把这条对齐放在 `KeptCredential` 分支里，于是修复「导入能拿到凭据」之后的主路径
	// （导入自带 access_key/cred_expires ⇒ KeptCredential=false）反而漏配 —— 正是复核指出的缺陷。
	//
	// 三级优先级（2026-10-17 用户实测补强；2026-10-18 独立复核再补两处）：
	//   ① 有临时凭据 + 有 cred_expires ⇒ 取 cred_expires（唯一与「手里这批 AK/SK」真实匹配的值）；
	//   ② 有临时凭据但**缺 cred_expires** ⇒ **不得落哨兵**：手里握着一个真实但寿命未知的
	//      AK/SK，写 +50 年等于宣称「它永不过期」——事实相反。取一个较短的到期（见
	//      `officeAceCredWithoutExpiryGrace`）让它尽快走一次续期去补出真实到期时刻；
	//      （复核指出的漏洞：闸门只要求 access_key 非空而 cred_expires 可以缺失，此时
	//       旧写法会落哨兵，面板还会宣称「到期时间已按保留凭据自己的到期对齐」——假话。
	//       INV「有 access_key ⇒ 非哨兵」在真实写路径上曾可复现，本分支就是补它。）
	//   ③ 无临时凭据但保留了 refresh_token ⇒ 取 `min(refresh_expire, 旧真值 expiresAt)`：
	//      两者都是真值口径时取**更早**的，避免把真值往后推成「欠刷新窗口」
	//      （复核 MED-3：旧写法一律取 refresh_expire，会把一个真实 2h 的 expiresAt 推到 30 天后）。
	//   ④ 都没有（纯网关 Basic 导入）⇒ 才写远期哨兵，且**沿用导入值本身**（不再跨秒重算）。
	hasCred := strings.TrimSpace(out.Auth.Extra["access_key"]) != ""
	ce := parseUnixSec(out.Auth.Extra["cred_expires"])
	re := parseUnixSec(out.Auth.Extra["refresh_expire"])
	prevExp := prev.Auth.ExpiresAt
	if hasCred && ce > 0 {
		out.Auth.ExpiresAt = ce
	} else if hasCred {
		out.Auth.ExpiresAt = time.Now().Unix() + officeAceCredWithoutExpiryGrace
	} else if strings.TrimSpace(out.Auth.RefreshToken) != "" && (re > 0 || (prevExp > 0 && !isFarFutureExpiry(prevExp))) {
		out.Auth.ExpiresAt = minPositive(re, prevExp)
	} else {
		// 纯网关 Basic 导入（无临时凭据、也无 refresh_expire 可对齐）：哨兵是本场景的正确取值
		// ——网关凭据确实不过期，写它可避免误触「必然失败的刷新」。
		// 用导入值本身（而非重新算一次 now+50 年）以保证**确定性**：两者正常情况下同值，
		// 但跨秒重算会产生 1 秒差异，让 `KeptExpiresAt` 变成偶发 true（测试会随机红）。
		if imported.Auth.ExpiresAt > 0 {
			out.Auth.ExpiresAt = imported.Auth.ExpiresAt
		} else {
			out.Auth.ExpiresAt = officeAceImportedSentinel()
		}
	}
	info.KeptExpiresAt = out.Auth.ExpiresAt != imported.Auth.ExpiresAt
	return out, info
}

// mergeOfficeAceExtra 合并 extra 袋：导入非空值覆盖旧值，导入空值/缺失一律保留旧值。
func mergeOfficeAceExtra(prev, imported map[string]string) map[string]string {
	if len(prev) == 0 && len(imported) == 0 {
		return nil
	}
	out := make(map[string]string, len(prev)+len(imported))
	for k, v := range prev {
		if strings.TrimSpace(v) != "" {
			out[k] = v
		}
	}
	for k, v := range imported {
		if strings.TrimSpace(v) != "" {
			out[k] = v
		}
	}
	return out
}

// pickNonEmpty 返回第一个非空白值（导入值优先，导入为空则回落旧值）。
func pickNonEmpty(primary, fallback string) string {
	if strings.TrimSpace(primary) != "" {
		return primary
	}
	return fallback
}

// parseUnixSec 解析 Unix 秒字符串（空/非法返回 0）。
func parseUnixSec(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// ---------------------------------------------------------------------------
// 远期哨兵（auth.expiresAt）——「这条凭据不会自己过期」的显式标记
// ---------------------------------------------------------------------------

// officeAceFarFutureExpiry 是导入型渠道给 `auth.expiresAt` 写的**远期哨兵值**：
// 表示「该凭据本身不过期，不要按 expiresAt 触发刷新」。
//
// 取值（`time.Now().AddDate(50, 0, 0).Unix()`）刻意保留为动态表达式（见调用处），
// 本常量只用于**识别**，不用于写入。
//
// 为什么需要识别（2026-10-17 实机取证，issue 现场）：
// OfficeAce 有**两层**凭据——网关 Basic（不过期）与临时 AK/SK（约 2h）。导入只拿得到
// 网关 Basic 时写哨兵是正确的；但同一账号随后若被 OAuth 登录或合并补上临时凭据，
// **哨兵值必须让位给 `extra.cred_expires`**，否则 `auth.NeedsRefresh()` 恒为假，
// 通用续期路径对该账号永不触发（实测：某账号 `expiresAt` 停在 +50 年、而临时凭据
// 每 2 小时被 officeace 自己的 `ensureCredential` 轮换，两者长期自相矛盾）。
//
// 判定口径偏保守：只要**明显超出正常生命周期**（> 10 年）就视为哨兵——这样连**历史
// 遗留**的哨兵值（含未来可能换个表达式的写法）也能被识别并纠正，而不必精确等值匹配。
const officeAceSentinelMinSeconds = 10 * 365 * 24 * 3600

// isFarFutureExpiry 判断 expiresAt 是否属于「远期哨兵」（>10 年）。0/负值不算。
func isFarFutureExpiry(exp int64) bool {
	if exp <= 0 {
		return false
	}
	return exp-time.Now().Unix() > officeAceSentinelMinSeconds
}

// officeAceCredWithoutExpiryGrace：手里有临时凭据（access_key 非空）却**没有**
// `extra.cred_expires` 时，给 expiresAt 取一个较短的保守值（1 小时）。
//
// 为什么不落哨兵：哨兵的语义是「这条凭据不会自己过期」，而网关 Basic 才是那种凭据；
// 临时 AK/SK 约 2 小时必过期。写成哨兵会让 `NeedsRefresh()` 永不触发，等于把一个
// 「寿命未知的真实凭据」长期当成永不过期（独立复核用 512 组合穷举 + 真实写路径样本
// 复现过这一组合）。取 1 小时是为了**尽快触发一次续期**，由 refresh 路径把真实到期
// （`cred_expires`）补出来——那之后本函数不再参与。
const officeAceCredWithoutExpiryGrace = 3600

// minPositive 返回两个值中**更早**的那个正数（都非正时返回 0）。
// 用于「取更保守的到期」：宁可早刷新，也不要把真实到期往后推（欠刷新窗口）。
func minPositive(a, b int64) int64 {
	switch {
	case a > 0 && b > 0:
		if a < b {
			return a
		}
		return b
	case a > 0:
		return a
	default:
		return b
	}
}

// officeAceImportedSentinel 返回「本次导入」给 auth.expiresAt 写的远期哨兵值。
// 与调用处同源，避免识别与写入两处各写一份表达式而漂移。
func officeAceImportedSentinel() int64 { return time.Now().AddDate(50, 0, 0).Unix() }

// authFilePath 返回渠道凭据文件路径（writeAuthFile 与「写前读旧值合并」共用同一命名规则，
// 避免两处各拼一次路径而漂移）。
func (a *App) authFilePath(prefix, uid string) string {
	return filepath.Join(a.cfg.AuthDir, prefix+"-"+sanitizeUID(uid)+".json")
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
	file := a.authFilePath(prefix, uid)
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
	// uid 提取统一走 raccoon.JWTUID（旧版取 name、新版 OIDC 回落 sub）——
	// 2026-10-15 上游迁站后 JWT 无 name claim，自行写 jwtClaim(access,"name") 会恒空。
	uid := raccoon.JWTUID(access)
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
	// 首次签到（与网页版登录路径同源）：setting_info 触发当日每日积分落账。
	a.safeGo(func() { a.firstCheckinRaccoon(uid) })
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
