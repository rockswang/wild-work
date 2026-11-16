package modelscope

// credits.go —— 魔粒（magicubes）余额与今日用量（票 02）。
//
// 为什么有这个文件：本渠道接入时（票 01）判定的「上游没有余额/积分端点」是**错的**
// ——当时只试了对话基址（api-inference.modelscope.{cn,ai}），而额度端点挂在**主站**
// `modelscope.cn` / `modelscope.ai` 上，与对话面是两个 host（D7）。2026-11-16 的只读探针
// （7 次请求，证据见 .scratch/modelscope-credits/probe-findings.md）实测：
//
//	GET /openapi/v1/magicubes/balance        Authorization: Bearer <ms- token>
//	  {"success":true,"data":{"total_balance":350,"available_balance":350,"frozen_amount":0}}
//
//	GET /api/v1/inference/rate-limit         Cookie: m_session_id=<ms- token>
//	  {"Code":200,"Data":{"currentUsagePerDay":0},"Message":"success","Success":true}
//
// 口径（D2）：
//   - 主积分 = `available_balance`（**只含可消耗额度**，不含冻结）；
//   - `total_balance` 是面额，`used = total − available − frozen`（为负记 0）；
//   - `frozen_amount` 单列一条 Usable=false 的明细（进「不可用」小计，与 TraeWork 专用池同款）。
//     ⚠️ 探针样本 frozen=0，**「冻结是否已包含在 available 内」未实证**，故 used 的公式
//     属假设（负值夹 0，不会把数字算成负数）。
//   - `currentUsagePerDay` 是「今日用量」，按 **token** 计、**只作展示**（InfoOnly），
//     不并入积分合计与积分流水。⚠️ 单位未实证（探针取到 0，无法分辨 token/魔粒/次数），
//     按调研的「按 token 计」展示。
//
// 失败口径（D5）：网络错误 / 5xx 原样返回错误 → 上层保留上次快照、面板标「待刷新」，
// **不写 0、不写「不适用」**；401/403 → `*provider.Error{Kind: ErrSessionDead}`（可自愈标记，
// 用户重贴 token 后自动清除），**不 Disable、不 CooldownModel/Cooldown、不做自动 refresh**
// （`ms-` token 没有 refresh 端点）。余额路径永远不会产生账号级惩罚。
//
// 两区各自独立（token 不通用、互不兜底）：主站基址同样按 Kind 二选一。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 主站端点（**不在**对话基址下，见 D7）。
const (
	// EpMagicubesBalance 魔粒余额（鉴权：Authorization: Bearer）。
	EpMagicubesBalance = "/openapi/v1/magicubes/balance"
	// EpDailyUsage 今日用量（鉴权：Cookie: m_session_id）。
	EpDailyUsage = "/api/v1/inference/rate-limit"
)

// 主站基址：额度/用量挂在主站，与对话基址（api-inference.*）是两个 host。
const (
	OriginCN = "https://modelscope.cn"
	OriginAI = "https://modelscope.ai"
)

const (
	// creditsTimeout 额度查询的独立超时：它是**附加**读取，不能让积分刷新整体卡在
	// 上游的慢端点上（与 comate 的 profileTimeout 同款考虑）。
	creditsTimeout = 30 * time.Second
	// siteUA 主站请求用浏览器 UA（探针即以此拿到 200；主站不是对话网关，没有 CLI 身份概念）。
	siteUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

	// itemNameUsable 可用魔粒条目的稳定名称。
	//
	// ⚠️ 票 03（逐笔到期）把这一条拆成多笔时**必须复用同一个 Name 且不设 Key**：
	// ledger.DiffCredits 的伪键是 Name，改名/拆名会让升级瞬间凭空多出一对假 expire+earn
	// （R107 同款纪律）。
	itemNameUsable = "可用魔粒"
	// itemNameFrozen 冻结魔粒（Usable=false ⇒ 进「不可用」小计，不进路由余额）。
	itemNameFrozen = "冻结魔粒"
	// itemNameDailyUsage 今日用量（InfoOnly ⇒ 只展示，不进任何积分算术）。
	itemNameDailyUsage = "今日用量"
)

// 主站两种鉴权方式（探针实测：balance 走 Bearer，rate-limit 走 Cookie）。
const (
	authBearer = iota
	authCookie
)

// SiteURL 主站基址（按 kind 二选一，两区独立、互不兜底）。
func SiteURL(kind provider.Kind) string {
	if kind == provider.ModelScopeAI {
		return OriginAI
	}
	return OriginCN
}

// siteBase 生效的主站基址（测试可注入 SiteBase）。
func (c *Client) siteBase() string {
	if b := strings.TrimRight(strings.TrimSpace(c.SiteBase), "/"); b != "" {
		return b
	}
	return SiteURL(c.Kind)
}

// magicubesBalance 是 /openapi/v1/magicubes/balance 的 data 段（**逐字字段名**，
// 探针实测带 `_balance` 后缀，不是 available/total）。
type magicubesBalance struct {
	TotalBalance     int64 `json:"total_balance"`
	AvailableBalance int64 `json:"available_balance"`
	FrozenAmount     int64 `json:"frozen_amount"`
}

