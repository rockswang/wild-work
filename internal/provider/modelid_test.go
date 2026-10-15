package provider

import "testing"

// 本文件锁死「对外模型 ID ↔ 路由解析」的唯一判据（决议 R59）。
//
// 背景：cline 免费档的上游 ID 自带命名空间 `cline-free/*`（上游 free[] 原样透传，R46），
// 用户裁定对外**直接使用上游 ID**，不再拼 `<渠道>/` 前缀；同时旧的
// `cline/cline-free/*` 双层写法必须继续可路由（老版本面板/配置/降级链里都是那种写法）。
// 这些规则只允许在此一处定义、由 provider.PublicModelID / SplitModelID 导出，
// 避免再次出现「面板一个判据、服务端另一个判据」（R52 的教训：拼接规则散在多处必漏）。

func TestPublicModelID(t *testing.T) {
	cases := []struct {
		kind Kind
		up   string
		want string
	}{
		// cline：上游 ID 自带命名空间 ⇒ 原样透传（不加渠道前缀）
		{Cline, "cline-free/mimo-v2.6-flash", "cline-free/mimo-v2.6-flash"},
		{Cline, "cline-free/muse-spark-1.3-contributor", "cline-free/muse-spark-1.3-contributor"},
		// cline：上游 ID 不带命名空间时不冒充直通（正常补前缀，路由仍可解析）
		{Cline, "plain-model", "cline/plain-model"},
		// 其它渠道恒为 <kind>/<上游ID>
		{WorkBuddy, "glm-5.2", "workbuddy/glm-5.2"},
		{WorkBuddyAI, "deepseek-v4.1-flash", "workbuddyai/deepseek-v4.1-flash"},
		{QoderCN, "qwen3.8-flash", "qodercn/qwen3.8-flash"},
	}
	for _, c := range cases {
		if got := PublicModelID(c.kind, c.up); got != c.want {
			t.Errorf("PublicModelID(%s, %q) = %q; want %q", c.kind, c.up, got, c.want)
		}
	}
}

func TestSplitModelID(t *testing.T) {
	cases := []struct {
		in       string
		kind     Kind
		upstream string
		ok       bool
	}{
		// 新形态：上游命名空间直通，整串就是发给上游的模型名
		{"cline-free/mimo-v2.6-flash", Cline, "cline-free/mimo-v2.6-flash", true},
		{"cline-free/muse-spark-1.3-contributor", Cline, "cline-free/muse-spark-1.3-contributor", true},
		// 旧形态：渠道前缀 + 上游命名空间，剥掉一层
		{"cline/cline-free/mimo-v2.6-flash", Cline, "cline-free/mimo-v2.6-flash", true},
		{"cline/plain-model", Cline, "plain-model", true},
		// 普通渠道
		{"workbuddy/glm-5.2", WorkBuddy, "glm-5.2", true},
		{"qodercn/qwen3.8-flash", QoderCN, "qwen3.8-flash", true},
		// 非法：无分隔、首段空、尾段空
		{"glm-5.2", "", "", false},
		{"/x", "", "", false},
		{"cline-free/", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		k, up, ok := SplitModelID(c.in)
		if k != c.kind || up != c.upstream || ok != c.ok {
			t.Errorf("SplitModelID(%q) = (%q, %q, %v); want (%q, %q, %v)",
				c.in, k, up, ok, c.kind, c.upstream, c.ok)
		}
	}
}

// 新旧两种写法必须归一到同一串：否则同一模型会在用量榜/请求日志里裂成两行。
func TestCanonicalAndUpstreamNormalization(t *testing.T) {
	for _, in := range []string{"cline-free/mimo-v2.6-flash", "cline/cline-free/mimo-v2.6-flash"} {
		if got := UpstreamModelOf(in); got != "cline-free/mimo-v2.6-flash" {
			t.Errorf("UpstreamModelOf(%q) = %q; want cline-free/mimo-v2.6-flash", in, got)
		}
		if got := CanonicalModelID(in); got != "cline-free/mimo-v2.6-flash" {
			t.Errorf("CanonicalModelID(%q) = %q; want cline-free/mimo-v2.6-flash", in, got)
		}
	}
	// 普通渠道：归一为原串（含渠道前缀）与裸上游名
	if got := CanonicalModelID("workbuddy/glm-5.2"); got != "workbuddy/glm-5.2" {
		t.Errorf("CanonicalModelID = %q", got)
	}
	if got := UpstreamModelOf("workbuddy/glm-5.2"); got != "glm-5.2" {
		t.Errorf("UpstreamModelOf = %q", got)
	}
	// 裸名（compat 层才做映射）原样返回，不做猜测
	if got := CanonicalModelID("glm-5.2"); got != "glm-5.2" {
		t.Errorf("裸名应原样返回，得到 %q", got)
	}
	if got := UpstreamModelOf("glm-5.2"); got != "glm-5.2" {
		t.Errorf("裸名应原样返回，得到 %q", got)
	}
}

// ModelIDPrefix 供 compat 层 default_channel 兜底拼裸名：cline 必须拼命名空间前缀，
// 否则裸名会被拼成 cline/<name> 而 cline 上游只认 cline-free/<name>。
func TestModelIDPrefix(t *testing.T) {
	if got := ModelIDPrefix(Cline); got != "cline-free" {
		t.Errorf("cline 的对外前缀应为 cline-free（上游命名空间），得 %q", got)
	}
	if got := ModelIDPrefix(WorkBuddy); got != "workbuddy" {
		t.Errorf("普通渠道前缀应为其 kind，得 %q", got)
	}
}
