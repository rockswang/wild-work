// minimax_test.go 假上游回归测试：**全程 httptest，不发任何真实网络请求**。
//
// 覆盖协议文档里标注为「坑」的每一条：
//   - 出站体形状（顶层 system、stream 恒 true、tools 形状、有值才带的字段）；
//   - thinking 决策表（M3.1 强制 adaptive、M2.7 不发 thinking、switchable 的 on/none）；
//   - 消息序列化（tool_use/tool_result 成对、孤儿两侧都剔除、不跨 assistant 边界累积、
//     reasoning 不回传、图片裸 base64）；
//   - 入站 SSE（signature_delta 忽略、error 帧抛错、截断尾行不丢 message_delta、
//     thinking→reasoning_content、空响应抛错）；
//   - 目录解析（长名/短名/最大档窗口/model_order/兜底）；
//   - 积分求和（字符串金额）、签到幂等（claim_result=2）、续期（invalid_grant）；
//   - Classify（402/429/5xx/401/403/2013）。
package minimax

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// newTestClient 起一个假上游，Base 与 Account 都指向它（本包没有任何真实出站请求）。
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewWithBase(srv.URL)
	c.Account = srv.URL
	return c
}

func testAuth() *auth.Auth {
	return &auth.Auth{
		Kind:         "minimax",
		AccessToken:  "mmoat_FAKE_access_for_test",
		RefreshToken: "mmort_FAKE_refresh_for_test",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}
}

// sse 把若干 JSON 事件拼成合法的 SSE 文本（每帧带空行分隔）。
func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		b.WriteString("data: ")
		b.WriteString(e)
		b.WriteString("\n\n")
	}
	return b.String()
}

// readAll 读完转换器输出（含收尾帧），失败时返回错误。
func readAll(t *testing.T, src string) (string, error) {
	t.Helper()
	r := newAnthropicStream(strings.NewReader(src), "minimax/MiniMax-M3")
	b, err := io.ReadAll(r)
	return string(b), err
}

// ---------------------------------------------------------------------------
// 出站体形状
// ---------------------------------------------------------------------------

