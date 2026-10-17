// zcode_test.go ZCode 渠道包的回归测试。
//
// 纪律：**全部走 httptest 假上游，零真实网络**（AGENTS.md §0.1：严禁穷举探测上游）。
// 覆盖点（对应任务要求）：
//   - 头族齐全（含 X-Device-Mid）；
//   - 身份块缺失 → 405/3012 的**本地前置校验**（且一个包都没发出去）；
//   - Anthropic SSE → OpenAI SSE（signature_delta 忽略、message_stop 收尾、error 帧抛错）；
//   - output_config.effort 下发；
//   - 额度解析（balance 的 token 桶 + preview 的可领计划 + 企业版）；
//   - DailyCheckin 返回「不支持」而非成功。
package zcode

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// ---------------------------------------------------------------------------
// 测试工具
// ---------------------------------------------------------------------------

func testAuth(extra map[string]string) *auth.Auth {
	if extra == nil {
		extra = map[string]string{"device_mid": "11111111-2222-4333-8444-555555555555"}
	}
	return &auth.Auth{
		Kind:        "zcode",
		AccessToken: "test.jwt.payload",
		UID:         "12345678901234567",
		Extra:       extra,
	}
}

// chatBody 构造一个最小的 OpenAI Chat 请求体（含 system / user / tools）。
func chatBody(t *testing.T, effort string) []byte {
	t.Helper()
	req := map[string]any{
		"model": "zcode/GLM-5.3-Flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "CALLER-SYSTEM-TEXT"},
			map[string]any{"role": "user", "content": "你好"},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{
				"name": "read_file", "description": "read",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
			}},
		},
	}
	if effort != "" {
		req["reasoning_effort"] = effort
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return raw
}

// ---------------------------------------------------------------------------
// 身份块
// ---------------------------------------------------------------------------

// TestIdentityConstantsMatchProtocol 锁定身份块的字符数指纹。
//
// 这是准入判据的指纹（协议 §1.2.4 的实测矩阵：cliPrefix 42 + stable 2856 = 2898 才 ✓）。
// 任何字符改动（格式化、复制粘贴丢空格）都必须在这里被抓住，而不是等上游回 405 + 3012
// 并给账号记一次风控冷却。
func TestIdentityConstantsMatchProtocol(t *testing.T) {
	if got := utf8.RuneCountInString(cliPrefix); got != identityCliRunes {
		t.Errorf("cliPrefix 字符数 = %d，期望 %d", got, identityCliRunes)
	}
	stable := officialStable()
	if got := utf8.RuneCountInString(stable); got != identityStableRunes {
		t.Errorf("stable 字符数 = %d，期望 %d", got, identityStableRunes)
	}
	if got := utf8.RuneCountInString(cliPrefix + stable); got != identityTotalRunes {
		t.Errorf("身份块合计字符数 = %d，期望 %d", got, identityTotalRunes)
	}
	if !strings.HasPrefix(cliPrefix, "You are ZCode, an interactive coding agent") {
		t.Errorf("cliPrefix 文本不符：%q", cliPrefix)
	}
	if len(stableSections) != 3 {
		t.Fatalf("stable 应为 3 段，实际 %d 段", len(stableSections))
	}
	if !strings.HasPrefix(stable, "\nYou are an interactive ZCode agent") {
		t.Errorf("stable 首段不符：%.40q", stable)
	}
}

// TestIdentityTextByteExact 逐字节锁定身份块文本（比字符数指纹更强的一道锁）。
//
// 期望值是从参考实现（`dsh-our-free-model/vendor/channel-pack/src/zcode-identity.ts`，
// 它本身由官方 3.11.2 bundle 反解）用一次性脚本提取后算出的 sha256。
//
// 为什么值得单锁一道：身份块是**准入判据**，而 3012 会给账号记风控冷却
// （30 分钟起步 → 24h 内第 3 次起 24h → **5 次停用**）。字符数相同但字符被换位/替换
// （例如一个空格变成全角空格、em dash 变成普通连字符）长度不会变，只有哈希能抓住。
func TestIdentityTextByteExact(t *testing.T) {
	const (
		wantCLI    = "46dd360a22c87a92dfdf29ae2c3011b0f9a014209a7b607f8fcd17a920e5eafa"
		wantStable = "3fdb43778fddc6a482c91725268a712774d5a090366e2a0df200369fed9f60ef"
	)
	if got := sha256Sum(cliPrefix); got != wantCLI {
		t.Errorf("cliPrefix sha256 = %s，期望 %s（身份块文本必须逐字节一致）", got, wantCLI)
	}
	if got := sha256Sum(officialStable()); got != wantStable {
		t.Errorf("stable sha256 = %s，期望 %s（身份块文本必须逐字节一致）", got, wantStable)
	}
}

