package chatgpt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"wild-work/internal/auth"
)

// sentinel token 预取缓存（账号级，落盘）。
//
// 事实依据（参考实现抓包 2026-10-26）：
//   - finalize 下发的 token 名义 TTL 是 **540 秒**，但**只能用一次**，复用即
//     403 `{"detail":"Unusual activity has been detected from your device"}`；
//   - 真实网页在**对话请求发出后 1 秒**（流还没结束）就已经跑完下一轮的 finalize；
//   - 也就是说浏览器并不是"每轮现场跑 5 步"，而是**并行预取 + 一次性消费**。
//
// 所以本文件复刻的是浏览器行为，不是发明：
//  1. 对话流式进行中，并行跑一遍 L1→L3，把 token 落盘（`Prefetch`）；
//  2. 下次调用直接消费落盘 token（**读后立即删**，保证一次性语义）；
//  3. 消费时校验 base/UA 与指纹新鲜度，避免拿到「换基址/换身份」后的旧票。
//
// 保守约束：只接受「预取后 TokenMaxAge 内」的 token。浏览器是人在几秒到几分钟内
// 才按发送；把窗口压到 120 秒，宁可多跑一次也不拿过期票去撞风控。

// TokenMaxAge 是落盘 token 的最大可用年龄（秒）。
//
// 名义 TTL 540 秒，这里只认 120 秒：预取到使用之间的间隔越短越像真人，
// 也留足代理抖动与进程启动的余量。
const TokenMaxAge = 120

// tokenCache 是落盘形态。
type tokenCache struct {
	Key       string `json:"key"`
	Base      string `json:"base"`
	UA        string `json:"ua"`
	DeviceID  string `json:"device_id"`
	DataBuild string `json:"data_build"`
	ReqToken  string `json:"req_token"`
	Proof     string `json:"proof"`
	Turnstile string `json:"turnstile"`
	FetchedAt int64  `json:"fetched_at"` // Unix 秒（预取时刻）
	TokenTTL  int64  `json:"token_ttl"`  // 上游给的名义 TTL
	DXOps     int    `json:"dx_ops"`
	DXAlias   int    `json:"dx_alias"`
	VMSteps   int    `json:"vm_steps"`
	PowMs     int64  `json:"pow_ms"`
	// 预取时的分段耗时，用于消费时回填诊断。
	HomeMs, PrepareMs, FinalizeMs int64
}

// tokenCachePath 返回 <data>/cache/sentinel-<key>.json（spec §8 的落盘路径）。
func (c *Client) tokenCachePath(a *auth.Auth) string {
	key := sanitizeKey(accountKey(a))
	if key == "" {
		return ""
	}
	return c.cachePath("sentinel-" + key + ".json")
}

// sanitizeKey 把账号 key 压成文件名安全形态（只留字母数字与 -_）。
func sanitizeKey(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}

