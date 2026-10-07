// Package middleware 通用上游中间层（URL 前缀式）：把任意第三方中间层
// （如 billion-context 上下文压缩代理）插到指定渠道的上游链路。
//
// 机制：http.RoundTripper 包装器，零渠道代码改动——出站请求的完整 URL 被
// 改写为「中间层基址 + 原完整 URL」的拼接形态，由中间层自行转发上游并回传
// 响应；适用渠道与开关由 config.middleware 控制（渠道级粒度），装配见
// cmd/wild-work/middleware.go。
//
// 拼接形态（简单字符串拼接，中间层按「前缀路由 + 原样转发」实现即可）：
//
//	归一基址（去空白与尾部斜杠）+ "/" + 原完整 URL（含 query，原样不转义）
//
// 中间层专属的路径段（如 billion-context 的 "/bili"）由 base_url 自带——
// 通用机制不内置任何具体中间层的路径约定。
//
// 计费/统计口径：中间层只改出站路径，账号、计费、统计口径全部不变——
// 请求仍按渠道账号池记账（token 用量由上游返回、照常入流水与运行统计）。
package middleware

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// prefix 归一中间层基址（去空白与尾部斜杠）并拼上前缀分隔符 "/"。
func prefix(base string) string {
	return strings.TrimRight(strings.TrimSpace(base), "/") + "/"
}

// Prefixed 判断 target 是否已带中间层前缀（幂等判定，防双重包裹）。
func Prefixed(base, target string) bool {
	return strings.HasPrefix(target, prefix(base))
}

// Rewrite 把目标 URL 包上中间层前缀，返回「中间层基址 + 原完整 URL」。
// target 为空或已含前缀时原样返回（幂等，防双重包裹）。
func Rewrite(base, target string) string {
	if target == "" || Prefixed(base, target) {
		return target
	}
	return prefix(base) + target
}

// roundTripper URL 前缀式中间层包装器：改写出站请求的 URL 后交给内层转发。
type roundTripper struct {
	base string // 归一后的前缀（含结尾 "/"）
	next http.RoundTripper
}

// Wrap 把 baseURL 对应的中间层套在 next 之上：返回的 RoundTripper 会把出站
// 请求改写为「中间层基址 + 原完整 URL」后交给 next（next 通常为渠道的
// *http.Transport，代理/连接池/超时参数全部保留，中间层永远在代理之上）。
// next 已是本包包装器时剥掉旧壳再包（热更新换基址不产生嵌套，内层永远是裸底座）。
func Wrap(baseURL string, next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	if old, ok := next.(*roundTripper); ok {
		next = old.next
	}
	return &roundTripper{base: prefix(baseURL), next: next}
}

// Unwrap 返回包装器内层的 RoundTripper；非本包包装器原样返回。
// 供装配层重建 Transport 时继承底层参数（如 ResponseHeaderTimeout，见
// cmd/wild-work/proxy.go 的 SetTransportProxy）。
func Unwrap(rt http.RoundTripper) http.RoundTripper {
	if mw, ok := rt.(*roundTripper); ok && mw.next != nil {
		return mw.next
	}
	return rt
}

// RoundTrip 改写出站请求：完整 URL 嵌入路径转给中间层。
// 响应（含流式 body）原样透传——包装器不触碰 body，SSE 长连接行为不变（R35）。
func (m *roundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil || req.URL.Host == "" {
		// 无 URL 的请求无从改写（客户端契约下不该出现）：原样交给底层，不猜语义。
		return m.next.RoundTrip(req)
	}
	raw := req.URL.String() // 原完整 URL（含 query）
	if Prefixed(m.base, raw) {
		// 已带中间层前缀（幂等防双重包裹）：不改写直接转发。
		return m.next.RoundTrip(req)
	}
	// RoundTripper 契约：不得修改传入请求；克隆后改写副本。
	clone := req.Clone(req.Context())
	u, err := url.Parse(prefix(m.base) + raw)
	if err != nil {
		return nil, fmt.Errorf("上游中间层改写 URL 失败: %w", err)
	}
	clone.URL = u
	// Host 头跟随新 URL（中间层基址）——中间层按拼接形态收到的是发给它自己的请求；
	// 真实上游地址已完整嵌在路径里，由中间层自行解析转发。
	clone.Host = ""
	// RequestURI 是服务端入站概念，出站请求必须为空（Clone 会原样带上）。
	clone.RequestURI = ""
	return m.next.RoundTrip(clone)
}
