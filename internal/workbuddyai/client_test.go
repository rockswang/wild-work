package workbuddyai

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestPrepareBodyForcesStream 上游拒绝非流式请求（实测 code=11101）。
func TestPrepareBodyForcesStream(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"hy3","stream":false,"messages":[{"role":"system","content":"s"}]}`))
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if m["stream"] != true {
		t.Fatalf("stream not forced true: %v", m["stream"])
	}
}

// TestPrepareBodyDeveloperToSystem developer 角色被上游拒为 11128。
func TestPrepareBodyDeveloperToSystem(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"hy3","messages":[{"role":"developer","content":"s"},{"role":"user","content":"u"}]}`))
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	msgs := m["messages"].([]any)
	if r := msgs[0].(map[string]any)["role"]; r != "system" {
		t.Fatalf("developer not rewritten: %v", r)
	}
}

// TestPrepareBodyInsertsLeadingSystem 上游要求首条为 system（实测 11128）。
func TestPrepareBodyInsertsLeadingSystem(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"hy3","messages":[{"role":"user","content":"hi"}]}`))
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	msgs := m["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	if r := msgs[0].(map[string]any)["role"]; r != "system" {
		t.Fatalf("leading system not inserted, got %v", r)
	}
	if r := msgs[1].(map[string]any)["role"]; r != "user" {
		t.Fatalf("original message order broken: %v", r)
	}
}

// TestPrepareBodyKeepsExistingSystem 已有 system 首条时不应重复插入。
func TestPrepareBodyKeepsExistingSystem(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"hy3","messages":[{"role":"system","content":"s"},{"role":"user","content":"u"}]}`))
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if n := len(m["messages"].([]any)); n != 2 {
		t.Fatalf("want 2 messages, got %d", n)
	}
}

// TestPrepareBodyEmptyMessages messages 为空时不干预（交由上游报错）。
func TestPrepareBodyEmptyMessages(t *testing.T) {
	src := `{"model":"hy3","messages":[]}`
	out := PrepareBody([]byte(src))
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if n := len(m["messages"].([]any)); n != 0 {
		t.Fatalf("empty messages should stay empty, got %d", n)
	}
}

// TestNormalizeToolChoiceFunctionObject 对象形式必须转为字符串名（实测 11101）。
func TestNormalizeToolChoiceFunctionObject(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"hy3","tool_choice":{"type":"function","function":{"name":"get_weather"}},"messages":[{"role":"system","content":"s"}]}`))
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if m["tool_choice"] != "get_weather" {
		t.Fatalf("tool_choice not flattened: %v", m["tool_choice"])
	}
}

// TestNormalizeToolChoiceNone none 应删除 tool_choice 与 tools。
func TestNormalizeToolChoiceNone(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"hy3","tool_choice":"none","tools":[{"x":1}],"messages":[{"role":"system","content":"s"}]}`))
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if _, ok := m["tool_choice"]; ok {
		t.Fatalf("tool_choice should be removed")
	}
	if _, ok := m["tools"]; ok {
		t.Fatalf("tools should be suppressed when tool_choice=none")
	}
}

// TestNormalizeToolChoiceAutoType auto 对象转字符串。
func TestNormalizeToolChoiceAutoType(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"hy3","tool_choice":{"type":"auto"},"messages":[{"role":"system","content":"s"}]}`))
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	if m["tool_choice"] != "auto" {
		t.Fatalf("want auto, got %v", m["tool_choice"])
	}
}

// TestPrepareBodyInvalidJSON 非法 JSON 原样返回。
func TestPrepareBodyInvalidJSON(t *testing.T) {
	src := []byte(`not json`)
	if got := PrepareBody(src); string(got) != string(src) {
		t.Fatalf("invalid json should pass through unchanged, got %s", got)
	}
}

// TestAggregate 聚合流式 SSE：content + reasoning_content。
func TestAggregate(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"cmb-1","model":"hy3","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`data: {"id":"cmb-1","model":"hy3","choices":[{"index":0,"delta":{"content":"He","reasoning_content":"thinking"}}]}`,
		`data: {"id":"cmb-1","model":"hy3","choices":[{"index":0,"delta":{"content":"llo"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n")
	got, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hello" {
		t.Fatalf("content = %v", msg["content"])
	}
	if msg["reasoning_content"] != "thinking" {
		t.Fatalf("reasoning_content = %v", msg["reasoning_content"])
	}
	if got["id"] != "cmb-1" {
		t.Fatalf("id = %v", got["id"])
	}
}

// TestAggregateToolCalls 流式 tool_calls 按 index 合并、arguments 拼接。
func TestAggregateToolCalls(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"c","model":"hy3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"ci"}}]}}]}`,
		`data: {"id":"c","model":"hy3","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"ty\":\"bj\"}"}}]}}]}`,
		`data: [DONE]`,
	}, "\n")
	got, err := Aggregate(strings.NewReader(sse))
	if err != nil {
		t.Fatal(err)
	}
	msg := got["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	calls := msg["tool_calls"].([]map[string]any)
	if len(calls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(calls))
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("name = %v", fn["name"])
	}
	if fn["arguments"] != `{"city":"bj"}` {
		t.Fatalf("arguments = %v", fn["arguments"])
	}
}

