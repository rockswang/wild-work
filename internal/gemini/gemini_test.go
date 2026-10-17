// gemini_test.go 假上游回归测试：**全程 httptest，不发任何真实网络请求**
// （AGENTS.md §0.1：验证上游行为一律最小化请求数，本文件连一次都不打）。
//
// 覆盖本渠道每一条"容易写错"的协议事实：
//   - 身份五头齐全、**不带** x-goog-api-key / Accept；
//   - 信封**逐层字母序**序列化（逐字节断言，含 tools schema 与 args 的键序）；
//   - 档位后缀是准入钥匙（`gemini-3.8-flash-<tier>`）、tiered 不发 thinkingBudget；
//   - SSE：单行 data、两种载荷形态（信封 / 裸 Response）、`data: [DONE]` 收尾、
//     思考走 reasoning_content、截断尾帧不丢 finishReason、空响应抛错；
//   - 配额是**窗口百分比**（只认两个 bucketId、resetTime 不可解析则整条丢弃）；
//   - project 探测（固定走 sandbox、探测失败不发推理、真值回写 Extra）；
//   - Classify 判据顺序（429 先于 quota 文案）；
//   - RefreshToken 带 client_secret 且轮换 refresh_token 必须回写。
package gemini

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// fixedHooks 把时间/随机/sessionId 派生钉死，让出站字节可逐字节断言。
func fixedHooks(t *testing.T) {
	t.Helper()
	oldNow, oldRand, oldDerive := nowMillisFn, randomHexFn, deriveSessionIDFn
	nowMillisFn = func() int64 { return 1700000000000 }
	randomHexFn = func(n int) string { return strings.Repeat("ab", n) }
	deriveSessionIDFn = func(project, firstUserText, lane string, generation int) string {
		return "12345"
	}
	t.Cleanup(func() { nowMillisFn, randomHexFn, deriveSessionIDFn = oldNow, oldRand, oldDerive })
}

// testAuth 构造一个"已有 project、不需探测"的账号（避免测试里意外多发一次探测请求）。
func testAuth() *auth.Auth {
	return &auth.Auth{
		Kind:         Kind.String(),
		AccessToken:  "ya29.FAKE_access_for_test",
		RefreshToken: "1//FAKE_refresh_for_test",
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		UID:          "sub-fake-1",
		Nickname:     "tester@example.com",
		Extra:        map[string]string{ExtraKeyProject: "aicode-consumers"},
	}
}

// newTestClient 起一个假上游，所有端点都指向它。
func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewWithBase(srv.URL)
}

// sse 把若干载荷拼成合法的 SSE 文本（每帧单行 `data:` + 空行，协议 §2.5 的形态）。
func sse(payloads ...string) string {
	var b strings.Builder
	for _, p := range payloads {
		b.WriteString("data: ")
		b.WriteString(p)
		b.WriteString("\n\n")
	}
	return b.String()
}

// frames 解析 OpenAI SSE 文本里的所有 data 载荷（跳过 [DONE]）。
func frames(t *testing.T, out string) []map[string]any {
	t.Helper()
	var got []map[string]any
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			t.Fatalf("帧不是合法 JSON：%v（%s）", err, payload)
		}
		got = append(got, obj)
	}
	return got
}

// tail 返回字符串最后 n 个字符（用于错误信息里展示流的结尾）。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ---------------------------------------------------------------------------
// 出站：身份头 + 字母序信封
// ---------------------------------------------------------------------------

// envelopeInput 固定输入：system + user + 一个带 schema 的工具 + temperature/max_tokens。
const envelopeInput = `{"model":"gemini/gemini-3.8-flash","reasoning_effort":"low","max_tokens":123,` +
	`"temperature":0.7,"messages":[{"role":"system","content":"S"},{"role":"user","content":"hi"}],` +
	`"tools":[{"type":"function","function":{"name":"f","description":"d",` +
	`"parameters":{"type":"object","properties":{"b":{"type":"string"},"a":{"type":"integer"}},` +
	`"required":["a"],"additionalProperties":false}}}]}`

// TestEnvelopeAlphabeticalBytes 逐字节断言出站信封：**每一层都按键名字典序**。
//
// 这是本渠道最容易被写错的一处（协议 §2.2：信封必须递归按键名字典序序列化）。
// 期望串里含两处"看起来像写反了"的顺序，必须保持：
//   - `sessionId` < `systemInstruction`（第 2 字符 e < y）；
//   - `toolConfig` < `tools`（第 5 字符大写 'C' 0x43 < 小写 's' 0x73）。
func TestEnvelopeAlphabeticalBytes(t *testing.T) {
	fixedHooks(t)
	out, code, err := buildInternalBody([]byte(envelopeInput), "aicode-consumers")
	if err != nil {
		t.Fatalf("buildInternalBody 失败：%v（code=%s）", err, code)
	}
	want := `{"model":"gemini-3.8-flash-low","project":"aicode-consumers","request":{` +
		`"contents":[{"parts":[{"text":"hi"}],"role":"user"}],` +
		`"generationConfig":{"maxOutputTokens":123,"temperature":0.7,` +
		`"thinkingConfig":{"includeThoughts":true,"thinkingBudget":1000}},` +
		`"sessionId":"12345",` +
		`"systemInstruction":{"parts":[{"text":"S"}],"role":"system"},` +
		`"toolConfig":{"functionCallingConfig":{"mode":"AUTO"}},` +
		`"tools":[{"functionDeclarations":[{"description":"d","name":"f",` +
		`"parameters":{"properties":{"a":{"type":"integer"},"b":{"type":"string"}},"required":["a"],"type":"object"}}]}]},` +
		`"requestId":"agent/1700000000000/abababab","userAgent":"antigravity"}`
	if string(out) != want {
		t.Fatalf("信封字节不一致\n得到：%s\n期望：%s", out, want)
	}
	// 工具 schema 的非白名单键必须整键删除（上游对未知键是硬 400）。
	if strings.Contains(string(out), "additionalProperties") {
		t.Errorf("工具 schema 的非白名单键必须整键删除：%s", out)
	}
}

