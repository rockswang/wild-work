package officeace

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// 本文件回归「从客户端导入」的两个真缺陷（均为实证）：
//
//	R-1 installRoots() 缺 Windows per-user 安装位置（%LOCALAPPDATA%\Programs\OfficeAce）⇒ 档案目录全 miss；
//	R-2 档案名 uid 段是桌面端的 URL 编码形式（'%3A'），而 acc.ID 是 decodeUID 后的含 ':' 真 uid
//	    ⇒ 只认解码形式在 Windows 上永远 miss（文件名不允许 ':'）。
//
// 纪律：测试只用假值（假 AK/SK/token），不打印任何密钥值；DPAPI 行为不在此覆盖
// （注入 readProtectedTextFn 桩即可在任意平台复现路径与文件名匹配逻辑）。

const (
	testInstallSecret  = "test-install-secret-not-a-real-key" // 假安装密钥
	testFakeAK         = "AKFAKE-not-a-real-key"
	testFakeSK         = "SKFAKE-not-a-real-key"
	testFakeSTSToken   = "STSFAKE-not-a-real-token"
	testFakeRefreshTok = "RTFAKE-not-a-real-token"
)

// testProfileUID 形如 "<accountId>:<userId>"（32 位 hex + ':' + 32 位 hex），与真机 uid 同形。
const testProfileUID = "01a13f3d3ee6741d832a05b19fb0c028:01a13f3d3ee67da097f2cae2d1415761"

// expectedProfileName 是桌面端实际落盘的文件名（uid 段为 URL 编码，'%3A' 大写）。
const expectedProfileName = "oauth-01a13f3d3ee6741d832a05b19fb0c028%3A01a13f3d3ee67da097f2cae2d1415761.json"

// realReadProtectedText 保存被测试接管的真实实现，供「正向/反向」两种断言切换。
var realReadProtectedText = readProtectedText

// ---- R-1：安装根候选 ----

func TestInstallRootsIncludesWindowsPerUserPrograms(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("per-user 安装根候选是 Windows 语义（依赖 %LOCALAPPDATA%）")
	}
	local := t.TempDir()
	appdata := t.TempDir()
	// installRoots 先做 os.Stat 存在性过滤（既有语义、不在本次改动内），故先把候选目录建出来。
	want := filepath.Join(local, "Programs", "OfficeAce")
	for _, d := range []string{want, filepath.Join(local, "OfficeAce"), filepath.Join(appdata, "OfficeAce")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("建候选目录失败：%v", err)
		}
	}
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("APPDATA", appdata)
	t.Setenv("OFFICEACE_INSTALL_DIR", "")

	roots := installRoots(nil)
	if !containsStr(roots, want) {
		t.Fatalf("installRoots() 缺 per-user 候选 %q；实际候选=%v", want, roots)
	}
	// 顺带覆盖 %LOCALAPPDATA%\OfficeAce 与 %APPDATA%\OfficeAce 变体。
	if alt := filepath.Join(local, "OfficeAce"); !containsStr(roots, alt) {
		t.Fatalf("installRoots() 缺 %q；实际候选=%v", alt, roots)
	}
	if alt := filepath.Join(appdata, "OfficeAce"); !containsStr(roots, alt) {
		t.Fatalf("installRoots() 缺 %q；实际候选=%v", alt, roots)
	}
	// 去重：同一路径只能出现一次。
	if n := countStr(roots, want); n != 1 {
		t.Fatalf("installRoots() 候选重复：%q 出现 %d 次；实际候选=%v", want, n, roots)
	}
}

