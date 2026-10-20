package app

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestFrontendAPIPathsHaveRoutes 前端 app.js 里调用的每个 `/api/…` 路径，后端都必须
// 有对应路由 —— 否则面板上那个按钮/开关点下去必 404，而且**编译与静态检查都发现不了**。
//
// 为什么要这条守门（本仓特有）：`cmd/wild-work/web` 三件套是本仓自维护的（B 决策），
// 上游改后端端点时不会连带改我们的前端，历史已踩两次：
//   - R57 把「每渠道上游代理」换成单一「备用上游通道」（`POST /api/config/proxy`）后，
//     面板仍调已删除的 `POST /api/config/proxies` ⇒ 「渠道上游代理」保存永远失败，
//     连同一个请求里的 OC Zen Key 也一起存不进去（2026-10-21 修，此前一直静默）。
//   - 上游 R100 在 app.go 注册 `GET /api/stats`，合并时整块被吞 ⇒ 运行统计面板 404。
//
// 判据取**源码里的路由注册**而不是 `ServeMux.Handler`：HandleAPI 的形态是
// 「外层 mux 挂 `/api/auth/*` 具体路由 + `/api/` 兜底 → 内层 api mux 挂全部业务路由」，
// 因此 `mux.Handler(req)` 对任何业务路径都只回兜底 pattern `/api/`，判「非空」会恒真
// （实测踩过：改成 `pattern != "/api/"` 后正向用例仍红，因为业务路由根本不在外层）。
// 扫源码则不受注册层次影响，也没有执行 handler 的副作用。
func TestFrontendAPIPathsHaveRoutes(t *testing.T) {
	src, err := os.ReadFile("../../cmd/wild-work/web/app.js")
	if err != nil {
		t.Fatalf("读 app.js 失败：%v", err)
	}
	// 只认**双引号包裹**的字面量路径：真实调用形如 api("/api/xxx", body)。
	// 注释里的裸文本路径（如「旧端点 POST /api/config/proxies 已删除」）不会被抓，
	// 所以注释可以放心记录历史端点名。
	re := regexp.MustCompile(`"(/api/[A-Za-z0-9/_-]*)"`)
	seen := map[string]bool{}
	var paths []string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		p := m[1]
		if p == "" || p == "/api/" || seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	// 解析下限守门：app.js 形态若大改（比如全改成模板字符串），正则静默失效会变成
	// 「0 个路径全部通过」的假绿 —— 宁可失败让人来改判据。
	if len(paths) < 20 {
		t.Fatalf("只解析出 %d 个 /api/ 路径，疑似判据失效（app.js 调用形态变了？）", len(paths))
	}

	routes := goRoutePatterns(t)
	// 注册表下限守门：正则失效同样会假绿。
	if len(routes) < 40 {
		t.Fatalf("只解析出 %d 条后端路由，疑似判据失效（注册形态变了？）", len(routes))
	}
	for _, p := range paths {
		if !routes[p] {
			t.Errorf("❌ 前端调用的 %s 在后端没有路由（端点被改名/删除，面板上该功能点下去必 404）", p)
		}
	}
}

// goRoutePatterns 从本包与 internal/server 的非测试源码里抓取全部路由注册的路径部分
// （`mux.HandleFunc("POST /api/x", …)` / `mux.Handle("/api/x", …)` 的第二个参数），
// 去掉方法前缀后返回集合。cwd 是 internal/app（go test 的包目录），故用相对路径。
func goRoutePatterns(t *testing.T) map[string]bool {
	t.Helper()
	re := regexp.MustCompile(`\.Handle(?:Func)?\("((?:(?:GET|POST|PUT|DELETE|PATCH) )?/[^"]*)"`)
	out := map[string]bool{}
	for _, dir := range []string{".", "../server"} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("读目录 %s 失败：%v", dir, err)
		}
		for _, e := range ents {
			name := e.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("读 %s/%s 失败：%v", dir, name, err)
			}
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				p := m[1]
				if i := strings.IndexByte(p, ' '); i >= 0 {
					p = p[i+1:] // 去掉 "POST " 之类的方法前缀
				}
				out[p] = true
			}
		}
	}
	return out
}