func TestVerifyIdentity(t *testing.T) {
	good := buildIdentityBlocks("caller", environment("GLM-5.3-Flash"))
	if err := verifyIdentity(good); err != nil {
		t.Fatalf("完整身份块应通过校验：%v", err)
	}
	if err := verifyIdentity(good[:1]); err == nil {
		t.Error("只有 cliPrefix 时应被拒")
	}
	if err := verifyIdentity(nil); err == nil {
		t.Error("空 system 时应被拒")
	}
	// 篡改 stable 一个字符 → 必须被拒（长度指纹也会变）。
	bad := buildIdentityBlocks("caller", environment("x"))
	bad[1].(map[string]any)["text"] = officialStable() + "!"
	if err := verifyIdentity(bad); err == nil {
		t.Error("stable 被篡改时应被拒")
	}
}

// ---------------------------------------------------------------------------
// 头族
// ---------------------------------------------------------------------------

func TestRequiredHeadersComplete(t *testing.T) {
	a := testAuth(nil)
	h := zcodeHeaders(a, "Bearer tok", true)
	for _, name := range requiredHeaderNames() {
		if strings.TrimSpace(h.Get(name)) == "" {
			t.Errorf("缺少必需头 %s", name)
		}
	}
	if got := h.Get(HdrDeviceMid); got != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("X-Device-Mid = %q", got)
	}
	if got := h.Get(HdrUserAgent); got != "ZCode/3.14.3" {
		t.Errorf("User-Agent = %q", got)
	}
	if got := h.Get(HdrPlatform); got != "win32" {
		t.Errorf("X-Platform = %q", got)
	}
	if got := h.Get(HdrOSCategory); got != "windows" {
		t.Errorf("X-Os-Category = %q", got)
	}
	if got := h.Get(HdrAnthropicVersion); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q", got)
	}
	if got := h.Get(HdrAuthorization); got != "Bearer tok" {
		t.Errorf("Authorization = %q", got)
	}
	// 不带 Authorization / Content-Type 时那两个键不出现（preview 端点形态）。
	plain := zcodeHeaders(a, "", false)
	if plain.Get(HdrAuthorization) != "" || plain.Get(HdrContentType) != "" {
		t.Error("preview 形态不应带 Authorization / Content-Type")
	}
	for _, name := range requiredHeaderNames() {
		if plain.Get(name) == "" {
			t.Errorf("preview 形态缺必需头 %s", name)
		}
	}
}

func TestDeviceMidFallbackIsStable(t *testing.T) {
	a := &auth.Auth{Kind: "zcode", AccessToken: "jwt", UID: "u1"}
	first := deviceMidOf(a)
	second := deviceMidOf(a)
	if first != second {
		t.Errorf("缺 device_mid 时的兜底值必须确定性稳定：%q vs %q", first, second)
	}
	if len(strings.Split(first, "-")) != 5 {
		t.Errorf("兜底值应是 UUID 形态：%q", first)
	}
	b := &auth.Auth{Kind: "zcode", AccessToken: "jwt", UID: "u2"}
	if deviceMidOf(b) == first {
		t.Error("不同账号的兜底 mid 不应相同")
	}
}

// ---------------------------------------------------------------------------
// 出站形态（Anthropic Messages + 身份块 + output_config.effort）
// ---------------------------------------------------------------------------

