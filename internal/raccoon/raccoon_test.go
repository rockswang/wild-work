package raccoon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/loopguard"
	"wild-work/internal/provider"
)

// TestClassify 覆盖阶段 C 实测到的全部错误形态（LiteLLM 信封）。
func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   provider.ErrKind
	}{
		{"401 空 token", 401, `{"code":200001,"message":"authorization_empty_error"}`, provider.ErrSessionDead},
		{"401 token 校验失败", 401, `{"code":200003,"message":"authorization_verify_error"}`, provider.ErrSessionDead},
		{"403 也按会话失效", 403, `{"code":200003}`, provider.ErrSessionDead},
		{"本地拒绝的未知模型", 400, `{"error":{"code":"model_not_found"}}`, provider.ErrBadParams},
		{"400 空消息/超长", 400, `{"error":{"code":"400","message":"litellm.BadRequestError: message is empty or message is bigger than 70MB"}}`, provider.ErrPromptTooLong},
		{"429 纯限流", 429, `{"error":{"message":"rate limit exceeded"}}`, provider.ErrSoftRate},
		{"429 带 quota 字样仍按限流（§6.15）", 429, `{"code":200001,"message":"quota exceeded"}`, provider.ErrSoftRate},
		{"402 余额不足", 402, `{"message":"insufficient credit"}`, provider.ErrHardCredit},
		{"404", 404, `404 page not found`, provider.ErrNotFound},
		{"500", 500, `internal error`, provider.ErrServer},
		{"成功", 200, `{"id":"x"}`, provider.ErrNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.status, tc.body); got != tc.want {
				t.Fatalf("Classify(%d, %s) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// TestChatStreamRejectsUnknownModel 守门：上游对未知模型会静默回落默认模型，
// 渠道必须本地拒绝，否则用户写错模型名会被静默计费。
func TestChatStreamRejectsUnknownModel(t *testing.T) {
	c := New()
	a := &auth.Auth{AccessToken: "dummy"}
	_, status, body, err := c.ChatStream(a, []byte(`{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != 400 {
		t.Fatalf("status = %d, want 400", status)
	}
	if !strings.Contains(string(body), "model_not_found") {
		t.Fatalf("body 应含 model_not_found，实际: %s", body)
	}
}

func TestKnownModel(t *testing.T) {
	for _, id := range []string{DefaultModel, ClientAliasModel, "sn-kimi-k3", "raccoon-405a1c"} {
		if !KnownModel(id) {
			t.Fatalf("KnownModel(%q) = false, want true", id)
		}
	}
	if KnownModel("nope-xyz") {
		t.Fatal("KnownModel(未知) 应为 false")
	}
}

// TestKnownModelAcceptsLiveCatalog 回归（2026-10-05 实测）：
//
//	上游目录里有、静态表里没有的模型（如 sn-sensenova-6-8-flash）必须可调。
//	旧实现只查静态表 ⇒ /v1/models 正常列出 9 个模型，但其中
//	sn-sensenova-6-8-flash 调对话被本地 400 拒（上游实际 200 正常服务）——
//	「面板列出却调不动」。修法：fetchCatalog 成功后把目录记进 Client，
//	校验时与静态表取并集。
func TestKnownModelAcceptsLiveCatalog(t *testing.T) {
	c := New()
	const live = "sn-sensenova-6-8-flash" // 不在 staticModels 里

	if c.knownModel(live) {
		t.Fatal("前提：该模型不应在静态表里")
	}
	if KnownModel(live) {
		t.Fatal("前提：KnownModel 不应认它（静态表）")
	}

	// 假上游返回含该模型的目录。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"categories":[{"type":"chat","models":[
			{"name":"SenseNova","model_name":"sn-sensenova-6-8-flash","billing_multiplier":0.5},
			{"name":"Kimi","model_name":"sn-kimi-k3"}
		]}]}}`))
	}))
	defer srv.Close()
	c.Base = srv.URL

	a := &auth.Auth{AccessToken: "dummy", ApiHost: srv.URL}
	if _, err := c.FetchModels(a); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}

	// 拉过目录后，该模型应被接受。
	if !c.knownModel(live) {
		t.Fatalf("拉取动态目录后 %q 仍被判未知（面板列出却调不动）", live)
	}
	// 真调一次：不应再返回本地 400 model_not_found。
	_, status, body, err := c.ChatStream(a, []byte(`{"model":"`+live+`","messages":[]}`))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if status == 400 && strings.Contains(string(body), "model_not_found") {
		t.Fatalf("仍被本地误拒：%s", body)
	}

	// 单调扩大：目录里没出现的旧模型不应被忘掉。
	if !c.knownModel("sn-kimi-k3") {
		t.Fatal("已知模型不应被遗忘")
	}
}

