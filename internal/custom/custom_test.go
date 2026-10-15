package custom

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChannelConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     ChannelConfig
		wantErr bool
	}{
		{
			name: "valid config with string models",
			cfg: ChannelConfig{
				Name:    "StepFun",
				BaseURL: "https://api.stepfun.com/step_plan/v1/",
				APIKey:  "sk-test",
				Models: []ModelConfig{
					{ID: "step-5-preview"},
					{ID: "step-5-preview"},
				},
			},
			wantErr: false,
		},
		{
			name: "valid config with full model objects",
			cfg: ChannelConfig{
				Name:    "proxycli",
				BaseURL: "http://127.0.0.1:8317/v1",
				Models: []ModelConfig{
					{
						ID:                     "gemini-3.8-flash-high",
						ContextWindow:          1000000,
						MaxTokens:              64000,
						SupportsImages:         true,
						SupportsReasoning:      true,
						ReasoningEfforts:       []string{"low", "medium", "high"},
						DefaultReasoningEffort: "high",
					},
				},
			},
			wantErr: false,
		},
		{
			name:    "empty name",
			cfg:     ChannelConfig{Name: "", BaseURL: "http://127.0.0.1:8000/v1"},
			wantErr: true,
		},
		{
			name:    "invalid name chars",
			cfg:     ChannelConfig{Name: "step/fun", BaseURL: "http://127.0.0.1:8000/v1"},
			wantErr: true,
		},
		{
			name:    "invalid url scheme",
			cfg:     ChannelConfig{Name: "stepfun", BaseURL: "ftp://127.0.0.1:8000/v1"},
			wantErr: true,
		},
		{
			name:    "empty url",
			cfg:     ChannelConfig{Name: "stepfun", BaseURL: ""},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			err := cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr {
				if cfg.Name != strings.ToLower(tc.cfg.Name) {
					t.Errorf("Name should be lowercased, got %q", cfg.Name)
				}
				if strings.HasSuffix(cfg.BaseURL, "/") {
					t.Errorf("BaseURL should strip trailing slash, got %q", cfg.BaseURL)
				}
			}
		})
	}
}

func TestChannelConfigValidateRejectsBuiltinNames(t *testing.T) {
	// R66：自定义渠道名不得与内建渠道/上游命名空间同名。
	// 同名会让 server 侧 getRuntime（内建优先）与 app 侧 UpdateCustomRuntimes（无条件覆盖）
	// 指向不同 Runtime —— 面板/账号操作打到自定义池、对话请求走内建渠道，静默出错。
	for _, name := range []string{"workbuddy", "cline", "oczen", "raccoon", "monkeycode", "cline-free"} {
		c := ChannelConfig{Name: name, BaseURL: "https://example.com/v1"}
		if err := c.Validate(); err == nil {
			t.Errorf("Validate(%q) 应拒绝（与内建渠道/命名空间同名）", name)
		}
	}
	// 非保留名照常通过
	ok := ChannelConfig{Name: "stepfun", BaseURL: "https://example.com/v1"}
	if err := ok.Validate(); err != nil {
		t.Errorf("Validate(stepfun) 不应报错: %v", err)
	}
}

