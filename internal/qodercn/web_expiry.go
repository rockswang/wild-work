package qodercn

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// web_expiry.go 读取 qoder.cn 网页端「积分包」的最早到期时刻。
//
// 业务 API /api/v2/quota/usage 只给 total/used/remaining，没有任何到期字段，
// 导致 pool 的最早到期时刻一直为空、qoder-cn 选号永远垫底。到期时间只在网页端
// /api/v2/me/usages/big_model_credits 按笔下发，且要网页会话 cookie。
//
// 客户端点「查看用量详情」的链路（已抓包验证）：
//   1. POST /api/v1/auth/session-transfers（Bearer deviceToken）→ 一次性 transferUrl
//   2. GET transferUrl，禁止跟随 303，从 Set-Cookie 取会话 cookie
//   3. GET big_model_credits（Cookie + CSRF 头）→ 各池 quota_detail[].expires_at
//
// ticket 一次作废，cookie 可复用一段时间。这里按 deviceToken 缓存 cookie 与
// 5 分钟内的到期结果：命中缓存直接返回；cookie 失效或 303 无 cookie 时重换一次。
// 任何一步失败都返回零值，由 UserResourceDetail 降级成「无到期信息」——网页接口
// 挂掉绝不能影响余额读取。

const (
	webExpiryTTL     = 5 * time.Minute
	webRequestTTL    = 8 * time.Second
	webTransferUA    = "QoderCN/0.5.7"
	webPageUA        = "Mozilla/5.0"
	webCSRFHeader    = "_echo_csrf_using_sec_fetch_site_"
	webCookieMaxScan = 1 << 20
)

// webSession 缓存一次网页会话。key 用 deviceToken 的哈希，避免凭据出现在内存快照里。
// 每个凭据自带 mutex：换票是串行的（ticket 一次性），但不同账号互不阻塞。
type webSession struct {
	mu      sync.Mutex
	cookie  string
	expiry  time.Time // zero = 上游没有任何有效积分包
	fetched time.Time // 上次成功读取积分包的时刻
	ok      bool      // 是否成功读取过（区分「缓存了零值」与「还没读过」）
	used    time.Time // 最近访问时刻，供淘汰用
}

func webSessionKey(dt string) string {
	sum := sha256.Sum256([]byte(dt))
	return hex.EncodeToString(sum[:8])
}

func (c *Client) webBase() string {
	if b := strings.TrimRight(strings.TrimSpace(c.Web), "/"); b != "" {
		return b
	}
	return WebBase
}

// webClient 短超时客户端：复用注入的 Transport（测试可指向 httptest），
// noRedirect 用于 GET transferUrl —— 必须自己处理 303，否则 cookie 会被吃掉。
func (c *Client) webClient(noRedirect bool) *http.Client {
	hc := &http.Client{Timeout: webRequestTTL}
	if c.HTTP != nil {
		hc.Transport = c.HTTP.Transport
	}
	if noRedirect {
		hc.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return hc
}

// earliestWebExpiry 返回最早一笔仍有剩余额度的积分包到期时刻；失败返回零值。
// 并发调用共享同一次换票（webMu 覆盖整个网络过程，避免重复消费一次性 ticket）。
func (c *Client) earliestWebExpiry(dt string) time.Time {
	if strings.TrimSpace(dt) == "" {
		return time.Time{}
	}
	// 只在建表/淘汰时持全局锁，网络过程持凭据自己的锁。
	c.webMu.Lock()
	if c.webSessions == nil {
		c.webSessions = map[string]*webSession{}
	}
	key := webSessionKey(dt)
	s := c.webSessions[key]
	if s == nil {
		s = &webSession{}
		c.webSessions[key] = s
	}
	// dt 会轮换，旧条目不会再命中；超过一天的会话直接丢掉，避免缓存无限增长。
	now := time.Now()
	for k, old := range c.webSessions {
		if k != key && now.Sub(old.used) > 24*time.Hour {
			delete(c.webSessions, k)
		}
	}
	c.webMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.used = now

	if s.ok && s.cookie != "" && time.Since(s.fetched) < webExpiryTTL {
		return s.expiry
	}

	for attempt := 0; attempt < 2; attempt++ {
		if s.cookie == "" {
			cookie, ok := c.webExchange(dt)
			if !ok {
				// 303 无 cookie：丢弃本次状态，第二轮换一张新票重试一次。
				s.ok = false
				continue
			}
			s.cookie = cookie
			s.ok = false
		}
		expiry, status, err := c.webCreditsExpiry(s.cookie)
		if err == nil {
			s.expiry, s.fetched, s.ok = expiry, time.Now(), true
			return expiry
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			// 会话过期：清 cookie，下一轮重换票（只重换一次）。
			log.Printf("qodercn web expiry: session rejected (http %d), re-exchanging", status)
			s.cookie = ""
			s.ok = false
			continue
		}
		// 网络/解析/5xx：保留 cookie 供下轮复用，本次降级为无到期信息。
		log.Printf("qodercn web expiry: credits read failed (http %d), keeping session", status)
		return time.Time{}
	}
	return time.Time{}
}

// webExchange 走第 1、2 步拿 cookie。任何错误都不回传原始 err —— 换票 URL 里带
// ticket，net/http 的错误文本会把它带出来，落进 app.log 就是凭据泄露。
func (c *Client) webExchange(dt string) (string, bool) {
	web := c.webBase()
	target := web + WebUsageTarget
	body, err := json.Marshal(map[string]string{"targetUrl": target})
	if err != nil {
		return "", false
	}
	req, err := http.NewRequest(http.MethodPost, web+EpWebSessionTransfers, bytes.NewReader(body))
	if err != nil {
		return "", false
	}
	req.Header.Set("Authorization", "Bearer "+dt)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", webTransferUA)

	resp, err := c.webClient(false).Do(req)
	if err != nil {
		log.Printf("qodercn web expiry: session transfer request failed")
		return "", false
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, webCookieMaxScan))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("qodercn web expiry: session transfer http %d", resp.StatusCode)
		return "", false
	}
	var out struct {
		TransferURL string `json:"transferUrl"`
	}
	if json.Unmarshal(raw, &out) != nil || out.TransferURL == "" {
		log.Printf("qodercn web expiry: session transfer response malformed")
		return "", false
	}
	// 只接受本站地址：ticket 是一次性凭据，绝不能发给响应里夹带的任意主机。
	if !strings.HasPrefix(out.TransferURL, web+"/") {
		log.Printf("qodercn web expiry: transfer url rejected (foreign host)")
		return "", false
	}

	hopReq, err := http.NewRequest(http.MethodGet, out.TransferURL, nil)
	if err != nil {
		return "", false
	}
	hopReq.Header.Set("User-Agent", webPageUA)
	hop, err := c.webClient(true).Do(hopReq)
	if err != nil {
		log.Printf("qodercn web expiry: transfer redirect request failed")
		return "", false
	}
	_ = hop.Body.Close()
	cookie := joinSetCookies(hop.Header.Values("Set-Cookie"))
	if cookie == "" {
		log.Printf("qodercn web expiry: transfer http %d returned no cookie", hop.StatusCode)
		return "", false
	}
	return cookie, true
}