// TestChatStreamOutboundShape 顶部字段 + 请求头 + 恒流式（协议 §2 / §4）。
func TestChatStreamOutboundShape(t *testing.T) {
	var got map[string]any
	var path, authz, accept, ver string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		authz = r.Header.Get("Authorization")
		accept = r.Header.Get("Accept")
		ver = r.Header.Get("Anthropic-Version")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse(
			`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":3}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			`{"type":"message_stop"}`,
		))
	})

	body := []byte(`{
	  "model":"MiniMax-M3.1-Flash-Preview",
	  "stream":false,
	  "max_tokens":1024,
	  "temperature":0.3,
	  "stop":["STOP"],
	  "reasoning_effort":"low",
	  "messages":[
	    {"role":"system","content":"SYS"},
	    {"role":"developer","content":"DEV"},
	    {"role":"user","content":"hi"}
	  ],
	  "tools":[{"type":"function","function":{"name":"f1","description":"d1","parameters":{"type":"object","properties":{"a":{"type":"string"}}}}}]
	}`)
	rc, status, respBody, err := c.ChatStream(testAuth(), body)
	if err != nil || status != 200 || respBody != nil {
		t.Fatalf("ChatStream = (%v, %d, %v, %v)，期望 200 流", rc, status, respBody, err)
	}
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatalf("读流失败：%v", err)
	}
	_ = rc.Close()

	if path != EpMessages {
		t.Errorf("路径 = %q，期望 %q", path, EpMessages)
	}
	if authz != "Bearer mmoat_FAKE_access_for_test" {
		t.Errorf("Authorization = %q", authz)
	}
	if accept != "text/event-stream" {
		t.Errorf("Accept = %q", accept)
	}
	// 协议 §2：**不需要 anthropic-version**（发了也不算错，但没依据就别发）。
	if ver != "" {
		t.Errorf("不该发送 Anthropic-Version，实得 %q", ver)
	}
	// stream **恒 true**（客户端传了 false 也不透传）。
	if got["stream"] != true {
		t.Errorf("stream = %v，期望恒 true", got["stream"])
	}
	if got["model"] != "MiniMax-M3.1-Flash-Preview" {
		t.Errorf("model = %v", got["model"])
	}
	// system 是顶层字段，且 system/developer 两条都提升上去了。
	sys, ok := got["system"].([]any)
	if !ok || len(sys) != 2 {
		t.Fatalf("system = %#v，期望顶层两条文本块", got["system"])
	}
	first, _ := sys[0].(map[string]any)
	if first["type"] != "text" || first["text"] != "SYS" {
		t.Errorf("system[0] = %#v", first)
	}
	// messages 里**不得**出现 system 角色。
	msgs, _ := got["messages"].([]any)
	for i, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["role"] == "system" || mm["role"] == "developer" {
			t.Errorf("messages[%d] 出现了 system 角色：%#v", i, mm)
		}
	}
	// 有值才带：max_tokens / temperature / stop_sequences 都在。
	if got["max_tokens"] != float64(1024) {
		t.Errorf("max_tokens = %v", got["max_tokens"])
	}
	if got["temperature"] != 0.3 {
		t.Errorf("temperature = %v", got["temperature"])
	}
	seq, _ := got["stop_sequences"].([]any)
	if len(seq) != 1 || seq[0] != "STOP" {
		t.Errorf("stop_sequences = %#v", got["stop_sequences"])
	}
	// tools 形状：[{name, description, input_schema}]，input_schema 来自 parameters。
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", got["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "f1" || tool["description"] != "d1" || tool["input_schema"] == nil {
		t.Errorf("tools[0] = %#v", tool)
	}
	if _, has := tool["function"]; has {
		t.Errorf("tools[0] 不该残留 OpenAI 的 function 包装：%#v", tool)
	}
	// thinking：M3.1 且 effort=low → adaptive + output_config.effort。
	th, _ := got["thinking"].(map[string]any)
	if th["type"] != "adaptive" {
		t.Errorf("thinking = %#v，期望 adaptive", got["thinking"])
	}
	oc, _ := got["output_config"].(map[string]any)
	if oc["effort"] != "low" {
		t.Errorf("output_config = %#v，期望 effort=low", got["output_config"])
	}
}

// TestOutboundOmitsEmptyOptionals：max_tokens / temperature / stop / tools 缺失时不产出该键。
func TestOutboundOmitsEmptyOptionals(t *testing.T) {
	out, err := buildMessagesBody([]byte(`{"model":"MiniMax-M2.7","messages":[{"role":"user","content":"hi"}]}`), catalogEntry{ID: "MiniMax-M2.7", ThinkingMode: "forced_on"}, true)
	if err != nil {
		t.Fatalf("buildMessagesBody: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"max_tokens", "temperature", "stop_sequences", "tools", "thinking", "output_config", "system"} {
		if _, has := got[k]; has {
			t.Errorf("不该出现字段 %q：%#v", k, got[k])
		}
	}
	if got["stream"] != true {
		t.Errorf("stream = %v", got["stream"])
	}
}

// TestThinkingDecisionTable 协议 §4.1 的决策表逐条验证。
func TestThinkingDecisionTable(t *testing.T) {
	m31 := catalogEntry{ID: "MiniMax-M3.1-Flash-Preview", Efforts: []string{"default", "low", "high"}, ThinkingMode: "forced_on"}
	m3 := catalogEntry{ID: "MiniMax-M3", ThinkingMode: "switchable", Efforts: []string{"on", "none"}}
	m27 := catalogEntry{ID: "MiniMax-M2.7", ThinkingMode: "forced_on"}
	cases := []struct {
		name   string
		entry  catalogEntry
		known  bool
		effort string
		want   string // thinking.type；空 = 整个字段不发
		oc     string // output_config.effort
	}{
		{"M3.1 无档位也必须 adaptive", m31, true, "", "adaptive", ""},
		{"M3.1 有档位 → adaptive+effort", m31, true, "high", "adaptive", "high"},
		{"M3.1 收到 none 仍必须 adaptive（disabled 会硬 400）", m31, true, "none", "adaptive", ""},
		{"M3.1 非法档位被丢弃", m31, true, "xhigh", "adaptive", ""},
		{"M3 switchable 选 none → disabled", m3, true, "none", "disabled", ""},
		{"M3 switchable 选 on → adaptive", m3, true, "on", "adaptive", ""},
		{"M3 未表达档位 → 不发", m3, true, "", "", ""},
		{"M2.7 无档位 → 整个 thinking 不发", m27, true, "", "", ""},
		{"未知模型未表达档位 → 不发（保守）", catalogEntry{}, false, "", "", ""},
		{"未知模型有档位 → adaptive+effort", catalogEntry{}, false, "medium", "adaptive", "medium"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			th, oc := thinkingFor(tc.entry, tc.known, tc.effort)
			if tc.want == "" {
				if th != nil {
					t.Fatalf("thinking = %#v，期望不发", th)
				}
				return
			}
			m, _ := th.(map[string]any)
			if m["type"] != tc.want {
				t.Fatalf("thinking.type = %v，期望 %v", m["type"], tc.want)
			}
			if tc.oc == "" {
				if oc != nil {
					t.Fatalf("output_config = %#v，期望不发", oc)
				}
				return
			}
			om, _ := oc.(map[string]any)
			if om["effort"] != tc.oc {
				t.Fatalf("output_config.effort = %v，期望 %v", om["effort"], tc.oc)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 消息序列化（协议 §4.2）
// ---------------------------------------------------------------------------

// TestToolPairingAndOrphans 配对 / 孤儿剔除 / 不跨 assistant 边界累积 / system 提升。
func TestToolPairingAndOrphans(t *testing.T) {
	body := []byte(`{
	  "model":"MiniMax-M3",
	  "messages":[
	    {"role":"system","content":"SYS"},
	    {"role":"user","content":"hi"},
	    {"role":"assistant","content":"","tool_calls":[
	       {"id":"call_1","type":"function","function":{"name":"f1","arguments":"{\"a\":1}"}},
	       {"id":"call_2","type":"function","function":{"name":"f2","arguments":"NOT_JSON"}}
	    ]},
	    {"role":"tool","tool_call_id":"call_1","content":"R1"},
	    {"role":"assistant","content":"mid"},
	    {"role":"tool","tool_call_id":"call_2","content":"R2_ORPHAN"},
	    {"role":"assistant","content":[{"type":"reasoning","text":"SHOULD_NOT_APPEAR"},{"type":"text","text":"tail"}]}
	  ]
	}`)
	out, err := buildMessagesBody(body, catalogEntry{ID: "MiniMax-M3", ThinkingMode: "switchable"}, true)
	if err != nil {
		t.Fatalf("buildMessagesBody: %v", err)
	}
	s := string(out)
	// 孤儿 tool_result（跨了 assistant 边界）必须消失。
	if strings.Contains(s, "R2_ORPHAN") {
		t.Errorf("跨 assistant 边界的 tool_result 未被剔除：%s", s)
	}
	// 配不成对的 tool_use（call_2）必须消失。
	if strings.Contains(s, "call_2") || strings.Contains(s, "NOT_JSON") {
		t.Errorf("孤儿 tool_use 未被剔除：%s", s)
	}
	// 历史 reasoning 块不回传（协议 §4.2 第 7 条）。
	if strings.Contains(s, "SHOULD_NOT_APPEAR") {
		t.Errorf("历史 reasoning 块被回传了：%s", s)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	msgs, _ := got["messages"].([]any)
	var roles []string
	var sawUse, sawResult bool
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		roles = append(roles, strOf(mm["role"]))
		blocks, _ := mm["content"].([]any)
		for _, b := range blocks {
			bm, _ := b.(map[string]any)
			switch bm["type"] {
			case "tool_use":
				sawUse = true
				if bm["id"] != "call_1" || bm["name"] != "f1" {
					t.Errorf("tool_use = %#v", bm)
				}
				// input 必须是**对象**（字符串会被上游拒）。
				in, _ := bm["input"].(map[string]any)
				if in["a"] != float64(1) {
					t.Errorf("tool_use.input = %#v", bm["input"])
				}
			case "tool_result":
				sawResult = true
				if bm["tool_use_id"] != "call_1" || bm["content"] != "R1" {
					t.Errorf("tool_result = %#v", bm)
				}
			}
		}
	}
	if !sawUse || !sawResult {
		t.Fatalf("配对后应同时保留 tool_use 与 tool_result：%s", s)
	}
	// 角色必须交替且以 user 开头（Anthropic 硬约束）。
	for i := 1; i < len(roles); i++ {
		if roles[i] == roles[i-1] {
			t.Errorf("相邻同角色未被合并：%v", roles)
		}
	}
	if len(roles) == 0 || roles[0] != "user" {
		t.Errorf("messages 必须以 user 开头：%v", roles)
	}
}

// TestToolResultUnreadableImageSkipped：工具结果里的图片读不到 → 跳过（协议 §4.2 第 9 条）。
func TestToolResultUnreadableImageSkipped(t *testing.T) {
	body := []byte(`{
	  "model":"MiniMax-M3",
	  "messages":[
	    {"role":"user","content":"hi"},
	    {"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
	    {"role":"tool","tool_call_id":"c1","content":[
	      {"type":"text","text":"look"},
	      {"type":"image_url","image_url":{"url":"https://example.invalid/x.png"}}
	    ]}
	  ]
	}`)
	out, err := buildMessagesBody(body, catalogEntry{ID: "MiniMax-M3", SupportsImage: true}, true)
	if err != nil {
		t.Fatalf("工具结果里的图片读不到应跳过，不该报错：%v", err)
	}
	if strings.Contains(string(out), "example.invalid") {
		t.Errorf("读不到的图片不该出现在出站体里：%s", out)
	}
	if !strings.Contains(string(out), "look") {
		t.Errorf("工具结果的文本内容丢了：%s", out)
	}
}

// TestMessageImageMustBeBareBase64：消息体图片 → 裸 base64；image_url 形状/读不到 → 报错。
func TestMessageImageMustBeBareBase64(t *testing.T) {
	// 1x1 PNG 的极小 base64（内容无关，只验形态）。
	const png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8AAAwAB/wD/AL0AAAAASUVORK5CYII="
	body := []byte(`{"model":"MiniMax-M3","messages":[{"role":"user","content":[
	  {"type":"text","text":"看这张"},
	  {"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `"}}
	]}]}`)
	out, err := buildMessagesBody(body, catalogEntry{ID: "MiniMax-M3", SupportsImage: true}, true)
	if err != nil {
		t.Fatalf("buildMessagesBody: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	msgs, _ := got["messages"].([]any)
	mm, _ := msgs[0].(map[string]any)
	blocks, _ := mm["content"].([]any)
	var img map[string]any
	for _, b := range blocks {
		bm, _ := b.(map[string]any)
		if bm["type"] == "image" {
			img = bm
		}
	}
	if img == nil {
		t.Fatalf("图片块丢了：%s", out)
	}
	src, _ := img["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" {
		t.Errorf("source = %#v", src)
	}
	if src["data"] != png {
		t.Errorf("data 必须是**裸 base64**（无 data: 前缀），实得 %v", src["data"])
	}
	if strings.Contains(string(out), "data:image") {
		t.Errorf("出站体里不该残留 data: 前缀：%s", out)
	}

	// 2) 读不到（http URL）→ 消息体里必须报错（不静默丢图）。
	bad := []byte(`{"model":"MiniMax-M3","messages":[{"role":"user","content":[
	  {"type":"image_url","image_url":{"url":"https://example.invalid/a.png"}}]}]}`)
	if _, err := buildMessagesBody(bad, catalogEntry{ID: "MiniMax-M3", SupportsImage: true}, true); err == nil {
		t.Error("消息体里的图片读不到时必须报错")
	}

	// 3) 模型不支持图片 → 报错（协议 §4.2 第 10 条）。
	if _, err := buildMessagesBody(body, catalogEntry{ID: "MiniMax-M2.7", SupportsImage: false}, true); err == nil {
		t.Error("supportsImage=false 的模型收到图片必须报错")
	}
}

// TestToolResultAfterInterveningUserText 兼容层会把「含 tool_result 与文本的 user 消息」
// 拆成 [user(文本), tool(结果A), tool(结果B)]（gateway/anthropic.go:236-253）。
// 这种「结果前插了一条 user 文本」的形态仍必须配对成功并合并成一条 user 消息。
func TestToolResultAfterInterveningUserText(t *testing.T) {
	body := []byte(`{
	  "model":"MiniMax-M3",
	  "messages":[
	    {"role":"user","content":"hi"},
	    {"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"f1","arguments":"{}"}}]},
	    {"role":"user","content":"这是 Claude Code 附加的说明"},
	    {"role":"tool","tool_call_id":"c1","content":"结果"}
	  ]
	}`)
	out, err := buildMessagesBody(body, catalogEntry{ID: "MiniMax-M3"}, true)
	if err != nil {
		t.Fatalf("buildMessagesBody: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"c1"`) || !strings.Contains(s, "结果") {
		t.Fatalf("工具调用上下文被误剔除：%s", s)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	msgs, _ := got["messages"].([]any)
	// 期望 [user, assistant(tool_use), user(tool_result + 文本)]。
	if len(msgs) != 3 {
		t.Fatalf("消息数 = %d，期望 3：%s", len(msgs), s)
	}
	last, _ := msgs[2].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("最后一条应为 user：%#v", last)
	}
	blocks, _ := last["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("合并后的 user 应含 tool_result 与文本两块：%#v", blocks)
	}
	first, _ := blocks[0].(map[string]any)
	if first["type"] != "tool_result" || first["tool_use_id"] != "c1" {
		t.Errorf("tool_result 必须在前：%#v", first)
	}
}

// TestToolResultCrossAssistantBoundaryIsOrphan：结果跨了 assistant 边界 → 两侧都剔除。
func TestToolResultCrossAssistantBoundaryIsOrphan(t *testing.T) {
	body := []byte(`{
	  "model":"MiniMax-M3",
	  "messages":[
	    {"role":"user","content":"hi"},
	    {"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"f1","arguments":"{}"}}]},
	    {"role":"assistant","content":"我又说了一句"},
	    {"role":"tool","tool_call_id":"c1","content":"迟到的结果"}
	  ]
	}`)
	out, err := buildMessagesBody(body, catalogEntry{ID: "MiniMax-M3"}, true)
	if err != nil {
		t.Fatalf("buildMessagesBody: %v", err)
	}
	s := string(out)
	if strings.Contains(s, "迟到的结果") || strings.Contains(s, `"c1"`) {
		t.Fatalf("跨 assistant 边界的配对必须被剔除：%s", s)
	}
}

// TestAssistantFirstGetsPlaceholderUser：messages 必须以 user 开头（Anthropic 硬约束），
// assistant 开头时补一条占位 user。
func TestAssistantFirstGetsPlaceholderUser(t *testing.T) {
	body := []byte(`{"model":"MiniMax-M3","messages":[{"role":"assistant","content":"我先说的"}]}`)
	out, err := buildMessagesBody(body, catalogEntry{ID: "MiniMax-M3"}, true)
	if err != nil {
		t.Fatalf("buildMessagesBody: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("消息数 = %d，期望 2（占位 user + assistant）：%s", len(msgs), out)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("首条必须是 user：%#v", first)
	}
	second, _ := msgs[1].(map[string]any)
	if second["role"] != "assistant" {
		t.Fatalf("第二条应为 assistant：%#v", second)
	}
}

// TestToolResultIsError：工具失败标记要透传成 is_error。
func TestToolResultIsError(t *testing.T) {
	body := []byte(`{
	  "model":"MiniMax-M3",
	  "messages":[
	    {"role":"user","content":"hi"},
	    {"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"f1","arguments":"{}"}}]},
	    {"role":"tool","tool_call_id":"c1","content":"炸了","is_error":true}
	  ]
	}`)
	out, err := buildMessagesBody(body, catalogEntry{ID: "MiniMax-M3"}, true)
	if err != nil {
		t.Fatalf("buildMessagesBody: %v", err)
	}
	if !strings.Contains(string(out), `"is_error":true`) {
		t.Errorf("is_error 未透传：%s", out)
	}
}

// TestChatStreamRejectsLocallyAsBadParams：本地出站体拒绝走 400 + bad_params（不罚号）。
func TestChatStreamRejectsLocallyAsBadParams(t *testing.T) {
	c := NewWithBase("http://127.0.0.1:0") // 不该被访问：本地拒绝在发请求之前
	rc, status, body, err := c.ChatStream(testAuth(), []byte(`{"messages":[]}`))
	if err != nil || rc != nil {
		t.Fatalf("本地拒绝不该返回传输错误：(%v, %v)", rc, err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d，期望 400", status)
	}
	if k := Classify(status, string(body)); k != provider.ErrBadParams {
		t.Fatalf("Classify(本地 400) = %v，期望 ErrBadParams（请求级、不罚号）", k)
	}
}

// ---------------------------------------------------------------------------
// 入站 SSE（协议 §5）
// ---------------------------------------------------------------------------

// TestSSEConversionAndSignatureIgnored：thinking→reasoning_content，签名不得进正文。
func TestSSEConversionAndSignatureIgnored(t *testing.T) {
	src := sse(
		`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"cache_read_input_tokens":4,"cache_creation_input_tokens":1}}}`,
		`{"type":"ping"}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"想想"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"deadbeefcafe"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"答案"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7,"output_tokens_details":{"thinking_tokens":3}}}`,
		`{"type":"message_stop"}`,
	)
	out, err := readAll(t, src)
	if err != nil {
		t.Fatalf("正常流不该报错：%v", err)
	}
	if !strings.Contains(out, `"reasoning_content":"想想"`) {
		t.Errorf("thinking 未映射到 reasoning_content：%s", out)
	}
	if !strings.Contains(out, `"content":"答案"`) {
		t.Errorf("正文丢了：%s", out)
	}
	if strings.Contains(out, "deadbeefcafe") {
		t.Errorf("signature_delta 被当成正文下发了：%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("收尾帧缺失或映射错误：%s", out)
	}
	// usage：prompt_tokens = input + cache_read + cache_creation（总输入口径）。
	if !strings.Contains(out, `"prompt_tokens":15`) {
		t.Errorf("prompt_tokens 口径错误（应为 10+4+1）：%s", out)
	}
	if !strings.Contains(out, `"cached_tokens":4`) {
		t.Errorf("缓存命中未搬运：%s", out)
	}
	if !strings.Contains(out, `"cache_write_tokens":1`) {
		t.Errorf("缓存写入未搬运：%s", out)
	}
	if !strings.Contains(out, `"completion_tokens":7`) {
		t.Errorf("completion_tokens 错误：%s", out)
	}
	if !strings.Contains(out, `"reasoning_tokens":3`) {
		t.Errorf("思考 token 未上报：%s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("收尾缺 [DONE]：%s", out)
	}
}

// TestStreamErrorFrameThrows：流内 error 帧必须抛错（不得静默收尾）。
func TestStreamErrorFrameThrows(t *testing.T) {
	src := sse(
		`{"type":"message_start","message":{"id":"msg_1"}}`,
		`{"type":"error","error":{"type":"overloaded_error","message":"上游拥挤"}}`,
	)
	out, err := readAll(t, src)
	if err == nil {
		t.Fatalf("error 帧必须抛错，实得输出：%s", out)
	}
	if !strings.Contains(err.Error(), "上游拥挤") {
		t.Errorf("错误文本应带上游原文：%v", err)
	}
	if strings.Contains(out, "[DONE]") {
		t.Errorf("失败流不得补 [DONE]（R36）：%s", out)
	}
}

// TestTruncatedTailKeepsMessageDelta：截断流的尾行（无换行符）不得丢 stop_reason 与 usage。
func TestTruncatedTailKeepsMessageDelta(t *testing.T) {
	full := sse(
		`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"f1"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"1}"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
	)
	// 去掉尾部换行：模拟「上游在最后一条 message_delta 之后直接断流」。
	truncated := strings.TrimRight(full, "\n")
	out, err := readAll(t, truncated)
	if err != nil {
		t.Fatalf("尾行完整时不该判截断：%v", err)
	}
	if !strings.Contains(out, `"arguments":"{\"a\":1}"`) {
		t.Errorf("tool_use 的完整 arguments 未交出：%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Errorf("stop_reason=tool_use 未映射成 tool_calls：%s", out)
	}
	if !strings.Contains(out, `"completion_tokens":9`) {
		t.Errorf("尾行上的 usage 丢了：%s", out)
	}
}

// TestTruncatedInsideBlockThrows：内容块中途断流必须报错，不得补 [DONE] 伪装收尾。
func TestTruncatedInsideBlockThrows(t *testing.T) {
	src := sse(
		`{"type":"message_start","message":{"id":"msg_1"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"f1"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`,
	)
	out, err := readAll(t, src)
	if err == nil {
		t.Fatalf("块中途断流必须报错，实得：%s", out)
	}
	if strings.Contains(out, "[DONE]") {
		t.Errorf("截断流不得补 [DONE]（R36）：%s", out)
	}
}

// TestEmptyResponseThrows：一个内容块都没有 → 抛错（协议 §5）。
func TestEmptyResponseThrows(t *testing.T) {
	src := sse(
		`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1}}}`,
		`{"type":"message_stop"}`,
	)
	if out, err := readAll(t, src); err == nil {
		t.Fatalf("空响应必须报错，实得输出：%s", out)
	}
}

// TestStreamThroughUpstreamLayer：经 upstream.StreamCapture 的端到端（含 usage 回传）。
func TestStreamThroughUpstreamLayer(t *testing.T) {
	src := sse(
		`{"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"cache_read_input_tokens":4}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		`{"type":"message_stop"}`,
	)
	w := httptest.NewRecorder()
	c := New()
	usage, err := c.Stream(w, strings.NewReader(src), "minimax/MiniMax-M3")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if usage == nil || usage["prompt_tokens"] != float64(14) {
		t.Fatalf("usage 捕获错误：%#v", usage)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"model":"minimax/MiniMax-M3"`) {
		t.Errorf("model 未回填客户端请求名（R14）：%s", body)
	}
	if !strings.Contains(body, `"content":"hi"`) {
		t.Errorf("正文未透出：%s", body)
	}
}

// ---------------------------------------------------------------------------
// 模型目录（协议 §6）
// ---------------------------------------------------------------------------

const catalogFixture = `{
  "providers": [
    {"providerId": "other", "config": {"models": {"Nope": {"name": "nope"}}}},
    {"providerId": "minimax", "config": {
      "models": {
        "MiniMax-M3.1-Flash-Preview": {
          "name": "M3.1-Flash-Preview",
          "limit": {"context": 512000, "output": 128000},
          "context_window_options": [512000, 1000000],
          "modalities": {"input": ["text", "image"]},
          "effort_options": ["default", "low", "low", "max"],
          "default_effort": "low",
          "thinking_config": {"mode": "forced_on"}
        },
        "MiniMax-M3": {
          "name": "M3",
          "limit": {"context": 1000000},
          "modalities": {"input": ["text"]},
          "thinking_config": {"mode": "switchable"}
        },
        "BrokenModel": {"limit": {"output": 0}, "default_effort": "nope"}
      },
      "model_order": ["MiniMax-M3", "MiniMax-M3.1-Flash-Preview"]
    }}
  ]
}`

func TestParseCatalog(t *testing.T) {
	entries, err := parseCatalog([]byte(catalogFixture))
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("条目数 = %d，期望 3", len(entries))
	}
	// model_order 说的两个优先，未列出的按插入序追加在后。
	if entries[0].ID != "MiniMax-M3" || entries[1].ID != "MiniMax-M3.1-Flash-Preview" || entries[2].ID != "BrokenModel" {
		t.Fatalf("排序错误：%v %v %v", entries[0].ID, entries[1].ID, entries[2].ID)
	}
	e := entries[1]
	// 长名是 id、短名是 name，两者都保留。
	if e.ID != "MiniMax-M3.1-Flash-Preview" || e.Name != "M3.1-Flash-Preview" {
		t.Errorf("长名/短名 = %q / %q", e.ID, e.Name)
	}
	// 窗口取 context_window_options 的**最大档**（1000000），而不是 limit.context（512000）。
	if e.ContextWindow != 1000000 {
		t.Errorf("ContextWindow = %d，期望 1000000（最大档）", e.ContextWindow)
	}
	if e.MaxTokens != 128000 {
		t.Errorf("MaxTokens = %d", e.MaxTokens)
	}
	if !e.SupportsImage {
		t.Error("modalities.input 含 image，SupportsImage 应为 true")
	}
	if len(e.Efforts) != 3 || e.Efforts[0] != "default" || e.Efforts[2] != "max" {
		t.Errorf("effort_options 去重保序失败：%v", e.Efforts)
	}
	if e.DefaultEffort != "low" || e.ThinkingMode != "forced_on" {
		t.Errorf("default_effort / thinking_config = %q / %q", e.DefaultEffort, e.ThinkingMode)
	}
	// M3：limit.context 回退、无 limit.output → MaxTokens 为 0（未声明）。
	m3 := entries[0]
	if m3.ContextWindow != 1000000 || m3.MaxTokens != 0 || m3.SupportsImage {
		t.Errorf("M3 归一错误：%+v", m3)
	}
	// BrokenModel：name 缺失回退 id；default_effort 不在档位表内 → 丢弃。
	b := entries[2]
	if b.Name != "BrokenModel" || b.ContextWindow != 0 || b.DefaultEffort != "" {
		t.Errorf("坏条目容错错误：%+v", b)
	}
}

func TestModelInfosShape(t *testing.T) {
	infos := modelInfosOf(staticCatalog)
	if len(infos) != 4 {
		t.Fatalf("静态兜底应 4 条，实得 %d", len(infos))
	}
	byID := map[string]provider.ModelInfo{}
	for _, m := range infos {
		byID[m.ID] = m
	}
	m31 := byID["MiniMax-M3.1-Flash-Preview"]
	if m31.ContextWindow != 1_000_000 || m31.MaxTokens != 128_000 || !m31.SupportsImages || !m31.ContextFromAPI {
		t.Errorf("M3.1 字段错误：%+v", m31)
	}
	if m31.DefaultReasoningEffort != "default" {
		t.Errorf("默认档位 = %q", m31.DefaultReasoningEffort)
	}
	if m31.CanDisableThinking == nil || *m31.CanDisableThinking {
		t.Errorf("forced_on 的 CanDisableThinking 应为 false：%v", m31.CanDisableThinking)
	}
	// M3 是 switchable：追加 on / none（on 在前）。
	m3 := byID["MiniMax-M3"]
	if m3.CanDisableThinking == nil || !*m3.CanDisableThinking {
		t.Errorf("switchable 的 CanDisableThinking 应为 true")
	}
	if len(m3.ReasoningEfforts) != 2 || m3.ReasoningEfforts[0] != "on" || m3.ReasoningEfforts[1] != "none" {
		t.Errorf("switchable 档位 = %v，期望 [on none]", m3.ReasoningEfforts)
	}
	// M2.7 系无档位表 → 不编档（R49）。
	m27 := byID["MiniMax-M2.7"]
	if m27.ReasoningEfforts != nil || m27.DefaultReasoningEffort != "" {
		t.Errorf("M2.7 不该有档位：%v / %q", m27.ReasoningEfforts, m27.DefaultReasoningEffort)
	}
}

// TestFetchModelsDynamicThenFallback：动态目录优先；失败/0 条 → 静态兜底且不报错。
func TestFetchModelsDynamicThenFallback(t *testing.T) {
	fail := false
	var sawPath, sawQuery, sawAuth string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		sawPath, sawQuery, sawAuth = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization")
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"base_resp":{"status_code":1,"status_msg":"boom"}}`)
			return
		}
		_, _ = io.WriteString(w, catalogFixture)
	})
	infos, err := c.FetchModels(testAuth())
	if err != nil {
		t.Fatalf("目录成功路径不该报错：%v", err)
	}
	if sawPath != EpModels || !strings.Contains(sawQuery, "region=cn") || !strings.Contains(sawQuery, "buildEnv=prod") {
		t.Errorf("目录请求错误：%s?%s", sawPath, sawQuery)
	}
	if sawAuth != "Bearer mmoat_FAKE_access_for_test" {
		t.Errorf("目录请求缺 Bearer：%q", sawAuth)
	}
	if len(infos) != 3 || infos[0].ID != "MiniMax-M3" {
		t.Fatalf("动态目录未生效：%+v", infos)
	}
	// 失败 → 静态兜底，不报错。
	fail = true
	infos, err = c.FetchModels(testAuth())
	if err != nil {
		t.Fatalf("兜底路径不得报错：%v", err)
	}
	if len(infos) != 4 || infos[0].ID != "MiniMax-M3.1-Flash-Preview" {
		t.Fatalf("静态兜底未生效：%+v", infos)
	}
	// 兜底后能力查询仍应命中（动态目录优先）。
	if e, ok := c.catalogEntry("MiniMax-M3"); !ok || e.ThinkingMode != "switchable" {
		t.Errorf("能力查询失败：%+v ok=%v", e, ok)
	}
	if _, ok := c.catalogEntry("MiniMax-M2.7"); !ok {
		t.Error("静态表条目应能查到")
	}
}

// ---------------------------------------------------------------------------
// 积分 / 签到 / 续期
// ---------------------------------------------------------------------------

func TestCreditDetailsSum(t *testing.T) {
	var path, authz string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		path, authz = r.URL.Path, r.Header.Get("Authorization")
		// 平铺结构：base_resp / total_count / details 同级，**没有 data 键**。
		_, _ = io.WriteString(w, `{"base_resp":{"status_code":0,"status_msg":"success"},
		  "total_count":2,
		  "details":[{"remaining_amount":"800.00"},{"remaining_amount":200}]}`)
	})
	remain, items, err := c.UserResourceDetail(testAuth())
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if path != EpCreditDetails || authz == "" {
		t.Errorf("请求错误：%s %q", path, authz)
	}
	// 余额 = Σ remaining_amount（字符串要兼容数字）；total_count 不是余额。
	if remain != 1000 {
		t.Errorf("余额 = %d，期望 1000（800 字符串 + 200 数字）", remain)
	}
	if len(items) != 1 || items[0].Remain != 1000 || !items[0].Usable {
		t.Errorf("明细条目错误：%+v", items)
	}
	// UserResource 与 UserResourceDetail 口径同源。
	if v, err := c.UserResource(testAuth()); err != nil || v != 1000 {
		t.Errorf("UserResource = (%d, %v)", v, err)
	}
}

