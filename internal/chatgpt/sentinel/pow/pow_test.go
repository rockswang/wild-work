package pow

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// 本文件锁死「算力证明」两颗最易写错的牙齿：
//  1. FNV-1a 必须**按 Unicode 码点**迭代（参考实现 Python 的 ord(char)），
//     用 []byte 会让任何非 ASCII 输入（中文 UA / 语言 / 脚本名）静默算出另一个哈希；
//  2. 25 字段 config 的**位置与取值**（上游 sentinel SDK 逐位读它）。

// TestFNV1AIteratesByRune 用黄金值锁死哈希，并显式证明「按字节」实现在 CJK 上会给出不同结果。
func TestFNV1AIteratesByRune(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "ab3e7c0b"},
		{"abc", "1cc93dbc"},
		{"chatgpt", "fc0ed4d6"},
		{"中文测试", "fa8ef507"}, // ← 按字节迭代会得 abab16c4
		{"种子～seed", "deb87076"},
	}
	for _, c := range cases {
		if got := fnv1a32(c.in); got != c.want {
			t.Errorf("fnv1a32(%q) = %s，期望 %s", c.in, got, c.want)
		}
	}

	// 反向护栏：按 UTF-8 字节迭代的中文结果必须**不等于**真值。
	// 这个断言的意义是：一旦有人把 fnv1a32 改回 []byte 循环，上面「中文测试」用例会红，
	// 而这条会同时变绿——两者一起构成「按字符」的判据。
	if byteHash("中文测试") == fnv1a32("中文测试") {
		t.Fatal("按字节迭代与按码点迭代给出了相同结果，说明测试样例失去了区分力")
	}
	if byteHash("中文测试") != "abab16c4" {
		t.Fatalf("按字节的对照实现与预期不符：%s", byteHash("中文测试"))
	}
}

// byteHash 是**故意错误**的按字节实现（测试对照用，生产代码不得引用）。
func byteHash(s string) string {
	value := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		value ^= uint32(s[i])
		value *= 16777619
	}
	value ^= value >> 16
	value *= 2246822507
	value ^= value >> 13
	value *= 3266489909
	value ^= value >> 16
	return hex8(value)
}

func hex8(v uint32) string {
	const digits = "0123456789abcdef"
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = digits[v&0xf]
		v >>= 4
	}
	return string(b[:])
}

// TestBuildLegacyRequirementsTokenShape 锁死 p token 的形态：25 字段数组，
// 且第 4/5/6 位就是 UA / script / data-build（上游按位读，错位即静默失效）。
func TestBuildLegacyRequirementsTokenShape(t *testing.T) {
	tok := BuildLegacyRequirementsToken("UA-X", []string{"https://x/sdk.js"}, "build-9", nil)
	if !strings.HasPrefix(tok, legacyRequirements) {
		t.Fatalf("p token 前缀 = %q，期望 %q", tok[:minInt(8, len(tok))], legacyRequirements)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(tok, legacyRequirements))
	if err != nil {
		t.Fatalf("p token base64 解不开: %v", err)
	}
	var arr []any
	if err := json.Unmarshal(raw, &arr); err != nil {
		t.Fatalf("p token 不是 JSON 数组: %v", err)
	}
	if len(arr) != 25 {
		t.Fatalf("config 字段数 = %d，期望 25（上游 SDK 按位读）", len(arr))
	}
	if arr[4] != "UA-X" {
		t.Errorf("config[4] = %v，期望 UA", arr[4])
	}
	if arr[5] != "https://x/sdk.js" {
		t.Errorf("config[5] = %v，期望 script src", arr[5])
	}
	if arr[6] != "build-9" {
		t.Errorf("config[6] = %v，期望 data-build", arr[6])
	}
	// 计数器 / 耗时位：求解时被覆盖，初值必须是整数（不是字符串）。
	if _, ok := arr[3].(float64); !ok {
		t.Errorf("config[3] 计数器类型 = %T，期望数值", arr[3])
	}
	if _, ok := arr[9].(float64); !ok {
		t.Errorf("config[9] 耗时类型 = %T，期望数值", arr[9])
	}
}

// TestSolveMeetsDifficulty 证明产出的 proof 满足出题条件，且形态是 "gAAAAAB"+b64+"~S"。
func TestSolveMeetsDifficulty(t *testing.T) {
	// difficulty 为空串 ⇒ 长度 0 ⇒ 任何哈希都「小于等于」空串，零循环即可得解。
	got, ok := Solve("seed-x", "", "UA", nil)
	if !ok {
		t.Fatal("空难度应立即可解")
	}
	if !strings.HasPrefix(got, proofPrefix) || !strings.HasSuffix(got, AnswerSuffix) {
		t.Fatalf("proof 形态 = %q，期望 %q…%q", got, proofPrefix, AnswerSuffix)
	}

	// 真难度：解出的 proof 必须真的过校验。
	const seed, diff = "seed-y", "0fff"
	env := map[string]any{"screen_sum": 3840, "heap_limit": 4395630592, "language": "zh-CN",
		"languages": "zh-CN,zh", "hardware_concurrency": 16, "date_suffix": "GMT+0800 (China Standard Time)",
		"utc_offset_minutes": -480}
	proof, ok := SolveWith(seed, diff, "UA", []string{"https://x/sdk.js"}, "build-1", env, 500000)
	if !ok {
		t.Fatalf("难度 %q 在 50 万次内应可解", diff)
	}
	body := strings.TrimSuffix(strings.TrimPrefix(proof, proofPrefix), AnswerSuffix)
	h := fnv1a32(seed + body)
	if h[:len(diff)] > diff {
		t.Fatalf("proof 不满足难度：fnv=%s diff=%s", h, diff)
	}
}

// TestSolveGivesUp 不可能难度 + 极小预算应如实返回 false（不得伪造一个解）。
func TestSolveGivesUp(t *testing.T) {
	if _, ok := SolveWith("s", "00000000", "UA", nil, "", nil, 1); ok {
		t.Fatal("预算 1 次时不应声称解出了 8 位难度")
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
