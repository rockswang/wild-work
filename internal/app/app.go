// Package app wails 绑定层：向托盘面板前端暴露账号/签到/积分/配置操作，
// 内部驱动 pool / scheduler / upstream / HTTP 服务。
package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/config"
	"github.com/rockswang/workbuddy-wild/internal/login"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/scheduler"
	"github.com/rockswang/workbuddy-wild/internal/server"
	"github.com/rockswang/workbuddy-wild/internal/upstream"
	"github.com/rockswang/workbuddy-wild/internal/winutil"
)

// Version 面板展示的版本号。
const Version = "0.1.0"

const (
	loginTimeout   = 5 * time.Minute
	loginPollEvery = 2 * time.Second
)

// Options 构建 App 的依赖。
type Options struct {
	ConfigPath string
	Config     *config.Config
	Pool       *pool.Pool
	Upstream   *upstream.Client
	Scheduler  *scheduler.Scheduler
	Handler    *server.Handler
}

// App 托盘面板后端。
type App struct {
	cfgPath string
	cfg     *config.Config
	pool    *pool.Pool
	up      *upstream.Client
	sch     *scheduler.Scheduler
	handler *server.Handler

	mu      sync.Mutex // 保护 httpSrv / cfg 修改
	httpSrv *http.Server

	ctx context.Context // wails runtime ctx（OnStartup 后可用）

	muLogin      sync.Mutex
	loginBusy    bool
	loginCtx     context.Context
	loginCancel  context.CancelFunc
	loginClient  *http.Client
	loginStateFP string

	logFile *os.File
}

// New 构建 App 并接管全局日志（写文件 + 环形缓冲 + 事件推送）。
func New(opts Options) (*App, error) {
	a := &App{
		cfgPath: opts.ConfigPath,
		cfg:     opts.Config,
		pool:    opts.Pool,
		up:      opts.Upstream,
		sch:     opts.Scheduler,
		handler: opts.Handler,
	}
	a.loginStateFP = filepath.Join(filepath.Dir(opts.Config.StateFile), "login-state.json")

	// 日志文件 data/app.log（与 state.json 同目录）
	logFP := filepath.Join(filepath.Dir(opts.Config.StateFile), "app.log")
	if err := os.MkdirAll(filepath.Dir(logFP), 0o755); err == nil {
		f, err := os.OpenFile(logFP, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			a.logFile = f
		}
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(&logWriter{app: a})
	return a, nil
}

// Close 关闭日志文件。
func (a *App) Close() {
	if a.logFile != nil {
		_ = a.logFile.Close()
		a.logFile = nil
	}
}

// ---------------------------------------------------------------------------
// 生命周期
// ---------------------------------------------------------------------------

// StartServer 按当前配置启动 HTTP 服务（监听失败返回错误）。
func (a *App) StartServer() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.serveLocked(a.cfg.Listen.Addr())
}

// serveLocked 在 newAddr 上启动新服务并切换；调用方需持有 a.mu。
func (a *App) serveLocked(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           a.handler,
		ReadHeaderTimeout: 30 * time.Second,
	}
	old := a.httpSrv
	a.httpSrv = srv
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("http serve %s: %v", addr, err)
		}
	}()
	if old != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = old.Shutdown(shutdownCtx)
	}
	return nil
}

// OnStartup wails 窗口就绪回调：定位面板、隐藏任务栏按钮、推送初始状态。
func (a *App) OnStartup(ctx context.Context) {
	a.ctx = ctx
	a.positionPanel()
	winutil.HideFromTaskbar(winutil.MainWindow())
	log.Printf("workbuddy2api %s 已启动，API 监听 %s（账号 %d）",
		Version, a.cfg.Listen.Addr(), len(a.pool.List()))
	a.emitAccounts()
}

// OnDomReady 前端就绪回调（启动提示已提前到 main 阶段，这里不再处理）。
func (a *App) OnDomReady(ctx context.Context) {}

// ShowStartupNotice 弹出“已启动 + API 地址”提示框。
// 由 main 在 StartServer 后立即调用（早于托盘/窗口初始化），体验即时。
func (a *App) ShowStartupNotice() {
	a.safeGo(a.showStartupNotice)
}

