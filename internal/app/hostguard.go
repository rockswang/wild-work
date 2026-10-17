// hostguard.go 面板的入站 Host 校验（2026-10-17 安全修复）。
//
// 威胁：**DNS rebinding**。攻击者页面用自己控制的域名（如 evil.com）承载恶意脚本，
// 先让该域名正常解析、随后把 TTL 压到 0 并重新解析到 127.0.0.1；浏览器于是把请求
// 发到本机的 wild-work，而 **Host 头仍然是 evil.com**。
//
// 为什么既有的两道防线挡不住：
//   - `guardMux` 在「未启用管理密码」时对 `/api/*` **完全放行**，而默认部署
//     （监听 127.0.0.1 + admin_password 为空，R4 允许）正落在这个分支；
//   - 它叠加的 `http.NewCrossOriginProtection()` 判据是**跨站**（Origin / Sec-Fetch-Site），
//     而 rebinding 请求从浏览器视角是**同源**（页面与目标同为 evil.com）⇒ 放行。
//
// 后果：攻击者脚本可读写本机面板——读配置与账号列表、改配置、增删账号、触发对话消耗额度。
// （`/v1/*` 有 API Key 鉴权，不在同一威胁面内；但同层校验对它也无害。）
//
// 口径（严格版，2026-10-17 与用户确认）：
//   - **已设 admin_password** ⇒ 密码即保护，**不做** Host 限制（局域网 / 反向代理访问照常）；
//   - **未设密码** ⇒ 启动期已强制「监听必须环回」（见 `requireRemoteAuth` 与 main.go 的检查），
//     因此这里要求入站 Host 也是环回主机名/IP（`127.0.0.0/8`、`::1`、`localhost`）。
//     代价：用自定义 hosts 名（如 `wildwork.local`）访问本机面板会被拒——这正是安全所必需；
//     确有该用法时，给面板设一个管理密码即可恢复（那时走密码保护这条路径）。
package app

import (
	"log"
	"net"
	"net/http"
	"strings"
)

// hostGuard 在「未启用管理密码」时拒绝 Host 非环回的入站请求；已启用密码时原样放行。
func (a *App) hostGuard(inner http.Handler) http.Handler {
	if inner == nil {
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.panelAuthEnabled() || isLoopbackHost(r.Host) {
			inner.ServeHTTP(w, r)
			return
		}
		// 421 Misdirected Request 的语义正是「该请求被发到了不该由本虚拟主机处理的连接」。
		// 日志保留 Host 与路径以便排查（不含任何凭据）。
		log.Printf("拒绝非环回 Host 的请求（未启用管理密码，疑似 DNS rebinding）host=%s path=%s", r.Host, r.URL.Path)
		http.Error(w, "未启用管理密码时，本机面板只接受环回地址访问", http.StatusMisdirectedRequest)
	})
}

// isLoopbackHost 判定入站 Host（可带端口）是否指向本机。
//
// 只认 `localhost` 字面量与环回 IP：`localhost.localdomain` 之类的变体、自定义 hosts 名、
// 以及任何外部域名一律拒绝——放宽等于把 rebinding 的入口重新打开。
// 空 Host（HTTP/1.0 无 Host 头）同样拒绝，因为「无从判定」不该被当成「安全」。
func isLoopbackHost(hostport string) bool {
	host := strings.TrimSpace(hostport)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
