// Package winutil Windows 平台小工具：工作区、任务栏隐藏、无痕浏览器拉起、开机自启。
// 仅支持 Windows amd64（本项目分发目标）。
package winutil

import (
	"fmt"
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
	procFindWindowW          = user32.NewProc("FindWindowW")
	procGetWindowLongPtrW    = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW    = user32.NewProc("SetWindowLongPtrW")
	procSetForegroundWindow  = user32.NewProc("SetForegroundWindow")
	procBringWindowToTop     = user32.NewProc("BringWindowToTop")
	procGetWindowRect        = user32.NewProc("GetWindowRect")
)

const (
	spiGetWorkArea = 0x0030
	wsExToolWindow = 0x00000080
)

// gwlExStyle GWL_EXSTYLE 索引；负数，需运行时转换，故用 var。
var gwlExStyle = int32(-20)

// WorkArea 返回主显示器工作区（不含任务栏）的 (x, y, w, h)。
func WorkArea() (int32, int32, int32, int32) {
	var rect struct{ Left, Top, Right, Bottom int32 }
	procSystemParametersInfo.Call(uintptr(spiGetWorkArea), 0, uintptr(unsafe.Pointer(&rect)), 0)
	return rect.Left, rect.Top, rect.Right - rect.Left, rect.Bottom - rect.Top
}

// MainWindow 返回 wails 主窗口句柄（类名 wailsWindow）。
func MainWindow() uintptr {
	className, _ := syscall.UTF16PtrFromString("wailsWindow")
	hwnd, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(className)), 0)
	return hwnd
}

// HideFromTaskbar 通过 WS_EX_TOOLWINDOW 隐藏窗口的任务栏按钮。
func HideFromTaskbar(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	style, _, _ := procGetWindowLongPtrW.Call(hwnd, exStyleArg())
	style |= wsExToolWindow
	procSetWindowLongPtrW.Call(hwnd, exStyleArg(), style)
}

// exStyleArg 返回 GWL_EXSTYLE 的无符号形式（-20 常量不能直接转 uintptr）。
func exStyleArg() uintptr {
	return uintptr(int32(gwlExStyle))
}

// FocusWindow 尝试把窗口带到前台（面板弹出时确保焦点）。
// SetForegroundWindow 受前台锁限制，调用方通常在收到托盘输入后调用，可正常生效。
func FocusWindow(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	procSetForegroundWindow.Call(hwnd)
	procBringWindowToTop.Call(hwnd)
}

// MessageBox 弹出一个错误提示框（GUI 应用无控制台，致命错误需可见）。
func MessageBox(msg string) {
	InfoBoxEx("WorkBuddy2API", msg, 0x10 /* MB_ICONERROR */)
}

// InfoBox 弹出一个信息提示框（MB_ICONINFORMATION）。
func InfoBox(title, msg string) {
	InfoBoxEx(title, msg, 0x40 /* MB_ICONINFORMATION */)
}

// PanelAnchor 计算右下角弹出面板的位置：贴任务栏（避免被较宽/自动隐藏的任务栏遮挡），
// 右、下各留 gap 间隙。任务栏探测失败时退回工作区右下角。
func PanelAnchor(panelW, panelH int) (int, int) {
	const gap = 12
	waX, waY, waW, waH := WorkArea()
	x, y, w, h := int(waX), int(waY), int(waW), int(waH)
	if tbx, tby, tbw, tbh, ok := taskbarRect(); ok && tbw > 0 && tbh > 0 {
		if int(tbw) > int(tbh) {
			// 水平任务栏（顶部或底部）
			if int(tby) <= y+h/2 { // 顶部
				return x + w - panelW - gap, int(tby) + int(tbh) + gap
			}
			return x + w - panelW - gap, int(tby) - panelH - gap
		}
		// 垂直任务栏（左侧或右侧）
		if int(tbx) <= x+w/2 { // 左侧
			return int(tbx) + int(tbw) + gap, y + h - panelH - gap
		}
		return int(tbx) - panelW - gap, y + h - panelH - gap
	}
	return x + w - panelW - gap, y + h - panelH - gap
}

// taskbarRect 获取主任务栏（Shell_TrayWnd）屏幕矩形。
func taskbarRect() (x, y, w, h int32, ok bool) {
	cls, _ := syscall.UTF16PtrFromString("Shell_TrayWnd")
	hwnd, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(cls)), 0)
	if hwnd == 0 {
		return 0, 0, 0, 0, false
	}
	var r struct{ Left, Top, Right, Bottom int32 }
	if ret, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r))); ret == 0 {
		return 0, 0, 0, 0, false
	}
	return r.Left, r.Top, r.Right - r.Left, r.Bottom - r.Top, true
}

