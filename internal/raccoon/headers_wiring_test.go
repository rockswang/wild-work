package raccoon

// 出站头接线的源码级契约测试（R148）。
//
// 为什么用源码断言而不是 mock：ApplyHeaders 是纯函数、没有可注入的工厂，
// 若只测函数本身，实现者完全可能「定义了却不接上线」——那五个单元测试全绿、
// 线上却仍是 Go 默认 UA。这类「定义了但没接线」是反复踩过的坑。

import (
	"os"
	"strings"
	"testing"
)

// TestApplyHeadersWired client.go 里两处带凭据的请求都必须走 ApplyHeaders。
func TestApplyHeadersWired(t *testing.T) {
	src, err := os.ReadFile("client.go")
	if err != nil {
		t.Fatalf("读 client.go 失败: %v", err)
	}
	s := string(src)

	// do() 与 ChatStream() 各一处调用。
	if n := strings.Count(s, "ApplyHeaders(req,"); n < 2 {
		t.Fatalf("ApplyHeaders 调用点只有 %d 处，do() 与 ChatStream() 应各一处", n)
	}
	// 不允许再出现手写的 Authorization —— 那会与 ApplyHeaders 争抢，也说明有漏接处。
	if strings.Contains(s, `req.Header.Set("Authorization"`) {
		t.Error("client.go 仍有手写 Authorization 设置，应统一走 ApplyHeaders")
	}
}
