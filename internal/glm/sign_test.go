package glm

import (
	"strings"
	"testing"
	"time"
)

// TestGenerateSignTimestampRaw 校验 X-Timestamp 用原始毫秒值（13 位纯数字）。
// 旧实现把倒数第二位替换成校验位——PROTOCOL §2 已证该算法对签名结果无影响，
// IDE 通道迁移时已删（chatglm-cli 实抓全部请求都是原始毫秒）。
func TestGenerateSignTimestampRaw(t *testing.T) {
	now := time.UnixMilli(1790384203102)
	s := generateSign(now)
	if s.Timestamp != "1790384203102" {
		t.Errorf("timestamp = %s, want 原始毫秒 1790384203102（不得改写任何位）", s.Timestamp)
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

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
