package app

// chatgpt_import_test.go 锁死票 08 的「从 ww3 客户端数据目录导入存量凭据」链路：
//
//	面板（填目录）→ POST /api/account/import_local {channel:"chatgpt", dir:"…"}
//	  → chatgpt.ScanAuthFiles（路径层错误整单报错）
//	  → chatgpt.ParseAuth（票 01 的同一份解析器，不另写）
//	  → chatgpt.Client.Me（每个凭据一次零费用校验）
//	  → 不覆盖已存在账号（逐条 duplicate 告警）→ 落盘 → 重载池
//
// 五类用例（票面要求）：正常导入 / 目录缺失 / 坏文件 / 重复账号 / 校验失败。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/chatgpt"
	"wild-work/internal/provider"
)

// fakeChatGPTImport 是导入链路的假上游：等价于「按 token 认账号」的 /backend-api/me。
//
// 约定：access token 形如 `tok-<uid>`（uid 很短，便于面板短 uid 断言），
// /me 回该 uid；`failTokens` 里的 token 按 status 回错（默认 401 + 把 token 回显到 body，
// 用来证明回包做了掩码）。
type fakeChatGPTImport struct {
	mu         sync.Mutex
	meTokens   []string
	failTokens map[string]int
}

func (f *fakeChatGPTImport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != chatgpt.PathMe {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"Model not found"}`))
		return
	}
	authz := r.Header.Get("Authorization")
	token := strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
	f.mu.Lock()
	f.meTokens = append(f.meTokens, token)
	status := f.failTokens[token]
	f.mu.Unlock()
	if status != 0 {
		if status == http.StatusUnauthorized {
			w.WriteHeader(status)
			// 故意回显 token：验证回包侧做掩码（零明文不能只靠上游不作恶）
			_, _ = fmt.Fprintf(w, `{"detail":"not_login","echo":%q}`, token)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"detail":"upstream error"}`))
		return
	}
	uid := strings.TrimPrefix(token, "tok-")
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":%q,"name":"账号 %s","email":"%s@example.com"}`, uid, uid, uid)
}

func (f *fakeChatGPTImport) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.meTokens))
	copy(out, f.meTokens)
	return out
}

// writeWW3Cred 按 ww3 客户端 `auths/<name>.json` 的真实形态写一份凭据。
func writeWW3Cred(t *testing.T, dataDir, name, uid, token string, cookies map[string]string) string {
	t.Helper()
	dir := filepath.Join(dataDir, "auths")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir auths: %v", err)
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken": token,
			"expiresAt":   4102444800,
			// ww3 的 auth 段也写一份 userId（account.uid 缺失时的回退）
			"userId":   uid,
			"deviceId": "dev-" + uid,
			"cookies":  cookies,
			// 未来新增字段：解析器必须宽容忽略，不得因此判整份凭据无效
			"someFutureField": map[string]any{"nested": true},
		},
		"account": map[string]any{"uid": uid, "nickname": "旧名字 " + uid},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatalf("marshal cred: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write cred: %v", err)
	}
	return path
}

func findAccount(res *ImportLocalResult, file string) (ImportLocalAccount, bool) {
	for _, it := range res.Accounts {
		if it.File == file {
			return it, true
		}
	}
	return ImportLocalAccount{}, false
}

