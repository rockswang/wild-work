// captcha.go MonkeyCode 签到的**无感人机验证**（Cap.js 工作量证明）本地求解。
//
// # 为什么需要这个文件
//
// 2026-10-17 实测确认：`POST {console}/api/v1/users/wallet/checkin` 只带 Cookie 会被拒，
// 回 `{"code":10701,"message":"兑换验证码失败"}`——签到必须携带一枚由**验证码兑换**换来的
// 一次性凭证 `captcha_token`（见 checkin.go 的三步流程）。
//
// 该验证码不是图形码，而是 Cap.js（github.com/tiagozip/cap，开源 PoW CAPTCHA）的
// SHA-256 工作量证明：客户端算出来即可，**无需人工交互**，所以本工具可以自动完成。
//
// # 协议（端点实测 + Cap 服务端源码逐行对齐）
//
//	POST /api/v1/public/captcha/challenge  body={}
//	  → {"challenge":{"c":50,"s":32,"d":3},"expires":<ms>,"token":"<25 hex>"}
//	POST /api/v1/public/captcha/redeem     body={"token":…,"solutions":[…]}
//	  → {"success":true,"token":"<id>:<ver>","expires":<ms>}
//
// 解法（与 cap.js-server `redeemChallenge` 一致，注意 **i 从 1 开始**）：
//
//	for i := 1..c:
//	    salt_i   = prng(token + itoa(i),     s)
//	    target_i = prng(token + itoa(i) + "d", d)
//	    solutions[i-1] = 最小的 n ≥ 0 使得 hex(sha256(salt_i + itoa(n))) 以 target_i 开头
//
// # 数值语义（独立复核已证明等价；不要把 float64 当成"必须"）
//
// Cap 的 `fnv1a` 用 JS 的 Number 语义：`^=`/`<<` 走 ToInt32，中间的 `+=` 是双精度加法，
// 最后 `>>> 0` 才 ToUint32。**这条链全程等价于 mod 2³² 运算** —— 中间值上界约
// 6·2³¹ ≪ 2⁵³（double 不丢精度），而每一步 `^=`/`<<` 都会重新取余。
//
// ⇒ 用 uint32 回绕累加与用 float64 承载，**结果完全相同**
//
//	（2026-10-17 独立复核：穷举 5,229,043 个短串 + 30k 随机串 + 800 组种子，
//	 与 Node 侧 Cap 官方 prng 对拍 0 处不一致）。
//	故此处用 float64 **不是正确性所必需**，只是为了与 JS 源码逐字对应、便于人工对照。
//
// ⚠️ 唯一会造成差异的是**非 ASCII 种子**：JS `charCodeAt` 取 UTF-16 code unit，
//
//	而 Go `s[i]` 取 UTF-8 字节。上游 token 是 24 位 hex，无实际影响；
//	本实现只保证 ASCII seed 下与 Cap.js 逐位等同（见 captcha_test.go 的黄金向量）。
//
// 真正会算错的地方是**下标**：服务端把 `i = i + 1` 写在 Array.from 回调里，
// 首个 seed 是 `${token}1`（不是 0）。写成 0 会让整片 salt/target 错位，
// 症状是 redeem 稳定回 `{"success":false,"message":"invalid solutions"}`。
// captcha_test.go 的黄金向量把它钉死。
//
// 成本：c=50 / d=3 的期望工作量是 50 × 16³ ≈ 20 万次 SHA-256，本机实测 <400 ms。
package monkeycode

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
)

// capFNV1a 复刻 Cap.js 的 `fnv1a`（详见文件头「数值语义」）。
//
// s 只可能是 ASCII：seed 是 `<hex token><十进制 i>`，故 charCodeAt(i) == s[i]。
func capFNV1a(s string) uint32 {
	h := float64(2166136261) // JS: let hash = 2166136261（Number）
	for i := 0; i < len(s); i++ {
		// hash ^= str.charCodeAt(i)：^ 把左值夹成有符号 32 位
		hi := int32(uint32(int64(h)) ^ uint32(s[i]))
		// 五个移位都在 int32 上做，和用双精度累加（不回绕）
		t := float64(int32(uint32(hi)<<1)) + float64(int32(uint32(hi)<<4)) +
			float64(int32(uint32(hi)<<7)) + float64(int32(uint32(hi)<<8)) +
			float64(int32(uint32(hi)<<24))
		h = float64(hi) + t
	}
	return uint32(int64(h)) // JS: hash >>> 0
}

// capPRNG 复刻 Cap.js 的确定性 PRNG（FNV-1a 播种 + xorshift32，小写 hex 输出）。
//
//	state = fnv1a(seed)
//	next(): state ^= state << 13; state ^= state >>> 17; state ^= state << 5; return state >>> 0
//	每轮追加 8 位小写 hex，直到够 length 位，再截断。
func capPRNG(seed string, length int) string {
	state := int32(capFNV1a(seed))
	var b strings.Builder
	b.Grow(length + 8)
	for b.Len() < length {
		state ^= int32(uint32(state) << 13) // << 在 JS 里是 ToInt32 移位
		state ^= int32(uint32(state) >> 17) // >>> 是逻辑右移
		state ^= int32(uint32(state) << 5)
		fmt.Fprintf(&b, "%08x", uint32(state)) // >>> 0 后按 8 位小写 hex 输出
	}
	return b.String()[:length]
}

// capHexPrefixOK 报告 sum 的十六进制表示是否以 target 开头。
//
// 与 `hex.EncodeToString(sum) + strings.HasPrefix` 等价，但只按需解码前
// len(target) 个半字节——热路径上省掉整条 64 字符 hex 串的分配。
func capHexPrefixOK(sum []byte, target string) bool {
	const digits = "0123456789abcdef"
	for k := 0; k < len(target); k++ {
		nib := sum[k/2] >> 4
		if k%2 == 1 {
			nib = sum[k/2] & 0x0f
		}
		if digits[nib] != target[k] {
			return false
		}
	}
	return true
}

// capSolve 解出 Cap.js 挑战的 solutions（长度 = c，第 k 项对应 i = k+1）。
//
// 返回的切片直接作为 redeem 的 `solutions` 字段提交（数字数组）。
func capSolve(token string, c, s, d int) ([]int64, error) {
	if token == "" || c <= 0 || s <= 0 || d <= 0 {
		return nil, fmt.Errorf("monkeycode: 验证码挑战参数非法（c=%d s=%d d=%d）", c, s, d)
	}
	if d > sha256.Size*2 {
		return nil, fmt.Errorf("monkeycode: 验证码难度 %d 超出可比较的 hex 长度", d)
	}
	solutions := make([]int64, c)
	for i := 1; i <= c; i++ {
		is := strconv.Itoa(i)
		salt := capPRNG(token+is, s)
		target := capPRNG(token+is+"d", d)
		var nonce int64
		for {
			sum := sha256.Sum256([]byte(salt + strconv.FormatInt(nonce, 10)))
			if capHexPrefixOK(sum[:], target) {
				break
			}
			nonce++
		}
		solutions[i-1] = nonce
	}
	return solutions, nil
}
