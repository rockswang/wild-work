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
	"net/http"
	"os"
	"path/filepath"
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

// 为什么**没有**静态 benefit 名单（2026-10-26 删）：
// `maas_type` 是**账号级通道权益**，不是模型属性。原 staticBenefit =
// {"glm-5.3-flash","deepseek-v4.1-flash"} 把两个模型硬标成 benefit，生产实测两个都撞
// 4004.200；A/B 证明 deepseek-v4.1-flash **不带这个头就正常出词**（`docs/决议项.md` R143）。
// 而 GET /v1/model/builtin 实测 9 条里 deepseek-v4.1-flash 就在其中，按官方
// isCurrentModelBenefitType 的语义（builtin 命中恒 false）它根本不该发。故一律以上游目录为准。

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
	at     time.Time
	models []provider.ModelInfo
	// maas 模型 id → 该模型应带的 maas_type（**channel 名**，如 "examination"）。
	// 只收 gatewayConfig 明确列出、且不在 builtin 目录里的模型。空 map = 本账号没有此类模型。
	maas map[string]string
	// builtin GET /v1/model/builtin 的模型 id 集合。官方语义：命中即恒不发 maas_type。
	builtin map[string]bool
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

// storeCatalog 写入目录缓存（models / maas 映射 / builtin 集合）。
func (c *Client) storeCatalog(models []provider.ModelInfo, maas map[string]string, builtin map[string]bool) {
	c.catMu.Lock()
	defer c.catMu.Unlock()
	c.cat = &catalogCache{at: time.Now(), models: models, maas: maas, builtin: builtin}
}

// maasTypeFor 返回该模型出站应带的 maas_type 值；空串 = **不发该头**。
//
// 判据严格照官方 extension.js 的 isCurrentModelBenefitType（2026-10-26 实读 .deb 26.10.0）：
//   - 在 builtin 目录里 ⇒ 空（builtin 优先；实测 deepseek-v4.1-flash 就在 builtin，
//     带 maas_type 回 4004.200、不带就正常出词）；
//   - 只出现在 gatewayConfig ⇒ 发该 channel 值（benefit/education/examination）；
//   - 两份目录都没有 ⇒ 空（**不猜**，这是原 staticBenefit 的死因）。
//
// ⚠️ 每次对话前都会调用 ⇒ 绝不发网络请求，只用进程内缓存。
func (c *Client) maasTypeFor(model string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		return ""
	}
	cat := c.cachedCatalog()
	if cat == nil {
		return ""
	}
	if cat.builtin[m] {
		return ""
	}
	return cat.maas[m]
}

// maasModels 返回当前生效的 maas 映射（供诊断/测试）。
func (c *Client) maasModels() map[string]string {
	if cat := c.cachedCatalog(); cat != nil && cat.maas != nil {
		return cat.maas
	}
	return map[string]string{}
}

// ---------------------------------------------------------------------------
// 远端目录
// ---------------------------------------------------------------------------

// builtinEnvelope `GET {Base}/v1/model/builtin` 的响应：取 `builtinModels[]`。
type builtinEnvelope struct {
	BuiltinModels []struct {
		ModelID   string `json:"model_id"`
		ModelName string `json:"model_name"`
	} `json:"builtinModels"`
}

// FetchModels 拉取并合并远端目录；任何失败都回退静态兜底且**不返回错误**
// （目录拿不到不该让整条渠道在面板里消失）。
//
// 只拉 builtin 一个目录：这是官方 extension.js 的 getModelBuiltinList，而它同时是
// `maas_type` 判据的第一层（builtin 命中恒不发，R143）。上游另一条
// `GET {BenefitBase}/api/v1/gateway/config?channel=`（官方 s4c()）对本账号三组全返回
// group not found / enabled=false，按 YAGNI 不实现，故 maas 映射始终为空。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	if cat := c.cachedCatalog(); cat != nil && len(cat.models) > 0 {
		return cat.models, nil
	}
	maas := map[string]string{}
	builtin := map[string]bool{}
	var dynamic []modelEntry

	// builtin 组（需要签名；无凭据时跳过）。
	if ac := accountFromAuthOrNil(a); ac != nil && ac.AccessKeyID != "" && ac.SecretAccessKey != "" {
		if entries, err := c.fetchBuiltinCatalog(ac); err == nil {
			for _, e := range entries {
				id := normalizeModelID(e.ID)
				builtin[id] = true // 官方语义：builtin 命中的模型恒不发 maas_type
				dynamic = append(dynamic, modelEntry{ID: id, Name: e.Name})
			}
		}
	}
	models := c.filterUsable(mergeCatalog(dynamic, staticCatalog))
	if len(models) == 0 {
		// 理论上不会走到（静态表非空），防御性兜底。
		return c.staticModels(), nil
	}
	c.storeCatalog(models, maas, builtin)
	return models, nil
}