// ShowSecondInstanceNotice 已运行实例被再次双击：询问打开面板或退出
// （面板异常时仍可从这里退出，保证“退得出去”）。
func (a *App) ShowSecondInstanceNotice() {
	a.safeGo(func() {
		host := a.cfg.Listen.Host
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		if winutil.AskYesNo("WorkBuddy2API 已启动",
			fmt.Sprintf("OpenAI 兼容 API 地址：\nhttp://%s:%d\n\n是否打开管理面板？\n（选择“否”将退出程序）", host, a.cfg.Listen.Port)) {
			a.ShowPanel()
		} else {
			a.Quit()
		}
	})
}

// showStartupNotice 显示“仅可关闭”的启动提示框。
func (a *App) showStartupNotice() {
	host := a.cfg.Listen.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1" // 面板/提示里展示本机可达地址
	}
	winutil.InfoBox("WorkBuddy2API 已启动",
		fmt.Sprintf("OpenAI 兼容 API 地址：\nhttp://%s:%d\n\n点击右下角托盘图标打开管理面板。", host, a.cfg.Listen.Port))
}

// OnShutdown wails 退出回调：关闭 HTTP 服务与日志。
func (a *App) OnShutdown(ctx context.Context) {
	a.mu.Lock()
	srv := a.httpSrv
	a.httpSrv = nil
	a.mu.Unlock()
	if srv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}
	a.Close()
}

// panelRect 计算面板最终位置（贴主任务栏/工作区右下角，右、下留 12px 间隙）。
func (a *App) panelRect() (x, y, pw, ph int) {
	pw, ph = 270, 640
	_, _, waW, waH := winutil.WorkArea()
	if int(waW) < pw {
		pw = int(waW)
	}
	if int(waH) < ph {
		ph = int(waH)
	}
	x, y = winutil.PanelAnchor(pw, ph)
	return x, y, pw, ph
}

// positionPanel 把面板定位到右下角（贴任务栏）。
func (a *App) positionPanel() {
	if a.ctx == nil {
		return
	}
	fx, fy, pw, ph := a.panelRect()
	runtime.WindowSetSize(a.ctx, pw, ph)
	runtime.WindowSetPosition(a.ctx, fx, fy)
}

var showMu sync.Mutex // 串行化 ShowPanel（防动画/定位竞态）

// ShowPanel 弹出面板（右下角上滑动效）并异步刷新数据。
// 注意：托盘回调里必须以 go a.ShowPanel() 调用，避免阻塞托盘消息循环。
func (a *App) ShowPanel() {
	if a.ctx == nil {
		return
	}
	showMu.Lock()
	defer showMu.Unlock()
	fx, fy, pw, ph := a.panelRect()
	// 起始位置：低 48px，再上滑到位（“从托盘弹出”效果）
	runtime.WindowSetSize(a.ctx, pw, ph)
	runtime.WindowSetPosition(a.ctx, fx, fy+48)
	runtime.WindowShow(a.ctx)
	runtime.EventsEmit(a.ctx, "panel:shown", nil) // 前端据此重置失焦宽限期 + 触发内容动效
	winutil.FocusWindow(winutil.MainWindow())
	// 上滑动画：8 步 × 6px，约 150ms；结束再补一次激活规避前台锁
	a.safeGo(func() {
		for i := 1; i <= 8; i++ {
			time.Sleep(19 * time.Millisecond)
			runtime.WindowSetPosition(a.ctx, fx, fy+48-i*6)
		}
		winutil.FocusWindow(winutil.MainWindow())
	})
	a.safeGo(a.RefreshAll)
}

// HidePanel 隐藏面板。
func (a *App) HidePanel() {
	if a.ctx != nil {
		runtime.WindowHide(a.ctx)
	}
}

// Quit 退出应用（触发 wails 关闭流程）。
func (a *App) Quit() {
	if a.ctx != nil {
		runtime.Quit(a.ctx)
	}
}

// ---------------------------------------------------------------------------
// 面板数据
// ---------------------------------------------------------------------------

// AccountView 面板展示的账号（脱敏）。
type AccountView struct {
	UID            string `json:"uid"`
	Nickname       string `json:"nickname"`
	Credits        int64  `json:"credits"`
	Cooling        bool   `json:"cooling"`
	Until          string `json:"until"`
	Reason         string `json:"reason"`
	Disabled       bool   `json:"disabled"`
	ErrCount       int    `json:"err_count"`
	LastCheckinOK  bool   `json:"last_checkin_ok"`
	LastCheckinAt  string `json:"last_checkin_at"`
	LastCheckinMsg string `json:"last_checkin_msg"`
}