// TestCreditDetailsEmptyIsZero：details 缺失/非数组 ⇒ 空数组 ⇒ 0，且**不是**查询失败。
func TestCreditDetailsEmptyIsZero(t *testing.T) {
	for _, resp := range []string{
		`{"base_resp":{"status_code":0},"total_count":0}`,
		`{"base_resp":{"status_code":0},"details":{"oops":"not-an-array"}}`,
	} {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, resp)
		})
		remain, items, err := c.UserResourceDetail(testAuth())
		if err != nil {
			t.Fatalf("details 缺失/非数组不该报错：%v（resp=%s）", err, resp)
		}
		if remain != 0 || len(items) != 1 || items[0].Remain != 0 {
			t.Errorf("应为 0 余额：%d %+v", remain, items)
		}
	}
	// 业务码非 0 → 才算失败。
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"base_resp":{"status_code":1001,"status_msg":"bad"},"details":[]}`)
	})
	if _, _, err := c.UserResourceDetail(testAuth()); err == nil {
		t.Error("业务码非 0 必须报错")
	}
}

func TestCheckinIdempotentAndClaim(t *testing.T) {
	var gotBody, gotQuery, gotMethod, gotPath string
	mode := "already"
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if mode == "already" {
			// 重复领取同样 HTTP 200，判据是 claim_result。
			_, _ = io.WriteString(w, `{"base_resp":{"status_code":0,"status_msg":"ok"},"data":{"claim_result":2,"points":0}}`)
			return
		}
		_, _ = io.WriteString(w, `{"base_resp":{"status_code":0,"status_msg":"ok"},"data":{"claim_result":1,"points":800}}`)
	})

	rep, err := c.DailyCheckinReport(testAuth())
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != EpSigninClaim {
		t.Errorf("签到请求错误：%s %s", gotMethod, gotPath)
	}
	if !strings.Contains(gotQuery, "timezone_id=") {
		t.Errorf("timezone_id 必须走 query 参数：%q", gotQuery)
	}
	if gotBody != "{}" {
		t.Errorf("签到请求体必须是空 JSON 对象，实得 %q", gotBody)
	}
	if rep.Status != provider.CheckinAlready {
		t.Fatalf("claim_result=2 应判 already，实得 %v", rep.Status)
	}
	// DailyCheckin 幂等：已领取返回 nil。
	if err := c.DailyCheckin(testAuth()); err != nil {
		t.Errorf("已领取时 DailyCheckin 应为 nil，实得 %v", err)
	}

	mode = "claimed"
	rep, err = c.DailyCheckinReport(testAuth())
	if err != nil || rep.Status != provider.CheckinClaimed || rep.Amount != 800 {
		t.Fatalf("claim_result=1 应判 claimed：%+v %v", rep, err)
	}
}

// TestCheckinInvalidTimezoneIsBusinessError：invalid timezone_id 也是 200，必须查 base_resp。
func TestCheckinInvalidTimezoneIsBusinessError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"base_resp":{"status_code":2013,"status_msg":"invalid timezone_id"},"data":{}}`)
	})
	rep, err := c.DailyCheckinReport(testAuth())
	if err != nil {
		t.Fatalf("业务码失败是「未达成」而非传输错误：%v", err)
	}
	if rep.Status != provider.CheckinError || !strings.Contains(rep.Msg, "invalid timezone_id") {
		t.Fatalf("应报业务码失败：%+v", rep)
	}
}

