package chatgpt

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"golang.org/x/net/proxy"
)

// 代理隧道自己实现，**不用** tls-client 内置的 CONNECT dialer。
//
// 原因是实测出来的耦合：tls-client 的 connectDialer 拿 `config.timeout` 当 CONNECT
// 的 socket deadline（`SetDeadline(time.Now().Add(timeout))`），而流式渠道必须把该值
// 设为 0（否则它的 `http.Client.Timeout` 会把长 SSE 流掐断，违反 R35）⇒ timeout=0
// 时 CONNECT 立刻 `i/o timeout`（本票单测实测复现）。用一个自定义
// `ProxyDialerFactory` 接管隧道，就能同时满足「流式无总超时」与「代理生效」。

// proxyDialTimeout 是建立代理隧道（含 CONNECT 握手）的上限；隧道只负责连通，
// 请求本身的时长由等响应头看门狗与调用方控制。
const proxyDialTimeout = 30 * time.Second

// newProxyDialerFactory 返回注入 tls-client 的代理工厂。代理地址必须烘进闭包里：
// tls-client 不允许同时设 `WithProxyUrl` 与工厂（校检会直接失败），且未设 proxyUrl 时
// 工厂收到的是空串。
func newProxyDialerFactory(proxyURL string) tls_client.ProxyDialerFactory {
	return func(_ string, _ time.Duration, _ *net.TCPAddr, _ fhttp.Header, _ tls_client.Logger) (proxy.ContextDialer, error) {
		return newProxyDialer(proxyURL)
	}
}

// newProxyDialer 按代理协议构造 dialer：http(s) 走 CONNECT，socks4/5 交给 x/net/proxy。
func newProxyDialer(proxyURL string) (proxy.ContextDialer, error) {
	u, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil {
		return nil, fmt.Errorf("chatgpt: 代理地址无法解析 %q: %w", proxyURL, err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("chatgpt: 代理地址缺少主机 %q（应为 scheme://host:port）", proxyURL)
	}
	d := &net.Dialer{Timeout: proxyDialTimeout}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
		var auth *proxy.Auth
		if u.User != nil {
			pw, _ := u.User.Password()
			auth = &proxy.Auth{User: u.User.Username(), Password: pw}
		}
		sd, err := proxy.SOCKS5("tcp", u.Host, auth, d)
		if err != nil {
			return nil, fmt.Errorf("chatgpt: 初始化 socks5 代理失败: %w", err)
		}
		return &detachedDialer{d: sd}, nil
	case "socks4", "socks4a":
		sd, err := proxy.FromURL(u, d)
		if err != nil {
			return nil, fmt.Errorf("chatgpt: 初始化 %s 代理失败: %w", u.Scheme, err)
		}
		return &detachedDialer{d: sd}, nil
	case "http", "https":
		return &connectDialer{proxy: u, dialer: d}, nil
	case "":
		return nil, fmt.Errorf("chatgpt: 代理地址缺少协议前缀（应为 http:// 或 socks5://）: %q", proxyURL)
	}
	return nil, fmt.Errorf("chatgpt: 不支持的代理协议 %q", u.Scheme)
}

// detachedDialer 把不支持 context 的 dialer（x/net/proxy）包成 proxy.ContextDialer，
// ctx 取消时放弃等待并关闭迟到建立的连接。
type detachedDialer struct{ d proxy.Dialer }

func (s *detachedDialer) Dial(network, addr string) (net.Conn, error) {
	return s.d.Dial(network, addr)
}

func (s *detachedDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() { c, err := s.d.Dial(network, addr); ch <- res{c, err} }()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.c != nil {
				_ = r.c.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

// connectDialer 是 HTTP(S) 代理的 CONNECT 隧道（HTTP/1.1）。
type connectDialer struct {
	proxy  *url.URL
	dialer *net.Dialer
}

func (c *connectDialer) Dial(network, addr string) (net.Conn, error) {
	return c.DialContext(context.Background(), network, addr)
}

func (c *connectDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var raw net.Conn
	var err error
	if strings.EqualFold(c.proxy.Scheme, "https") {
		d := &tls.Dialer{
			NetDialer: c.dialer,
			Config:    &tls.Config{ServerName: c.proxy.Hostname(), NextProtos: []string{"http/1.1"}},
		}
		raw, err = d.DialContext(ctx, network, c.proxyHostPort())
	} else {
		raw, err = c.dialer.DialContext(ctx, network, c.proxyHostPort())
	}
	if err != nil {
		return nil, err
	}
	conn, err := c.handshake(ctx, raw, addr)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	return conn, nil
}

func (c *connectDialer) proxyHostPort() string {
	if c.proxy.Port() != "" {
		return c.proxy.Host
	}
	if strings.EqualFold(c.proxy.Scheme, "https") {
		return net.JoinHostPort(c.proxy.Hostname(), "443")
	}
	return net.JoinHostPort(c.proxy.Hostname(), "80")
}

// handshake 发 CONNECT 并校验 200。ctx 取消或超时都会立刻放弃。
func (c *connectDialer) handshake(ctx context.Context, raw net.Conn, addr string) (net.Conn, error) {
	deadline := time.Now().Add(proxyDialTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	stop := context.AfterFunc(ctx, func() { _ = raw.SetDeadline(time.Now()) })
	defer stop()
	if err := raw.SetDeadline(deadline); err != nil {
		return nil, err
	}

	var sb strings.Builder
	sb.WriteString("CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n")
	if u := c.proxy.User; u != nil {
		pw, _ := u.Password()
		cred := base64.StdEncoding.EncodeToString([]byte(u.Username() + ":" + pw))
		sb.WriteString("Proxy-Authorization: Basic " + cred + "\r\n")
	}
	sb.WriteString("\r\n")
	if _, err := raw.Write([]byte(sb.String())); err != nil {
		return nil, fmt.Errorf("chatgpt: 向代理发 CONNECT 失败: %w", err)
	}

	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		return nil, fmt.Errorf("chatgpt: 读代理响应失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("chatgpt: 代理拒绝 CONNECT（%s）", resp.Status)
	}
	_ = raw.SetDeadline(time.Time{})

	// 代理若在与响应头同一次写入里多发了字节，必须把缓冲一起带进隧道，否则丢字节。
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: raw, r: br}, nil
	}
	return raw, nil
}

// bufferedConn 保留 CONNECT 响应头之后被 bufio 预读的字节。
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }
