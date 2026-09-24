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
}

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

// expired 判定条目到期日是否已到（≤今天，UTC+8 墙钟）。
// ExpireAt 为空串（渠道未下发，如 Qoder）恒 false → 下降走 spend 口径。
func expired(expireAt string, now time.Time) bool {
	if expireAt == "" {
		return false
	}
	t, err := time.ParseInLocation("2006-01-02", expireAt, expireLoc)
	if err != nil {
		return false
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, expireLoc)
	return !t.After(today)
}

// DiffCredits 余额刷新后差分记账：cur 为本次 UserResourceDetail 拿到的条目，
// balance 为本次可消耗余额（事件后的 balance_after）。
// 与快照逐条目对比，产生 0..n 条流水并更新快照；返回事件数。
// 只统计 Usable=true 的条目——不可消耗池（如 TraeWork ep=1）本工具动不到，
// 其变动与路由无关，混入会污染 earn/spend 口径。
func (l *Ledger) DiffCredits(ch, uid string, balance int64, cur []provider.ResourceItem) int {
	now := time.Now()
	sk := ch + "/" + uid
	l.mu.Lock()
	snapAll := l.loadSnapshots()
	prev := snapAll[sk]
	prevMap := make(map[string]itemSnap, len(prev))
	for _, p := range prev {
		prevMap[p.Key] = p
	}

	var events []CreditEntry
	firstTime := len(prev) == 0 // 首次见此账号：只记一条汇总，避免存量条目刷屏

	// 按 key 聚合本次条目（issue #38）：伪键不唯一时多条独立套餐共享一个 key，
	// 必须先合并成一条再与快照差分，保证每 key 每次刷新最多一条事件。
	type aggEnt struct {
		name, expire string
		remain       int64
	}
	agg := map[string]*aggEnt{} // key → 聚合条目
	var order []string          // 首现顺序遍历，事件输出稳定可测
	for _, it := range cur {
		if !it.Usable {
			continue
		}
		k := itemKey(it)
		a := agg[k]
		if a == nil {
			a = &aggEnt{name: it.Name, expire: it.ExpireAt}
			agg[k], order = a, append(order, k)
		}
		a.remain += it.Remain // 同 key 多条余额求和，视作一个整体
		if a.expire == "" {
			a.expire = it.ExpireAt // 到期日取首个非空（同 key 通常相同）
		}
	}

	curMap := map[string]itemSnap{}
	for _, k := range order {
		a := agg[k]
		curMap[k] = itemSnap{Key: k, Remain: a.remain, Expire: a.expire}
		p, ok := prevMap[k]
		if !ok {
			// 无快照的存量条目：不在首次差分逐条展开（会把存量余额刷屏成假 earn），
			// 仅后续刷新中新增的条目才逐条记 earn。
			continue
		}
		delta := a.remain - p.Remain
		switch {
		case delta > 0:
			events = append(events, CreditEntry{Ch: ch, UID: uid, Kind: "earn",
				Amount: delta, Balance: balance, Note: a.name})
		case delta < 0:
			// 下降：到期日已过优先归因过期（签到奖励等当天到期条目的典型形态），
			// 否则归因消耗。
			kind := "spend"
			if expired(p.Expire, now) {
				kind = "expire"
			}
			events = append(events, CreditEntry{Ch: ch, UID: uid, Kind: kind,
				Amount: delta, Balance: balance, Note: a.name})
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