// TestTierSuffixIsAdmissionKey 模型名必须带档位后缀（裸名上游 404）。
func TestTierSuffixIsAdmissionKey(t *testing.T) {
	fixedHooks(t)
	cases := []struct {
		effort     string
		wantModel  string
		wantBudget bool
		wantValue  float64
	}{
		{"", "gemini-3.8-flash-medium", true, 4000},
		{"low", "gemini-3.8-flash-low", true, 1000},
		{"MEDIUM", "gemini-3.8-flash-medium", true, 4000},
		{"high", "gemini-3.8-flash-high", true, 10000},
		{"tiered", "gemini-3.8-flash-tiered", false, 0}, // tiered 只发 includeThoughts
		{"weird", "gemini-3.8-flash-medium", true, 4000},
	}
	for _, tc := range cases {
		body := `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"hi"}]`
		if tc.effort != "" {
			body += `,"reasoning_effort":"` + tc.effort + `"`
		}
		body += `}`
		out, code, err := buildInternalBody([]byte(body), DefaultProject)
		if err != nil {
			t.Fatalf("effort=%q buildInternalBody 失败：%v（code=%s）", tc.effort, err, code)
		}
		var env struct {
			Model   string `json:"model"`
			Request struct {
				GenerationConfig struct {
					ThinkingConfig map[string]any `json:"thinkingConfig"`
				} `json:"generationConfig"`
			} `json:"request"`
		}
		if err := json.Unmarshal(out, &env); err != nil {
			t.Fatalf("信封解析失败：%v", err)
		}
		if env.Model != tc.wantModel {
			t.Errorf("effort=%q model=%q，期望 %q", tc.effort, env.Model, tc.wantModel)
		}
		thinkCfg := env.Request.GenerationConfig.ThinkingConfig
		if thinkCfg["includeThoughts"] != true {
			t.Errorf("effort=%q includeThoughts 必须恒 true：%v", tc.effort, thinkCfg)
		}
		budget, hasBudget := thinkCfg["thinkingBudget"]
		if hasBudget != tc.wantBudget {
			t.Errorf("effort=%q thinkingBudget 存在性=%v，期望 %v（%v）", tc.effort, hasBudget, tc.wantBudget, thinkCfg)
		}
		if hasBudget && budget.(float64) != tc.wantValue {
			t.Errorf("effort=%q thinkingBudget=%v，期望 %v", tc.effort, budget, tc.wantValue)
		}
	}
}

