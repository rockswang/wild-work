package app

// chatgpt_import.go 落「从 ww3 客户端数据目录导入存量凭据」这一条入口（票 08）：
//
//	面板 → POST /api/account/import_local {channel:"chatgpt", dir:"…"}
//	     → chatgpt.ScanAuthFiles（找文件）→ chatgpt.ParseAuth（票 01 的**同一份**解析器）
//	     → chatgpt.Client.Me（零配额校验）→ 落盘 auths/chatgpt-<uid>.json → 重载池 + 重拉模型
//
// 三条纪律：
//  1. **解析器不复用即漂移**：文件里出现新字段时由 auth.Parse 宽容忽略，不在本文件另写映射；
//  2. **不静默跳过**：路径层错误整单报错（「路径不对」），单文件问题逐条回传（「这份凭据坏了/过期了」）；
//  3. 零明文：回包与日志只有**文件名 + 短 uid + 上游原因（掩码后）**，绝不出现 token / cookie。

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"wild-work/internal/auth"
	"wild-work/internal/chatgpt"
	"wild-work/internal/provider"
)

const (
	chatgptImportImported  = "imported"
	chatgptImportDuplicate = "duplicate"
	chatgptImportInvalid   = "invalid"
	chatgptImportFailed    = "failed"
)

// importChatGPT 是 chatgpt 的多文件导入：一个数据目录里可能有好几个账号。
//
// 返回错误只表示「整单无法开始」（路径不对 / 渠道未装配 / 凭据目录不可写）；
// 单个文件坏掉或凭据过期属于逐条结果，不影响其它文件的导入。
func (a *App) importChatGPT(dir string) (*ImportLocalResult, error) {
	files, err := chatgpt.ScanAuthFiles(dir)
	if err != nil {
		return nil, err
	}
	cf, err := a.chatgptClient()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(a.cfg.AuthDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建凭据目录失败：%w", err)
	}

	res := &ImportLocalResult{
		Channel:  string(provider.ChatGPT),
		Accounts: make([]ImportLocalAccount, 0, len(files)),
	}
	for _, file := range files {
		item := a.importChatGPTFile(cf, file)
		switch item.Status {
		case chatgptImportImported:
			res.Imported++
		case chatgptImportDuplicate:
			res.Skipped++
		default:
			res.Failed++
		}
		res.Accounts = append(res.Accounts, item)
		// 零明文（不变量 2）：只记文件名与短 uid —— 路径与上游原因一律不入日志。
		log.Printf("chatgpt 导入：file=%s status=%s uid=%s", item.File, item.Status, item.UID)
	}

	res.Note = chatgptImportNote(res)
	if res.Imported > 0 {
		// 重载一次池即可让全部新账号立即可见（逐一 reload 会做 N 次全量扫描）。
		a.reloadAccounts()
		a.afterAccountAdded(provider.ChatGPT)
	}
	return res, nil
}

// importChatGPTFile 处理单个文件：读 → 解析 → 零费用校验 → 判重 → 落盘。
func (a *App) importChatGPTFile(cf *chatgpt.Client, file string) ImportLocalAccount {
	item := ImportLocalAccount{File: filepath.Base(file)}

	raw, err := os.ReadFile(file)
	if err != nil {
		item.Status = chatgptImportInvalid
		item.Reason = fmt.Sprintf("读取文件失败：%v", err)
		return item
	}
	// 解析复用票 01 的 ParseAuth：新增字段由 auth.Parse 宽容忽略，缺必需字段则明确报错。
	parsed, err := chatgpt.ParseAuth(raw)
	if err != nil {
		item.Status = chatgptImportInvalid
		item.Reason = "不是可用的 ChatGPT 凭据：" + redactChatGPTSecrets(err.Error(), "", nil)
		return item
	}

	deviceID := strings.TrimSpace(parsed.DeviceID)
	if deviceID == "" {
		deviceID = chatgpt.NewDeviceID()
	}
	probe := &auth.Auth{
		Kind:        "chatgpt",
		AccessToken: parsed.AccessToken,
		Cookies:     parsed.Cookies,
		DeviceID:    deviceID,
	}
	// 每个凭据一次零费用校验（/backend-api/me 不计额度）：失败即不落盘，
	// 且原因要能区分「未登录/过期」与「没配代理」（见 chatgptAuthError）。
	me, err := cf.Me(probe)
	if err != nil {
		item.Status = chatgptImportFailed
		item.Reason = redactChatGPTSecrets(chatgptAuthError(err).Error(), parsed.AccessToken, parsed.Cookies)
		return item
	}

	uid := chatgptUID(me, parsed.AccessToken)
	if uid == "" {
		item.Status = chatgptImportFailed
		item.Reason = "上游未返回账号 ID，无法确定这是哪个账号（凭据可能已失效）"
		return item
	}
	item.UID = chatgptUIDTail(uid)

	// 已存在的账号**不覆盖**（票面要求：给告警而不是覆盖）。
	if prev := a.chatgptPrevNickname(uid); prev.seen {
		item.Status = chatgptImportDuplicate
		item.Reason = "已存在同 uid 账号，已跳过（不覆盖现有凭据）"
		return item
	}

	aa := a.newChatGPTAuth(uid, parsed.AccessToken, parsed.Cookies, deviceID, me, chatgptPrev{})
	// 导入到的新库里，原文件名没有保留价值（也可能撞名），统一按 uid 命名。
	aa.FilePath = filepath.Join(a.cfg.AuthDir, "chatgpt-"+uid+".json")
	if err := aa.SaveAtomic(); err != nil {
		item.Status = chatgptImportFailed
		item.Reason = fmt.Sprintf("保存凭据失败：%v", err)
		return item
	}
	item.Status = chatgptImportImported
	return item
}

// chatgptImportNote 给一句可读汇总（成功多少、跳过多少、失败多少）。
func chatgptImportNote(res *ImportLocalResult) string {
	if res.Imported > 0 && res.Skipped == 0 && res.Failed == 0 {
		return ""
	}
	parts := []string{fmt.Sprintf("成功导入 %d 个", res.Imported)}
	if res.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("已存在跳过 %d 个", res.Skipped))
	}
	if res.Failed > 0 {
		parts = append(parts, fmt.Sprintf("失败 %d 个", res.Failed))
	}
	return strings.Join(parts, "，") + "（逐条原因见下）"
}

// redactChatGPTSecrets 是最后一道零明文护栏：上游回显（或未来某天报错文案里带上请求内容）
// 若把 token / cookie 值带出来，也会在回包前被抹掉。
//
// 只替换长度 ≥ 8 的值：短值（如 `1`）替换会误伤正常文案。
func redactChatGPTSecrets(s, token string, cookies map[string]string) string {
	if len(token) >= 8 {
		s = strings.ReplaceAll(s, token, "***")
	}
	for _, v := range cookies {
		if len(v) >= 8 {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return s
}
