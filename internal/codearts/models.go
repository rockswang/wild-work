// models.go 模型目录：静态兜底表 + 两个远端目录 + benefit（免费额度）集合（协议 §2.5）。
//
// 三条协议事实决定了本文件的形状：
//
//  1. **静态兜底表**（§2.5(a)）**必须**存在：目录拉取失败/无凭据时面板仍要有列表
//     （拉不到目录不该让整条渠道消失，与 R42「静态表 ∪ 动态目录」同向）；
//  2. **benefit 集合决定请求是否必须带 `maas_type: benefit`**（§2.5(c)）：不带 → 上游回
//     `InferHub.002002009.404 model is not registered`；非 benefit 模型带上 → `unsupported model`。
//     集合只能记**归一化未改写**的 id：带日期后缀的 `deepseek-v4-flash-0731` 与无后缀的
//     `deepseek-v4-flash` 在后端是**不同模型、benefit 属性相反**，故带后缀的条目不入集合；
//  3. id 归一化（去末尾 `-NNNN` 日期后缀）与视觉模型过滤（`-VL-`/`-VL` 结尾）是**目录展示**
//     规则，与 benefit 集合的「原样记录」规则刻意分叉（同上）。
package codearts

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// modelEntry 归一化后的模型条目（静态兜底与远端目录共用同一形状）。
type modelEntry struct {
	// ID 发给上游的模型名（协议 §2.3：大小写敏感、无统一前缀）。
	ID string
	// Name 展示名（上游 model_name，缺失回退 id）。
	Name string
	// ContextWindow 上下文窗口；**0 = 未知**（消费方必须判 > 0）。
	ContextWindow int64
}

// staticCatalog 静态兜底表（协议 §2.5(a)，顺序照抄）。
//
// ContextWindow 只填协议明确给出的 5 条（参考实现的静态表实测值），其余保持 0=未知
// ——「上游没给就不编容量」与 ContextFromAPI 哲学一致。
var staticCatalog = []modelEntry{
	{ID: "GLM-5.2", ContextWindow: 202752},
	{ID: "GLM-5.1"},
	{ID: "GLM-5"},
	{ID: "glm-5.3-flash", ContextWindow: 1048576},
	{ID: "openpangu-2.0-flash"},
	{ID: "openpangu-2.0-pro"},
	{ID: "deepseek-v4-flash", ContextWindow: 1048576},
	{ID: "deepseek-v4-pro", ContextWindow: 1048576},
	{ID: "deepseek-v4.1-flash", ContextWindow: 1000000},
}

// staticBenefit benefit 集合的静态兜底（协议 §2.5(c)）。
//
// 为什么不能只靠它：远端目录会新增 benefit 模型（历史教训：硬编码 glm-5.3-flash
// 会漏掉 deepseek-v4.1-flash）。动态拉取成功时以远端为准。
var staticBenefit = []string{"glm-5.3-flash", "deepseek-v4.1-flash"}

// StaticModels 静态模型表（装配时注入 Runtime.StaticModels；无凭据时面板也有列表）。
func StaticModels() []provider.ModelInfo { return modelInfos(staticCatalog) }

// modelInfos 把内部条目转成 provider.ModelInfo。
func modelInfos(entries []modelEntry) []provider.ModelInfo {
	out := make([]provider.ModelInfo, 0, len(entries))
	for _, e := range entries {
		if strings.TrimSpace(e.ID) == "" {
			continue
		}
		out = append(out, provider.ModelInfo{
			ID:   e.ID,
			Name: firstNonEmpty(e.Name, e.ID),
			// ContextWindow 来自参考实现的静态表/上游声明，**不是**上游接口字段，
			// 故 ContextFromAPI 保持 false（面板据此不把它当官方容量标注）。
			ContextWindow: e.ContextWindow,
		})
	}
	return out
}

// reDateSuffix 匹配末尾的 `-NNNN` 日期后缀（如 -0731）。
var reDateSuffix = regexp.MustCompile(`-\d{4}$`)

// normalizeModelID 去掉末尾的四位日期后缀（协议 §2.5(b)）。
func normalizeModelID(id string) string { return reDateSuffix.ReplaceAllString(id, "") }

// isHiddenModel 视觉模型从目录隐藏（协议 §2.5(b)：id 含 `-VL-` 或以 `-VL` 结尾）。
func isHiddenModel(id string) bool {
	return strings.Contains(id, "-VL-") || strings.HasSuffix(id, "-VL")
}