// TestChatStreamIdentityHeaders 身份五头齐全、且**不带** x-goog-api-key / Accept / x-goog-api-client。
func TestChatStreamIdentityHeaders(t *testing.T) {
	fixedHooks(t)
	var (
		gotPath string
		gotHdr  http.Header
	)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotHdr = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`, "[DONE]"))
	})
	rc, status, body, err := c.ChatStream(testAuth(), []byte(envelopeInput))
	if err != nil || status != 200 {
		t.Fatalf("ChatStream status=%d err=%v body=%s", status, err, body)
	}
	defer rc.Close()
	_, _ = io.ReadAll(rc)

	if gotPath != EpStreamGenerate+StreamQuery {
		t.Errorf("出站路径 = %q，期望 %q", gotPath, EpStreamGenerate+StreamQuery)
	}
	want := map[string]string{
		"Authorization":      "Bearer ya29.FAKE_access_for_test",
		"Content-Type":       "application/json",
		"User-Agent":         "antigravity/4.3.0 (cmdc-pak)",
		"X-Client-Name":      "antigravity",
		"X-Client-Version":   "4.3.0",
		"X-Machine-Id":       "cmdc-pak",
		"X-Vscode-Sessionid": "proxy",
	}
	for k, v := range want {
		if gotHdr.Get(k) != v {
			t.Errorf("头 %s = %q，期望 %q", k, gotHdr.Get(k), v)
		}
	}
	// 逐字身份五头之外，**不得**注入这些头（原版不带，多一个就多一个识别特征）。
	for _, banned := range []string{"X-Goog-Api-Key", "X-Goog-Api-Client", "Accept"} {
		if v := gotHdr.Get(banned); v != "" {
			t.Errorf("不得注入 %s（得到 %q）", banned, v)
		}
	}
}

// TestChatStreamLocalBadParams 未知模型 → 本地 400 + bad_params（不罚账号、不轮转）。
func TestChatStreamLocalBadParams(t *testing.T) {
	fixedHooks(t)
	var hits int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { hits++ })
	for _, model := range []string{"gemini-3.8-flash-lite", "totally-bogus"} {
		body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
		rc, status, respBody, err := c.ChatStream(testAuth(), []byte(body))
		if err != nil || rc != nil {
			t.Fatalf("model=%s 期望本地 400，得 status=%d err=%v", model, status, err)
		}
		if status != http.StatusBadRequest {
			t.Errorf("model=%s status=%d，期望 400", model, status)
		}
		if !provider.CodeMarker(strings.ToLower(string(respBody)), errCodeBadParams) {
			t.Errorf("model=%s 错误体应带 code=bad_params：%s", model, respBody)
		}
		if got := Classify(status, string(respBody)); got != provider.ErrBadParams {
			t.Errorf("model=%s Classify=%s，期望 ErrBadParams", model, got)
		}
	}
	if hits != 0 {
		t.Errorf("本地拒绝**不得**打上游，实际命中 %d 次", hits)
	}
}

// ---------------------------------------------------------------------------
// 入站 SSE
// ---------------------------------------------------------------------------

// TestStreamBothEnvelopeShapesAndDone 两种载荷形态都认，且以 `data: [DONE]` 收尾。
func TestStreamBothEnvelopeShapesAndDone(t *testing.T) {
	src := ": keepalive\n" +
		"event: ignored\n" +
		sse(
			// 形态 A：信封 {"response":{…}}
			`{"response":{"candidates":[{"content":{"parts":[{"text":"he"}]}}]}}`,
			// 纯 usageMetadata 的收尾帧：**不算内容**，要继续读
			`{"response":{"usageMetadata":{"totalTokenCount":5,"promptTokenCount":2}}}`,
			// 形态 B：裸 Response（含思考 + finishReason + usage）
			`{"candidates":[{"content":{"parts":[{"text":"llo"},{"thought":true,"text":"thinking..."}]},`+
				`"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":3,`+
				`"totalTokenCount":13,"cachedContentTokenCount":4,"thoughtsTokenCount":2}}`,
			"[DONE]",
		)
	out, err := io.ReadAll(newGeminiStream(strings.NewReader(src), "gemini/gemini-3.8-flash"))
	if err != nil {
		t.Fatalf("读取转换流失败：%v", err)
	}
	text := string(out)
	if !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Fatalf("流必须以 `data: [DONE]` 收尾，得尾部 %q", tail(text, 40))
	}
	for _, want := range []string{`"content":"he"`, `"content":"llo"`, `"reasoning_content":"thinking..."`,
		`"finish_reason":"stop"`, `"prompt_tokens":10`, `"completion_tokens":3`, `"total_tokens":13`,
		`"cached_tokens":4`, `"reasoning_tokens":2`, `"model":"gemini/gemini-3.8-flash"`} {
		if !strings.Contains(text, want) {
			t.Errorf("转换结果缺少 %s\n%s", want, text)
		}
	}
	// 第一个内容帧必须是 role=assistant 的空帧（OpenAI 客户端依赖它建立消息）。
	fs := frames(t, text)
	if len(fs) == 0 {
		t.Fatal("没有任何帧")
	}
	first := fs[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if first["role"] != "assistant" {
		t.Errorf("首个 delta 应带 role=assistant：%v", first)
	}
}

// TestStreamKeepsTruncatedTail 被截断的流里最后一行没有换行符，仍必须处理
// （否则携带 finishReason / usageMetadata 的末帧会被静默丢弃）。
func TestStreamKeepsTruncatedTail(t *testing.T) {
	src := `data: {"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}` + "\n\n" +
		`data: {"candidates":[{"finishReason":"MAX_TOKENS"}],"usageMetadata":{"totalTokenCount":7,"promptTokenCount":4,"candidatesTokenCount":3}}`
	out, err := io.ReadAll(newGeminiStream(strings.NewReader(src), "gemini/gemini-3.8-flash"))
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	text := string(out)
	if !strings.Contains(text, `"finish_reason":"length"`) {
		t.Errorf("MAX_TOKENS 应映射成 length，且末帧不得丢：%s", text)
	}
	if !strings.Contains(text, `"total_tokens":7`) {
		t.Errorf("末帧 usage 不得丢：%s", text)
	}
	if !strings.HasSuffix(text, "data: [DONE]\n\n") {
		t.Errorf("正常 EOF 应兜底补 [DONE]：%s", text)
	}
}

// TestStreamEmptyResponseThrows 一个内容块都没有 ⇒ 抛错（静默收尾会让上层认为
// 「模型正常回答但没内容」而**不重试**）。
func TestStreamEmptyResponseThrows(t *testing.T) {
	src := sse(`{"response":{"usageMetadata":{"totalTokenCount":3}}}`, "[DONE]")
	_, err := io.ReadAll(newGeminiStream(strings.NewReader(src), "gemini/gemini-3.8-flash"))
	if err == nil || !strings.Contains(err.Error(), "未返回任何内容块") {
		t.Fatalf("空响应必须抛错，得 err=%v", err)
	}
}

// TestStreamToolCallAndFinish 工具调用独占一个块、参数**一次给全**且按键名字典序。
func TestStreamToolCallAndFinish(t *testing.T) {
	src := sse(
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"read_file","args":{"path":"/x","line":3}}}]}}]}`,
		`{"candidates":[{"finishReason":"STOP"}]}`,
		"[DONE]",
	)
	out, err := io.ReadAll(newGeminiStream(strings.NewReader(src), "gemini/gemini-3.8-flash"))
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	text := string(out)
	// args 的键序必须是 line < path（递归字母序）。
	if !strings.Contains(text, `"arguments":"{\"line\":3,\"path\":\"/x\"}"`) {
		t.Errorf("工具参数必须是一次给全且键名字典序：%s", text)
	}
	if !strings.Contains(text, `"name":"read_file"`) {
		t.Errorf("缺工具名：%s", text)
	}
	if !strings.Contains(text, `"finish_reason":"tool_calls"`) {
		t.Errorf("有工具调用时 finish_reason 应为 tool_calls：%s", text)
	}
}

// TestStreamThroughUpstreamLayer 经 internal/upstream 规范化后仍能拿到正文与 usage
// （证明本包吐出的确实是内层协议认得的 OpenAI SSE）。
func TestStreamThroughUpstreamLayer(t *testing.T) {
	src := sse(
		`{"response":{"candidates":[{"content":{"parts":[{"text":"你好"}]}}]}}`,
		`{"response":{"candidates":[{"content":{"parts":[{"text":"，世界"}]},"finishReason":"STOP"}],`+
			`"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9}}}`,
		"[DONE]",
	)
	rec := httptest.NewRecorder()
	usage, err := (&Client{}).Stream(rec, strings.NewReader(src), "gemini/gemini-3.8-flash")
	if err != nil {
		t.Fatalf("Stream 失败：%v", err)
	}
	if !strings.Contains(rec.Body.String(), "你好") || !strings.Contains(rec.Body.String(), "，世界") {
		t.Errorf("正文缺失：%s", rec.Body.String())
	}
	if usage == nil || usage["prompt_tokens"] != float64(7) {
		t.Errorf("usage 捕获失败：%v", usage)
	}
}