// fetchSite 向主站发一次 GET。鉴权按端点各自的形态给（见 authBearer/authCookie），
// 状态码 ≥400 一律类型化返回（不变式 19：自愈链全靠 errors.As 取 Kind）：
//
//	401/403 → ErrSessionDead（面板「需重新登录」+ 重贴入口，可自愈，不 Disable）
//	其余 → 交给本渠道 Classify（429→软限流 / 5xx→上游故障 / 4xx→客户端）
//
// ⚠️ 错误信息里绝不拼 token（零 token 落盘纪律）。
func (c *Client) fetchSite(a *auth.Auth, path string, mode int) ([]byte, error) {
	var tok string
	if a != nil {
		tok = strings.TrimSpace(a.AccessToken)
	}
	ctx, cancel := context.WithTimeout(context.Background(), creditsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.siteBase()+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", siteUA)
	if tok != "" {
		if mode == authCookie {
			req.Header.Set("Cookie", "m_session_id="+tok)
		} else {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	cli := c.HTTP
	if cli == nil {
		cli = http.DefaultClient
	}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// 零 token 落盘纪律：错误原文直接进 provider.Error.Msg，若上游把凭据回显在报文里
	// （某些网关会），这里统一抹掉，绝不让它流进日志/面板。
	msg := truncateBody(raw, 256)
	if tok != "" {
		msg = strings.ReplaceAll(msg, tok, "<token>")
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// 401/403 = 凭据失效（与 Classify 同判据，但这里必须**显式**给 ErrSessionDead：
		// 这是可自愈标记的入口，绝不能退化成账号级惩罚）。
		return nil, &provider.Error{Kind: provider.ErrSessionDead, Status: resp.StatusCode, Msg: msg}
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, &provider.Error{Kind: c.Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: msg}
	}
	return raw, nil
}

// fetchBalance 读魔粒余额。
func (c *Client) fetchBalance(a *auth.Auth) (magicubesBalance, error) {
	raw, err := c.fetchSite(a, EpMagicubesBalance, authBearer)
	if err != nil {
		return magicubesBalance{}, err
	}
	var env struct {
		Success bool              `json:"success"`
		Code    string            `json:"code"`
		Data    *magicubesBalance `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return magicubesBalance{}, fmt.Errorf("modelscope: balance 响应解析失败: %w", err)
	}
	if !env.Success || env.Data == nil {
		return magicubesBalance{}, fmt.Errorf("modelscope: balance 业务错误 code=%s", env.Code)
	}
	return *env.Data, nil
}

// fetchDailyUsage 读今日用量（token 计，仅展示）。
func (c *Client) fetchDailyUsage(a *auth.Auth) (int64, error) {
	raw, err := c.fetchSite(a, EpDailyUsage, authCookie)
	if err != nil {
		return 0, err
	}
	// 注意：本端点的信封是**首字母大写**的旧式形态（Code/Data/Message/Success），
	// 与 balance 的小写信封不同——两个端点必须各解析一份。
	var env struct {
		Code    int  `json:"Code"`
		Success bool `json:"Success"`
		Data    struct {
			CurrentUsagePerDay int64 `json:"currentUsagePerDay"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return 0, fmt.Errorf("modelscope: rate-limit 响应解析失败: %w", err)
	}
	if !env.Success || env.Code != http.StatusOK {
		return 0, fmt.Errorf("modelscope: rate-limit 业务错误 code=%d", env.Code)
	}
	return env.Data.CurrentUsagePerDay, nil
}

// balanceItems 把余额摊成明细条目（D2）：
//
//	可用魔粒（Usable=true，进路由余额与积分合计）
//	冻结魔粒（Usable=false，仅当 frozen>0；进「不可用」小计，不进路由）
func balanceItems(b magicubesBalance) []provider.ResourceItem {
	used := b.TotalBalance - b.AvailableBalance - b.FrozenAmount
	if used < 0 {
		used = 0
	}
	items := []provider.ResourceItem{{
		Name:   itemNameUsable,
		Total:  b.TotalBalance,
		Used:   used,
		Remain: b.AvailableBalance,
		Usable: true,
	}}
	// 冻结为 0 时不铺一条「0」的行（不制造无意义的 0）。
	if b.FrozenAmount > 0 {
		items = append(items, provider.ResourceItem{
			Name:   itemNameFrozen,
			Total:  b.FrozenAmount,
			Remain: b.FrozenAmount,
			Usable: false,
		})
	}
	return items
}

// dailyUsageItem 今日用量条目：**InfoOnly**（单位是 token，与魔粒相加无意义）。
// 数字落在 Used 列（它是「已消耗」而不是「剩余」），前端按 tokens 紧凑单位显示。
func dailyUsageItem(usage int64) provider.ResourceItem {
	return provider.ResourceItem{
		Name:     itemNameDailyUsage,
		Used:     usage,
		Usable:   true,
		InfoOnly: true,
		Unit:     "token",
	}
}

// UserResourceDetail 实现 provider.Upstream：返回（可用魔粒, 明细, error）。
//
// 今日用量是**锦上添花**：它读取失败只记日志、不影响余额刷新（与 comate 的试用额度同款
// ——附加维度失败不得把主维度搞挂）。余额本身失败则原样上抛（上层保留上次快照）。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	bal, err := c.fetchBalance(a)
	if err != nil {
		return 0, nil, err
	}
	items := balanceItems(bal)
	if usage, uerr := c.fetchDailyUsage(a); uerr == nil {
		items = append(items, dailyUsageItem(usage))
	} else {
		// 只记渠道名与错误（零 token）。
		log.Printf("modelscope 今日用量读取失败 platform=%s err=%v", c.Kind, uerr)
	}
	return bal.AvailableBalance, items, nil
}

// UserResource 实现 provider.Upstream：**直接复用** UserResourceDetail 的 remain
// （D7：两者不得各算一份，否则同一屏两个数字对不上）。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	remain, _, err := c.UserResourceDetail(a)
	return remain, err
}
