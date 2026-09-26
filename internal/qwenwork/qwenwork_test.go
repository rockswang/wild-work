// qwenwork_test.go 渠道单测：签名、body 改写、SSE 剥壳、费率/明细解析、错误分类。
// 不打真实上游；SSE 用 httptest 或内嵌字符串模拟。
package qwenwork

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// ---------------------------------------------------------------------------
// COSY 签名
// ---------------------------------------------------------------------------

// TestNewCosySession 验证签名材料结构：tempKey 16 字符、cosyKey 可 base64 解出 128 字节。
func TestNewCosySession(t *testing.T) {
	s, err := NewCosySession("uid-1", "nick", "a@b.c", "tok-1")
	if err != nil {
		t.Fatalf("NewCosySession: %v", err)
	}
	if len(s.TempKey) != 16 {
		t.Fatalf("tempKey len = %d, want 16", len(s.TempKey))
	}
	for _, c := range string(s.TempKey) {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("tempKey 含非 hex 字符 %q", c)
		}
	}
	// RSA-1024 密文固定 128 字节 → base64 172 字符
	decoded, err := base64Raw(s.CosyKey)
	if err != nil {
		t.Fatalf("cosyKey not base64: %v", err)
	}
	if len(decoded) != 128 {
		t.Fatalf("cosyKey decoded len = %d, want 128 (RSA-1024)", len(decoded))
	}
	// info 含 uid 与 token（AES 密文，无法直接断言内容，但长度必须非零）
	if s.Info == "" {
		t.Fatal("info 为空")
	}
}

// TestAuthHeaderFormat 验证 Authorization 三段结构与 path 归一化。
func TestAuthHeaderFormat(t *testing.T) {
	s, _ := NewCosySession("uid", "n", "", "tok")
	auth, _, err := s.AuthHeader(`{"x":1}`, "https://gateway.qwenwork.cn/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=x")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(auth, ".", 3)
	if len(parts) != 3 || parts[0] != "Bearer COSY" {
		t.Fatalf("auth 格式非法: %q", auth[:40])
	}
	// 三段签名串确定性：同 ts 不同 body → 不同签名（此处只验不 panic 且格式合法）
	if !strings.Contains(auth, "Bearer COSY.") {
		t.Fatal("缺 COSY 前缀")
	}
}

// ---------------------------------------------------------------------------
// body 改写
// ---------------------------------------------------------------------------

// TestPrepareChatBody 验证必填字段补全 + model key 映射 + 前缀剥离。
func TestPrepareChatBody(t *testing.T) {
	c := New()
	// 缺 request_id/session_id → 自动补；model 带渠道前缀 → 剥离+映射
	in := []byte(`{"model":"qwenwork/flash","messages":[{"role":"user","content":"hi"}]}`)
	out, err := c.prepareChatBody(in)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if json.Unmarshal(out, &obj) != nil {
		t.Fatal("输出非法 JSON")
	}
	if obj["model"] != "flash" {
		t.Fatalf("model = %v, want flash（前缀剥离+映射）", obj["model"])
	}
	if obj["request_id"] == "" || obj["session_id"] == "" {
		t.Fatal("request_id/session_id 未补全")
	}
	if obj["stream"] != true {
		t.Fatal("stream 未强制 true")
	}
	// business 段：上游 1.0.4 起按 body.business 解析模型目录，缺失即 503
	// "Model catalog unavailable"（仅补 Cosy-Business-* 头无效，见 client.go 注释）
	biz, ok := obj["business"].(map[string]any)
	if !ok {
		t.Fatal("business 段缺失（回归：503 Model catalog unavailable）")
	}
	if biz["product"] != "qoder_work" || biz["type"] != "agent" {
		t.Fatalf("business 字段错误: %v", biz)
	}
	if biz["version"] != "1" {
		t.Fatalf("business.version = %v, want \"1\"", biz["version"])
	}
	if _, ok := biz["feature_switches"].(map[string]any); !ok {
		t.Fatalf("business.feature_switches 应为空对象: %v", biz["feature_switches"])
	}

	// 旧 key 兼容映射
	in2 := []byte(`{"model":"qwork-advanced","messages":[]}`)
	out2, _ := c.prepareChatBody(in2)
	var obj2 map[string]any
	json.Unmarshal(out2, &obj2)
	if obj2["model"] != "pro" {
		t.Fatalf("qwork-advanced 应映射 pro, got %v", obj2["model"])
	}

	// 别名 auto
	in3 := []byte(`{"model":"auto","messages":[]}`)
	out3, _ := c.prepareChatBody(in3)
	var obj3 map[string]any
	json.Unmarshal(out3, &obj3)
	if obj3["model"] != "pro" {
		t.Fatalf("auto 应映射 pro, got %v", obj3["model"])
	}
}

