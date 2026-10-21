// quota.go 配额窗口（`retrieveUserQuotaSummary`）与账号档位 / 项目探测（`loadCodeAssist`）。
//
// 协议依据：事实文档 §2.7 与参考实现 gemini-credits.ts / gemini-project.ts。
//
// ## ⚠️ 这不是「积分余额」
//
// Gemini 免费线是**配额窗口**制（5 小时 / 周两个窗口的**剩余比例**），不是充值积分。
// 面板如实显示「5 小时窗口 99% / 周窗口 99%」，**不**换算成任何积分数字——那会是编造。
// 本仓的 provider.ResourceItem 只有 int64 数字、没有单位字段，故本包用两处如实表达单位：
//   - Name 里带 `（%）`（例如「5 小时窗口（%）」）；
//   - **InfoOnly=true**：该条目只展示，不参与任何积分算术（Summarize 小计 / 临期 / ledger 差分）。
//     否则「99 + 99 = 198」会被当作 198 积分显示出来，比不显示更误导。
//
// ## ⚠️ 三条硬约束（照参考实现，别"优化"）
//
//  1. 请求体字段名是 **`project`**，不是 `cloudaicompanionProject`
//     （后者回 400 `Unknown name "cloudaicompanionProject": Cannot find field.`）；
//     且**必须带**：原版发 `{}` 是单账号抓包结论，实测第二个账号 `{}` 被拒
//     `403 PERMISSION_DENIED SUBSCRIPTION_REQUIRED (#3501)`，同 token 同头加 `project` 即 200。
//  2. 只认 `gemini-5h` / `gemini-weekly` 两个 **bucketId**，**不按** `displayName` / `window`
//     匹配（第三组 `3p-*` 属 Claude/GPT 产品线，与本渠道无关）。
//  3. `resetTime` 解析失败 ⇒ **整条桶丢弃**：拿不到可解析的重置时间就无法表达窗口周期，
//     编一个空串会让面板显示一个**永远不过期**的窗口（宁可少一个包，也不伪造）。
//
// ## ⚠️ 两条端点纪律
//
//   - 配额与探测**固定走 sandbox**（协议 §2.1：只有推理走轮换端点）；
//   - 未授权**不是错误**：没有 access_token 时返回 ErrSessionDead（上层据此 refresh +
//     重试一次、或提示重新登录），而不是把它当成配额查询失败。
package gemini

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// quotaLoc 配额重置时间的墙钟口径（UTC+8），与 provider 的 expireLoc 同值。
var quotaLoc = time.FixedZone("UTC+8", 8*60*60)

// codeAssistInfo loadCodeAssist 的解析结果（只取本包真正需要的字段）。
type codeAssistInfo struct {
	// Project 上游下发的项目号（免费档常为空 ⇒ 调用方回落 DefaultProject）。
	Project string
	// PaidTierID / PaidTierName 账号档位。⚠️ 判据是 **paidTier**，不是 currentTier：
	// 实测两个账号的 currentTier 都是 free-tier/Antigravity，真正区分「有没有 Pro」的是 paidTier。
	PaidTierID   string
	PaidTierName string
	// CurrentTierID / CurrentTierName 仅作 paidTier 缺席时的兜底。
	CurrentTierID   string
	CurrentTierName string
}

// loadCodeAssist 探测账号档位与项目（`POST {sandbox}/v1internal:loadCodeAssist`）。
//
// 请求体是**逐字常量**（LoadCodeAssistBody，content-length 恰为 38）：刻意不带
// platform / pluginType / cloudaicompanionProject，多一个字段就可能被识别成非官方客户端。
func (c *Client) loadCodeAssist(a *auth.Auth) (codeAssistInfo, error) {
	token := tokenOf(a)
	if token == "" {
		return codeAssistInfo{}, &provider.Error{Kind: provider.ErrSessionDead, Msg: "gemini：尚未授权 Google 账号"}
	}
	status, raw, err := c.postJSON(c.sandboxHost()+EpLoadCodeAssist, token, []byte(LoadCodeAssistBody))
	if err != nil {
		return codeAssistInfo{}, err
	}
	if status >= 400 {
		return codeAssistInfo{}, classifyErr(status, raw, "账号档位/项目探测")
	}
	info, perr := parseCodeAssist(raw)
	if perr != nil {
		// 空响应/非 JSON **算探测失败**：协议 §2.7 明确「探测失败（网络 / 非 2xx / 空响应）
		// ⇒ 不发推理」。这里把解析失败也归入同一类，避免拿一个「看起来成功但读不出项目」
		// 的结果去发请求。
		return codeAssistInfo{}, perr
	}
	return info, nil
}

