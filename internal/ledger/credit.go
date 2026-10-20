// credit.go 积分条目快照差分：把每次余额刷新看到的 ResourceItem 列表与上次快照对比，
// 产出 earn/spend/expire 三类流水事件。
//
// 分类规则（与讨论定稿一致，不追求逐笔精确）：
//   - 同 key 条目余额上升          → earn（签到/活动发放/套餐充值）
//   - 同 key 条目余额下降且到期日已过 → expire（到期作废）
//   - 同 key 条目余额下降且未到期    → spend（对话消耗）
//   - 上次有、本次消失的 key        → expire（套餐/赠送包到期后从明细里整体移除是主因）
//   - 本次新出现的 key             → earn（新发放的套餐/赠送包）
//
// 同 key 多条目先聚合求和再差分（issue #38）：WorkBuddy 的「套餐名|到期日」伪键
// 可能对应多条独立套餐，逐条差分会每条都对比同一份旧快照 → 一次刷新重复记
// spend、快照只留末条下次继续错。
//
// key 来源：ResourceItem.Key（渠道设置的稳定标识，如 TraeWork entitlement_id），
// 缺失时退回 Name。快照持久化在 ledger 目录的 credit-snapshot.json（非 state 文件，
// 可随时删除——丢了只是下次差分把全部条目误记一次 earn，无功能影响）。
package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wild-work/internal/provider"
)

// itemSnap 快照里的单条目（只存差分所需字段）。
type itemSnap struct {
	Key    string `json:"k"`
	Remain int64  `json:"r"`
	Expire string `json:"e,omitempty"` // YYYY-MM-DD（UTC+8 墙钟口径）
	// Used 已消耗量（issue #67）：delta=0 时用于识别「发放即消耗」形态——
	// WorkBuddy 每日签到包提前一天以 r=0 建档，发放与消耗落在同一刷新窗内时
	// remain 与快照持平，earn 只能靠 used 差分救回。旧快照无此字段 → 零值，安全。
	Used int64 `json:"u,omitempty"`
	// Total 池子总量（R101）：remain 下降时用来区分「池子缩水（整包到期/回收）」
	// 与「用掉更多」——remain = total - used 是三者的恒等式，故
	// Δremain = ΔTotal - ΔUsed。旧快照无此字段 → 零值，配合 UsedEver 门槛
	// 保证老数据一律退回旧口径（不变量 5 向后兼容）。
	Total int64 `json:"t,omitempty"`
}

// snapChannels 快照文件里保存「渠道能力」的保留键。真实账号键恒为
// "ch/uid"（含斜杠），故 "@ch" 不会与之冲突。
//
// 为什么能力记在渠道级而不是账号/key 级（R101 第二轮修正，2026-10-19）：
// 「这个渠道的 total 是真面额还是 remain 的拷贝」是**渠道属性**。按单 key 判会把
// 「从不花钱、只签到」的账号（QoderCN 实录：019fe484 的 total=2300 正好等于累计
// 发放，used 恒 0）误当成「不区分 total/used」的渠道，它的整包到期就仍会记成消耗。
// 反过来把整个渠道一次性标记成是 or 否：只要该渠道**任一账号任一 key** 花过一次
// （used>0），就能证明它会给 total 与 used 分别赋值，全渠道永久受益。
const snapChannels = "@ch"

// snapshots credit-snapshot.json 结构：key "ch/uid" → 条目列表。
type snapshots map[string][]itemSnap

// expireLoc 与 provider.ExpiringWithin 同口径：固定 UTC+8 墙钟。
var expireLoc = time.FixedZone("UTC+8", 8*60*60)

// loadSnapshots 读快照文件（不存在/损坏返回空表，不阻塞）。
func (l *Ledger) loadSnapshots() snapshots {
	out := snapshots{}
	raw, err := os.ReadFile(filepath.Join(l.dir, "credit-snapshot.json"))
	if err == nil {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// saveSnapshots 原子写快照文件。
func (l *Ledger) saveSnapshots(s snapshots) {
	raw, err := json.Marshal(s)
	if err != nil {
		return
	}
	fp := filepath.Join(l.dir, "credit-snapshot.json")
	tmp := fp + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, fp)
	}
}

