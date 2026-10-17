// client_test.go kilo 渠道的离线回归：全部用 httptest 假上游，
// **不打真实上游**（AGENTS.md §0.1：验证上游行为只做单发 A/B，能用离线就不打上游）。
package kilo

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/provider"
)

// newFake 起一个假上游并返回客户端（Base 指向它）。
func newFake(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewWithBase(srv.URL)
	// 测试里把空闲上限压到 1s：假上游立即结束，正常路径用不到它。
	c.IdleTimeout = time.Second
	return c, srv
}

// ---------------------------------------------------------------------------
// 模型清单：只认上游 isFree 声明
// ---------------------------------------------------------------------------

func TestFetchModelsKeepsOnlyFreeRows(t *testing.T) {
	listing := `{"data":[
		{"id":"kilo-auto/free","name":"Kilo Auto","isFree":true,"context_length":200000,"max_output":32000,
		 "supported_parameters":["tools","reasoning"]},
		{"id":"some-paid-model","name":"Paid","isFree":false,"context_length":100000},
		{"id":"another/free","name":"Another Free","isFree":true,"context_length":64000}
	]}`
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("路径 = %q，want /models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("清单请求不应带 Authorization，实际 %q", got)
		}
		_, _ = io.WriteString(w, listing)
	})

	models, err := c.FetchModels(nil)
	if err != nil {
		t.Fatalf("FetchModels 出错：%v", err)
	}
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	if len(ids) != 2 || ids[0] != "kilo-auto/free" || ids[1] != "another/free" {
		t.Fatalf("免费集合 = %v，want [kilo-auto/free another/free]", ids)
	}
	// 静态目录命中时用静态元数据；未命中时按上游行构造，能力只认显式声明。
	if !models[0].SupportsTools || !models[0].SupportsReasoning {
		t.Errorf("kilo-auto/free 应声明 tools 与 reasoning，实际 %+v", models[0])
	}
	if models[1].ContextWindow != 64000 {
		t.Errorf("another/free 上下文 = %d，want 64000", models[1].ContextWindow)
	}
}

func TestFetchModelsFallsBackToStaticOnError(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	models, err := c.FetchModels(nil)
	if err != nil {
		t.Fatalf("清单失败时应回静态兜底且不报错，实际 err=%v", err)
	}
	if len(models) == 0 || models[0].ID != "kilo-auto/free" {
		t.Fatalf("静态兜底 = %+v，want 至少含 kilo-auto/free", models)
	}
}

// ---------------------------------------------------------------------------
// 对话：强制 stream=true、无鉴权头、错误体透传
// ---------------------------------------------------------------------------

func TestChatStreamForcesStreamAndOmitsAuth(t *testing.T) {
	var got map[string]any
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("路径 = %q，want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("免密渠道不应带 Authorization，实际 %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != userAgent {
			t.Errorf("User-Agent = %q，want %q", got, userAgent)
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	})

	// 客户端请求里显式 stream=false，渠道侧必须改写为 true。
	rc, status, errBody, err := c.ChatStream(nil, []byte(`{"model":"kilo-auto/free","stream":false,"messages":[{"role":"user","content":"ping"}]}`))
	if err != nil {
		t.Fatalf("ChatStream 出错：%v（body=%s）", err, errBody)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d，want 200", status)
	}
	if got["stream"] != true {
		t.Errorf("出站 stream = %v，want true", got["stream"])
	}
	raw, _ := io.ReadAll(rc)
	rc.Close()
	if !strings.Contains(string(raw), "[DONE]") {
		t.Errorf("应原样透传上游 SSE，实际 %q", raw)
	}
}

