package upstream

import (
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

func TestPrepareBodyForcesStream(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"glm-5.2","messages":[]}`))
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["stream"] != true {
		t.Errorf("stream=%v", m["stream"])
	}
}

func TestPrepareBodyToolChoiceFunctionObject(t *testing.T) {
	out := PrepareBody([]byte(`{"tool_choice":{"type":"function","function":{"name":"get_weather"}},"tools":[{"type":"function"}]}`))
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["tool_choice"] != "get_weather" {
		t.Errorf("tool_choice=%v", m["tool_choice"])
	}
	if _, ok := m["tools"]; !ok {
		t.Error("tools should be kept for function choice")
	}
}

func TestPrepareBodyToolChoiceNone(t *testing.T) {
	for _, in := range []string{
		`{"tool_choice":"none","tools":[{}],"functions":[{}]}`,
		`{"tool_choice":{"type":"none"},"tools":[{}]}`,
	} {
		out := PrepareBody([]byte(in))
		var m map[string]any
		json.Unmarshal(out, &m)
		if _, ok := m["tool_choice"]; ok {
			t.Errorf("%s: tool_choice should be deleted", in)
		}
		if _, ok := m["tools"]; ok {
			t.Errorf("%s: tools should be deleted", in)
		}
		if _, ok := m["functions"]; ok {
			t.Errorf("%s: functions should be deleted", in)
		}
	}
}

func TestPrepareBodyToolChoiceAuto(t *testing.T) {
	out := PrepareBody([]byte(`{"tool_choice":{"type":"auto"}}`))
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["tool_choice"] != "auto" {
		t.Errorf("tool_choice=%v", m["tool_choice"])
	}
}

func TestPrepareBodyInvalidJSON(t *testing.T) {
	in := []byte(`{broken`)
	out := PrepareBody(in)
	if string(out) != string(in) {
		t.Error("invalid json should pass through unchanged")
	}
}

func TestPrepareBodyMaxCompletionTokensTranslated(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"deepseek-v4-pro","max_completion_tokens":16000,"messages":[{"role":"user","content":"x"}]}`))
	var m map[string]any
	json.Unmarshal(out, &m)
	if _, has := m["max_completion_tokens"]; has {
		t.Error("alias max_completion_tokens should be removed")
	}
	if m["max_tokens"] != float64(16000) {
		t.Errorf("max_tokens=%v want 16000", m["max_tokens"])
	}
}