func TestChatStreamOutboundShape(t *testing.T) {
	var (
		gotPath string
		gotHdr  http.Header
		gotBody map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHdr = r.Header.Clone()
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("出站体不是 JSON：%v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sseStream("hello"))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	rc, status, _, err := c.ChatStream(testAuth(nil), chatBody(t, "max"))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	defer rc.Close()

	if gotPath != EpZCodePlanMessages {
		t.Errorf("path = %q，期望 %q", gotPath, EpZCodePlanMessages)
	}
	// 头族在真实请求上也必须齐全。
	for _, name := range requiredHeaderNames() {
		if gotHdr.Get(name) == "" {
			t.Errorf("请求缺必需头 %s", name)
		}
	}
	if gotHdr.Get(HdrReferer) != Origin {
		t.Errorf("HTTP-Referer = %q，期望 %q", gotHdr.Get(HdrReferer), Origin)
	}
	if gotHdr.Get(HdrAuthorization) != "Bearer test.jwt.payload" {
		t.Errorf("Authorization = %q", gotHdr.Get(HdrAuthorization))
	}

	// stream 恒 true；max_tokens 缺省 8192（官方 `options.maxTokens ?? 8192`）。
	if got, _ := gotBody["stream"].(bool); !got {
		t.Error("stream 必须恒为 true")
	}
	if got, _ := gotBody["max_tokens"].(float64); got != defaultMaxTokens {
		t.Errorf("max_tokens = %v，期望 %d", gotBody["max_tokens"], defaultMaxTokens)
	}

	system, _ := gotBody["system"].([]any)
	if len(system) < 4 {
		t.Fatalf("system 块数 = %d，期望 >= 4（身份块 + 调用方）", len(system))
	}
	if blockText(system[0]) != cliPrefix {
		t.Error("system[0] 必须是官方 cliPrefix")
	}
	if blockText(system[1]) != officialStable() {
		t.Error("system[1] 必须是官方 stable 段")
	}
	if !strings.Contains(blockText(system[2]), "# Environment") {
		t.Error("system[2] 应是 # Environment 段")
	}
	if !strings.Contains(blockText(system[3]), "CALLER-SYSTEM-TEXT") {
		t.Error("调用方 system 必须追加在最后")
	}
	last := system[len(system)-1].(map[string]any)
	cc, _ := last["cache_control"].(map[string]any)
	if strOf(cc["type"]) != "ephemeral" {
		t.Error("只有最后一块应带 cache_control:ephemeral")
	}
	if _, has := system[0].(map[string]any)["cache_control"]; has {
		t.Error("身份块不应带 cache_control（只给最后一块）")
	}

	// tools 扁平形态 + 最后一个 tool 打缓存断点。
	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 数 = %d", len(tools))
	}
	tool := tools[0].(map[string]any)
	if _, ok := tool["input_schema"]; !ok {
		t.Error("tools 必须是扁平 input_schema 形态")
	}
	if _, ok := tool["function"]; ok {
		t.Error("tools 不得带 OpenAI 的嵌套 function 字段")
	}
	if tcc, ok := tool["cache_control"].(map[string]any); !ok || strOf(tcc["type"]) != "ephemeral" {
		t.Error("最后一个 tool 应带 cache_control 断点")
	}

	// 思考档位：output_config.effort（不是 reasoning_effort）。
	oc, _ := gotBody["output_config"].(map[string]any)
	if strOf(oc["effort"]) != "max" {
		t.Errorf("output_config.effort = %v，期望 max", gotBody["output_config"])
	}
	if _, has := gotBody["reasoning_effort"]; has {
		t.Error("不得下发 reasoning_effort（本渠道的字段是 output_config.effort）")
	}

	// 首轮 user 消息带 <system-reminder> 日期块，且 messages 里没有 system 角色消息。
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("messages 为空")
	}
	firstMsg := msgs[0].(map[string]any)
	if strOf(firstMsg["role"]) != "user" {
		t.Fatalf("messages[0].role = %v", firstMsg["role"])
	}
	blocks, _ := firstMsg["content"].([]any)
	if len(blocks) == 0 || !strings.HasPrefix(blockText(blocks[0]), "<system-reminder>") {
		t.Error("首轮 user 消息应以 <system-reminder> 日期块开头")
	}
	for _, m := range msgs {
		if strOf(m.(map[string]any)["role"]) == "system" {
			t.Error("messages 里不得出现 system 角色消息（已提升到顶层 system）")
		}
	}
}

// TestEffortOnlyWhenDeclared 档位只在该模型声明的表里才下发（R49：绝不编造）。
func TestEffortOnlyWhenDeclared(t *testing.T) {
	cases := []struct {
		name   string
		effort string
		want   string
	}{
		{"声明的档位", "high", "high"},
		{"未声明的档位", "xhigh", ""},
		{"关闭思考（本渠道无此写法）", "none", ""},
		{"开启思考（不是档位）", "on", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry, known := catalogEntryForTest("GLM-5.3-Flash")
			out, err := buildMessagesBody(chatBody(t, tc.effort), entry, known)
			if err != nil {
				t.Fatalf("buildMessagesBody: %v", err)
			}
			var body map[string]any
			if err := json.Unmarshal(out, &body); err != nil {
				t.Fatal(err)
			}
			oc, _ := body["output_config"].(map[string]any)
			if got := strOf(oc["effort"]); got != tc.want {
				t.Errorf("effort = %q，期望 %q", got, tc.want)
			}
		})
	}
}

