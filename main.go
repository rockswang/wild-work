// main.go WorkBuddy2API 托盘 GUI 入口。
// 单进程集成：OpenAI 兼容 HTTP 服务 + 自动签到调度器 + 系统托盘管理面板。
// 依赖：Windows 10 21H2+（WebView2 运行时）或无 WebView2 时自动安装；无 Go/Python/Docker/Bash。
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/energye/systray"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"github.com/rockswang/workbuddy-wild/internal/app"
	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/config"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/provider"
	"github.com/rockswang/workbuddy-wild/internal/scheduler"
	"github.com/rockswang/workbuddy-wild/internal/server"
	"github.com/rockswang/workbuddy-wild/internal/traework"
	"github.com/rockswang/workbuddy-wild/internal/upstream"
	"github.com/rockswang/workbuddy-wild/internal/winutil"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed build/trayicon.ico
var trayIconICO []byte

func main() {
	// 工作目录固定为 exe 所在目录，保证相对路径配置（./auths ./data）稳定
	if exe, err := os.Executable(); err == nil {
		_ = os.Chdir(filepath.Dir(exe))
	}

	cfgPath := "config.json"
	cfg, err := config.Load(cfgPath)
	// --autostart 由开机自启注册表项附带：开机启动不弹“已启动”提示
	autostart := false
	for _, arg := range os.Args[1:] {
		if arg == "--autostart" {
			autostart = true
		}
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// 首次运行：生成默认配置
			log.Printf("config.json 不存在，使用默认配置并生成")
			cfg = config.Default()
			if err := config.Save(cfg, cfgPath); err != nil {
				log.Printf("write default config: %v", err)
			}
		} else {
			fatal("加载配置失败：%v\n\n请检查 config.json 后重新启动", err)
		}
	}
	_ = os.MkdirAll(cfg.AuthDir, 0o755)
	_ = os.MkdirAll(filepath.Dir(cfg.StateFile), 0o755)

	// 双平台运行时：workbuddy / traework 各自独立 pool + state + scheduler。
	stateDir := filepath.Dir(cfg.StateFile)
	wbAuths, err := auth.LoadWorkBuddyDir(cfg.AuthDir, cfg.Region)
	if err != nil {
		fatal("读取 WorkBuddy 账号目录失败：%v", err)
	}
	trAuths, err := auth.LoadTraeDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 TraeWork 账号目录失败：%v", err)
	}
	log.Printf("loaded accounts: workbuddy=%d %s, traework=%d from %s", len(wbAuths), cfg.Region, len(trAuths), cfg.AuthDir)

	wbPool := pool.New(filepath.Join(stateDir, "state-workbuddy.json"))
	for _, a := range wbAuths {
		wbPool.Add(a)
	}
	trPool := pool.New(filepath.Join(stateDir, "state-traework.json"))
	for _, a := range trAuths {
		trPool.Add(a)
	}

	wbUp := upstream.New()
	wbUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	trUp := traework.New()
	trUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second

	wbSch := scheduler.New(scheduler.Config{Pool: wbPool, Upstream: wbUp, CheckinHours: cfg.Schedule.CheckinHours, KeepaliveHours: cfg.Schedule.KeepaliveHours})
	trSch := scheduler.New(scheduler.Config{Pool: trPool, Upstream: trUp, CheckinHours: cfg.Schedule.CheckinHours, KeepaliveHours: cfg.Schedule.KeepaliveHours})

	runtimes := map[provider.Kind]*server.Runtime{
		provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: wbPool, Upstream: wbUp, StaticModels: server.WorkBuddyStaticModels()},
		provider.TraeWork:  {Kind: provider.TraeWork, Pool: trPool, Upstream: trUp, StaticModels: server.TraeWorkStaticModels()},
	}
	appRuntimes := map[provider.Kind]*app.Runtime{
		provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: wbPool, Upstream: wbUp, Scheduler: wbSch},
		provider.TraeWork:  {Kind: provider.TraeWork, Pool: trPool, Upstream: trUp, Scheduler: trSch},
	}

	h := server.NewHandler(server.Config{
		Runtimes:     runtimes,
		APIKey:       cfg.APIKey,
		HardCooldown: cfg.HardCreditDur,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
	})

	appInst, err := app.New(app.Options{
		ConfigPath: cfgPath,
		Config:     cfg,
		Runtimes:   appRuntimes,
		Handler:    h,
	})
	if err != nil {
		fatal("初始化失败：%v", err)
	}
	if err := appInst.StartServer(); err != nil {
		log.Printf("listen %s failed: %v（面板中将提示）", cfg.Listen.Addr(), err)
	}

	// 交互式启动（非 --autostart）：立即弹“已启动 + API 地址”提示，不等待窗口/托盘就绪
	if !autostart {
		appInst.ShowStartupNotice()
	}

	// 调度器后台运行
	sctx, stop := context.WithCancel(context.Background())
	defer stop()
	go wbSch.Run(sctx)
	go trSch.Run(sctx)

	// 系统托盘：单击 / 双击 / 右击 均弹主面板（无原生右键菜单）
	go runTray(appInst)

	// WebView2 用户数据目录固定在 data/webview：不随 exe 改名变化，
	// 且启动前若有孤儿进程占用（强杀残留）先清理，避免白窗口长时间等待。
	webviewPath := filepath.Join(filepath.Dir(mustExecutable()), "data", "webview")
	if winutil.IsWebViewProfileLocked(webviewPath) {
		log.Printf("检测到孤儿 WebView2 进程占用 %s，正在清理", webviewPath)
		winutil.KillOrphanWebViews(webviewPath)
	}

	runGUI(appInst, webviewPath)
}

