package monkeycode

// captcha_test.go Cap.js 工作量证明求解器的**黄金向量**回归。
//
// 向量的来源是权威侧的**参考实现**，不是本项目自造的期望值：
// 用 Node 跑 Cap 官方发布的 `@cap.js/server` / `capjs-core` 里的 `prng.js`，
// 对固定 token 输出 salt / target / solutions（生成脚本见本任务取证记录）。
// 因此这组用例锁定的是「与 Cap.js 逐字节一致」，而不是「和本文件里的常量一致」。
//
// 这是本渠道最关键的一条测试：Geo 侧一旦把 JS 的双精度语义写成 uint32 回绕，
// salt 会整片错位、redeem 稳定回 `invalid solutions`，而**面板只会显示签到失败**，
// 从症状根本看不出根因。这条测试把它变成编译期就能发现的回归。

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// capGolden 是 Node 侧 Cap 官方 prng.js 生成的向量（token → salt/target/solutions）。
var capGolden = []struct {
	token     string
	c, s, d   int
	salts     []string
	targets   []string
	solutions []int64
}{
	{
		token: "abc", c: 4, s: 32, d: 3,
		salts: []string{
			"a240feb00f42af1b0e9dab2bc4ea4c37",
			"88ca17aa8dd964158a2903b8e1948477",
			"79747a8032a433d22f1b1e1dd7cb9651",
			"65c1f9750701f44208bcb8466932c41c",
		},
		targets:   []string{"e6e", "b68", "01e", "198"},
		solutions: []int64{11775, 6971, 310, 4613},
	},
	{
		token: "00112233445566778899aabbc", c: 4, s: 32, d: 3,
		salts: []string{
			"b80c4c130048ae54a5c6b6351305d915",
			"42c1ff0ed920325e3218208bf7222a6f",
			"6a43c75a47b94fcea657a52e0cbbc6b7",
			"fb8b39d60a87464eab901a49b3f4aa85",
		},
		targets:   []string{"0b5", "b25", "731", "4a0"},
		solutions: []int64{862, 2442, 3586, 1854},
	},
	{
		token: "zzz123", c: 4, s: 32, d: 3,
		salts: []string{
			"e29d19037616627df16a7571474558f3",
			"13ba30afe050b57829060ee7f389cbca",
			"0ab99c3b1ee2f4244cacd6f71839e40e",
			"bb13314e7a6965b38d3af55ca1b28e14",
		},
		targets:   []string{"a0e", "4f8", "804", "dfe"},
		solutions: []int64{345, 6144, 421, 4852},
	},
	{
		token: "tok-0", c: 4, s: 32, d: 3,
		salts: []string{
			"493fa73f06db23b337032e450bbf4320",
			"30b9efc3a570a5038bc3208b1594db22",
			"1f326c4705ee065a6181bdc8912a4c54",
			"1db2a13b7b295bd15a392cf88b64764b",
		},
		targets:   []string{"30f", "553", "8dd", "f53"},
		solutions: []int64{11647, 1866, 2349, 2645},
	},
}

// TestCapPRNGMatchesCapJS 锁定 prng 的输出与 Cap.js 完全一致（salt 与 target 两族 seed）。
func TestCapPRNGMatchesCapJS(t *testing.T) {
	for _, g := range capGolden {
		for idx := range g.salts {
			i := idx + 1 // Cap 服务端的下标从 1 开始
			iStr := itoaForTest(i)
			if got := capPRNG(g.token+iStr, g.s); got != g.salts[idx] {
				t.Errorf("salt[%d] token=%q: got %s want %s", idx, g.token, got, g.salts[idx])
			}
			if got := capPRNG(g.token+iStr+"d", g.d); got != g.targets[idx] {
				t.Errorf("target[%d] token=%q: got %s want %s", idx, g.token, got, g.targets[idx])
			}
		}
	}
}

