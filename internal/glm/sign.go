// sign.go 智谱清言私有接口的签名与请求头构造。
//
// 清言网页版所有私有接口都要求一组签名头，缺一即 400 bad request(40001)：
//
//	X-Timestamp  毫秒时间戳，但倒数第二位被替换成校验位
//	X-Nonce      32 位随机 hex
//	X-Sign       md5("<timestamp>-<nonce>-<SIGN_SECRET>")
//	X-Device-Id  设备标识（参考实现每请求随机，实测可行）
//	X-Request-Id 请求标识
//
// 时间戳算法（复刻官网 JS，勿改）：
//
//	取 Date.now() 的十进制字符串 A，令 t = len(A)；
//	校验位 = (A 各位数字之和 - A[t-2]) % 10；
//	结果 = A[:t-2] + 校验位 + A[t-1:]
//
// 即把倒数第二位数字替换为校验位，最后一位保持不变。
package glm

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Sign 一次签名所需的三个值。
type Sign struct {
	Timestamp string
	Nonce     string
	Sign      string
}

// generateSign 按官网算法生成签名。now 由调用方传入以便测试。
func generateSign(now time.Time) Sign {
	ms := strconv.FormatInt(now.UnixMilli(), 10)
	ts := timestampWithChecksum(ms)
	nonce := randomHex(32)
	return Sign{
		Timestamp: ts,
		Nonce:     nonce,
		Sign:      md5Hex(ts + "-" + nonce + "-" + SIGN_SECRET),
	}
}

// timestampWithChecksum 复刻官网时间戳算法：把倒数第二位替换为校验位。
// 输入短于 2 位时原样返回（实际不会发生，毫秒时间戳恒为 13 位）。
func timestampWithChecksum(ms string) string {
	n := len(ms)
	if n < 2 {
		return ms
	}
	sum := 0
	for i := 0; i < n; i++ {
		c := ms[i]
		if c < '0' || c > '9' {
			return ms // 非纯数字，放弃改写（防御性）
		}
		sum += int(c - '0')
	}
	second := int(ms[n-2] - '0')
	checksum := (sum - second) % 10
	if checksum < 0 {
		checksum += 10
	}
	return ms[:n-2] + strconv.Itoa(checksum) + ms[n-1:]
}

// md5Hex 返回小写 hex 的 MD5。
func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// randomHex 返回 n 位小写 hex 随机串（n 为偶数时恰好 n/2 字节）。
// 随机源失败时退回时间派生值，保证调用方永远拿得到非空串。
func randomHex(n int) string {
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		fallback := md5.Sum([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		return hex.EncodeToString(fallback[:])[:n]
	}
	return hex.EncodeToString(buf)[:n]
}

// applyCommonHeaders 注入清言私有接口要求的伪装头。
// 与参考实现（GLM-Free-API / Chat2API）的 FAKE_HEADERS 对齐：
// 缺 Origin/Referer/App-Name 会被网关判为异常客户端。
func applyCommonHeaders(req *http.Request, accept string) {
	h := req.Header
	h.Set("Content-Type", "application/json")
	if accept == "" {
		accept = "application/json, text/plain, */*"
	}
	h.Set("Accept", accept)
	h.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	h.Set("App-Name", "chatglm")
	h.Set("Cache-Control", "no-cache")
	h.Set("Pragma", "no-cache")
	h.Set("Origin", Origin)
	h.Set("Referer", Origin+"/main/alltoolsdetail")
	h.Set("User-Agent", userAgent)
	h.Set("X-App-Fr", "browser_extension")
	h.Set("X-App-Platform", "pc")
	h.Set("X-App-Version", "0.0.1")
	h.Set("X-Device-Brand", "")
	h.Set("X-Device-Model", "")
	h.Set("X-Lang", "zh")
	// Edge 143 的客户端提示头，与 UA 保持一致。
	h.Set("Sec-Ch-Ua", `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`)
	h.Set("Sec-Ch-Ua-Mobile", "?0")
	h.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	h.Set("Sec-Fetch-Dest", "empty")
	h.Set("Sec-Fetch-Mode", "cors")
	h.Set("Sec-Fetch-Site", "same-origin")
	h.Set("Priority", "u=1, i")
}

// applySignedHeaders 注入签名头。token 非空时同时带 Authorization。
// accept 传 SSE 时表示对话流接口（需 text/event-stream）。
func applySignedHeaders(req *http.Request, token, accept string) {
	applyCommonHeaders(req, accept)
	s := generateSign(time.Now())
	req.Header.Set("X-Device-Id", randomHex(32))
	req.Header.Set("X-Request-Id", randomHex(32))
	req.Header.Set("X-Nonce", s.Nonce)
	req.Header.Set("X-Sign", s.Sign)
	req.Header.Set("X-Timestamp", s.Timestamp)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// eventDate 返回清言签到接口要求的 event_date（UTC+8 当天，YYYY-MM-DD）。
// 对应官网 JS：
//
//	new Date(Date.now() - 6e4*new Date().getTimezoneOffset()).toJSON().slice(0,10)
//
// 该表达式把本地时间平移到 UTC，再取日期——对 UTC+8 机器即本地当天。
// 本工具全渠道统一用 UTC+8 墙钟（见 provider.expireLoc），故直接取 UTC+8 日期。
func eventDate(now time.Time) string {
	return now.In(utc8).Format("2006-01-02")
}
