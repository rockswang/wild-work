package main

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"wild-work/internal/config"
	"wild-work/internal/middleware"
)

// isWrapped 判断 Transport 是否已被中间层包装（Unwrap 对非包装器恒等返回）。
func isWrapped(rt http.RoundTripper) bool { return middleware.Unwrap(rt) != rt }

// newBareClient 模拟渠道出厂 client：独立 *http.Transport（120s 同 traework/oczen 出厂值）。
func newBareClient() *http.Client {
	return &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 120 * time.Second}}
}

// TestApplyUpstreamChainMiddleware 锁定装配语义（全局开关，不做渠道挑选）：
//  1. R35：**全部渠道**的 HTTP + StreamHTTP + BillingHTTP? 全套上（只套非流式会让流式漏掉中间层）；
//  2. 热更新切换（关闭后完整链路重铺 → Transport 复位为裸底座）；
//  3. 代理与中间层可叠加：包装器之下是带 Proxy 的底座。
func TestApplyUpstreamChainMiddleware(t *testing.T) {
	wbHTTP, wbStream, wbBilling := newBareClient(), newBareClient(), newBareClient()
	twC := newBareClient()
	targets := map[string][]*http.Client{
		"workbuddy": {wbHTTP, wbBilling, wbStream}, // BillingHTTP? 同套（R35）
		"traework":  {twC},
	}

	// 启用：全部渠道的所有 client 一并套上（全局开关）
	cfg := config.Default()
	cfg.Middleware = config.Middleware{Enabled: true, BaseURL: "http://127.0.0.1:8787"}
	applyUpstreamChain(cfg, targets)
	for name, c := range map[string]*http.Client{"workbuddy.HTTP": wbHTTP, "workbuddy.BillingHTTP": wbBilling, "workbuddy.StreamHTTP": wbStream, "traework.HTTP": twC} {
		if !isWrapped(c.Transport) {
			t.Fatalf("%s 未被套上中间层（全局开关：全部渠道全套，R35）", name)
		}
	}

	// 叠加代理：包装器之下应是带 Proxy 的裸底座（中间层永远在代理之上）
	proxyCfg := config.Default()
	proxyCfg.Proxies = map[string]string{"workbuddy": "socks5://127.0.0.1:1080"}
	proxyCfg.Middleware = cfg.Middleware
	applyUpstreamChain(proxyCfg, targets)
	inner, ok := middleware.Unwrap(wbHTTP.Transport).(*http.Transport)
	if !ok || inner.Proxy == nil {
		t.Fatalf("代理+中间层叠加后，包装器之下应是带 Proxy 的底座（inner=%T）", middleware.Unwrap(wbHTTP.Transport))
	}

	// 热更新关闭：完整链路重铺后 Transport 复位为裸底座（不残留包装）
	offCfg := config.Default() // middleware 缺省关闭
	offCfg.Proxies = proxyCfg.Proxies
	applyUpstreamChain(offCfg, targets)
	for name, c := range map[string]*http.Client{"workbuddy.HTTP": wbHTTP, "workbuddy.StreamHTTP": wbStream, "traework.HTTP": twC} {
		if isWrapped(c.Transport) {
			t.Fatalf("关闭中间层后 %s 的 Transport 应复位为裸底座", name)
		}
	}
	// 底座超时参数在多轮重铺后仍保留（120s 不被中间层包装吞掉）
	tr, ok := wbHTTP.Transport.(*http.Transport)
	if !ok || tr.ResponseHeaderTimeout != 120*time.Second {
		t.Fatalf("重铺后底座应保留原 ResponseHeaderTimeout=120s（got %T %+v）", wbHTTP.Transport, wbHTTP.Transport)
	}
}

// TestSetTransportProxyInheritsTimeoutThroughWrapper 锁定 proxy.go 的继承修复：
// Transport 已被中间层包装时，重建仍继承原 ResponseHeaderTimeout——
// 不剥壳会把 oczen 的 120s 静默降级成默认 60s。
func TestSetTransportProxyInheritsTimeoutThroughWrapper(t *testing.T) {
	c := &http.Client{
		Transport: middleware.Wrap("http://127.0.0.1:8787",
			&http.Transport{ResponseHeaderTimeout: 120 * time.Second}),
	}
	SetTransportProxy(c, nil) // 热更新：直连重铺
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("重铺后应为裸 *http.Transport（got %T）", c.Transport)
	}
	if tr.ResponseHeaderTimeout != 120*time.Second {
		t.Fatalf("应继承被包装底座的 120s（got %v）", tr.ResponseHeaderTimeout)
	}
}

// TestMiddlewareWiringIsComplete 锁定「声明了却没接线」类回归（R34 家族）：
// main.go 的两处装配点都必须走 applyUpstreamChain，且中间层热更新回调已注入。
func TestMiddlewareWiringIsComplete(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go 失败: %v", err)
	}
	code := string(src)
	// 启动装配：启动点必须调用完整链路（applyProxies + applyMiddleware）
	if !strings.Contains(code, "applyUpstreamChain(cfg, upstreamClients)") {
		t.Fatal("main.go 启动装配未调用 applyUpstreamChain —— 只重铺代理会让中间层在启动时静默失效")
	}
	// 两处热更新：代理保存与中间层保存都必须走完整链路（各出现一次）
	if strings.Count(code, "applyUpstreamChain(&next, upstreamClients)") != 2 {
		t.Fatalf("main.go 热更新应有且仅有两处调用 applyUpstreamChain（代理保存/中间层保存），实际 %d 处",
			strings.Count(code, "applyUpstreamChain(&next, upstreamClients)"))
	}
	// 中间层热更新回调必须注入 App（漏注入 = 面板保存只落盘不生效）
	if !strings.Contains(code, "appInst.SetMiddlewareSyncer(func(mw config.Middleware)") {
		t.Fatal("main.go 未注入 SetMiddlewareSyncer —— 面板保存中间层后只写配置不热更")
	}
}