// InfoBoxEx 底层 MessageBoxW 封装。
func InfoBoxEx(title, msg string, flags uintptr) {
	proc := user32.NewProc("MessageBoxW")
	caption, _ := syscall.UTF16PtrFromString(title)
	text, _ := syscall.UTF16PtrFromString(msg)
	proc.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(caption)), flags)
}

// AskYesNo 弹“是/否”选择框（MB_YESNO|MB_ICONQUESTION），返回 true=是。
func AskYesNo(title, msg string) bool {
	proc := user32.NewProc("MessageBoxW")
	caption, _ := syscall.UTF16PtrFromString(title)
	text, _ := syscall.UTF16PtrFromString(msg)
	res, _, _ := proc.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(caption)), 0x24 /* MB_YESNO|MB_ICONQUESTION */)
	return res == 6 // IDYES
}

// ---------------------------------------------------------------------------
// 无痕浏览器
// ---------------------------------------------------------------------------

// browserSpec 浏览器 progId → exe 名 + 无痕参数。
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

// DefaultBrowserIncognito 探测默认浏览器的无痕启动方式，返回 (exe路径, 无痕参数, ok)。
// 默认浏览器无法识别时按 Edge→Chrome→Firefox 顺序回退探测已安装浏览器。
func DefaultBrowserIncognito() (exePath, flag string, ok bool) {
	pid := defaultProgID()
	for _, b := range browserSpecs {
		if pid == "" || strings.EqualFold(pid, b.progID) {
			if p := findBrowserExe(b.exe); p != "" {
				return p, b.flag, true
			}
		}
	}
	// 回退：探测常见浏览器
	for _, b := range browserSpecs {
		if p := findBrowserExe(b.exe); p != "" {
			return p, b.flag, true
		}
	}
	return "", "", false
}

// defaultProgID 读注册表默认浏览器 ProgId（如 ChromeHTML）。
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

// findBrowserExe 依次从 App Paths 注册表与常见安装路径定位浏览器。
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

// LaunchIncognito 以无痕模式启动浏览器打开 url（不等待、隐藏控制台窗口）。
func LaunchIncognito(exe, flag, url string) error {
	cmd := exec.Command(exe, flag, url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// OpenURL 用系统默认方式打开 URL（兜底，普通模式）。
func OpenURL(url string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// ---------------------------------------------------------------------------
// 开机自启（注册表 Run 键）
// ---------------------------------------------------------------------------

const autostartKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const autostartValue = "WorkBuddy2API"

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
	// 附带 --autostart 标志：开机启动时应用不弹“已启动”提示
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

// OpenFile 用系统默认关联程序打开文件。
func OpenFile(path string) error {
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Start()
}

// OpenWithNotepad 用系统记事本打开文件。
// 注意：不能设 HideWindow —— 那会让 notepad 以隐藏窗口启动（看起来“无效果”）。
func OpenWithNotepad(path string) error {
	cmd := exec.Command("notepad.exe", path)
	return cmd.Start()
}

// IsWebViewProfileLocked 报告 WebView2 用户数据目录是否被占用
// （Chromium 以 SingletonLock 标记 profile 正被某个浏览器进程使用）。
func IsWebViewProfileLocked(userDataPath string) bool {
	_, err := os.Stat(filepath.Join(userDataPath, "SingletonLock"))
	return err == nil
}

// KillOrphanWebViews 结束占用指定 user-data-dir 的孤儿 msedgewebview2 进程。
// 必须在创建本进程 WebView 之前调用：此时所有带该目录的 webview 进程都是强杀残留。
func KillOrphanWebViews(userDataPath string) {
	escaped := strings.ReplaceAll(userDataPath, "'", "''")
	escaped = strings.ReplaceAll(escaped, "[", "`[") // 转义 -like 通配符
	escaped = strings.ReplaceAll(escaped, "]", "`]")
	ps := fmt.Sprintf(
		`Get-CimInstance Win32_Process -Filter "Name='msedgewebview2.exe'" | Where-Object { $_.CommandLine -like '*%s*' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }`,
		escaped)
	cmd := exec.Command("powershell.exe", "-NoProfile", "-WindowStyle", "Hidden", "-Command", ps)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Run()
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
