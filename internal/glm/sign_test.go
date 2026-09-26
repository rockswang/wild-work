package glm

import (
	"strings"
	"testing"
	"time"
)

// TestTimestampWithChecksum 校验时间戳算法与官网 JS 逐位一致。
//
// 官网算法（chat.ts generateSign）：
//
//	A = Date.now().toString()
//	o = A.split("").map(Number)
//	i = o.reduce((e,A)=>e+A, 0) - o[t-2]     // 各位和 减去 倒数第二位
//	a = i % 10
//	timestamp = A[:t-2] + a + A[t-1:]
//
// 本测试独立复算一遍，避免实现与测试同源而失去校验意义。
func TestTimestampWithChecksum(t *testing.T) {
	cases := []string{
		"1790384203102", // 2026-09-26 前后实测样例
		"1000000000000",
		"1999999999999",
		"1700000000001",
	}
	for _, ms := range cases {
		got := timestampWithChecksum(ms)

		// 独立复算
		digits := make([]int, len(ms))
		sum := 0
		for i := 0; i < len(ms); i++ {
			digits[i] = int(ms[i] - '0')
			sum += digits[i]
		}
		checksum := (sum - digits[len(ms)-2]) % 10
		if checksum < 0 {
			checksum += 10
		}
		want := ms[:len(ms)-2] + string(rune('0'+checksum)) + ms[len(ms)-1:]

		if got != want {
			t.Errorf("timestampWithChecksum(%s) = %s, want %s", ms, got, want)
		}
		// 不变量：长度不变、首位不变、末位不变、只有倒数第二位可能变
		if len(got) != len(ms) {
			t.Errorf("长度改变: %s → %s", ms, got)
		}
		if got[0] != ms[0] || got[len(got)-1] != ms[len(ms)-1] {
			t.Errorf("首/末位被改动: %s → %s", ms, got)
		}
		if got[:len(got)-2] != ms[:len(ms)-2] {
			t.Errorf("前段被改动: %s → %s", ms, got)
		}
	}
}

// TestTimestampWithChecksumKnownValue 锁定一个具体样例的期望值，
// 防止算法被「自洽地」改错（独立复算也可能一起错）。
func TestTimestampWithChecksumKnownValue(t *testing.T) {
	// 1790384203102 各位和 = 1+7+9+0+3+8+4+2+0+3+1+0+2 = 40
	// 倒数第二位 = 0；checksum = (40-0)%10 = 0 → 该位本就是 0，结果不变
	if got := timestampWithChecksum("1790384203102"); got != "1790384203102" {
		t.Errorf("got %s, want 1790384203102", got)
	}

	// 构造一个必然改变的样例：1790384203192 → 各位和 40+9-0 = 49? 手算如下
	// 1+7+9+0+3+8+4+2+0+3+1+9+2 = 49；倒数第二位 = 9；checksum = (49-9)%10 = 0
	// → 结果 1790384203102
	if got := timestampWithChecksum("1790384203192"); got != "1790384203102" {
		t.Errorf("got %s, want 1790384203102", got)
	}
}

// TestGenerateSign 校验签名串形状与拼接顺序。
func TestGenerateSign(t *testing.T) {
	now := time.UnixMilli(1790384203102)
	s := generateSign(now)

	if len(s.Nonce) != 32 {
		t.Errorf("nonce 长度 = %d, want 32", len(s.Nonce))
	}
	if !isLowerHex(s.Nonce) {
		t.Errorf("nonce 非小写 hex: %s", s.Nonce)
	}
	if len(s.Sign) != 32 {
		t.Errorf("sign 长度 = %d, want 32", len(s.Sign))
	}
	if len(s.Timestamp) != 13 {
		t.Errorf("timestamp 长度 = %d, want 13", len(s.Timestamp))
	}
	// 签名必须等于 md5("<timestamp>-<nonce>-<secret>")
	want := md5Hex(s.Timestamp + "-" + s.Nonce + "-" + SIGN_SECRET)
	if s.Sign != want {
		t.Errorf("sign 拼接顺序不对: got %s, want %s", s.Sign, want)
	}
}

// TestEventDateUTC8 校验签到日期取 UTC+8 当天。
// 关键边界：UTC 时间 2026-09-25 16:30 = UTC+8 的 2026-09-26 00:30，必须算作 26 日。
func TestEventDateUTC8(t *testing.T) {
	cases := []struct {
		utc  time.Time
		want string
	}{
		{time.Date(2026, 9, 25, 16, 30, 0, 0, time.UTC), "2026-09-26"}, // 跨日边界
		{time.Date(2026, 9, 25, 15, 59, 59, 0, time.UTC), "2026-09-25"},
		{time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC), "2026-09-26"},
		{time.Date(2026, 9, 26, 23, 59, 59, 0, time.UTC), "2026-09-27"}, // UTC+8 已是次日
	}
	for _, c := range cases {
		if got := eventDate(c.utc); got != c.want {
			t.Errorf("eventDate(%s) = %s, want %s", c.utc.Format(time.RFC3339), got, c.want)
		}
	}
}