func TestStaticModelsNonEmpty(t *testing.T) {
	ms := StaticModels()
	if len(ms) != len(staticModels) {
		t.Fatalf("StaticModels 应返回副本，got %d", len(ms))
	}
	ms[0].ID = "mutated"
	if staticModels[0].ID == "mutated" {
		t.Fatal("StaticModels 必须返回副本，不能共享底层数组")
	}
}

// sse 构造一条 data 行。
func sseLine(v any) string {
	raw, _ := json.Marshal(v)
	return "data: " + string(raw) + "\n\n"
}

func TestStreamRewritesModelAndEmitsDone(t *testing.T) {
	in := sseLine(map[string]any{
		"id": "a", "object": "chat.completion.chunk", "model": "upstream-name",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hi"}}},
	}) + "data: [DONE]\n\n"

	rec := httptest.NewRecorder()
	if _, err := Stream(rec, strings.NewReader(in), "raccoon/sn-kimi-k3"); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"model":"raccoon/sn-kimi-k3"`) {
		t.Fatalf("model 未被重写为客户端模型名: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("缺少 [DONE]: %s", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

// TestStreamAppendsDoneWhenUpstreamOmitsIt 覆盖上游无 [DONE] 的异常收尾。
func TestStreamAppendsDoneWhenUpstreamOmitsIt(t *testing.T) {
	in := sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "x"}}}})
	rec := httptest.NewRecorder()
	if _, err := Stream(rec, strings.NewReader(in), "m"); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("应在流末尾补 [DONE]: %s", rec.Body.String())
	}
}

func TestAggregateMergesDeltaAndReasoning(t *testing.T) {
	in := sseLine(map[string]any{
		"id": "id-1", "created": 1700000000, "model": "upstream",
		"choices": []any{map[string]any{"index": 0,
			"delta": map[string]any{"role": "assistant", "reasoning_content": "think"}}},
	}) +
		sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "he"}}}}) +
		sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "llo"}}}}) +
		sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}) +
		sseLine(map[string]any{"usage": map[string]any{"total_tokens": 7}}) +
		"data: [DONE]\n\n"

	out, err := aggregate(strings.NewReader(in), "raccoon/x")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if out["model"] != "raccoon/x" {
		t.Fatalf("model = %v", out["model"])
	}
	if out["object"] != "chat.completion" {
		t.Fatalf("object = %v", out["object"])
	}
	choices := out["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "hello" {
		t.Fatalf("content = %v, want hello", msg["content"])
	}
	if msg["reasoning_content"] != "think" {
		t.Fatalf("reasoning_content = %v", msg["reasoning_content"])
	}
	if choices[0].(map[string]any)["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", choices[0].(map[string]any)["finish_reason"])
	}
	if _, ok := out["usage"]; !ok {
		t.Fatal("usage 应被带上")
	}
}

func TestAggregateMergesToolCallArguments(t *testing.T) {
	delta1 := map[string]any{"tool_calls": []any{
		map[string]any{
			"index": 0, "id": "call_1", "type": "function",
			"function": map[string]any{"name": "f", "arguments": `{"a":`},
		},
	}}
	delta2 := map[string]any{"tool_calls": []any{
		map[string]any{
			"index":    0,
			"function": map[string]any{"arguments": "1}"},
		},
	}}
	in := sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta1}}}) +
		sseLine(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta2}}}) +
		"data: [DONE]\n\n"

	out, err := aggregate(strings.NewReader(in), "m")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls 数量 = %d", len(tcs))
	}
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != `{"a":1}` {
		t.Fatalf("arguments 拼接结果 = %v", fn["arguments"])
	}
}

// TestJwtExp 覆盖 refresh 后从 JWT 取到期时间的路径。
func TestJwtExp(t *testing.T) {
	// {"exp":1790089841}
	tok := "h." + "eyJleHAiOjE3OTAwODk4NDF9" + ".s"
	if got := jwtExp(tok); got != 1790089841 {
		t.Fatalf("jwtExp = %d", got)
	}
	if got := jwtExp("not-a-jwt"); got != 0 {
		t.Fatalf("非 JWT 应返回 0，got %d", got)
	}
}

// TestParsePointsBalance 覆盖积分余额的四个池子与充值冻结标记（实测响应形状）。
func TestParsePointsBalance(t *testing.T) {
	raw := []byte(`{"code":0,"message":"success","data":{"available_points":6313,"daily_points":313,` +
		`"monthly_points":0,"reward_points":6000,"topup_points":0,"topup_frozen":false}}`)
	var resp pointsBalanceResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	remain, items := parsePointsBalance(resp.Data)
	if remain != 6313 {
		t.Fatalf("available = %d, want 6313", remain)
	}
	if len(items) != 4 {
		t.Fatalf("items = %d, want 4（每日/奖励/充值/月度）", len(items))
	}
	byName := map[string]provider.ResourceItem{}
	for _, it := range items {
		byName[it.Name] = it
	}
	if byName["每日额度"].Remain != 313 || byName["奖励积分"].Remain != 6000 {
		t.Fatalf("池子拆分不对: %+v", items)
	}
	if !byName["充值积分"].Usable {
		t.Fatal("topup_frozen=false 时充值积分应可用")
	}

	// 冻结场景：充值池标记为不可用，Summarize 应把它算进 unusable（面板分开展示）。
	// 测试数据保持自洽：实测 available_points == 可用池之和（313+6000=6313），
	// 故冻结的 50 不计入 available。
	frozen := pointsBalanceData{AvailablePoints: 100, DailyPoints: 100, TopupPoints: 50, TopupFrozen: true}
	_, items2 := parsePointsBalance(frozen)
	for _, it := range items2 {
		if it.Name == "充值积分" && it.Usable {
			t.Fatal("topup_frozen=true 时充值积分必须 Usable=false")
		}
	}
	usable, unusable := provider.Summarize(items2)
	if usable != 100 || unusable != 50 {
		t.Fatalf("Summarize = (%d,%d), want (100,50)", usable, unusable)
	}
	if usable != frozen.AvailablePoints {
		t.Fatalf("available_points(%d) 应等于可用池之和(%d)", frozen.AvailablePoints, usable)
	}
}

// TestForceUpstreamDeepThinking 守门：本渠道必须剥离 `reasoning_effort`，走上游默认（= 深度思考）。
//
// 取证（2026-09-24 VM 实测，reasoning_tokens 多轮交叉采样）：
//
//	无字段（上游默认）      rtok = 2300 / 2147 / 2133 / 4067
//	reasoning_effort=high  rtok = 142 / 260 / 331 / 135
//	reasoning_effort=low   rtok = 0
//
// 即上游默认档本身最深，下发任何档位都会削弱思考量。故本渠道对带 `reasoning_effort`
// 的请求一律剥离（客户端档位选择器/面板默认档在此被中和），确保实际走深度思考。
func TestForceUpstreamDeepThinking(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantEffort string // 期望残留值；空串 = 字段必须不存在
	}{
		{"未表达 → 不改写", `{"model":"m","messages":[]}`, ""},
		{"high → 剥离", `{"model":"m","reasoning_effort":"high"}`, ""},
		{"low → 剥离", `{"model":"m","reasoning_effort":"low"}`, ""},
		{"medium → 剥离", `{"model":"m","reasoning_effort":"medium"}`, ""},
		{"none → 剥离", `{"model":"m","reasoning_effort":"none"}`, ""},
		{"空串 → 剥离", `{"model":"m","reasoning_effort":""}`, ""},
		{"上游未声明的 ultra → 剥离", `{"model":"m","reasoning_effort":"ultra"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := forceUpstreamDeepThinking([]byte(tc.in))
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Fatal(err)
			}
			got, _ := m["reasoning_effort"].(string)
			if got != tc.wantEffort {
				t.Fatalf("reasoning_effort = %q, want %q", got, tc.wantEffort)
			}
			// 其它字段不得丢失（model/messages 是路由与模型校验的依赖）。
			if m["model"] != "m" {
				t.Fatalf("model 字段丢失: %v", m)
			}
		})
	}
	t.Run("未表达时输出与输入逐字节一致", func(t *testing.T) {
		raw := []byte(`{"model":"m","messages":[]}`)
		if got := string(forceUpstreamDeepThinking(raw)); got != string(raw) {
			t.Fatalf("未表达档位时不应改写：%s", got)
		}
	})
	t.Run("非法 JSON 原样返回", func(t *testing.T) {
		raw := []byte(`not-json`)
		if got := string(forceUpstreamDeepThinking(raw)); got != string(raw) {
			t.Fatalf("非法 JSON 应原样返回，得到 %s", got)
		}
	})
}

