//go:build !windows

package officeace

import (
	"errors"
	"os"
	"strings"
)

// DPAPI 是 Windows 专属能力（绑定 Windows 登录会话密钥），其它平台无法解密。
// 非 Windows 下 oauth 档案的临时 AK/SK 解不出，账号仍可对话（网关 Basic 凭据不过期），
// 但积分/签到会因缺临时凭据而降级——需在支持 DPAPI 的机器上导入，或改用自助 OAuth 登录。

func dpapiSupported() bool { return false }

// readProtectedText 非 Windows：仅支持明文；加密档案返回错误。
func readProtectedText(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(raw))
	if strings.HasPrefix(text, dpapiHeader) {
		return "", errors.New("officeace: 该平台不支持 DPAPI 解密（Windows 专属）")
	}
	return text, nil
}
