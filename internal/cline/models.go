package cline

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// ---------------------------------------------------------------------------
// 动态目录（唯一来源）
// ---------------------------------------------------------------------------

// remoteModel 是 recommended-models 数组元素的形状。
type remoteModel struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
}

// recommendResp 上游 recommended-models 信封：模型面只消费 free 数组（其余分组不落结构）。
type recommendResp struct {
	Free []remoteModel `json:"free"`
}

// StaticModels 恒 nil：本渠道**不做任何本地假设**（无静态表、无映射、无过滤）。
// 模型面完全由上游 recommended-models 的 free 数组决定，原样透传。
// 兜底语义见 handler.CachedChannelModels —— 缓存优先于静态表，空表不会让渠道分组消失
// （与 qodercn/qodercom 同款先例）。
func StaticModels() []provider.ModelInfo { return nil }

// measuredCap 是**手工实测**的模型元数据（非上游下发、非官方值）。
type measuredCap struct {
	ctx    int64 // 实测通过的最大 prompt_tokens，已向下取整到档位
	images bool  // 发图是否被上游接受
}

// measuredCaps 上下文/多模态的实测结果（2026-10-12，经本工具网关 127.0.0.1:7863 实测）。
//
// **为什么必须本地维护这张表**：上游 `recommended-models` 的 free[] 条目只含
// id/name/description/tags，**完全不返回上下文窗口与能力字段**（实测），
// 而客户端（Claude Code / Codex 等）靠 `/v1/models` 的 context_length 与
// architecture.input_modalities 决定何时压缩、能否收图，故只能本地实测补全。
//
// 口径与边界（勿当官方值，也勿当作永久真值）：
//  1. ctx = 实测「成功返回且三暗号（头/中/尾）全部回忆正确」的最大 prompt_tokens，
//     再**向上取到常用档位并留一档余量**（用户裁定 2026-10-12）：实测值被本工具
//     90s 响应头超时与探针阶梯双重压制，必然偏低；且客户端把 context_length 当作
//     「可用输入上限」用于压缩时机，略微宽报比偏窄更贴合上游真实容量（上游超限会
//     给出权威错误，不会静默错误回答）。具体：mimo 实测 728K → 报 1M；
//     muse 实测 928K → 报 1M；step-5 实测 800K → 报 1M；solar 实测 400K → 报 512K。
//     三暗号验证是为了排除上游静默截断（只保留尾部）把实测值抬高到虚假水平。
//  2. 超大 prompt（>~700k）常因上游首字节耗时过长而被本工具 client 的
//     90s ResponseHeaderTimeout 中断 —— 故实测值只是**下界**（第 1 条已据此上取
//     一档），且 >90s 首字节本身已无实用意义，不再继续抬高求证。
//  3. images 由「发图是否被上游接受」实测；solar-mini4 上游明确回
//     404 `No endpoints found that support image input`。
//     mimo / muse / step 三者的实测均能正确识别图片内容（红/蓝/黄圆）。
//  4. tools：四个模型均实测返回标准 tool_calls 并正确消费 tool 结果，故未单列开关
//     （provider.ModelInfo 的 SupportsTools 保持 false = 「未声明」，不虚报）。
//  5. 地区限制：muse-spark-1.3-contributor 对国内直连恒回 403
//     `not available in your region`，需在「设置 → 备用上游通道」配一个代理地址才可用（实测）。
//     与旧「每渠道代理」不同：备用通道不参与常规请求，只在本渠道命中地区拒绝时
//     由 ChatStream 在同一次请求内自动重发一次（见 client.go 的地区限制改道分支与决议 R57）。
//  6. 未列入本表的模型（上游新增）仍然照常暴露，但没有上下文/能力声明 ——
//     这与 R46「模型面绑死上游 free[]」不矛盾：本表只补元数据，不增删模型。
var measuredCaps = map[string]measuredCap{
	"cline-free/mimo-v2.6-flash":            {ctx: 1_000_000, images: true},
	"cline-free/muse-spark-1.3-contributor": {ctx: 1_000_000, images: true},
	"cline-free/solar-mini4":                {ctx: 512_000, images: false},
	"cline-free/step-5-preview":             {ctx: 1_000_000, images: true},
}

// modelInfoOf 组装单条模型信息：基础字段来自上游 free[]，上下文/能力来自实测表（若有）。
// ContextFromAPI 保持 false：这些值确实不来自上游接口；另置 HandMeasured=true 如实标注
// 来源（面板据此展示数字并标注「实测」，/v1/models 也照常带上，供客户端判断压缩时机）。
func modelInfoOf(id, name string) provider.ModelInfo {
	mi := provider.ModelInfo{ID: id, Name: name}
	if c, ok := measuredCaps[id]; ok {
		mi.ContextWindow = c.ctx
		mi.SupportsImages = c.images
		mi.HandMeasured = true
	}
	return mi
}

