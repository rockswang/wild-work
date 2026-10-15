package officeace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/idle"
	"wild-work/internal/provider"
)

// 推理预算常量（对齐 officeace2api/config.mjs）：
// 上游把 max_tokens 当成「推理 + 正文」的总预算，推理模型的思维链会先把预算吃光 ⇒ 正文空。
const (
	tinyMaxTokens   = 16     // 小于此值原样透传
	defaultMinMax   = 1024   // 推理预算追加量
	maxTokensCeil   = 393216 // 全局上限
)

// glm53NoThinking：glm-5.3 系不吃 thinking 参数，需剥离后重试。
var glm53NoThinking = map[string]bool{
	"glm-5.3": true, "glm-5.3-test": true, "glm-5.3-zx": true, "glm-5-test": true,
}

// stripWhenUnsupported 上游不认这些参数时剥离。
var stripWhenUnsupported = []string{"thinking", "enable_thinking", "reasoning_effort", "stream_options"}

// chatStream 发对话请求：恒 stream:true，按模型改写 body。
func (c *Client) chatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	ac := accountFromAuth(a)
	if ac.BaseURL == "" {
		return nil, 0, []byte("officeace: missing base_url"), fmt.Errorf("officeace: missing base_url")
	}
	out := rewriteBody(body, c.modelForAuth(a))
	req, err := http.NewRequest(http.MethodPost, ac.chatURL(), bytes.NewReader(out))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", "WildWork/1.0 (officeace)")
	if ac.Authorization != "" {
		req.Header.Set("Authorization", ac.Authorization)
	}

	req, cancel := idle.WithCancel(req)
	resp, err := c.StreamHTTP.Do(req)
	if err != nil {
		cancel()
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		cancel()
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		return nil, resp.StatusCode, rb, nil
	}
	return idle.Monitor(resp.Body, 300*time.Second, cancel), http.StatusOK, nil, nil
}

// modelForAuth 取该账号首选模型名（用于 body 中没有 model 时兜底）。当前恒空：model 由客户端传入。
func (c *Client) modelForAuth(_ *auth.Auth) string { return "" }

// rewriteBody 就地改写出站 body（不改动用户语义）：
//  1. 强制 stream=true + stream_options.include_usage；
//  2. glm-5.3 剥离 thinking；
//  3. max_tokens 补足推理预算；
//  4. temperature/top_p 夹取。
func rewriteBody(body []byte, _ string) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	model, _ := m["model"].(string)

	// 1. 恒 stream
	m["stream"] = true
	if _, ok := m["stream_options"]; !ok {
		m["stream_options"] = map[string]any{"include_usage": true}
	}

	// 2. glm-5.3 剥离 thinking
	if glm53NoThinking[model] {
		for _, k := range stripWhenUnsupported {
			delete(m, k)
		}
	}

	// 3. max_tokens 推理预算（仅对推理模型；此处保守地对所有给了 max_tokens 的请求补足）
	if v, ok := m["max_tokens"].(float64); ok && v >= tinyMaxTokens {
		ceiling := float64(maxTokensCeil)
		n := v + float64(defaultMinMax)
		if n > ceiling {
			n = ceiling
		}
		m["max_tokens"] = n
	}

	// 4. 夹取
	if v, ok := m["temperature"].(float64); ok {
		if v < 0 {
			v = 0
		} else if v > 2 {
			v = 2
		}
		m["temperature"] = v
	}
	if v, ok := m["top_p"].(float64); ok {
		if v < 0 {
			v = 0
		} else if v > 1 {
			v = 1
		}
		m["top_p"] = v
	}

	buf, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return buf
}

// fetchModels 拉取上游 /models（OpenAI 形状），映射为 provider.ModelInfo。
// 失败时返回错误（调用方回退本地静态表）。
func (c *Client) fetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	ac := accountFromAuth(a)
	if ac.BaseURL == "" {
		return nil, fmt.Errorf("officeace: missing base_url")
	}
	req, err := http.NewRequest(http.MethodGet, ac.modelsURL(), nil)
	if err != nil {
		return nil, err
	}
	if ac.Authorization != "" {
		req.Header.Set("Authorization", ac.Authorization)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("officeace: models HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	var data struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	out := make([]provider.ModelInfo, 0, len(data.Data))
	for _, d := range data.Data {
		id := strings.TrimSpace(d.ID)
		if id == "" {
			continue
		}
		out = append(out, provider.ModelInfo{
			ID:                 id,
			Name:               id,
			SupportsTools:      true,
			SupportsReasoning:  true, // 华为网关多数为推理模型；未声明具体档位时不透出档位面
		})
	}
	return out, nil
}
