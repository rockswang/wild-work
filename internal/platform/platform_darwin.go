//go:build darwin

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

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

// 本 fork 说明：CodeArts 授权走「无痕窗口 + 用户点授权」（internal/app/app.go 调
// DefaultBrowserIncognito/LaunchIncognito），故上游 1e25be4 的死代码清理在本分支不适用，
// 这组函数必须保留；否则 internal/app 编译失败。
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
	plist := autostartPlist(exe)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// 日志重定向目标 data/ 必须先于 plist 加载存在，否则 launchd 打开重定向文件失败（新装场景）。
	if err := os.MkdirAll(filepath.Join(workDirFor(exe), "data"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(plist), 0o644); err != nil {
		return err
	}
	return exec.Command("launchctl", "load", path).Run()
}

// workDirFor 推导数据/工作目录，规则与 cmd/wild-work 的 workDir() 保持一致：
// WILDWORK_HOME > .app bundle（~/Library/Application Support/WildWork）> 二进制所在目录。
// 日志落点必须与 app 实际写入的 state 目录同源，否则 plist 重定向会指向错误路径。
func workDirFor(prog string) string {
	if v := os.Getenv("WILDWORK_HOME"); v != "" {
		return v
	}
	dir := filepath.Dir(prog)
	if strings.Contains(dir, ".app/Contents/MacOS") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "WildWork")
		}
	}
	return dir
}

// autostartPlist 生成开机自启 plist 内容。
//
// 本地补丁（2026-09-19，2026-09-24 重放到 v2.5.2 线）：对齐本机 launchd 实际运行态——
// 无头模式 + KeepAlive + 日志重定向 + WorkingDirectory。上游原模板只写 ``--autostart``
// 且无 KeepAlive/日志，与本机实际加载的定义不一致：一旦 bootout+bootstrap（或重启）
// 按磁盘 plist 重载，服务会退化为「崩溃不自愈、无日志」形态。
//
// 注：prog 为二进制绝对路径；日志与工作目录都从 prog 所在目录推导（main.go 会 chdir 到该目录）。
func autostartPlist(prog string) string {
	dir := workDirFor(prog)
	dataDir := filepath.Join(dir, "data")
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>--no-tray</string>
	</array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>WorkingDirectory</key><string>%s</string>
	<key>StandardOutPath</key><string>%s</string>
	<key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchAgentName, prog, dir,
		filepath.Join(dataDir, "app.log"), filepath.Join(dataDir, "error.log"))
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