// FetchModels 返回暴露集 = recommended-models 的 free 数组**原样透传**：
//  1. 不碰 /models（458 条 OpenRouter 镜像，对「免费池」定位无信息量）；
//  2. ID 不改名不改前缀（free[] 本身就是 `cline-free/*` 别名空间）；
//  3. 不按 tag/后缀过滤、不做基名去重 —— 上游给的什么就暴露什么；
//  4. 仅做两条机械兜底：精确 ID 去重、name 缺失时 prettifyID 兜底；
//  5. free[] 条目顺带记入 knownIDs（本地校验集 = 暴露集，构造即一致）。
//
// 撞共享上限的模型从暴露集摘除（markCapped），解冻后自动回归。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	var rec recommendResp
	raw, err := c.getJSON(a, EpRecommends)
	if err == nil {
		err = json.Unmarshal(raw, &rec)
	}
	if err != nil {
		// 兜底：free[] 为空但缓存尚有内容 ⇒ 返回缓存（上游目录瞬时抖动不掏空面板）
		c.knownMu.RLock()
		cached := make([]provider.ModelInfo, 0, len(c.knownIDs))
		for id, name := range c.knownIDs {
			if !c.isCapped(id) {
				cached = append(cached, modelInfoOf(id, name))
			}
		}
		c.knownMu.RUnlock()
		sort.Slice(cached, func(i, j int) bool { return cached[i].ID < cached[j].ID })
		if len(cached) > 0 {
			return cached, nil
		}
		return nil, fmt.Errorf("cline: 免费模型目录拉取失败: %w", err)
	}

	seen := map[string]bool{}
	out := make([]provider.ModelInfo, 0, len(rec.Free))
	live := make([]string, 0, len(rec.Free))
	for _, m := range rec.Free {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] || c.isCapped(id) {
			continue
		}
		seen[id] = true
		live = append(live, id)
		name := m.Name
		if name == "" || name == id {
			name = prettifyID(id)
		}
		out = append(out, modelInfoOf(id, name))
		c.rememberLiveModel(id, name)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// RouteOf 按上游响应 id 的形态判断该请求实际走了哪条链。
//
// 实测判据（2026-10-10，9 个 ID 零反例）：
//
//	gen-<unix秒>-<20位Base62> → OpenRouter（Cline 转发给聚合器，响应无 provider_metadata）
//	gen_<ULID 26 位>          → Cline 自有网关（带 marketCost/routing/fallbacksAvailable）
//
// 为什么需要它：配额键名是 `…UsageCostUSDPerUser`（按 Cline 账号计），
// 而 `limit_rpd` 日限落在 **Cline 自己的 OpenRouter key** 上 ⇒ 换 Cline 账号必然复现。
// 二者必须区分处置（见 Classify 的 ErrPassthrough vs ErrSoftRate），而成功响应里
// **没有任何字段直接写着聚合器名字**（is_byok / provider 实测都不等价，不可用作判据）。
func RouteOf(responseID string) string {
	switch {
	case strings.HasPrefix(responseID, routeClineGW):
		return "cline_gateway"
	case strings.HasPrefix(responseID, routeOpenRouter):
		return "openrouter"
	default:
		return "unknown"
	}
}

// prettifyID 把 `cline-free/step-5-preview` 这类裸 ID 变成可读展示名。
func prettifyID(id string) string {
	s := strings.TrimPrefix(strings.TrimSpace(id), freePrefix)
	s = strings.ReplaceAll(s, "-", " ")
	if r := []rune(s); len(r) > 0 {
		return strings.ToUpper(string(r[0])) + string(r[1:])
	}
	return s
}

// ---------------------------------------------------------------------------
// 校验集与模型级冷却
// ---------------------------------------------------------------------------

// knownModel 本地校验。**冷启动放行**：目录尚未成功拉取过（knownIDs 为空）时不拦截 ——
// 若拦，daemon 重启后第一批请求会全部被本地 404（等于「渠道周期性瘫痪」），
// 而放行的最坏结果只是让上游给出权威错误。目录拉取成功后校验集 == 暴露集 == free[]，构造即一致。
func (c *Client) knownModel(id string) bool {
	c.knownMu.RLock()
	defer c.knownMu.RUnlock()
	if len(c.knownIDs) == 0 {
		return true
	}
	_, ok := c.knownIDs[id]
	return ok
}

// rememberLiveModel 把 free[] 条目记入已知集（ID → name，**替换式**更新）：
// name 跟随上游目录同步，避免用缓存里的旧名。同时清掉同名冷却残迹由 isCapped 的过期清理负责。
func (c *Client) rememberLiveModel(id, name string) {
	c.knownMu.Lock()
	defer c.knownMu.Unlock()
	if c.knownIDs == nil {
		c.knownIDs = map[string]string{}
	}
	c.knownIDs[id] = name
}

// capCooldown 撞共享上限后的默认摘除时长：`limit_rpd` 是**日**上限，给 1 小时足够
// 让面板不至于长期空列表，且每日重置后自动回归；`temporarily rate-limited` 更短就够，
// 统一取一个中等值避免为两种文案各写一条分支（工程实用主义）。
const capCooldown = time.Hour

// markCapped 记录「该模型此刻拿不到」，FetchModels 期间从暴露集摘除。
// 键为**精确 ID**：free[] 本身就是别名空间（没有「别名 vs 真实名」对），无需基名归一。
func (c *Client) markCapped(model string, d time.Duration) {
	if strings.TrimSpace(model) == "" {
		return
	}
	c.capMu.Lock()
	defer c.capMu.Unlock()
	if c.capped == nil {
		c.capped = map[string]time.Time{}
	}
	c.capped[model] = time.Now().Add(d)
	for k, t := range c.capped {
		if time.Now().After(t) {
			delete(c.capped, k)
		}
	}
}

// isCapped 按精确 ID 判定。
func (c *Client) isCapped(model string) bool {
	c.capMu.RLock()
	defer c.capMu.RUnlock()
	t, ok := c.capped[model]
	return ok && time.Now().Before(t)
}