// accountFromAuthOrNil 安全取账号（nil / 空 Auth 时返回 nil）。
func accountFromAuthOrNil(a *auth.Auth) *Account {
	if a == nil {
		return nil
	}
	return accountFromAuth(a)
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

// ---------------------------------------------------------------------------
// 不可用模型摘除窗口（R143）
// ---------------------------------------------------------------------------

// reUnusableModel 上游明确声明「本账号用不了这个模型」的两类报文（均实测 2026-10-26）：
//
//	81009.404 Invalid model            —— deepseek-v4-pro（目录里有、账号无权限）
//	002002009 model is not registered  —— glm-5.3-flash（不在 builtin、也没有任何通道权益）
//
// 为什么在目录层处理：它们只被 stream 路径记 `stream error ignored`，不罚号，于是面板照样列出、
// 客户端点了必错（用户 2026-10-26 决定隐藏）。
var reUnusableModel = regexp.MustCompile(`(?i)81009\.404|002002009|invalid model|model is not registered`)

// 为什么不再有 6h 窗口（原设计，2026-10-26 用户决定改永久）：81009.404 是「该模型对本账号不存在」、
// 002002009 是「该模型没注册」，都不是临时限流，等多久都不会好；窗口语义的唯一效果是每 6 小时
// 向用户重新暴露一次必错选项。持久化存 state 文件、重登清空。

// unusableState `state-codearts.json` 的模型摘除段，键名 `unusable_models`。
//
// 存在这个键（而不是塞进 pool 的 accountState）：pool 的 `unusable` 字段语义是
// **用不了的积分**（面板展示用），与本处的「模型摘除」完全不同源，混用会互相污染。
type unusableState struct {
	// Models 模型名 → 摘除时刻（**仅诊断用，不参与过期判断**）。
	Models map[string]time.Time `json:"unusable_models,omitempty"`
}

// bareModelName 去掉模型 ID 里的渠道前缀（如 `huawei/deepseek-v4.1-flash` → `deepseek-v4.1-flash`），
// 与模型目录/摘除记录里的 ID 口径一致，否则摘除记录永远命中不了目录条目。
func bareModelName(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		return model[i+1:]
	}
	return model
}

// markUnusable 记录「本账号调不通这个模型」并落盘。
// d<=0 表示永久（当前唯一用法）；保留参数是为了不把调用点写死成一种语义。
func (c *Client) markUnusable(model string, d time.Duration) {
	m := strings.TrimSpace(model)
	if m == "" {
		return
	}
	c.capMu.Lock()
	if c.unusable == nil {
		c.unusable = map[string]time.Time{}
	}
	c.unusable[m] = time.Now().Add(d)
	state := unusableState{Models: make(map[string]time.Time, len(c.unusable))}
	for k, t := range c.unusable {
		if d > 0 && time.Now().After(t) {
			continue // 窗口语义下只落未过期的（d<=0 全落）
		}
		state.Models[k] = t
	}
	c.capMu.Unlock()
	c.saveUnusable(state)
	// 让目录缓存失效，下次 FetchModels 会重新 filterUsable（立即生效）。
	c.catMu.Lock()
	c.cat = nil
	c.catMu.Unlock()
}

