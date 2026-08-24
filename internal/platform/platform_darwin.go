//go:build darwin

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// WorkArea macOS：主屏可见区域（不含菜单栏/Dock）近似取全屏，供面板定位参考。
func WorkArea() (int32, int32, int32, int32) {
	return 0, 0, 1440, 900
}

// InfoBox macOS：用 osascript 弹对话框。
func InfoBox(title, msg string) {
	_ = exec.Command("osascript", "-e",
		fmt.Sprintf(`display dialog %q with title %q buttons {"OK"} default button "OK"`, msg, title)).Start()
}

// Notify macOS：系统通知。
func Notify(title, msg string) {
	_ = exec.Command("osascript", "-e",
		fmt.Sprintf(`display notification %q with title %q`, msg, title)).Start()
}

// AskYesNo macOS：osascript 是/否对话框。
func AskYesNo(title, msg string) bool {
	out, err := exec.Command("osascript", "-e",
		fmt.Sprintf(`display dialog %q with title %q buttons {"否","是"} default button "是" cancel button "否"`, msg, title)).
		Output()
	return err == nil && strings.Contains(string(out), "button returned:是")
}

// ---------------------------------------------------------------------------
// 浏览器
// ---------------------------------------------------------------------------

var browserApps = []struct {
	bundle string
	name   string
	flag   string
}{
	{"com.google.Chrome", "Google Chrome", "--incognito"},
	{"com.microsoft.edgemac", "Microsoft Edge", "--inprivate"},
	{"org.mozilla.firefox", "Firefox", "-private-window"},
	{"com.apple.Safari", "Safari", ""},
}

// DefaultBrowserIncognito 返回系统默认浏览器的无痕启动方式。
// 简化实现：探测已安装浏览器，按 Chrome→Edge→Firefox→Safari 顺序。
func DefaultBrowserIncognito() (exePath, flag string, ok bool) {
	for _, b := range browserApps {
		if appExists(b.name) {
			return b.name, b.flag, true
		}
	}
	return "", "", false
}

func appExists(name string) bool {
	_, err := os.Stat(filepath.Join("/Applications", name+".app"))
	return err == nil
}

// LaunchIncognito 用 open -na 以无痕模式打开。
func LaunchIncognito(app, flag, url string) error {
	args := []string{"-na", app}
	if flag != "" {
		args = append(args, "--args", flag, url)
	} else {
		args = append(args, "--args", url)
	}
	return exec.Command("open", args...).Start()
}

// OpenURL 用系统默认方式打开 URL。
func OpenURL(url string) error {
	return exec.Command("open", url).Start()
}

// ---------------------------------------------------------------------------
// 开机自启（LaunchAgent plist）
// ---------------------------------------------------------------------------

const launchAgentName = "com.rockswang.wild-work"

func launchAgentPath() string {
	return filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", launchAgentName+".plist")
}

// SetAutostart 写/删 ~/Library/LaunchAgents plist。
func SetAutostart(on bool) error {
	path := launchAgentPath()
	if !on {
		_ = exec.Command("launchctl", "unload", path).Run()
		return os.Remove(path)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>--autostart</string>
	</array>
	<key>RunAtLoad</key><true/>
</dict>
</plist>
`, launchAgentName, exe)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return err
	}
	return exec.Command("launchctl", "load", path).Run()
}

// AutostartEnabled 检查 plist 是否存在且路径匹配。
func AutostartEnabled() bool {
	raw, err := os.ReadFile(launchAgentPath())
	if err != nil {
		return false
	}
	exe, err := os.Executable()
	if err == nil && strings.Contains(string(raw), exe) {
		return true
	}
	return false
}

// OpenWithTextEditor 用 TextEdit 打开文件。
func OpenWithTextEditor(path string) error {
	return exec.Command("open", "-a", "TextEdit", path).Start()
}
