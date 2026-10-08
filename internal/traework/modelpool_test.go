package traework

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"wild-work/internal/auth"
)

// TestFunctionForModelBuiltInFallback 内置兜底：目录尚未拉取时，已知 solo_agent
// 池专属模型也能按归属池调用——否则新账号首日列表拉取失败前这些模型恒 4001。
func TestFunctionForModelBuiltInFallback(t *testing.T) {
	for model, want := range map[string]string{
		"qwen3.8-flash": FunctionCode,
		"glm-5.3-flash": FunctionCode,
	} {
		if got := functionForModel(model); got != want {
			t.Fatalf("functionForModel(%q) = %q, want %q", model, got, want)
		}
	}
	if got := functionForModel("totally-unknown-model"); got != "" {
		t.Fatalf("未登记的模型应返回空串（沿用渠道默认），实际 %q", got)
	}
}

// TestPrepareBodySelectsPoolByModel PrepareBody 必须按模型归属池覆盖 function：
// 已登记模型用归属池，未登记模型沿用渠道传入的默认 function。
func TestPrepareBodySelectsPoolByModel(t *testing.T) {
	mk := func(model string) []byte {
		return []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model))
	}
	fn := func(body []byte) string {
		var obj map[string]any
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Fatalf("输出不是合法 JSON: %v", err)
		}
		s, _ := obj["function"].(string)
		return s
	}
	// 内置兜底：solo_agent 池专属模型即便在 TraeWork 渠道也按归属池调用
	if got := fn(PrepareBody(mk("glm-5.3-flash"), Function)); got != FunctionCode {
		t.Fatalf("agent 池模型 function = %q, want %q", got, FunctionCode)
	}
	// 未登记模型：沿用渠道默认 function（不悄悄改写）
	if got := fn(PrepareBody(mk("totally-unknown-model"), Function)); got != Function {
		t.Fatalf("未登记模型 function = %q, want %q", got, Function)
	}
	// 空模型名走 DefaultConfigName 兜底（未登记 → 渠道默认）
	if got := fn(PrepareBody([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), Function)); got != Function {
		t.Fatalf("空模型名 function = %q, want %q", got, Function)
	}
}

// TestFetchModelsMergesBothPools 双池合并 + 归属登记：
// 本渠道池先到先得；另一池补充列表并登记自己的模型。
func TestFetchModelsMergesBothPools(t *testing.T) {
	workList := `{"config_info_list":[
		{"config_name":"common-model","display_config":{"display_name":"C"}},
		{"config_name":"work-only-model","display_config":{"display_name":"W"}},
		{"config_name":"custom_model_x","display_config":{"display_name":"X","is_custom_model":true}}]}`
	codeList := `{"config_info_list":[
		{"config_name":"common-model","display_config":{"display_name":"C"}},
		{"config_name":"code-only-model","display_config":{"display_name":"A"}},
		{"config_name":"glm-5.3-flash","display_config":{"display_name":"F"}}]}`

	var gotWork, gotCode atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpModels {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req["function"] {
		case Function:
			gotWork.Add(1)
			_, _ = w.Write([]byte(workList))
		case FunctionCode:
			gotCode.Add(1)
			_, _ = w.Write([]byte(codeList))
		default:
			http.Error(w, "unexpected function", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.AgentHost = srv.URL

	out, err := c.FetchModels(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if gotWork.Load() != 1 || gotCode.Load() != 1 {
		t.Fatalf("两个池各应请求一次：work=%d code=%d", gotWork.Load(), gotCode.Load())
	}
	// 双池合并：common 去重一次、work-only/code-only/flash 各一条；custom_model_ 过滤
	ids := map[string]bool{}
	for _, m := range out {
		ids[m.ID] = true
	}
	for _, want := range []string{"common-model", "work-only-model", "code-only-model", "glm-5.3-flash"} {
		if !ids[want] {
			t.Fatalf("合并列表缺 %q：实际 %v", want, ids)
		}
	}
	if ids["custom_model_x"] {
		t.Fatal("自定义模型应被过滤")
	}
	if len(out) != 4 {
		t.Fatalf("合并后模型数 = %d, want 4（common 去重）: %+v", len(out), out)
	}
	// 归属登记：common 先见于本渠道池（work 赢）；code-only/glm-5.3-flash 归 code 池
	if got := functionForModel("common-model"); got != Function {
		t.Fatalf("common-model 归属 = %q, want %q（先到的池赢）", got, Function)
	}
	if got := functionForModel("code-only-model"); got != FunctionCode {
		t.Fatalf("code-only-model 归属 = %q, want %q", got, FunctionCode)
	}
	// 登记后调用：work 渠道也能用 code-only 模型（function 被按归属覆盖）
	var obj map[string]any
	if err := json.Unmarshal(PrepareBody([]byte(`{"model":"code-only-model","messages":[]}`), Function), &obj); err != nil {
		t.Fatal(err)
	}
	if got, _ := obj["function"].(string); got != FunctionCode {
		t.Fatalf("code-only-model 在 work 渠道调用 function = %q, want %q", got, FunctionCode)
	}
}

// TestFetchModelsSecondPoolFailureTolerated 另一池拉取失败只降级、不整报错：
// 本渠道池结果照常返回（另一池属于增量信息，不能因它失败让整个列表 404）。
func TestFetchModelsSecondPoolFailureTolerated(t *testing.T) {
	workList := `{"config_info_list":[
		{"config_name":"common-model","display_config":{"display_name":"C"}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["function"] == Function {
			_, _ = w.Write([]byte(workList))
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.AgentHost = srv.URL

	out, err := c.FetchModels(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("另一池失败不应整体报错: %v", err)
	}
	if len(out) != 1 || out[0].ID != "common-model" {
		t.Fatalf("应返回本渠道池结果，实际 %+v", out)
	}
}

// TestFetchModelsPrimaryPoolFailureStillErrors 本渠道池失败照旧报错
// （沿用旧行为：目录拉不到就是拉不到，不静默空表）。
func TestFetchModelsPrimaryPoolFailureStillErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.AgentHost = srv.URL

	if _, err := c.FetchModels(&auth.Auth{AccessToken: "at"}); err == nil {
		t.Fatal("本渠道池失败应报错")
	}
}

// TestOtherFunction 双池枚举完备：两个 function 互为另一池，无第三种取值。
func TestOtherFunction(t *testing.T) {
	if otherFunction(Function) != FunctionCode || otherFunction(FunctionCode) != Function {
		t.Fatalf("otherFunction 映射错误：%q→%q / %q→%q", Function, otherFunction(Function), FunctionCode, otherFunction(FunctionCode))
	}
	if strings.Contains(otherFunction("whatever"), "remote") {
		t.Fatal("定价分组名（*_remote）不是池 function")
	}
}
