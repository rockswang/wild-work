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
	tinyMaxTokens = 16     // 小于此值原样透传
	defaultMinMax = 1024   // 推理预算追加量
	maxTokensCeil = 393216 // 全局上限
)

// glm53NoThinking：glm-5.3 系不吃 thinking 参数，需剥离后重试。
var glm53NoThinking = map[string]bool{
	"glm-5.3": true, "glm-5.3-test": true, "glm-5.3-zx": true, "glm-5-test": true,
}

// stripWhenUnsupported 上游不认这些参数时剥离（glm-5.3 系）。
// 注意：**不含 stream_options** —— 它是本工具记账所需的 include_usage 载体，
// 剥掉会让该模型 token 流水恒 0（见下方 rewriteBody 的注入顺序）。
var stripWhenUnsupported = []string{"thinking", "enable_thinking", "reasoning_effort"}

// isReasoningModel 判定是否推理模型（推理模型的 max_tokens 会被思维链吃光 ⇒ 需补预算）。
// 已知非推理的是 qwen3-32b / qwen3-30b-a3b；其余（deepseek/glm/kimi/openpangu/longcat 系）按推理模型处理。
func isReasoningModel(model string) bool {
	return !strings.HasPrefix(strings.ToLower(model), "qwen3")
}

// chatStream 发对话请求：恒 stream:true，按模型改写 body。
func (c *Client) chatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	ac := accountFromAuth(a)
	if ac.BaseURL == "" {
		return nil, 0, []byte("officeace: missing base_url"), fmt.Errorf("officeace: missing base_url")
	}
	out := rewriteBody(body)
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

// rewriteBody 就地改写出站 body：
//  1. 强制 stream=true；
//  2. glm-5.3 剥离不认的参数（thinking/enable_thinking/reasoning_effort）；
//  3. 注入 stream_options.include_usage（**在剥离之后**，保证 glm-5.3 也带上 usage）；
//  4. 推理模型补足 max_tokens 推理预算（非推理模型不动）；
//  5. temperature/top_p 夹取。
func rewriteBody(body []byte) []byte {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	model, _ := m["model"].(string)

	// 1. 恒 stream
	m["stream"] = true

	// 2. glm-5.3 剥离不认参数（先剥）
	if glm53NoThinking[model] {
		for _, k := range stripWhenUnsupported {
			delete(m, k)
		}
	}

	// 3. 后注入 include_usage（不被上一步剥掉）
	if _, ok := m["stream_options"]; !ok {
		m["stream_options"] = map[string]any{"include_usage": true}
	}

	// 4. 推理模型补足 max_tokens 预算（非推理模型保持用户原值；<16 原样透传）
	if v, ok := m["max_tokens"].(float64); ok && v >= tinyMaxTokens && isReasoningModel(model) {
		ceiling := float64(maxTokensCeil)
		n := v + float64(defaultMinMax)
		if n > ceiling {
			n = ceiling
		}
		m["max_tokens"] = n
	}

	// 5. 夹取
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
		// 上游 /models 只回 id，无任何能力声明 ⇒ 能力字段一律保持零值（未知），
		// 不得编造 SupportsTools/SupportsReasoning（R49 / ContextFromAPI 哲学：
		// 上游声明了才透出，未声明不编造）。
		out = append(out, provider.ModelInfo{
			ID:   id,
			Name: id,
		})
	}
	return out, nil
}
