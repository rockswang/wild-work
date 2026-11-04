// client.go OpenCodeZen 匿名免费通道客户端，实现 provider.Upstream。
//
// 与其它渠道最大的不同：本渠道「无账号」。匿名凭证是常量 "public"，
// 面板里只有一个虚拟账号（AnonymousUID），不可增删停用、无签到、无积分。
// 所有请求都按 OpenCode 官方 CLI 的形态构造（见 constants.go 的三道校验）。
package oczen

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/backuplink"
	"wild-work/internal/idle"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// Client 上游 HTTP 客户端。Base 可覆盖以便测试。
type Client struct {
	HTTP *http.Client
	// StreamHTTP 用于对话流（SSE）：不设整体 Timeout，避免长回答在超时点被掐断
	// 且无终止帧（R35 / issue #42）。
	StreamHTTP *http.Client
	// IdleTimeout 聊天 SSE 流中空闲超时（StreamHTTP 无总时长上限，靠本字段兜底
	// 「上游中途卡死」）。<=0 表示禁用（测试用）。
	IdleTimeout time.Duration
	Base        string

	// routes 地区限制学习路由（与 cline 共用 backuplink.Routes / Replay 编排，R62）。
	routes backuplink.Routes

	// mu 保护 apikey 的并发读写（面板热更新与请求路径并发）。
	mu sync.RWMutex
	// apiKey 自定义 API key；空 = 匿名凭证（AnonymousKey）。
	// 由面板设置（config.Proxies 同屏的 oczen_api_key）热更新。
	apiKey string
}

// New 生产默认客户端。
func New() *Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	tr := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	// 免费档闸门判 TLS ClientHello 指纹（R155）：同一份请求体，Go 原生 ClientHello 恒
	// 429 FreeUsageLimitError，复刻官方 CLI（Bun/BoringSSL）的 ClientHello 才 200。
	// 见 tlsfp.go 的取证记录。
	if !fingerprintDisabled() {
		tr.DialTLSContext = officialFingerprintDialer(dialer.DialContext)
	}
	// 刻意**不**设 Base：让它留空，由 base() 按凭证车道选择
	// （匿名 → AnonymousBase，自备 key → DefaultBase）。设成 DefaultBase 会把匿名车道
	// 钉在 /zen 上，正是 issue #117 的恒 403 形态（R69）。
	return &Client{HTTP: &http.Client{Timeout: requestTimeout, Transport: tr},
		StreamHTTP:  &http.Client{Transport: tr}, // 共用 Transport，不设 Timeout
		IdleTimeout: idle.DefaultTimeout,         // 流中空闲监控兜底（见 internal/idle）
	}
}

// NewWithBase 测试用：覆盖上游基址。
func NewWithBase(base string) *Client {
	c := New()
	c.Base = base
	return c
}

// base 返回当前出站基址：**按车道分流**。
//
// 匿名（无 key）走 AnonymousBase(/inference)，自备 sk-… key 走 DefaultBase(/zen)：
// 上游 2026-10-14 把 keyless 推理迁到了独立入口，两条车道基址不再相同。
// 显式 c.Base（测试注入 NewWithBase）仍优先，两种车道共用同一 httptest 假上游。
func (c *Client) base() string {
	if c.Base != "" {
		return c.Base
	}
	// apiKey 由面板热更新（SetAPIKey 持写锁），故取值也须持读锁，否则与保存操作并发即数据竞争。
	c.mu.RLock()
	keyed := c.apiKey != ""
	c.mu.RUnlock()
	if keyed {
		return DefaultBase
	}
	return AnonymousBase
}

// AnonymousAuth 构造匿名虚拟账号。唯一合法账号，由 main 装配时注入 pool。
// ExpiresAt 取远期值：使 NeedsRefresh 恒为假，避免走无意义的 token 刷新路径。
func AnonymousAuth() *auth.Auth {
	return &auth.Auth{
		Kind:        string(Kind),
		AccessToken: AnonymousKey,
		ExpiresAt:   noExpiry,
		UID:         AnonymousUID,
		Nickname:    AnonymousName,
	}
}

// StaticModels 上游不可达时的静态兜底清单（仍只含免费模型）。
func StaticModels() []provider.ModelInfo { return staticFree() }