// itemKey 条目差分键：优先渠道提供的稳定 Key（如 entitlement_id），退回 Name。
func itemKey(it provider.ResourceItem) string {
	if it.Key != "" {
		return it.Key
	}
	return it.Name
}

// expired 判定条目是否**已失效**（UTC+8 墙钟）。
// 统一走 provider.ExpireInstant，两种 ExpireAt 写法各按自己的语义：
//   - "YYYY-MM-DD"：该日全天可用，**次日 00:00** 才失效 ⇒ 当天不算过期；
//   - RFC3339：精确时刻，到点即失效。
//
// 历史坑（2026-10-18 生产实录，两个方向都错）：旧实现用 `!t.After(today)` 且只认
// 日期格式 —— 一方面把「到期日 = 今天」判成已过期（当日有效池的消耗被记成 expire，
// loomy 每日积分 / qwenwork 当日钱包都中招）；另一方面对 gemini 的 RFC3339 解析失败后
// 恒 false（被当成「永不过期」）。两处都在 provider.ExpireInstant 收口。
//
// ExpireAt 为空串（渠道未下发，如 Qoder）或不可解析时恒 false → 下降走 spend 口径。
func expired(expireAt string, now time.Time) bool {
	t, ok := provider.ExpireInstant(expireAt)
	if !ok {
		return false
	}
	return !now.In(expireLoc).Before(t)
}

// poolShrankExpiry 报告「余额下降」是否由**池子缩水**而非消耗引起（R101）。
//
// 背景（2026-10-19 生产实录，用户报「QoderCN 今日消耗 99 分但我没咋用」）：
// QoderCN 一个 100 分的赠送包整包到期消失，而它此前已被花掉 1 分。上游回包
// total 2500→2400、used 1→0、remaining 2499→2400 ⇒ 账本按「余额下降 + 无到期日」
// 记成 spend(-99)，面板显示成「今日消耗」，实际当天真消耗是 0。
//
// 判据用的是恒等式 remain = total - used 拆项后的 Δremain = Δtotal - Δused：
// 「花了钱」是 used 变大（total 不动），「池子被回收/到期」是 total 变小（used 可能同步
// 归零）。三门槛**缺一不可**：
//
//  1. 该条目**未上报到期日**：有到期日的渠道走 expired() 既有判据（那一路本来就对），
//     本条只接管「上游不给包级到期时间，账本无从判过期」的盲区，对既有渠道零影响；
//  2. 该**渠道**被证实会给 total / used 分别赋值（channelBooksUsage）：qwenwork/
//     raccoon/minimax/monkeycode/loomy/glm 六个渠道把 Total 填成 Remain、Used 恒 0
//     （见各自 UserResourceDetail），那种形态下每次真实消耗都会让 Total 一起下降——
//     无此门槛就会把它们的消耗全记成过期。判定按**渠道**而不是按 key：只签到不花钱的
//     账号（QoderCN 实录 019fe484，used 恒 0，total 2300 = 累计发放）也会整包到期，
//     按 key 判会把它误归成「不记账渠道」而漏修；反过来只要该渠道任一账号任一 key
//     花过一次，就证明上游确实分别赋值，全渠道永久受益；
//  3. **total 确实下降了**：且上次快照必须有 total（老快照无 t 字段 = 0，自动退回旧口径，
//     保证不变量 5「只增不减」的向后兼容——升级后首轮不改写历史归因）。
//
// 只在 remain 已经确认下降时调用，故这里不再复核 delta。
func poolShrankExpiry(prev itemSnap, curTotal int64, booksUsage bool) bool {
	if prev.Expire != "" { // 门槛 1
		return false
	}
	if !booksUsage { // 门槛 2
		return false
	}
	return prev.Total > 0 && curTotal < prev.Total // 门槛 3
}

// channelBooksUsage 报告该渠道是否**分别上报** total 与 used（即 total 是真面额，
// 而非 remain 的拷贝）。一个渠道只要有任意账号任意 key 出现过 used>0，就证明了这一点
// ——这是渠道级事实，一旦为真永久有效（持久化在快照的 "@ch" 保留键里）。
// 详见 poolShrankExpiry 门槛 2。
func channelBooksUsage(snapAll snapshots, ch string) bool {
	return len(snapAll[snapChannels+"/"+ch]) > 0
}