func catalogEntryForTest(id string) (catalogEntry, bool) {
	e, ok := staticIndex[id]
	return e, ok
}

// TestIdentityGateBlocksLocally 身份块缺失 → **本地** 405/3012 前置校验，零上游请求。
func TestIdentityGateBlocksLocally(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// 注入「缺身份块」的构造器：模拟常量被破坏 / 上层改写了 system 结构的场景。
	orig := identitySystemFn
	identitySystemFn = func(caller string, env envInfo) []any {
		if strings.TrimSpace(caller) == "" {
			return nil
		}
		return []any{textBlock(caller)}
	}
	defer func() { identitySystemFn = orig }()

	c := NewWithBase(srv.URL)
	rc, status, body, err := c.ChatStream(testAuth(nil), chatBody(t, ""))
	if err != nil {
		t.Fatalf("本地拦截不应是传输错误：%v", err)
	}
	if rc != nil {
		t.Error("本地拦截不应返回 body")
	}
	if status != http.StatusMethodNotAllowed {
		t.Errorf("status = %d，期望 405（与上游同条件的形态对齐）", status)
	}
	if !strings.Contains(string(body), "3012") {
		t.Errorf("错误体应说明上游同条件回 code 3012：%s", body)
	}
	// ⚠ 归类必须是请求级（本地缺陷不得惩罚健康账号）。
	if k := Classify(status, string(body)); k != provider.ErrBadParams {
		t.Errorf("本地身份块拦截的归类 = %v，期望 bad_params", k)
	}
	if hits != 0 {
		t.Errorf("本地前置校验必须零上游请求，实际发了 %d 个", hits)
	}
}