// ---------------------------------------------------------------------------
// 请求构造
// ---------------------------------------------------------------------------

// 时间戳段不是任意 12 位 hex：免费档会按官方客户端 Identifier.descending()
// 的编码解析它——~(unix_ms<<12|counter) 取低 48 位，故小写 hex 段的末三位是
// 0xFFF-counter。实盘探测（2026-10-13）：免费档接受 counter 1–63（末三位
// 0xFC0–0xFFF），超出即判为非 OpenCode 客户端 → 403 FreeTierError，即便 ses_
// 外形正确；摘要段（旧实现）同属该拒收窗口。故仅当时间戳段落在该编码窗口内
// 才原样保留，否则确定性重编码——同时保住「同一会话稳定映射到同一 session」
// 的 prompt cache 亲和。
var descendingTimePartPattern = regexp.MustCompile(`^[0-9a-f]{9}f[c-f][0-9a-f]$`)

// canonicalSessionID 把任意会话种子映射成 OpenCode 官方格式
// ses_<12位小写hex><14位Base62>。已是官方客户端签发的 descending 编码则原样
// 返回（保住 prompt cache 亲和）；其余（含旧实现的摘要段与升序 ID）重编码成
// 该精确布局。上游自 2026-09-16 起对非该形状/编码的会话头一律回 403 FreeTierError；
// 自 2026-10-25 起还校验时间戳合理性（超窗回 429 FreeUsageLimitError，见 descendingTimePart）。
func canonicalSessionID(signal string) string {
	// 官方形状：ses_ + 12 位小写 hex（时间戳）+ 14 位 Base62
	if len(signal) == 4+12+14 && strings.HasPrefix(signal, "ses_") {
		ok := true
		for i := 4; i < 16; i++ {
			c := signal[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				ok = false
				break
			}
		}
		if ok {
			for i := 16; i < len(signal); i++ {
				if !isBase62(signal[i]) {
					ok = false
					break
				}
			}
		}
		if ok && descendingTimePartPattern.MatchString(signal[4:16]) {
			return signal
		}
		// 形状对但编码外来（旧摘要段 / 升序 ID）：落到下面确定性重编码。
	}
	sum := sha256.Sum256([]byte("ses\x00" + signal))
	timePart := descendingTimePart(time.Now(), sum[:6])
	n := new(big.Int).SetBytes(sum[6:16])
	return "ses_" + timePart + base62Fixed(n, 14)
}

// sessionTimestampWindow 限制合成会话时间戳回落的窗口：上游接受距今约一年内的日期
// （两个方向），一个月已留足时钟偏移与长驻进程的余量，同时绝不逼近边界。
const sessionTimestampWindow = 30 * 24 * time.Hour

// descendingTimePart 以官方客户端 Identifier.descending() 的布局渲染落在合理窗口内的时间戳：
// 36 位时间戳移位进 48 位字的高位，OR 上 counter=1，再按位取反，故末三位恒为 ffe。
// 时间戳锚定今日 UTC 零点、再减哈希派生的偏移（≤sessionTimestampWindow）：上游自
// 2026-10-25 起对解码后超出约一年窗口的时间戳回 429 FreeUsageLimitError（与真实
// 配额耗尽同一报文），哈希直派生会落在 1970 年代而必被拒（参考实现 opencode2api
// 9533101 同款修法）。哈希只挑窗口内偏移、不提供日期；同一信号一天内稳定（对话亲和）。
func descendingTimePart(now time.Time, hash []byte) string {
	window := uint64(sessionTimestampWindow / time.Millisecond)
	offset := (uint64(hash[0])<<24 | uint64(hash[1])<<16 | uint64(hash[2])<<8 | uint64(hash[3])) % window
	base := now.UTC().Truncate(24 * time.Hour).Add(-time.Duration(offset) * time.Millisecond)
	current := uint64(base.UnixMilli())<<12 | 1
	inverted := ^current & 0xFFFFFFFFFFFF
	return hex.EncodeToString([]byte{
		byte(inverted >> 40), byte(inverted >> 32), byte(inverted >> 24),
		byte(inverted >> 16), byte(inverted >> 8), byte(inverted),
	})
}

