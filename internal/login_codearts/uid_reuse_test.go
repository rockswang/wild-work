package login_codearts

import (
	"path/filepath"
	"strings"
	"testing"

	"wild-work/internal/auth"
)

// TestSaveAuthReusesExistingUID 守门「方案 B」（2026-10-17 用户实测报告落地）：
//
// 上游 STS 响应不含任何身份字段（协议 §2.7），UID 必然退化到**每次新签发的临时** AK；
// 若任其每次都新建条目，同一个华为账号会在面板上不断累积重复项（用户实际遇到的现象：
// 点一次「新增账号」就多出一个账号）。
//
// 约定：**没有稳定身份字段时**，同渠道已有账号 ⇒ 必须复用其 UID（覆盖同一文件），不得新增。
func TestSaveAuthReusesExistingUID(t *testing.T) {
	dir := t.TempDir()

	// 第一次登录：目录为空 ⇒ 只能用 AK 作 UID（保留旧行为）。
	fp1, err := SaveAuth(dir, Result{
		UID: "AK-FIRST", AccessToken: "AK-FIRST",
		AccessKeyID: "AK-FIRST", SecretAccessKey: "SK-FIRST",
	})
	if err != nil {
		t.Fatalf("首次 SaveAuth: %v", err)
	}
	if got := filepath.Base(fp1); got != "codearts-AK-FIRST.json" {
		t.Fatalf("首次落盘文件名 = %s，期望 codearts-AK-FIRST.json", got)
	}

	// 第二次登录：AK 变了、上游仍没给身份字段 ⇒ 必须复用第一个 UID（同一文件被覆盖）。
	fp2, err := SaveAuth(dir, Result{
		UID: "AK-SECOND", AccessToken: "AK-SECOND",
		AccessKeyID: "AK-SECOND", SecretAccessKey: "SK-SECOND",
	})
	if err != nil {
		t.Fatalf("二次 SaveAuth: %v", err)
	}
	if fp2 != fp1 {
		t.Errorf("重复登录必须覆盖同一文件（复用 UID）：首次 %s，二次 %s", fp1, fp2)
	}

	auths, err := auth.LoadCodeArtsDir(dir)
	if err != nil {
		t.Fatalf("LoadCodeArtsDir: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("目录里应只有 1 个账号（不得新增重复条目），实际 %d", len(auths))
	}
	// 复用 UID ≠ 复用旧凭据：AK/SK 必须换成这次的。
	if auths[0].AccessToken != "AK-SECOND" {
		t.Errorf("复用 UID 时应把凭据更新为最新：got %q", auths[0].AccessToken)
	}
}

// TestSaveAuthKeepsStableIDWhenPresent 有稳定身份字段时**不得**复用旧 UID ——
// 那才是真正的多账号场景（方案 A 落地后走这条路径），必须各归各的条目。
func TestSaveAuthKeepsStableIDWhenPresent(t *testing.T) {
	dir := t.TempDir()
	if _, err := SaveAuth(dir, Result{UID: "AK-1", AccessToken: "AK-1", AccessKeyID: "AK-1", SecretAccessKey: "SK1"}); err != nil {
		t.Fatalf("SaveAuth(1): %v", err)
	}
	if _, err := SaveAuth(dir, Result{
		UID: "user-2", UserID: "user-2", AccessToken: "AK-2", AccessKeyID: "AK-2", SecretAccessKey: "SK2",
	}); err != nil {
		t.Fatalf("SaveAuth(2): %v", err)
	}

	auths, err := auth.LoadCodeArtsDir(dir)
	if err != nil {
		t.Fatalf("LoadCodeArtsDir: %v", err)
	}
	if len(auths) != 2 {
		t.Fatalf("有稳定身份字段时应各自成条，实际 %d 个账号", len(auths))
	}
	var uids []string
	for _, a := range auths {
		uids = append(uids, a.UID)
	}
	if !strings.Contains(strings.Join(uids, ","), "user-2") {
		t.Errorf("user-2 应作为独立条目存在，实际 %v", uids)
	}
}