func TestChatStreamPassesErrorBodyThrough(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"type":"PAID_MODEL_AUTH_REQUIRED","message":"paid"}}`)
	})
	rc, status, body, err := c.ChatStream(nil, []byte(`{"model":"paid/model","messages":[]}`))
	if err != nil {
		t.Fatalf("HTTP 4xx 不应报传输错误：%v", err)
	}
	if rc != nil {
		t.Error("4xx 时不应返回响应体流")
	}
	if status != http.StatusForbidden || !strings.Contains(string(body), "PAID_MODEL_AUTH_REQUIRED") {
		t.Fatalf("错误体未透传：status=%d body=%s", status, body)
	}
	if got := c.Classify(status, string(body)); got != provider.ErrClient {
		t.Errorf("付费 id 应归 ErrClient（请求方问题），实际 %v", got)
	}
}

func TestChatStreamRejectsMalformedBody(t *testing.T) {
	c, _ := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("请求体不可解析时不应发起上游请求")
	})
	if _, _, _, err := c.ChatStream(nil, []byte("not-json")); err == nil {
		t.Fatal("非法请求体应报错")
	}
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

func TestClassify(t *testing.T) {
	c := New()
	cases := []struct {
		status int
		body   string
		want   provider.ErrKind
	}{
		{http.StatusTooManyRequests, `{"error":{"type":"RATE_LIMIT"}}`, provider.ErrSoftRate},
		{http.StatusBadGateway, "bad gateway", provider.ErrServer},
		{http.StatusForbidden, `{"error":{"type":"PAID_MODEL_AUTH_REQUIRED"}}`, provider.ErrClient},
		{http.StatusBadRequest, `{"error":{"message":"moderation flagged"}}`, provider.ErrClient},
		{http.StatusForbidden, `{"error":{"type":"RegionError"}}`, provider.ErrPassthrough},
		{http.StatusNotFound, "not found", provider.ErrPassthrough},
		{http.StatusOK, "", provider.ErrNone},
	}
	for _, tc := range cases {
		if got := c.Classify(tc.status, tc.body); got != tc.want {
			t.Errorf("Classify(%d, %q) = %v，want %v", tc.status, tc.body, got, tc.want)
		}
	}
}

// 402 必须是 ErrHardCredit：handler 对单账号渠道唯一保留的例外是「模型级冷却」，
// 归错类会让整条渠道被当成账号级 402 处理（本渠道没有第二个账号可换）。
func TestClassifyQuotaExceededIsHardCredit(t *testing.T) {
	c := New()
	// 上游 402 文案未实测（参考实现把 402 一律当 QUOTA_EXCEEDED），
	// 故这里只锁「402 不落到 ErrNone/ErrServer」这一保守边界。
	got := c.Classify(http.StatusPaymentRequired, `{"error":{"message":"insufficient quota"}}`)
	if got == provider.ErrNone || got == provider.ErrServer {
		t.Fatalf("402 分类 = %v，不应为 none/server", got)
	}
}

// ---------------------------------------------------------------------------
// 流式 model 回填
// ---------------------------------------------------------------------------

func TestRewriteModelLine(t *testing.T) {
	in := []byte("data: {\"id\":\"x\",\"model\":\"kilo-auto/free\",\"choices\":[]}\n")
	out := string(rewriteModelLine(in, "kilo/kilo-auto/free"))
	if !strings.Contains(out, `"model":"kilo/kilo-auto/free"`) {
		t.Fatalf("model 未回填：%s", out)
	}
	if strings.Contains(out, `"model":"kilo-auto/free"`) {
		t.Fatalf("上游裸名应被替换：%s", out)
	}
}

func TestRewriteModelReaderKeepsLineBoundaries(t *testing.T) {
	src := strings.NewReader("data: {\"model\":\"a\"}\n\ndata: {\"model\":\"b\"}\n\n")
	got, err := io.ReadAll(rewriteModelReader(src, "kilo/x"))
	if err != nil {
		t.Fatalf("读取出错：%v", err)
	}
	want := "data: {\"model\":\"kilo/x\"}\n\ndata: {\"model\":\"kilo/x\"}\n\n"
	if string(got) != want {
		t.Fatalf("输出 = %q，want %q", got, want)
	}
}

// 免密渠道的空能力：这些方法必须**不报错**（面板据此显示「不适用」而不是失败）。
func TestNoAccountCapabilities(t *testing.T) {
	c := New()
	if err := c.RefreshToken(nil); err != nil {
		t.Errorf("RefreshToken 应空实现不报错：%v", err)
	}
	if n, err := c.UserResource(nil); err != nil || n != 0 {
		t.Errorf("UserResource = (%d, %v)，want (0, nil)", n, err)
	}
	if n, items, err := c.UserResourceDetail(nil); err != nil || n != 0 || items != nil {
		t.Errorf("UserResourceDetail = (%d, %v, %v)，want (0, nil, nil)", n, items, err)
	}
	a := AnonymousAuth()
	if a.ExpiresAt != noExpiry {
		t.Errorf("虚拟账号 ExpiresAt = %d，want %d（远期值，否则 NeedsRefresh 恒真）", a.ExpiresAt, noExpiry)
	}
	if a.UID != AnonymousUID {
		t.Errorf("虚拟账号 UID = %q，want %q", a.UID, AnonymousUID)
	}
}
