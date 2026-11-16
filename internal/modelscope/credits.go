package modelscope

// credits.go —— 魔粒（magicubes）余额、今日用量与逐笔到期（票 02 / 票 03）。
//
// 为什么有这个文件：本渠道接入时（票 01）判定的「上游没有余额/积分端点」是**错的**
// ——当时只试了对话基址（api-inference.modelscope.{cn,ai}），而额度端点挂在**主站**
// `modelscope.cn` / `modelscope.ai` 上，与对话面是两个 host（D7）。2026-11-16 的只读探针
// （7 次请求，证据见 .scratch/modelscope-credits/probe-findings.md）实测：
//
//	GET /openapi/v1/magicubes/balance        Authorization: Bearer <ms- token>
//	  {"success":true,"data":{"total_balance":350,"available_balance":350,"frozen_amount":0}}
//
//	GET /openapi/v1/magicubes/transactions   Authorization: Bearer <ms- token>
//	  {"success":true,"data":{"page":1,"page_size":20,"total":18,"records":[…]}}
//	  EARN 行逐笔带 expire_at（"2026-11-17 10:00:36"，UTC+8 墙钟、非 RFC3339）
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
//   - `transactions` 的 EARN 行逐笔带 expire_at ⇒ 拆成逐笔条目、各带自己的到期时刻
//     （D3，票 03），面板与账本据此算「今日到期 / N 日内到期」与 expire 归因。
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
	"sort"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 主站端点（**不在**对话基址下，见 D7）。
const (
	// EpMagicubesBalance 魔粒余额（鉴权：Authorization: Bearer）。
	EpMagicubesBalance = "/openapi/v1/magicubes/balance"
	// EpMagicubesTransactions 魔粒逐笔流水（鉴权：Authorization: Bearer）。
	// EARN 行逐笔带 expire_at ⇒ 这是「逐笔到期」的唯一来源（票 03，探针 §2 实测）。
	EpMagicubesTransactions = "/openapi/v1/magicubes/transactions"
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

	// magicubeExpireLayout 上游 expire_at 的**逐字**格式（探针 §2 实测）：
	// `"2026-11-17 10:00:36"` —— 空格分隔、无 T、**无时区后缀**、按 UTC+8 墙钟解读。
	// ⚠️ 不能拿 time.RFC3339 去 Parse：解析失败会被当成「无到期信息」而静默哑火。
	magicubeExpireLayout = "2006-01-02 15:04:05"
)

// creditLoc 魔粒到期时刻的墙钟口径（与不变量 18 / provider.ExpireInstant 一致）。
var creditLoc = time.FixedZone("UTC+8", 8*60*60)

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
	// ⚠️ 必须**先替换再截断**：倒过来的话，跨截断边界的 token 只会漏半截进错误且替换不命中。
	body := string(raw)
	if tok != "" {
		body = strings.ReplaceAll(body, tok, "<token>")
	}
	msg := truncateBody([]byte(body), 256)
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

// magicubesTransactionsData /openapi/v1/magicubes/transactions 的 data 段。
//
// 记录是**聚合行**：每行以 (gmt_created, rule_key) 分组并带 `count`（EXPIRE 行实测 count:2）。
// 本样本所有 EARN 行 count 均为 1；若某条 EARN 行 count>1，其单个 expire_at 无法表示多笔
// 不同到期时刻——按一个聚合笔处理（待观察，见 probe-findings.md §2.2）。
// `total_amount` 是**行金额**（EXPIRE 行 250 + count:2 即两笔合计），直接当发放额用。
type magicubesTransactionsData struct {
	Page     int                    `json:"page"`
	PageSize int                    `json:"page_size"`
	Total    int                    `json:"total"`
	Records  []magicubesTransaction `json:"records"`
}

// magicubesTransaction 单条魔粒流水（探针 §2.1 实测字段）。
//
// type 实测三种：`EARN`（发放，逐笔带 expire_at）/ `SPEND_CONFIRM`（对话消耗，无到期）/
// `EXPIRE`（到期回收，无到期，**仅用于对账**）。
type magicubesTransaction struct {
	GMTCreated string `json:"gmt_created"` // 日期粒度（"2026-11-16"），非时刻
	Type       string `json:"type"`
	RuleKey    string `json:"rule_key"`
	TotalAmt   int64  `json:"total_amount"`
	Count      int64  `json:"count"`
	ExpireAt   string `json:"expire_at"` // 仅 EARN 有，"2006-01-02 15:04:05"（UTC+8）
	ExpireDays int64  `json:"expire_days"`
}

