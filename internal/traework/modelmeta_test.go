// traework/traecode 模型元数据解析回归（PR #108）：上游 get_detail_param 下发的
// 上下文/输出上限/多模态必须进 /v1/models；且元数据形状突变不得拖垮整张目录。
package traework

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// serveWorkPool 只回应本渠道池（另一池 500，走「另一池失败只记日志」分支）。
func serveWorkPool(t *testing.T, workList string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["function"] == Function {
			_, _ = w.Write([]byte(workList))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// byID 模型表按 ID 建索引，便于逐条断言。
func byID(list []provider.ModelInfo) map[string]provider.ModelInfo {
	m := map[string]provider.ModelInfo{}
	for _, x := range list {
		m[x.ID] = x
	}
	return m
}

// TestFetchModelsCarriesUpstreamMeta 上游声明的容量/能力必须原样透出（不编造）：
// max 优先于 dev；两者皆缺 → 不填且 ContextFromAPI=false；multimodal 直取上游布尔。
func TestFetchModelsCarriesUpstreamMeta(t *testing.T) {
	workList := `{"config_info_list":[
		{"config_name":"full-meta","context_window_tokens":{"dev":200000,"max":1000000},
		 "display_config":{"display_name":"Full","multimodal":true,"model_capability":"reasoning_model"},
		 "model_detail_list":[{"max_tokens":32000}]},
		{"config_name":"dev-only","context_window_tokens":{"dev":128000},
		 "display_config":{"display_name":"Dev"}},
		{"config_name":"max-output-only","display_config":{"display_name":"MO"},
		 "model_detail_list":[{"max_tokens":8192}]},
		{"config_name":"no-meta","display_config":{"display_name":"None"}}]}`
	srv := serveWorkPool(t, workList)

	c := New()
	c.HTTP = srv.Client()
	c.AgentHost = srv.URL

	out, err := c.FetchModels(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	got := byID(out)
	for id, want := range map[string]struct {
		ctx, max int64
		img, api bool
	}{
		"full-meta":       {1000000, 32000, true, true}, // max 档赢过 dev 档
		"dev-only":        {128000, 0, false, true},     // 无 max → 回落 dev
		"max-output-only": {0, 8192, false, true},       // 只有输出上限
		"no-meta":         {0, 0, false, false},         // 全缺省：一个都不编造
	} {
		m, ok := got[id]
		if !ok {
			t.Fatalf("目录缺少 %q：%+v", id, out)
		}
		if m.ContextWindow != want.ctx || m.MaxTokens != want.max ||
			m.SupportsImages != want.img || m.ContextFromAPI != want.api {
			t.Errorf("%s = ctx %d/max %d/img %v/api %v，want %d/%d/%v/%v",
				id, m.ContextWindow, m.MaxTokens, m.SupportsImages, m.ContextFromAPI,
				want.ctx, want.max, want.img, want.api)
		}
	}
}

// TestFetchModelsToleratesMetaShapeDrift 元数据字段形状突变（对象→标量、数组→对象）
// 只丢该条元数据，**不得**让整张目录解析失败——模型列表是可用性问题，元数据是展示问题。
// 去掉 poolEntry 的 json.RawMessage 承接（改回强类型结构）本测试即失败。
func TestFetchModelsToleratesMetaShapeDrift(t *testing.T) {
	workList := `{"config_info_list":[
		{"config_name":"ctx-as-number","context_window_tokens":200000,
		 "display_config":{"display_name":"N"},"model_detail_list":[{"max_tokens":4096}]},
		{"config_name":"list-as-object","context_window_tokens":{"dev":64000,"max":128000},
		 "display_config":{"display_name":"O"},"model_detail_list":{"max_tokens":"32000"}},
		{"config_name":"meta-null","context_window_tokens":null,
		 "display_config":{"display_name":"Null"},"model_detail_list":null}]}`
	srv := serveWorkPool(t, workList)

	c := New()
	c.HTTP = srv.Client()
	c.AgentHost = srv.URL

	out, err := c.FetchModels(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("元数据形状突变不应让目录报错: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("三个模型都应保留，实际 %d: %+v", len(out), out)
	}
	got := byID(out)
	if m := got["ctx-as-number"]; m.ContextWindow != 0 || m.MaxTokens != 4096 || !m.ContextFromAPI {
		t.Errorf("ctx-as-number 应只丢上下文、保留输出上限: %+v", m)
	}
	if m := got["list-as-object"]; m.ContextWindow != 128000 || m.MaxTokens != 0 {
		t.Errorf("list-as-object 应保留上下文、只丢输出上限: %+v", m)
	}
	if m := got["meta-null"]; m.ContextWindow != 0 || m.MaxTokens != 0 || m.ContextFromAPI {
		t.Errorf("meta-null 应全部不填: %+v", m)
	}
}
