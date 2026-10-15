// reasoning_test.go 思考档位透传测试（issue #96）。
//
// 原则与能力字段同款：只透传上游目录真实返回的档位，不自造数据。
// 形状依据：ref/Buddy2api v2.1.11（reasoning.supportedEfforts/defaultEffort/
// canDisableThinking；deepseek 系只有老式 effort 默认档）。
package upstream

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// TestFetchModelsPassesThroughReasoning 目录 reasoning 对象应原样投影进 ModelInfo。
func TestFetchModelsPassesThroughReasoning(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		body := `{"code":0,"data":{
			"models":[
				{"id":"glm-5.3","name":"GLM-5.3","supportsReasoning":true,
				 "reasoning":{"supportedEfforts":["low","high","max","max",""],"defaultEffort":"high","canDisableThinking":true}},
				{"id":"deepseek-v4-pro","name":"DS Pro","supportsReasoning":true,
				 "reasoning":{"effort":"high"}},
				{"id":"plain","name":"Plain"}
			],
			"agents":[{"name":"cli","models":["glm-5.3","deepseek-v4-pro","plain"]}]
		}}`
		return jsonResp(200, body), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	byID := map[string]provider.ModelInfo{}
	for _, m := range infos {
		byID[m.ID] = m
	}
	// glm-5.3：完整档位表（重复与空串清洗），默认档 + 可关闭
	glm := byID["glm-5.3"]
	if len(glm.ReasoningEfforts) != 3 || glm.ReasoningEfforts[0] != "low" || glm.ReasoningEfforts[2] != "max" {
		t.Fatalf("glm-5.3 efforts = %v, want [low high max]", glm.ReasoningEfforts)
	}
	if glm.DefaultReasoningEffort != "high" {
		t.Fatalf("glm-5.3 default = %q, want high", glm.DefaultReasoningEffort)
	}
	if !glm.ReasoningCanDisable {
		t.Fatalf("glm-5.3 canDisable = %v, want true", glm.CanDisableThinking)
	}
	// deepseek 系：只有老式 effort 默认档、无列表 → 只透出默认档
	ds := byID["deepseek-v4-pro"]
	if ds.ReasoningEfforts != nil {
		t.Fatalf("deepseek efforts = %v, want nil", ds.ReasoningEfforts)
	}
	if ds.DefaultReasoningEffort != "high" {
		t.Fatalf("deepseek default = %q, want high（effort 老式字段回退）", ds.DefaultReasoningEffort)
	}
	// 无 reasoning 对象：三者全空（不得编造）
	pl := byID["plain"]
	if pl.SupportedEfforts != nil || pl.DefaultEffort != "" || pl.ReasoningCanDisable {
		t.Fatalf("plain 不应有档位痕迹: %+v", pl)
	}
}

// TestApplyCatalogReasoningEmpty 空对象/缺对象不留痕迹。
func TestApplyCatalogReasoningEmpty(t *testing.T) {
	if eff, def, cd := applyCatalogReasoning(&catalogModel{ID: "x"}); eff != nil || def != "" || cd != nil {
		t.Fatalf("无 reasoning 对象应有 nil 痕迹: %v %q %v", eff, def, cd)
	}
	if eff, def, cd := applyCatalogReasoning(&catalogModel{ID: "x", Reasoning: modelReasoningMeta{}}); eff != nil || def != "" || cd != nil {
		t.Fatalf("空 reasoning 对象应整体视为未声明: %v %q %v", eff, def, cd)
	}
}

// TestCleanEfforts 规范化：去空白/去重/去空串/全无效归 nil。
func TestCleanEfforts(t *testing.T) {
	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"low", "high", "max", "max", ""}, []string{"low", "high", "max"}},
		{[]string{"  low  ", "low"}, []string{"low"}},
		{[]string{"", "   "}, nil},
		{nil, nil},
	}
	for _, c := range cases {
		got := provider.CleanEfforts(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("CleanEfforts(%q) = %v, want %v", c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("CleanEfforts(%q) = %v, want %v", c.in, got, c.want)
			}
		}
	}
}
