package minimax

// expiry.go 读取积分批次的到期时刻。
//
// /minimax-cloud/api/v1/credit/details 返回的是**按批**的积分账本：每批带
// credit_type（1 购买 / 2 签到）、granted_amount、remaining_amount、
// expire_at_ms。到期时间只在这一层有，登录 / 余额接口都不带；不读它，
// 账号的 earliest_expire 恒为空，选号永远排在「有到期信息」的账号之后。
//
// 口径按 MiniMax Code 客户端的「积分明细」页（Settings → Usage → Credit details）：
// 只统计 credit_type 1/2、剩余 > 0 且未过期的批次，取最小的 expire_at_ms。
// 账本分页读取（page_size 20，最多 10 页）；任何一页读不全就整体放弃 ——
// 只看到半份账本会算出偏早的到期日。读失败一律降级为空，不影响余额读取。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"wild-work/internal/auth"
)

const (
	// creditLedgerPage 单页批次条数（与官方客户端一致）。
	creditLedgerPage = 20
	// creditLedgerPages 最多翻页数：10×20 足够覆盖正常账号的账本。
	creditLedgerPages = 10
	// creditExpiryTTL 到期结果缓存时长。到期日是慢变量，缓存只为挡住
	// 「hover 明细 / 手动刷新」这类高频调用，避免反复翻十页账本。
	creditExpiryTTL = 5 * time.Minute
)

// creditExpiryEntry 一条到期缓存。ok 区分「读到但无有效批次」与「没读到」：
// 前者也缓存，避免零积分账号每次刷新都重翻账本。
type creditExpiryEntry struct {
	expires time.Time
	fetched time.Time
	ok      bool
}

func expiryCacheKey(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:8])
}

// creditExpiry 返回最早一笔仍然有效且有剩余的积分批次到期时刻；读不到返回零值。
func (c *Client) creditExpiry(a *auth.Auth) time.Time {
	tok := tokenOf(a)
	if tok == "" {
		return time.Time{}
	}
	key := expiryCacheKey(tok)
	c.expMu.Lock()
	if c.expiries == nil {
		c.expiries = map[string]creditExpiryEntry{}
	}
	if e, ok := c.expiries[key]; ok && e.ok && time.Since(e.fetched) < creditExpiryTTL {
		c.expMu.Unlock()
		return e.expires
	}
	c.expMu.Unlock()

	expires, read := c.fetchCreditExpiry(tok)
	c.expMu.Lock()
	if c.expiries == nil {
		c.expiries = map[string]creditExpiryEntry{}
	}
	if read {
		// 读全了才缓存（到期可为零值：账号还没有有效批次）。读失败不缓存，
		// 下一轮刷新立刻重试，而不是把空值钉住 5 分钟。
		c.expiries[key] = creditExpiryEntry{expires: expires, fetched: time.Now(), ok: true}
	} else {
		delete(c.expiries, key)
	}
	c.expMu.Unlock()
	return expires
}

// fetchCreditExpiry 翻页读完整账本并取最早到期；第二个返回值表示账本是否读全。
func (c *Client) fetchCreditExpiry(tok string) (time.Time, bool) {
	now := time.Now()
	total := -1
	seen := 0
	var earliest time.Time
	for page := 1; page <= creditLedgerPages; page++ {
		status, raw, err := c.getJSON(c.apiHost()+EpCreditDetails+"?"+creditLedgerQuery(page), tok)
		if err != nil || status >= 400 {
			return time.Time{}, false
		}
		top, perr := decodeObject(raw)
		if perr != nil {
			return time.Time{}, false
		}
		body := businessObject(top)
		if code, _, ok := baseResp(body, top); ok && code != 0 {
			return time.Time{}, false
		}
		if total < 0 {
			rawTotal, has := body["total_count"]
			if !has {
				rawTotal, has = top["total_count"]
			}
			if !has {
				return time.Time{}, false // 没有总数就无法判断账本是否读完
			}
			total = int(intOfRaw(rawTotal))
			if total <= 0 {
				return time.Time{}, true // 空账本：合法结果，只是没有到期
			}
		}
		details := detailsArray(body)
		if len(details) == 0 {
			break
		}
		seen += len(details)
		if t, ok := earliestLiveExpiry(details, now); ok && (earliest.IsZero() || t.Before(earliest)) {
			earliest = t
		}
		if seen >= total {
			break
		}
	}
	if seen < total {
		return time.Time{}, false // 账本没读全：半份账本会算错份额
	}
	return earliest, true
}

// creditLedgerQuery 积分明细的查询参数（与官方客户端的明细页同形）。
func creditLedgerQuery(page int) string {
	q := url.Values{}
	q.Set("device_platform", "web")
	q.Set("is_desktop", "1")
	q.Set("desktop_version", "0.6.12")
	q.Set("page_number", strconv.Itoa(page))
	q.Set("page_size", strconv.Itoa(creditLedgerPage))
	q.Set("include_expired", "true")
	return q.Encode()
}

// earliestLiveExpiry 从一批批次条目里取最早到期：只认 credit_type 1/2、
// 剩余 > 0 且 expires_at > now 的批次。
func earliestLiveExpiry(details []map[string]json.RawMessage, now time.Time) (time.Time, bool) {
	nowMS := now.UnixMilli()
	var minMS int64
	for _, d := range details {
		switch intOfRaw(d["credit_type"]) {
		case 1, 2: // 1 购买 / 2 签到；其它类型语义未知，不参与
		default:
			continue
		}
		if rest, ok := ledgerAmount(d["remaining_amount"]); !ok || rest <= 0 {
			continue
		}
		end := intOfRaw(d["expire_at_ms"])
		if end <= 0 || end <= nowMS {
			continue // 0/缺失 = 不过期或未知；已过期的批次不计
		}
		if minMS == 0 || end < minMS {
			minMS = end
		}
	}
	if minMS == 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(minMS), true
}

// ledgerAmount 解析账本金额：十进制字符串（可能带千分位逗号），负数按 0 处理。
// 与 amountOf 分开是刻意的 —— 后者服务于余额汇总的既有口径，这里只用于筛选，
// 解析不出来的批次直接跳过，不改变余额结果。
func ledgerAmount(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	} else {
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil {
			return 0, false
		}
		return f, true
	}
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	if f < 0 {
		return 0, true
	}
	return f, true
}

// expireAtString 到期时刻 → RFC3339（UTC+8），供 ResourceItem.ExpireAt 使用。
func expireAtString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(time.FixedZone("CST", 8*3600)).Format(time.RFC3339)
}