func isBase62(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

func base62Fixed(n *big.Int, width int) string {
	base := big.NewInt(62)
	out := make([]byte, width)
	rem := new(big.Int)
	for i := width - 1; i >= 0; i-- {
		n.DivMod(n, base, rem)
		out[i] = base62Alphabet[rem.Int64()]
	}
	return string(out)
}

func randomID(prefix string, size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return prefix + "_" + hex.EncodeToString([]byte(time.Now().String()))
	}
	return prefix + "_" + hex.EncodeToString(buf)
}

// conversationSeed 取首个 user 消息的 content 作为会话种子：
// 多轮对话中历史不断增长，取首轮可保证同一对话映射到同一会话（prompt cache 亲和），
// 不同对话则自然分开。
func conversationSeed(obj map[string]any) string {
	msgs, _ := obj["messages"].([]any)
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok || m["role"] != "user" {
			continue
		}
		if b, err := json.Marshal(m["content"]); err == nil && len(b) > 0 && string(b) != "null" {
			return string(b)
		}
	}
	return ""
}

// projectID 生成官方 CLI 那种**裸 40 位小写 hex**的 project id。
//
// 官方口径（OpenCode `packages/core/src/project.ts`，2026-11-01 抓包）：有 remote
// 的 git 仓取 `sha1("git-remote:host/path")`，否则取 `sha1("directory:" + 工作目录)`；
// 只有「无 remote 且无提交」的仓才发字面量 `global`。上游只校验形状（非 `prj_`
// 前缀的 40 位 hex），但**每请求随机**是官方客户端从不出现的形态（真实客户端整个
// 工作区固定一个 id，见 magpie 0.1.24 注释：0.1.14–0.1.18 每请求发 `global` 等于
// 把全体用户塞进同一个 project 桶）。故按官方派生、进程内固定一次。
func projectID() string { return projectIDOnce() }

var projectIDOnce = sync.OnceValue(func() string {
	// sha1 是**上游规定的形状**（40 位 hex），不是本仓的完整性保护，故此处忽略弱哈希告警。
	sum := sha1.Sum([]byte("directory:" + workDir()))
	return hex.EncodeToString(sum[:])
})

// randomHex 返回 n 字节的随机 hex 串（失败时退回时间派生，不阻塞请求）。
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		sum := sha256.Sum256([]byte(time.Now().String()))
		return hex.EncodeToString(sum[:n])
	}
	return hex.EncodeToString(buf)
}

// traceHeaders 生成官方 CLI（Effect tracer 的 B3 + W3C 两种形态）的链路头：
// **一次对话一个 trace，每个请求一个新 span**。trace 由会话 id 派生（无状态、
// 同一会话跨请求稳定），span 每请求随机。
func traceHeaders(session string) (b3, traceparent string) {
	sum := sha256.Sum256([]byte("trace\x00" + session))
	trace := hex.EncodeToString(sum[:16])
	span := randomHex(8)
	return trace + "-" + span + "-1-" + randomHex(8), "00-" + trace + "-" + span + "-01"
}

// authKey 返回当前出站凭证：自定义 key 优先，空则回匿名。
func (c *Client) authKey() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.apiKey != "" {
		return c.apiKey
	}
	return AnonymousKey
}

// SetAPIKey 热更新自定义 API key（空串 = 回匿名凭证）。
func (c *Client) SetAPIKey(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.apiKey = strings.TrimSpace(key)
}

// chatURL 返回对话端点：显式 Base（测试注入）→ base()/chat/completions；
// 匿名车道 → AnonymousChatURL（v2 的 /inference/openai/v1 族，R152）。
func (c *Client) chatURL() string {
	if c.Base != "" {
		return strings.TrimRight(c.Base, "/") + "/chat/completions"
	}
	c.mu.RLock()
	keyed := c.apiKey != ""
	c.mu.RUnlock()
	if keyed {
		return DefaultBase + "/chat/completions"
	}
	return AnonymousChatURL
}