// magicubeGrant 一笔在效的魔粒发放（EARN 里未到期的那部分）。
type magicubeGrant struct {
	Points   int64
	ExpireAt string // RFC3339 精确时刻（已换算成带 +08:00 的形态）
}

// fetchMagicubeTransactions 读原始魔粒流水（不筛选）。抽出来供票 04 的「当日领取」
// 复用（那边要按 gmt_created 找当日 EARN，与「在效发放」的筛选口径不同）。
//
// 分页：探针实测默认 `page=1&page_size=20`（样本 18 条一页取全），但 `page_size` 可调性
// **未验证**（待观察）。本轮只取第 1 页——`page_size=100` 之类属推断，不擅自发明；
// 若日后实测确认可调，只改这一处。
func (c *Client) fetchMagicubeTransactions(a *auth.Auth) (magicubesTransactionsData, error) {
	raw, err := c.fetchSite(a, EpMagicubesTransactions, authBearer)
	if err != nil {
		return magicubesTransactionsData{}, err
	}
	var env struct {
		Success bool                       `json:"success"`
		Code    string                     `json:"code"`
		Data    *magicubesTransactionsData `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return magicubesTransactionsData{}, fmt.Errorf("modelscope: transactions 响应解析失败: %w", err)
	}
	if !env.Success || env.Data == nil {
		return magicubesTransactionsData{}, fmt.Errorf("modelscope: transactions 业务错误 code=%s", env.Code)
	}
	return *env.Data, nil
}

// fetchMagicubeGrants 拉取「仍在有效期内」的魔粒发放，按**到期时刻升序**返回。
//
// 为什么要按到期升序：上游按**先进先出**消耗（最老的一笔先花，见 R107 同款实测），
// 升序即「消耗顺序」，withMagicubeExpiry 的余额拆分依赖它。已到期的发放必须剔除，
// 否则「按到期升序」的第一笔永远是过期笔，把今日到期算成 0（与 raccoon 同款坑）。
func (c *Client) fetchMagicubeGrants(a *auth.Auth) ([]magicubeGrant, error) {
	tx, err := c.fetchMagicubeTransactions(a)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	grants := make([]magicubeGrant, 0, len(tx.Records))
	for _, r := range tx.Records {
		// 只认 EARN：SPEND_CONFIRM 是消耗、EXPIRE 是到期回收（仅对账，不单独成条）。
		if !strings.EqualFold(strings.TrimSpace(r.Type), "EARN") || r.TotalAmt <= 0 {
			continue
		}
		exp, ok := normalizeMagicubeExpire(r.ExpireAt)
		if !ok {
			continue
		}
		if t, valid := provider.ExpireInstant(exp); valid && !now.Before(t) {
			continue // 已到期：对应的额度早已被上游清零并从池子里扣掉
		}
		grants = append(grants, magicubeGrant{Points: r.TotalAmt, ExpireAt: exp})
	}
	sort.Slice(grants, func(i, j int) bool {
		ti, oki := provider.ExpireInstant(grants[i].ExpireAt)
		tj, okj := provider.ExpireInstant(grants[j].ExpireAt)
		if !oki || !okj {
			return grants[i].ExpireAt < grants[j].ExpireAt
		}
		return ti.Before(tj)
	})
	return grants, nil
}

// normalizeMagicubeExpire 把上游 expire_at 归一到 **RFC3339 精确时刻**。
//
// 魔粒是「发放后 24h 精确过期」而非「当日 24:00」，故**不得**用纯日期写法
// （"YYYY-MM-DD" 会被 provider.ExpireInstant 解读为次日 00:00，把 10:00 到期的
// 额度算到次日）。上游原格式是 "2006-01-02 15:04:05" + UTC+8 墙钟（探针 §2 实测），
// 换算后即为 "2026-11-17T10:00:36+08:00"。
func normalizeMagicubeExpire(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if t, err := time.ParseInLocation(magicubeExpireLayout, s, creditLoc); err == nil {
		return t.Format(time.RFC3339), true
	}
	// 容错：上游若改成 RFC3339，原样接受并统一到 UTC+8 墙钟形态。
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(creditLoc).Format(time.RFC3339), true
	}
	return "", false
}

// withMagicubeExpiry 把聚合的「可用魔粒」条目换成**逐笔发放**的条目，各带自己的到期时刻。
//
// 两个约束同时成立，这是本函数存在的全部理由（与 raccoon 的 withDailyExpiry 同构）：
//
//  1. **面板要逐笔**：`stats.expiryItems` 是逐条目分桶的，只有拆开才能算出
//     「今日到期 / N 日内到期」；不拆就只能整池挂一个到期日，把全部在效发放
//     都报成最早的到期（虚高，属于编造）。
//  2. **账本要单键**：所有拆分条目共用同一个 Name（itemNameUsable）且**不设 Key**，
//     `ledger.DiffCredits` 的按 key 聚合（remain/used/total 求和、expire 取首个非空）
//     会把它们合回「可用魔粒」一条——与拆分前完全同键同值，升级零假流水；
//     聚合后的 Expire 取到的是**最早**那一笔，正是 `ledger.expired()` 需要的判据
//     （最早一笔过期 ⇒ 本轮余额下降归因 expire 而非 spend）。
//
// 余额拆分口径：上游按先进先出消耗（最老先花），故池子里剩下的余额属于**最新的那几笔**；
// 拆分从到期最晚的一笔往前分配，再按到期升序输出——Remain 之和恒等于 available
// （`provider.Summarize` 与 pool 快照据此取值），输出升序则让账本聚合后取到的 Expire
// 是最早一笔。
//
// ⚠️ 方向不能反：反着填（余额先给最早的一笔）会让「今晚到期的额度」这种假临期虚报。
// 这与 D3「按到期升序 FIFO 扣减 SPEND_CONFIRM，余量挂最后一笔」等价：可用余额本身
// 就是扣减完消耗之后的产物，从最新一笔往前分配 = 最老那几笔已被烧掉。
func withMagicubeExpiry(items []provider.ResourceItem, grants []magicubeGrant) []provider.ResourceItem {
	idx := -1
	for i, it := range items {
		if it.Name == itemNameUsable {
			idx = i
			break
		}
	}
	if idx < 0 || len(grants) == 0 {
		return items
	}
	usable := items[idx]
	if usable.Remain <= 0 {
		// 池子已空：仍要挂上**最早一笔在效发放**的到期时刻，否则「有效期」整列会随
		// 余额花完而消失（面板时有时无）。Total/Remain/Name/Key 一律原样，只补 ExpireAt。
		out := make([]provider.ResourceItem, len(items))
		copy(out, items)
		out[idx].ExpireAt = grants[0].ExpireAt
		return out
	}

	budget := usable.Remain
	split := make([]provider.ResourceItem, 0, len(grants))
	// grants 按到期升序，故从后往前 = 从最新的发放往前分配（先进先出 ⇒ 余额归最新几笔）。
	for i := len(grants) - 1; i >= 0 && budget > 0; i-- {
		g := grants[i]
		take := g.Points
		if take > budget {
			take = budget // 池子不够：这笔只承接剩下的余额
		}
		if take <= 0 {
			continue
		}
		split = append(split, provider.ResourceItem{
			Name: itemNameUsable, Total: take, Remain: take,
			ExpireAt: g.ExpireAt, Usable: usable.Usable,
		})
		budget -= take
	}
	if len(split) == 0 {
		return items
	}
	if budget > 0 {
		// 上游口径漂移（池子比流水里在效发放之和还大）：余量挂到**最新**那一笔上
		// （此刻 split[0] 即最新），保证 Remain 之和仍等于 available。
		split[0].Total += budget
		split[0].Remain += budget
	}
	// 输出按到期升序：ledger 按 key 聚合取首个非空 Expire，必须是最早那笔。
	for i, j := 0, len(split)-1; i < j; i, j = i+1, j-1 {
		split[i], split[j] = split[j], split[i]
	}

	out := make([]provider.ResourceItem, 0, len(items)-1+len(split))
	out = append(out, items[:idx]...)
	out = append(out, split...)
	out = append(out, items[idx+1:]...)
	return out
}

// UserResourceDetail 实现 provider.Upstream：返回（可用魔粒, 明细, error）。
//
// 两个附加维度都是**锦上添花**，读取失败只记日志、不影响余额刷新（与 comate 的试用额度
// 同款——附加维度失败不得把主维度搞挂）：
//   - transactions（逐笔到期）失败 ⇒ 退回「无到期信息」口径（余额仍按 available 返回）；
//   - 今日用量失败 ⇒ 不产出该条明细。
//
// 余额本身失败则原样上抛（上层保留上次快照）。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	bal, err := c.fetchBalance(a)
	if err != nil {
		return 0, nil, err
	}
	items := balanceItems(bal)
	if grants, gerr := c.fetchMagicubeGrants(a); gerr == nil {
		items = withMagicubeExpiry(items, grants)
	} else {
		// 只记渠道名与错误（零 token）。
		log.Printf("modelscope 魔粒到期查询失败（本轮退回无到期口径）platform=%s err=%v", c.Kind, gerr)
	}
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