// TestModelKeyOf 验证 x-model-key 提取。
func TestModelKeyOf(t *testing.T) {
	if got := modelKeyOf([]byte(`{"model":"flash"}`)); got != "flash" {
		t.Fatalf("modelKeyOf = %q", got)
	}
	if got := modelKeyOf([]byte(`{}`)); got != "pro" {
		t.Fatalf("缺 model 时应回退 pro, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// SSE 剥壳
// ---------------------------------------------------------------------------

// ssePayload 构造一行嵌套 SSE data。
func ssePayload(t *testing.T, inner any, scv int) string {
	bodyB, _ := json.Marshal(inner)
	bodyStr := string(bodyB)
	env := map[string]any{
		"headers":         map[string]any{"Content-Type": []string{"application/json"}},
		"body":            bodyStr,
		"statusCodeValue": scv,
		"statusCode":      "OK",
	}
	b, _ := json.Marshal(env)
	return "data:" + string(b)
}

// TestParseNestedSSE_Chunks 正常 chunk 聚合。
func TestParseNestedSSE_Chunks(t *testing.T) {
	c1 := map[string]any{"id": "x", "choices": []any{map[string]any{
		"delta": map[string]any{"content": "你"}}}}
	c2 := map[string]any{"id": "x", "choices": []any{map[string]any{
		"delta": map[string]any{"content": "好"}, "finish_reason": "stop"}}}
	sse := strings.Join([]string{
		ssePayload(t, c1, 200),
		ssePayload(t, c2, 200),
		"data:{\"body\":\"[DONE]\",\"statusCodeValue\":200}",
	}, "\n") + "\n"

	agg, err := aggregate(strings.NewReader(sse), "qwenwork/flash")
	if err != nil {
		t.Fatal(err)
	}
	msg := agg["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Fatalf("content = %v", msg["content"])
	}
	if agg["model"] != "qwenwork/flash" {
		t.Fatalf("model 回填失败: %v", agg["model"])
	}
}

// TestParseNestedSSE_EnvelopeError 验证 HTTP 200 + envelope 400 错误被剥壳捕获。
func TestParseNestedSSE_EnvelopeError(t *testing.T) {
	sse := `data:{"headers":{"Content-Type":["application/json"]},"body":"{\"code\":\"400\",\"message\":\"request_id is required\"}","statusCodeValue":400,"statusCode":"Bad Request"}` + "\n"
	_, err := aggregate(strings.NewReader(sse), "m")
	if err == nil {
		t.Fatal("应捕获 envelope 错误")
	}
	if !strings.Contains(err.Error(), "request_id is required") {
		t.Fatalf("错误文案不符: %v", err)
	}
}

// TestParseNestedSSE_ReasoningAndTools 验证 reasoning_content 与 tool_calls 合并。
func TestParseNestedSSE_ReasoningAndTools(t *testing.T) {
	c1 := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"reasoning_content": "想"}}}}
	c2 := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"content": "答"}}}}
	c3 := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{
			map[string]any{"index": 0, "id": "c1", "type": "function",
				"function": map[string]any{"name": "get", "arguments": "{\"a\""}},
			map[string]any{"index": 0,
				"function": map[string]any{"arguments": ":1}"}},
		}}}}}
	sse := strings.Join([]string{
		ssePayload(t, c1, 200), ssePayload(t, c2, 200), ssePayload(t, c3, 200),
	}, "\n") + "\n"
	agg, err := aggregate(strings.NewReader(sse), "m")
	if err != nil {
		t.Fatal(err)
	}
	msg := agg["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["reasoning_content"] != "想" {
		t.Fatalf("reasoning = %v", msg["reasoning_content"])
	}
	calls := msg["tool_calls"].([]map[string]any)
	if len(calls) != 1 || calls[0]["function"].(map[string]any)["arguments"] != `{"a":1}` {
		t.Fatalf("tool_calls 合并错误: %v", msg["tool_calls"])
	}
}

// ---------------------------------------------------------------------------
// 余额解析
// ---------------------------------------------------------------------------