// DiffCredits 余额刷新后差分记账：cur 为本次 UserResourceDetail 拿到的条目，
// balance 为本次可消耗余额（事件后的 balance_after）。
// 与快照逐条目对比，产生 0..n 条流水并更新快照；返回事件数。
// 只统计 Usable=true 的条目——不可消耗池（如 TraeWork ep=1）本工具动不到，
// 其变动与路由无关，混入会污染 earn/spend 口径。
// InfoOnly 条目同样跳过：单位不同的另一套额度（如 MonkeyCode 每日 Token），
// 记进积分流水会让余额对不上账。
func (l *Ledger) DiffCredits(ch, uid string, balance int64, cur []provider.ResourceItem) int {
	now := time.Now()
	sk := ch + "/" + uid
	l.mu.Lock()
	snapAll := l.loadSnapshots()
	prev := snapAll[sk]
	prevMap := make(map[string]itemSnap, len(prev))
	// R101 门槛 2：渠道是否分别上报 total / used。
	// 两个来源取并：① 快照里的 "@ch" 保留键（历年累积的信道级事实）；
	// ② 本账号旧快照里留下的 used>0（无 "@ch" 标记时代的铁证，向后兼容）。
	booksUsage := channelBooksUsage(snapAll, ch)
	for _, p := range prev {
		prevMap[p.Key] = p
		if p.Used > 0 {
			booksUsage = true
		}
	}

	var events []CreditEntry
	firstTime := len(prev) == 0 // 首次见此账号：只记一条汇总，避免存量条目刷屏

	// 按 key 聚合本次条目（issue #38）：伪键不唯一时多条独立套餐共享一个 key，
	// 必须先合并成一条再与快照差分，保证每 key 每次刷新最多一条事件。
	type aggEnt struct {
		name, expire string
		remain, used int64
		total        int64
	}
	agg := map[string]*aggEnt{} // key → 聚合条目
	var order []string          // 首现顺序遍历，事件输出稳定可测
	for _, it := range cur {
		if !it.Usable || it.InfoOnly {
			continue
		}
		k := itemKey(it)
		a := agg[k]
		if a == nil {
			a = &aggEnt{name: it.Name, expire: it.ExpireAt}
			agg[k], order = a, append(order, k)
		}
		a.remain += it.Remain // 同 key 多条余额求和，视作一个整体
		a.used += it.Used
		a.total += it.Total
		if it.Used > 0 {
			booksUsage = true // 本次就发现「分别赋值」的铁证，立即信道内生效
		}
		if a.expire == "" {
			a.expire = it.ExpireAt // 到期日取首个非空（同 key 通常相同）
		}
	}

	curMap := map[string]itemSnap{}
	for _, k := range order {
		a := agg[k]
		p, ok := prevMap[k]
		curMap[k] = itemSnap{Key: k, Remain: a.remain, Expire: a.expire, Used: a.used, Total: a.total}
		if !ok {
			if firstTime {
				// 首次见此账号：存量条目由末尾的「存量额度」baseline 统一汇总，
				// 不在此逐条展开（会把存量余额刷屏成假 earn）。
				continue
			}
			// 非首次出现的新 key = 真实新增发放 → 逐条记 earn。
			// 修复少记（2026-10-02 生产账本实证）：WorkBuddy 的伪键是「套餐名|到期日」，
			// 月周期切换时旧键消失（已记 expire）、同余额挂在新键上——此前这里静默跳过，
			// 导致每账号每月约 2500 的周期发放从不进入收入流水，账本只降不升。
			// 条目抖动（上游瞬时缺失后回归）会产出 expire+earn 一对，净额不变，
			// 优于旧行为「回归被吞、收入永久少记」。
			if a.remain > 0 {
				events = append(events, CreditEntry{Ch: ch, UID: uid, Kind: "earn",
					Amount: a.remain, Balance: balance, Note: a.name})
			}
			continue
		}
		delta := a.remain - p.Remain
		switch {
		case delta > 0:
			events = append(events, CreditEntry{Ch: ch, UID: uid, Kind: "earn",
				Amount: delta, Balance: balance, Note: a.name})
		case delta < 0:
			// 下降：到期日已过优先归因过期（签到奖励等当天到期条目的典型形态），
			// 否则归因消耗
			// 除非池子本身缩小了（R101）——见 poolShrankExpiry。
			kind := "spend"
			if expired(p.Expire, now) || poolShrankExpiry(p, a.total, booksUsage) {
				kind = "expire"
			}
			events = append(events, CreditEntry{Ch: ch, UID: uid, Kind: kind,
				Amount: delta, Balance: balance, Note: a.name})
		case a.used > p.Used:
			// remain 持平但 used 上涨（issue #67）：发放与消耗落在同一刷新窗内，
			// 差分把签到发放误判为「条目没变」。由 remain=total-used 可知
			// 发放额 = usedDelta（total 同量增加），记 earn 保住收入；对应的
			// 消耗不另记 spend（两次快照间无法拆分先后，净额守恒）。used 恒 0
			// 的渠道（如 qwenwork）自然不触发，零影响。
			events = append(events, CreditEntry{Ch: ch, UID: uid, Kind: "earn",
				Amount: a.used - p.Used, Balance: balance, Note: a.name})
		}
	}
	// 上次有、本次消失：套餐到期移除是主因，归因过期。
	for k, p := range prevMap {
		if _, ok := curMap[k]; !ok && p.Remain > 0 {
			events = append(events, CreditEntry{Ch: ch, UID: uid, Kind: "expire",
				Amount: -p.Remain, Balance: balance, Note: k})
		}
	}

	// 首次快照：存量余额整体记一条 earn（baseline），不逐条展开
	if firstTime {
		sum := int64(0)
		for _, v := range curMap {
			sum += v.Remain
		}
		if sum > 0 {
			events = append([]CreditEntry{{Ch: ch, UID: uid, Kind: "earn", Amount: sum, Balance: balance, Note: "存量额度"}}, events...)
		}
	}

	// 更新快照（curMap 仅含 Usable 条目；空表删除该账号条目）。
	if len(curMap) == 0 {
		delete(snapAll, sk)
	} else {
		list := make([]itemSnap, 0, len(curMap))
		for _, v := range curMap {
			list = append(list, v)
		}
		snapAll[sk] = list
	}
	// 渠道级「分别上报 total/used」标记（R101 门槛 2）：一旦证实就永久保留，
	// 供同渠道其他账号（如只签到不花钱、used 恒 0 的号）共享。
	if booksUsage {
		snapAll[snapChannels+"/"+ch] = []itemSnap{{Key: ch}}
	}
	l.saveSnapshots(snapAll)
	l.mu.Unlock()

	for _, e := range events {
		l.AppendCredit(e)
	}
	return len(events)
}

// migrateDupKeyCredit 一次性归档旧版重复 key 差分产生的错误积分流水（issue #38）。
// 重复生成的 spend 与真实消耗混写进同一条流水，事后无法区分真伪，故整段归档为
// old-credit-*.jsonl（不进任何查询口径，随 cleanup 保留窗口到期自清）；同时删快照
// 让下次刷新按聚合口径重建 baseline。marker（.credit-dedup-migrated）存在即跳过。
func (l *Ledger) migrateDupKeyCredit() {
	mk := filepath.Join(l.dir, ".credit-dedup-migrated")
	if _, err := os.Stat(mk); err == nil {
		return
	}
	ents, _ := os.ReadDir(l.dir)
	for _, e := range ents {
		if n := e.Name(); strings.HasPrefix(n, "credit-") && strings.HasSuffix(n, ".jsonl") {
			_ = os.Rename(filepath.Join(l.dir, n), filepath.Join(l.dir, "old-"+n))
		}
	}
	// 旧快照对重复 key 只留末条值，与聚合后的 cur 差分会凭空多记一次 earn，一并删除重建
	_ = os.Remove(filepath.Join(l.dir, "credit-snapshot.json"))
	_ = os.WriteFile(mk, []byte("1"), 0o600)
}
