// protocol.go 小浣熊「桌面协议回调」的跨平台部分：深链解析 + 回调载荷落盘。
//
// 背景：小浣熊的网页授权把授权码通过**深链**回传（逆向自 desktopLogin.js 与服务端 SPA，见
// 本地 docs/raccoon渠道接入备忘.md §11）：
//
//	① 打开 https://xiaohuanxiong.com/code/authorize?login_source=desktop&appname=办公小浣熊客户端
//	② 用户在系统浏览器完成登录
//	③ 页面跳 office-raccoon://auth/callback?code=<授权码>&state=<可选>
//	④ 该深链由 HKCU\Software\Classes\office-raccoon 注册的协议处理器接收
//	   （官方值指向「商汤小浣熊.exe」，本包在登录期间临时指向 wild-work 自身）
//	⑤ 拿到 code 后 POST {authApi}/login_with_authorization_code 兑换 token
//
// 桌面客户端走的就是上述这条链路，本包只是把第④步的协议处理方临时换成本工具，
// 以便在用户用浏览器完成官方授权后接住回调（官方客户端本身不参与，故登录期间
// 请勿启动它 —— 它会重新注册该协议）。
//
// 平台相关的注册表读写见 protocol_windows.go；本文件只放纯逻辑与文件读写，
// 因此在任意平台都能被测试覆盖。
package raccoon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// ProtocolScheme 小浣熊登录回调使用的自定义协议。
	ProtocolScheme = "office-raccoon"
	// ProtocolHost / ProtocolPath 官方客户端对深链的校验值
	// （desktopLogin.js parseDesktopLoginCode：protocol==='office-raccoon:'
	//  && hostname==='auth' && pathname==='/callback'）。
	ProtocolHost = "auth"
	ProtocolPath = "/callback"

	// ProtocolKeyPath 协议注册表键（HKCU 下）。官方客户端安装时写入，启动时可能重新注册。
	ProtocolKeyPath = `Software\Classes\office-raccoon`

	// CallbackFileName 协议处理器子进程把深链参数落盘的文件名（位于 data/ 下）。
	CallbackFileName = "raccoon-callback.json"
	// BackupFileName 注册表备份文件名（位于 data/ 下）。
	// 它同时是「是否存在未完成改写」的判据：启动时若存在则自愈恢复。
	BackupFileName = "raccoon-protocol-backup.json"

	// MainSite 主站（授权页与兑换端点所在站点）。
	MainSite = "https://xiaohuanxiong.com"

	// EpAuthCodeWeb / EpAuthCodeElectron 授权码兑换端点。
	// 官方 getAuthApiUrl() 优先用 NEXT_PUBLIC_DESKTOP_REMOTE_AUTH_API_PREFIX（web 面），
	// 为空才回落 electron 面；两者实测都可用，故按序尝试。
	EpAuthCodeWeb      = "/api/web/auth/v1/login_with_authorization_code"
	EpAuthCodeElectron = "/api/electron/auth/v1/login_with_authorization_code"
)

// CallbackFlag 注入协议注册表命令行的标志位：Windows 以
// `"<exe>" --raccoon-callback "<深链>"` 唤起本进程，入口层据此走「只落盘后退出」分支
// （不能让它启动第二份服务/托盘）。
const CallbackFlag = "--raccoon-callback"

// ErrProtocolUnsupported 协议回调接管当前仅在 Windows 实现（HKCU 协议注册）；
// 其他平台尚未实现，欢迎有对应设备的社区贡献者补齐。
var ErrProtocolUnsupported = errors.New("小浣熊授权登录当前仅支持 Windows")

// AuthorizeURL 授权入口（desktopLogin.js 的 DESKTOP_AUTH_PATH + DESKTOP_AUTH_PARAMS）。
// appname 走 percent-encoding，避免非 ASCII 在 URL 里出现歧义。
var AuthorizeURL = MainSite + "/code/authorize?login_source=desktop&appname=" +
	url.QueryEscape("办公小浣熊客户端")

// CallbackPayload 协议处理器收到的深链参数（落盘形态）。
//
// 刻意**不落盘原始深链**：它内含一次性授权码，没有必要多留一份副本。
type CallbackPayload struct {
	Code       string `json:"code,omitempty"`
	State      string `json:"state,omitempty"`
	Err        string `json:"err,omitempty"`
	ReceivedAt int64  `json:"receivedAt"`
}

// StateDir 归一化 state 目录（空值兜底为 data，与 config.Default 的 ./data/state.json 一致）。
func StateDir(dir string) string {
	if d := strings.TrimSpace(dir); d != "" {
		return d
	}
	return "data"
}

// CallbackPath 回调载荷文件路径。
func CallbackPath(stateDir string) string {
	return filepath.Join(StateDir(stateDir), CallbackFileName)
}

// BackupPath 注册表备份文件路径。
func BackupPath(stateDir string) string {
	return filepath.Join(StateDir(stateDir), BackupFileName)
}

// ParseCallbackURL 解析 office-raccoon://auth/callback?code=… 深链。
//
// scheme/host/path 任一不符即判为非登录回调（对齐官方客户端的校验口径）；
// 返回的错误文案**不含授权码**，可安全写日志。
func ParseCallbackURL(raw string) (CallbackPayload, error) {
	p := CallbackPayload{ReceivedAt: time.Now().Unix()}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return p, errors.New("深链为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return p, fmt.Errorf("深链解析失败：%w", err)
	}
	if !strings.EqualFold(u.Scheme, ProtocolScheme) ||
		!strings.EqualFold(u.Host, ProtocolHost) ||
		u.Path != ProtocolPath {
		return p, fmt.Errorf("不是小浣熊登录回调（期望 %s://%s%s）", ProtocolScheme, ProtocolHost, ProtocolPath)
	}
	q := u.Query()
	p.Code = strings.TrimSpace(q.Get("code"))
	p.State = strings.TrimSpace(q.Get("state"))
	if e := strings.TrimSpace(q.Get("error")); e != "" {
		desc := strings.TrimSpace(q.Get("error_description"))
		if desc != "" {
			e = e + " (" + desc + ")"
		}
		return p, fmt.Errorf("授权被拒绝：%s", e)
	}
	if p.Code == "" {
		return p, errors.New("回调缺少 code 参数")
	}
	return p, nil
}

// SaveCallback 协议处理器子进程调用：解析深链并原子落盘。
//
// 解析失败也照样落盘（带 Err 字段），这样主进程能感知到「回调来过但不可用」，
// 从而及时报错并恢复注册表，而不是一直等到超时。
func SaveCallback(stateDir, rawURL string) error {
	p, perr := ParseCallbackURL(rawURL)
	if perr != nil {
		p.Err = perr.Error()
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := StateDir(stateDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建目录失败：%w", err)
	}
	fp := CallbackPath(dir)
	tmp := fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("写入回调载荷失败：%w", err)
	}
	if err := os.Rename(tmp, fp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("落盘回调载荷失败：%w", err)
	}
	return nil
}

// ClearCallback 删除回调载荷（发起登录前清残留、登录结束后清理）。
func ClearCallback(stateDir string) error {
	err := os.Remove(CallbackPath(stateDir))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