// isUnusable 报告该模型是否被摘除。永久语义：**没有任何过期判断**。
func (c *Client) isUnusable(model string) bool {
	m := strings.TrimSpace(model)
	if m == "" {
		return false
	}
	c.capMu.RLock()
	_, ok := c.unusable[m]
	c.capMu.RUnlock()
	if ok {
		return true
	}
	// 首次访问时从 state 文件懒加载（进程重启后恢复）。
	c.loadUnusable()
	c.capMu.RLock()
	defer c.capMu.RUnlock()
	_, ok = c.unusable[m]
	return ok
}

// loadUnusable 从 UnusableFile 懒加载摘除记录。任何读错都静默跳过：
// 状态文件坏了不该让整条渠道不可用。
func (c *Client) loadUnusable() {
	if c.UnusableFile == "" {
		return
	}
	c.capMu.Lock()
	defer c.capMu.Unlock()
	if c.unusableLoaded {
		return
	}
	c.unusableLoaded = true
	if c.unusable == nil {
		c.unusable = map[string]time.Time{}
	}
	raw, err := os.ReadFile(c.UnusableFile)
	if err != nil {
		return
	}
	// 独占文件，结构就是 {"unusable_models": {"模型名": 时刻}}（saveUnusable 直接落的
	// unusableState）；与写入端同一形状，不再有"写入加一层外壳"的错配。
	var state unusableState
	if json.Unmarshal(raw, &state) != nil {
		return
	}
	for k, t := range state.Models {
		if k = strings.TrimSpace(k); k != "" {
			c.unusable[k] = t
		}
	}
}

// clearUnusable 清空全部摘除记录并落盘。重新登录成功后调用（用户 2026-10-26 决定：
// 不做面板入口，重登即重置）。
func (c *Client) ClearUnusable() {
	c.capMu.Lock()
	c.unusable = map[string]time.Time{}
	c.unusableLoaded = true
	c.capMu.Unlock()
	c.saveUnusable(unusableState{})
	c.catMu.Lock()
	c.cat = nil
	c.catMu.Unlock()
}

// saveUnusable 把摘除记录写入 UnusableFile（**独占文件**，无需保留其他键）。
//
// 为什么独立文件而不是共用的 state 文件：state-codearts.json 由 pool.saveLocked
// 整文件覆写，它不认识额外键 ⇒ 一次积分刷新就会把记录抹掉（线上实测 2026-10-26）。
// 各写各的文件后，本函数只需关心自己的数据。
func (c *Client) saveUnusable(state unusableState) {
	if c.UnusableFile == "" {
		return
	}
	out, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(c.UnusableFile), 0o700); err != nil {
		return
	}
	// tmp + rename 原子替换：写到一半被杀不会留下半个 JSON。
	tmp := c.UnusableFile + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, c.UnusableFile)
}

// FilterUnusable 供装配层把摘除过滤挂到 server 的出口（Runtime.ModelFilter）。
//
// 为什么还要这一层：本包的目录过滤只管自己那层缓存，而 server 的 rt.models 另有
// 1h 缓存（dynamicModelsTTL）。渠道刚摘除的模型会被那层缓存继续列出 1 小时
// （线上实测 2026-10-26：记录已落盘，/v1/models 仍显示）。挂到出口后两条缓存都绕不过。
func (c *Client) FilterUnusable(models []provider.ModelInfo) []provider.ModelInfo {
	return c.filterUsable(models)
}

// filterUsable 摘掉处在窗口内的模型。
//
// 必须在**目录层**过滤，不能放到 server 层：`internal/server/handler.go` 的 modelList()
// 在 fetchRuntimeModels 返回空时会回退 Runtime.StaticModels，而 staticCatalog 里就有
// deepseek-v4-pro / glm-5.3-flash，server 层过滤会被静态兜底再塞回来。
func (c *Client) filterUsable(models []provider.ModelInfo) []provider.ModelInfo {
	out := models[:0]
	for _, m := range models {
		if c.isUnusable(m.ID) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// staticModels 本 Client 的静态兜底（已按摘除窗口过滤，保证 server 回退时同样不列出不可用模型）。
func (c *Client) staticModels() []provider.ModelInfo {
	return c.filterUsable(modelInfos(staticCatalog))
}