// ---------------------------------------------------------------------------
// 配额窗口
// ---------------------------------------------------------------------------

// TestQuotaPercentParsing 配额是**窗口剩余百分比**：只认两个 bucketId、
// resetTime 不可解析则整条丢弃、**必须**带 project。
func TestQuotaPercentParsing(t *testing.T) {
	var (
		gotBody    []byte
		gotHdr     http.Header
		quotaCalls int
	)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpQuotaSummary:
			quotaCalls++
			gotHdr = r.Header.Clone()
			gotBody, _ = io.ReadAll(r.Body)
			_, _ = io.WriteString(w, `{"groups":[`+
				`{"displayName":"Gemini Models","buckets":[`+
				`{"bucketId":"gemini-weekly","window":"weekly","resetTime":"2026-10-20T00:00:00Z","remainingFraction":0.5},`+
				`{"bucketId":"gemini-5h","window":"5h","resetTime":"2026-10-17T15:00:00Z","remainingFraction":0.99},`+
				`{"bucketId":"gemini-5h","window":"5h","resetTime":"not-a-time","remainingFraction":0.1}]},`+
				`{"displayName":"Claude and GPT models","buckets":[`+
				`{"bucketId":"3p-claude","window":"5h","resetTime":"2026-10-17T15:00:00Z","remainingFraction":1}]}]}`)
		case EpLoadCodeAssist:
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"aicode-consumers"}`)
		default:
			t.Errorf("意外的出站路径：%s", r.URL.Path)
		}
	})

	remain, items, err := c.UserResourceDetail(testAuth())
	if err != nil {
		t.Fatalf("UserResourceDetail 失败：%v", err)
	}
	// 本渠道没有「可消耗积分」：标量恒 0（配额由 InfoOnly 条目承载）。
	if remain != 0 {
		t.Errorf("UserResource 应恒 0（本渠道无积分概念），得 %d", remain)
	}
	if len(items) != 2 {
		t.Fatalf("应只认两个 bucketId（3p-* 与 resetTime 不可解析的桶都丢弃），得 %d 条：%+v",
			len(items), items)
	}
	fiveHour, weekly := items[0], items[1]
	if fiveHour.Key != BucketFiveHour || fiveHour.Remain != 99 || fiveHour.Total != 100 || fiveHour.Used != 1 {
		t.Errorf("5 小时窗口条目错误：%+v", fiveHour)
	}
	if fiveHour.Name != "5 小时窗口（%）" {
		t.Errorf("单位必须写进名字（ResourceItem 没有单位字段）：%q", fiveHour.Name)
	}
	if !fiveHour.Usable || !fiveHour.InfoOnly {
		t.Errorf("配额条目应 Usable=true（真实可用）+ InfoOnly=true（不是积分，不参与小计）：%+v", fiveHour)
	}
	// resetTime 2026-10-17T15:00:00Z 换算到 UTC+8 墙钟 = 23:00。
	if fiveHour.ExpireAt != "2026-10-17T23:00:00+08:00" {
		t.Errorf("ExpireAt 应按 UTC+8 墙钟：%q", fiveHour.ExpireAt)
	}
	if weekly.Key != BucketWeekly || weekly.Remain != 50 {
		t.Errorf("周窗口条目错误：%+v", weekly)
	}
	// ⚠️ 两个百分比相加**无意义**，故 Summarize 的可消耗小计必须为 0（不得显示 198 积分）。
	if usable, unusable := provider.Summarize(items); usable != 0 || unusable != 0 {
		t.Errorf("InfoOnly 条目不得进小计：usable=%d unusable=%d", usable, unusable)
	}
	if expiring := provider.ExpiringWithin(items, 24*time.Hour); expiring != 0 {
		t.Errorf("InfoOnly 条目不得进临期统计：%d", expiring)
	}

	// 请求体：字段名必须是 `project`（不是 cloudaicompanionProject）。
	var probe map[string]string
	if err := json.Unmarshal(gotBody, &probe); err != nil {
		t.Fatalf("配额请求体不是 JSON 对象：%s", gotBody)
	}
	if probe["project"] != "aicode-consumers" {
		t.Errorf("配额请求体 project=%q（body=%s）", probe["project"], gotBody)
	}
	if _, has := probe["cloudaicompanionProject"]; has {
		t.Errorf("字段名必须是 project：%s", gotBody)
	}
	// 身份头与推理**完全一致**。
	if gotHdr.Get("User-Agent") != IdentityUserAgent || gotHdr.Get("X-Machine-Id") != IdentityMachineID {
		t.Errorf("配额请求缺身份头：%v", gotHdr)
	}
	if quotaCalls != 1 {
		t.Fatalf("首次应恰好查一次配额，得 %d", quotaCalls)
	}
	// 第二次调用命中 60s 缓存 → **配额**不再打上游（档位探测仍会发，故只断言配额计数）。
	if _, _, err := c.UserResourceDetail(testAuth()); err != nil {
		t.Fatalf("第二次 UserResourceDetail 失败：%v", err)
	}
	if quotaCalls != 1 {
		t.Errorf("TTL 内应命中缓存，配额请求次数 %d", quotaCalls)
	}
}

// TestEndpointRouting 配额/探测**固定走 sandbox**，推理按 [daily, sandbox] 轮换。
func TestEndpointRouting(t *testing.T) {
	c := New()
	c.Daily, c.Sandbox = "https://daily.example", "https://sandbox.example"
	if got := c.sandboxHost(); got != "https://sandbox.example" {
		t.Fatalf("sandboxHost=%q", got)
	}
	if eps := c.endpoints(); len(eps) != 2 || eps[0] != c.Daily || eps[1] != c.Sandbox {
		t.Fatalf("端点轮换顺序应为 [daily, sandbox]，得 %v", eps)
	}
	// 测试用 NewWithBase 覆盖**全部**端点（含沙箱），保证不出真实网络。
	if got := NewWithBase("http://127.0.0.1:1").sandboxHost(); got != "http://127.0.0.1:1" {
		t.Errorf("NewWithBase 应覆盖沙箱端点，得 %q", got)
	}
}

// ---------------------------------------------------------------------------
// project 探测
// ---------------------------------------------------------------------------

// TestProjectProbeThenEnvelope 项目未知时先探测（sandbox）、把真值写进出站信封并回写凭据。
func TestProjectProbeThenEnvelope(t *testing.T) {
	fixedHooks(t)
	var (
		probeBody []byte
		envelopes []map[string]any
		probes    int
		streams   int
	)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpLoadCodeAssist:
			probes++
			probeBody, _ = io.ReadAll(r.Body)
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"CCP-TWO",`+
				`"paidTier":{"id":"g1-pro-tier","name":"Google AI Pro"}}`)
		case EpStreamGenerate:
			streams++
			raw, _ := io.ReadAll(r.Body)
			var env map[string]any
			_ = json.Unmarshal(raw, &env)
			envelopes = append(envelopes, env)
			_, _ = io.WriteString(w, sse(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`, "[DONE]"))
		default:
			t.Errorf("意外路径：%s", r.URL.Path)
		}
	})

	dir := t.TempDir()
	a := testAuth()
	a.Extra = nil // 项目未知 ⇒ 触发探测
	a.FilePath = filepath.Join(dir, "gemini-sub-fake-1.json")

	rc, status, body, err := c.ChatStream(a, []byte(envelopeInput))
	if err != nil || status != 200 {
		t.Fatalf("ChatStream status=%d err=%v body=%s", status, err, body)
	}
	_, _ = io.ReadAll(rc)
	_ = rc.Close()
	if probes != 1 || streams != 1 {
		t.Fatalf("应各发一次（探测 1 + 推理 1），得 probes=%d streams=%d", probes, streams)
	}
	// 探测请求体是**逐字常量**（多一个字段就可能被识别成非官方客户端）。
	if string(probeBody) != LoadCodeAssistBody {
		t.Errorf("loadCodeAssist 请求体必须逐字：%s", probeBody)
	}
	if len(envelopes) != 1 || envelopes[0]["project"] != "CCP-TWO" {
		t.Fatalf("出站信封必须带探测到的 project：%v", envelopes)
	}
	// 真值回写 Extra 并落盘（档位顺带落，供面板「账号规格」）。
	if a.Extra[ExtraKeyProject] != "CCP-TWO" || a.Extra[ExtraKeyPaidTierLabel] != "Pro" {
		t.Errorf("Extra 回写错误：%v", a.Extra)
	}
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatalf("落盘失败：%v", err)
	}
	saved, err := auth.Parse(raw)
	if err != nil {
		t.Fatalf("落盘文件不可被 auth.Parse 读取：%v", err)
	}
	if saved.Extra[ExtraKeyProject] != "CCP-TWO" {
		t.Errorf("落盘的 Extra 缺 project：%v", saved.Extra)
	}

	// 第二次请求：project 已在 Extra → **不再探测**。
	rc2, status2, _, err := c.ChatStream(a, []byte(envelopeInput))
	if err != nil || status2 != 200 {
		t.Fatalf("第二次 ChatStream status=%d err=%v", status2, err)
	}
	_, _ = io.ReadAll(rc2)
	_ = rc2.Close()
	if probes != 1 {
		t.Errorf("project 已知时不得重复探测，probes=%d", probes)
	}
}

// TestProjectProbeFailureBlocksInference 探测失败 ⇒ **不发推理**（协议 §2.7 的实测行为）。
func TestProjectProbeFailureBlocksInference(t *testing.T) {
	fixedHooks(t)
	var streams int
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpLoadCodeAssist:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"code":500,"message":"boom"}}`)
		case EpStreamGenerate:
			streams++
		}
	})
	a := testAuth()
	a.Extra = nil
	rc, status, body, err := c.ChatStream(a, []byte(envelopeInput))
	if err != nil || rc != nil {
		t.Fatalf("期望本地错误体，得 status=%d err=%v", status, err)
	}
	if status != http.StatusInternalServerError {
		t.Errorf("探测失败的 status 应透出 500，得 %d", status)
	}
	if !strings.Contains(string(body), "探测失败") {
		t.Errorf("错误体应说明未发推理：%s", body)
	}
	if streams != 0 {
		t.Errorf("探测失败时**不得**发推理，实际 %d 次", streams)
	}
}