// TestCapSolveMatchesCapJS 锁定求解结果与 Cap.js 一致（最小 nonce，逐项相等）。
func TestCapSolveMatchesCapJS(t *testing.T) {
	for _, g := range capGolden {
		got, err := capSolve(g.token, g.c, g.s, g.d)
		if err != nil {
			t.Fatalf("capSolve(%q) 出错: %v", g.token, err)
		}
		if len(got) != len(g.solutions) {
			t.Fatalf("capSolve(%q) 长度 %d，期望 %d", g.token, len(got), len(g.solutions))
		}
		for i := range got {
			if got[i] != g.solutions[i] {
				t.Errorf("capSolve(%q)[%d] = %d，期望 %d", g.token, i, got[i], g.solutions[i])
			}
		}
	}
}

// TestCapSolutionsSatisfyPoW 自洽校验：解出的 nonce 必须真的满足前缀条件。
//
// 与黄金向量互补——向量证明「和 Cap.js 一样」，这条证明「解本身是对的」，
// 即使将来换 token、换难度也不会失效。
func TestCapSolutionsSatisfyPoW(t *testing.T) {
	const token, c, s, d = "abcdef0123456789", 6, 32, 3
	sols, err := capSolve(token, c, s, d)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= c; i++ {
		salt := capPRNG(token+itoaForTest(i), s)
		target := capPRNG(token+itoaForTest(i)+"d", d)
		if len(target) != d {
			t.Fatalf("target 长度 %d，期望 %d", len(target), d)
		}
		sum := sha256.Sum256([]byte(salt + itoa64ForTest(sols[i-1])))
		if !capHexPrefixOK(sum[:], target) {
			t.Errorf("第 %d 项不满足 PoW：hex=%s target=%s", i, hex.EncodeToString(sum[:]), target)
		}
		// 同时确认「最小」：比它小的 nonce 都不满足（抽查 32 个，避免 O(n) 慢测）
		for n := sols[i-1] - 1; n >= 0 && n > sols[i-1]-32; n-- {
			s2 := sha256.Sum256([]byte(salt + itoa64ForTest(n)))
			if capHexPrefixOK(s2[:], target) {
				t.Errorf("第 %d 项不是最小解：nonce %d 也满足", i, n)
				break
			}
		}
	}
}

// TestCapHexPrefixOK 边界：奇偶长度前缀都要正确比较。
func TestCapHexPrefixOK(t *testing.T) {
	sum := []byte{0x0a, 0xbc, 0xde}
	cases := map[string]bool{
		"0":      true,
		"0a":     true,
		"0ab":    true,
		"0abc":   true,
		"0abcd":  true,
		"0abcde": true,
		"a":      false,
		"0b":     false,
		"0ac":    false,
		"0abx":   false, // 非 hex 字符也必须判否，不得越界
	}
	for prefix, want := range cases {
		if got := capHexPrefixOK(sum, prefix); got != want {
			t.Errorf("capHexPrefixOK(%x, %q) = %v，期望 %v", sum, prefix, got, want)
		}
	}
}

// TestCapSolveRejectsBadParams 参数非法时不得去算（更不能死循环）。
func TestCapSolveRejectsBadParams(t *testing.T) {
	for _, tc := range []struct {
		token   string
		c, s, d int
	}{
		{"", 1, 32, 3},
		{"t", 0, 32, 3},
		{"t", 1, 0, 3},
		{"t", 1, 32, 0},
		{"t", 1, 32, 65},
	} {
		if _, err := capSolve(tc.token, tc.c, tc.s, tc.d); err == nil {
			t.Errorf("capSolve(%q,%d,%d,%d) 期望报错，实际成功", tc.token, tc.c, tc.s, tc.d)
		}
	}
}

// 小工具：避免为了拼 i 引入额外 import 噪音。
func itoaForTest(i int) string { return itoa64ForTest(int64(i)) }
func itoa64ForTest(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	p := len(buf)
	for n > 0 {
		p--
		buf[p] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[p:])
}