// headered 为上游请求注入 OpenCode 2.x CLI 伪装头（含规范会话头与链路头）。
//
// 头集按 2026-11-04 的**官方 2.2.5 抓包**镜像（原始头序：
// `authorization` → `content-type` → `user-agent` → `b3` → `traceparent` →
// `x-opencode-client` → `x-opencode-project` → 四个会话头 → `accept`）：
//   - **不发 `x-opencode-request`**：该关联头一旦出现即判非官方客户端 ⇒ 403；
//   - **发 `Accept: */*`**：官方抓包里确有该头（R152「不得发 Accept」的结论在抓包前，
//     已按证据修正；Go 不显式设置时根本不发该头，与官方不一致）；
//   - `x-opencode-project` 必须是**裸 40 位小写 hex**，按官方 sha1 派生（见 projectID）；
//   - 会话头四个都要带：`x-opencode-session` + **`x-opencode-session-id`**
//   - `x-session-affinity` + `X-Session-Id`，且同值（缺 `-id` 变体会被拒）；
//   - `b3` / `traceparent` 是官方 Effect tracer 的链路头（一对话一 trace、一请求一 span）。
func (c *Client) headered(req *http.Request, session string) {
	b3, traceparent := traceHeaders(session)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Authorization", "Bearer "+c.authKey())
	req.Header.Set("b3", b3)
	req.Header.Set("traceparent", traceparent)
	req.Header.Set("x-opencode-client", "cli")
	// 会话头族：上游要求「两个 session 头携带同一 descending id」，官方客户端
	// 同时发 -session 与 -session-id 两种拼写，故四个都发同一值。
	req.Header.Set("x-opencode-session", session)
	req.Header.Set("x-opencode-session-id", session)
	req.Header.Set("x-session-affinity", session)
	req.Header.Set("X-Session-Id", session)
	req.Header.Set("x-opencode-project", projectID())
	req.Header.Set("Accept", "*/*")
}

// ---------------------------------------------------------------------------
// provider.Upstream 实现
// ---------------------------------------------------------------------------

// ChatStream 改写客户端请求体后转发到 Zen。上游恒为流式，非流式客户端由 Aggregate 收敛。
//
// 地区限制自动改道（R62）：直连命中 RegionError 时，在同一次请求内经**备用上游通道**重放一次；
// 未配置则原样透传。编排与 cline 共用 backuplink.Replay（判据/首因保留/学习路由只有一份实现）。
func (c *Client) ChatStream(_ *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, 0, nil, fmt.Errorf("oczen: invalid request body: %w", err)
	}
	// v2 整形层：官方 prompt + 官方工具集 + 客户端工具改名 + 聊天体常量。
	// 返回的改名集合用于响应侧把工具名还原成调用方声明的名字。
	renames := shapeChatRequest(obj)
	// 会话标识由对话首轮稳定派生；缺 seed 时随机（单次会话，无亲和需求）。
	seed := conversationSeed(obj)
	if seed == "" {
		seed = randomID("fallback", 16)
	}
	session := canonicalSessionID(seed)
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("oczen: marshal body: %w", err)
	}
	model, _ := obj["model"].(string)
	chatURL := c.chatURL()

	// 每次 attempt 重新构造请求（体已在内存，重放无需 GetBody）。
	// cancel 所有权随**本次响应**走：在本函数内包好 idle.Monitor（空闲兜底 + 关闭时取消），
	// 非 200/传输错误路径就地 cancel。如此不依赖「最后一次 attempt 恰好就是成功那次」这一
	// 隐式顺序（编排可能连发 1~3 次：直连→备用→回落直连），也避免 ErrNoBackup 等
	// 提前返回路径漏调 cancel 而泄漏 context。
	attempt := func(useBackup bool) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodPost, chatURL, bytes.NewReader(out))
		if err != nil {
			return nil, err
		}
		c.headered(req, session)
		req, cancel := idle.WithCancel(req)
		hc := c.HTTP
		if c.StreamHTTP != nil {
			hc = c.StreamHTTP
		}
		if useBackup {
			tr, _ := hc.Transport.(*http.Transport)
			bc := backuplink.Client(tr)
			if bc == nil {
				cancel()
				return nil, backuplink.ErrNoBackup
			}
			hc = bc
		}
		resp, err := hc.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			cancel() // 失败分支在本函数内收尾；200 的取消权随 resp 交给下面的 Monitor
			return resp, err
		}
		// 响应侧还原工具名：包在 idle.Monitor 外层，Stream（透传）与
		// Aggregate（本地聚合）两条路径都自动经过，不必各实现一遍。
		resp.Body = newToolNameReader(idle.Monitor(resp.Body, c.IdleTimeout, cancel), renames) // 流空闲兜底 + 取消权转移
		return resp, nil
	}

	rp := &backuplink.Replay{
		Kind:    "oczen",
		Model:   model,
		Routes:  &c.routes,
		Attempt: attempt,
		Blocked: IsRegionBlocked,
		// 不需要 Wrap：200 的 body 已在 attempt 内包好 idle.Monitor（取消权随响应走）。
		// 旧实现用 1MB 读错误体（上游错误详情偶尔较长），保持原值以免削弱「原文透传」。
		MaxErrBody: 1 << 20,
	}
	return rp.Do()
}