// TestStreamClientHasNoTotalTimeout 守门：流式必须走**无总超时**的 client。
//
// 与 loomy 同源（见其同名测试的完整背景）：http.Client.Timeout 是整请求上限，
// 会把长思考的 SSE 流从中间掐断。本渠道「剥离档位、走上游默认最深思考」，
// 生成期天然更长，风险更高。非流式 client 必须保留总超时。
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

// TestIdleTimeoutDefaults 未注入配置时回落默认值。
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

// TestStreamTruncationEmitsFrames 守门：流中断必须补一帧 error，且不得补 [DONE]。
//
// 此前 sc.Err() != nil 时直接 return，客户端收到无收尾的截断流 =「突然无响应」。
// 但补 [DONE] 同样不对（R36 / issue #42）：[DONE] 表示正常结束，与 error 帧一起
// 发出等于把截断伪装成成功收尾，客户端会拿着半截 tool_call arguments 去解析。
func TestStreamTruncationEmitsFrames(t *testing.T) {
	in := sseLine(map[string]any{"id": "a", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hi"}}}})
	rc := &errAfterReader{data: in, err: provider.ErrIdleTimeout}

	rec := httptest.NewRecorder()
	_, err := Stream(rec, rc, "raccoon/raccoon-8c4485")
	if err == nil {
		t.Fatal("读错误必须上抛给 handler（用于日志）")
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"upstream_timeout"`) {
		t.Fatalf("缺少 upstream_timeout 错误帧: %s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("截断不得补 [DONE]: %s", body)
	}
	if !strings.Contains(body, "hi") {
		t.Fatalf("已透传内容丢失: %s", body)
	}
}

// errAfterReader 先吐完 data，再返回指定错误（模拟「读到一半流断了」）。
type errAfterReader struct {
	data string
	err  error
	off  int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, r.err
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

// TestRefreshWithTokenWebFirst 刷新必须**先试 web 前缀**（2026-10-10 实测 electron 旧前缀已 404），
// 并正确解析出轮换后的 access/refresh。
func TestRefreshWithTokenWebFirst(t *testing.T) {
	var hit []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = append(hit, r.URL.Path)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["refresh_token"] != "old-rt" {
			t.Errorf("提交的 refresh_token 不符：%v", body)
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"new-at","refresh_token":"new-rt"}}`))
	}))
	defer srv.Close()

	c := New()
	c.AuthBase = srv.URL
	at, rt, err := c.RefreshWithToken("old-rt")
	if err != nil {
		t.Fatalf("RefreshWithToken: %v", err)
	}
	if at != "new-at" || rt != "new-rt" {
		t.Fatalf("轮换后的令牌不符：at=%q rt=%q", at, rt)
	}
	if len(hit) != 1 || hit[0] != EpAuthRefreshWeb {
		t.Fatalf("应先试且只试 web 前缀，实际命中 %v", hit)
	}
}