// TestInstallRootsInfersPerUserRootWithoutLocalAppData：%LOCALAPPDATA% 缺失时的兜底推导必须落在
// <用户目录>\AppData\Local\Programs\OfficeAce；**不得**落到 <用户目录>\AppData\Programs\OfficeAce
//（后者是「用 filepath.Dir(os.UserConfigDir()) 当 Local」这一错误的产物，本用例专挡它）。
func TestInstallRootsInfersPerUserRootWithoutLocalAppData(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("per-user 安装根候选是 Windows 语义")
	}
	base := t.TempDir()
	userProfile := filepath.Join(base, "u")
	appData := filepath.Join(userProfile, "AppData")
	local := filepath.Join(appData, "Local")
	inferred := filepath.Join(local, "Programs", "OfficeAce")
	buggy := filepath.Join(appData, "Programs", "OfficeAce") // 「取 AppData 而非 AppData\Local」的产物
	fromConfigDir := filepath.Join(local, "OfficeAce")       // UserConfigDir 同级 Local 的第二个候选
	for _, d := range []string{inferred, fromConfigDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("建候选目录失败：%v", err)
		}
	}
	t.Setenv("LOCALAPPDATA", "")                                // 模拟缺失
	t.Setenv("USERPROFILE", userProfile)                        // 推导 ①
	t.Setenv("APPDATA", filepath.Join(appData, "Roaming"))      // 推导 ②（os.UserConfigDir 的来源）
	t.Setenv("OFFICEACE_INSTALL_DIR", "")

	roots := installRoots(nil)
	if !containsStr(roots, inferred) {
		t.Fatalf("LOCALAPPDATA 缺失时未推导出 %q（localAppDataCandidate()=%q，目录存在=%v）；实际候选=%v",
			inferred, localAppDataCandidate(), dirExists(inferred), roots)
	}
	if !containsStr(roots, fromConfigDir) {
		t.Fatalf("LOCALAPPDATA 缺失时未由 UserConfigDir 推出 %q；实际候选=%v", fromConfigDir, roots)
	}
	if containsStr(roots, buggy) {
		t.Fatalf("兜底推错目录：出现了 %q（应为 %q）；实际候选=%v", buggy, inferred, roots)
	}
}

// ---- R-2：档案名匹配（'%3A' 编码名 / ':' 解码名 / 编码优先） ----

func TestOAuthProfileNameCandidatesPreferEncodedForm(t *testing.T) {
	got := oauthProfileNameCandidates(testProfileUID)
	want := []string{expectedProfileName, "oauth-" + testProfileUID + ".json"}
	if len(got) != len(want) {
		t.Fatalf("oauthProfileNameCandidates(%q)=%v，期望 %v（两种写法都要试）", testProfileUID, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个候选=%q，期望 %q（编码形式必须优先）", i, got[i], want[i])
		}
	}
	// 无 ':' 的 uid（目录名未被编码过）→ 只试自身一种写法。
	plain := "01a13f3d3ee6741d832a05b19fb0c028"
	if n := oauthProfileNameCandidates(plain); len(n) != 1 || n[0] != "oauth-"+plain+".json" {
		t.Fatalf("无 ':' 的 uid 候选=%v，期望只试自身一种写法", n)
	}
}

func TestLoadOAuthProfileMatchesEncodedFileName(t *testing.T) {
	root := t.TempDir()
	writeTestProfile(t, root, expectedProfileName, testInstallSecret) // 落盘名 = 编码形式

	restoreTestReadProtectedText(t)
	readProtectedTextFn = func(string) (string, error) { return testInstallSecret, nil }

	prof := loadOAuthProfile(testProfileUID, []string{root})
	if prof == nil {
		t.Fatal("loadOAuthProfile 未命中 '%3A' 编码档案名（R-2 回归）")
	}
	if prof.Credential.Access != testFakeAK || prof.Credential.Secret != testFakeSK ||
		prof.Credential.STSToken != testFakeSTSToken || prof.Credential.RefreshToken != testFakeRefreshTok {
		t.Fatal("档案解密字段与写入值不一致")
	}
}

func TestLoadOAuthProfileMatchesDecodedFileName(t *testing.T) {
	root := t.TempDir()
	// 落盘名用解码形式（含 ':'）；非 Windows 上 os.WriteFile 会拒绝 ':' ⇒ 跳过该断言。
	if runtime.GOOS != "windows" {
		t.Skip("文件名含 ':' 只在 Windows 语义下存在（POSIX 允许但路径语义不同）")
	}
	writeTestProfile(t, root, "oauth-"+testProfileUID+".json", testInstallSecret)

	restoreTestReadProtectedText(t)
	readProtectedTextFn = func(string) (string, error) { return testInstallSecret, nil }

	if prof := loadOAuthProfile(testProfileUID, []string{root}); prof == nil {
		t.Fatal("loadOAuthProfile 未命中 ':' 解码档案名（两种写法都必须能试）")
	}
}