// TestClassify 错误分类（实测码）。
func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{402, "", "hard_credit"},
		{200, "积分不足", "hard_credit"},
		{200, "Offline user session not found", "session_dead"},
		{200, "code=12153", "session_dead"},
		{429, "too many requests", "soft_rate"},
		// issue #53：按**模型**限流（6004 / "switch to the other models"）仍归 soft_rate，
		// 由 handler 按 provider.IsModelScopedSoftRate 分流成 (账号, 模型) 粒度冷却，
		// 账号上其它模型照常路由（见 R47）。
		{429, "usage exceeds frequency limit ... switch to the other models to continue", "soft_rate"},
		{429, "our gateway is closed currently", "soft_rate"},
		// 14018 积分耗尽（2026-09-23 生产实录原文 body）：账号级余额问题归 hard_credit
		// ——误判软限流只冷 60s，降级链永不触发，客户端死循环吃 429。
		{429, `{"error":{"data":{"code":14018,"msg":"Credits exhausted. Please visit the link below to purchase add-on packs and get more credits: https://www.codebuddy.ai/profile/usage ","requestId":"9044201e"}}`, "hard_credit"},
		// 匹配防前缀误伤（S2）：code 140180 含子串 "code":14018，须保持软限流；
		// 尾形（code 后紧跟 }，无 msg）是真 14018，须命中。
		{429, `{"error":{"data":{"code":140180,"msg":"future code"}}}`, "soft_rate"},
		{429, `{"error":{"data":{"code":14018}}}`, "hard_credit"},
		// 14003 过频（构造 body，未实证）：保持软限流，不回归。
		{429, `{"code":14003,"msg":"too many requests"}`, "soft_rate"},
		// 429 + quota exceeded（非 14018）：429 优先于 hardRule 的既有语义保持——
		// 先判 hard 会把 6004 类模型级限流误归 12h 账号冷却。
		{429, `{"error":{"msg":"quota exceeded"}}`, "soft_rate"},
		{404, "", "not_found"},
		// 会话头族缺失（11128 conversation id missing）= 上游按账号灰度的渠道校验，
		// 归账号级（软冷却 + 换号，见 R116）；同码的 unapproved channel 是请求级内容
		// 指纹问题，不罚号。
		{400, `{"code":11128,"msg":"conversation id missing","requestId":"x"}`, "account_fault"},
		{400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`, "content_blocked"},
		{500, "", "server"},
		{400, "code=11102", "client"},
		{200, "ok", "none"},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body).String(); got != c.want {
			t.Errorf("Classify(%d,%q) = %s, want %s", c.status, c.body, got, c.want)
		}
	}
}

// TestParseCredits 倍率解析：目录 credits 字段的多种写法。
func TestParseCredits(t *testing.T) {
	cases := map[string]float64{
		"x0.79 credits": 0.79,
		"x3.47":         3.47,
		"x0.00":         0,
		"0x":            0,
		"":              0,
	}
	for in, want := range cases {
		if got := parseCredits(in); got != want {
			t.Errorf("parseCredits(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestFetchModelsExcludesBrokenAndAddsExtra 目录结果应剔除不可用模型并补入目录外可用模型。
func TestFetchModelsExcludesBrokenAndAddsExtra(t *testing.T) {
	seen := map[string]bool{}
	// 模拟 FetchModels 的合并逻辑（不发起网络请求）
	raws := []catalogModel{
		{ID: "hy3", Name: "Hy3"},
		{ID: "deepseek-v4-pro", Name: "DS Pro"},     // broken，应剔除
		{ID: "deepseek-v4-flash", Name: "DS Flash"}, // broken，应剔除
		{ID: "glm-5.2", Name: "GLM"},
	}
	out := make([]string, 0)
	for _, m := range raws {
		if m.ID == "" || m.Disabled || brokenModels[m.ID] || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m.ID)
	}
	for _, m := range extraModels {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m.ID)
	}
	joined := strings.Join(out, ",")
	if strings.Contains(joined, "deepseek-v4-pro") || strings.Contains(joined, "deepseek-v4-flash") {
		t.Fatalf("broken models must be excluded: %s", joined)
	}
	if !strings.Contains(joined, "deepseek-v4.1-flash") {
		t.Fatalf("deepseek-v4.1-flash must be added: %s", joined)
	}
	if !strings.Contains(joined, "hy4-preview-f") {
		t.Fatalf("hy4-preview-f must be added: %s", joined)
	}
}

// TestStaticModelsSane 静态表不含不可用模型且包含免费模型。
func TestStaticModelsSane(t *testing.T) {
	ids := map[string]bool{}
	for _, m := range StaticModels() {
		ids[m.ID] = true
		if brokenModels[m.ID] {
			t.Errorf("static table contains broken model %s", m.ID)
		}
	}
	for _, want := range []string{"hy3", "hy4-preview", "deepseek-v4.1-flash", "hy4-preview-f"} {
		if !ids[want] {
			t.Errorf("static table missing %s", want)
		}
	}
}

// 国际版档位能力与国内版刻意不同：deepseek-v4.1-flash 在国际版只认 high，
// 客户端发 low/max 必须降级（上游静默接受不报错，但声明外的档位语义失真）。
func TestPrepareBodyClampsEffortByGlobalRealm(t *testing.T) {
	cases := []struct{ model, in, want string }{
		{"deepseek-v4.1-flash", "low", "high"},
		{"deepseek-v4.1-flash", "max", "high"},
		{"gpt-5.6-luna", "xhigh", "xhigh"},
		{"gpt-5.3-codex", "high", "medium"},
		{"deepseek-v4.1-flash", "ultra", "high"},
	}
	for _, tc := range cases {
		out := PrepareBody([]byte(`{"model":"` + tc.model + `","messages":[],"reasoning_effort":"` + tc.in + `"}`))
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got := obj["reasoning_effort"]; got != tc.want {
			t.Errorf("%s + %s → %v, want %s（国际版档位表）", tc.model, tc.in, got, tc.want)
		}
	}
}

// 输出上限三别名收敛在国际版同样生效：null/0 形态透传会被上游按整数下限拒掉
// （实测 11133 param=max_output_tokens）。
func TestPrepareBodyConvergesOutputLimits(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want any
	}{
		{"max_output_tokens 正整数继承", `{"max_output_tokens":8192}`, float64(8192)},
		{"max_output_tokens null 删除", `{"max_output_tokens":null}`, nil},
		{"max_tokens null 时不透传", `{"max_tokens":null}`, nil},
		{"显式 max_tokens 优先", `{"max_tokens":32000,"max_output_tokens":8192}`, float64(32000)},
		{"max_tokens 1 按未设置下发", `{"max_tokens":1}`, nil},
		{"max_output_tokens 1 按未设置下发", `{"max_output_tokens":1}`, nil},
		{"131072 不动", `{"max_tokens":131072}`, float64(131072)},
	}
	for _, c := range cases {
		out := PrepareBody([]byte(`{"model":"gpt-5.6-luna","messages":[],` + c.src[1:]))
		var obj map[string]any
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("%s: unmarshal: %v", c.name, err)
		}
		if _, has := obj["max_output_tokens"]; has {
			t.Errorf("%s: 别名不应透传", c.name)
		}
		if c.want == nil {
			if v, has := obj["max_tokens"]; has {
				t.Errorf("%s: max_tokens 不应出现，得到 %v", c.name, v)
			}
			continue
		}
		if obj["max_tokens"] != c.want {
			t.Errorf("%s: max_tokens=%v want %v", c.name, obj["max_tokens"], c.want)
		}
	}
}

// TestStreamClientHasNoTotalTimeout 守门：流式必须走**无总超时**的 client。
//
// 背景：http.Client.Timeout 是整请求上限，计时器在 Do() 返回后继续跑直到 body 读完；
// SSE 整个生成期都在读 body，故长思考请求会被从流中间掐断（实测本渠道 15582 completion
// tokens 的长输出）。非流式 client 必须保留总超时（短请求的合理兜底）。
func TestStreamClientHasNoTotalTimeout(t *testing.T) {
	c := New()
	if c.StreamHTTP == nil {
		t.Fatal("StreamHTTP 必须存在")
	}
	if c.StreamHTTP.Timeout != 0 {
		t.Fatalf("StreamHTTP.Timeout = %v，必须为 0（流式不能有整请求上限）", c.StreamHTTP.Timeout)
	}
	if c.HTTP == nil || c.HTTP.Timeout <= 0 {
		t.Fatalf("非流式 HTTP.Timeout = %v，必须 > 0", c.HTTP.Timeout)
	}
	// 无总超时后必须靠 ResponseHeaderTimeout 兜底，否则连响应头都等不到会无限挂住。
	tr, ok := c.StreamHTTP.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatalf("StreamHTTP.Transport = %T，应为 *http.Transport", c.StreamHTTP.Transport)
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Fatal("StreamHTTP 必须有 ResponseHeaderTimeout 兜底")
	}
	if tr.TLSNextProto == nil {
		t.Fatal("应强制 HTTP/1.1（禁 h2）")
	}
}

// TestIdleTimeoutDefaults 未注入配置时回落默认值（装配漏注入不应退化成「无兜底」）。
func TestIdleTimeoutDefaults(t *testing.T) {
	c := New()
	if got := c.idleTimeout(); got != DefaultIdleTimeout {
		t.Fatalf("idleTimeout() = %v, want %v", got, DefaultIdleTimeout)
	}
	c.IdleTimeout = 33 * time.Second
	if got := c.idleTimeout(); got != 33*time.Second {
		t.Fatalf("idleTimeout() = %v, want 33s", got)
	}
}
