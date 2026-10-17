// models.go 动态模型获取：COSY 签名 GET /algo/api/v2/model/list?Encode=1。
// 代码级复制自 internal/qoder/models.go 后改造：
//   - 无静态兜底表；缓存策略为「上次成功拉取的动态表」（内存态，进程内保留）
//   - 解析 qoder2api 的完整字段：is_default + context_config.token_count
//   - 场景解析 assistant → developer → chat 三级回退（qoder2api parseQoderModels）
package qodercn

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
	"wild-work/internal/reasoning"
)

// contextConfig 形状：{"<label>": {"is_default":bool,"token_count":int}}
type contextConfig map[string]struct {
	IsDefault  bool  `json:"is_default"`
	TokenCount int64 `json:"token_count"`
}

// tokenCount 取上游声明的上下文窗口：**只认标记了 is_default 的那一档**。
// 上游实测总是恰好标一档默认（COM 77/77、CN 104/104），未标默认时不猜——
// 返回 0 让上层回退 max_input_tokens（保守方向），避免误取 1M 这类最大档。
// 多个档同时标默认时取最小值（确定性 + 保守），不依赖 map 迭代序。
func (cc contextConfig) tokenCount() int64 {
	var best int64
	for _, cfg := range cc {
		if !cfg.IsDefault || cfg.TokenCount <= 0 {
			continue
		}
		if best == 0 || cfg.TokenCount < best {
			best = cfg.TokenCount
		}
	}
	return best
}