// parseCodeAssist 解析 loadCodeAssist 响应。
//
// 项目取值顺序（协议 §2.7）：顶层 `cloudaicompanionProject` → `currentTier.cloudaicompanionProject`。
// 档位取值顺序：`paidTier` → `currentTier`（paidTier 才是判据，currentTier 只兜底）。
func parseCodeAssist(raw []byte) (codeAssistInfo, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return codeAssistInfo{}, fmt.Errorf("gemini：loadCodeAssist 返回空响应")
	}
	var payload struct {
		CloudAICodeProject string `json:"cloudaicompanionProject"`
		PaidTier           *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"paidTier"`
		CurrentTier *struct {
			ID                 string `json:"id"`
			Name               string `json:"name"`
			CloudAICodeProject string `json:"cloudaicompanionProject"`
		} `json:"currentTier"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return codeAssistInfo{}, fmt.Errorf("gemini：loadCodeAssist 响应不是 JSON（%v）", err)
	}
	info := codeAssistInfo{
		Project: strings.TrimSpace(payload.CloudAICodeProject),
	}
	if payload.CurrentTier != nil {
		info.CurrentTierID = strings.TrimSpace(payload.CurrentTier.ID)
		info.CurrentTierName = strings.TrimSpace(payload.CurrentTier.Name)
		if info.Project == "" {
			info.Project = strings.TrimSpace(payload.CurrentTier.CloudAICodeProject)
		}
	}
	if payload.PaidTier != nil {
		info.PaidTierID = strings.TrimSpace(payload.PaidTier.ID)
		info.PaidTierName = strings.TrimSpace(payload.PaidTier.Name)
	}
	if info.Project == "" && info.PaidTierID == "" && info.CurrentTierID == "" {
		// 三个阶段都读不出东西：按探测失败处理（照原版：失败就不发推理）。
		// ⚠️ 注意「200 但 project 为空」是**免费档常态**，那时 CurrentTier 通常仍在，
		// 故不会被判失败；这里拦的是「响应里什么都没有」的真失败形态。
		return codeAssistInfo{}, fmt.Errorf("gemini：loadCodeAssist 响应里既无 project 也无档位")
	}
	return info, nil
}

// tierLabel 档位 id / 名 → 面板短标签（gemini-credits.ts:123-129）。
//
// ⚠️ 顺序有意义：`Ultra` → `Pro` → `Free`。上游把「Pro 但已降级」也叫 `free-tier`
// （Antigravity Starter Quota），故不能只看 id 的 `-tier` 后缀。
func tierLabel(id, name string) string {
	combined := strings.ToLower(strings.TrimSpace(id) + " " + strings.TrimSpace(name))
	switch {
	case strings.Contains(combined, "ultra"):
		return "Ultra"
	case strings.Contains(combined, "pro"):
		return "Pro"
	case strings.Contains(combined, "free"):
		return "Free"
	}
	if n := strings.TrimSpace(name); n != "" {
		return n
	}
	return strings.TrimSpace(id)
}

// ---------------------------------------------------------------------------
// 配额窗口
// ---------------------------------------------------------------------------

// quotaBucket 一个上游配额桶（只取本包需要的四个字段）。
type quotaBucket struct {
	BucketID          string   `json:"bucketId"`
	Window            string   `json:"window"`
	ResetTime         string   `json:"resetTime"`
	RemainingFraction *float64 `json:"remainingFraction"`
}

// quotaResponse 配额响应形状（协议 §2.7 的实测样例）。
type quotaResponse struct {
	Groups []struct {
		DisplayName string        `json:"displayName"`
		GroupID     string        `json:"groupId"`
		Buckets     []quotaBucket `json:"buckets"`
	} `json:"groups"`
}