// ---------------------------------------------------------------------------
// 分类与续期
// ---------------------------------------------------------------------------

// TestClassify 判据顺序：429 先于 quota 文案（不变量 15）；403/404 归 SERVER（协议 §2.5）。
func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   provider.ErrKind
	}{
		{"429 优先于 quota 文案", 429, `{"error":{"message":"quota exceeded"}}`, provider.ErrSoftRate},
		{"401 会话失效", 401, `{"error":{"message":"unauthenticated"}}`, provider.ErrSessionDead},
		{"403 归 SERVER", 403, `{"error":{"status":"PERMISSION_DENIED","details":[{"reason":"SUBSCRIPTION_REQUIRED"}]}}`, provider.ErrServer},
		{"404 归 SERVER", 404, `{"error":{"code":404,"status":"NOT_FOUND"}}`, provider.ErrServer},
		{"上下文超限", 400, `{"error":{"message":"The input token count exceeds the maximum number of tokens allowed 1048576"}}`, provider.ErrPromptTooLong},
		{"配额耗尽", 400, `{"error":{"message":"resource_exhausted: quota exceeded"}}`, provider.ErrHardCredit},
		{"5xx", 503, `{"error":{"message":"backend down"}}`, provider.ErrServer},
		{"本地 bad_params", 400, `{"error":{"code":"bad_params"}}`, provider.ErrBadParams},
		{"本地 prompt_too_long", 400, `{"error":{"code":"prompt_too_long"}}`, provider.ErrPromptTooLong},
		{"其它 4xx", 400, `{"error":{"message":"bad field"}}`, provider.ErrClient},
		{"成功", 200, `{}`, provider.ErrNone},
	}
	for _, tc := range cases {
		if got := Classify(tc.status, tc.body); got != tc.want {
			t.Errorf("%s：Classify(%d)=%s，期望 %s", tc.name, tc.status, got, tc.want)
		}
	}
}

