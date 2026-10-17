// sign.go 华为云 SDK-HMAC-SHA256 请求签名（协议 §2.2）。
//
// 这套签名必须**逐字节一致**，否则上游回
// `401 {"error_code":"APIG.0301","error_msg":"...verify ak sk signature fail"}`。
// 三个最容易写错的点（协议 §2.2「移植要点」）：
//
//  1. `maas_type: benefit` **必须参与签名**（早于计算加入，且随请求发出）；
//  2. `Agent-Type` / `X-Language` **绝不参与签名**，只能签名后追加；
//  3. `host` 参与签名，但**不手工设置**请求头——Go 的 Host 由 URL 生成
//     （手工 Header.Set("Host") 会被 net/http 忽略），两者天然一致。
//
// 另有两条形态细节：URI 要**补结尾 `/`**；query 串要**去掉前导 `?`** 且不重排。
// 时间戳按协议把 ISO 串里的 `-`、`:` 与毫秒剥掉、保留 `Z`（等价于直接格式化成
// `20060102T150405Z`，且没有时区歧义）。
package codearts

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// signAlgorithm 签名方案名（协议 §2.2）。
const signAlgorithm = "SDK-HMAC-SHA256"

// sdkDate 生成秒级、无分隔的 x-sdk-date（形如 20261016T103000Z）。
//
// 等价于协议里 `Date.toISOString().replace(/-|:/g,”).replace(/\.\d+Z$/,'Z')`：
// toISOString 恒为 UTC，故这里直接以 UTC 格式化（不含毫秒）。
func sdkDate(t time.Time) string { return t.UTC().Format("20060102T150405Z") }

// sha256Hex 返回小写十六进制 SHA-256。
func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// hmacSHA256 计算 HMAC-SHA256（原始字节）。
func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// canonicalURI 参与签名的规范 URI：取 `url.pathname` 并对非空路径**补结尾 `/`**。
//
// 用 EscapedPath 而非 Path：协议取的是 URL 里 pathname 的**已编码**形态，
// Path 会先解码再让我们重编码，等于擅自换一套编码规则。
func canonicalURI(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// normalizeSignedHeaders 归一化参与签名的头：键小写、值 trim 后折叠连续空白；
// 返回（canonical headers 块, SignedHeaders 串）。
//
// 头名按**小写字典序**排序——协议明确 SignedHeaders 是 `h1;h2;…` 且头名全小写。
// 值折叠空白对齐华为云官方 SDK 的 CanonicalHeaders；协议里的取值本身都无空格，
// 故对真实请求是无操作。
func normalizeSignedHeaders(in map[string]string) (string, string) {
	names := make([]string, 0, len(in))
	values := make(map[string]string, len(in))
	for k, v := range in {
		lk := strings.ToLower(strings.TrimSpace(k))
		if lk == "" {
			continue
		}
		if _, dup := values[lk]; !dup {
			names = append(names, lk)
		}
		values[lk] = strings.Join(strings.Fields(v), " ")
	}
	sort.Strings(names)
	var sb strings.Builder
	for _, n := range names {
		sb.WriteString(n)
		sb.WriteByte(':')
		sb.WriteString(values[n])
		sb.WriteByte('\n')
	}
	return sb.String(), strings.Join(names, ";")
}

// canonicalRequest 组装 SDK-HMAC-SHA256 的规范请求串（协议 §2.2）。
//
// 形态（\n 为字面换行，倒数第三行是**空行**）：
//
//	method\nuri\nquery\n<header:value…>\n\n<signedHeaders>\n<payloadHash>
func canonicalRequest(method string, u *url.URL, body []byte, signed map[string]string) (canonical, signedNames string) {
	block, names := normalizeSignedHeaders(signed)
	var sb strings.Builder
	sb.WriteString(method)
	sb.WriteByte('\n')
	sb.WriteString(canonicalURI(u))
	sb.WriteByte('\n')
	// query 取 RawQuery（已去掉前导 '?'）：协议要求「参数保持原样、不重排」，
	// 用 url.Query() 再拼会按 key 重排且重编码。
	sb.WriteString(u.RawQuery)
	sb.WriteByte('\n')
	sb.WriteString(block)
	sb.WriteByte('\n')
	sb.WriteString(names)
	sb.WriteByte('\n')
	sb.WriteString(sha256Hex(body))
	return sb.String(), names
}

// SignRequest 计算 SDK-HMAC-SHA256 签名头集（含 Authorization）。
//
// 参与签名的头（小写）：host、x-sdk-date、x-sdk-content-sha256、x-security-token、
// content-type（**仅非 GET**）、以及 extraSigned 里的业务头（当前唯一用途是
// `maas_type: benefit`）。**没有任何 nonce / request-id**（协议 §2.2）。
//
// 返回 map 的键已是小写形态，用 ApplySignedHeaders 落到请求上。
func SignRequest(ak, sk, securityToken, method, rawURL string, body []byte, contentType string,
	extraSigned map[string]string, t time.Time) (map[string]string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("codearts: 解析 URL 失败: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("codearts: 签名 URL 缺 host: %s", rawURL)
	}
	if strings.TrimSpace(ak) == "" || strings.TrimSpace(sk) == "" {
		return nil, fmt.Errorf("codearts: 缺 access_key_id / secret_access_key")
	}
	date := sdkDate(t)
	signed := map[string]string{
		hdrHost:          u.Host,
		hdrSDKDate:       date,
		hdrContentSHA256: sha256Hex(body),
	}
	if strings.TrimSpace(securityToken) != "" {
		signed[hdrSecurityToken] = securityToken
	}
	// content-type 只在有请求体时参与签名（GET 不带，协议 §2.2 表）。
	if strings.TrimSpace(contentType) != "" {
		signed[hdrContentType] = contentType
	}
	for k, v := range extraSigned {
		lk := strings.ToLower(strings.TrimSpace(k))
		if lk == "" {
			continue
		}
		signed[lk] = v
	}

	canonical, signedNames := canonicalRequest(method, u, body, signed)
	stringToSign := signAlgorithm + "\n" + date + "\n" + sha256Hex([]byte(canonical))
	sig := hex.EncodeToString(hmacSHA256([]byte(sk), []byte(stringToSign)))

	out := make(map[string]string, len(signed)+1)
	for k, v := range signed {
		out[k] = v
	}
	out[hdrAuthorization] = signAlgorithm + " Access=" + ak + ",SignedHeaders=" + signedNames + ",Signature=" + sig
	return out, nil
}

// ApplySignedHeaders 把签名头写到出站请求上。
//
// 刻意**跳过 host**：Go 的请求行 Host 由 req.URL.Host 生成，手工 Header.Set("Host")
// 会被 net/http 忽略（协议同款处理：`host` 参与签名但不手工设置）。
func ApplySignedHeaders(req *http.Request, headers map[string]string) {
	for k, v := range headers {
		if strings.EqualFold(k, hdrHost) {
			continue
		}
		req.Header.Set(k, v)
	}
}

// SignedHeaderNames 解出 Authorization 里的 SignedHeaders（供测试与日志断言用；
// 不解析则返回空串）。**绝不**用于业务分支。
func SignedHeaderNames(authorization string) string {
	const marker = "SignedHeaders="
	i := strings.Index(authorization, marker)
	if i < 0 {
		return ""
	}
	rest := authorization[i+len(marker):]
	if j := strings.IndexByte(rest, ','); j >= 0 {
		return rest[:j]
	}
	return rest
}
