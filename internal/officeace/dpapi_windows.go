//go:build windows

package officeace

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unsafe"
)

// DPAPI 解密（Windows 专属）：OfficeAce 把安装密钥与部分敏感档案用 DPAPI 加密落盘，
// 文件形如 "OC-DPAPI-1\n<base64(blob)>"。解密用 CryptUnprotectData（等价 .NET
// ProtectedData.Unprotect(..., CurrentUser)），无需 PowerShell。
//
// 非 Windows 平台无法解密（DPAPI 绑定 Windows 登录会话密钥），见 dpapi_other.go。

var (
	crypt32           = syscall.NewLazyDLL("crypt32.dll")
	procUnprotectData = crypt32.NewProc("CryptUnprotectData")
	kernel32          = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree     = kernel32.NewProc("LocalFree")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(d []byte) dataBlob {
	if len(d) == 0 {
		return dataBlob{}
	}
	return dataBlob{cbData: uint32(len(d)), pbData: &d[0]}
}

// dpapiSupported 报告当前平台是否支持 DPAPI 解密。
func dpapiSupported() bool { return true }

// readProtectedText 读文件：首行是 "OC-DPAPI-1" 则走 DPAPI 解密，否则当明文返回。
func readProtectedText(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(text, dpapiHeader) {
		return text, nil
	}
	b64 := strings.ReplaceAll(strings.TrimPrefix(text, dpapiHeader), "\n", "")
	b64 = strings.TrimSpace(b64)
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("dpapi: base64 解码失败：%w", err)
	}
	plain, err := cryptUnprotect(blob)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// cryptUnprotect 调 CryptUnprotectData 解密 blob。
func cryptUnprotect(blob []byte) ([]byte, error) {
	in := newBlob(blob)
	var out dataBlob
	ret, _, err := procUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // pOptionalEntropy
		0, // pvReserved
		0, // pPromptStruct
		0, // dwFlags（0 = CurrentUser 作用域）
		uintptr(unsafe.Pointer(&out)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("dpapi: CryptUnprotectData 失败：%v", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	if out.pbData == nil || out.cbData == 0 {
		return nil, fmt.Errorf("dpapi: 解密结果为空")
	}
	res := unsafe.Slice(out.pbData, out.cbData)
	cp := make([]byte, len(res))
	copy(cp, res)
	return cp, nil
}
