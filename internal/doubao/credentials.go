package doubao

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"strings"
)

// 设备指纹参数的**合法格式**（2026-11-15 按 wangchuxiaoji-oss/doubao2api 的
// generate_device_params 对齐）。
//
// 为什么卡这么细：上游对 device_id / web_id / fp 的形态有校验，形状不对直接回
// `710020202 common invalid param`（本仓首版对话实测：用 sha256 十六进制当
// device_id、fp 只拼了 verify_<16hex>，立刻被拒）。三个字段的形态：
//
//	device_id  16 位十进制数字，首位 7
//	web_id     19 位十进制数字，首位 76
//	fp         "verify_" + 五段 alnum（8/4/4/4/12）用下划线连接
const (
	deviceIDDigits = 16
	webIDDigits    = 19
)

// ---------- 凭据承载 ----------
//
// 豆包没有 token，只有 Cookie。存放位置（auth.Auth.Extra，不变量：只增不减）：
//
//	cookie     完整 Cookie 请求头串（必须含 sessionid）
//	device_id  稳定派生的设备号（扫码登录时生成；缺省时由 cookie 派生）
//	web_id     可选；缺省则不发送该参数
//
// UID 由 sessionid 派生（见 UIDOfCookie），保证同一账号多次登录落到同一个号上。

const (
	// ExtraCookie 完整 Cookie 串的键。
	ExtraCookie = "cookie"
	// ExtraDeviceID 设备号的键。
	ExtraDeviceID = "device_id"
	// ExtraWebID web_id 的键（可选）。
	ExtraWebID = "web_id"
	// ExtraFP fp 设备指纹的键（可选）。
	ExtraFP = "fp"
)

// CookieOf 取完整 Cookie 串。
func CookieOf(cookie string) string { return strings.TrimSpace(cookie) }

// SessionIDOf 从 Cookie 串里取 sessionid 的值（登录完成的判据，参考实现同款）。
func SessionIDOf(cookie string) string {
	for _, part := range strings.Split(cookie, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if k, v, ok := strings.Cut(part, "="); ok && strings.TrimSpace(k) == "sessionid" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// UIDOfCookie 由 Cookie 派生稳定 uid。
//
// 为什么用 sessionid 而不是整串 Cookie：sessionid 是账号级长期凭据，
// 其它 Cookie（msToken/s_v_web_id 等）每次登录都变；用整串会让同一账号
// 每次扫码都生成一个新号。sessionid 变了（重新登录）才会换 uid —— 这种
// 情况下旧卡片需手工删除，与 WPS 灵犀重登的语义一致。
func UIDOfCookie(cookie string) string {
	sid := SessionIDOf(cookie)
	if sid == "" {
		sid = strings.TrimSpace(cookie)
	}
	sum := sha256.Sum256([]byte(sid))
	return "doubao-" + hex.EncodeToString(sum[:])[:16]
}

// DeviceIDOf 取设备号；缺省时由 Cookie 稳定派生（参考实现 shortHash(cookie) 同思路）。
//
// 为什么必须稳定：device_id 同时进入 device_id / fp / tea_uuid 三个参数，
// 每次请求都变会让上游看到「同一账号无限多的新设备」。
func DeviceIDOf(cookie, stored string) string {
	if d := strings.TrimSpace(stored); d != "" {
		return d
	}
	sid := SessionIDOf(cookie)
	if sid == "" {
		sid = strings.TrimSpace(cookie)
	}
	sum := sha256.Sum256([]byte("doubao-dev:" + sid))
	return hex.EncodeToString(sum[:])[:16]
}

// NormalizeCookie 容忍三种粘贴形态：裸值 / `k=v; k=v` 串 / 整段 Cookie 请求头。
//
// 与 WPS Comate 的 NormalizeSID 同款思路（R131）：用户从 DevTools 复制到的
// 常常是整行，直接当请求头值用即可，不需要用户自己清洗。
func NormalizeCookie(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "Cookie:")
	s = strings.TrimSpace(s)
	// 去掉首尾引号（从表格复制时常见）
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	// 折叠换行/制表符为空格后按 ; 重新规范
	s = strings.NewReplacer("\r\n", "; ", "\n", "; ", "\t", "; ").Replace(s)
	parts := make([]string, 0, 8)
	for _, p := range strings.Split(s, ";") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "; ")
}

