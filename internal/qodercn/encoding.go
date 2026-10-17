// encoding.go 实现 QoderEncoding：base64 + 自定义字母表 + 三段重排。
// 代码级复制自 internal/qoder/encoding.go（同源自 qoderwork2api/qoder2api，两实现逐字一致）。
package qodercn

import (
	"encoding/base64"
	"strings"
)

const (
	customAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	stdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	customPad      = '$'
)

var stdToCustom [128]byte

func init() {
	for i := range stdToCustom {
		stdToCustom[i] = 0xFF
	}
	for i := 0; i < len(stdAlphabet); i++ {
		stdToCustom[stdAlphabet[i]] = customAlphabet[i]
	}
}

// qoderEncode 编码：base64 → 三段重排 → 字符映射（'=' → '$'）。
func qoderEncode(plain []byte) string {
	std := base64.StdEncoding.EncodeToString(plain)
	n := len(std)
	if n == 0 {
		return ""
	}
	a := n / 3
	rearranged := std[n-a:] + std[a:n-a] + std[:a]
	var sb strings.Builder
	sb.Grow(n)
	for i := 0; i < n; i++ {
		c := rearranged[i]
		if c == '=' {
			sb.WriteByte(customPad)
		} else {
			sb.WriteByte(stdToCustom[c])
		}
	}
	return sb.String()
}