// TestRefreshFallsBackToElectronOn404 web 前缀 404 时回落 electron 旧前缀（兼容旧上游）。
func TestRefreshFallsBackToElectronOn404(t *testing.T) {
	var hit []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = append(hit, r.URL.Path)
		if r.URL.Path == EpAuthRefreshWeb {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"access_token":"at2","refresh_token":"rt2"}}`))
	}))
	defer srv.Close()

	c := New()
	c.AuthBase = srv.URL
	at, rt, err := c.RefreshWithToken("old-rt")
	if err != nil {
		t.Fatalf("应回落成功：%v", err)
	}
	if at != "at2" || rt != "rt2" {
		t.Fatalf("回落结果不符：at=%q rt=%q", at, rt)
	}
	want := []string{EpAuthRefreshWeb, EpAuthRefresh}
	if len(hit) != 2 || hit[0] != want[0] || hit[1] != want[1] {
		t.Fatalf("回落路径不符：%v", hit)
	}
}

// TestStreamReasoningLoopIntercepted 思考链死循环命中：发 error 帧、不补 [DONE]、
// 返回 *loopguard.Error，且死循环之后的正文不再透传。
func TestStreamReasoningLoopIntercepted(t *testing.T) {
	line := "这里是一个足够长的、会被模型机械重复的思考行内容"
	var in strings.Builder
	for i := 0; i < 10; i++ {
		in.WriteString(sseLine(map[string]any{
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": line + "\n"}}},
		}))
	}
	in.WriteString(sseLine(map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "正文"}}},
	}))
	in.WriteString("data: [DONE]\n\n")

	rec := httptest.NewRecorder()
	_, err := Stream(rec, strings.NewReader(in.String()), "raccoon/x")
	if err == nil {
		t.Fatal("期望守卫命中返回错误，实际 nil")
	}
	var lge *loopguard.Error
	if !asLoopGuardErr(err, &lge) {
		t.Fatalf("期望 *loopguard.Error，得 %T: %v", err, err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"upstream_reasoning_loop"`) {
		t.Errorf("缺少错误帧: %q", body)
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Errorf("守卫命中不得补 [DONE]: %q", body)
	}
	if strings.Contains(body, "正文") {
		t.Errorf("死循环之后的正文不应透传: %q", body)
	}
}

