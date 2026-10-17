//go:build !windows && !darwin

package platform

import (
	"os"
	"os/exec"
)

// InfoBox 其他平台：输出到 stderr。
func InfoBox(title, msg string) { Notify(title, msg) }

// Notify 其他平台：输出到 stderr。
func Notify(title, msg string) {
	_, _ = os.Stderr.WriteString(title + ": " + msg + "\n")
}

// AskYesNo 其他平台：默认否。
func AskYesNo(title, msg string) bool { return false }

// 本 fork 说明：CodeArts 授权走「无痕窗口 + 用户点授权」（internal/app/app.go 调
// DefaultBrowserIncognito/LaunchIncognito），故上游 1e25be4 的死代码清理在本分支不适用，
// 这组函数必须保留；否则 internal/app 编译失败。
// DefaultBrowserIncognito 其他平台：不探测，走系统默认。
func DefaultBrowserIncognito() (exePath, flag string, ok bool) { return "", "", false }

// LaunchIncognito 其他平台：直接 open url。
func LaunchIncognito(exe, flag, url string) error { return OpenURL(url) }

// OpenURL 其他平台：xdg-open。
func OpenURL(url string) error { return exec.Command("xdg-open", url).Start() }

// SetAutostart 其他平台：未实现。
func SetAutostart(on bool) error { return nil }

// AutostartEnabled 其他平台：false。
func AutostartEnabled() bool { return false }

// OpenWithTextEditor 其他平台：xdg-open。
func OpenWithTextEditor(path string) error { return exec.Command("xdg-open", path).Start() }
