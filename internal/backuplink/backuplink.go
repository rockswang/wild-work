// Package backuplink 持有「应用内备用上游通道」——面板配置的**单一**代理地址。
//
// 定位（决议 R57）：**默认不参与本工具的任何请求**。全部渠道一律直连出站；
// 只有渠道自己判定「这次请求被上游按地区拒绝」时，才在同一次请求内取用它重放一次，
// 未配置就原样透传失败。当前取用方：cline（实测 muse-spark-1.3-contributor 国内直连
// 恒 403 `not available in your region`）与 oczen（免费档部分模型直连 403 `RegionError`）。
// 两渠道共用 route.go 的 Replay 编排，只各自提供「怎么发一发」与「什么算地区限制」。
//
// 为什么是包级原子单例，而不是旧的「按渠道把代理套进各 client Transport」：
//  1. 旧做法要在**两张**接线表（启动 + 面板热更新）里逐渠道列 client，实证已经漏过——
//     trCodeUp 从未出现在任何表里，热更新表又漏 monkeycode/raccoon/loomy（R34 同族）；
//     本包零接线：渠道按需自取，面板保存即换指针。
//  2. 备用通道只在个别请求上用，套进 Transport 反而会让它参与该渠道**全部**请求
//     （与「不影响任何请求」的定位相反）。
//  3. 换代理不再重建渠道 Transport ⇒ 不杀 idle 连接，也不必再手工继承
//     ResponseHeaderTimeout（旧 SetTransportProxy 为 oczen 120s 写的特例注释就是这个坑）。
package backuplink

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// AllowedSchemes 备用通道接受的代理协议（与旧 proxies 配置口径一致）。
var AllowedSchemes = []string{"http", "https", "socks5"}

// link 是一份生效中的备用通道快照（不可变；Set 成功后整体替换）。
type link struct {
	raw string
	u   *url.URL
	// cli 缓存「base Transport → 绑定本通道的 http.Client」：同一渠道多次取用
	// 复用同一连接池，不会每发都新建 Transport。
	mu  sync.Mutex
	cli map[*http.Transport]*http.Client
}

var (
	cur atomic.Pointer[link] // nil = 未配置（直连）
	gen atomic.Uint64
)

// Parse 校验代理地址形态并解析（空串返回 nil,nil）。协议白名单见 AllowedSchemes。
// 导出的理由：config.normalize 与面板保存都要用**同一套**判据，避免两处漂移
// （旧实现 config/app/main 三处各写一份 url.Parse + scheme 判断）。
func Parse(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("备用上游代理地址无效 %q（应为 http://host:port 或 socks5://host:port）", raw)
	}
	ok := false
	for _, s := range AllowedSchemes {
		if strings.EqualFold(s, u.Scheme) {
			ok = true
			break
		}
	}
	if !ok {
		return nil, fmt.Errorf("备用上游代理协议 %q 不支持（仅 %s）", u.Scheme, strings.Join(AllowedSchemes, "/"))
	}
	return u, nil
}

// Set 生效一份备用通道配置（空串 = 关闭并清空）。校验失败返回错误且**不改动**现有配置。
func Set(raw string) error {
	u, err := Parse(raw)
	if err != nil {
		return err
	}
	raw = strings.TrimSpace(raw)
	var next *link
	if u != nil {
		next = &link{raw: raw, u: u, cli: make(map[*http.Transport]*http.Client)}
	}
	old := cur.Swap(next)
	gen.Add(1) // 无论开启还是关闭都递增：渠道的学习结果只在「配置未变」时沿用
	// 关掉旧快照的空闲连接：残留连接仍指向旧出口，会让「已改地址」在一段时间里不生效。
	if old != nil {
		old.closeIdle()
	}
	return nil
}

// Enabled 报告是否已配置可用的备用通道。
func Enabled() bool { return cur.Load() != nil }

// Generation 配置代号：每次 Set 成功（含关闭）自增。
// 渠道据此作废本地「该模型只能走备用通道」的学习结果 —— 配置一变就得重新试探直连。
func Generation() uint64 { return gen.Load() }