// asLoopGuardErr errors.As 小工具。
func asLoopGuardErr(err error, target **loopguard.Error) bool {
	if err == nil {
		return false
	}
	le, ok := err.(*loopguard.Error)
	if ok {
		*target = le
	}
	return ok
}

// ---------------------------------------------------------------------------
// R107 每日额度逐笔到期（2026-10-20 生产实录驱动）
// ---------------------------------------------------------------------------

// billsFixture 一段实测形状的积分流水（2026-10-20 抓自 RaccoonAddison）。
// 三种 biz_type 都在：daily_grant 带 expire_at，daily_expire / desktop 不带。
const billsFixture = `{"code":0,"message":"success","data":{"items":[
 {"bill_id":"b1","client_type":"web","event_name":"每日积分发放","biz_type":"daily_grant","points":300,
  "created_at":"2026-10-20T08:37:00+08:00","grant_at":"2026-10-20T00:00:00+08:00","expire_at":"2026-10-26T23:59:59+08:00"},
 {"bill_id":"b2","client_type":"web","event_name":"每日积分到期清零","biz_type":"daily_expire","points":-200,
  "created_at":"2026-10-19T23:59:59+08:00","grant_at":"2026-10-13T00:00:00+08:00","expire_at":"2026-10-19T23:59:59+08:00"},
 {"bill_id":"b3","client_type":"web","event_name":"每日积分发放","biz_type":"daily_grant","points":300,
  "created_at":"2026-10-19T08:37:00+08:00","grant_at":"2026-10-19T00:00:00+08:00","expire_at":"2026-10-25T23:59:59+08:00"},
 {"bill_id":"b4","client_type":"desktop","event_name":"桌面端 LLM 对话消耗","biz_type":"desktop","points":-2,
  "created_at":"2026-10-18T21:45:05+08:00"},
 {"bill_id":"b5","client_type":"web","event_name":"每日积分发放","biz_type":"daily_grant","points":300,
  "created_at":"2026-10-18T00:01:11+08:00","grant_at":"2026-10-18T00:00:00+08:00","expire_at":"2026-10-24T23:59:59+08:00"},
 {"bill_id":"b6","client_type":"web","event_name":"服务异常补偿积分","biz_type":"reward_grant","points":1500,
  "created_at":"2026-10-16T05:35:50+08:00"}
]}}`

