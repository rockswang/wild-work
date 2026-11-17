package login_codearts

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wild-work/internal/auth"
)

// fakeRT 造一枚**结构真实**的 refresh_token（JWS 形态，payload 带 user_profile）。
// 只用于测试；不含任何真实凭据（AGENTS.md §0.2）。
func fakeRT(accountID, principalID, accountName string) string {
	profile := fmt.Sprintf(`{"account_id":%q,"account_name":%q,"principal_id":%q,"principal_urn":"iam::%s:user:%s"}`,
		accountID, accountName, principalID, accountID, accountName)
	payload := fmt.Sprintf(`{"exp":1796891843,"iat":1794877200,"family_id":"RT:2026111008:testfam","counter":7,"user_profile":%q}`,
		base64.RawURLEncoding.EncodeToString([]byte(profile)))
	return "eyJhbGciOiJSUzI1NiIsImtpZCI6ImNuLW5vcnRoLTQ6NiJ9." +
		base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".fakesig"
}

// loadUIDs 读目录里所有账号的 uid（排序后返回，便于断言）。
func loadUIDs(t *testing.T, dir string) []string {
	t.Helper()
	auths, err := auth.LoadCodeArtsDir(dir)
	if err != nil {
		t.Fatalf("LoadCodeArtsDir: %v", err)
	}
	var out []string
	for _, a := range auths {
		if a != nil {
			out = append(out, a.UID)
		}
	}
	return out
}