// ① 正常导入：两个文件都导入、都进池，回包零明文。
func TestImportChatGPTCredentialsSuccess(t *testing.T) {
	f := &fakeChatGPTImport{}
	a, cgPool := newChatGPTAppWithHandler(t, f)
	dataDir := t.TempDir()
	writeWW3Cred(t, dataDir, "one.json", "ua1234", "tok-ua1234",
		map[string]string{"__Secure-next-auth.session-token": "cookie-a-123456"})
	writeWW3Cred(t, dataDir, "two.json", "ub5678", "tok-ub5678",
		map[string]string{"__Secure-next-auth.session-token": "cookie-b-123456"})

	res, err := a.ImportLocalCredentials("chatgpt", dataDir) // 走白名单分发，覆盖「本渠道已登记」
	if err != nil {
		t.Fatalf("ImportLocalCredentials: %v", err)
	}
	if res.Channel != "chatgpt" {
		t.Errorf("Channel = %q，期望 chatgpt", res.Channel)
	}
	if res.Imported != 2 || res.Skipped != 0 || res.Failed != 0 {
		t.Fatalf("imported/skipped/failed = %d/%d/%d，期望 2/0/0（items=%+v）",
			res.Imported, res.Skipped, res.Failed, res.Accounts)
	}
	if res.Note != "" {
		t.Errorf("全部成功时不应有汇总告警，得到 %q", res.Note)
	}
	if len(res.Accounts) != 2 {
		t.Fatalf("逐文件结果 %d 条，期望 2（每个文件都必须有一条，不静默跳过）", len(res.Accounts))
	}
	// 文件名升序（ScanAuthFiles 的稳定输出），且只出现文件名与短 uid
	if res.Accounts[0].File != "one.json" || res.Accounts[1].File != "two.json" {
		t.Errorf("逐文件结果顺序 = %q,%q，期望 one.json,two.json",
			res.Accounts[0].File, res.Accounts[1].File)
	}
	for _, it := range res.Accounts {
		if it.Status != "imported" {
			t.Errorf("%s status = %q，期望 imported（reason=%q）", it.File, it.Status, it.Reason)
		}
		if it.UID == "" || len(it.UID) > 6 {
			t.Errorf("%s uid = %q，期望短 uid（尾 6 位）", it.File, it.UID)
		}
	}

	// 每个凭据恰好一次零费用校验
	if calls := f.calls(); len(calls) != 2 {
		t.Errorf("零费用校验 %d 次，期望每个凭据各 1 次（共 2 次）：%v", len(calls), calls)
	}

	// 账号立即可见（无需重启）
	for _, uid := range []string{"ua1234", "ub5678"} {
		if cgPool.AuthByUID(uid) == nil {
			t.Errorf("reloadAccounts 没把 %s 放进池里（导入后账号必须立即可见）", uid)
		}
	}

	// 落盘形态与权限；原文件里的未来字段不得挡住导入
	list, err := auth.LoadChatGPTDir(a.cfg.AuthDir)
	if err != nil || len(list) != 2 {
		t.Fatalf("LoadChatGPTDir = %d 个（err=%v），期望 2", len(list), err)
	}
	for _, got := range list {
		fp := filepath.Join(a.cfg.AuthDir, "chatgpt-"+got.UID+".json")
		fi, statErr := os.Stat(fp)
		if statErr != nil {
			t.Fatalf("凭据未落盘到 %s：%v", fp, statErr)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s 权限 = %v，必须仅属主可读（0600）", fp, fi.Mode().Perm())
		}
	}

	// 零明文：回包（被前端展示的整份 JSON）不含 token / cookie 值
	blob, _ := json.Marshal(res)
	for _, secret := range []string{"tok-ua1234", "tok-ub5678", "cookie-a-123456", "cookie-b-123456"} {
		if strings.Contains(string(blob), secret) {
			t.Errorf("导入回包出现凭据明文 %q（不变量 2）", secret)
		}
	}
}

// ② 目录缺失：整单失败必须是「路径不对」，且一个上游请求都不发。
func TestImportChatGPTDirMissing(t *testing.T) {
	f := &fakeChatGPTImport{}
	a, _ := newChatGPTAppWithHandler(t, f)
	missing := filepath.Join(t.TempDir(), "not-there")

	_, err := a.ImportLocalCredentials("chatgpt", missing)
	if err == nil {
		t.Fatal("目录不存在时必须报错（否则用户只看到「导入 0 个」）")
	}
	if !strings.Contains(err.Error(), "路径不存在") || !strings.Contains(err.Error(), missing) {
		t.Errorf("错误没点明是路径不对：%v", err)
	}
	if calls := f.calls(); len(calls) != 0 {
		t.Errorf("路径不对时不得打上游，实际发了 %d 次：%v", len(calls), calls)
	}
}

