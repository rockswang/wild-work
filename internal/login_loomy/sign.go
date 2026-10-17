package login_loomy

import (
	"crypto/hmac"
	"crypto/md5"  //nolint:gosec // 讯飞 CAccount 协议要求 Content-MD5，非安全用途
	"crypto/sha1" //nolint:gosec // 讯飞 CAccount 协议固定用 HMAC-SHA1，不是本工具的选择
	"encoding/base64"
	"strings"
)

// 本文件逐段对应参考实现 `dsh-our-free-model/vendor/channel-pack/src/loomy-sign.ts`
// （它本身又复刻 Loomy 客户端 `electron/xfyun/sign.js`）。
//
// 为什么必须逐字节对齐：**任何偏差都会让账号端点返回鉴权失败**，
// 而账号端点正是「浏览器登录」唯一可编程的入口。故这里全部是纯函数，
// 由单测用**手算的**签名串锁死拼串格式（见 TestSignMatchesReferenceVector）。

// contentMD5 计算 `Content-MD5`（base64）。
//
// ⚠️ 空 body 返回**空串**而不是「空串的 md5」——与客户端 sign.js:13-18 一致。
func contentMD5(body string) string {
	if body == "" {
		return ""
	}
	sum := md5.Sum([]byte(body)) //nolint:gosec // 协议要求，非安全用途
	return base64.StdEncoding.EncodeToString(sum[:])
}

// escapeRFC3986 是 `encodeURIComponent` + 补转 `! ' ( ) *`。
//
// 依据 sign.js:23-42：路径段与 query 值用同一套转义。
// Go 的 url.QueryEscape 把空格转成 `+`、且不转义 `*`，**不能**直接用。
func escapeRFC3986(value string) string {
	// 注意：这里刻意不用 url.PathEscape —— 它对 `+`/`$`/`&` 等保留字符的
	// 处理与 encodeURIComponent 不同（例如它保留 `+`、`$`、`&`），
	// 会让签名与客户端不一致。故手写逐字节转义，与 JS 的 encodeURIComponent 等价。
	const upperHex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		// encodeURIComponent 不转义的字符集（未保留字符 + JS 特例）。
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' ||
			c == '!' || c == '\'' || c == '(' || c == ')' || c == '*' {
			// 后 5 个在 encodeURIComponent 里不转义，但本协议**要求**再转一次
			// （sign.js 的 .replace 链）。故这里不写进保留集。
			if c == '!' || c == '\'' || c == '(' || c == ')' || c == '*' {
				b.WriteByte('%')
				b.WriteByte(upperHex[c>>4])
				b.WriteByte(upperHex[c&0x0f])
				continue
			}
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperHex[c>>4])
		b.WriteByte(upperHex[c&0x0f])
	}
	return b.String()
}

// escapedPath 构建 ESCAPED_PATH。
//
// 依据 sign.js:47-58：补前导 `/`、剥末尾 `/`（长度 > 1 时）、
// 按 `/` 切段后逐段转义再拼回（**段内的 `/` 因此不会被转义**）。
func escapedPath(rawPath string) string {
	clean := rawPath
	if !strings.HasPrefix(clean, "/") {
		clean = "/" + clean
	}
	if len(clean) > 1 && strings.HasSuffix(clean, "/") {
		clean = clean[:len(clean)-1]
	}
	segs := strings.Split(clean, "/")
	for i, s := range segs {
		if s != "" {
			segs[i] = escapeRFC3986(s)
		}
	}
	return strings.Join(segs, "/")
}

// escapedQueryString 构建 ESCAPED_QUERY_STRING。
//
// 依据 sign.js:63-79：`key=value` 用 `&` 连接、**不排序**（保持传入顺序），
// 两者都转义。本期所有账号端点都无 query，故传空串即可。
func escapedQueryString(query string) string {
	query = strings.TrimPrefix(query, "?")
	if query == "" {
		return ""
	}
	pairs := strings.Split(query, "&")
	for i, p := range pairs {
		k, v, _ := strings.Cut(p, "=")
		pairs[i] = escapeRFC3986(k) + "=" + escapeRFC3986(v)
	}
	return strings.Join(pairs, "&")
}

// buildSigningString 构造待签名串（9 段，`\n` 连接）。
//
// 后两段（SignedHeaders / CanonicalizedHeaders）在本项目**恒为空串**
// （我们不发任何 `x-*` 头），故最终字符串**以两个换行结尾**。
// 这是对 9 个元素 join 的自然结果，**不要**"顺手"去掉尾随换行 —— 去掉会让签名不匹配
// （sign.js:14-17 的原始告警）。
//
// date / nonce 由调用方传入（而非在此生成），使本函数**完全确定**、可用固定值单测。
func buildSigningString(method, path, query, bodyMD5, contentType, date, nonce string) string {
	return strings.Join([]string{
		strings.ToUpper(method),
		escapedPath(path),
		escapedQueryString(query),
		bodyMD5,
		contentType,
		date,
		nonce,
		"", // SignedHeaders —— 恒空
		"", // CanonicalizedHeaders —— 恒空
	}, "\n")
}

// authHeaders 生成完整的讯飞账号请求头。
//
// 认证头前缀是 **`account`**（`account {ak}:{sig}`），不是 `Bearer`。
//
// ⚠️ 返回的 body 必须与签名时用的是**同一个字符串**：调用方应先把 body
// 序列化一次，签名与发送共用该串，否则二次序列化会改变字节（键序/空格），签名随即失效。
// 本包的做法见 postAccount：一次 json.Marshal → 同一 []byte 既签名又发送。
func authHeaders(method, path, query, body, contentType, date, nonce string) map[string]string {
	bodyMD5 := contentMD5(body)
	stringToSign := buildSigningString(method, path, query, bodyMD5, contentType, date, nonce)
	mac := hmac.New(sha1.New, []byte(accessKeySecret)) //nolint:gosec // 协议固定 SHA1
	mac.Write([]byte(stringToSign))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	h := map[string]string{
		"Authorization": "account " + accessKeyID + ":" + sig,
		"Date":          date,
		"Nonce":         nonce,
		"Content-Type":  contentType,
	}
	if bodyMD5 != "" {
		h["Content-MD5"] = bodyMD5
	}
	return h
}