// regionBlockedMarkers 是「换出口 IP 有可能救回来」的拒绝文案（触发备用通道重放）。
//
// 实测形态：
//   - 403 + `{"error":{"type":"RegionError",...}}`：地域受限的免费模型
//     （如 muse-spark-*-contributor-free）。
//   - 403 + `{"error":{"type":"FreeTierError","message":"...aren't available from
//     your current network because we've detected abusive traffic from this IP
//     address..."}}`：**出口 IP 级**封禁。2026-10-19 实盘：同一条请求在武汉家宽、
//     日本/香港/新加坡/英国/德国/韩国/土耳其/加拿大/法国节点全部命中此文案，
//     而换到干净的美国出口后同一实现立刻 200 —— 判据必须是「换 IP 能救」，此文案正合。
//
// 判据**宁窄勿宽**：命中会多发一发经代理的重放请求，误判的代价是把流量发到第三方出口，
// 故只认明确的地区/国家/出口 IP 语义文案；`access forbidden` 这类宽文案仍不得入选
// （回归测试 TestOczenIsRegionBlocked 有对应用例）。
var regionBlockedMarkers = []string{
	"regionerror", // 实测的 error.type 字面量
	"not available in your region",
	"not available in your country",
	"region-restricted",
	"geo-block",
	"geoblock",
	"abusive traffic",                     // FreeTierError：出口 IP 被判定滥用（换 IP 可解）
	"available from your current network", // FreeTierError：网络/出口级不可用（换 IP 可解）
}

// IsRegionBlocked 报告响应是否属于「地域限制」（可尝试经备用通道重试）。
// 状态码限 403/451：其余状态一律不认（保守，避免把限流/风控误发到第三方出口）。
func IsRegionBlocked(status int, body string) bool {
	if status != http.StatusForbidden && status != http.StatusUnavailableForLegalReasons {
		return false
	}
	low := strings.ToLower(body)
	for _, m := range regionBlockedMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// TestKey 用 big-pickle 模型发一次最小对话验证凭证可用性。
// 返回 (status, 上游原文片段, viaBackup, error)。判定口径（面板展示用）：
//   - 200：key 有效且配额正常；
//   - 429：key 有效但触发限流（匿名共享配额或临时背压）——也算「连通」；
//   - 401/403：key 无效、免费档形态不符或被风控；402：付费模型余额不足；
//   - 503：上游端点故障（key 无法判定，连接是通的）。
//
// 与 ChatStream 同一条编排（R62 的补充）：直连被地域拒（403/451 + RegionError 文案）时
// **同样**经备用上游通道重放一次——否则用户点了「测试」看到 403，就无从判断是「备用通道
// 没生效」还是「上游本来就不给这个出口用」（issue #117）。viaBackup 报告本次答复是否
// 来自备用出口，供面板如实展示「经备用通道」。
func (c *Client) TestKey() (status int, body string, viaBackup bool, err error) {
	obj := map[string]any{
		"model":      "big-pickle",
		"stream":     true,
		"max_tokens": 8,
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
	}
	// 与真实对话同一条整形层：测试探针若用 v1 形态，看到的是「形态不合格」
	// 的 429，而不是凭证/口径的真实状态（R64 同源要求）。
	shapeChatRequest(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		return 0, "", false, err
	}
	session := canonicalSessionID(randomID("testkey", 8))
	chatURL := c.chatURL()
	via := false
	attempt := func(useBackup bool) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodPost, chatURL, bytes.NewReader(out))
		if err != nil {
			return nil, err
		}
		c.headered(req, session)
		hc := c.HTTP
		if useBackup {
			tr, _ := hc.Transport.(*http.Transport)
			bc := backuplink.Client(tr)
			if bc == nil {
				return nil, backuplink.ErrNoBackup
			}
			hc = bc
		}
		return hc.Do(req)
	}
	rp := &backuplink.Replay{Kind: "oczen-test", Model: "big-pickle", Routes: &c.routes, Attempt: attempt, Blocked: IsRegionBlocked, MaxErrBody: 4 << 10,
		Note: func(v bool) { via = v }} // 学习路由 + 本次答复出口标记：两者都要（issue #117）
	rc, st, raw, rerr := rp.Do()
	if rerr != nil {
		return 0, "", via, rerr
	}
	if rc != nil { // 200：读尽小片段后关闭（上游恒流式，同 ChatStream）
		defer rc.Close()
		b, _ := io.ReadAll(io.LimitReader(rc, 4<<10))
		return st, string(b), via, nil
	}
	return st, string(raw), via, nil
}

