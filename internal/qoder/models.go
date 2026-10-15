// models.go 动态模型获取：COSY 签名 GET /algo/api/v2/model/list?Encode=1，
// 拿 chat scene 的 key 列表 → provider.ModelInfo。
// 移植自 qoderwork2api internal/upstream/models.go。
package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
	"wild-work/internal/reasoning"
)

// DynamicModel 上游模型条目（chat/assistant/developer 场景同构）。
type DynamicModel struct {
	Key            string  `json:"key"`
	DisplayName    string  `json:"display_name"`
	Enable         bool    `json:"enable"`
	IsDefault      bool    `json:"is_default"`
	IsReasoning    bool    `json:"is_reasoning"`
	IsVL           bool    `json:"is_vl"`
	MaxInputTokens int64   `json:"max_input_tokens"`
	PriceFactor    float64 `json:"price_factor"`
	// MaxOutputTokens 输出上限（桌面端把它写进 parameters.max_tokens）。
	// 目录缺该字段时按 defaultMaxOutputTokens 兜底。
	MaxOutputTokens int64 `json:"max_output_tokens"`
	// Format 上游声明的协议形态；Source 上游声明来源（model_config.source 即思考总开关）。
	Format string `json:"format"`
	Source string `json:"source"`
	// ContextWindow context_config 默认档（is_default）的 token_count；
	// AvailableWindows 全部档位（升序）。两者是 json:"-"，由 parseSceneModels 解析后回填。
	ContextWindow    int64   `json:"-"`
	AvailableWindows []int64 `json:"-"`
	// ContextConfig 上下文窗口档位表：
	//   {"200K":{"token_count":200000,"is_default":true},"400K":{"token_count":400000}}
	// 桌面端把标了 is_default 的那档写进 parameters.context_length。
	ContextConfig json.RawMessage `json:"context_config"`
	// ThinkingConfig 上游声明的思考能力（档位 ladder / 是否可关闭）。
	// 实测形状（2026-09-19，qwen3.8-max / deepseek-v4-pro / glm-5.3 等）：
	//   {"disabled":{"description":"Disable thinking"},
	//    "enabled":{"description":"...","is_default":true,
	//               "efforts":{"low":{},"medium":{"is_default":true},"xhigh":{}}}}
	// 注意 efforts 是**对象**（键即档位名，值里可能带 is_default/description），
	// 不是数组；且部分模型只有 enabled 没有 disabled（不可显式关闭），
	// 也有 is_reasoning=false 却带 ladder 的（开关与档位能力不绑定）。
	ThinkingConfig json.RawMessage `json:"thinking_config"`
}

// thinkCaps 从 thinking_config 提取出的思考能力。
type thinkCaps struct {
	// Efforts 支持的档位（按强度升序）。
	Efforts []string
	// DefaultEffort 上游标了 is_default 的档位（可能为空）。
	DefaultEffort string
	// SupportsDisable 存在 disabled 节点 → 支持显式关闭思考。
	SupportsDisable bool
}

// parseThinkingConfig 解析 thinking_config。缺失/非法一律返回零值（未知），
// 调用方据此「不暴露档位、不降级」，而不是猜一条 ladder。
func parseThinkingConfig(raw json.RawMessage) thinkCaps {
	if len(raw) == 0 || string(raw) == "null" {
		return thinkCaps{}
	}
	var tc struct {
		Disabled json.RawMessage `json:"disabled"`
		Enabled  struct {
			IsDefault bool                       `json:"is_default"`
			Efforts   map[string]json.RawMessage `json:"efforts"`
		} `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return thinkCaps{}
	}
	caps := thinkCaps{
		SupportsDisable: len(tc.Disabled) > 0 && string(tc.Disabled) != "null",
	}
	for name, meta := range tc.Enabled.Efforts {
		caps.Efforts = append(caps.Efforts, name)
		if caps.DefaultEffort != "" {
			continue
		}
		var m struct {
			IsDefault bool `json:"is_default"`
		}
		if err := json.Unmarshal(meta, &m); err == nil && m.IsDefault {
			caps.DefaultEffort = name
		}
	}
	caps.Efforts = reasoning.SortEfforts(caps.Efforts)
	if !containsEffort(caps.Efforts, caps.DefaultEffort) {
		caps.DefaultEffort = "" // 默认档必须落在支持的档位里
	}
	return caps
}

// parseDefaultContextWindow 从 context_config 取标了 is_default 的窗口大小。
// 没有 is_default 标记时返回 0（宁可不下发 context_length，也不猜一档）。
func parseDefaultContextWindow(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var cfg map[string]struct {
		TokenCount int64 `json:"token_count"`
		IsDefault  bool  `json:"is_default"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return 0
	}
	// 多个档同时标默认时取最小值：确定性 + 保守，不依赖 map 迭代序。
	var best int64
	for _, v := range cfg {
		if !v.IsDefault || v.TokenCount <= 0 {
			continue
		}
		if best == 0 || v.TokenCount < best {
			best = v.TokenCount
		}
	}
	return best
}

