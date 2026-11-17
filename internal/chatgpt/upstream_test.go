package chatgpt

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"wild-work/internal/provider"
)

// 本文件锁定 Upstream 适配层的两条纪律（票 04）：
// ① 模型目录**只如实转述**上游给的字段（R49：不编造上下文窗口与能力声明）；
// ② 上游不可用时退回最小静态表，而不是把整条渠道的模型面归零（R143 的反面）。

func TestFetchModelsMapsUpstreamFieldsOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PathModels {
			t.Errorf("path = %q，期望 %q", r.URL.Path, PathModels)
		}
		_, _ = w.Write([]byte(`{"models":[
			{"slug":"gpt-6","title":"GPT-6","max_tokens":8192,"tags":["gpt_5_auto"]},
			{"slug":"","title":"no slug"},
			{"slug":"gpt-6o","title":""}
		]}`))
	}))
	defer srv.Close()

	c := NewWithConfig(Config{Base: srv.URL})
	defer c.Close()

	got, err := c.FetchModels(testAuth(map[string]string{"session": "s1"}))
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	// 无 slug 的条目无法路由 ⇒ 跳过（不得编一个 ID 顶上去）。
	if len(got) != 2 {
		t.Fatalf("got %d 条模型，期望 2 条（无 slug 的必须跳过）：%+v", len(got), got)
	}
	if got[0].ID != "gpt-6" || got[0].Name != "GPT-6" || got[0].MaxTokens != 8192 {
		t.Errorf("第一条 = %+v，期望 ID/Name/MaxTokens 全照上游", got[0])
	}
	// title 缺失时用 slug 顶名字（上游没给名字，不能空着让面板显示空白）。
	if got[1].ID != "gpt-6o" || got[1].Name != "gpt-6o" {
		t.Errorf("第二条 = %+v，期望 Name 回退成 slug", got[1])
	}
	// R49：上游目录里没有的维度一个都不许补。
	for _, m := range got {
		if m.ContextWindow != 0 {
			t.Errorf("%s 的 ContextWindow = %d，期望 0（上游没声明，不得编造）", m.ID, m.ContextWindow)
		}
		if m.SupportsTools || m.SupportsReasoning || m.SupportsImages || m.SupportsImageGeneration {
			t.Errorf("%s 声明了上游没给的能力：%+v", m.ID, m)
		}
		if len(m.ReasoningEfforts) != 0 || m.DefaultReasoningEffort != "" || m.CanDisableThinking != nil {
			t.Errorf("%s 声明了思考档位：%+v", m.ID, m)
		}
	}
}

func TestFetchModelsPropagatesUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":"not logged in"}`))
	}))
	defer srv.Close()

	c := NewWithConfig(Config{Base: srv.URL})
	defer c.Close()

	if _, err := c.FetchModels(testAuth(map[string]string{"session": "s1"})); err == nil {
		t.Fatal("上游 401 必须返回错误（否则 /v1/models 会把空目录当成功并缓存）")
	}
}

// TestStaticModelsMinimalAndHonest 静态兜底表只有 `auto` 一条，且不带任何编造的能力。
func TestStaticModelsMinimalAndHonest(t *testing.T) {
	ms := StaticModels()
	if len(ms) != 1 || ms[0].ID != defaultChatModel {
		t.Fatalf("静态表 = %+v，期望只有一条 %q（具体型号 slug 会随上游改版变化，写死等于编造）", ms, defaultChatModel)
	}
	if ms[0].ContextWindow != 0 || ms[0].SupportsTools || ms[0].SupportsReasoning {
		t.Errorf("静态表条目不得声明上下文/能力：%+v", ms[0])
	}
	// 返回副本：调用方改动不得影响后续调用（Runtime.StaticModels 会被多次读取）。
	ms[0].ID = "tampered"
	if StaticModels()[0].ID != defaultChatModel {
		t.Error("StaticModels 返回了共享切片，调用方改动污染了静态表")
	}
}

// TestNoCreditEndpointsAreNotErrors 无分值/无费率端点必须回 (0, nil) / (nil, nil)，
// 不是报错 —— 报错会让面板与 pool 把「上游没有这个概念」显示成「刷新失败」。
func TestNoCreditEndpointsAreNotErrors(t *testing.T) {
	c := New()
	defer c.Close()
	a := testAuth(nil)

	if n, err := c.UserResource(a); n != 0 || err != nil {
		t.Errorf("UserResource = (%d, %v)，期望 (0, nil)", n, err)
	}
	if n, items, err := c.UserResourceDetail(a); n != 0 || len(items) != 0 || err != nil {
		t.Errorf("UserResourceDetail = (%d, %v, %v)，期望 (0, nil, nil)", n, items, err)
	}
	if p, err := c.FetchModelPricing(a); p != nil || err != nil {
		t.Errorf("FetchModelPricing = (%v, %v)，期望 (nil, nil)", p, err)
	}
	if err := c.DailyCheckin(a); err == nil {
		t.Error("DailyCheckin 必须返回可读错误（本渠道无签到活动），不能假装成功")
	}
}

// TestClassifyDelegates 实例方法必须与包级 Classify 同口径（handler 走实例）。
func TestClassifyDelegates(t *testing.T) {
	c := New()
	defer c.Close()
	cases := []struct {
		status int
		body   string
	}{
		{429, `{"detail":"too many requests"}`},
		{403, `{"detail":"Unusual activity has been detected"}`},
		{403, `{"detail":"forbidden"}`},
		{413, `{"detail":"too large"}`},
		{404, `{"detail":"model not found"}`},
		{401, `{"detail":"not logged in"}`},
		{500, `{"detail":"boom"}`},
	}
	for _, tc := range cases {
		if got, want := c.Classify(tc.status, tc.body), Classify(tc.status, tc.body); got != want {
			t.Errorf("Classify(%d, %q) = %v，包级 = %v", tc.status, tc.body, got, want)
		}
	}
}

// 断言适配层确实实现了 provider.Upstream（少一个方法就编译失败，这里再显式留痕）。
func TestUpstreamInterfaceSatisfied(t *testing.T) {
	var _ provider.Upstream = New()
}