// FetchModels 拉取上游模型列表并只保留免费集合。
// 上游不可达时不报错、回静态兜底清单：匿名渠道无账号可轮换，
// 报错只会让面板显示空白，不如给出已知可用清单。
func (c *Client) FetchModels(_ *auth.Auth) ([]provider.ModelInfo, error) {
	ids, err := c.fetchModelIDs()
	if err != nil || len(ids) == 0 {
		return StaticModels(), nil
	}
	free := filterFree(ids)
	if len(free) == 0 {
		return StaticModels(), nil
	}
	return free, nil
}

// fetchModelIDs 请求 GET /models（同样需要伪装头）。
func (c *Client) fetchModelIDs() ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, c.base()+"/models", nil)
	if err != nil {
		return nil, err
	}
	c.headered(req, canonicalSessionID(randomID("catalog", 8)))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("oczen: models http %d", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		ids = append(ids, d.ID)
	}
	return ids, nil
}

// FetchModelPricing 免费模型一律 0 倍率（Explicit=true 让面板显示「免费」而非 unknown）。
func (c *Client) FetchModelPricing(_ *auth.Auth) ([]provider.ModelPricing, error) {
	models, _ := c.FetchModels(nil)
	yes := true
	out := make([]provider.ModelPricing, 0, len(models))
	for _, m := range models {
		out = append(out, provider.ModelPricing{
			Model:    m.ID,
			Channel:  string(Kind),
			Rate:     0,
			Explicit: &yes,
		})
	}
	return out, nil
}

// RefreshToken 空实现：匿名通道无 token 可刷。
// 不返回错误以免上层把它当作会话失效；虚拟账号 ExpiresAt 为远期值，
// 正常路径不会走到这里。
func (c *Client) RefreshToken(_ *auth.Auth) error { return nil }

// UserResource 匿名通道无积分概念，恒为 0 且不报错（面板显示「不适用」）。
func (c *Client) UserResource(_ *auth.Auth) (int64, error) { return 0, nil }

// UserResourceDetail 同上：无明细条目。
func (c *Client) UserResourceDetail(_ *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}

// DailyCheckin 匿名通道无签到活动（渠道声明为 noExplicitCheckin，调度器不会调用）。
func (c *Client) DailyCheckin(_ *auth.Auth) error {
	return fmt.Errorf("opencodezen 匿名通道无签到活动")
}