// State 面板初始数据。
type State struct {
	Accounts       []AccountView `json:"accounts"`
	CheckinHours   []int         `json:"checkin_hours"`
	KeepaliveHours []int         `json:"keepalive_hours"`
	ListenHost     string        `json:"listen_host"`
	ListenPort     int           `json:"listen_port"`
	APIKey         string        `json:"api_key"`
	LoginBusy      bool          `json:"login_busy"`
	NextCheckin    string        `json:"next_checkin"`
	Version        string        `json:"version"`
	Autostart      bool          `json:"autostart"`
	Running        bool          `json:"running"`
}

// GetState 返回面板初始数据。
func (a *App) GetState() State {
	st := State{
		CheckinHours:   a.sch.CheckinHours(),
		KeepaliveHours: a.sch.KeepaliveHours(),
		ListenHost:     a.cfg.Listen.Host,
		ListenPort:     a.cfg.Listen.Port,
		APIKey:         a.cfg.APIKey,
		LoginBusy:      a.loginActive(),
		NextCheckin:    fmtTime(a.sch.NextFire()),
		Version:        Version,
		Autostart:      winutil.AutostartEnabled(),
		Running:        a.serverRunning(),
	}
	st.Accounts = a.accountViews()
	return st
}

// accountViews 账号列表（按 UID 排序）。
func (a *App) accountViews() []AccountView {
	statuses := a.pool.List()
	out := make([]AccountView, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, AccountView{
			UID:            s.UID,
			Nickname:       s.Nickname,
			Credits:        s.Credits,
			Cooling:        s.Cooling,
			Until:          fmtTime(s.Until),
			Reason:         s.Reason,
			Disabled:       s.Disabled,
			ErrCount:       s.ErrCount,
			LastCheckinOK:  s.LastCheckinOK,
			LastCheckinAt:  fmtTime(s.LastCheckinAt),
			LastCheckinMsg: s.LastCheckinMsg,
		})
	}
	return out
}

func (a *App) serverRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.httpSrv != nil
}

func (a *App) loginActive() bool {
	a.muLogin.Lock()
	defer a.muLogin.Unlock()
	return a.loginBusy
}

// ---------------------------------------------------------------------------
// 账号操作
// ---------------------------------------------------------------------------

// StartLogin 发起登录：拿授权 URL、拉起无痕浏览器、后台轮询，返回 URL 供前端复制兜底。
func (a *App) StartLogin() (string, error) {
	a.muLogin.Lock()
	if a.loginBusy {
		a.muLogin.Unlock()
		return "", errors.New("已有登录流程进行中，请先完成或取消")
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.loginCtx, a.loginCancel = ctx, cancel
	a.loginClient = login.NewClient()
	a.loginBusy = true
	a.muLogin.Unlock()

	authURL, err := login.Start(a.loginClient, a.loginStateFP)
	if err != nil {
		a.finishLogin()
		return "", err
	}
	// 手动跟随登录页跳转链（copilot.tencent.com → codebuddy.cn），浏览器直接打开最终地址
	if resolved, rerr := login.ResolveAuthURL(a.loginClient, authURL); rerr == nil && resolved != "" {
		authURL = resolved
	}
	go a.launchBrowser(authURL)
	a.safeGo(func() { a.pollLogin(ctx) })
	log.Printf("登录流程已发起")
	return authURL, nil
}

// CancelLogin 取消当前登录流程。
func (a *App) CancelLogin() error {
	a.muLogin.Lock()
	cancel := a.loginCancel
	ctx := a.loginCtx
	a.muLogin.Unlock()
	if cancel == nil {
		return errors.New("没有进行中的登录")
	}
	cancel()
	_ = ctx
	_ = os.Remove(a.loginStateFP)
	return nil
}

// launchBrowser 无痕拉起默认浏览器；失败则用系统默认方式兜底。
func (a *App) launchBrowser(authURL string) {
	if exe, flag, ok := winutil.DefaultBrowserIncognito(); ok {
		if err := winutil.LaunchIncognito(exe, flag, authURL); err == nil {
			return
		}
	}
	_ = winutil.OpenURL(authURL)
}

// pollLogin 后台轮询登录结果，事件推送到前端。
func (a *App) pollLogin(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("login poll panic: %v", r)
		}
		a.finishLogin()
	}()
	deadline := time.Now().Add(loginTimeout)
	a.emitLogin("waiting", "已打开浏览器，请在无痕窗口中完成登录…")
	t := time.NewTicker(loginPollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			a.emitLogin("cancelled", "登录已取消")
			return
		case <-t.C:
		}
		if time.Now().After(deadline) {
			a.emitLogin("failed", "登录超时，请重新发起")
			return
		}
		r, err := login.Poll(a.loginClient, a.loginStateFP)
		if err == nil {
			a.completeLogin(r)
			return
		}
		if errors.Is(err, login.ErrPending) {
			a.emitLogin("waiting", "等待浏览器完成登录…")
		} else {
			log.Printf("login poll: %v", err)
			a.emitLogin("waiting", "轮询暂时不可达，自动重试中…")
		}
	}
}

// completeLogin 登录成功：写 auth 文件、重载账号池，然后异步签到 + 查积分
// （慢网络下签到可能耗时较长，不能阻塞登录完成的提示）。
func (a *App) completeLogin(r login.Result) {
	fp, err := login.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		a.emitLogin("failed", "保存凭证失败: "+err.Error())
		return
	}
	log.Printf("新账号已保存: %s", filepath.Base(fp))
	a.reloadAccounts()
	name := r.Nickname
	if name == "" && len(r.UID) >= 8 {
		name = r.UID[:8]
	}
	a.emitLogin("success", fmt.Sprintf("登录成功：%s（正在同步积分…）", name))
	a.finishLogin() // 立即释放登录锁，允许再次发起
	a.safeGo(func() {
		res, err := a.sch.CheckinAccount(r.UID)
		if err != nil {
			log.Printf("新账号签到失败 %s: %v", name, err)
		} else {
			remain := "-"
			if res.HasRemain {
				remain = fmt.Sprintf("%d", res.Remain)
			}
			log.Printf("新账号签到完成 %s：%s，剩余积分 %s", name, res.Msg, remain)
		}
		a.emitAccounts()
	})
}

func (a *App) finishLogin() {
	a.muLogin.Lock()
	a.loginBusy = false
	a.loginCtx, a.loginCancel, a.loginClient = nil, nil, nil
	a.muLogin.Unlock()
}

// reloadAccounts 用 auths 目录最新文件对齐账号池。
func (a *App) reloadAccounts() {
	auths, err := auth.LoadDir(a.cfg.AuthDir, a.cfg.Region)
	if err != nil {
		log.Printf("reload accounts: %v", err)
		return
	}
	a.pool.SyncToDir(auths)
	a.emitAccounts()
}

// CheckinAccount 单个账号立即签到。
func (a *App) CheckinAccount(uid string) (scheduler.CheckinResult, error) {
	res, err := a.sch.CheckinAccount(uid)
	a.emitAccounts()
	if err != nil {
		return res, err
	}
	log.Printf("签到 %s：%s", shortUID(uid), res.Msg)
	return res, nil
}

// CheckinAll 全部账号立即签到。
func (a *App) CheckinAll() []scheduler.CheckinResult {
	results := make([]scheduler.CheckinResult, 0)
	for _, st := range a.pool.List() {
		if st.Disabled {
			continue
		}
		res, err := a.sch.CheckinAccount(st.UID)
		if err != nil {
			continue
		}
		results = append(results, res)
	}
	a.emitAccounts()
	log.Printf("批量签到完成：%d 个账号", len(results))
	return results
}

// RefreshCredits 刷新单个账号积分。
func (a *App) RefreshCredits(uid string) (int64, error) {
	au := a.pool.AuthByUID(uid)
	if au == nil {
		return 0, fmt.Errorf("unknown account %s", uid)
	}
	remain, err := a.up.UserResource(au)
	if err != nil {
		return 0, err
	}
	a.pool.SetCredits(uid, remain)
	a.emitAccounts()
	return remain, nil
}

// RefreshAll 刷新全部账号积分（面板打开时调用）。
func (a *App) RefreshAll() {
	for _, st := range a.pool.List() {
		au := a.pool.AuthByUID(st.UID)
		if au == nil || au.AccessToken == "" {
			continue
		}
		if remain, err := a.up.UserResource(au); err == nil {
			a.pool.SetCredits(st.UID, remain)
		} else {
			log.Printf("refresh credits %s: %v", st.UID, err)
		}
	}
	a.emitAccounts()
}

