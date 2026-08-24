// encoding.go 实现 QoderEncoding：base64 + 自定义字母表 + 三段重排。
// 移植自 qoderwork2api internal/upstream/encoding.go。
package qoder

import (
	"encoding/base64"
	"fmt"
	"strings"
)

const (
	customAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	stdAlphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	customPad      = '$'
)

var stdToCustom [128]byte
var customToStd [256]byte

func init() {
	for i := range stdToCustom {
		stdToCustom[i] = 0xFF
	}
	for i := range customToStd {
		customToStd[i] = 0xFF
	}
	for i := 0; i < len(stdAlphabet); i++ {
		stdToCustom[stdAlphabet[i]] = customAlphabet[i]
		customToStd[customAlphabet[i]] = stdAlphabet[i]
	}
	customToStd[customPad] = '='
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

// qoderDecode 逆编码：字符映射（'$' → '='）→ 三段逆重排 → base64 解码。
func qoderDecode(enc string) ([]byte, error) {
	n := len(enc)
	if n == 0 {
		return nil, nil
	}
	mapped := make([]byte, n)
	for i := 0; i < n; i++ {
		c := enc[i]
		s := customToStd[c]
		if s == 0xFF {
			return nil, fmt.Errorf("invalid char %q at %d", c, i)
		}
		mapped[i] = s
	}
	a := n / 3
	std := string(mapped[n-a:]) + string(mapped[a:n-a]) + string(mapped[:a])
	return base64.StdEncoding.DecodeString(std)
}