// catalogCache 进程内目录缓存（协议 §2.5(b)：参考实现 2 h 刷新一次；
// 本包刻意**不**落盘缓存——DSH_CODEARTS_CACHE_DIR 那套磁盘缓存只是参考实现的
// 优化，多一份可能陈旧的磁盘状态对排障是负资产）。
type catalogCache struct {
	at      time.Time
	models  []provider.ModelInfo
	benefit map[string]bool
}

// cachedCatalog 取未过期的缓存（nil = 无缓存/已过期）。
func (c *Client) cachedCatalog() *catalogCache {
	c.catMu.RLock()
	defer c.catMu.RUnlock()
	if c.cat == nil || time.Since(c.cat.at) > catalogTTL {
		return nil
	}
	return c.cat
}

// storeCatalog 写入缓存。
func (c *Client) storeCatalog(models []provider.ModelInfo, benefit map[string]bool) {
	c.catMu.Lock()
	defer c.catMu.Unlock()
	c.cat = &catalogCache{at: time.Now(), models: models, benefit: benefit}
}

// isBenefitModel 报告该模型是否属于 benefit 集合（决定是否带 maas_type 签名）。
//
// ⚠️ 本函数在**每次对话前**都会走一遍，故**绝不发网络请求**：只用进程内缓存，
// 无缓存时用静态兜底集合（远端集合要等一次 FetchModels 才更新，这是刻意的取舍——
// 宁可偶尔漏带 maas_type（上游回 4004.200，代码会去掉头重试一次），
// 也不能让对话路径挂上一个目录请求。
func (c *Client) isBenefitModel(model string) bool {
	m := strings.TrimSpace(model)
	if m == "" {
		return false
	}
	if cat := c.cachedCatalog(); cat != nil && cat.benefit != nil {
		return cat.benefit[m]
	}
	for _, b := range staticBenefit {
		if b == m {
			return true
		}
	}
	return false
}

// benefitModels 返回当前生效的 benefit 集合（供诊断/测试）。
func (c *Client) benefitModels() map[string]bool {
	if cat := c.cachedCatalog(); cat != nil && cat.benefit != nil {
		return cat.benefit
	}
	out := make(map[string]bool, len(staticBenefit))
	for _, b := range staticBenefit {
		out[b] = true
	}
	return out
}

// ---------------------------------------------------------------------------
// 远端目录
// ---------------------------------------------------------------------------

// benefitConfigEnvelope `GET {BenefitBase}/api/v1/gateway/config` 的响应：
// 取 `result.models[]`（协议 §2.5(b)）。
type benefitConfigEnvelope struct {
	Result struct {
		Models []struct {
			ModelID   string `json:"model_id"`
			ModelName string `json:"model_name"`
		} `json:"models"`
	} `json:"result"`
}

// builtinEnvelope `GET {Base}/v1/model/builtin` 的响应：取 `builtinModels[]`。
type builtinEnvelope struct {
	BuiltinModels []struct {
		ModelID   string `json:"model_id"`
		ModelName string `json:"model_name"`
	} `json:"builtinModels"`
}

// FetchModels 拉取并合并远端目录；任何失败都回退静态兜底且**不返回错误**
// （目录拿不到不该让整条渠道在面板里消失）。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	if cat := c.cachedCatalog(); cat != nil && len(cat.models) > 0 {
		return cat.models, nil
	}
	benefit := map[string]bool{}
	var dynamic []modelEntry

	// ① benefit 组（无账号也可读：这是公开的网关配置）。
	if entries, err := c.fetchBenefitCatalog(); err == nil {
		for _, e := range entries {
			// 集合只记**未改写**的 id（协议 §2.5(c)）：带日期后缀的
			// `deepseek-v4-flash-0731` 与无后缀的 `deepseek-v4-flash` 在后端是不同
			// 模型、benefit 属性相反，记错会把请求打成 4004.200 / unsupported model。
			if normalizeModelID(e.ID) == e.ID {
				benefit[e.ID] = true
			}
			dynamic = append(dynamic, modelEntry{ID: normalizeModelID(e.ID), Name: e.Name})
		}
	}
	// ② 常规组（需要签名；无凭据时跳过）。
	if ac := accountFromAuthOrNil(a); ac != nil && ac.AccessKeyID != "" && ac.SecretAccessKey != "" {
		if entries, err := c.fetchBuiltinCatalog(ac); err == nil {
			for _, e := range entries {
				dynamic = append(dynamic, modelEntry{ID: normalizeModelID(e.ID), Name: e.Name})
			}
		}
	}
	models := mergeCatalog(dynamic, staticCatalog)
	if len(models) == 0 {
		// 理论上不会走到（静态表非空），防御性兜底。
		return StaticModels(), nil
	}
	if len(benefit) == 0 {
		// 远端 benefit 目录拿不到时**回落静态兜底集合**，绝不落一个空集：
		// 空集会让 isBenefitModel 对 benefit 模型恒 false ⇒ 静默不发 maas_type ⇒
		// 上游回 `InferHub.002002009.404 model is not registered`。
		for _, b := range staticBenefit {
			benefit[b] = true
		}
	}
	c.storeCatalog(models, benefit)
	return models, nil
}