// ③ 坏文件：逐条回传原因，不静默跳过，也不影响同目录里的好文件。
func TestImportChatGPTBadFileReported(t *testing.T) {
	f := &fakeChatGPTImport{}
	a, cgPool := newChatGPTAppWithHandler(t, f)
	dataDir := t.TempDir()
	writeWW3Cred(t, dataDir, "good.json", "ua1234", "tok-ua1234",
		map[string]string{"__Secure-next-auth.session-token": "cookie-a-123456"})

	// 截断的 JSON（读得动、解析不了）
	broken := filepath.Join(dataDir, "auths", "broken.json")
	if err := os.WriteFile(broken, []byte(`{"auth":{"accessToken":"tok-x"`), 0o600); err != nil {
		t.Fatalf("write broken: %v", err)
	}
	// 形状合法但整组 cookies 为空：必须判无效（否则是「看着加上了、每次 403」的账号）
	writeWW3Cred(t, dataDir, "nocookie.json", "uc9999", "tok-uc9999", map[string]string{})

	res, err := a.ImportLocalCredentials("chatgpt", dataDir)
	if err != nil {
		t.Fatalf("坏文件不得让整单失败：%v", err)
	}
	if res.Imported != 1 || res.Failed != 2 {
		t.Fatalf("imported/failed = %d/%d，期望 1/2（items=%+v）", res.Imported, res.Failed, res.Accounts)
	}
	if len(res.Accounts) != 3 {
		t.Fatalf("逐文件结果 %d 条，期望 3（坏文件也要出现，不静默跳过）", len(res.Accounts))
	}
	for _, name := range []string{"broken.json", "nocookie.json"} {
		it, ok := findAccount(res, name)
		if !ok {
			t.Fatalf("坏文件 %s 没有出现在结果里（静默跳过了）", name)
		}
		if it.Status != "invalid" {
			t.Errorf("%s status = %q，期望 invalid", name, it.Status)
		}
		if strings.TrimSpace(it.Reason) == "" {
			t.Errorf("%s 没有回传原因（用户只能反复试同一份文件）", name)
		}
	}
	if _, ok := findAccount(res, "good.json"); !ok {
		t.Error("好文件被坏文件带崩了（逐文件隔离失败）")
	}
	if cgPool.AuthByUID("ua1234") == nil {
		t.Error("好文件没进池")
	}
	if calls := f.calls(); len(calls) != 1 {
		t.Errorf("坏文件不得打上游；实际 /me 调用 %d 次，期望 1：%v", len(calls), calls)
	}
	if !strings.Contains(res.Note, "失败 2 个") {
		t.Errorf("汇总 note = %q，期望点明失败数", res.Note)
	}
}

// ④ 重复账号：校验通过但已存在 ⇒ 跳过并告警，**不覆盖**原凭据文件。
func TestImportChatGPTDuplicateNotOverwritten(t *testing.T) {
	f := &fakeChatGPTImport{}
	a, _ := newChatGPTAppWithHandler(t, f)

	// 先放一份「已有账号」的凭据（token 与待导入文件不同，覆盖与否一眼可辨）
	oldPath := filepath.Join(a.cfg.AuthDir, "chatgpt-ua1234.json")
	oldRaw := []byte(`{"auth":{"accessToken":"tok-old-9999","cookies":{"__Secure-next-auth.session-token":"cookie-old-1234"}},` +
		`"account":{"uid":"ua1234","nickname":"旧名字 ua1234"}}`)
	if err := os.WriteFile(oldPath, oldRaw, 0o600); err != nil {
		t.Fatalf("write existing auth: %v", err)
	}

	dataDir := t.TempDir()
	writeWW3Cred(t, dataDir, "again.json", "ua1234", "tok-ua1234",
		map[string]string{"__Secure-next-auth.session-token": "cookie-new-5678"})

	res, err := a.ImportLocalCredentials("chatgpt", dataDir)
	if err != nil {
		t.Fatalf("ImportLocalCredentials: %v", err)
	}
	if res.Imported != 0 || res.Skipped != 1 {
		t.Fatalf("imported/skipped = %d/%d，期望 0/1（items=%+v）", res.Imported, res.Skipped, res.Accounts)
	}
	it, ok := findAccount(res, "again.json")
	if !ok || it.Status != "duplicate" {
		t.Fatalf("again.json 结果 = %+v（ok=%v），期望 duplicate", it, ok)
	}
	if !strings.Contains(it.Reason, "不覆盖") {
		t.Errorf("重复账号的告警 = %q，应明确说「不覆盖」", it.Reason)
	}
	// 即使判重也必须走过一次零费用校验（票面：每个凭据走一次校验）
	if calls := f.calls(); len(calls) != 1 {
		t.Errorf("判重前应完成校验；/me 调用 %d 次，期望 1", len(calls))
	}
	// 不覆盖：原文件逐字节不变
	nowRaw, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatalf("读回已有凭据失败：%v", err)
	}
	if string(nowRaw) != string(oldRaw) {
		t.Error("导入覆盖了已存在的账号凭据（票面要求给告警而不是覆盖）")
	}
}