// windows 返回 context_config 全部档位（升序、去重、>0），供选档校验。
func (cc contextConfig) windows() []int64 {
	seen := make(map[int64]struct{}, len(cc))
	out := make([]int64, 0, len(cc))
	for _, cfg := range cc {
		if cfg.TokenCount <= 0 {
			continue
		}
		if _, ok := seen[cfg.TokenCount]; ok {
			continue
		}
		seen[cfg.TokenCount] = struct{}{}
		out = append(out, cfg.TokenCount)
	}
	// 插入排序（档位数 ≤4，无需 sort 包）
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// modelWire 上游模型表条目：在 ModelEntry 之外带上 context_config。
// ModelEntry.ContextWindow 是 json:"-"（上游不直接下发该字段名），只能由此处解析后回填。
type modelWire struct {
	ModelEntry
	ContextConfig json.RawMessage `json:"context_config"`
}

// parseContextConfig 解析 context_config → (默认档, 全部档位)。
// 用 RawMessage 承接：形状不符时只损失本字段，不会让整批模型解析失败。
func (w modelWire) parseContextConfig() (int64, []int64) {
	if len(w.ContextConfig) == 0 {
		return 0, nil
	}
	var cc contextConfig
	if err := json.Unmarshal(w.ContextConfig, &cc); err != nil {
		return 0, nil
	}
	return cc.tokenCount(), cc.windows()
}

// parseDynamicModels 解析模型列表响应：assistant→developer→chat 三级回退。
func parseDynamicModels(apiResp map[string]json.RawMessage) ([]ModelEntry, error) {
	for _, scene := range []string{"assistant", "developer", "chat"} {
		raw, ok := apiResp[scene]
		if !ok {
			continue
		}
		var wires []modelWire
		if err := json.Unmarshal(raw, &wires); err != nil {
			continue
		}
		enabled := make([]ModelEntry, 0, len(wires))
		for _, w := range wires {
			if !w.Enable || w.Key == "" {
				continue
			}
			m := w.ModelEntry
			m.ContextWindow, m.AvailableWindows = w.parseContextConfig() // 默认档 + 全档位
			enabled = append(enabled, m)
		}
		if len(enabled) > 0 {
			return enabled, nil
		}
	}
	return nil, fmt.Errorf("no enabled models in any scene")
}

// fetchModels 调上游动态模型接口。
// GET 无 body，签名用空串 ""（非 "{}"，后者 403 Signature invalid）。
func (c *Client) fetchModels(a *auth.Auth) ([]ModelEntry, error) {
	dt := a.JWT()
	if dt == "" {
		return nil, fmt.Errorf("no dt- available")
	}
	rawURL := c.Gateway + EpModels
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	ut := c.userTypeOf(a)
	sess, err := NewCosySession(a.MachineID, a.MachineToken, a.MachineType, a.Nickname, a.UID, dt, a.RefreshToken, ut)
	if err != nil {
		return nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, "", rawURL, a.UID, "application/json", false, ""); err != nil {
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
	enabled, err := parseDynamicModels(apiResp)
	if err != nil {
		return nil, err
	}
	c.setCache(enabled) // 上次成功即缓存（无静态兜底）
	return enabled, nil
}

// ---------------------------------------------------------------------------
// 上次成功缓存（进程内存态；无静态兜底表）
// ---------------------------------------------------------------------------

// cacheMu 保护 cache；cache 为最近一次成功拉取的模型表。
var cacheMu sync.RWMutex

// setCache 记录最近一次成功拉取，并同步重建 客户端名→key 映射。
func (c *Client) setCache(m []ModelEntry) {
	mm := make(map[string]string, len(m))
	for _, e := range m {
		name := NormalizeModelName(e.DisplayName)
		if name == "" {
			name = e.Key
		}
		mm[name] = e.Key
	}
	c.modelMu.Lock()
	c.modelMap = mm
	c.cache = m
	c.modelMu.Unlock()
}

// cachedModels 返回上次成功拉取的模型表快照；无缓存返回 nil。
func (c *Client) cachedModels() []ModelEntry {
	c.modelMu.RLock()
	defer c.modelMu.RUnlock()
	if len(c.cache) == 0 {
		return nil
	}
	out := make([]ModelEntry, len(c.cache))
	copy(out, c.cache)
	return out
}

// FetchModels 实现 provider.Upstream：动态模型 → provider.ModelInfo。
// 客户端名 = display_name 规范化（无 display_name 用 key 兜底）。
// 失败时回退上次成功缓存；连缓存都无 → 返回错误（无静态兜底，按 Fail Early）。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	dyn, err := c.fetchModels(a)
	if err != nil {
		if cached := c.cachedModels(); len(cached) > 0 {
			log.Printf("qodercn models fetch failed (%v), using last-success cache %d models", err, len(cached))
			return toModelInfos(cached), nil
		}
		return nil, err
	}
	return toModelInfos(dyn), nil
}

// toModelInfos 动态表 → provider.ModelInfo（含 context_config 解析）。
// 旧注释声称的「max_output 推导」系遗留失实：上游模型接口从不下发输出上限字段
// （2026-10-13 COSY 实测 9 场景全部条目仅有 max_input_tokens，无任何 max_output 类键），
// 故 MaxTokens 恒为 0——这是「不编造」原则下的正确行为，非缺陷。
func toModelInfos(dyn []ModelEntry) []provider.ModelInfo {
	mm := make(map[string]string, len(dyn))
	out := make([]provider.ModelInfo, 0, len(dyn))
	for _, m := range dyn {
		name := NormalizeModelName(m.DisplayName)
		if name == "" {
			name = m.Key
		}
		mm[name] = m.Key
		mi := provider.ModelInfo{
			ID:            name,
			Name:          m.DisplayName,
			ContextWindow: 180000,
			// is_vl 即上游的视觉能力声明；is_reasoning 为思考模式。
			SupportsImages:    m.IsVL,
			SupportsReasoning: m.IsReasoning,
		}
		// 档位能力只在上游明确声明时透出：本地不猜 Qoder 的 ladder
		// （各模型 ladder 不同，猜错会发非法档位）。能力进入 reasoning.Caps
		// 的 RealmQoderCN 面（见 server.publishEffortCaps），与 Qoder / QoderCOM 互不干扰。
		caps := reasoning.ParseThinkingConfig(m.ThinkingConfig)
		if len(caps.Efforts) > 0 {
			mi.ReasoningEfforts = provider.CleanEfforts(caps.Efforts)
			mi.SupportedEfforts = caps.Efforts
			mi.DefaultReasoningEffort = caps.DefaultEffort
			mi.DefaultEffort = caps.DefaultEffort
		}
		mi.ReasoningCanDisable = caps.SupportsDisable
		// 上下文窗口：广告宁高勿低——max(档位表) > is_default 档 > max_input_tokens
		// （与 resolveContextWindow 默认档一致，对齐 workbuddy2api 口径）
		if n := len(m.AvailableWindows); n > 0 {
			mi.ContextWindow = m.AvailableWindows[n-1]
			mi.ContextFromAPI = true
		} else if m.ContextWindow > 0 {
			mi.ContextWindow = m.ContextWindow
			mi.ContextFromAPI = true
		} else if m.MaxInputTokens > 0 {
			mi.ContextWindow = m.MaxInputTokens
			mi.ContextFromAPI = true
		}
		out = append(out, mi)
	}
	return out
}

// FetchModelPricing 实现 provider.Upstream：Qoder 模型带 price_factor，
// 直接复用 FetchModels 的倍率字段；失败回退上次成功缓存。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	dyn, err := c.fetchModels(a)
	if err != nil {
		if cached := c.cachedModels(); len(cached) > 0 {
			log.Printf("qodercn pricing fetch failed (%v), using last-success cache", err)
			return toPricings(cached), nil
		}
		return nil, err
	}
	return toPricings(dyn), nil
}

func toPricings(dyn []ModelEntry) []provider.ModelPricing {
	out := make([]provider.ModelPricing, 0, len(dyn))
	for _, m := range dyn {
		name := NormalizeModelName(m.DisplayName)
		if name == "" {
			name = m.Key
		}
		out = append(out, provider.ModelPricing{
			Model:   name,
			Channel: "qodercn",
			Rate:    m.PriceFactor,
		})
	}
	return out
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
