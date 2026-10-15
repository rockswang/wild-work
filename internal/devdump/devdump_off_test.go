//go:build !devdump

// devdump_off_test.go 发布版契约测试（无 `devdump` 标签）。
//
// 核心承诺（不变量级）：发布版 Wrap 原样返回入参、不动 Transport、
// Enable/Refresh 无效——即「dump 代码不在产物中」，而非「靠环境变量管住」。
// 开发版行为测试见 devdump_on_test.go。
package devdump

import (
	"net/http"
	"testing"
)

// TestWrapIsNoopWithoutTag 发布版 Wrap 必须完全无副作用。
func TestWrapIsNoopWithoutTag(t *testing.T) {
	if IsDevBuild {
		t.Fatal("无标签构建下 IsDevBuild 必须为 false")
	}
	base := http.DefaultTransport
	c := &http.Client{Transport: base}
	if got := Wrap(c, "demo"); got != c {
		t.Fatalf("Wrap 应返回同一个 client：got=%p want=%p", got, c)
	}
	if c.Transport != base {
		t.Fatalf("Wrap 不应替换 Transport：got=%T", c.Transport)
	}
	if Wrap(nil, "demo") != nil {
		t.Fatal("Wrap(nil) 应为 nil")
	}
}

// TestSwitchesAreInertWithoutTag 发布版开关恒关：即使设了环境变量、调了 Enable，
// 也不得开启（发布版没有落盘能力）。
func TestSwitchesAreInertWithoutTag(t *testing.T) {
	t.Setenv("WILDWORK_DUMP_DIR", t.TempDir())
	Refresh()
	Enable(t.TempDir())
	if Enabled() {
		t.Fatal("发布版 Enabled 必须恒为 false")
	}
	Disable()
	Refresh()
	if Enabled() {
		t.Fatal("Refresh 后仍应为 false")
	}
}
