package workbuddyai

// catalog_fallback_test.go 锁死目录端点的降级链。
//
// 2026-11-14 实测：/v2/enterprises/personal/models 对三个账号一致返回
// 404 {"error_msg":"404 Route Not Found"}（上游已下线该路由），而 /v3/config
// 仍正常且 data 结构与 v2 同形。这里验证：v2 404 时自动切 v3、记住不再重试 v2、
// 其它错误（401/网络）不触发切换、v3 也能提供 cli 面与促销折扣。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const v3ConfigBody = `{"code":0,"msg":"ok","data":{
  "agents":[{"name":"cli","models":["hy3","gpt-6-sol"]}],
  "models":[
    {"id":"hy3","name":"Hy3","credits":"x0.00","maxInputTokens":128000,"maxOutputTokens":32000,
     "supportsImages":true,"supportsToolCall":true},
    {"id":"gpt-6-sol","name":"GPT-6-Sol","credits":"x1.33 credits","maxInputTokens":1000000},
    {"id":"hy-image-3.5","name":"Hy Image","credits":"x2.00"}
  ],
  "modelPromotions":[{"enabled":true,"modelIds":["hy3"],
    "discount":{"factor":0},"badge":{"label":"Free now","color":"#FF0000"}}]
}}`

// resetCatalogFallback 清空包级目录缓存（含 v2 下线标记与 v3 缓存）。
func resetCatalogFallback() {
	cache.mu.Lock()
	cache.models, cache.promo, cache.badges, cache.colors = nil, nil, nil, nil
	cache.fetched = time.Time{}
	cache.v2Down, cache.v2RetryAt = false, time.Time{}
	cache.v3, cache.v3OK, cache.v3Fetched = nil, false, time.Time{}
	cache.mu.Unlock()
}

func TestCatalogFallsBackToV3On404(t *testing.T) {
	resetCatalogFallback()
	t.Cleanup(resetCatalogFallback)
	var v2Calls, v3Calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCatalog:
			atomic.AddInt32(&v2Calls, 1)
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error_msg":"404 Route Not Found"}`)
		case EpCatalogV3:
			atomic.AddInt32(&v3Calls, 1)
			if ua := r.Header.Get("User-Agent"); ua != clientUA {
				t.Errorf("v3 必须带 CLI UA，实际 %q", ua)
			}
			_, _ = io.WriteString(w, v3ConfigBody)
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewWithBase(srv.URL)

	models, err := c.FetchModels(testAuth())
	if err != nil {
		t.Fatalf("v2 404 应回退 v3，而不是报错：%v", err)
	}
	// 模型面按 cli agent 顺序，只含 hy3 / gpt-6-sol（hy-image-3.5 不在 cli 面）。
	var ids []string
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	joined := strings.Join(ids, ",")
	if !strings.Contains(joined, "hy3") || !strings.Contains(joined, "gpt-6-sol") {
		t.Fatalf("模型列表缺少 cli 面模型：%s", joined)
	}
	if atomic.LoadInt32(&v2Calls) != 1 || atomic.LoadInt32(&v3Calls) != 1 {
		t.Fatalf("首轮应各请求一次：v2=%d v3=%d", v2Calls, v3Calls)
	}

	// 记住 v2 已下线：清掉目录缓存后再刷，不应再撞 v2。
	cache.mu.Lock()
	cache.models, cache.fetched = nil, time.Time{}
	cache.mu.Unlock()
	if _, err := c.FetchModels(testAuth()); err != nil {
		t.Fatalf("第二轮失败：%v", err)
	}
	if n := atomic.LoadInt32(&v2Calls); n != 1 {
		t.Fatalf("v2 已判下线后不应重试，实际 %d 次", n)
	}
}

// TestCatalogDoesNotSwitchOnAuthError 401 属于凭据问题，换端点救不回来，
// 必须原样上抛（上层靠它触发 refresh）。
func TestCatalogDoesNotSwitchOnAuthError(t *testing.T) {
	resetCatalogFallback()
	t.Cleanup(resetCatalogFallback)
	var v3Calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCatalogV3 {
			atomic.AddInt32(&v3Calls, 1)
			_, _ = io.WriteString(w, v3ConfigBody)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"msg":"token expired"}`)
	}))
	t.Cleanup(srv.Close)
	c := NewWithBase(srv.URL)
	if _, err := c.FetchModels(testAuth()); err == nil {
		t.Fatal("401 应上抛")
	}
	if n := atomic.LoadInt32(&v3Calls); n != 0 {
		t.Fatalf("401 不应切 v3，实际请求 %d 次", n)
	}
}

// TestCatalogV3Promotions v3 的 modelPromotions 与 v2 同形（modelIds/factor/badge），
// 促销免费标记必须能带出来。
func TestCatalogV3Promotions(t *testing.T) {
	resetCatalogFallback()
	t.Cleanup(resetCatalogFallback)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCatalog {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, v3ConfigBody)
	}))
	t.Cleanup(srv.Close)
	c := NewWithBase(srv.URL)
	prices, err := c.FetchModelPricing(testAuth())
	if err != nil {
		t.Fatalf("FetchModelPricing: %v", err)
	}
	byModel := map[string]string{}
	for _, p := range prices {
		byModel[p.Model] = p.Note
	}
	if byModel["hy3"] != "Free now" {
		t.Fatalf("hy3 应带促销标签，实际 %q（全部=%v）", byModel["hy3"], byModel)
	}
}