// mustExecutable 返回当前 exe 的绝对路径（失败时退化为相对路径）。
func mustExecutable() string {
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return os.Args[0]
}

// fatal 记录日志并弹出 MessageBox 后退出（GUI 无控制台，错误必须可见）。
func fatal(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("%s", msg)
	winutil.MessageBox(msg)
	os.Exit(1)
}

// runGUI 启动 wails 窗口；若窗口创建失败（无交互桌面/WebView2 异常），
// 捕获 panic 后保持 HTTP 服务与调度器继续运行（无头兜底）。
func runGUI(a *app.App, webviewPath string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("GUI 启动失败，以无头模式继续运行: %v", r)
			select {} // 保持进程存活（HTTP 服务 + 调度器仍在跑）
		}
	}()
	err := wails.Run(&options.App{
		Title:             "WorkBuddy2API",
		Width:             270,
		Height:            640,
		MinWidth:          240,
		MinHeight:         420,
		Frameless:         true,
		StartHidden:       true,
		AlwaysOnTop:       true,
		HideWindowOnClose: true,
		BackgroundColour:  options.NewRGB(246, 246, 248),
		AssetServer:       &assetserver.Options{Assets: assets},
		Windows: &windows.Options{
			WebviewUserDataPath: webviewPath,
		},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId: "workbuddy2api-gui-v1",
			OnSecondInstanceLaunch: func(d options.SecondInstanceData) {
				a.ShowSecondInstanceNotice() // 已在运行：双击 exe 询问打开面板或退出
			},
		},
		OnStartup:  func(ctx context.Context) { a.OnStartup(ctx) },
		OnDomReady: func(ctx context.Context) { a.OnDomReady(ctx) },
		OnShutdown: func(ctx context.Context) { a.OnShutdown(ctx) },
		Bind:       []interface{}{a},
	})
	if err != nil {
		log.Printf("wails: %v（以无头模式继续运行）", err)
		select {}
	}
}

// runTray 托盘生命周期：无原生菜单，单击/双击/右击均弹主面板。
// 回调全部 go 化：托盘消息循环线程只做投递，绝不执行重量级逻辑，避免托盘卡死。
func runTray(a *app.App) {
	systray.Run(func() {
		systray.SetIcon(trayIconICO)
		systray.SetTooltip("WorkBuddy2API — 托盘管理面板")
		systray.SetOnClick(func(systray.IMenu) { go a.ShowPanel() })
		systray.SetOnDClick(func(systray.IMenu) { go a.ShowPanel() })
		systray.SetOnRClick(func(systray.IMenu) { go a.ShowPanel() })
	}, func() {})
}
