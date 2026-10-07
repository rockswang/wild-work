// 单渠道上游代理注入：把 config.proxies 中配置的代理地址热套到各渠道 HTTP client 上。
// 独立成文件避免 main.go 膨胀；SetTransportProxy 供面板保存代理配置后热更新复用。
package main

import (
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"wild-work/internal/config"
	"wild-work/internal/middleware"
)

// proxyFor 返回渠道 kind 对应的代理 URL（未配置返回 nil = 直连）。
func proxyFor(cfg *config.Config, kind string) (*url.URL, error) {
	raw := strings.TrimSpace(cfg.Proxies[kind])
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("渠道 %s 的代理地址无效: %q", kind, raw)
	}
	return u, nil
}

// baseTransport 生成与各渠道出厂 Transport 同构的底座（禁 h2 + 连接池 + 超时）。
// 代理只改 Proxy 字段，其余参数保持一致，避免热更新后行为漂移。
func baseTransport(respHeaderTimeout time.Duration) *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper), // 强制 HTTP/1.1
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: respHeaderTimeout,
	}
}

// SetTransportProxy 把 proxyURL（nil = 直连）套到 client 的 Transport 上。
// 统一重建 Transport：出入方向都换新底座，代理切换无残留连接。
// SOCKS5 由 Transport.Proxy 原生支持（Go 内置 socks5 拨号器）。
func SetTransportProxy(client *http.Client, proxyURL *url.URL) {
	if client == nil {
		return
	}
	// 继承原 Transport 的 ResponseHeaderTimeout（未知给默认 60s），
	// oczen 的出厂值是 120s，不能在这里被降级。
	// 热更新链路里 Transport 可能已被中间层包装：先剥掉再断言类型，
	// 否则断言失败会把 oczen 的 120s 静默降级成默认值。
	rhTimeout := 60 * time.Second
	if tr, ok := middleware.Unwrap(client.Transport).(*http.Transport); ok && tr != nil && tr.ResponseHeaderTimeout > 0 {
		rhTimeout = tr.ResponseHeaderTimeout
	}
	tr := baseTransport(rhTimeout)
	tr.Proxy = http.ProxyURL(proxyURL) // proxyURL 为 nil 时即直连
	client.Transport = tr
}

// applyProxies 批量套代理：kind → 该渠道的全部 HTTP client。
// 单渠道配置无效仅记日志不中断启动（fail-soft：代理配错不应把整个 daemon 拉死）。
func applyProxies(cfg *config.Config, targets map[string][]*http.Client) {
	for kind, clients := range targets {
		if err := applyProxyToKind(cfg, kind, clients...); err != nil {
			log.Printf("代理配置无效（该渠道保持直连）：%v", err)
		}
	}
}

// applyProxyToKind 把 cfg 中渠道 kind 的代理套到一组 HTTP client 上（client 可含 nil 项）。
// traework 的 StreamHTTP 与主 client 共用出厂 Transport，因此必须先切共享再传入，
// 此处对每个 client 独立套代理；代理配置无效时返回错误（面板路径不致命仅回显）。
func applyProxyToKind(cfg *config.Config, kind string, clients ...*http.Client) error {
	u, err := proxyFor(cfg, kind)
	if err != nil {
		return err
	}
	// 未配置（u==nil）也要显式套直连：面板删除某渠道代理后需立即恢复直连，
	// 不能因「未配置」跳过而残留旧代理 Transport（启动时对全新 client 无影响）。
	for _, c := range clients {
		SetTransportProxy(c, u)
	}
	return nil
}