// TestFetchDailyGrants 只收未到期的 daily_grant，且按到期时刻升序。
//
// 为什么必须升序：上游每日 300 分、7 天有效，消耗按先进先出（最老一笔先花），
// 升序 = 消耗顺序，withDailyExpiry 的余额拆分依赖它。
func TestFetchDailyGrants(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpPointsBills {
			t.Errorf("路径不符：%s（应走 %s）", r.URL.Path, EpPointsBills)
		}
		if got := r.URL.Query().Get("paging.limit"); got == "" {
			t.Error("缺少 paging.limit 分页参数（上游缺省 limit 很小）")
		}
		_, _ = w.Write([]byte(billsFixture))
	}))
	defer srv.Close()

	c := New()
	c.AuthBase = srv.URL
	grants, err := c.fetchDailyGrants(&auth.Auth{AccessToken: "at-1", UID: "u1"})
	if err != nil {
		t.Fatalf("fetchDailyGrants: %v", err)
	}
	if len(grants) != 3 {
		t.Fatalf("在效发放 = %d, want 3（daily_expire/desktop/reward_grant 都不算）: %+v", len(grants), grants)
	}
	for i := 1; i < len(grants); i++ {
		if grants[i-1].ExpireAt >= grants[i].ExpireAt {
			t.Fatalf("未按到期升序: %+v", grants)
		}
	}
	if grants[0].Points != 300 {
		t.Fatalf("金额丢失: %+v", grants[0])
	}
}