// TestCodingPlanLegSelection 按凭据形态选腿：无 JWT + 有 coding_plan_key → 订阅腿，
// 端点 api.z.ai 形态且**不带 HTTP-Referer**。
func TestCodingPlanLegSelection(t *testing.T) {
	var (
		gotPath string
		gotHdr  http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHdr = r.Header.Clone()
		_, _ = io.WriteString(w, sseStream("ok"))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	a := &auth.Auth{Kind: "zcode", UID: "u", Extra: map[string]string{
		"device_mid":          "mid-1",
		"coding_plan_key_zai": "plan-key",
	}}
	rc, status, _, err := c.ChatStream(a, chatBody(t, ""))
	if err != nil || status != 200 {
		t.Fatalf("ChatStream: status=%d err=%v", status, err)
	}
	defer rc.Close()
	if gotPath != "/api/anthropic/v1/messages" {
		t.Errorf("订阅腿 path = %q", gotPath)
	}
	if gotHdr.Get(HdrReferer) != "" {
		t.Error("订阅腿不得带 HTTP-Referer（协议 §1.2.3）")
	}
	if gotHdr.Get(HdrAuthorization) != "Bearer plan-key" {
		t.Errorf("订阅腿 Authorization = %q", gotHdr.Get(HdrAuthorization))
	}
	if gotHdr.Get(HdrDeviceMid) != "mid-1" {
		t.Error("订阅腿仍需 X-Device-Mid")
	}

	// 两条腿都承载的模型（glm-5.3-flash）→ start-plan 优先（积分不该被订阅额度先消耗）。
	both := &auth.Auth{Kind: "zcode", AccessToken: "jwt", UID: "u", Extra: map[string]string{
		"device_mid": "mid-1", "coding_plan_key_zai": "plan-key",
	}}
	if ch := resolveChannel(both, "GLM-5.3-Flash"); ch != channelStartPlan {
		t.Errorf("两条腿都可用且模型共有时应优先 start-plan，实际 %v", ch)
	}
	// 只属于订阅腿白名单的模型（glm-5.3）→ 走订阅腿（协议 §1.6 第 3 层）。
	if ch := resolveChannel(both, "GLM-5.3"); ch != channelCodingPlan {
		t.Errorf("GLM-5.3 只属订阅腿白名单，应选 coding-plan，实际 %v", ch)
	}
	// 只有 JWT（无订阅 key）时，即便模型只属订阅白名单也保守回退 start-plan。
	jwtOnly := &auth.Auth{Kind: "zcode", AccessToken: "jwt", UID: "u"}
	if ch := resolveChannel(jwtOnly, "GLM-5.3"); ch != channelStartPlan {
		t.Errorf("订阅凭据缺失时应保守回退 start-plan，实际 %v", ch)
	}
}

// ---------------------------------------------------------------------------
// 入站 SSE
// ---------------------------------------------------------------------------

// sseStream 构造一段完整的 Anthropic SSE（thinking → text → 收尾）。
func sseStream(text string) string {
	return strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":1000,"cache_read_input_tokens":400,"cache_creation_input_tokens":100}}}`,
		"",
		"event: ping",
		`data: {"type":"ping"}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"想一下"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"DEADBEEFSIGNATURE"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"` + text + `"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":1}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20,"output_tokens_details":{"thinking_tokens":7}}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
}

func TestSSEConversion(t *testing.T) {
	c := NewWithBase("http://127.0.0.1:1") // 本用例不打上游
	rec := httptest.NewRecorder()
	usage, err := c.Stream(rec, strings.NewReader(sseStream("hello")), "zcode/GLM-5.3-Flash")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"reasoning_content":"想一下"`) {
		t.Errorf("thinking_delta 应映射为 reasoning_content：%s", out)
	}
	if !strings.Contains(out, `"content":"hello"`) {
		t.Errorf("text_delta 应映射为 content：%s", out)
	}
	// signature_delta 必须被忽略（否则回答里会多出一长串签名）。
	if strings.Contains(out, "DEADBEEFSIGNATURE") {
		t.Errorf("signature_delta 必须忽略，实际出现在输出里：%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("end_turn 应映射为 stop：%s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("转换器输出应以 [DONE] 收尾（这是 OpenAI 侧协议要求）：%q", out[len(out)-40:])
	}
	if usage == nil {
		t.Fatal("usage 为空")
	}
	// prompt_tokens = input + cache_read + cache_creation（总输入口径）。
	if got := intOf(usage, "prompt_tokens"); got != 1500 {
		t.Errorf("prompt_tokens = %d，期望 1500", got)
	}
	if got := intOf(usage, "completion_tokens"); got != 20 {
		t.Errorf("completion_tokens = %d", got)
	}
	details, _ := usage["prompt_tokens_details"].(map[string]any)
	if got := intOf(details, "cached_tokens"); got != 400 {
		t.Errorf("cached_tokens = %d", got)
	}
	if got := intOf(details, "cache_write_tokens"); got != 100 {
		t.Errorf("cache_write_tokens = %d", got)
	}
	ctd, _ := usage["completion_tokens_details"].(map[string]any)
	if got := intOf(ctd, "reasoning_tokens"); got != 7 {
		t.Errorf("reasoning_tokens = %d", got)
	}
}

func TestSSEErrorFrameFails(t *testing.T) {
	c := NewWithBase("http://127.0.0.1:1")
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m","usage":{"input_tokens":1}}}`,
		"",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		"",
		`data: {"type":"error","error":{"type":"overloaded_error","message":"服务繁忙"}}`,
		"",
	}, "\n")
	rec := httptest.NewRecorder()
	_, err := c.Stream(rec, strings.NewReader(stream), "zcode/GLM-5.3-Flash")
	if err == nil {
		t.Fatal("error 帧必须抛错（R40：不得伪装正常收尾）")
	}
	if !strings.Contains(err.Error(), "上游错误帧") {
		t.Errorf("错误文案应指明错误帧：%v", err)
	}
	if strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Error("错误路径不得补 [DONE] 伪装正常收尾（R36）")
	}
}

func TestSSETruncatedBlockFails(t *testing.T) {
	c := NewWithBase("http://127.0.0.1:1")
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m","usage":{"input_tokens":1}}}`,
		"",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text"}}`,
		"",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"半个"}}`,
		"",
	}, "\n")
	rec := httptest.NewRecorder()
	_, err := c.Stream(rec, strings.NewReader(stream), "zcode/GLM-5.3-Flash")
	if err == nil || !strings.Contains(err.Error(), "内容块中途结束") {
		t.Fatalf("内容块中途结束应报错，实际 err=%v", err)
	}
}

func TestSSEEmptyResponseFails(t *testing.T) {
	c := NewWithBase("http://127.0.0.1:1")
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m","usage":{"input_tokens":1}}}`,
		"",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
	rec := httptest.NewRecorder()
	_, err := c.Stream(rec, strings.NewReader(stream), "zcode/GLM-5.3-Flash")
	if err == nil || !strings.Contains(err.Error(), "空响应") {
		t.Fatalf("空响应应报错，实际 err=%v", err)
	}
}