// parseContextOptions 从 context_config 取全部可选窗口档位，升序去重。
// 形状：{"1M":{"token_count":1000000},"200K":{"token_count":200000,"is_default":true},...}
func parseContextOptions(raw json.RawMessage) []int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var cfg map[string]struct {
		TokenCount int64 `json:"token_count"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil
	}
	seen := make(map[int64]bool, len(cfg))
	out := make([]int64, 0, len(cfg))
	for _, v := range cfg {
		if v.TokenCount > 0 && !seen[v.TokenCount] {
			seen[v.TokenCount] = true
			out = append(out, v.TokenCount)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// containsEffort 档位成员判定。
func containsEffort(efforts []string, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return false
	}
	for _, e := range efforts {
		if strings.ToLower(strings.TrimSpace(e)) == want {
			return true
		}
	}
	return false
}

// fetchModels 调上游动态模型接口。
// GET 无 body，签名用空串 ""（非 "{}"，后者 403 Signature invalid）。
func (c *Client) fetchModels(a *auth.Auth) ([]DynamicModel, error) {
	dt := a.JWT()
	if dt == "" {
		return nil, fmt.Errorf("no dt- available")
	}
	rawURL := c.gatewayBase() + EpModels
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	sess, err := NewCosySession(a.MachineID, a.MachineToken, a.MachineType, a.Nickname, a.UID, dt, a.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, "", rawURL, a.UID, false, ""); err != nil {
		return nil, fmt.Errorf("cosy headers: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var apiResp map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	return parseSceneModels(apiResp)
}

// parseSceneModels 解析模型列表响应：assistant→developer→chat 三级回退。
// 上游把模型挪场景时（如迁到 assistant）不至于硬失败（上游 issue #27 同批改动）。
func parseSceneModels(apiResp map[string]json.RawMessage) ([]DynamicModel, error) {
	for _, scene := range []string{"assistant", "developer", "chat"} {
		raw, ok := apiResp[scene]
		if !ok {
			continue
		}
		var models []DynamicModel
		if err := json.Unmarshal(raw, &models); err != nil {
			continue
		}
		enabled := make([]DynamicModel, 0, len(models))
		for _, m := range models {
			if !m.Enable || m.Key == "" {
				continue
			}
			// context_config 解析后回填（两个字段是 json:"-"，不解析永远是零值）
			m.ContextWindow = parseDefaultContextWindow(m.ContextConfig)
			m.AvailableWindows = parseContextOptions(m.ContextConfig)
			enabled = append(enabled, m)
		}
		if len(enabled) > 0 {
			return enabled, nil
		}
	}
	return nil, fmt.Errorf("no enabled models in assistant/developer/chat scenes")
}

// FetchModels 实现 provider.Upstream：动态模型 → provider.ModelInfo。
// 客户端名 = display_name 规范化（无 display_name 用 key 兜底）。
// 同时把 客户端名→key 映射与 key→条目表缓存到 Client，供 ChatStream 路由与取 format/source。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	dyn, err := c.fetchModels(a)
	if err != nil {
		return nil, err
	}
	mm := make(map[string]string, len(dyn))
	metas := make(map[string]modelMeta, len(dyn))
	out := make([]provider.ModelInfo, 0, len(dyn))
	for _, m := range dyn {
		name := NormalizeModelName(m.DisplayName)
		if name == "" {
			name = m.Key
		}
		mm[name] = m.Key
		opts := parseContextOptions(m.ContextConfig)
		metas[m.Key] = modelMeta{
			Key:                  m.Key,
			DisplayName:          m.DisplayName,
			IsVL:                 m.IsVL,
			MaxInputTokens:       m.MaxInputTokens,
			MaxOutputTokens:      m.MaxOutputTokens,
			Format:               m.Format,
			Source:               m.Source,
			DefaultContextWindow: parseDefaultContextWindow(m.ContextConfig),
			ContextOptions:       opts,
		}
		caps := parseThinkingConfig(m.ThinkingConfig)
		mi := provider.ModelInfo{
			ID:             name,
			Name:           m.DisplayName,
			ContextOptions: opts,
			// is_vl 即上游的视觉能力声明；is_reasoning 为思考模式。
			SupportsImages:    m.IsVL,
			SupportsReasoning: m.IsReasoning,
		}
		// 档位能力只在上游明确声明时透出：本地不猜 Qoder 的 ladder
		// （各模型 ladder 不同，猜错会发非法档位）。
		if len(caps.Efforts) > 0 {
			mi.ReasoningEfforts = provider.CleanEfforts(caps.Efforts)
			mi.SupportedEfforts = caps.Efforts
			mi.DefaultReasoningEffort = caps.DefaultEffort
			mi.DefaultEffort = caps.DefaultEffort
		}
		mi.ReasoningCanDisable = caps.SupportsDisable
		// 上下文窗口对外展示的是上游允许的最大档位：max_input_tokens 是单次输入
		// 上限（目录里多为 180000），与可选的上下文窗口档位不是同一个量。
		switch {
		case len(opts) > 0:
			mi.ContextWindow = opts[len(opts)-1]
			mi.ContextFromAPI = true
		case m.MaxInputTokens > 0:
			mi.ContextWindow = m.MaxInputTokens
			mi.ContextFromAPI = true
		}
		out = append(out, mi)
	}
	c.setModelMap(mm, metas)
	for _, m := range dyn {
		nm := NormalizeModelName(m.DisplayName)
		if strings.Contains(m.DisplayName, "3.8") || strings.Contains(strings.ToLower(m.DisplayName), "flash") {
			log.Printf("qoder catalog: client=%q key=%q display=%q", nm, m.Key, m.DisplayName)
		}
	}
	if mm["qwen3.8-flash"] == "" {
		log.Printf("qoder catalog: qwen3.8-flash NOT in dynamic modelMap")
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// FetchModelPricing 实现 provider.Upstream：Qoder 模型带 price_factor，
// 直接复用 FetchModels 的倍率字段。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	dyn, err := c.fetchModels(a)
	if err != nil {
		return nil, err
	}
	out := make([]provider.ModelPricing, 0, len(dyn))
	for _, m := range dyn {
		name := NormalizeModelName(m.DisplayName)
		if name == "" {
			name = m.Key
		}
		out = append(out, provider.ModelPricing{
			Model:   name,
			Channel: "qoder",
			Rate:    m.PriceFactor,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("pricing api returned empty models")
	}
	return out, nil
}

// NormalizeModelName 把 display_name 转成 OpenAI 风格客户端名：
// 小写、空格/下划线转连字符、保留点号（版本号）、去重连字符。
// "Qwen3.8-Max" → "qwen3.8-max"；"GLM-5.3" → "glm-5.3"。
func NormalizeModelName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	b.Grow(len(s))
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.':
			b.WriteRune(r)
			prevDash = false
		case r == ' ' || r == '_' || r == '-':
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		default:
			b.WriteRune(r)
			prevDash = false
		}
	}
	return strings.Trim(b.String(), "-")
}