// TestRefreshTokenSendsClientSecretAndRotates 续期表单**必须**带 client_secret，
// 且轮换后的 refresh_token 必须回写（不变量 20 的前置条件）。
func TestRefreshTokenSendsClientSecretAndRotates(t *testing.T) {
	fixedHooks(t)
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(raw))
		_, _ = io.WriteString(w, `{"access_token":"ya29.NEW","refresh_token":"1//ROTATED",`+
			`"token_type":"Bearer","expires_in":3599,"scope":"openid"}`)
	}))
	defer srv.Close()

	c := New()
	c.TokenURL = srv.URL
	a := testAuth()
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("RefreshToken 失败：%v", err)
	}
	if form.Get("client_id") != ClientID() {
		t.Errorf("client_id=%q", form.Get("client_id"))
	}
	if form.Get("client_secret") != ClientSecret() || form.Get("client_secret") == "" {
		t.Errorf("token 端点**强制**客户端身份：表单缺 client_secret（form=%v）", form)
	}
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "1//FAKE_refresh_for_test" {
		t.Errorf("续期表单错误：%v", form)
	}
	if a.AccessToken != "ya29.NEW" || a.RefreshToken != "1//ROTATED" {
		t.Errorf("令牌未回写：access=%q refresh=%q", a.AccessToken, a.RefreshToken)
	}
	if delta := a.ExpiresAt - time.Now().Unix(); delta < 3500 || delta > 3700 {
		t.Errorf("ExpiresAt 应按 expires_in（**秒**）写入，delta=%d", delta)
	}
}