// TestLoadOAuthProfileEncodedPreferredOverDecoded：两种写法同时存在时，编码形式优先（桌面端真实写法）。
func TestLoadOAuthProfileEncodedPreferredOverDecoded(t *testing.T) {
	root := t.TempDir()
	writeTestProfile(t, root, expectedProfileName, testInstallSecret)
	if runtime.GOOS == "windows" {
		// 写一个可解析但内容不同的「解码名」档案；若实现先试解码形式就会解出它 ⇒ 断言失败。
		writeTestProfile(t, root, "oauth-"+testProfileUID+".json", testInstallSecret, "AKWRONG-not-a-real-key")
	}

	restoreTestReadProtectedText(t)
	readProtectedTextFn = func(string) (string, error) { return testInstallSecret, nil }

	prof := loadOAuthProfile(testProfileUID, []string{root})
	if prof == nil {
		t.Fatal("loadOAuthProfile 未命中任何档案名")
	}
	if prof.Credential.Access != testFakeAK {
		t.Fatalf("编码形式未优先：解出的 access=%q（期望编码档案里的值）", prof.Credential.Access)
	}
}

// TestLoadOAuthProfilePlaintextInstallKeyOnWindows：端到端（真实 readProtectedText，
// 密钥文件为明文，不触碰 DPAPI），证明「路径 + 文件名 + 解密」三者串起来能出凭据。
func TestLoadOAuthProfilePlaintextInstallKeyOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("非 Windows 下密钥文件是 DPAPI blob，无法解密（readProtectedText 必然报错）")
	}
	root := t.TempDir()
	writeTestProfile(t, root, expectedProfileName, testInstallSecret)
	restoreTestReadProtectedText(t) // 不注入桩：走真实读取路径

	prof := loadOAuthProfile(testProfileUID, []string{root})
	if prof == nil {
		t.Fatal("端到端未取到档案（安装根/文件名/解密任一处 miss）")
	}
	if prof.Credential.RefreshToken != testFakeRefreshTok {
		t.Fatal("端到端解出的 refresh_token 与写入值不一致")
	}
}

// TestLoadOAuthProfileKeepsGoingWhenNoProfile：某安装根缺档案/缺密钥时不报错，且不影响其它根。
//
// ⚠️ 2026-10-17 修正一处**断言写反**的缺陷（测试自引入起就是红的，`go test ./...` 在
// internal/officeace 包上从未真正绿过）：原文案写「有密钥文件但无档案的根**不应**触碰
// DPAPI（先在磁盘层排除）」，而代码是 `if !pr.probed() { t.Fatal(...) }` —— 断言的是
// **必须**被探测过，与文案相反、也与实现相反（loadOAuthProfile 先找档案文件，miss 就
// `continue`，根本走不到读密钥那一步）。当时之所以没被发现，是因为它只在**包级全量跑**
// 时才暴露，容易被「看结尾 ok」的习惯漏掉。
//
// 现在的两条断言都与设计一致，并且**互相区分**：
//   - 只有密钥、没有档案 ⇒ 一次也不能读密钥（磁盘层排除，避免对每个候选根做无谓 DPAPI 调用）；
//   - 有档案 ⇒ 必须真的读过密钥（否则「能读出档案」就是靠别的路径蒙对的）。
func TestLoadOAuthProfileKeepsGoingWhenNoProfile(t *testing.T) {
	restoreTestReadProtectedText(t)
	keylessRoot := emptySecureRoot(t) // 有密钥文件、没有档案
	accRoot := t.TempDir()
	writeTestProfile(t, accRoot, expectedProfileName, testInstallSecret)
	emptyRoot := emptySecureRoot(t) // 有密钥文件、没有档案（同 keylessRoot，但顺序在后）

	pr := newProbeReadProtectedText(testInstallSecret)
	readProtectedTextFn = pr.fn

	if prof := loadOAuthProfile(testProfileUID, []string{keylessRoot, emptyRoot}); prof != nil {
		t.Fatal("无档案的安装根不应返回档案")
	}
	if pr.probed() {
		t.Fatal("无档案的根不得去读密钥（应在磁盘层先排除，避免无谓的 DPAPI 调用）")
	}
	pr.reset()
	prof := loadOAuthProfile(testProfileUID, []string{keylessRoot, emptyRoot, accRoot})
	if prof == nil {
		t.Fatal("前面的安装根无档案时，应继续向后找（不得提前放弃）")
	}
	if !pr.probed() {
		t.Fatal("命中档案的根必须真的读过密钥（否则「解出档案」是假的）")
	}
	if prof.Credential.RefreshToken != testFakeRefreshTok {
		t.Fatal("续找命中的档案内容不正确")
	}
}

