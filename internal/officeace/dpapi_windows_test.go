//go:build windows

package officeace

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"
)

// 本文件锁死 DPAPI 链路的**调用契约**与文件层语义。
//
// 为什么必须存在：`cryptUnprotect` 曾因为少传一个参数（`ppszDataDescr`）而在本机
// 恒失败，而失败被 `loadOAuthProfile` 静默吞掉（`err != nil → continue`），最终
// 表现为「导入的 OfficeAce 账号永远没有临时凭据」——一条没有任何日志的静默失效链
// （R34/R41 家族）。syscall 的参数错位在编译期与 vet 期都查不出来，只能靠**真实
// 往返调用**锁死：用 CryptProtectData 加密一段假文本，再要求 cryptUnprotect 原样解回。

var procProtectData = syscall.NewLazyDLL("crypt32.dll").NewProc("CryptProtectData")

// cryptProtectForTest 用 CryptProtectData（CurrentUser 作用域）加密，作为往返测试的输入。
// 与 cryptUnprotect 同款 7 参契约。
func cryptProtectForTest(t *testing.T, plain []byte) []byte {
	t.Helper()
	in := newBlob(plain)
	var out dataBlob
	ret, _, callErr := procProtectData.Call(
		uintptr(unsafe.Pointer(&in)),  // pDataIn
		0,                             // szDataDescr
		0,                             // pOptionalEntropy
		0,                             // pvReserved
		0,                             // pPromptStruct
		0,                             // dwFlags（0 = CurrentUser）
		uintptr(unsafe.Pointer(&out)), // pDataOut
	)
	if ret == 0 {
		t.Fatalf("CryptProtectData 失败（测试环境 DPAPI 不可用）：%v", callErr)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	if out.pbData == nil || out.cbData == 0 {
		t.Fatal("CryptProtectData 返回空 blob")
	}
	res := unsafe.Slice(out.pbData, out.cbData)
	return append([]byte(nil), res...)
}

// TestCryptUnprotectRoundTrip 锁死 cryptUnprotect 的参数个数与顺序。
//
// 反向验证（已做）：把 `ppszDataDescr` 那个 0 删掉（退回 6 参写法），本用例立即失败并
// 报 "The parameter is incorrect." —— 因为 dwFlags 收到了 &out 指针值。
func TestCryptUnprotectRoundTrip(t *testing.T) {
	const want = "officeace-dpapi-roundtrip-probe" // 假值：不得使用真实密钥/令牌
	blob := cryptProtectForTest(t, []byte(want))
	got, err := cryptUnprotect(blob)
	if err != nil {
		t.Fatalf("cryptUnprotect 失败：%v", err)
	}
	if !bytes.Equal(got, []byte(want)) {
		t.Fatalf("往返内容不一致：got %q want %q", got, want)
	}
}

// TestReadProtectedTextRoundTrip 覆盖文件层：`OC-DPAPI-1\n<base64>` 形态。
func TestReadProtectedTextRoundTrip(t *testing.T) {
	const want = "install-secret-roundtrip" // 假值
	blob := cryptProtectForTest(t, []byte(want))
	p := filepath.Join(t.TempDir(), ".oauth-profile-encryption-key")
	content := dpapiHeader + "\n" + base64.StdEncoding.EncodeToString(blob) + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readProtectedText(p)
	if err != nil {
		t.Fatalf("readProtectedText 失败：%v", err)
	}
	if got != want {
		t.Fatalf("解出内容不一致：got %q want %q", got, want)
	}
}

// TestReadProtectedTextPlainPassthrough 无魔数头的文件按明文返回（既有语义，不得回归）。
func TestReadProtectedTextPlainPassthrough(t *testing.T) {
	p := filepath.Join(t.TempDir(), "plain.txt")
	if err := os.WriteFile(p, []byte("plain-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readProtectedText(p)
	if err != nil {
		t.Fatalf("明文直通失败：%v", err)
	}
	if got != "plain-secret" {
		t.Fatalf("明文直通内容不一致：got %q", got)
	}
}