// TestFetchDailyGrantsDropsExpired 已过期的发放必须剔除（否则「今日到期」恒为 0）。
func TestFetchDailyGrantsDropsExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"items":[
		 {"biz_type":"daily_grant","points":300,"expire_at":"2020-01-01T23:59:59+08:00"},
		 {"biz_type":"daily_grant","points":300,"expire_at":"2099-01-01T23:59:59+08:00"}
		]}}`))
	}))
	defer srv.Close()

	c := New()
	c.AuthBase = srv.URL
	grants, err := c.fetchDailyGrants(&auth.Auth{AccessToken: "at-1"})
	if err != nil {
		t.Fatalf("fetchDailyGrants: %v", err)
	}
	if len(grants) != 1 || grants[0].ExpireAt != "2099-01-01T23:59:59+08:00" {
		t.Fatalf("已过期发放未被剔除: %+v", grants)
	}
}

// TestWithDailyExpiryFIFO 余额按到期升序逐笔扣减（先进先出），余量挂最后一笔。
//
// 这是本改动的**口径核心**：拆开后 Remain 之和必须恒等于 daily_points，
// 否则 pool 快照与面板余额对不上（Summarize 直接求和）。
func TestWithDailyExpiryFIFO(t *testing.T) {
	items := parsePointsBalanceOnly(pointsBalanceData{
		AvailablePoints: 1000, DailyPoints: 500, RewardPoints: 500,
	})
	grants := []dailyGrant{
		{Points: 300, ExpireAt: "2026-10-21T23:59:59+08:00"},
		{Points: 300, ExpireAt: "2026-10-22T23:59:59+08:00"},
		{Points: 300, ExpireAt: "2026-10-23T23:59:59+08:00"},
	}
	got := withDailyExpiry(items, grants)

	var daily []provider.ResourceItem
	var sum int64
	for _, it := range got {
		if it.Name == dailyItemName {
			daily = append(daily, it)
			sum += it.Remain
		}
	}
	if sum != 500 {
		t.Fatalf("拆分后每日额度合计 = %d, want 500（必须等于 daily_points）", sum)
	}
	// 500 分池子 + 三笔 300：前两笔满额 300，最后一笔只剩 0 → 不产生空条目。
	if len(daily) != 2 {
		t.Fatalf("拆分条目 = %d, want 2（第三笔余额为 0 应省略）: %+v", len(daily), daily)
	}
	if daily[0].Remain != 300 || daily[0].ExpireAt != grants[0].ExpireAt {
		t.Fatalf("第一笔应按最早到期满额: %+v", daily[0])
	}
	if daily[1].Remain != 200 {
		t.Fatalf("第二笔应承接余量 200: %+v", daily[1])
	}

	// Summarize 必须仍等于可用余额（拆分对 pool 快照零影响）。
	usable, _ := provider.Summarize(got)
	if usable != 1000 {
		t.Fatalf("Summarize 可用 = %d, want 1000", usable)
	}
}

// TestWithDailyExpiryPoolExceedsGrants 上游口径漂移（池子比 bills 在效之和还大）时
// 余量挂最后一笔，绝不丢余额。
func TestWithDailyExpiryPoolExceedsGrants(t *testing.T) {
	items := parsePointsBalanceOnly(pointsBalanceData{DailyPoints: 900})
	grants := []dailyGrant{
		{Points: 300, ExpireAt: "2026-10-21T23:59:59+08:00"},
		{Points: 300, ExpireAt: "2026-10-22T23:59:59+08:00"},
	}
	got := withDailyExpiry(items, grants)
	var sum int64
	var n int
	for _, it := range got {
		if it.Name == dailyItemName {
			sum += it.Remain
			n++
		}
	}
	if sum != 900 {
		t.Fatalf("余量丢失：合计 %d, want 900", sum)
	}
	if n != 2 {
		t.Fatalf("条目数 = %d, want 2", n)
	}
}

// TestWithDailyExpirySharesOneLedgerKey 拆分条目必须共用同一个 ledger 差分键。
//
// 差分键 = Key（非空优先）else Name。所有拆分条目必须 Key 为空、Name 相同，
// ledger 的按 key 聚合才会把它们合回「每日额度」一条 —— 与拆分前完全同键同值，
// 升级零假流水；反之若给每条挂上不同的 Key（如拿到期时刻当 Key），一次刷新就会
// 产出 N 条独立流水、快照留 N 条，升级瞬间还会凭空产生一批 earn。
func TestWithDailyExpirySharesOneLedgerKey(t *testing.T) {
	items := parsePointsBalanceOnly(pointsBalanceData{DailyPoints: 900})
	grants := []dailyGrant{
		{Points: 300, ExpireAt: "2026-10-21T23:59:59+08:00"},
		{Points: 300, ExpireAt: "2026-10-22T23:59:59+08:00"},
		{Points: 300, ExpireAt: "2026-10-23T23:59:59+08:00"},
	}
	got := withDailyExpiry(items, grants)
	keys := map[string]bool{}
	for _, it := range got {
		if it.Name != dailyItemName {
			continue
		}
		if it.Key != "" {
			t.Fatalf("拆分条目不得设 Key（会拆散 ledger 差分键）: %+v", it)
		}
		keys[it.Name] = true
	}
	if len(keys) != 1 {
		t.Fatalf("拆分条目应共用 1 个差分键，实际 %d 个: %v", len(keys), keys)
	}
}

// TestWithDailyExpiryNoGrantsUnchanged bills 拿不到发放时原样返回（降级路径）。
func TestWithDailyExpiryNoGrantsUnchanged(t *testing.T) {
	items := parsePointsBalanceOnly(pointsBalanceData{DailyPoints: 300})
	got := withDailyExpiry(items, nil)
	if len(got) != len(items) {
		t.Fatalf("无发放数据时不得改动条目: %+v", got)
	}
	for i := range got {
		if got[i] != items[i] {
			t.Fatalf("条目被改动: %+v", got[i])
		}
	}
}

// TestUserResourceDetailAttachesDailyExpiry 端到端：balance + bills 两次请求，
// 每日额度条目带上逐笔到期时刻，其余池子不受影响。
func TestUserResourceDetailAttachesDailyExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpPointsBalance:
			_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{"available_points":8100,` +
				`"daily_points":2100,"monthly_points":0,"reward_points":6000,"topup_points":0,"topup_frozen":false}}`))
		case EpPointsBills:
			_, _ = w.Write([]byte(billsFixture))
		default:
			t.Errorf("意外路径: %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New()
	c.AuthBase = srv.URL
	remain, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "at-1", UID: "u1"})
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if remain != 8100 {
		t.Fatalf("remain = %d, want 8100", remain)
	}
	var dailyCount int
	var dailySum int64
	for _, it := range items {
		if it.Name != dailyItemName {
			continue
		}
		dailyCount++
		dailySum += it.Remain
		if it.ExpireAt == "" {
			t.Fatalf("每日额度条目必须带到期时刻: %+v", it)
		}
	}
	if dailySum != 2100 {
		t.Fatalf("每日额度合计 = %d, want 2100", dailySum)
	}
	if dailyCount != 3 {
		t.Fatalf("每日额度条目数 = %d, want 3（bills 里三笔在效发放）", dailyCount)
	}
	// 奖励积分不受影响（仍是一条聚合条目、无到期时刻）。
	for _, it := range items {
		if it.Name == "奖励积分" && (it.Remain != 6000 || it.ExpireAt != "") {
			t.Fatalf("奖励积分不应被改动: %+v", it)
		}
	}
}

