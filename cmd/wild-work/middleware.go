// 通用上游中间层（URL 前缀式）装配：config.middleware 按 kind 套到各渠道
// HTTP client 上。与代理装配（proxy.go）共用同一张目标表、同一个入口函数；
// 独立成文件避免 main.go 膨胀（同 proxy.go 的分工）。
package main

import (
	"net/http"
	"strings"

	"wild-work/internal/config"
	"wild-work/internal/middleware"
)

// applyUpstreamChain 铺设渠道出站链路：先重建代理底座、再按需套中间层。
// 启动与两个热更新入口（代理保存 / 中间层保存，见 main.go 的
// SetProxySyncer / SetMiddlewareSyncer）都必须调用**完整链路**：
//   - SetTransportProxy 每轮**新建** Transport，天然丢弃上一轮的中间层包装——
//     只重铺代理会把已开启的中间层静默弄丢；
//   - 反过来只套中间层不重建底座，则无法「关闭中间层」（旧包装残留）。
//
// 中间层永远在代理之上：包装器把改写后的请求交给底层 Transport，由其按
// Proxy 字段经代理拨号到中间层基址。
func applyUpstreamChain(cfg *config.Config, targets map[string][]*http.Client) {
	applyProxies(cfg, targets)
	applyMiddleware(cfg, targets)
}

// applyMiddleware 给**全部渠道**的每个 HTTP client 套中间层包装器（R35：
// HTTP / StreamHTTP / BillingHTTP? 必须一起套——只套非流式会让流式漏掉中间层）。
// 全局开关、不做渠道挑选（中间层对所有上游一视同仁）；未开启 = 不动
// （保持 applyProxies 铺好的直连底座）。
func applyMiddleware(cfg *config.Config, targets map[string][]*http.Client) {
	if !cfg.Middleware.Enabled || strings.TrimSpace(cfg.Middleware.BaseURL) == "" {
		return
	}
	for _, clients := range targets {
		for _, c := range clients {
			if c == nil {
				continue
			}
			// Wrap 幂等替换旧壳（不嵌套）；渠道出站请求由中间层转发上游并回传。
			// 计费/统计口径不变：请求仍按渠道账号池记账。
			c.Transport = middleware.Wrap(cfg.Middleware.BaseURL, c.Transport)
		}
	}
}