// takeTokenCache 尝试消费落盘 token（**读后立即删**，保证只用一次）。
//
// 返回 ok=false 表示没有可用缓存，调用方应走完整 L1→L3。
func (c *Client) takeTokenCache(a *auth.Auth) (*Sentinel, bool) {
	path := c.tokenCachePath(a)
	if path == "" {
		return nil, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	// 无论内容是否可用，先删掉：token 一旦被读到就不能再给别人用。
	_ = os.Remove(path)

	var tc tokenCache
	if err := json.Unmarshal(b, &tc); err != nil {
		return nil, false
	}
	ua, _ := c.cfgUA()
	if tc.ReqToken == "" || tc.Base != c.baseURL() || tc.UA != ua {
		return nil, false
	}
	age := time.Since(time.Unix(tc.FetchedAt, 0)).Seconds()
	if age < 0 || age > TokenMaxAge {
		return nil, false // 太旧：宁可重跑，也不拿过期票撞风控
	}
	// 预取 token 必须伴随一次新鲜的指纹校验（预取本身跑了完整 L1→L3，含指纹对比）。
	// 否则删了指纹文件/改了指纹基线后，光靠缓存就能绕过升级预警。
	if !c.fingerprintFresh(a) {
		return nil, false
	}
	return &Sentinel{
		Home:        &HomeInfo{Status: 200, DataBuild: tc.DataBuild},
		ReqToken:    tc.ReqToken,
		Proof:       tc.Proof,
		Turnstile:   tc.Turnstile,
		DXOps:       tc.DXOps,
		DXAlias:     tc.DXAlias,
		VMSteps:     tc.VMSteps,
		PowElapsed:  tc.PowMs,
		HomeMs:      tc.HomeMs,
		PrepareMs:   tc.PrepareMs,
		FinalizeMs:  tc.FinalizeMs,
		TokenTTL:    tc.TokenTTL,
		TokenCached: true,
		CacheAgeSec: int64(age),
	}, true
}

// storeTokenCache 把一次预取结果落盘（原子写 0600）。
func (c *Client) storeTokenCache(a *auth.Auth, s *Sentinel) {
	path := c.tokenCachePath(a)
	if path == "" || s == nil || s.ReqToken == "" {
		return
	}
	sess, err := c.session(a)
	if err != nil {
		return
	}
	ua, _ := c.cfgUA()
	tc := tokenCache{
		Key: sanitizeKey(accountKey(a)), Base: c.baseURL(), UA: ua, DeviceID: sess.device(),
		DataBuild: homeBuild(s),
		ReqToken:  s.ReqToken, Proof: s.Proof, Turnstile: s.Turnstile,
		FetchedAt: time.Now().Unix(), TokenTTL: s.TokenTTL,
		DXOps: s.DXOps, DXAlias: s.DXAlias, VMSteps: s.VMSteps, PowMs: s.PowElapsed,
		HomeMs: s.HomeMs, PrepareMs: s.PrepareMs, FinalizeMs: s.FinalizeMs,
	}
	b, err := json.Marshal(tc)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// DropTokenCache 主动清掉落盘 token（出错重试前 / 需要强制重跑预检时调用）。
func (c *Client) DropTokenCache(a *auth.Auth) {
	if p := c.tokenCachePath(a); p != "" {
		_ = os.Remove(p)
	}
}

// Prefetch 跑一遍完整 L1→L3 并把 token 落盘（同步；预取失败不影响主业务）。
//
// 已有未过期的缓存就不重复预取（避免浪费一次零配额往返）。
func (c *Client) Prefetch(a *auth.Auth) {
	if c.hasFreshToken(a) {
		return
	}
	s, err := c.runSentinelFull(a)
	if err != nil || s == nil || s.ReqToken == "" {
		return
	}
	c.storeTokenCache(a, s)
}

// PrefetchAsync 是 Prefetch 的后台版本：对话流进行中调用，不跟本轮 context 取消。
//
// 返回的 chan 在完成时关闭（nil = 未启用预取：没有 dataDir）。
func (c *Client) PrefetchAsync(a *auth.Auth) <-chan struct{} {
	if c.cachePath("") == "" {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = recover() }() // 预取失败不影响主流程
		c.Prefetch(a)
	}()
	return done
}

// hasFreshToken 判断是否已有可用的预取 token（只查不看，不消费）。
func (c *Client) hasFreshToken(a *auth.Auth) bool {
	p := c.tokenCachePath(a)
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	if err != nil {
		return false
	}
	return time.Since(st.ModTime()) < TokenMaxAge*time.Second
}

// EnsureFresh 在高敏感调用（非匿名对话、生图、上传参考图）**之前**主动做一次
// 升级校验，把「对抗算法已升级」的发现提前到零配额阶段。
//
// 语义：
//   - 已有新鲜 token 且指纹新鲜 → 直接复用（零请求，不消耗任何配额）；
//   - 否则完整跑一遍 L1→L3（零配额）并把结果落盘，供紧随其后的业务请求消费
//     （避免跑两次）。
//
// 返回的 Sentinel 已经过 diffFingerprint 门禁，漂移时会返回 SentinelDriftError。
func (c *Client) EnsureFresh(a *auth.Auth) (*Sentinel, error) {
	if s, ok := c.takeTokenCache(a); ok {
		return s, nil
	}
	s, err := c.runSentinelFull(a)
	if err != nil {
		return s, err
	}
	c.storeTokenCache(a, s)
	return s, nil
}
