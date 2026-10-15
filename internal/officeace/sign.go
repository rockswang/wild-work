package officeace

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"time"
)

// 华为云签名：控制面接口（/v1/claw/*）用 SDK-HMAC-SHA256（V4，密钥即 SK）；
// 模型网关的订阅/费率/签到接口（/v1/subscription*、/v1/model-token-rates）用
// V11-HMAC-SHA256（多一层 region 作用域，且签名密钥需先派生）。
// 用错算法上游回 APIG.0624 Unsupported signature algorithm。
// 参考 officeace2api/src/quota.mjs 的 signV4 / signV11。

const v11Region = "cn-southwest-2" // V11 作用域 region

// sdkDate 生成秒级、无分隔的 X-Sdk-Date（形如 20261008T094600Z）。
func sdkDate(t time.Time) string {
	return t.UTC().Format("20060102T150405Z")
}

// rfc3986 是华为要求的 URI 编码（encodeURIComponent + 补转义 !'()*）。
func rfc3986(s string) string {
	e := url.QueryEscape(s)
	// QueryEscape 把空格编成 '+'，改回 %20；再把 !'()* 补成 %XX。
	e = strings.ReplaceAll(e, "+", "%20")
	var b strings.Builder
	b.Grow(len(e))
	for i := 0; i < len(e); i++ {
		c := e[i]
		switch c {
		case '!', '\'', '(', ')', '*':
			b.WriteString("%")
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// sha256Hex 返回小写十六进制 SHA-256。
func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// canonicalPath 按华为规则取规范 URI：分段 decode 后 rfc3986 重编码，末尾补 '/'。
func canonicalPath(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	segs := strings.Split(u.Path, "/")
	for i, s := range segs {
		if s == "" {
			continue
		}
		dec, derr := url.PathUnescape(s)
		if derr != nil {
			dec = s
		}
		segs[i] = rfc3986(dec)
	}
	p := strings.Join(segs, "/")
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p, nil
}

// canonicalQuery 取规范查询串：query 对按 key 升序、同 key 按 value 升序，rfc3986 编码。
func canonicalQuery(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	type kv struct{ k, v string }
	var pairs []kv
	for k, vs := range u.Query() {
		for _, v := range vs {
			pairs = append(pairs, kv{k, v})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, rfc3986(p.k)+"="+rfc3986(p.v))
	}
	return strings.Join(parts, "&"), nil
}

// normalizeHeaders 归一化参与签名的头：key 转小写，value trim 后折叠连续空白。
func normalizeHeaders(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		lk := strings.ToLower(strings.TrimSpace(k))
		if lk == "" {
			continue
		}
		out[lk] = strings.Join(strings.Fields(v), " ")
	}
	return out
}

// signedHeaderNames 返回参与签名的头名（升序）。
func signedHeaderNames(h map[string]string) []string {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// buildCanonicalRequest 组装规范请求（V4/V11 共用前 7 步）。
func buildCanonicalRequest(method, rawURL, bodyText string, headers map[string]string) (string, string, error) {
	cp, err := canonicalPath(rawURL)
	if err != nil {
		return "", "", err
	}
	cq, err := canonicalQuery(rawURL)
	if err != nil {
		return "", "", err
	}
	nh := normalizeHeaders(headers)
	names := signedHeaderNames(nh)
	var sb strings.Builder
	sb.WriteString(method)
	sb.WriteByte('\n')
	sb.WriteString(cp)
	sb.WriteByte('\n')
	sb.WriteString(cq)
	sb.WriteByte('\n')
	for _, n := range names {
		sb.WriteString(n)
		sb.WriteByte(':')
		sb.WriteString(nh[n])
		sb.WriteByte('\n')
	}
	sb.WriteByte('\n')
	signed := strings.Join(names, ";")
	sb.WriteString(signed)
	sb.WriteByte('\n')
	sb.WriteString(sha256Hex([]byte(bodyText)))
	return sb.String(), signed, nil
}

// SignV4 生成 SDK-HMAC-SHA256 授权头（返回需要设置的完整头集合，含 Authorization）。
// contentType 为空时不参与签名（GET 请求）。
func SignV4(ak, sk, securityToken, projectID, method, rawURL, bodyText, contentType string, extra map[string]string, t time.Time) (map[string]string, error) {
	headers := map[string]string{
		"X-Sdk-Date": sdkDate(t),
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	headers["Host"] = u.Host
	if securityToken != "" {
		headers["X-Security-Token"] = securityToken
	}
	if projectID != "" {
		headers["X-Project-ID"] = projectID
	}
	if contentType != "" {
		headers["Content-Type"] = contentType
	}
	for k, v := range extra {
		headers[k] = v
	}
	canonReq, signed, err := buildCanonicalRequest(method, rawURL, bodyText, headers)
	if err != nil {
		return nil, err
	}
	sts := "SDK-HMAC-SHA256\n" + sdkDate(t) + "\n" + sha256Hex([]byte(canonReq))
	sig := hex.EncodeToString(hmacSHA256([]byte(sk), []byte(sts)))
	headers["Authorization"] = "SDK-HMAC-SHA256 Access=" + ak + ", SignedHeaders=" + signed + ", Signature=" + sig
	return headers, nil
}

// SignV11 生成 V11-HMAC-SHA256 授权头。签名密钥派生：
//
//	step1 = HMAC-SHA256(key=AK, data=SK)                       （原始字节）
//	seed  = scope 字节 || 0x01
//	key   = hex(HMAC-SHA256(key=step1, data=seed) 的前 32 字节)
func SignV11(ak, sk, securityToken, projectID, method, rawURL, bodyText, contentType string, extra map[string]string, t time.Time) (map[string]string, error) {
	headers := map[string]string{
		"X-Sdk-Date": sdkDate(t),
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	headers["Host"] = u.Host
	if securityToken != "" {
		headers["X-Security-Token"] = securityToken
	}
	if projectID != "" {
		headers["X-Project-ID"] = projectID
	}
	if contentType != "" {
		headers["Content-Type"] = contentType
	}
	for k, v := range extra {
		headers[k] = v
	}
	canonReq, signed, err := buildCanonicalRequest(method, rawURL, bodyText, headers)
	if err != nil {
		return nil, err
	}
	sd := sdkDate(t)
	scope := sd[:8] + "/" + v11Region + "/apic"
	sts := "V11-HMAC-SHA256\n" + sd + "\n" + scope + "\n" + sha256Hex([]byte(canonReq))
	step1 := hmacSHA256([]byte(ak), []byte(sk))
	seed := append([]byte(scope), 0x01)
	keyBytes := hmacSHA256(step1, seed)
	if len(keyBytes) > 32 {
		keyBytes = keyBytes[:32]
	}
	signingKey := hex.EncodeToString(keyBytes)
	sig := hex.EncodeToString(hmacSHA256([]byte(signingKey), []byte(sts)))
	headers["Authorization"] = "V11-HMAC-SHA256 Credential=" + ak + "/" + scope + ", SignedHeaders=" + signed + ", Signature=" + sig
	return headers, nil
}