// quotaSummary 查询配额窗口并映射成 ResourceItem（结果按 QuotaCacheTTL 缓存）。
func (c *Client) quotaSummary(a *auth.Auth, project string) ([]provider.ResourceItem, error) {
	token := tokenOf(a)
	if token == "" {
		return nil, &provider.Error{Kind: provider.ErrSessionDead, Msg: "gemini：尚未授权 Google 账号"}
	}
	key := projectKey(a)
	if items, ok := c.cachedQuota(key); ok {
		return items, nil
	}
	// ⚠️ 字段名是 `project`（不是 cloudaicompanionProject），见文件头第 1 条。
	body, err := json.Marshal(map[string]string{"project": project})
	if err != nil {
		return nil, err
	}
	status, raw, err := c.postJSON(c.sandboxHost()+EpQuotaSummary, token, body)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, classifyErr(status, raw, "配额查询")
	}
	items, perr := parseQuotaItems(raw)
	if perr != nil {
		return nil, perr
	}
	c.cacheQuota(key, items)
	return items, nil
}

// parseQuotaItems 解析配额响应 → ResourceItem 列表。
// 按模型组（Gemini 与 Claude/GPT 等）分别识别 5 小时窗口和周窗口，独立分开显示。
func parseQuotaItems(raw []byte) ([]provider.ResourceItem, error) {
	var resp quotaResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("gemini：配额响应不是 JSON（%v）", err)
	}

	items := make([]provider.ResourceItem, 0, 4)
	for gi := range resp.Groups {
		grp := resp.Groups[gi]
		groupPrefix, groupKey := normalizeQuotaGroup(grp.DisplayName, grp.GroupID)
		if groupPrefix == "" {
			continue
		}

		var fiveHour, weekly *quotaBucket
		for bi := range grp.Buckets {
			b := grp.Buckets[bi]
			w := normalizeQuotaWindow(b)
			switch w {
			case "5h":
				if fiveHour == nil {
					fiveHour = &b
				}
			case "weekly":
				if weekly == nil {
					weekly = &b
				}
			}
		}

		for _, b := range []*quotaBucket{fiveHour, weekly} {
			if b == nil {
				continue
			}
			item, ok := quotaItem(*b, groupPrefix, groupKey)
			if ok {
				items = append(items, item)
			}
		}
	}

	if len(items) == 0 {
		return nil, fmt.Errorf("gemini：配额响应里未找到可用配额桶")
	}
	return items, nil
}

// normalizeQuotaGroup 归一化配额组前缀与键名。
func normalizeQuotaGroup(displayName, groupID string) (prefix, key string) {
	combined := strings.ToLower(displayName + " " + groupID)
	if strings.Contains(combined, "gemini") {
		return "Gemini", "gemini"
	}
	if strings.Contains(combined, "claude") || strings.Contains(combined, "gpt") {
		return "Claude/GPT", "claude_gpt"
	}
	trimmed := strings.TrimSpace(displayName)
	if trimmed == "" {
		trimmed = strings.TrimSpace(groupID)
	}
	if trimmed == "" {
		return "", ""
	}
	return trimmed, strings.ToLower(trimmed)
}

// normalizeQuotaWindow 识别桶是 5 小时窗口还是周窗口。
func normalizeQuotaWindow(b quotaBucket) string {
	w := strings.ToLower(strings.TrimSpace(b.Window))
	if w == "5h" || w == "session" {
		return "5h"
	}
	if w == "weekly" || w == "7d" {
		return "weekly"
	}
	text := strings.ToLower(b.BucketID)
	if strings.Contains(text, "5h") || strings.Contains(text, "session") {
		return "5h"
	}
	if strings.Contains(text, "weekly") || strings.Contains(text, "7d") {
		return "weekly"
	}
	return ""
}

// quotaItem 单个桶 → ResourceItem；ok=false 表示该桶字段不满足硬约束。
func quotaItem(b quotaBucket, groupPrefix, groupKey string) (provider.ResourceItem, bool) {
	windowType := normalizeQuotaWindow(b)
	windowLabel := ""
	switch windowType {
	case "5h":
		windowLabel = WindowLabelFiveHour
	case "weekly":
		windowLabel = WindowLabelWeekly
	default:
		return provider.ResourceItem{}, false
	}

	if b.RemainingFraction == nil || math.IsNaN(*b.RemainingFraction) || math.IsInf(*b.RemainingFraction, 0) {
		return provider.ResourceItem{}, false
	}
	reset, err := time.Parse(time.RFC3339, strings.TrimSpace(b.ResetTime))
	if err != nil {
		return provider.ResourceItem{}, false
	}
	percent := int64(math.Round(clamp01(*b.RemainingFraction) * 100))
	key := b.BucketID
	if key == "" {
		key = groupKey + "_" + windowType
	}
	return provider.ResourceItem{
		// 格式：Gemini 5 小时窗口（%）、Claude/GPT 周窗口（%）
		Name:     groupPrefix + " " + windowLabel + "（" + QuotaUnit + "）",
		Total:    100,
		Remain:   percent,
		Used:     100 - percent,
		ExpireAt: reset.In(quotaLoc).Format(time.RFC3339),
		Key:      key,
		Usable:   true,
		InfoOnly: true,
	}, true
}