func TestSSEToolUse(t *testing.T) {
	c := NewWithBase("http://127.0.0.1:1")
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"m","usage":{"input_tokens":3}}}`,
		"",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file"}}`,
		"",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`,
		"",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"a.txt\"}"}}`,
		"",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
		"",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
	rec := httptest.NewRecorder()
	if _, err := c.Stream(rec, strings.NewReader(stream), "zcode/GLM-5.3-Flash"); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"name":"read_file"`) {
		t.Errorf("tool_use 起始帧应带 name：%s", out)
	}
	if !strings.Contains(out, `"arguments":"{\"path\":\"a.txt\"}"`) {
		t.Errorf("arguments 应在 block stop 时一次性给出：%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Errorf("stop_reason=tool_use 应映射为 tool_calls：%s", out)
	}
}

// ---------------------------------------------------------------------------
// 额度
// ---------------------------------------------------------------------------

func TestBillingParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpBalance:
			if r.Header.Get(HdrAuthorization) == "" {
				t.Error("balance 必须带 Authorization（缺则上游 401）")
			}
			if r.Header.Get(HdrDeviceMid) == "" {
				t.Error("balance 必须带 X-Device-Mid（缺则上游 400 code 3001）")
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"displayMode":"personal","balances":[
				{"plan_id":"plan-a","show_name":"GLM-5.3-Flash","unit_type":"token","meter":"model_usage",
				 "total_units":100000000,"used_units":5460725,"remaining_units":94539275,"available_units":94539275,
				 "expires_at":1893456000}]}}`)
		case EpPreview:
			if !strings.Contains(r.URL.RawQuery, "platform=win32") {
				t.Errorf("preview 必须带 platform=win32：%q", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[
				{"plan_id":"trust-1003","priority":1,"name":"每日赠送",
				 "entitlements":[{"grant_units":100000000,"unit_type":"token"}]}]}}`)
		default:
			t.Errorf("意外请求 %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	a := testAuth(nil)
	// 2026-10-17：UserResource 返回 Σ token —— **这是 token 口径的额度余额，不是积分**
	// （由渠道级 credits_unit="token" 标注：前端用「Token 计数」面板展示，
	// 后端不进积分合计、不进积分流水；条目仍是 InfoOnly，见下）。
	remain, err := c.UserResource(a)
	if err != nil {
		t.Fatalf("UserResource: %v", err)
	}
	if remain != 94539275 {
		t.Errorf("remain = %d，期望 94539275（token 桶剩余之和）", remain)
	}
	_, items, err := c.UserResourceDetail(a)
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("明细条目数 = %d，期望 2（1 个余额桶 + 1 个可领计划）", len(items))
	}
	bal := items[0]
	if !strings.Contains(bal.Name, "tokens") {
		t.Errorf("token 桶的名称应标明单位：%q", bal.Name)
	}
	if bal.Total != 100000000 || bal.Used != 5460725 || bal.Remain != 94539275 {
		t.Errorf("桶数值不符：%+v", bal)
	}
	if bal.Key != "plan-a" {
		t.Errorf("桶 Key 应为 plan_id：%q", bal.Key)
	}
	// ⚠️ 2026-10-17 修正：token 桶**必须 InfoOnly**（它不是积分）。
	// 旧断言要求「非 InfoOnly」，正是「上亿 token 被当成积分污染统计」的根源。
	if !bal.Usable || !bal.InfoOnly {
		t.Errorf("token 桶应 可展示(Usable) 且 InfoOnly（token 不是积分）：%+v", bal)
	}
	if bal.ExpireAt != "2030-01-01" {
		t.Errorf("ExpireAt = %q，期望 UTC+8 墙钟日期 2030-01-01", bal.ExpireAt)
	}
	plan := items[1]
	if !plan.InfoOnly || !plan.Usable {
		t.Errorf("可领计划必须是 InfoOnly + Usable：%+v", plan)
	}
	if plan.Total != 100000000 {
		t.Errorf("可领额度应取 entitlements[0].grant_units：%+v", plan)
	}
	// InfoOnly 条目不进任何小计：token 桶与可领计划都不得算进「可消耗积分」，
	// 否则上亿 token 会污染积分口径（2026-10-17 修正）。
	usable, unusable := provider.Summarize(items)
	if usable != 0 || unusable != 0 {
		t.Errorf("Summarize = (%d,%d)，期望 (0,0)（token 不是积分）", usable, unusable)
	}
}

func TestBillingEnterpriseDoesNotShowZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpBalance {
			_, _ = io.WriteString(w, `{"code":0,"data":{"displayMode":"enterprise","balances":[]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[]}}`)
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	_, items, err := c.UserResourceDetail(testAuth(nil))
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if len(items) != 1 || !items[0].InfoOnly {
		t.Fatalf("企业版应给一条只作展示的说明条目，实际 %+v", items)
	}
	if !strings.Contains(items[0].Name, "企业版") {
		t.Errorf("说明条目文案不符：%q", items[0].Name)
	}
}

func TestBillingErrorIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":1002,"msg":"unauthorized"}`)
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL)
	// 用 UserResourceDetail 验证 401 分类：UserResource 自 2026-10-17 起恒 0 且**不发请求**
	// （token 不是积分），401 的自愈判据由仍在请求上游的明细路径承担。
	_, _, err := c.UserResourceDetail(testAuth(nil))
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrSessionDead {
		t.Fatalf("401 必须是 *provider.Error{ErrSessionDead}（不变式 19 的自愈判据），实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// 签到：本渠道不支持
// ---------------------------------------------------------------------------

func TestDailyCheckinUnsupported(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	err := c.DailyCheckin(testAuth(nil))
	if !errors.Is(err, ErrClaimUnsupported) {
		t.Fatalf("DailyCheckin 必须返回 ErrClaimUnsupported，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "不支持") || !strings.Contains(err.Error(), "验证码") {
		t.Errorf("错误文案应说明「不支持 + 原因」：%v", err)
	}
	if hits != 0 {
		t.Errorf("不支持领取时不得请求上游，实际 %d 个请求", hits)
	}
	// 不得实现 CheckinReporter：实现了会让调度器在当日窗口内反复重试一条永不可能成功的路径。
	if _, ok := any(c).(provider.CheckinReporter); ok {
		t.Error("本渠道不得实现 provider.CheckinReporter")
	}
}

// ---------------------------------------------------------------------------
// 续期：空实现
// ---------------------------------------------------------------------------

func TestRefreshTokenIsNoop(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()
	c := NewWithBase(srv.URL)
	a := testAuth(nil)
	if err := c.RefreshToken(a); err != nil {
		t.Errorf("RefreshToken 应是空实现且返回 nil，实际 %v", err)
	}
	if hits != 0 {
		t.Errorf("不可续期渠道不得请求上游，实际 %d", hits)
	}
	if a.AccessToken != "test.jwt.payload" {
		t.Error("RefreshToken 不得改写凭据")
	}
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   provider.ErrKind
	}{
		{"并发限流 3009", 429, `{"code":3009,"msg":"model concurrency limit exceeded"}`, provider.ErrSoftRate},
		{"额度用尽 1005", 429, `{"code":1005,"msg":"quota exceeded"}`, provider.ErrHardCredit},
		{"订阅余额不足 1113", 429, `{"code":1113}`, provider.ErrHardCredit},
		{"余额不足文本（429 但无 3009）", 429, `{"msg":"余额不足"}`, provider.ErrHardCredit},
		{"风控 3012", 405, `{"code":3012,"msg":"request has been blocked due to unusual activity."}`, provider.ErrClient},
		{"风控短语无码", 405, `request has been blocked due to unusual activity.`, provider.ErrClient},
		{"captcha 索要 3007", 400, `{"code":3007}`, provider.ErrSoftRate},
		{"凭据失效 1002", 401, `{"code":1002}`, provider.ErrSessionDead},
		{"字符串业务码 3012", 405, `{"code":"3012"}`, provider.ErrClient},
		{"本地身份块拦截体", 405, `{"error":{"code":"bad_params","message":"…code 3012…"}}`, provider.ErrBadParams},
		{"本地出站体错误", 400, `{"error":{"code":"bad_params"}}`, provider.ErrBadParams},
		{"402 欠费", 402, `{}`, provider.ErrHardCredit},
		{"普通 429", 429, `{}`, provider.ErrSoftRate},
		{"401", 401, `{}`, provider.ErrSessionDead},
		{"404", 404, `{}`, provider.ErrNotFound},
		{"500", 500, `{}`, provider.ErrServer},
		{"其它 400", 400, `{"msg":"bad request"}`, provider.ErrClient},
		{"200", 200, `{}`, provider.ErrNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.status, tc.body); got != tc.want {
				t.Errorf("Classify(%d, %q) = %v，期望 %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 模型目录
// ---------------------------------------------------------------------------

func TestStaticModels(t *testing.T) {
	got := StaticModels()
	if len(got) != 2 {
		t.Fatalf("静态表应只有协议实测的 2 个模型，实际 %d", len(got))
	}
	byID := map[string]provider.ModelInfo{}
	for _, m := range got {
		byID[m.ID] = m
	}
	flash, ok := byID["GLM-5.3-Flash"]
	if !ok {
		t.Fatal("缺 GLM-5.3-Flash")
	}
	if !flash.SupportsImages || flash.ContextWindow != 1_000_000 || flash.MaxTokens != 128_000 {
		t.Errorf("GLM-5.3-Flash 能力字段不符：%+v", flash)
	}
	if len(flash.ReasoningEfforts) != 3 || flash.DefaultReasoningEffort != "max" {
		t.Errorf("档位应透传 low/high/max + 默认 max：%+v", flash)
	}
	if flash.CanDisableThinking == nil || *flash.CanDisableThinking {
		t.Error("本渠道无关闭思考的写法 ⇒ can_disable_thinking 应为 false")
	}
	pro := byID["GLM-5.3"]
	if pro.SupportsImages {
		t.Error("GLM-5.3 上游 capabilities 为空对象 ⇒ 不得声明 vision")
	}
	for _, m := range got {
		if !m.SupportsTools {
			t.Errorf("%s 应声明工具能力（渠道级）", m.ID)
		}
		if !m.ContextFromAPI {
			t.Errorf("%s 的容量来自上游声明 ⇒ ContextFromAPI 应为 true", m.ID)
		}
	}
}

func TestFetchModelsDynamicAndFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpClientConfigs {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// platform 必须是 unknown（协议 §1.2.2）。
		if !strings.Contains(r.URL.RawQuery, "platform=unknown") {
			t.Errorf("client/configs 的 platform 必须是 unknown：%q", r.URL.RawQuery)
		}
		if r.Header.Get(HdrAuthorization) == "" || r.Header.Get(HdrDeviceMid) == "" {
			t.Error("client/configs 需要 Authorization + X-Device-Mid")
		}
		// builtinModels 是**对象**（键为序号字串），不是数组。
		_, _ = io.WriteString(w, `{"code":0,"data":{"builtinModels":{
			"0":{"modelId":"GLM-5.3-Flash","contextWindow":1000000,"maxCompletionTokens":128000,
			     "capabilities":{"vision":true},"modalities":{"input":["text","image"]},
			     "reasoning":{"levels":{"max":{"anthropic":{"set":{"path":["output_config","effort"],"value":"max"}}},
			                            "low":{"anthropic":{"set":{"path":["output_config","effort"],"value":"low"}}}},
			                  "defaultLevel":"max"}},
			"1":{"modelId":"GLM-5.3","contextWindow":1000000,"maxCompletionTokens":128000,
			     "capabilities":{},"modalities":{"input":["text"]},
			     "reasoning":{"levels":{},"defaultLevel":""}}}}}`)
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	models, err := c.FetchModels(testAuth(nil))
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(models) != 2 || models[0].ID != "GLM-5.3-Flash" || models[1].ID != "GLM-5.3" {
		t.Fatalf("动态目录解析不符：%+v", models)
	}
	// 档位展示序 low → max（未声明的 high 不编造）。
	if got := strings.Join(models[0].ReasoningEfforts, ","); got != "low,max" {
		t.Errorf("档位序 = %q，期望 low,max", got)
	}
	if models[0].DefaultReasoningEffort != "max" {
		t.Errorf("defaultLevel 应为 max，实际 %q", models[0].DefaultReasoningEffort)
	}
	if models[1].SupportsImages {
		t.Error("capabilities 为空对象时不得声明 vision")
	}

	// 远端失败 → 回退静态表且**不返回错误**。
	bad := NewWithBase("http://127.0.0.1:1")
	fallback, err := bad.FetchModels(testAuth(nil))
	if err != nil {
		t.Fatalf("目录失败不得返回错误：%v", err)
	}
	if len(fallback) != 2 {
		t.Errorf("应回退静态兜底表 2 条，实际 %d", len(fallback))
	}
}

func TestFetchModelPricingEmpty(t *testing.T) {
	c := NewWithBase("http://127.0.0.1:1")
	got, err := c.FetchModelPricing(testAuth(nil))
	if err != nil || len(got) != 0 {
		t.Fatalf("本渠道无定价端点 ⇒ 应返回空切片且无错误，实际 %v %v", got, err)
	}
}