func TestRefreshToken(t *testing.T) {
	var form url.Values
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("OAuth 必须走 form-urlencoded，实得 %q", ct)
		}
		raw, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(raw))
		if r.URL.Path != EpToken {
			t.Errorf("续期路径 = %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"access_token":"mmoat_new","refresh_token":"mmort_new","token_type":"bearer","expires_in":3600,"scope":"agent.default"}`)
	})
	a := testAuth()
	before := time.Now()
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "mmort_FAKE_refresh_for_test" ||
		form.Get("client_id") != "mcode-public" || form.Get("scope") != "agent.default" || form.Get("audience") != "agent-backend" {
		t.Errorf("续期表单字段错误：%v", form)
	}
	if a.AccessToken != "mmoat_new" || a.RefreshToken != "mmort_new" {
		t.Errorf("token 未回写：%q / %q", a.AccessToken, a.RefreshToken)
	}
	// expires_in 单位是**秒**（协议 §3.6：access_token 不是 JWT，只能由此写入）。
	if a.ExpiresAt < before.Add(3500*time.Second).Unix() || a.ExpiresAt > before.Add(3700*time.Second).Unix() {
		t.Errorf("ExpiresAt = %d，期望 ≈ now+3600s", a.ExpiresAt)
	}
}

// TestRefreshTokenTerminal：错误文本命中 invalid_grant/expired/revoked → ErrSessionDead 语义。
func TestRefreshTokenTerminal(t *testing.T) {
	for _, body := range []string{
		`{"error":"invalid_grant","error_description":"refresh token revoked"}`,
		`{"status":"expired"}`,
	} {
		c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, body)
		})
		err := c.RefreshToken(testAuth())
		var pe *provider.Error
		if !errors.As(err, &pe) || pe.Kind != provider.ErrSessionDead {
			t.Fatalf("期望 *provider.Error{ErrSessionDead}，实得 %#v（body=%s）", err, body)
		}
	}
}