// accountFromAuthOrNil 安全取账号（nil / 空 Auth 时返回 nil）。
func accountFromAuthOrNil(a *auth.Auth) *Account {
	if a == nil {
		return nil
	}
	return accountFromAuth(a)
}

// fetchBenefitCatalog 拉 benefit 模型目录（公开端点，不签名）。
func (c *Client) fetchBenefitCatalog() ([]modelEntry, error) {
	req, err := http.NewRequest(http.MethodGet, c.benefitBase()+EpBenefitConfig, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.httClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, errStatus("benefit config", resp.StatusCode)
	}
	var env benefitConfigEnvelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&env); err != nil {
		return nil, err
	}
	out := make([]modelEntry, 0, len(env.Result.Models))
	for _, m := range env.Result.Models {
		if id := strings.TrimSpace(m.ModelID); id != "" {
			out = append(out, modelEntry{ID: id, Name: strings.TrimSpace(m.ModelName)})
		}
	}
	return out, nil
}

// fetchBuiltinCatalog 拉常规模型目录（需签名；Content-Type 按协议属于「未签名但必需的头」）。
func (c *Client) fetchBuiltinCatalog(ac *Account) ([]modelEntry, error) {
	unsigned := controlHeaders()
	unsigned["Content-Type"] = "application/json"
	status, raw, err := c.doSigned(c.httClient(), ac, http.MethodGet, c.base()+EpBuiltinModels, "", nil, nil, unsigned)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, errStatus("builtin models", status)
	}
	var env builtinEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	out := make([]modelEntry, 0, len(env.BuiltinModels))
	for _, m := range env.BuiltinModels {
		if id := strings.TrimSpace(m.ModelID); id != "" {
			out = append(out, modelEntry{ID: id, Name: strings.TrimSpace(m.ModelName)})
		}
	}
	return out, nil
}

// mergeCatalog 合并动态与静态目录：动态在前（保序去重），静态补齐缺失项；
// 视觉模型一律隐藏；同 id 冲突时保留先出现的（动态优先，因为名称更新）。
func mergeCatalog(dynamic, fallback []modelEntry) []provider.ModelInfo {
	seen := map[string]bool{}
	out := make([]provider.ModelInfo, 0, len(dynamic)+len(fallback))
	add := func(e modelEntry) {
		id := strings.TrimSpace(e.ID)
		if id == "" || seen[id] || isHiddenModel(id) {
			return
		}
		seen[id] = true
		out = append(out, provider.ModelInfo{
			ID:            id,
			Name:          firstNonEmpty(e.Name, id),
			ContextWindow: e.ContextWindow,
		})
	}
	for _, e := range dynamic {
		add(e)
	}
	for _, e := range fallback {
		if !seen[strings.TrimSpace(e.ID)] {
			add(e)
			continue
		}
		// 已存在：仅补 ContextWindow（动态目录没有这个字段）。
		if e.ContextWindow > 0 {
			for i := range out {
				if out[i].ID == e.ID && out[i].ContextWindow == 0 {
					out[i].ContextWindow = e.ContextWindow
				}
			}
		}
	}
	return out
}

// errStatus 构造不含响应体的目录错误（目录响应无凭据，但仍不把整包塞进错误文本）。
func errStatus(what string, status int) error {
	return &statusError{what: what, status: status}
}

// statusError 目录类接口的状态码错误。
type statusError struct {
	what   string
	status int
}

func (e *statusError) Error() string {
	return "codearts: " + e.what + " HTTP " + itoa(e.status)
}