// TestRandomHex 校验随机串形状与长度。
func TestRandomHex(t *testing.T) {
	for _, n := range []int{8, 16, 32} {
		s := randomHex(n)
		if len(s) != n {
			t.Errorf("randomHex(%d) 长度 = %d", n, len(s))
		}
		if !isLowerHex(s) {
			t.Errorf("randomHex(%d) 非小写 hex: %s", n, s)
		}
	}
	// 两次调用不应相同
	if randomHex(32) == randomHex(32) {
		t.Error("randomHex 连续两次结果相同，随机性可疑")
	}
}

// TestStaticModelsShape 校验静态模型表与 assistant 表一致、前缀已剥离。
func TestStaticModelsShape(t *testing.T) {
	models := StaticModels()
	if len(models) == 0 {
		t.Fatal("静态模型表为空")
	}
	for _, m := range models {
		if m.ID == "" {
			t.Error("模型 ID 为空")
		}
		if strings.HasPrefix(m.ID, "glm/") {
			t.Errorf("模型 ID 未剥离 channel 前缀: %s", m.ID)
		}
	}
	// 默认模型必须在表中（2026-09-26 起带上游模型代号后缀 `:moe_53f`）
	found := false
	for _, m := range models {
		if strings.HasPrefix(m.ID, "chatglm:") {
			found = true
		}
	}
	if !found {
		t.Error("静态表缺少默认模型 chatglm:<上游代号>")
	}

	// 三个 chatglm 变体都必须带 `:moe_53f` 后缀（让用户看出上游模型）
	for _, want := range []string{"chatglm:moe_53f", "chatglm-think:moe_53f", "chatglm-deepresearch:moe_53f"} {
		ok := false
		for _, m := range models {
			if m.ID == want {
				ok = true
			}
		}
		if !ok {
			t.Errorf("静态表缺少 %s", want)
		}
	}
}

// TestResolveAssistantStripsUpstreamSuffix 锁定 `:moe_53f` 后缀的路由行为。
//
// 后缀只用于展示上游模型代号，**路由时必须剥掉**，
// 且带后缀与不带后缀的名字要解析到同一个 assistant_id + chat_mode。
func TestResolveAssistantStripsUpstreamSuffix(t *testing.T) {
	cases := []struct {
		in       string
		wantMode string
	}{
		{"chatglm:moe_53f", ""},
		{"chatglm", ""},
		{"glm/chatglm:moe_53f", ""},
		{"chatglm-think:moe_53f", "zero"},
		{"chatglm-think", "zero"},
		{"chatglm-deepresearch:moe_53f", "deep_research"},
		{"chatglm-deepresearch", "deep_research"},
	}
	for _, c := range cases {
		id, mode := resolveAssistant(c.in)
		if id != DefaultAssistantID {
			t.Errorf("resolveAssistant(%q) id = %q, want %q", c.in, id, DefaultAssistantID)
		}
		if mode != c.wantMode {
			t.Errorf("resolveAssistant(%q) mode = %q, want %q", c.in, mode, c.wantMode)
		}
	}

	// 带后缀与不带后缀必须解析一致（向后兼容的关键）
	for _, pair := range [][2]string{
		{"chatglm:moe_53f", "chatglm"},
		{"chatglm-think:moe_53f", "chatglm-think"},
		{"chatglm-deepresearch:moe_53f", "chatglm-deepresearch"},
	} {
		id1, m1 := resolveAssistant(pair[0])
		id2, m2 := resolveAssistant(pair[1])
		if id1 != id2 || m1 != m2 {
			t.Errorf("%q 与 %q 解析不一致: (%s,%s) vs (%s,%s)",
				pair[0], pair[1], id1, m1, id2, m2)
		}
	}
}

// TestUpstreamModelMapping 校验「客户端模型名 → 上游代号」的映射。
func TestUpstreamModelMapping(t *testing.T) {
	cases := map[string]string{
		"glm/chatglm:moe_53f":              "moe_53f",
		"chatglm:moe_53f":                  "moe_53f",
		"chatglm":                          "moe_53f", // 旧名回填
		"chatglm-think":                    "moe_53f",
		"glm/chatglm-deepresearch:moe_53f": "moe_53f",
		"glm/search":                       "ai-search",
		"search":                           "ai-search",
		"glm/ppt":                          "all-tools-glms-glms-v2",
		"video":                            "all-tools-glms-glms-v2",
		"unknown-model":                    "",
	}
	for in, want := range cases {
		if got := UpstreamModel(in); got != want {
			t.Errorf("UpstreamModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