// ---- 测试辅助（只用假值；不打印任何密钥） ----

// writeTestProfile 在 <root>/packages/api/.config/secure-config-nodejs/ 下落盘
// 一个 AES-256-GCM 加密档案 + 明文安装密钥；ak 省略时用假 AK。
func writeTestProfile(t *testing.T, root, fileName, secret string, ak ...string) {
	t.Helper()
	access := testFakeAK
	if len(ak) > 0 {
		access = ak[0]
	}
	dir := filepath.Join(root, filepath.FromSlash(secureDirRel))
	if err := os.MkdirAll(dir, 0o700); err != nil { // 同时保证 root 本身存在（installRoots 会 os.Stat）
		t.Fatalf("建档案目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".oauth-profile-encryption-key"), []byte(secret), 0o600); err != nil {
		t.Fatalf("写假安装密钥失败：%v", err)
	}
	raw := encryptTestProfile(t, secret, access)
	if err := os.WriteFile(filepath.Join(dir, fileName), raw, 0o600); err != nil {
		t.Fatalf("写假档案失败：%v", err)
	}
}

// encryptTestProfile 生成桌面端同形的加密档案信封：
// {version:1, alg:"aes-256-gcm", iv/tag/data: base64url}，密钥 = SHA-256(secret)。
func encryptTestProfile(t *testing.T, secret, access string) []byte {
	t.Helper()
	plain, err := jsonMarshal(map[string]any{
		"authType":  "huawei-oauth",
		"accountId": "acc-fake",
		"user_id":   "usr-fake",
		"userName":  "测试账号",
		"credential": map[string]any{
			"access":             access,
			"secret":             testFakeSK,
			"sts_token":          testFakeSTSToken,
			"project_id":         "pj-fake",
			"expires_at":         "2026-11-08T16:43:17.412Z",
			"refresh_token":      testFakeRefreshTok,
			"refresh_expires_at": "2026-12-08T16:43:17.412Z",
		},
	})
	if err != nil {
		t.Fatalf("构造假档案内容失败：%v", err)
	}
	key := sha256Sum(secret)
	iv := make([]byte, 12)
	for i := range iv {
		iv[i] = byte(i + 1)
	}
	ct := mustGCM(t, key).Seal(nil, iv, plain, nil)
	env, err := jsonMarshal(map[string]any{
		"version": 1, "alg": "aes-256-gcm",
		"iv":   b64url(iv),
		"tag":  b64url(ct[len(ct)-16:]),
		"data": b64url(ct[:len(ct)-16]),
	})
	if err != nil {
		t.Fatalf("构造假档案信封失败：%v", err)
	}
	return env
}

// emptySecureRoot 造一个只有密钥文件、没有档案的安装根。
func emptySecureRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, filepath.FromSlash(secureDirRel))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("建目录失败：%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".oauth-profile-encryption-key"), []byte(testInstallSecret), 0o600); err != nil {
		t.Fatalf("写假安装密钥失败：%v", err)
	}
	return root
}

type probeReadProtectedText struct {
	secret string
	n      int
}

func newProbeReadProtectedText(secret string) *probeReadProtectedText {
	return &probeReadProtectedText{secret: secret}
}

func (p *probeReadProtectedText) fn(string) (string, error) {
	p.n++
	return p.secret, nil
}

func (p *probeReadProtectedText) probed() bool { return p.n > 0 }

func (p *probeReadProtectedText) reset() { p.n = 0 }

// restoreTestReadProtectedText 在每个用例末尾把包内间接层还原成真实实现，
// 避免用例间串味（Go 测试默认串行，但显式还原更稳）。
func restoreTestReadProtectedText(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { readProtectedTextFn = realReadProtectedText })
}

func containsStr(list []string, want string) bool { return countStr(list, want) > 0 }

// dirExists 供断言消息区分「推导错路径」与「推导为空/目录不存在」。
func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func countStr(list []string, want string) int {
	n := 0
	for _, s := range list {
		if s == want {
			n++
		}
	}
	return n
}