// 显式 max_tokens 优先：别名只删不译。
func TestPrepareBodyMaxCompletionTokensExplicitMaxTokensWins(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"deepseek-v4-pro","max_tokens":32000,"max_completion_tokens":16000,"messages":[{"role":"user","content":"x"}]}`))
	var m map[string]any
	json.Unmarshal(out, &m)
	if _, has := m["max_completion_tokens"]; has {
		t.Error("alias should be dropped")
	}
	if m["max_tokens"] != float64(32000) {
		t.Errorf("max_tokens=%v want 32000 (explicit kept)", m["max_tokens"])
	}
}

// 非法别名值（0/负数/小数字符串畸形）不翻译，别名照删。
func TestPrepareBodyMaxCompletionTokensInvalidUntranslated(t *testing.T) {
	for _, src := range []string{
		`{"max_completion_tokens":0,"messages":[{"role":"user","content":"x"}]}`,
		`{"max_completion_tokens":-5,"messages":[{"role":"user","content":"x"}]}`,
		`{"max_completion_tokens":"16000","messages":[{"role":"user","content":"x"}]}`,
	} {
		out := PrepareBody([]byte(src))
		var m map[string]any
		json.Unmarshal(out, &m)
		if _, has := m["max_completion_tokens"]; has {
			t.Errorf("%s: alias should be dropped", src)
		}
		if _, has := m["max_tokens"]; has {
			t.Errorf("%s: invalid alias should NOT translate to max_tokens", src)
		}
	}
}

// Responses 协议别名 max_output_tokens：正整数继承为 max_tokens；null/0
// （客户端用「未设置」表达）一律删除，不再透传给上游触发整数下限校验（11133）。
func TestPrepareBodyMaxOutputTokensConverged(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want any // nil 表示不应出现 max_tokens
	}{
		{"正整数继承", `{"max_output_tokens":8192,"messages":[{"role":"user","content":"x"}]}`, float64(8192)},
		{"null 删除", `{"max_output_tokens":null,"messages":[{"role":"user","content":"x"}]}`, nil},
		{"0 删除", `{"max_output_tokens":0,"messages":[{"role":"user","content":"x"}]}`, nil},
		{"显式 max_tokens 优先", `{"max_tokens":32000,"max_output_tokens":8192,"messages":[{"role":"user","content":"x"}]}`, float64(32000)},
		{"max_tokens 为 null 时由别名顶替", `{"max_tokens":null,"max_output_tokens":8192,"messages":[{"role":"user","content":"x"}]}`, float64(8192)},
		{"max_tokens 为 0 时由别名顶替", `{"max_tokens":0,"max_output_tokens":8192,"messages":[{"role":"user","content":"x"}]}`, float64(8192)},
		{"max_completion_tokens 优先于 max_output_tokens", `{"max_completion_tokens":16000,"max_output_tokens":8192,"messages":[{"role":"user","content":"x"}]}`, float64(16000)},
		{"非整数值删除", `{"max_output_tokens":8.5,"messages":[{"role":"user","content":"x"}]}`, nil},
	}
	for _, c := range cases {
		out := PrepareBody([]byte(c.src))
		var m map[string]any
		json.Unmarshal(out, &m)
		if _, has := m["max_output_tokens"]; has {
			t.Errorf("%s: alias should be dropped", c.name)
		}
		if c.want == nil {
			if v, has := m["max_tokens"]; has {
				t.Errorf("%s: max_tokens 不应出现，得到 %v", c.name, v)
			}
			continue
		}
		if m["max_tokens"] != c.want {
			t.Errorf("%s: max_tokens=%v want %v", c.name, m["max_tokens"], c.want)
		}
	}
}

// 低于上游下限（16）的输出上限按「未设置」下发：1/2/8/15 实测被上游 11133
// integer_below_min_value 拒绝，而完全不下发该字段实测 200（走上游默认），
// 因此删掉比抬到 16 更安全——抬下限会把要长回复的请求截成十几 token。
func TestPrepareBodyDropsBelowUpstreamFloor(t *testing.T) {
	for _, n := range []string{"1", "2", "8", "15"} {
		out := PrepareBody([]byte(`{"max_tokens":` + n + `,"messages":[{"role":"user","content":"x"}]}`))
		var m map[string]any
		json.Unmarshal(out, &m)
		if v, has := m["max_tokens"]; has {
			t.Errorf("max_tokens=%s 应删除（走上游默认），得到 %v", n, v)
		}
	}
	for _, tc := range []struct {
		in   string
		want float64
	}{{"16", 16}, {"32000", 32000}, {"131072", 131072}} {
		out := PrepareBody([]byte(`{"max_tokens":` + tc.in + `,"messages":[{"role":"user","content":"x"}]}`))
		var m map[string]any
		json.Unmarshal(out, &m)
		if m["max_tokens"] != tc.want {
			t.Errorf("max_tokens=%s 不应被改动，得到 %v", tc.in, m["max_tokens"])
		}
	}
	// 别名越界同样删除（不再翻译成越界的 max_tokens）。
	out := PrepareBody([]byte(`{"max_output_tokens":1,"messages":[{"role":"user","content":"x"}]}`))
	var m map[string]any
	json.Unmarshal(out, &m)
	if v, has := m["max_tokens"]; has {
		t.Errorf("别名 1 应删除，得到 %v", v)
	}
}

// 缺失时补 stream_options include_usage；显式带了则不覆盖。
func TestPrepareBodyStreamOptionsDefault(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"glm-5.2","messages":[]}`))
	var m map[string]any
	json.Unmarshal(out, &m)
	so, ok := m["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options=%v want map", m["stream_options"])
	}
	if so["include_usage"] != true {
		t.Errorf("include_usage=%v want true", so["include_usage"])
	}
}

