package chatgpt

// import_test.go 锁死「从 ww3 数据目录找凭据文件」的路径口径（票 08）：
// 用户可能指数据目录、指 auths/ 目录本身、或直接指一个文件；指错时要能明确说「路径不对」。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScanAuthFilesPrefersAuthsSubdir(t *testing.T) {
	dir := t.TempDir()
	auths := filepath.Join(dir, "auths")
	if err := os.MkdirAll(auths, 0o755); err != nil {
		t.Fatal(err)
	}
	// auths/ 里的真凭据（乱序写入，断言输出已排序）
	for _, name := range []string{"b.json", "a.json"} {
		if err := os.WriteFile(filepath.Join(auths, name), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// 噪音：非 json、点文件、子目录、以及数据目录根下的 json（不该被扫）
	for _, name := range []string{"notes.txt", ".hidden.json"} {
		if err := os.WriteFile(filepath.Join(auths, name), []byte(`x`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(auths, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ScanAuthFiles(dir)
	if err != nil {
		t.Fatalf("ScanAuthFiles: %v", err)
	}
	want := []string{filepath.Join(auths, "a.json"), filepath.Join(auths, "b.json")}
	if len(got) != len(want) {
		t.Fatalf("扫到 %d 个文件 %v，期望 %d 个 %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个 = %q，期望 %q（顺序须稳定可复现）", i, got[i], want[i])
		}
	}
}

func TestScanAuthFilesDirectDirAndFile(t *testing.T) {
	// 用户直接指了 auths/ 目录：没有更深一层的 auths/ 子目录，就扫本层 *.json
	dir := t.TempDir()
	p := filepath.Join(dir, "only.json")
	if err := os.WriteFile(p, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ScanAuthFiles(dir)
	if err != nil || len(got) != 1 || got[0] != p {
		t.Fatalf("ScanAuthFiles(dir) = %v（err=%v），期望 [%s]", got, err, p)
	}

	// 用户直接指了单个文件：只试这一个
	got, err = ScanAuthFiles(p)
	if err != nil || len(got) != 1 || got[0] != p {
		t.Fatalf("ScanAuthFiles(file) = %v（err=%v），期望 [%s]", got, err, p)
	}

	// 路径噪声（粘贴带引号 / 前后空白）应被规整掉
	got, err = ScanAuthFiles(`  "` + p + `"  `)
	if err != nil || len(got) != 1 || got[0] != p {
		t.Fatalf("带引号/空白的路径没被规整：%v（err=%v）", got, err)
	}
}

func TestScanAuthFilesMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	_, err := ScanAuthFiles(missing)
	if err == nil {
		t.Fatal("不存在的路径必须报错（否则用户只看到「导入 0 个」）")
	}
	// 锚点：用户要能分辨「路径不对」与「凭据过期」
	if !strings.Contains(err.Error(), "路径不存在") || !strings.Contains(err.Error(), missing) {
		t.Errorf("错误没说清是路径不对：%v", err)
	}

	if _, err := ScanAuthFiles("   "); err == nil {
		t.Error("空路径必须报错")
	}
}

func TestScanAuthFilesEmptyDir(t *testing.T) {
	dir := t.TempDir()
	_, err := ScanAuthFiles(dir)
	if err == nil {
		t.Fatal("目录里没有 .json 时不能静默返回空清单")
	}
	// 提示要指向 auths/，否则用户不知道该指哪一层
	if !strings.Contains(err.Error(), "auths") {
		t.Errorf("空目录的错误没提示 auths/ 子目录：%v", err)
	}
}

func TestScanAuthFilesTildeExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows 的 UserHomeDir 读它
	auths := filepath.Join(home, "ww3", "auths")
	if err := os.MkdirAll(auths, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(auths, "a.json")
	if err := os.WriteFile(p, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ScanAuthFiles("~/ww3")
	if err != nil {
		t.Fatalf("~ 展开失败：%v", err)
	}
	if len(got) != 1 || got[0] != p {
		t.Errorf("ScanAuthFiles(~/ww3) = %v，期望 [%s]", got, p)
	}
}