func TestModelConfigPolymorphicJSON(t *testing.T) {
	// 支持简写字符串数组与完整对象数组
	rawJSON := `[
		"step-5-preview",
		{
			"id": "gemini-3.8-flash-high",
			"context_window": 1000000,
			"max_tokens": 64000,
			"supports_images": true,
			"supports_reasoning": true,
			"reasoning_efforts": ["high"],
			"default_reasoning_effort": "high"
		}
	]`
	var models []ModelConfig
	if err := json.Unmarshal([]byte(rawJSON), &models); err != nil {
		t.Fatalf("UnmarshalJSON failed: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}
	if models[0].ID != "step-5-preview" || models[0].ContextWindow != 0 {
		t.Errorf("model 0 mismatch: %+v", models[0])
	}
	if models[1].ID != "gemini-3.8-flash-high" || models[1].ContextWindow != 1000000 || !models[1].SupportsImages || models[1].DefaultReasoningEffort != "high" {
		t.Errorf("model 1 mismatch: %+v", models[1])
	}
}

func TestFetchModelsReturnsConfiguredVerbatimWithoutUpstreamCallOrInference(t *testing.T) {
	// 配的是什么就返回什么，不上游拉取，不推测
	client := New(ChannelConfig{
		Name:    "stepfun",
		BaseURL: "http://127.0.0.1:9999", // 故意给不存在的端口，证明绝不向上游发网络请求
		Models: []ModelConfig{
			{
				ID:                     "step-5-preview",
				ContextWindow:          1000000,
				MaxTokens:              64000,
				SupportsImages:         true,
				SupportsReasoning:      true,
				ReasoningEfforts:       []string{"low", "medium", "high"},
				DefaultReasoningEffort: "high",
			},
			{
				ID: "custom-plain-model", // 没配的字段如实为 0 / false，绝不推测
			},
		},
	})

	models, err := client.FetchModels(nil)
	if err != nil {
		t.Fatalf("FetchModels failed: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(models))
	}

	// 验证第一款模型：完全符合用户配置
	m1 := models[0]
	if m1.ID != "step-5-preview" || m1.ContextWindow != 1000000 || m1.MaxTokens != 64000 {
		t.Errorf("m1 attributes mismatch: %+v", m1)
	}
	if !m1.SupportsImages || !m1.SupportsReasoning || m1.DefaultReasoningEffort != "high" {
		t.Errorf("m1 capabilities mismatch: %+v", m1)
	}

	// 验证第二款模型：没配的绝不推测
	m2 := models[1]
	if m2.ID != "custom-plain-model" || m2.ContextWindow != 0 || m2.SupportsImages || m2.SupportsReasoning {
		t.Errorf("m2 should not guess fields: %+v", m2)
	}
}

func TestCustomClientChatStreamAndStream(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("expected Bearer test-key, got %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" world\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer ts.Close()

	client := New(ChannelConfig{
		Name:    "mychannel",
		BaseURL: ts.URL,
		APIKey:  "test-key",
	})

	rc, status, _, err := client.ChatStream(nil, []byte(`{"model":"upstream-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("ChatStream failed: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d", status)
	}
	defer rc.Close()

	rec := httptest.NewRecorder()
	usage, err := client.Stream(rec, rc, "mychannel/upstream-model")
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	if usage == nil || usage["prompt_tokens"] != float64(10) {
		t.Errorf("expected usage prompt_tokens 10, got %v", usage)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"model":"mychannel/upstream-model"`) {
		t.Errorf("expected rewritten model name in output, got: %s", body)
	}
	if !strings.Contains(body, "Hello") || !strings.Contains(body, "world") {
		t.Errorf("expected content in output, got: %s", body)
	}
}

func TestCustomClientAggregateNonStreamJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{
			"id":      "chatcmpl-2",
			"object":  "chat.completion",
			"model":   "upstream-model",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "non-stream answer"}}},
			"usage":   map[string]any{"prompt_tokens": 8, "completion_tokens": 12, "total_tokens": 20},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client := New(ChannelConfig{
		Name:    "mychannel",
		BaseURL: ts.URL,
	})

	rc, status, _, err := client.ChatStream(nil, []byte(`{"model":"upstream-model","stream":false}`))
	if err != nil {
		t.Fatalf("ChatStream failed: %v", err)
	}
	if status != 200 {
		t.Fatalf("expected 200, got %d", status)
	}
	defer rc.Close()

	resp, err := client.Aggregate(rc, "mychannel/upstream-model")
	if err != nil {
		t.Fatalf("Aggregate failed: %v", err)
	}
	if resp["model"] != "mychannel/upstream-model" {
		t.Errorf("expected model mychannel/upstream-model, got %v", resp["model"])
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("expected choices, got %v", resp)
	}
}

func TestParseTokens(t *testing.T) {
	cases := []struct {
		input any
		want  int64
	}{
		{"1M", 1000000},
		{"1m", 1000000},
		{"1.5M", 1500000},
		{"64k", 64000},
		{"64K", 64000},
		{"128k", 128000},
		{"1000000", 1000000},
		{1000000, 1000000},
		{float64(64000), 64000},
		{"", 0},
	}
	for _, tc := range cases {
		got, err := ParseTokens(tc.input)
		if err != nil {
			t.Fatalf("ParseTokens(%v) error: %v", tc.input, err)
		}
		if got != tc.want {
			t.Errorf("ParseTokens(%v) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

func TestModelConfigUnitStrings(t *testing.T) {
	// 上下文/输出支持 1M / 64k 带单位串；supports_tools 显式 false 时如实保留
	raw := `{
		"id": "m1",
		"context_window": "1M",
		"max_tokens": "64k",
		"supports_images": true,
		"supports_reasoning": true,
		"supports_tools": true,
		"reasoning_efforts": ["low", "high"]
	}`
	var m ModelConfig
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.ContextWindow != 1000000 || m.MaxTokens != 64000 {
		t.Errorf("unit parse mismatch: ctx=%d max=%d", m.ContextWindow, m.MaxTokens)
	}
	if !m.ToolsEnabled() {
		t.Errorf("supports_tools should be true")
	}
	if len(m.ReasoningEfforts) != 2 {
		t.Errorf("efforts mismatch: %v", m.ReasoningEfforts)
	}

	// 缺省 supports_tools = true（向后兼容老配置）
	var m2 ModelConfig
	if err := json.Unmarshal([]byte(`{"id":"m2"}`), &m2); err != nil {
		t.Fatalf("unmarshal m2: %v", err)
	}
	if !m2.ToolsEnabled() {
		t.Errorf("missing supports_tools should default to true")
	}

	// 显式 false 保留
	var m3 ModelConfig
	if err := json.Unmarshal([]byte(`{"id":"m3","supports_tools":false}`), &m3); err != nil {
		t.Fatalf("unmarshal m3: %v", err)
	}
	if m3.ToolsEnabled() {
		t.Errorf("explicit false should be preserved")
	}
	// 字符串形态（最常用的简写）也必须默认支持工具，否则 /v1/models 会静默不带 supports_tools
	var ss []ModelConfig
	if err := json.Unmarshal([]byte(`["step-5-preview"]`), &ss); err != nil {
		t.Fatalf("unmarshal string form: %v", err)
	}
	if !ss[0].ToolsEnabled() {
		t.Errorf("字符串形态模型应默认支持工具（ToolsEnabled=false 会让客户端以为不能发 tools）")
	}
	// 真回环：显式 false 必须能落盘再读回（omitempty + 非指针会把 false 丢掉，读回变 true）
	var c ChannelConfig
	if err := json.Unmarshal([]byte(`{"name":"s","base_url":"https://x/v1","models":[{"id":"m","supports_tools":false}]}`), &c); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(c)
	var c2 ChannelConfig
	if err := json.Unmarshal(out, &c2); err != nil {
		t.Fatal(err)
	}
	if c2.Models[0].ToolsEnabled() {
		t.Errorf("显式 false 经 Marshal/Unmarshal 回环后丢失（omitempty 需配指针类型）")
	}
}