// GenerateDeviceParams 生成一套**随机**设备指纹（扫码登录时用，每次登录换新身份，
// 避免继承上一次会话的风控分——与参考实现同思路）。
func GenerateDeviceParams() (deviceID, webID, fp string) {
	deviceID = randDigits(deviceIDDigits, "7")
	webID = randDigits(webIDDigits, "76")
	fp = "verify_" + randAlnum(8) + "_" + randAlnum(4) + "_" + randAlnum(4) +
		"_" + randAlnum(4) + "_" + randAlnum(12)
	return deviceID, webID, fp
}

// DeviceParamsOf 取设备指纹；凭据里缺（老账号）时按 Cookie **稳定派生**一套合法格式。
//
// 为什么派生也要合法：形状不合法上游直接拒（710020202），而老账号没有这三个字段，
// 不能因此把它们全都打不出去；稳定派生保证同一账号每次请求是同一套设备。
func DeviceParamsOf(cookie, storedDevice, storedWeb, storedFp string) (deviceID, webID, fp string) {
	deviceID = strings.TrimSpace(storedDevice)
	webID = strings.TrimSpace(storedWeb)
	fp = strings.TrimSpace(storedFp)
	if validDeviceID(deviceID) && validWebID(webID) && validFP(fp) {
		return deviceID, webID, fp
	}
	// 有一个不合法就整套重新派生（混用新旧更容易被判异常）。
	seed := sha256.Sum256([]byte("doubao-device:" + SessionIDOf(cookie)))
	d := digitsFromSeed(seed[:], deviceIDDigits)
	d = "7" + d[1:]
	w := digitsFromSeed(seed[:], webIDDigits)
	w = "76" + w[2:]
	f := "verify_" + alnumFromSeed(seed[:], 8) + "_" + alnumFromSeed(seed[8:], 4) +
		"_" + alnumFromSeed(seed[12:], 4) + "_" + alnumFromSeed(seed[16:], 4) +
		"_" + alnumFromSeed(seed[20:], 12)
	return d, w, f
}

func validDeviceID(s string) bool {
	return len(s) == deviceIDDigits && allDigits(s)
}

func validWebID(s string) bool {
	return len(s) == webIDDigits && allDigits(s)
}

func validFP(s string) bool {
	if !strings.HasPrefix(s, "verify_") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(s, "verify_"), "_")
	if len(parts) != 5 {
		return false
	}
	want := []int{8, 4, 4, 4, 12}
	for i, p := range parts {
		if len(p) != want[i] {
			return false
		}
		for _, c := range p {
			if !isAlnum(c) {
				return false
			}
		}
	}
	return true
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isAlnum(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// randDigits 生成指定位数的十进制数字串，首位固定为 prefix。
// crypto/rand 失败（极低概率）时用 '0' 填满保证形状合法（设备指纹形状不对上游
// 会回 710020202），绝不 panic / 返回短串。
func randDigits(n int, prefix string) string {
	out := make([]byte, 0, n)
	out = append(out, prefix...)
	for len(out) < n {
		v, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			break
		}
		out = append(out, byte('0'+v.Int64()))
	}
	for len(out) < n {
		out = append(out, '0')
	}
	return string(out[:n])
}

// randAlnum 生成指定位数的字母数字串。
func randAlnum(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		v, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			break
		}
		out = append(out, chars[v.Int64()])
	}
	return string(out)
}

// digitsFromSeed 由种子确定性地推出 n 位十进制数字。
func digitsFromSeed(seed []byte, n int) string {
	out := make([]byte, 0, n)
	for i := 0; len(out) < n; i++ {
		b := seed[int(i)%len(seed)]
		out = append(out, byte('0'+int(b)%10))
	}
	return string(out)
}

// alnumFromSeed 由种子确定性地推出 n 位字母数字。
func alnumFromSeed(seed []byte, n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	out := make([]byte, 0, n)
	for i := 0; len(out) < n; i++ {
		b := seed[int(i)%len(seed)]
		out = append(out, chars[int(b)%len(chars)])
	}
	return string(out)
}