// TestRefreshTokenMissingExpiresInIsError：没有 expires_in 时宁可报错，也不留假到期时刻。
func TestRefreshTokenMissingExpiresInIsError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"mmoat_new","refresh_token":"mmort_new","token_type":"bearer"}`)
	})
	if err := c.RefreshToken(testAuth()); err == nil {
		t.Fatal("缺 expires_in 必须报错（access_token 不是 JWT，没有第二个到期来源）")
	}
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   provider.ErrKind
	}{
		{http.StatusPaymentRequired, `{"error":"insufficient balance"}`, provider.ErrHardCredit},
		{http.StatusTooManyRequests, `{"error":"quota exceeded, please retry"}`, provider.ErrSoftRate},
		{http.StatusUnauthorized, ``, provider.ErrSessionDead},
		{http.StatusForbidden, ``, provider.ErrSessionDead},
		{http.StatusInternalServerError, ``, provider.ErrServer},
		{http.StatusBadGateway, ``, provider.ErrServer},
		{http.StatusNotFound, ``, provider.ErrNotFound},
		{http.StatusBadRequest, `{"code":2013,"message":"requires adaptive thinking"}`, provider.ErrBadParams},
		{http.StatusBadRequest, `{"code":2013,"message":"tool call result does not follow tool call"}`, provider.ErrBadParams},
		{http.StatusBadRequest, `{"code":2013,"message":"unsupported content type 'image_url'"}`, provider.ErrBadParams},
		{http.StatusBadRequest, `{"code":"bad_params","message":"本地拒绝"}`, provider.ErrBadParams},
		{http.StatusBadRequest, `{"error":"some other problem"}`, provider.ErrClient},
		{http.StatusOK, ``, provider.ErrNone},
	}
	for _, tc := range cases {
		if got := Classify(tc.status, tc.body); got != tc.want {
			t.Errorf("Classify(%d, %q) = %v，期望 %v", tc.status, tc.body, got, tc.want)
		}
	}
	// 429 必须优先于「余额」文案判定（否则限流会被误判成欠费 → 长冷却）。
	if got := Classify(http.StatusTooManyRequests, `{"message":"insufficient balance / quota exceeded"}`); got != provider.ErrSoftRate {
		t.Errorf("429 应优先判软限流，实得 %v", got)
	}
}
