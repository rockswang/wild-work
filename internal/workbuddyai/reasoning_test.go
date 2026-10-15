// reasoning_test.go 思考档位透传测试（issue #96）。
//
// 原则与能力字段同款：只透传上游目录真实返回的档位，不自造数据。
// 形状依据：ref/Buddy2api v2.1.11（reasoning.supportedEfforts/defaultEffort/
// canDisableThinking）+ ref/dsh-workbuddyai-connect（effort 枚举与回退语义）。
package workbuddyai

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"wild-work/internal/provider"
)

// TestFetchModelsPassesThroughReasoning 假上游目录带 reasoning 对象 →
// FetchModels 应把 supportedEfforts/defaultEffort/canDisableThinking 原样带出。
func TestFetchModelsPassesThroughReasoning(t *testing.T) {
	// catalogCache 是包级全局（10min TTL）：先清空数据字段（不能整体覆盖结构体——
	// 那会把已锁住的 mu 一并换掉，Unlock 直接 fatal），避免读到其它测试/上一轮的缓存
	cache.mu.Lock()
	cache.models, cache.v3, cache.v3OK, cache.fetched, cache.v3Fetched = nil, nil, false, time.Time{}, time.Time{}
	cache.mu.Unlock()
	srv := testServer(func(w http.ResponseWriter, r *http.Request, n int) {
		switch r.URL.Path {
		case EpCatalog:
			_, _ = w.Write([]byte(`{"code":0,"data":{
				"models":[
					{"id":"glm-5.3","name":"GLM-5.3","supportsReasoning":true,
					 "reasoning":{"supportedEfforts":["low","high","max","max",""],"defaultEffort":"high","canDisableThinking":true}},
					{"id":"deepseek-v4.1-pro","name":"DS Pro","supportsReasoning":true,
					 "reasoning":{"effort":"high"}},
					{"id":"plain","name":"Plain"}
				],
				"agents":[{"name":"cli","models":["glm-5.3","deepseek-v4.1-pro","plain"]}]
			}}`))
		case EpCatalogV3:
			http.Error(w, `{"code":1}`, http.StatusBadRequest) // v3 失败：退化为 v2 行为
		default:
			http.Error(w, `{"code":1}`, http.StatusBadRequest)
		}
	})
	defer srv.Close()
	c := NewWithBase(srv.URL)
	infos, err := c.FetchModels(testAuth())
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	byID := map[string]provider.ModelInfo{}
	for _, m := range infos {
		byID[m.ID] = m
	}
	// glm-5.3：完整档位表（含重复/空串清洗），默认档 + 可关闭
	glm := byID["glm-5.3"]
	if len(glm.ReasoningEfforts) != 3 || glm.ReasoningEfforts[0] != "low" || glm.ReasoningEfforts[2] != "max" {
		t.Fatalf("glm-5.3 efforts = %v, want [low high max]（重复与空串应清洗）", glm.ReasoningEfforts)
	}
	if glm.DefaultReasoningEffort != "high" {
		t.Fatalf("glm-5.3 default = %q, want high", glm.DefaultReasoningEffort)
	}
	if !glm.ReasoningCanDisable {
		t.Fatalf("glm-5.3 canDisable = %v, want true", glm.CanDisableThinking)
	}
	// deepseek 系（避开 brokenModels 表里的 deepseek-v4-pro）：只有老式 effort 默认档、无列表 → 只透出默认档
	ds := byID["deepseek-v4.1-pro"]
	if ds.ReasoningEfforts != nil {
		t.Fatalf("deepseek efforts = %v, want nil", ds.ReasoningEfforts)
	}
	if ds.DefaultReasoningEffort != "high" {
		t.Fatalf("deepseek default = %q, want high（effort 老式字段回退）", ds.DefaultReasoningEffort)
	}
	// 无 reasoning 对象的模型：三者全空（不得编造）
	pl := byID["plain"]
	if pl.ReasoningEfforts != nil || pl.DefaultReasoningEffort != "" || pl.CanDisableThinking != nil {
		t.Fatalf("plain 不应有档位痕迹: %+v", pl)
	}
}

// TestMergeV3ReasoningOverride v3 目录的档位非空时覆盖 v2（与 credits 同款「非空者优先」）。
func TestMergeV3ReasoningOverride(t *testing.T) {
	v2 := []catalogModel{{ID: "m1", Name: "M1", Reasoning: modelReasoningMeta{SupportedEfforts: []string{"low"}}}}
	v3 := []catalogModel{{ID: "m1", Name: "M1", Reasoning: modelReasoningMeta{
		SupportedEfforts: []string{"low", "high"}, DefaultEffort: "high"}}}
	merged, _ := mergeV3(v2, v3)
	if len(merged) != 1 || len(merged[0].Reasoning.SupportedEfforts) != 2 {
		t.Fatalf("v3 档位应覆盖 v2: %+v", merged)
	}
	// v3 无档位：保留 v2
	merged, _ = mergeV3(v2, []catalogModel{{ID: "m1", Name: "M1"}})
	if len(merged[0].Reasoning.SupportedEfforts) != 1 {
		t.Fatalf("v3 无档位时应保留 v2: %+v", merged[0].Reasoning)
	}
}

// TestReasoningMetaEmptyObjectIsEmpty 全空 reasoning 对象视为未声明（不透出任何档位痕迹）。
func TestReasoningMetaEmptyObjectIsEmpty(t *testing.T) {
	if (modelReasoningMeta{}).hasReasoningMeta() {
		t.Fatal("空对象应视为未声明")
		t.Fatal("仅有 effort 默认档也算已声明")
	}
}

// TestReasoningJSONRoundTrip ModelInfo 带档位时可 JSON 序列化（/v1/models 消费方按 JSON 解析）。
func TestReasoningJSONRoundTrip(t *testing.T) {
	cd := true
	raw, err := json.Marshal(provider.ModelInfo{ID: "m", ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "low", CanDisableThinking: &cd})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back provider.ModelInfo
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back.ReasoningEfforts) != 2 || back.DefaultReasoningEffort != "low" || back.CanDisableThinking == nil || !*back.CanDisableThinking {
		t.Fatalf("round trip 失败: %s → %+v", raw, back)
	}
}