// clamp01 把剩余比例收敛到 [0,1]（越界值来自上游，不能让百分比溢出）。
func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// cachedQuota 读配额缓存（TTL 内有效）。
func (c *Client) cachedQuota(key string) ([]provider.ResourceItem, bool) {
	if key == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.quotaCache[key]
	if !ok || time.Since(e.at) > QuotaCacheTTL {
		return nil, false
	}
	return e.items, true
}

// cacheQuota 写配额缓存。
func (c *Client) cacheQuota(key string, items []provider.ResourceItem) {
	if key == "" {
		return
	}
	c.mu.Lock()
	if c.quotaCache == nil {
		c.quotaCache = map[string]quotaCacheEntry{}
	}
	c.quotaCache[key] = quotaCacheEntry{at: time.Now(), items: items}
	c.mu.Unlock()
}

// classifyErr 把 HTTP 层失败转成类型化的 *provider.Error。
func classifyErr(status int, raw []byte, what string) error {
	return &provider.Error{
		Kind:   Classify(status, string(raw)),
		Status: status,
		Msg:    fmt.Sprintf("gemini：%s失败（HTTP %d）：%s", what, status, snippet(raw)),
	}
}

// ---------------------------------------------------------------------------
// provider.Upstream：余额
// ---------------------------------------------------------------------------

// UserResource 实现 provider.Upstream。
//
// ⚠️ 返回 **0**：本渠道没有「可消耗积分」这个概念，配额是窗口百分比（见文件头）。
// 返回 0 是如实的「本工具口径下没有可消耗积分」，而不是「查询失败」——
// 配额明细由 UserResourceDetail 的 InfoOnly 条目承载。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	_, _, err := c.UserResourceDetail(a)
	return 0, err
}

// UserResourceDetail 实现 provider.Upstream：返回 (0, 配额窗口条目, error)。
//
// 编排（与参考实现同构）：
//  1. project 取「凭据真值 → 未知则先探测」：配额请求**必须**带正确的 project
//     （协议 §2.7 第 1 条），拿默认值去问会得到 403；
//  2. 档位探测（loadCodeAssist）与配额查询**并行**，且**档位失败绝不污染配额结果**——
//     「账号规格」只是一栏附注，不能因为它挂了就把整行变成错误。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	token := tokenOf(a)
	if token == "" {
		return 0, nil, &provider.Error{Kind: provider.ErrSessionDead, Msg: "gemini：尚未授权 Google 账号"}
	}

	project := storedProject(a)
	info := codeAssistInfo{}
	probed := false
	if project == "" {
		// 项目未知：先探一次（探测失败也继续——配额用默认值试，错误如实透出）。
		if in, err := c.loadCodeAssist(a); err == nil {
			info, probed = in, true
			if p := strings.TrimSpace(in.Project); p != "" {
				project = p
			}
			c.cacheProject(projectKey(a), orDefaultProject(project))
		}
	}
	if project == "" {
		project = DefaultProject
	}

	tierCh := make(chan tierProbe, 1)
	go func() {
		if probed {
			tierCh <- tierProbe{info: info}
			return
		}
		in, err := c.loadCodeAssist(a)
		tierCh <- tierProbe{info: in, err: err}
	}()

	items, qerr := c.quotaSummary(a, project)
	tr := <-tierCh
	if tr.err == nil {
		c.persistProject(a, tr.info)
	}
	if qerr != nil {
		return 0, nil, qerr
	}
	return 0, items, nil
}

// tierProbe 档位探测的并行结果。
type tierProbe struct {
	info codeAssistInfo
	err  error
}

// orDefaultProject 空串 → DefaultProject（缓存与请求体都不接受空 project）。
func orDefaultProject(p string) string {
	if strings.TrimSpace(p) == "" {
		return DefaultProject
	}
	return p
}