// RemoveAccount 删除账号（auth 文件 + 内存池）。
func (a *App) RemoveAccount(uid string) error {
	au := a.pool.AuthByUID(uid)
	if au == nil {
		return fmt.Errorf("unknown account %s", uid)
	}
	if au.FilePath != "" {
		_ = os.Remove(au.FilePath)
	}
	a.pool.Remove(uid)
	a.emitAccounts()
	log.Printf("已删除账号 %s", shortUID(uid))
	return nil
}

// ---------------------------------------------------------------------------
// 配置操作
// ---------------------------------------------------------------------------

// SetCheckinHours 更新自动签到时间并写回 config.json（运行时生效）。
func (a *App) SetCheckinHours(hours []int) error {
	clean := normalizeHours(hours)
	if len(clean) == 0 {
		return errors.New("请至少保留一个签到时间")
	}
	a.mu.Lock()
	a.cfg.Schedule.CheckinHours = clean
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	a.sch.SetCheckinHours(clean)
	log.Printf("自动签到时间已更新：%s", hoursStr(clean))
	return nil
}

// SetListen 修改 API 监听主机 + 端口：保存配置并热切换监听（失败保持原样）。
func (a *App) SetListen(host string, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("端口无效：%d", port)
	}
	addr := config.Listen{Host: host, Port: port}.Addr()

	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.serveLocked(addr); err != nil {
		return fmt.Errorf("监听 %s 失败（可能被占用）：%v", addr, err)
	}
	a.cfg.Listen = config.Listen{Host: host, Port: port}
	if err := config.Save(a.cfg, a.cfgPath); err != nil {
		// 配置写回失败不回滚监听（服务已在跑），下次启动回退
		log.Printf("save config after listen change: %v", err)
	}
	log.Printf("API 监听已切换至 %s", addr)
	return nil
}

// SetAPIKey 修改 API 密钥：写回 config.json 并即时生效（handler 运行时更新）。
func (a *App) SetAPIKey(key string) error {
	key = strings.TrimSpace(key)
	a.mu.Lock()
	a.cfg.APIKey = key
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	a.handler.SetAPIKey(key)
	log.Printf("API-Key 已更新")
	return nil
}

// SetAutostart 设置/取消开机自启。
func (a *App) SetAutostart(on bool) error {
	if err := winutil.SetAutostart(on); err != nil {
		return err
	}
	if on {
		log.Printf("已开启开机自启")
	} else {
		log.Printf("已关闭开机自启")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 事件与日志
// ---------------------------------------------------------------------------

func (a *App) emitAccounts() {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "accounts", a.accountViews())
	}
}

func (a *App) emitLogin(phase, msg string) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "login", map[string]string{"phase": phase, "msg": msg})
	}
}

// logWriter 写日志文件（GUI 无控制台，app.log 供“查看日志”使用）。
type logWriter struct{ app *App }

func (w *logWriter) Write(p []byte) (int, error) {
	if w.app.logFile != nil {
		_, _ = w.app.logFile.Write(p)
	}
	return len(p), nil
}

// OpenLogFile 用系统记事本打开日志文件（先确保文件存在）。
func (a *App) OpenLogFile() error {
	fp := filepath.Join(filepath.Dir(a.cfg.StateFile), "app.log")
	if _, err := os.Stat(fp); err != nil {
		_ = os.WriteFile(fp, []byte("(empty)\n"), 0o644)
	}
	abs, err := filepath.Abs(fp)
	if err != nil {
		return err
	}
	return winutil.OpenWithNotepad(abs)
}

// safeGo 带 panic 兜底的 goroutine 启动器（避免单个 goroutine 崩溃静默）。
func (a *App) safeGo(f func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PANIC: %v", r)
			}
		}()
		f()
	}()
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func normalizeHours(hours []int) []int {
	seen := map[int]bool{}
	out := []int{}
	for _, h := range hours {
		if h >= 0 && h <= 23 && !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	sort.Ints(out)
	return out
}

func hoursStr(hours []int) string {
	out := make([]string, 0, len(hours))
	for _, h := range hours {
		out = append(out, fmt.Sprintf("%02d:00", h))
	}
	return strings.Join(out, "、")
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("01-02 15:04")
}

func shortUID(uid string) string {
	if len(uid) <= 10 {
		return uid
	}
	return uid[:10] + "…"
}