// TestUserResourceDetailBillsFailureDegrades bills 失败不得影响余额主流程。
func TestUserResourceDetailBillsFailureDegrades(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpPointsBalance:
			_, _ = w.Write([]byte(`{"code":0,"data":{"available_points":300,"daily_points":300}}`))
		case EpPointsBills:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":500}`))
		}
	}))
	defer srv.Close()

	c := New()
	c.AuthBase = srv.URL
	remain, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "at-1", UID: "u1"})
	if err != nil {
		t.Fatalf("bills 失败不得让余额查询失败: %v", err)
	}
	if remain != 300 {
		t.Fatalf("remain = %d, want 300", remain)
	}
	var found bool
	for _, it := range items {
		if it.Name == dailyItemName {
			found = true
			if it.Remain != 300 || it.ExpireAt != "" {
				t.Fatalf("降级后应为原聚合条目（无到期时刻）: %+v", it)
			}
		}
	}
	if !found {
		t.Fatal("每日额度条目丢失")
	}
}

// TestDailyItemNameStable 条目名是 ledger 差分键（Key 为空时退回 Name），不得改动。
//
// 改名 ⇒ 升级瞬间旧键「消失」记 expire、新键「出现」记 earn，凭空一对假流水（R105）。
func TestDailyItemNameStable(t *testing.T) {
	if dailyItemName != "每日额度" {
		t.Fatalf("dailyItemName = %q，改了会换 ledger 差分键并产出假流水", dailyItemName)
	}
	items := parsePointsBalanceOnly(pointsBalanceData{DailyPoints: 1})
	if items[0].Name != dailyItemName || items[0].Key != "" {
		t.Fatalf("每日额度条目必须用稳定 Name 且不设 Key（否则拆分条目会被账本当多条）: %+v", items[0])
	}
}

// parsePointsBalanceOnly 只取条目（忽略余额），测试可读性辅助。
func parsePointsBalanceOnly(d pointsBalanceData) []provider.ResourceItem {
	_, items := parsePointsBalance(d)
	return items
}
