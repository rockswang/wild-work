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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// web_expiry.go 通过 qoder.cn 网页会话读取每笔积分包余额与到期时间。

const (
	webRequestTTL    = 8 * time.Second
	webTransferUA    = "QoderCN/0.5.7"
	webPageUA        = "Mozilla/5.0"
	webCSRFHeader    = "_echo_csrf_using_sec_fetch_site_"
	webCookieMaxScan = 1 << 20
)

// 每个凭据独立加锁，避免重复消费一次性 ticket；key 不保存原始 deviceToken。
type webSession struct {
	mu     sync.Mutex
	cookie string
	used   time.Time
}

type webCreditPack struct {
	pool      string
	total     int64
	used      int64
	remain    int64
	expiresAt int64
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

// 每次刷新都重新读取积分包，仅复用 cookie，避免沿用旧余额或旧到期时间。
func (c *Client) webCreditPacks(dt string) []webCreditPack {
	if strings.TrimSpace(dt) == "" {
		return nil
	}
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

	for attempt := 0; attempt < 2; attempt++ {
		if s.cookie == "" {
			cookie, ok := c.webExchange(dt)
			if !ok {
				continue
			}
			s.cookie = cookie
		}
		packs, status, err := c.webCredits(s.cookie)
		if err == nil {
			return packs
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			log.Printf("qodercn web expiry: session rejected (http %d), re-exchanging", status)
			s.cookie = ""
			continue
		}
		log.Printf("qodercn web expiry: credits read failed (http %d), keeping session", status)
		return nil
	}
	return nil
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

func (c *Client) webCredits(cookie string) ([]webCreditPack, int, error) {
	web := c.webBase()
	req, err := http.NewRequest(http.MethodGet, web+EpWebCredits, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("request build failed")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Referer", web+WebUsageTarget)
	req.Header.Set("x-csrf-token", webCSRFHeader)
	req.Header.Set("x-requested-with", "XMLHttpRequest")
	req.Header.Set("User-Agent", webPageUA)

	resp, err := c.webClient(false).Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("http %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, webCookieMaxScan))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read failed")
	}
	return parseWebCreditPacks(raw), resp.StatusCode, nil
}

// 保留所有有余额的包；expires_at=0 表示不过期，不能漏掉，否则无法核对聚合余额。
func parseWebCreditPacks(raw []byte) []webCreditPack {
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return nil
	}
	scopes := []map[string]json.RawMessage{top}
	if d, exists := top["data"]; exists {
		var nested map[string]json.RawMessage
		if json.Unmarshal(d, &nested) == nil {
			scopes = append(scopes, nested)
		}
	}
	var packs []webCreditPack
	for _, scope := range scopes {
		names := make([]string, 0, len(scope))
		for name := range scope {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if name == "total_quota" {
				continue // 汇总副本，逐包明细已包含同样的额度
			}
			var pool struct {
				QuotaDetail []struct {
					LimitValue     json.RawMessage `json:"limit_value"`
					TotalValue     json.RawMessage `json:"total_value"`
					UsedValue      json.RawMessage `json:"used_value"`
					RemainingValue json.RawMessage `json:"remaining_value"`
					ExpiresAt      json.RawMessage `json:"expires_at"`
				} `json:"quota_detail"`
			}
			if json.Unmarshal(scope[name], &pool) != nil {
				continue
			}
			for _, d := range pool.QuotaDetail {
				remain := rawNumber(d.RemainingValue)
				if remain <= 0 {
					continue
				}
				total := rawNumber(d.TotalValue)
				if total <= 0 {
					total = rawNumber(d.LimitValue)
				}
				packs = append(packs, webCreditPack{
					pool:      name,
					total:     total,
					used:      rawNumber(d.UsedValue),
					remain:    remain,
					expiresAt: rawNumber(d.ExpiresAt),
				})
			}
		}
	}
	return packs
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