// Client 返回「经备用通道出站」的 http.Client；未配置通道时返回 nil（调用方据此走直连）。
//
// base 传该渠道自己的 Transport：逐字段复刻其对外旋钮（超时/连接池/TLS 校验）只为换出口，
// 使备用链路除出口 IP 外不引入新的超时差异；**一律禁用 HTTP/2**（原因见 copyTransport 注释）。
// base 为 nil 时退回内置保守参数（禁 h2 + 120s 响应头超时）。
//
// 返回的 client **不设 Client.Timeout**：Go 的 Timeout 覆盖整个请求生命周期（含读 body），
// 对 SSE 意味着长回答必被掐断（R35）；非流式调用方需要总超时时自行包一层。
func Client(base *http.Transport) *http.Client {
	l := cur.Load()
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if c, ok := l.cli[base]; ok {
		return c
	}
	var tr *http.Transport
	if base != nil {
		tr = copyTransport(base)
	} else {
		tr = fallbackTransport()
	}
	tr.Proxy = http.ProxyURL(l.u)
	c := &http.Client{Transport: tr}
	l.cli[base] = c
	return c
}

// copyTransport 逐字段复刻 base 的对外旋钮，**不用 base.Clone()**。
//
// 2026-10-14 实盘教训（R57 回归）：Clone() 会把源 Transport 的**内部 h2 启用态**一起带走，
// 之后再设 TLSNextProto 空 map / ForceAttemptHTTP2=false 都**无效**（Go 1.24+ 的 h2 已内建，
// 不再看 TLSNextProto）⇒ 经 HTTP 代理时 ALPN 仍会协商 h2，而代理隧道的 h2 握手失败，
// 回 h2 SETTINGS/GOAWAY 帧，调用方报 `net/http: HTTP/1.x transport connection broken:
// malformed HTTP response "\x00\x00\x12\x04…http2_handshake_failed"`，表现为「备用通道永远拨不通」。
//
// 四组对照（同一代理、同一发 muse）：
//
//	base.Clone()+Proxy（任意 h2 字段）  → ✗ malformed HTTP response
//	逐字段新建+Proxy+禁h2             → ✓ GET 200 / POST 200
//	逐字段新建+Proxy（不塞 TLSNextProto）→ ✓（差异只在 h2 是否被真禁）
//	直连（不走备用）                   → ✓ POST 403（地区限制，符合预期）
//
// 逐字段新建的另一个好处：与旧实现（SetTransportProxy 的 baseTransport）同构，
// 那条路已在线验证过可通代理，不需重新积累信心。
func copyTransport(base *http.Transport) *http.Transport {
	tr := &http.Transport{
		Proxy:                  nil, // 出口由调用方设置
		Dial:                   base.Dial,
		DialContext:            base.DialContext,
		DialTLSContext:         base.DialTLSContext,
		TLSClientConfig:        stripALPN(base.TLSClientConfig),
		TLSHandshakeTimeout:    base.TLSHandshakeTimeout,
		DisableKeepAlives:      base.DisableKeepAlives,
		DisableCompression:     base.DisableCompression,
		MaxIdleConns:           base.MaxIdleConns,
		MaxIdleConnsPerHost:    base.MaxIdleConnsPerHost,
		MaxConnsPerHost:        base.MaxConnsPerHost,
		IdleConnTimeout:        base.IdleConnTimeout,
		ExpectContinueTimeout:  base.ExpectContinueTimeout,
		ResponseHeaderTimeout:  base.ResponseHeaderTimeout,
		ProxyConnectHeader:     base.ProxyConnectHeader,
		MaxResponseHeaderBytes: base.MaxResponseHeaderBytes,
		WriteBufferSize:        base.WriteBufferSize,
		ReadBufferSize:         base.ReadBufferSize,
		// 显式禁 h2：**必须建在字面量上**（见函数注释），Clone 后再设无效。
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	if tr.Dial == nil && tr.DialContext == nil {
		tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}).DialContext
	}
	return tr
}

// stripALPN 复制 TLS 配置但不带 NextProtos：保留自定义校验旋钮（如 InsecureSkipVerify），
// 同时不让 ALPN 广告 h2（备用链路一律 HTTP/1.1）。config 为 nil 时返回 nil。
func stripALPN(cfg *tls.Config) *tls.Config {
	if cfg == nil {
		return nil
	}
	cp := cfg.Clone()
	cp.NextProtos = nil
	return cp
}

// closeIdle 关闭本快照所有缓存 client 的空闲连接。
func (l *link) closeIdle() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.cli {
		if tr, ok := c.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
	clear(l.cli)
}

// fallbackTransport 与多数渠道出厂 Transport 同构的兜底底座（禁 h2 + 响应头超时）。
func fallbackTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper), // 强制 HTTP/1.1
		TLSHandshakeTimeout:   15 * time.Second,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
}
