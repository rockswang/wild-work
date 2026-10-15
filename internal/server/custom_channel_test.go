package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/custom"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

func TestCustomChannelChatCompletionsAndModels(t *testing.T) {
	// 假上游：模拟 StepFun 或本地 CLIProxy
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key-123" {
			t.Errorf("expected Bearer test-key-123, got %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"id":"step-5-preview"}]}`)
			return
		}
		if r.URL.Path == "/chat/completions" {
			var body struct {
				Model  string `json:"model"`
				Stream bool   `json:"stream"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Model != "step-5-preview" {
				t.Errorf("upstream should receive model without channel prefix, got %q", body.Model)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"step-5-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"step-answer\"}}]}\n\n")
			_, _ = fmt.Fprint(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"step-5-preview\",\"choices\":[{\"index\":0,\"delta\":{}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14}}\n\n")
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer upstreamServer.Close()

	chCfg := custom.ChannelConfig{
		Name:    "stepfun",
		BaseURL: upstreamServer.URL,
		APIKey:  "test-key-123",
		Models:  []custom.ModelConfig{{ID: "step-5-preview"}},
	}
	customClient := custom.New(chCfg)

	stateFile := t.TempDir() + "/state.json"
	p := pool.New(stateFile)
	p.Add(&auth.Auth{
		Kind:      "stepfun",
		UID:       "custom-stepfun",
		Nickname:  "StepFun (自定义)",
		ExpiresAt: 4102444800,
	})

	runtimes := map[provider.Kind]*Runtime{
		provider.Kind("stepfun"): {
			Kind:                    provider.Kind("stepfun"),
			Pool:                    p,
			Upstream:                customClient,
			StaticModels:            customClient.StaticModels(),
			SingleAccount:           true,
			NoCooldownOnServerError: true,
		},
	}

	h := NewHandler(Config{
		Runtimes: runtimes,
	})

	// 1. 测试 /v1/models 包含自定义渠道模型
	reqModels := httptest.NewRequest("GET", "/v1/models", nil)
	wModels := httptest.NewRecorder()
	h.ServeHTTP(wModels, reqModels)
	if wModels.Code != 200 {
		t.Fatalf("/v1/models returned status %d: %s", wModels.Code, wModels.Body.String())
	}
	var modelsResp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(wModels.Body.Bytes(), &modelsResp); err != nil {
		t.Fatalf("unmarshal /v1/models failed: %v", err)
	}
	found := false
	for _, m := range modelsResp.Data {
		if m.ID == "stepfun/step-5-preview" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected stepfun/step-5-preview in /v1/models, got %+v", modelsResp.Data)
	}

	// 2. 测试 /v1/chat/completions 路由到自定义渠道
	chatBody := `{"model":"stepfun/step-5-preview","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	reqChat := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatBody))
	wChat := httptest.NewRecorder()
	h.ServeHTTP(wChat, reqChat)
	if wChat.Code != 200 {
		t.Fatalf("chat completions returned status %d: %s", wChat.Code, wChat.Body.String())
	}
	chatResp := wChat.Body.String()
	if !strings.Contains(chatResp, `"model":"stepfun/step-5-preview"`) {
		t.Errorf("expected rewritten model in SSE output, got: %s", chatResp)
	}
	if !strings.Contains(chatResp, "step-answer") {
		t.Errorf("expected step-answer in SSE output, got: %s", chatResp)
	}

	// 3. 测试热更新 SetCustomRuntimes
	p2 := pool.New(t.TempDir() + "/state2.json")
	p2.Add(&auth.Auth{Kind: "cliproxy", UID: "custom-cliproxy", ExpiresAt: 4102444800})
	c2 := custom.New(custom.ChannelConfig{
		Name:    "cliproxy",
		BaseURL: upstreamServer.URL,
		Models:  []custom.ModelConfig{{ID: "gemini-3.8-flash-high"}},
	})
	h.SetCustomRuntimes(map[provider.Kind]*Runtime{
		provider.Kind("cliproxy"): {
			Kind:          provider.Kind("cliproxy"),
			Pool:          p2,
			Upstream:      c2,
			StaticModels:  c2.StaticModels(),
			SingleAccount: true,
		},
	})
	rt, upModel, err := h.runtimeForModel("cliproxy/gemini-3.8-flash-high")
	if err != nil || rt == nil || upModel != "gemini-3.8-flash-high" {
		t.Fatalf("expected runtimeForModel for hot-reloaded cliproxy, got rt=%v, upModel=%q, err=%v", rt, upModel, err)
	}
}