// Classify 上游错误分类。本渠道是单账号（且不可人工恢复），分类原则是
// 「绝不因请求级问题惩罚账号」，否则唯一账号一冷却就等于整条渠道下线：
//   - 429 → ErrSoftRate：真限流，短冷却 —— 单账号渠道唯一需要的背压；
//   - 其它 4xx（含 401/402/403 的 FreeTierError/RegionError）→ ErrPassthrough：
//     原文透传、不计错不冷却 —— 这些是请求形态/地域限制，换账号无从规避；
//     尤其不得归 ErrSessionDead：那会 pool.Disable 永久禁用，而匿名账号不可重登；
//   - 5xx → ErrServer（server.Runtime 已置 NoCooldownOnServerError，同样不罚账号）。
//
// 注：RegionError 现在**多半到不了这里**——ChatStream 会在渠道内部先经备用上游通道
// 重放一次（R62），成功则客户端拿到 200。只有「未配备用通道 / 备用也受限 / 备用拨不动」
// 才会把 403 原文交给 handler，此时本条归 ErrPassthrough 的语义仍然正确（不罚账号）。
func (c *Client) Classify(status int, body string) provider.ErrKind {
	switch {
	case status == http.StatusTooManyRequests:
		return provider.ErrSoftRate
	case status >= 500:
		return provider.ErrServer
	case status >= 400:
		return provider.ErrPassthrough
	default:
		return provider.ErrNone
	}
}

// SystemOne 调用 jev 决策模型端点（POST /v1/systemone）。
// 透传 state+questions，自动注入 model、规范 criteria 并附加伪装头。
// 返回上游原始 JSON 响应体（非流式）；status>=400 时 body 为错误详情。
func (c *Client) SystemOne(state string, questions map[string]any) (int, []byte, error) {
	body := map[string]any{
		"model":     "jev-1.13-free",
		"state":     state,
		"questions": questions,
	}
	out, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("oczen systemone marshal: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, c.base()+"/systemone", bytes.NewReader(out))
	if err != nil {
		return 0, nil, err
	}
	c.headered(req, canonicalSessionID(randomID("sysone", 8)))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// Stream 透传上游 SSE，并把 model 字段回填成客户端请求的原始模型名（R14）。
// 返回值为末帧捕获的 usage（供记账，上游未返回时为 nil）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
	err := upstream.StreamCapture(w, rewriteModelReader(r, model), func(u map[string]any) { usage = u })
	return usage, err
}

// Aggregate 把上游 SSE 收敛成单个 OpenAI 响应，同样回填客户端模型名。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	resp, err := upstream.Aggregate(r)
	if err != nil {
		return nil, err
	}
	if model != "" {
		resp["model"] = model
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// model 字段回填
// ---------------------------------------------------------------------------

// rewriteModelReader 逐行把 SSE 帧里的 "model":"<上游裸名>" 替换成客户端模型名。
// 上游帧经规范化白名单重建，model 字段格式固定为 `"model":"xxx"`（紧凑无空格），
// 故按字节替换即可；按行处理保证流式不被缓冲（每行一到即吐，不攒整包）。
type modelRewriter struct {
	src     *bufio.Reader
	pending []byte // 已读入但尚未吐出的当前行缓冲
	model   string
}

func rewriteModelReader(r io.Reader, model string) io.Reader {
	if model == "" {
		return r
	}
	return &modelRewriter{src: bufio.NewReaderSize(r, 64*1024), model: model}
}

// Read 以「行」为单位吐数据：一次吐一行（含尾部换行），保持 SSE 帧边界完整。
func (m *modelRewriter) Read(p []byte) (int, error) {
	for len(m.pending) == 0 {
		line, err := m.src.ReadBytes('\n')
		if len(line) > 0 {
			m.pending = rewriteModelLine(line, m.model)
		}
		if err != nil {
			if len(m.pending) == 0 {
				return 0, err
			}
			break
		}
	}
	n := copy(p, m.pending)
	m.pending = m.pending[n:]
	return n, nil
}

// rewriteModelLine 替换单行内所有 "model":"..." 的值。
func rewriteModelLine(line []byte, model string) []byte {
	needle := []byte(`"model":"`)
	out := line
	from := 0
	for {
		i := bytes.Index(out[from:], needle)
		if i < 0 {
			return out
		}
		start := from + i + len(needle)
		end := bytes.IndexByte(out[start:], '"')
		if end < 0 {
			return out
		}
		end += start
		// 已替换段不会再次命中 needle（模型名不含 `"model":"`），从替换点之后继续扫描
		repl := make([]byte, 0, len(out)-(end-start)+len(model))
		repl = append(repl, out[:start]...)
		repl = append(repl, model...)
		repl = append(repl, out[end:]...)
		out = repl
		from = start + len(model)
	}
}
