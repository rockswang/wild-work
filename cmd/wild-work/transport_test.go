package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 本测试锁一个**真实存在过**的缺陷（R34 同族的「声明了却没接线」）：
//
//	旧的每渠道代理要在**两张**接线表里逐渠道列 client，实证漏过两处——
//	trCodeUp（TraeCode 独立的 traework.Client 实例）从未出现在任何一张表里，
//	面板热更新那张表又漏了 monkeycode/raccoon/loomy。
//	后果是「配了代理但对该渠道不生效」，且**不报错不崩溃**，用户只能猜。
//
// R57 起备用上游通道不再需要接线表（渠道按需自取 backuplink），main.go 里
// 只剩 devdump 落盘包装这一张表。它漏项的代价较小（少一份开发期快照），
// 但同一张表将来承接任何「全渠道统一能力」时就是同一个坑，故仍在此静态锁死：
// **凡注册进 runtimes 的渠道，其 Upstream 实例的 HTTP client 必须出现在 wrapDumps 表里。**
func TestEveryRuntimeUpstreamIsWrapped(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go 失败: %v", err)
	}
	code := string(src)

	// ① runtimes 注册表：`provider.Xxx: {Kind: provider.Xxx, Pool: p, Upstream: uUp, ...}`
	rtRe := regexp.MustCompile(`provider\.(\w+):\s*\{Kind: provider\.\w+,[^}]*?Upstream: (\w+),`)
	rts := rtRe.FindAllStringSubmatch(code, -1)
	if len(rts) < 10 {
		t.Fatalf("runtimes 正则疑似失效（只匹配到 %d 条）", len(rts))
	}

	// ② wrapDumps 接线表：`provider.Xxx.String(): {uUp.HTTP, uUp.StreamHTTP}`
	wrapRe := regexp.MustCompile(`provider\.(\w+)\.String\(\):\s*\{([^}]*)\}`)
	wrapped := map[string]string{}
	for _, m := range wrapRe.FindAllStringSubmatch(code, -1) {
		wrapped[m[1]] = m[2]
	}
	if len(wrapped) == 0 {
		t.Fatal("未找到 wrapDumps 接线表（正则失效？）")
	}

	var missing []string
	for _, m := range rts {
		kind, up := m[1], m[2]
		clients, ok := wrapped[kind]
		if !ok {
			missing = append(missing, kind+"（整条渠道未进 wrapDumps 表）")
			continue
		}
		// 该渠道的 Upstream 实例必须被点名（同名的 StreamHTTP/HTTP 至少各一处）
		for _, field := range []string{".HTTP", ".StreamHTTP"} {
			if !strings.Contains(clients, up+field) {
				missing = append(missing, kind+"（缺 "+up+field+"）")
			}
		}
	}
	if len(missing) > 0 {
		t.Errorf("❌ 以下注册渠道没被 wrapDumps 覆盖：\n  %s\n"+
			"修复：在 main.go 的 wrapDumps 表里补上该渠道的全部 HTTP client（含别名渠道的独立 client 实例）",
			strings.Join(missing, "\n  "))
	}
	t.Logf("runtimes %d 条渠道，wrapDumps 覆盖 %d 条", len(rts), len(wrapped))
}

// 备用上游通道必须是「零接线」形态：main.go 里不得再出现按渠道套代理的代码。
// （防倒退：一旦有人把 backuplink 又改成 SetTransportProxy 式的接线表，R57 的
// 「不参与任何请求」保证就会失效，且重新引入漏接线的可能。）
func TestNoPerChannelProxyWiring(t *testing.T) {
	for _, f := range []string{"main.go", "transport.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", f, err)
		}
		code := string(src)
		for _, banned := range []string{"SetTransportProxy", "applyProxies", "applyProxyToKind", "cfg.Proxies", "next.Proxies"} {
			if strings.Contains(code, banned) {
				t.Errorf("%s 里不应再出现 %q：单渠道代理已于 R57 删除", f, banned)
			}
		}
	}
}
