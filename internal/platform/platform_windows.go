//go:build windows

package platform

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	user32 = windows.NewLazySystemDLL("user32.dll")
	procSystemParametersInfo = user32.NewProc("SystemParametersInfoW")
)

const spiGetWorkArea = 0x0030

// WorkArea 返回主显示器工作区（不含任务栏）。
func WorkArea() (int32, int32, int32, int32) {
	var rect struct{ Left, Top, Right, Bottom int32 }
	procSystemParametersInfo.Call(uintptr(spiGetWorkArea), 0, uintptr(unsafe.Pointer(&rect)), 0)
	return rect.Left, rect.Top, rect.Right - rect.Left, rect.Bottom - rect.Top
}

// InfoBox 弹信息提示框。
func InfoBox(title, msg string) {
	msgBox(title, msg, 0x40 /* MB_ICONINFORMATION */)
}

// Notify 弹信息提示框。
func Notify(title, msg string) { InfoBox(title, msg) }

// AskYesNo 弹"是/否"框，true=是。
func AskYesNo(title, msg string) bool {
	return msgBox(title, msg, 0x24 /* MB_YESNO|MB_ICONQUESTION */) == 6 /* IDYES */
}

func msgBox(title, msg string, flags uintptr) uintptr {
	proc := user32.NewProc("MessageBoxW")
	caption, _ := syscall.UTF16PtrFromString(title)
	text, _ := syscall.UTF16PtrFromString(msg)
	// MB_TOPMOST(0x40000) + MB_SETFOREGROUND(0x10000) + MB_DEFAULT_DESKTOP_ONLY(0x20000)
	// 强制主显示器前台显示，避免双显示器下弹窗位置飘忽
	res, _, _ := proc.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(caption)), flags|0x40000|0x10000|0x20000)
	return res
}

// ---------------------------------------------------------------------------
// 浏览器
// ---------------------------------------------------------------------------

var browserSpecs = []struct {
	progID string
	exe    string
	flag   string
}{
	{"ChromeHTML", "chrome.exe", "--incognito"},
	{"MSEdgeHTM", "msedge.exe", "--inprivate"},
	{"EdgeChromiumHTM", "msedge.exe", "--inprivate"},
	{"FirefoxURL", "firefox.exe", "-private-window"},
}

// DefaultBrowserIncognito 探测默认浏览器的无痕启动方式。
func DefaultBrowserIncognito() (exePath, flag string, ok bool) {
	pid := defaultProgID()
	for _, b := range browserSpecs {
		if pid == "" || strings.EqualFold(pid, b.progID) {
			if p := findBrowserExe(b.exe); p != "" {
				return p, b.flag, true
			}
		}
	}
	for _, b := range browserSpecs {
		if p := findBrowserExe(b.exe); p != "" {
			return p, b.flag, true
		}
	}
	return "", "", false
}

func defaultProgID() string {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\Shell\Associations\UrlAssociations\http\UserChoice`,
		registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("ProgId")
	if err != nil {
		return ""
	}
	return v
}

func findBrowserExe(exe string) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE,
		`SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\`+exe,
		registry.QUERY_VALUE)
	if err == nil {
		v, _, err2 := k.GetStringValue("")
		k.Close()
		if err2 == nil && v != "" && fileExists(v) {
			return v
		}
	}
	rel := map[string][]string{
		"chrome.exe":  {"Google", "Chrome", "Application", "chrome.exe"},
		"msedge.exe":  {"Microsoft", "Edge", "Application", "msedge.exe"},
		"firefox.exe": {"Mozilla", "Firefox", "firefox.exe"},
	}[exe]
	for _, base := range []string{
		os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LocalAppData"),
	} {
		if base == "" || rel == nil {
			continue
		}
		cand := filepath.Join(append([]string{base}, rel...)...)
		if fileExists(cand) {
			return cand
		}
	}
	return ""
}

// LaunchIncognito 以无痕模式启动浏览器打开 url。
func LaunchIncognito(exe, flag, url string) error {
	cmd := exec.Command(exe, flag, url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// OpenURL 用系统默认方式打开 URL。
func OpenURL(url string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// ---------------------------------------------------------------------------
// 开机自启（注册表 Run 键）
// ---------------------------------------------------------------------------

const autostartKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const autostartValue = "wild-work"

// SetAutostart 设置/取消当前 exe 的开机自启。
func SetAutostart(on bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue(autostartValue); err != nil && err != registry.ErrNotExist {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue(autostartValue, `"`+exe+`" --autostart`)
}

// AutostartEnabled 查询当前 exe 是否已设置开机自启。
func AutostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(autostartValue)
	if err != nil {
		return false
	}
	if exe, err := os.Executable(); err == nil {
		return strings.HasPrefix(v, `"`+exe+`"`)
	}
	return v != ""
}

// OpenWithTextEditor 用记事本打开文件。
func OpenWithTextEditor(path string) error {
	cmd := exec.Command("notepad.exe", path)
	return cmd.Start()
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