func TestPrepareBodyStreamOptionsExplicitPreserved(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"glm-5.2","stream_options":{"include_usage":false},"messages":[]}`))
	var m map[string]any
	json.Unmarshal(out, &m)
	so, _ := m["stream_options"].(map[string]any)
	if so["include_usage"] != false {
		t.Errorf("explicit stream_options should be preserved, got %v", m["stream_options"])
	}
}

const sseFixture = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"，世界\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
	"data: [DONE]\n\n"

func TestAggregate(t *testing.T) {
	resp, err := Aggregate(strings.NewReader(sseFixture))
	if err != nil {
		t.Fatal(err)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	if resp["model"] != "glm-5.2" {
		t.Errorf("model=%v", resp["model"])
	}
	choices := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好，世界" {
		t.Errorf("content=%q", msg["content"])
	}
	if msg["role"] != "assistant" {
		t.Errorf("role=%v", msg["role"])
	}
	if choices[0].(map[string]any)["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v", choices[0].(map[string]any)["finish_reason"])
	}
	usage := resp["usage"].(map[string]any)
	if usage["total_tokens"].(float64) != 7 {
		t.Errorf("usage=%v", usage)
	}
}

func TestAggregateSkipsNonDataLines(t *testing.T) {
	raw := ": comment\n\n" + sseFixture
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好，世界" {
		t.Errorf("content=%q", msg["content"])
	}
}

func TestAggregateToolCalls(t *testing.T) {
	// 流式 tool_calls：首片带 id/type/name + 空 arguments，后续只带 arguments 片段
	raw := `data: {"id":"x1","model":"deepseek-v4-pro","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"","tool_calls":[{"id":"call_a","type":"function","function":{"name":"get_weather","arguments":""},"index":0}]}}],"usage":null}

data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"{\"city\":"},"index":0}]}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"\"北京\"}"},"index":0}]}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"total_tokens":11}}

data: [DONE]