// TestRefreshTokenTerminalIsSessionDead 终态错误必须是 *provider.Error{ErrSessionDead}
// （上层 errors.As 依赖，不变量 19/23）。
func TestRefreshTokenTerminalIsSessionDead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`)
	}))
	defer srv.Close()
	c := New()
	c.TokenURL = srv.URL
	err := c.RefreshToken(testAuth())
	var ue *provider.Error
	if !errors.As(err, &ue) || ue.Kind != provider.ErrSessionDead {
		t.Fatalf("期望 *provider.Error{ErrSessionDead}，得 %#v", err)
	}
}

// TestRefreshTokenKeepsOldRefreshTokenWhenAbsent 响应不带 refresh_token ⇒ 回退旧值
// （Google 只在轮换时才给新的，不给就必须沿用，否则下次续期必失败）。
func TestRefreshTokenKeepsOldRefreshTokenWhenAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"ya29.NEW","expires_in":3600}`)
	}))
	defer srv.Close()
	c := New()
	c.TokenURL = srv.URL
	a := testAuth()
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("RefreshToken 失败：%v", err)
	}
	if a.RefreshToken != "1//FAKE_refresh_for_test" {
		t.Errorf("缺 refresh_token 时须沿用旧值，得 %q", a.RefreshToken)
	}
}

// ---------------------------------------------------------------------------
// 静态表与其它 provider.Upstream 空能力
// ---------------------------------------------------------------------------

// TestStaticModelsAndNoCheckin 静态表只 1 条 + 四档；签到必须返回明确错误
// （返回 nil 会把缺陷伪装成「今日已签到」）。
func TestStaticModelsAndNoCheckin(t *testing.T) {
	ms := StaticModels()
	if len(ms) != 1 {
		t.Fatalf("静态表应只有 1 条（不暴露带后缀的名字与 lite），得 %d 条", len(ms))
	}
	m := ms[0]
	if m.ID != ModelFlash || m.ContextWindow != ContextWindow || m.MaxTokens != MaxOutputTokens {
		t.Errorf("静态表条目错误：%+v", m)
	}
	if !m.SupportsImages || !m.SupportsReasoning || !m.SupportsTools {
		t.Errorf("能力声明错误：%+v", m)
	}
	if len(m.ReasoningEfforts) != 4 || m.DefaultReasoningEffort != TierMedium {
		t.Errorf("档位表错误：%v / %q", m.ReasoningEfforts, m.DefaultReasoningEffort)
	}
	if m.CanDisableThinking == nil || *m.CanDisableThinking {
		t.Errorf("「关闭思考」是假关 ⇒ 必须显式声明不可关闭：%v", m.CanDisableThinking)
	}
	if m.ContextFromAPI {
		t.Error("静态表不是上游目录：ContextFromAPI 必须为 false")
	}
	c := &Client{}
	if err := c.DailyCheckin(nil); err == nil || !strings.Contains(err.Error(), "没有签到机制") {
		t.Errorf("本渠道无签到，必须返回明确错误：%v", err)
	}
	if models, err := c.FetchModels(nil); err != nil || len(models) != 1 {
		t.Errorf("FetchModels 应返回静态表且不报错：%v %v", models, err)
	}
	if pricing, err := c.FetchModelPricing(nil); err != nil || len(pricing) != 0 {
		t.Errorf("无费率端点时应返回空切片（面板显示未知，而非免费）：%v %v", pricing, err)
	}
	// 接口实现自检：Client 必须满足 provider.Upstream（10 个方法）。
	var _ provider.Upstream = (*Client)(nil)
}

