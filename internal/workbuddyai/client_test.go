package workbuddyai

import (
	"encoding/json"
	"strings"
	"testing"
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

// TestPrepareBodySanitizesSystemFingerprint 脱敏随 PrepareBody 生效（system 指纹）。
func TestPrepareBodySanitizesSystemFingerprint(t *testing.T) {
	const fp = "You are an AI agent powered by DeepSeek Harness"
	out := PrepareBody([]byte(`{"model":"hy3","messages":[{"role":"system","content":"` + fp + `"},{"role":"user","content":"hi"}]}`))
	if strings.Contains(string(out), "powered by DeepSeek Harness") {
		t.Fatalf("system 指纹未被净化: %s", out)
	}
	if !strings.Contains(string(out), "powered by DSH") {
		t.Fatalf("身份句改写缺失: %s", out)
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
		// 按模型限额的 6004 同样归软限流：handler 走 CooldownModel 只冷却「账号×该模型」
		// 到重置点（模型级冷却已化解 issue #53 的账号级误伤担忧），模型全冷时降级链
		// 自动顶替；历史上的 ErrPassthrough 纯透传什么都不冷却，降级链永不触发。
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
