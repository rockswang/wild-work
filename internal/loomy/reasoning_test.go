// reasoning_test.go 思考档位透传测试（issue #96）。
//
// Loomy 的 /models 为每个模型声明 reasoning_efforts（上游权威档位，实测
// 8 模型一致：none/low/medium/high/xhigh，default=low）——本项目原样透传，
// 不做本地假设。
package loomy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// TestFetchModelsPassesThroughReasoning 上游 reasoning_efforts/default_reasoning_effort
// 应原样进入 ModelInfo（经 CleanEfforts 清洗）。
func TestFetchModelsPassesThroughReasoning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"id":"spark-x","name":"Spark X2.5","reasoning_efforts":["low","low","","high"],"default_reasoning_effort":"low"},
			{"id":"no-effort","name":"No Effort"}
		]}`))
	}))
	defer srv.Close()
	c := New()
	c.Base = srv.URL
	infos, err := c.FetchModels(&auth.Auth{AccessToken: "dummy"})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	byID := map[string]provider.ModelInfo{}
	for _, m := range infos {
		byID[m.ID] = m
	}
	sp := byID["spark-x"]
	if len(sp.ReasoningEfforts) != 2 || sp.ReasoningEfforts[0] != "low" || sp.ReasoningEfforts[1] != "high" {
		t.Fatalf("spark-x efforts = %v, want [low high]（重复与空串应清洗）", sp.ReasoningEfforts)
	}
	if sp.DefaultReasoningEffort != "low" {
		t.Fatalf("spark-x default = %q, want low", sp.DefaultReasoningEffort)
	}
	ne := byID["no-effort"]
	if ne.ReasoningEfforts != nil || ne.DefaultReasoningEffort != "" {
		t.Fatalf("no-effort 不应有档位痕迹: %v %q", ne.ReasoningEfforts, ne.DefaultReasoningEffort)
	}
}
