package upstream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoAdhocDataPrefixParsing 断言「入站 SSE 帧解析只有 sseDataPayload 一处实现」。
//
// 背景（2026-10-18）：LobsterAI 6 账号 6/6 全坏 —— 上游发 `data:{...}`（冒号后无空格），
// 而 svse.go 里两处解析只认 `data: `（带空格）：
//   - Aggregate：一帧都认不出 ⇒ validEvents=0 ⇒ 502 upstream_parse
//   - StreamCapture：帧原样透传但不计有效数 ⇒ 追加伪造的 empty upstream stream 错误帧
//
// 而 errEmptyStream 会被上层当成「账号连续错误」⇒ 健康账号被冷却。
//
// 这个 bug 的本质是「同一条判据散落多处、其中一处写窄了」。修法除了改正，还要让
// 「再写一份窄判据」在结构上被拦下：任何新解析点必须走 sseDataPayload。
//
// 本测试扫描 upstream 包内所有非测试 Go 文件：除 sseDataPayload 定义处外，
// 不得出现 `HasPrefix(..., "data:` 或 `TrimPrefix(..., "data:` 的入站解析。
func TestNoAdhocDataPrefixParsing(t *testing.T) {
	// 允许出现 data: 前缀判断的文件与（简化的）场景：
	// 写出路径用 `io.WriteString(w, "data: "+payload)`，形态不同，不会命中下面的正则。
	allowed := map[string]bool{
		"sse.go": true, // sseDataPayload 的唯一实现处
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("列举文件失败：%v", err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || allowed[f] {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("读 %s 失败：%v", f, err)
		}
		for i, ln := range strings.Split(string(raw), "\n") {
			code := ln
			if idx := strings.Index(code, "//"); idx >= 0 {
				code = code[:idx]
			}
			// 只抓「解析入站帧」的形态：HasPrefix/TrimPrefix 的第二个参数是 data: 字面量。
			if !strings.Contains(code, `"data:`) {
				continue
			}
			if !strings.Contains(code, "HasPrefix") && !strings.Contains(code, "TrimPrefix") {
				continue
			}
			t.Errorf("❌ %s:%d 自行解析 data: 前缀（应统一走 sseDataPayload，否则窄判据会复发）：\n     %s",
				f, i+1, strings.TrimSpace(ln))
		}
	}
}
