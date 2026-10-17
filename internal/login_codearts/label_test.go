package login_codearts

import (
	"path/filepath"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/codearts"
)

// TestSaveAuthLabelWinsAndEnablesMultiAccount 守门「账号标识」（2026-10-17，用户要求）：
//
// 上游不提供任何稳定身份字段（实测），所以用户填的 Label 是**唯一**能区分多个华为账号的手段。
// 约定：
//   - **填了 Label 就用它** —— 哪怕该渠道已有账号也**不得**复用（那正是「添加第二个账号」的路径）；
//   - **留空**则维持复用已有 UID（= 同一账号重新登录，覆盖同一文件）；
//   - 同一 Label 重登 ⇒ 覆盖那一条，不新增。
func TestSaveAuthLabelWinsAndEnablesMultiAccount(t *testing.T) {
	dir := t.TempDir()

	// 第一个账号：未填标识 ⇒ 沿用 AK 作 UID（目录为空时的既有行为）。
	fp1, err := SaveAuth(dir, Result{
		UID: "AK-1", AccessToken: "AK-1", AccessKeyID: "AK-1", SecretAccessKey: "SK1",
	})
	if err != nil {
		t.Fatalf("SaveAuth(1): %v", err)
	}

	// 第二个账号：填了标识 ⇒ 必须**另立条目**（不得被复用到第一条上，否则第二个账号无处安放）。
	fp2, err := SaveAuth(dir, Result{
		UID: "AK-2", Label: "工作号", AccessToken: "AK-2", AccessKeyID: "AK-2", SecretAccessKey: "SK2",
	})
	if err != nil {
		t.Fatalf("SaveAuth(2): %v", err)
	}
	if fp2 == fp1 {
		t.Fatalf("填了账号标识必须另立条目，实际覆盖了第一条：%s", fp2)
	}
	if got := filepath.Base(fp2); got != "codearts-工作号.json" {
		t.Errorf("落盘文件名 = %s，期望 codearts-工作号.json", got)
	}

	// 同一标识重登 ⇒ 覆盖那一条（这是「以后重登同一账号填同一标识」的约定）。
	fp3, err := SaveAuth(dir, Result{
		UID: "AK-3", Label: "工作号", AccessToken: "AK-3", AccessKeyID: "AK-3", SecretAccessKey: "SK3",
	})
	if err != nil {
		t.Fatalf("SaveAuth(3): %v", err)
	}
	if fp3 != fp2 {
		t.Errorf("同一标识重登应覆盖同一条目：%s vs %s", fp3, fp2)
	}

	auths, err := auth.LoadCodeArtsDir(dir)
	if err != nil {
		t.Fatalf("LoadCodeArtsDir: %v", err)
	}
	if len(auths) != 2 {
		t.Fatalf("应并存 2 个账号（一个默认 + 一个「工作号」），实际 %d", len(auths))
	}
}

// TestSanitizeFileIDKeepsUnicodeDistinct 守门：**不同中文标识不得撞成同一个文件名**。
//
// 旧实现只放行 `[A-Za-z0-9_-]`，于是每个汉字都变成一个 `_` ⇒「工作号」与「生活号」双双变成
// `___`，两个账号互相覆盖（2026-10-17 落盘测试实测踩到）。修法：放行 Unicode 字母数字。
// 同时必须确认**放宽字符集没有削弱路径穿越防护**。
func TestSanitizeFileIDKeepsUnicodeDistinct(t *testing.T) {
	a, b := codearts.SanitizeFileID("工作号"), codearts.SanitizeFileID("生活号")
	if a == b {
		t.Fatalf("不同中文标识撞名：%q vs %q（两个账号会互相覆盖）", a, b)
	}
	if a != "工作号" {
		t.Errorf("中文标识应原样保留，实际 %q", a)
	}
	if len([]rune(codearts.SanitizeFileID(strings.Repeat("字", 200)))) > 64 {
		t.Error("超长标识必须按 rune 截断（否则文件名过长）")
	}

	// 危险输入仍必须被消毒：不得残留路径分隔符，也不得变成 `.` / `..`。
	bad := []string{"../evil", "..", ".", "", `a\b`, "a/b", `a:b*c?d"e<f>g|h`, "／etc／passwd", "a∕b"}
	for _, in := range bad {
		got := codearts.SanitizeFileID(in)
		if strings.ContainsAny(got, `/\`) {
			t.Errorf("SanitizeFileID(%q) = %q，残留路径分隔符", in, got)
		}
		if got == "." || got == ".." || got == "" {
			t.Errorf("SanitizeFileID(%q) = %q，非法文件名", in, got)
		}
	}
}