// TestSessionIDDerivation sessionId 依赖维度与"同输入同输出"
// （取值与原版**不逐字相同**，见 envelope.go 的存疑项标注）。
func TestSessionIDDerivation(t *testing.T) {
	base := deriveSessionID("p1", "hello", SessionLaneInfer, 0)
	if base == "" {
		t.Fatal("sessionId 不得为空")
	}
	if base != deriveSessionID("p1", "hello", SessionLaneInfer, 0) {
		t.Error("同输入必须同输出")
	}
	if base == deriveSessionID("p2", "hello", SessionLaneInfer, 0) {
		t.Error("换 project 必须变")
	}
	if base == deriveSessionID("p1", "hello2", SessionLaneInfer, 0) {
		t.Error("换首条文本必须变")
	}
	if base == deriveSessionID("p1", "hello", "smoke", 0) {
		t.Error("换 lane 必须变")
	}
	if base == deriveSessionID("p1", "hello", SessionLaneInfer, 1) {
		t.Error("升代必须变")
	}
	// 输出形态：有符号 int64 的十进制串（原版取值正负都有）。
	if _, err := strconv.ParseInt(base, 10, 64); err != nil {
		t.Errorf("sessionId 必须是十进制整数串，得 %q", base)
	}
	// 依赖维度只认 project / 首条 user 文本 / lane：换 model 与后续文本都不该变。
	contentsA := []content{{Role: "user", Parts: []part{{Text: "hi"}, {Text: "more"}}}}
	contentsB := []content{{Role: "user", Parts: []part{{Text: "hi"}}}}
	if firstUserText(contentsA) != firstUserText(contentsB) {
		t.Errorf("只有首个文本 part 参与派生：%q vs %q", firstUserText(contentsA), firstUserText(contentsB))
	}
}

// ---------------------------------------------------------------------------
// 图片与工具配对
// ---------------------------------------------------------------------------

// TestImageDataURLAndErrors 图片：data URL → 裸 base64（去换行/空白）；
// 非 data URL 或非法 base64 → **报错**（不静默丢图，否则用户以为模型看到了）。
func TestImageDataURLAndErrors(t *testing.T) {
	fixedHooks(t)
	body := `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"看图"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,AAECAw==\n"}}]}]}`
	out, code, err := buildInternalBody([]byte(body), DefaultProject)
	if err != nil {
		t.Fatalf("buildInternalBody 失败：%v（%s）", err, code)
	}
	if !strings.Contains(string(out), `"inlineData":{"data":"AAECAw==","mimeType":"image/png"}`) {
		t.Errorf("图片应转成裸 base64 的 inlineData：%s", out)
	}
	for _, bad := range []string{
		`{"model":"gemini-3.8-flash","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`,
		`{"model":"gemini-3.8-flash","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,!!!!"}}]}]}`,
	} {
		_, c, err := buildInternalBody([]byte(bad), DefaultProject)
		if err == nil || c != errCodeBadParams {
			t.Errorf("图片不可用必须报 bad_params：err=%v code=%s", err, c)
		}
	}
}

// TestToolPairingAndFunctionResponseName 工具结果按 **name** 配对（上游不认 id）；
// 孤儿两侧都剔除。
func TestToolPairingAndFunctionResponseName(t *testing.T) {
	fixedHooks(t)
	body := `{"model":"gemini-3.8-flash","messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function",` +
		`"function":{"name":"read_file","arguments":"{\"path\":\"/x\"}"}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"{\"ok\":true}"},` +
		`{"role":"assistant","content":"bye","tool_calls":[{"id":"call_orphan","type":"function",` +
		`"function":{"name":"ghost","arguments":"{}"}}]},` +
		`{"role":"tool","tool_call_id":"call_nope","content":"orphan"}]}`
	out, code, err := buildInternalBody([]byte(body), DefaultProject)
	if err != nil {
		t.Fatalf("buildInternalBody 失败：%v（%s）", err, code)
	}
	text := string(out)
	if !strings.Contains(text, `"functionCall":{"args":{"path":"/x"},"name":"read_file"}`) {
		t.Errorf("functionCall 形状/键序错误：%s", text)
	}
	if !strings.Contains(text, `"functionResponse":{"name":"read_file","response":{"content":"{\"ok\":true}"}}`) {
		t.Errorf("functionResponse 必须按 name 配对：%s", text)
	}
	if strings.Contains(text, "ghost") || strings.Contains(text, "orphan") {
		t.Errorf("孤儿 tool_call / tool_result 两侧都必须剔除：%s", text)
	}
	// 角色只有 user / model。
	if strings.Contains(text, `"role":"assistant"`) || strings.Contains(text, `"role":"tool"`) {
		t.Errorf("role 只能是 user / model：%s", text)
	}
	if !strings.Contains(text, `"role":"model"`) {
		t.Errorf("assistant 必须映射成 model：%s", text)
	}
}

// TestEnvelopeRejectsEmptyMessages 空 messages 必须本地拒绝（上游 400 的报错文本
// 与真实原因完全无关，排查成本极高）。
func TestEnvelopeRejectsEmptyMessages(t *testing.T) {
	if _, code, err := buildInternalBody([]byte(`{"model":"gemini-3.8-flash","messages":[]}`), DefaultProject); err == nil {
		t.Fatal("空 messages 必须拒绝")
	} else if code != errCodeBadParams {
		t.Errorf("code=%s，期望 %s", code, errCodeBadParams)
	}
}