`
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls=%#v", msg["tool_calls"])
	}
	if calls[0]["id"] != "call_a" || calls[0]["type"] != "function" {
		t.Errorf("call meta=%v", calls[0])
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("fn.name=%v", fn["name"])
	}
	if fn["arguments"] != `{"city":"北京"}` {
		t.Errorf("fn.arguments=%q", fn["arguments"])
	}
}

func TestStreamPassthrough(t *testing.T) {
	rec := httptest.NewRecorder()
	err := StreamCapture(rec, strings.NewReader(sseFixture), nil)
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body missing chunks: %q", body)
	}
	// 逐行仍是合法 SSE（每行以 data: 开头或是空行）
	for _, ln := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		if ln != "" && !strings.HasPrefix(ln, "data: ") {
			t.Errorf("bad line: %q", ln)
		}
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type=%q", ct)
	}
}

// liveUpstreamSSEFixture 线上实抓的 WorkBuddy 上游形态（2026-09-18，deepseek-v4.1-flash）：
//   - 首帧 role-only，且 finish_reason 是**空字符串**（非 OpenAI 规范的 null）；
//   - 中间帧同样带 finish_reason:""，并夹带空 content/reasoning_content/refusal、
//     空 tool_calls 列表、null function_call、上游私有 extra_fields；
//   - 末帧才给 finish_reason:"stop" + usage；
//   - 显式 [DONE] 收尾。
//
// 该形态是本仓库 normalizeFrame 存在的直接原因（回归锚点，勿删）。
const liveUpstreamSSEFixture = "" +
	`data: {"id":"5fbd114d1bb140658cbd5a90e9b41e05","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","created":1789679930,"choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":""}],"usage":null}` + "\n\n" +
	`data: {"id":"5fbd114d1bb140658cbd5a90e9b41e05","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","created":1789679930,"choices":[{"index":0,"delta":{"content":"Hi","reasoning_content":"","function_call":null,"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":""}],"usage":null}` + "\n\n" +
	`data: {"id":"5fbd114d1bb140658cbd5a90e9b41e05","model":"deepseek-v4.1-flash","object":"chat.completion.chunk","created":1789679930,"choices":[{"index":0,"delta":{"role":"assistant","content":"","reasoning_content":"","function_call":{"name":"","arguments":""},"refusal":"","tool_calls":[],"extra_fields":null},"logprobs":null,"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":10,"total_tokens":15}}` + "\n\n" +
	"data: [DONE]\n\n"

// TestStreamNormalizesEmptyFinishReason 回归：上游把「流进行中」编码成 finish_reason:""，
// 而不是规范要求的 null。若原样透传，下游按 Option<String> 反序列化的网关（实测
// cc-switch v3.20.3 的 OpenAI→Anthropic 转换器）会把首帧的 "" 当成一次真实的
// finish_reason 消费掉其「只发一次 message_delta」的去重标志位，导致末帧真正的
// "stop" 走去重分支被跳过，连带跳过 content_block_stop——Claude Code 侧表现为
// 「有思考、token 也计费，但回复空白」。
//
// 归一为 null 后中间帧不再命中「存在 finish_reason」分支，去重标志位留给末帧。
func TestStreamNormalizesEmptyFinishReason(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := StreamCapture(rec, strings.NewReader(liveUpstreamSSEFixture), nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	body := rec.Body.String()

	if strings.Contains(body, `"finish_reason":""`) {
		t.Errorf("empty finish_reason leaked downstream (breaks cc-switch dedup):\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":null`) {
		t.Errorf("in-progress frames should carry finish_reason:null:\n%s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("real stop reason must survive normalization:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("missing [DONE] terminator:\n%s", body)
	}
	// 正文不得被规范化吞掉
	if !strings.Contains(body, `"content":"Hi"`) {
		t.Errorf("content lost during normalization:\n%s", body)
	}
}

// TestStreamStripsUpstreamNoise 上游噪声帧经规范化后只剩规范字段：
// 空 content/reasoning_content/refusal、空 tool_calls 列表、null/空 function_call、
// 私有 extra_fields、logprobs 等一律剔除；usage 缺失补 null。
func TestStreamStripsUpstreamNoise(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := StreamCapture(rec, strings.NewReader(liveUpstreamSSEFixture), nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for _, noise := range []string{
		`"extra_fields"`, `"logprobs"`, `"refusal"`, `"function_call"`,
		`"reasoning_content"`, `"tool_calls"`,
	} {
		if strings.Contains(rec.Body.String(), noise) {
			t.Errorf("noise field %s should be stripped:\n%s", noise, rec.Body.String())
		}
	}
	// 首个 role-only 帧：delta 只保留 role
	frames := []map[string]any{}
	for _, ln := range strings.Split(rec.Body.String(), "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "data: ") || strings.Contains(ln, "[DONE]") {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(ln, "data: ")), &obj); err != nil {
			t.Fatalf("bad frame %q: %v", ln, err)
		}
		frames = append(frames, obj)
	}
	if len(frames) == 0 {
		t.Fatal("no frames emitted")
	}
	ch := frames[0]["choices"].([]any)[0].(map[string]any)
	delta := ch["delta"].(map[string]any)
	if len(delta) != 1 || delta["role"] != "assistant" {
		t.Errorf("role-only frame delta should be exactly {role}, got %#v", delta)
	}
	if ch["finish_reason"] != nil {
		t.Errorf("frame1 finish_reason=%v want null", ch["finish_reason"])
	}
}

// TestStreamEmptyUpstreamErrors 上游 200 但无有效数据帧 → errEmptyStream 哨兵
// （而非合成空 content 假成功），供 handler 记 502 upstream_parse。
func TestStreamEmptyUpstreamErrors(t *testing.T) {
	for _, raw := range []string{
		"data: [DONE]\n\n",
		": keep-alive comment\n\n",
		"",
	} {
		rec := httptest.NewRecorder()
		err := StreamCapture(rec, strings.NewReader(raw), nil)
		if err == nil {
			t.Errorf("raw=%q: want errEmptyStream, got nil", raw)
			continue
		}
		if !errors.Is(err, errEmptyStream) {
			t.Errorf("raw=%q: want IsEmptyStreamError, got %v", raw, err)
		}
		if !strings.Contains(rec.Body.String(), "data: [DONE]") {
			t.Errorf("raw=%q: empty stream must still close with [DONE]", raw)
		}
	}
}

// 回归（issue #42）：通用流式路径（WorkBuddy/WorkBuddyAI/oczen 共用）在「可证实的
// 截断」时不得补 [DONE] 伪装成成功收尾。
//   - 传输层读错误 / 断在半个帧上（末行无换行）→ 发 error 帧、不补 [DONE]；
//     错误码按成因细分：空闲超时 upstream_timeout，其余 upstream_stream_error。
//   - 正常 [DONE] 收尾、EOF 但帧完整且漏发 [DONE] → 行为不变，仍补 [DONE]。
func TestStreamTruncationNotDisguisedAsDone(t *testing.T) {
	full := `data: {"id":"x","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash","arguments":"{\"cmd\""}}]}}]}` + "\n\n"

	mkStream := func(rd io.Reader) (string, error) {
		rec := httptest.NewRecorder()
		err := StreamCapture(rec, rd, nil)
		return rec.Body.String(), err
	}

	// 1) 传输层读错误
	body, err := mkStream(&readErrAfter{payload: full})
	if err == nil {
		t.Error("read-error: want err != nil")
	}
	if strings.Contains(body, "[DONE]") || !strings.Contains(body, "upstream_stream_error") {
		t.Errorf("read-error: 截断不得补 [DONE]，应发 error 帧\nbody=%s", body)
	}

	// 1b) 空闲超时：错误码要能区分出来（upstream_timeout）
	body, err = mkStream(&readErrAfter{payload: full, err: provider.ErrIdleTimeout})
	if err == nil {
		t.Error("idle: want err != nil")
	}
	if strings.Contains(body, "[DONE]") || !strings.Contains(body, "upstream_timeout") {
		t.Errorf("idle: 空闲超时应发 upstream_timeout 帧且不补 [DONE]\nbody=%s", body)
	}

	// 2) 断在半个帧上
	body, err = mkStream(&cutStream{payload: full, keep: len(full) - 12})
	if err == nil {
		t.Error("cut: want err != nil")
	}
	if strings.Contains(body, "[DONE]") || !strings.Contains(body, "upstream_stream_error") {
		t.Errorf("cut: 截断不得补 [DONE]，应发 error 帧\nbody=%s", body)
	}

	// 3) 正常 [DONE] 收尾
	body, err = mkStream(strings.NewReader(full + "data: [DONE]\n\n"))
	if err != nil {
		t.Fatalf("normal: %v", err)
	}
	if !strings.Contains(body, "[DONE]") || strings.Contains(body, "truncated") {
		t.Errorf("normal: 应恰好一个 [DONE] 且无 error 帧\nbody=%s", body)
	}

	// 4) EOF 但帧完整、上游漏发 [DONE]（向后兼容兜底）
	body, err = mkStream(strings.NewReader(full))
	if err != nil {
		t.Fatalf("eof-no-done: %v", err)
	}
	if !strings.Contains(body, "[DONE]") || strings.Contains(body, "truncated") {
		t.Errorf("eof-no-done: 仍应兜底补 [DONE]\nbody=%s", body)
	}
}

type readErrAfter struct {
	payload string
	err     error
	sent    bool
}

func (r *readErrAfter) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.payload), nil
	}
	if r.err != nil {
		return 0, r.err
	}
	return 0, errors.New("simulated: broken pipe")
}

type cutStream struct{ payload string; keep, off int }

func (r *cutStream) Read(p []byte) (int, error) {
	if r.off >= r.keep {
		return 0, io.EOF
	}
	n := copy(p, r.payload[r.off:r.keep])
	r.off += n
	return n, nil
}