// TestSaveAuthTwoAccountsDoNotOverwrite 是本轮修复的**核心回归**（R159）：
//
// 旧实现（方案 B）在「拿不到身份字段」时会复用**第一条已有账号的 UID**，于是第二个华为账号
// 无论登的是谁，都会写进第一个账号的文件 —— 真机事故里表现为「登录成功、账号数还是 2、
// 但其中一个的余额从 3620 变成新号的 4500」，且老号凭据被彻底覆盖（refresh_token 是单次
// 轮转票据，覆盖后不可兑回）。
//
// 新实现：account_id 不同 ⇒ 各自成条，**绝不动别人的文件**。
func TestSaveAuthTwoAccountsDoNotOverwrite(t *testing.T) {
	dir := t.TempDir()

	fp1, err := SaveAuth(dir, Result{
		UID: "AK-FIRST", AccessToken: "AK-FIRST", AccessKeyID: "AK-FIRST", SecretAccessKey: "SK-FIRST",
		RefreshToken: fakeRT("acct-aaaa", "principal-a", "hw000000000000001"),
		AccountID:    "acct-aaaa", PrincipalID: "principal-a",
	})
	if err != nil {
		t.Fatalf("首次 SaveAuth: %v", err)
	}
	if got := filepath.Base(fp1); got != "codearts-acct-aaaa.json" {
		t.Fatalf("首次落盘文件名 = %s，期望 codearts-acct-aaaa.json（uid 取 account_id）", got)
	}

	// 第二个账号：不同 account_id。旧实现会写进 codearts-acct-aaaa.json（覆盖）。
	fp2, err := SaveAuth(dir, Result{
		UID: "AK-SECOND", AccessToken: "AK-SECOND", AccessKeyID: "AK-SECOND", SecretAccessKey: "SK-SECOND",
		RefreshToken: fakeRT("acct-bbbb", "principal-b", "hw000000000000002"),
		AccountID:    "acct-bbbb", PrincipalID: "principal-b",
	})
	if err != nil {
		t.Fatalf("二次 SaveAuth: %v", err)
	}
	if fp2 == fp1 {
		t.Fatalf("不同账号**不得**写进同一文件：fp1=%s fp2=%s", fp1, fp2)
	}
	if got := filepath.Base(fp2); got != "codearts-acct-bbbb.json" {
		t.Fatalf("二次落盘文件名 = %s，期望 codearts-acct-bbbb.json", got)
	}

	uids := loadUIDs(t, dir)
	if len(uids) != 2 {
		t.Fatalf("两个不同账号应各成一条，实际 %d 条：%v", len(uids), uids)
	}
	// 老号的凭据必须原封不动（这是「覆盖」判据的关键：文件在但内容被换掉同样是事故）。
	raw, err := os.ReadFile(fp1)
	if err != nil {
		t.Fatalf("读首个文件: %v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("Parse 首个文件: %v", err)
	}
	if a.AccessToken != "AK-FIRST" {
		t.Fatalf("首个文件的凭据被改写：got %q，期望 AK-FIRST", a.AccessToken)
	}
}

// TestSaveAuthSameAccountReloginOverwritesSelf 同一账号（account_id 相同）重登：
// 覆盖**它自己**那条 —— 既满足「重复登录不新增条目」，又不会牵连别的账号。
func TestSaveAuthSameAccountReloginOverwritesSelf(t *testing.T) {
	dir := t.TempDir()
	rt1 := fakeRT("acct-aaaa", "principal-a", "hw000000000000001")
	fp1, err := SaveAuth(dir, Result{
		UID: "AK-1", AccessToken: "AK-1", AccessKeyID: "AK-1", SecretAccessKey: "SK-1",
		RefreshToken: rt1, AccountID: "acct-aaaa", PrincipalID: "principal-a",
	})
	if err != nil {
		t.Fatalf("SaveAuth(1): %v", err)
	}
	// 另一个账号先插入，制造「同账号不是第一条」的局面（旧实现只看第一条，必错）。
	if _, err := SaveAuth(dir, Result{
		UID: "AK-X", AccessToken: "AK-X", AccessKeyID: "AK-X", SecretAccessKey: "SK-X",
		RefreshToken: fakeRT("acct-bbbb", "principal-b", "hw000000000000002"),
		AccountID:    "acct-bbbb", PrincipalID: "principal-b",
	}); err != nil {
		t.Fatalf("SaveAuth(X): %v", err)
	}

	fp2, err := SaveAuth(dir, Result{
		UID: "AK-2", AccessToken: "AK-2", AccessKeyID: "AK-2", SecretAccessKey: "SK-2",
		RefreshToken: fakeRT("acct-aaaa", "principal-a", "hw000000000000001"),
		AccountID:    "acct-aaaa", PrincipalID: "principal-a",
	})
	if err != nil {
		t.Fatalf("SaveAuth(2): %v", err)
	}
	if fp2 != fp1 {
		t.Fatalf("同账号重登必须覆盖自己那条：首次 %s，二次 %s", fp1, fp2)
	}
	raw, _ := os.ReadFile(fp1)
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.AccessToken != "AK-2" {
		t.Fatalf("重登应把凭据换成这次的：got %q", a.AccessToken)
	}
	if got := len(loadUIDs(t, dir)); got != 2 {
		t.Fatalf("目录里应仍是 2 个账号（覆盖不是新增），实际 %d", got)
	}
}

// TestSaveAuthReusesLegacyFileByAccountID 历史文件（uid 是旧的 HSTA… 形态、Extra 里没有
// account_id）在**同一账号**重登时必须被原地更新，不能另起一个新文件 —— 这样老用户升级后
// 不会看到重复条目（零迁移，不变量 5）。
func TestSaveAuthReusesLegacyFileByAccountID(t *testing.T) {
	dir := t.TempDir()
	// 造「老文件」：身份来自 UserID（老口径），refresh_token 里带 account_id，Extra 无该键。
	legacyRT := fakeRT("acct-legacy", "principal-l", "hw000000000000003")
	fpLegacy, err := SaveAuth(dir, Result{
		UID: "HSTALEGACY0001", UserID: "HSTALEGACY0001",
		AccessToken: "AK-L", AccessKeyID: "AK-L", SecretAccessKey: "SK-L",
		RefreshToken: legacyRT,
	})
	if err != nil {
		t.Fatalf("造老文件: %v", err)
	}
	if got := filepath.Base(fpLegacy); got != "codearts-HSTALEGACY0001.json" {
		t.Fatalf("老文件命名 = %s", got)
	}

	fp2, err := SaveAuth(dir, Result{
		UID: "AK-NEW", AccessToken: "AK-NEW", AccessKeyID: "AK-NEW", SecretAccessKey: "SK-NEW",
		RefreshToken: fakeRT("acct-legacy", "principal-l", "hw000000000000003"),
		AccountID:    "acct-legacy", PrincipalID: "principal-l",
	})
	if err != nil {
		t.Fatalf("同账号重登: %v", err)
	}
	if fp2 != fpLegacy {
		t.Fatalf("老文件必须被原地更新：legacy=%s 本次=%s", fpLegacy, fp2)
	}
	if got := len(loadUIDs(t, dir)); got != 1 {
		t.Fatalf("升级后不应多出条目，实际 %d 条", got)
	}
}

// TestSaveAuthNoIdentityFallsBackToAK 上游哪天连 refresh_token 都不给（或 JWS 变异导致解不出）：
// 退回 AK 作 uid，**但绝不能复用已有账号的 UID** —— 那正是覆盖别人的老路。
func TestSaveAuthNoIdentityFallsBackToAK(t *testing.T) {
	dir := t.TempDir()
	if _, err := SaveAuth(dir, Result{
		UID: "AK-EXISTING", AccessToken: "AK-EXISTING", AccessKeyID: "AK-EXISTING", SecretAccessKey: "SK-E",
	}); err != nil {
		t.Fatalf("首个 SaveAuth: %v", err)
	}
	fp2, err := SaveAuth(dir, Result{
		UID: "AK-NOIDENT", AccessToken: "AK-NOIDENT", AccessKeyID: "AK-NOIDENT", SecretAccessKey: "SK-N",
	})
	if err != nil {
		t.Fatalf("无身份 SaveAuth: %v", err)
	}
	if got := filepath.Base(fp2); got != "codearts-AK-NOIDENT.json" {
		t.Fatalf("无身份时应以自己的 AK 命名，实际 %s（复用了别人的 UID = 覆盖 bug 复现）", got)
	}
	if got := len(loadUIDs(t, dir)); got != 2 {
		t.Fatalf("应为 2 条，实际 %d", got)
	}
}

// TestSaveAuthRefusesCrossAccountOverwrite 跨账号覆盖护栏：Label 手滑填成另一个账号已有的
// 文件名时必须**报错且不动文件**，而不是静默覆盖（Label 是唯一能撞出这种情况的输入）。
func TestSaveAuthRefusesCrossAccountOverwrite(t *testing.T) {
	dir := t.TempDir()
	fp1, err := SaveAuth(dir, Result{
		UID: "AK-1", AccessToken: "AK-1", AccessKeyID: "AK-1", SecretAccessKey: "SK-1",
		RefreshToken: fakeRT("acct-aaaa", "principal-a", "hw000000000000001"),
		AccountID:    "acct-aaaa", PrincipalID: "principal-a",
	})
	if err != nil {
		t.Fatalf("SaveAuth(1): %v", err)
	}
	// 同一 Label，但本次登录的是另一个账号 ⇒ 必须拒绝。
	_, err = SaveAuth(dir, Result{
		Label: "acct-aaaa", UID: "AK-2", AccessToken: "AK-2",
		AccessKeyID: "AK-2", SecretAccessKey: "SK-2",
		RefreshToken: fakeRT("acct-bbbb", "principal-b", "hw000000000000002"),
		AccountID:    "acct-bbbb", PrincipalID: "principal-b",
	})
	if err == nil {
		t.Fatal("跨账号写同一文件必须报错，实际成功")
	}
	if !strings.Contains(err.Error(), "拒绝覆盖") {
		t.Fatalf("错误信息应说明拒绝覆盖，实际：%v", err)
	}
	raw, _ := os.ReadFile(fp1)
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if a.AccessToken != "AK-1" {
		t.Fatalf("被拒绝时不得改动目标文件：got %q", a.AccessToken)
	}
}

// TestSaveAuthLabelStaysHighestPriority Label 仍是最高优先级：同一账号的两套会话想各成一条，
// 填不同 Label 即可（用户显式意图，不被 account_id 合并）。
func TestSaveAuthLabelStaysHighestPriority(t *testing.T) {
	dir := t.TempDir()
	same := fakeRT("acct-aaaa", "principal-a", "hw000000000000001")
	fp1, err := SaveAuth(dir, Result{
		Label: "工作号", UID: "AK-1", AccessToken: "AK-1", AccessKeyID: "AK-1", SecretAccessKey: "SK-1",
		RefreshToken: same, AccountID: "acct-aaaa",
	})
	if err != nil {
		t.Fatalf("SaveAuth(1): %v", err)
	}
	fp2, err := SaveAuth(dir, Result{
		Label: "生活号", UID: "AK-2", AccessToken: "AK-2", AccessKeyID: "AK-2", SecretAccessKey: "SK-2",
		RefreshToken: same, AccountID: "acct-aaaa",
	})
	if err != nil {
		t.Fatalf("SaveAuth(2): %v", err)
	}
	if fp1 == fp2 {
		t.Fatalf("不同 Label 必须各自成条：%s vs %s", fp1, fp2)
	}
	if got := len(loadUIDs(t, dir)); got != 2 {
		t.Fatalf("应为 2 条，实际 %d", got)
	}
}