// joinSetCookies 取每段 Set-Cookie 的 name=value，多个用 "; " 拼成请求头。
func joinSetCookies(values []string) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		first := strings.TrimSpace(strings.SplitN(v, ";", 2)[0])
		if first != "" && strings.Contains(first, "=") {
			parts = append(parts, first)
		}
	}
	return strings.Join(parts, "; ")
}

// webCreditsExpiry 第 3 步：读积分包并取最早到期时刻。
func (c *Client) webCreditsExpiry(cookie string) (time.Time, int, error) {
	web := c.webBase()
	req, err := http.NewRequest(http.MethodGet, web+EpWebCredits, nil)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("request build failed")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Referer", web+WebUsageTarget)
	req.Header.Set("x-csrf-token", webCSRFHeader)
	req.Header.Set("x-requested-with", "XMLHttpRequest")
	req.Header.Set("User-Agent", webPageUA)

	resp, err := c.webClient(false).Do(req)
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return time.Time{}, resp.StatusCode, fmt.Errorf("http %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, webCookieMaxScan))
	if err != nil {
		return time.Time{}, resp.StatusCode, fmt.Errorf("read failed")
	}
	return parseWebExpiry(raw), resp.StatusCode, nil
}

// parseWebExpiry 扫所有 *_quota 池的 quota_detail，取 remaining_value > 0 且
// expires_at > 0 的最小 expires_at（毫秒时间戳）。expires_at=0 表示不过期，跳过。
func parseWebExpiry(raw []byte) time.Time {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return time.Time{}
	}
	// 池可能直接挂在顶层，也可能包一层 {"data": {...}}；两种都扫。
	scopes := []map[string]json.RawMessage{top}
	if d, exists := top["data"]; exists {
		var nested map[string]json.RawMessage
		if json.Unmarshal(d, &nested) == nil {
			scopes = append(scopes, nested)
		}
	}
	var minMS int64
	for _, scope := range scopes {
		for _, poolRaw := range scope {
			var pool struct {
				QuotaDetail []struct {
					RemainingValue json.RawMessage `json:"remaining_value"`
					ExpiresAt      json.RawMessage `json:"expires_at"`
				} `json:"quota_detail"`
			}
			if json.Unmarshal(poolRaw, &pool) != nil {
				continue
			}
			for _, d := range pool.QuotaDetail {
				left := rawNumber(d.RemainingValue)
				end := rawNumber(d.ExpiresAt)
				if left <= 0 || end <= 0 {
					continue
				}
				if minMS == 0 || end < minMS {
					minMS = end
				}
			}
		}
	}
	if minMS <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(minMS)
}

// rawNumber 兼容上游把数字写成 JSON number 或 string 两种形态。
func rawNumber(raw json.RawMessage) int64 {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return 0
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(f)
	}
	return 0
}

// webExpireAtString 把到期时刻格式化成 RFC3339（UTC+8），供 ResourceItem.ExpireAt 使用。
func webExpireAtString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(time.FixedZone("CST", 8*3600)).Format(time.RFC3339)
}