// TestParseWalletTotals 验证 /user/wallets 三类汇总解析（回归：内联 struct 标签丢失）。
func TestParseWalletTotals(t *testing.T) {
	raw := []byte(`{"daily_credits":{"total_balance":100},"longterm_credits":{"total_balance":2000},"monthly_credits":{"total_balance":0}}`)
	var w struct {
		DailyCredits    walletTotals `json:"daily_credits"`
		LongtermCredits walletTotals `json:"longterm_credits"`
		MonthlyCredits  walletTotals `json:"monthly_credits"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		t.Fatal(err)
	}
	if w.DailyCredits.TotalBalance != 100 || w.LongtermCredits.TotalBalance != 2000 {
		t.Fatalf("daily=%v longterm=%v（标签丢失回归）", w.DailyCredits, w.LongtermCredits)
	}
}

// TestTruncCredits 验证浮点积分 → int64 直转（与其他渠道 int64(Remaining) 口径一致，不 ×100）。
// 回归：早期实现误 ×100 导致面板显示虚高 100 倍。
func TestTruncCredits(t *testing.T) {
	cases := []struct {
		in  float64
		out int64
	}{{2090.38, 2090}, {2094.8294, 2094}, {0.5, 0}, {0, 0}, {-1, 0}}
	for _, c := range cases {
		if got := truncCredits(c.in); got != c.out {
			t.Errorf("truncCredits(%v) = %d, want %d", c.in, got, c.out)
		}
	}
}

// TestExpireDate 验证 UTC+8 墙钟解析（含带偏移与不带偏移两种形态）。
func TestExpireDate(t *testing.T) {
	if got := expireDate("2026-09-19T00:00:00+08:00"); got != "2026-09-19" {
		t.Fatalf("RFC3339 = %q", got)
	}
	if got := expireDate("2026-09-19T00:00:00"); got != "2026-09-19" {
		t.Fatalf("无时区 = %q", got)
	}
	if got := expireDate(""); got != "" {
		t.Fatalf("空串应返回空, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

// TestClassify 分类矩阵（429 优先于 hardMarker，见 AGENTS.md §6.15）。
func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   provider.ErrKind
	}{
		{429, `{"error":{"message":"quota exceeded"}}`, provider.ErrSoftRate},
		{402, "insufficient credit", provider.ErrHardCredit},
		{401, `{"errorCode":"INVALID_TOKEN"}`, provider.ErrSessionDead},
		{403, `{"code":"101","message":"Signature invalid"}`, provider.ErrSessionDead},
		{404, "not found", provider.ErrNotFound},
		{500, "internal", provider.ErrServer},
		{200, `{"code":"400","message":"request_id is required"}`, provider.ErrNone},
		{400, `{"code":"400","message":"prompt is too long"}`, provider.ErrPromptTooLong},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Stream 透传
// ---------------------------------------------------------------------------

// TestStream_Transfer 验证 SSE 透传（header + model 回填 + [DONE]）。
func TestStream_Transfer(t *testing.T) {
	chunk := map[string]any{"id": "1", "choices": []any{map[string]any{
		"delta": map[string]any{"content": "hi"}}}}
	sse := ssePayload(t, chunk, 200) + "\n"
	// 模拟上游响应
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	rec := httptest.NewRecorder()
	if _, err := Stream(rec, resp.Body, "qwenwork/flash"); err != nil {
		t.Fatal(err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"model":"qwenwork/flash"`) {
		t.Fatalf("model 未回填: %s", out[:200])
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatal("缺 [DONE]")
	}
	if rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatal("Content-Type 错误")
	}
}

// base64Raw 标准 base64 解码辅助。
func base64Raw(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// TestRefreshExpiresInSeconds 验证 refresh 响应的 expires_in 按【秒】解释。
// 回归：早期误按毫秒处理（*time.Millisecond），把 7 天压成 604.8 秒，
// 落盘 expiresAt 比 access token 真实寿命少 ~7 天 → NeedsRefresh 恒为真 →
// 每次请求都刷 token，与千问办公 App 高频互踩直至 refresh token 作废。
// 证据：上游 expires_in=604800 恰好等于 access token JWT 的 iat→exp（7 天）。
func TestRefreshExpiresInSeconds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 上游真实形态：expires_in 以【秒】计
		_, _ = w.Write([]byte(`{"device_token":"dt-new","refresh_token":"rt-new","expires_in":604800}`))
	}))
	defer srv.Close()

	a := &auth.Auth{UID: "u1", AccessToken: "old", RefreshToken: "rt-old"}
	c := New()
	c.Gateway = srv.URL
	before := time.Now().Unix()
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	got := a.ExpiresAt - before
	want := int64(604800)
	if got < want-60 || got > want+60 {
		t.Fatalf("expiresAt 距现在 %ds，want ≈%ds（7 天）；按毫秒解释会得到 ≈604s", got, want)
	}
	if a.AccessToken != "dt-new" || a.RefreshToken != "rt-new" {
		t.Fatalf("token 未更新: %q / %q", a.AccessToken, a.RefreshToken)
	}
}