// ⑤ 校验失败（401 = 凭据过期/未登录）：不落盘，原因要说清是凭据问题，且回包掩码。
func TestImportChatGPTValidationFailure(t *testing.T) {
	token := "tok-expired-123456"
	f := &fakeChatGPTImport{failTokens: map[string]int{token: http.StatusUnauthorized}}
	a, cgPool := newChatGPTAppWithHandler(t, f)
	dataDir := t.TempDir()
	writeWW3Cred(t, dataDir, "stale.json", "ue7777", token,
		map[string]string{"__Secure-next-auth.session-token": "cookie-stale-123456"})

	res, err := a.ImportLocalCredentials("chatgpt", dataDir)
	if err != nil {
		t.Fatalf("校验失败属逐条结果，不该让整单报错：%v", err)
	}
	if res.Failed != 1 || res.Imported != 0 {
		t.Fatalf("failed/imported = %d/%d，期望 1/0（items=%+v）", res.Failed, res.Imported, res.Accounts)
	}
	it, ok := findAccount(res, "stale.json")
	if !ok || it.Status != "failed" {
		t.Fatalf("stale.json 结果 = %+v（ok=%v），期望 failed", it, ok)
	}
	// 判据必须是「凭据过期/未登录」而不是笼统的「导入失败」（票面锚点：用户要能分辨路径问题 vs 凭据问题）
	if !strings.Contains(it.Reason, "未登录") {
		t.Errorf("校验失败原因 = %q，应点明凭据未登录/过期", it.Reason)
	}
	// 零明文：上游把 token 回显进了 401 body，回包也不得出现
	if strings.Contains(it.Reason, token) {
		t.Errorf("失败原因里带出了 token 明文（不变量 2）：%q", it.Reason)
	}
	// 失败不落盘
	list, err := auth.LoadChatGPTDir(a.cfg.AuthDir)
	if err != nil {
		t.Fatalf("LoadChatGPTDir: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("校验失败的凭据被落盘了（%d 个）：会产生一个每次 403 的假账号", len(list))
	}
	if got := len(cgPool.List()); got != 0 {
		t.Errorf("校验失败却进了池：%d 个账号", got)
	}
}

// 白名单登记：chatgpt 必须由 ImportLocalCredentials 接住（而不是落到「该渠道不支持本地导入」）。
func TestImportLocalWhitelistsChatGPT(t *testing.T) {
	// 未注册 runtime 也要走到「渠道已登记」的分支：直接看分发结果不是「不支持导入」。
	a := &App{} // 只测分支登记，不碰 runtime / 文件系统
	_, err := a.ImportLocalCredentials(string(provider.ChatGPT), "")
	if err == nil {
		t.Fatal("空目录参数应报错（证明进的是 chatgpt 导入分支而不是静默成功）")
	}
	if strings.Contains(err.Error(), "不支持本地导入") {
		t.Errorf("chatgpt 未登记进导入白名单：%v", err)
	}
}
