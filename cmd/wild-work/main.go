// Command wild-work 系统托盘 daemon 入口。
// 单进程 = OpenAI 兼容 HTTP 服务 + 自动签到调度器 + 系统托盘 + 静态 Web UI。
// 双击 exe 启动常驻托盘；托盘菜单：打开主界面 / 刷新积分 / 查看日志 / 退出；
// 打开主界面或双击托盘 → 系统浏览器打开 Web UI。
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"wild-work/internal/app"
	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/gateway"
	"wild-work/internal/ledger"
	"wild-work/internal/oczen"
	"wild-work/internal/platform"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/qoder"
	"wild-work/internal/qodercn"
	"wild-work/internal/qodercom"
	"wild-work/internal/qwenwork"
	"wild-work/internal/scheduler"
	"wild-work/internal/server"
	"wild-work/internal/systray"
	"wild-work/internal/traework"
	"wild-work/internal/upstream"
	"wild-work/internal/workbuddyai"
)

//go:embed all:web
var webFS embed.FS

//go:embed build/trayicon.ico
var trayIconICO []byte

func main() {
	// 工作目录：便携/CLI 形态固定为 exe 所在目录，保证相对路径配置（./auths ./data）稳定。
	// macOS .app bundle 内该目录只读、且随 app 替换被清空，故改用系统数据目录（见 workDir）。
	_ = os.Chdir(workDir())

	cfgPath := "config.json"
	cfg, err := config.Load(cfgPath)
	// --autostart 由开机自启项附带：开机启动不弹提示
	// --no-tray 无头模式（无桌面 Linux/服务器）
	autostart := false
	noTray := false
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--autostart":
			autostart = true
		case "--no-tray":
			noTray = true
		}
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
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

	// 多渠道运行时：workbuddy / traework / qoder 各自独立 pool + state + scheduler。
	stateDir := filepath.Dir(cfg.StateFile)
	// 旧版兼容：迁移 state.json → 分渠道 state-{kind}.json
	migrateStateFiles(cfg.StateFile, stateDir)

	wbAuths, err := auth.LoadWorkBuddyDir(cfg.AuthDir, cfg.Region)
	if err != nil {
		fatal("读取 WorkBuddy 账号目录失败：%v", err)
	}
	trAuths, err := auth.LoadTraeDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 TraeWork 账号目录失败：%v", err)
	}
	qdAuths, err := auth.LoadQoderDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 Qoder 账号目录失败：%v", err)
	}
	wbaAuths, err := auth.LoadWorkBuddyAiDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 WorkBuddy 国际版账号目录失败：%v", err)
	}
	qwAuths, err := auth.LoadQwenWorkDir(cfg.AuthDir)
	if err != nil {
		fatal("读取千问办公账号目录失败：%v", err)
	}
	qcnAuths, err := auth.LoadQoderCNDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 QoderCN 账号目录失败：%v", err)
	}
	qcmAuths, err := auth.LoadQoderCOMDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 QoderCOM 账号目录失败：%v", err)
	}
	// OpenCodeZen 匿名通道无凭证文件：全程只有一个虚拟账号，
	// 不经目录扫描、不参与 reload（见 app.reloadAccounts 的说明）。
	ocAuths := []*auth.Auth{oczen.AnonymousAuth()}
	log.Printf("loaded accounts: workbuddy=%d %s, traework=%d, qoder=%d, qodercn=%d, qodercom=%d, workbuddyai=%d, qwenwork=%d, oczen=%d(匿名) from %s",
		len(wbAuths), cfg.Region, len(trAuths), len(qdAuths), len(qcnAuths), len(qcmAuths), len(wbaAuths), len(qwAuths), len(ocAuths), cfg.AuthDir)

	wbPool := pool.New(filepath.Join(stateDir, "state-workbuddy.json"))
	for _, a := range wbAuths {
		wbPool.Add(a)
	}
	trPool := pool.New(filepath.Join(stateDir, "state-traework.json"))
	for _, a := range trAuths {
		trPool.Add(a)
	}
	qdPool := pool.New(filepath.Join(stateDir, "state-qoder.json"))
	for _, a := range qdAuths {
		qoder.EnsureFingerprint(a) // 老凭证补机器指纹
		qdPool.Add(a)
	}
	wbaPool := pool.New(filepath.Join(stateDir, "state-workbuddyai.json"))
	for _, a := range wbaAuths {
		wbaPool.Add(a)
	}
	qwPool := pool.New(filepath.Join(stateDir, "state-qwenwork.json"))
	for _, a := range qwAuths {
		qwPool.Add(a)
	}
	qcnPool := pool.New(filepath.Join(stateDir, "state-qodercn.json"))
	for _, a := range qcnAuths {
		qodercn.EnsureFingerprint(a) // 老凭证补机器指纹
		qcnPool.Add(a)
	}
	qcmPool := pool.New(filepath.Join(stateDir, "state-qodercom.json"))
	for _, a := range qcmAuths {
		qodercom.EnsureFingerprint(a) // 老凭证补机器指纹
		qcmPool.Add(a)
	}
	// oczen：整池只有一个匿名虚拟账号，state 仅用于记冷却（无积分、无签到）
	ocPool := pool.New(filepath.Join(stateDir, "state-oczen.json"))
	for _, a := range ocAuths {
		ocPool.Add(a)
	}
	// 安全网：匿名账号不允许被禁用（任何历史 state 或异常路径写脏都可自愈）
	ocPool.SetDisabled(oczen.AnonymousUID, false)

	wbUp := upstream.New()
	wbUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	trUp := traework.New()
	trUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// TraeCode（代码版）：与 TraeWork 同一上游、共用账号，仅 function 不同（solo_agent）。
	trCodeUp := traework.NewTraeCode()
	trCodeUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	qdUp := qoder.New()
	qdUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	wbaUp := workbuddyai.New()
	wbaUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	qwUp := qwenwork.New()
	qwUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	qcnUp := qodercn.New()
	qcnUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	qcmUp := qodercom.New()
	qcmUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	ocUp := oczen.New()

	// 单渠道上游代理：config.proxies 按 kind 套到各渠道 HTTP client 上（未配置 = 直连）。
	// traework 的 StreamHTTP 与主 client 共用出厂 Transport，先切独立再套代理，
	// 否则热更新代理时会把非流式 client 的 Transport 一起替换。
	trUp.StreamHTTP.Transport = trUp.HTTP.Transport
	applyProxies(cfg, map[string][]*http.Client{
		provider.WorkBuddy.String():   {wbUp.HTTP, wbUp.BillingHTTP},
		provider.WorkBuddyAI.String(): {wbaUp.HTTP},
		provider.TraeWork.String():    {trUp.HTTP, trUp.StreamHTTP},
		provider.Qoder.String():       {qdUp.HTTP},
		provider.QoderCN.String():     {qcnUp.HTTP},
		provider.QoderCOM.String():    {qcmUp.HTTP},
		provider.QwenWork.String():    {qwUp.HTTP},
		provider.Oczen.String():       {ocUp.HTTP},
	})
	checkinMinutes, err := config.ParseClockTimes(cfg.Schedule.CheckinTimes)
	if err != nil {
		fatal("解析签到时间失败：%v", err)
	}

	// 临期阈值（全渠道共用，scheduler/App 两侧同源，默认 24h，下限 24h）
	expiringThreshold := cfg.ExpiringThresholdDur

	// 积分流水记账器：在 scheduler 之前创建（签到后差分记账用）；
	// App 内部不重建，main 装配时统一注入。失败仅降级为无统计。
	lg, lgerr := ledger.New(filepath.Join(stateDir, "ledger"))
	if lgerr != nil {
		log.Printf("ledger init failed（统计功能不可用）: %v", lgerr)
	}

	wbSch := scheduler.New(scheduler.Config{Pool: wbPool, Upstream: wbUp, Name: "workbuddy", CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours, ExpiringThreshold: expiringThreshold, Ledger: lg})
	trSch := scheduler.New(scheduler.Config{Pool: trPool, Upstream: trUp, Name: "traework", CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// Qoder 无签到活动：调度器只做 token keepalive（每日 refresh 保活）
	qdSch := scheduler.New(scheduler.Config{Pool: qdPool, Upstream: qdUp, Name: "qoder", CheckinMinutes: nil, KeepaliveHours: cfg.Schedule.KeepaliveHours, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// WorkBuddy 国际版：无显式签到（ActivitiesOnly 模式）。
	// 定时仍对每个账号调用 DailyCheckin，其实现为「免费模型对话保活 + 签到探测」；
	// 不记录/上报签到状态，保持对用户透明。
	// Keepalive 关闭（token 有效期 365 天，无需每日刷新）。
	wbaSch := scheduler.New(scheduler.Config{Pool: wbaPool, Upstream: wbaUp, Name: "workbuddyai",
		CheckinMinutes: checkinMinutes, KeepaliveHours: nil, ActivitiesOnly: true, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// 千问办公：无签到活动（每日积分服务端被动发放，无需保活/领取）；
	// CheckinMinutes=nil + KeepaliveHours=nil（token 由 deviceToken/refresh 按需轮换，
	// 定时保活反而会与千问办公 App 互踩 —— 见备忘 §7.5 风险 1）。
	// 余额/费率靠 StartCreditAutoRefresh 循环拉取。
	qwSch := scheduler.New(scheduler.Config{Pool: qwPool, Upstream: qwUp, Name: "qwenwork",
		CheckinMinutes: nil, KeepaliveHours: nil, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// QoderCN：签到双路径已实现（campaigns 主路径 + daily-check-in 兑底），
	// 沿用全局签到时段；token keepalive 与 qoder 相同。
	qcnSch := scheduler.New(scheduler.Config{Pool: qcnPool, Upstream: qcnUp, Name: "qodercn", CheckinMinutes: []int{615}, KeepaliveHours: cfg.Schedule.KeepaliveHours, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// QoderCOM：仅 campaigns 活动路径（无 daily-check-in）；其余同 QoderCN。
	qcmSch := scheduler.New(scheduler.Config{Pool: qcmPool, Upstream: qcmUp, Name: "qodercom", CheckinMinutes: []int{615}, KeepaliveHours: cfg.Schedule.KeepaliveHours, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// OpenCodeZen 匿名：无账号、无签到、无 token 可保活（凭证是常量 public）。
	// CheckinMinutes/KeepaliveHours 均为 nil → 调度器不发生任何上游调用。
	ocSch := scheduler.New(scheduler.Config{Pool: ocPool, Upstream: ocUp, Name: "oczen",
		CheckinMinutes: nil, KeepaliveHours: nil})

	runtimes := map[provider.Kind]*server.Runtime{
		provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: wbPool, Upstream: wbUp, StaticModels: server.WorkBuddyStaticModels()},
		provider.WorkBuddyAI: {Kind: provider.WorkBuddyAI, Pool: wbaPool, Upstream: wbaUp, StaticModels: workbuddyai.StaticModels(),
			// 国际版网关实测间歇性 502/503/504，账号本身健康，不计入账号错误
			NoCooldownOnServerError: true},
		provider.TraeWork: {Kind: provider.TraeWork, Pool: trPool, Upstream: trUp, StaticModels: server.TraeWorkStaticModels()},
		// TraeCode 与 TraeWork 共享 trPool（同一账号体系，避免 refresh token 轮换冲突），
		// 仅上游客户端不同（function=solo_agent）。
		provider.TraeCode: {Kind: provider.TraeCode, Pool: trPool, Upstream: trCodeUp, StaticModels: server.TraeCodeStaticModels()},
		provider.Qoder:    {Kind: provider.Qoder, Pool: qdPool, Upstream: qdUp, StaticModels: qoder.StaticModels()},
		provider.QoderCN:  {Kind: provider.QoderCN, Pool: qcnPool, Upstream: qcnUp, StaticModels: qodercn.StaticModels()},
		provider.QoderCOM: {Kind: provider.QoderCOM, Pool: qcmPool, Upstream: qcmUp, StaticModels: qodercom.StaticModels()},
		provider.QwenWork: {Kind: provider.QwenWork, Pool: qwPool, Upstream: qwUp, StaticModels: qwenwork.StaticModels()},
		// oczen：免费档 5xx/4xx 属通道级问题，账号只是虚拟占位，不因上游 5xx 累计错误
		provider.Oczen: {Kind: provider.Oczen, Pool: ocPool, Upstream: ocUp, StaticModels: oczen.StaticModels(),
			NoCooldownOnServerError: true},
	}
	appRuntimes := map[provider.Kind]*app.Runtime{
		provider.WorkBuddy:   {Kind: provider.WorkBuddy, Pool: wbPool, Upstream: wbUp, Scheduler: wbSch},
		provider.WorkBuddyAI: {Kind: provider.WorkBuddyAI, Pool: wbaPool, Upstream: wbaUp, Scheduler: wbaSch},
		provider.TraeWork:    {Kind: provider.TraeWork, Pool: trPool, Upstream: trUp, Scheduler: trSch},
		provider.TraeCode:    {Kind: provider.TraeCode, Pool: trPool, Upstream: trCodeUp, Alias: true}, // 别名渠道：无独立 Scheduler，账号聚合/签到/刷新均由 TraeWork 负责
		provider.Qoder:       {Kind: provider.Qoder, Pool: qdPool, Upstream: qdUp, Scheduler: qdSch},
		provider.QoderCN:     {Kind: provider.QoderCN, Pool: qcnPool, Upstream: qcnUp, Scheduler: qcnSch},
		provider.QoderCOM:    {Kind: provider.QoderCOM, Pool: qcmPool, Upstream: qcmUp, Scheduler: qcmSch},
		provider.QwenWork:    {Kind: provider.QwenWork, Pool: qwPool, Upstream: qwUp, Scheduler: qwSch},
		provider.Oczen:       {Kind: provider.Oczen, Pool: ocPool, Upstream: ocUp, Scheduler: ocSch},
	}

	appInst, err := app.New(app.Options{
		ConfigPath: cfgPath,
		Config:     cfg,
		Runtimes:   appRuntimes,
		Ledger:     lg,
	})
	if err != nil {
		fatal("初始化失败：%v", err)
	}
	defer appInst.Close()

	// 调度器结果写日志（无 GUI 推送，日志即面板数据源）
	wbSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("workbuddy", r) })
	trSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("traework", r) })
	qdSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("qoder", r) })
	wbSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("workbuddy", uid, ok, msg) })
	trSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("traework", uid, ok, msg) })
	qdSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("qoder", uid, ok, msg) })
	wbaSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("workbuddyai", r) })
	wbaSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("workbuddyai", uid, ok, msg) })
	qwSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("qwenwork", uid, ok, msg) })

	// HTTP handler：OpenAI 端点 + Web UI + 管理 API
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		fatal("embed web: %v", err)
	}
	inner := server.NewHandler(server.Config{
		Runtimes:     runtimes,
		APIKey:       cfg.APIKey,
		Ledger:       appInst.Ledger(),
		HardCooldown: cfg.HardCreditDur,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
		WebUI:        sub,
		AttachAPI:    appInst.HandleAPI,
	})

	// 两层结构：外层兼容层只接管三个新端点，其余（含 /v1/chat/completions、Web UI、
	// 管理 API）原样落到内层 handler，旧客户端的调用栈完全不变。
	// 渠道清单注入 Router，用于校验模型名前缀并给出可读报错。
	var channels []string
	for k := range runtimes {
		channels = append(channels, k.String())
	}
	compat := gateway.New(gateway.Config{
		Inner:  inner,
		APIKey: cfg.APIKey,
		Router: gateway.Router{
			Default:  cfg.Compat.DefaultChannel,
			Map:      cfg.Compat.ModelMap,
			Channels: gateway.SortChannels(channels),
		},
		MaxTokensCap: cfg.Compat.MaxTokensCap,
	})
	// 面板保存 compat 时热更新兼容层路由表（不然新映射要重启才生效）
	appInst.SetCompatSyncer(func(defaultChannel string, maxTokensCap int, modelMap map[string]string) {
		compat.SetCompat(defaultChannel, maxTokensCap, modelMap, channels)
	})
	// 面板保存代理配置时热更新各渠道 HTTP client（重建 Transport，无需重启）
	appInst.SetProxySyncer(func(proxies map[string]string) {
		next := *cfg
		next.Proxies = proxies
		applyProxies(&next, map[string][]*http.Client{
			provider.WorkBuddy.String():   {wbUp.HTTP, wbUp.BillingHTTP},
			provider.WorkBuddyAI.String(): {wbaUp.HTTP},
			provider.TraeWork.String():    {trUp.HTTP, trUp.StreamHTTP},
			provider.Qoder.String():       {qdUp.HTTP},
			provider.QoderCN.String():     {qcnUp.HTTP},
			provider.QoderCOM.String():    {qcmUp.HTTP},
			provider.QwenWork.String():    {qwUp.HTTP},
			provider.Oczen.String():       {ocUp.HTTP},
		})
	})
	// 面板保存 oczen key 后热更新渠道凭证；启动时也应用一次配置中的初始 key
	appInst.SetOczenSyncer(ocUp.SetAPIKey)
	ocUp.SetAPIKey(cfg.OczenAPIKey)
	compat.SetAPIKeySource(inner.CurrentAPIKey) // 面板改 API-Key 后，兼容层立即跟随
	compat.SetSystemOneHandler(func(w http.ResponseWriter, r *http.Request, state string, questions map[string]any) (int, []byte, error) {
		return ocUp.SystemOne(state, questions)
	})
	mux := http.NewServeMux()
	compat.Routes(mux) // POST /v1/responses · /v1/messages · /v1/messages/count_tokens · /v1/systemone
	mux.Handle("/", inner)
	if compat != nil {
		log.Printf("三接口兼容层已启用：default_channel=%q max_tokens_cap=%d model_map=%d 条",
			cfg.Compat.DefaultChannel, cfg.Compat.MaxTokensCap, len(cfg.Compat.ModelMap))
	}
	appInst.SetHandler(inner)
	appInst.SetRootHandler(mux)

	if err := appInst.StartServer(); err != nil {
		log.Printf("listen %s failed: %v（面板中将提示）", cfg.Listen.Addr(), err)
	}

	// 调度器后台运行
	sctx, stop := context.WithCancel(context.Background())
	defer stop()
	go wbSch.Run(sctx)
	go trSch.Run(sctx)
	go qdSch.Run(sctx)
	go wbaSch.Run(sctx)
	go qwSch.Run(sctx)
	go qcnSch.Run(sctx)
	go qcmSch.Run(sctx)
	go ocSch.Run(sctx)

	// 积分自动刷新覆盖全部渠道：
	// - workbuddyai / qoder 无签到活动，不自动刷就会一直显示旧值或 0；
	// - traework / workbuddy(CN) 虽有签到顺带拉余额，但一天只有两次，
	//   其间 token 若在别处被轮换（401）也无法自愈；统一纳入循环才能
	//   启动即出真实拆分数字，并靠 401 自愈（refreshIfSessionDead）及时恢复。
	// oczen 不在其中：匿名通道无积分可刷（面板显示「不适用」），
	// 其 FetchModelPricing 已在费率刷新路径单独覆盖。
	appInst.StartCreditAutoRefresh(sctx, []provider.Kind{
		// 旧 Qoder 渠道已下线：不自动刷积分/token（避免周期性 token refresh failed 噪音）
		provider.WorkBuddy, provider.WorkBuddyAI, provider.TraeWork, provider.QoderCN, provider.QoderCOM, provider.QwenWork,
	}, app.CreditRefreshInterval)

	// 启动即刷新「模型列表 + 费率」，之后每 30 分钟。
	// 否则刚启动时费率缓存为空，面板下方全是 unknown（需手动点刷新才正常）。
	appInst.StartPricingAutoRefresh(sctx, app.PricingRefreshInterval)

	// 启动提示（非 --autostart）：系统通知
	if !autostart && !noTray {
		platform.Notify("wild-work 已启动",
			fmt.Sprintf("OpenAI 兼容 API 地址：\nhttp://%s:%d\n\n点击右下角托盘图标或菜单打开主界面。", displayHost(cfg), cfg.Listen.Port))
	}

	if noTray {
		// 无头模式：打印信息，阻塞等待信号
		addr := cfg.Listen.Addr()
		fmt.Printf("wild-work headless mode started\n")
		fmt.Printf("  OpenAI API:    http://%s:%d/v1\n", displayHost(cfg), cfg.Listen.Port)
		fmt.Printf("  API-Key:       %s\n", cfg.APIKey)
		fmt.Printf("  Web UI (本机): http://%s:%d/\n", openHost(cfg), cfg.Listen.Port)
		fmt.Printf("  Listen:        %s\n", addr)
		fmt.Printf("  Accounts:      workbuddy=%d traework=%d qoder=%d oczen=%d(匿名)\n", len(wbAuths), len(trAuths), len(qdAuths), len(ocAuths))
		fmt.Printf("\nPress Ctrl+C to exit\n")
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		fmt.Printf("\nShutting down...\n")
		stop()
		appInst.Stop()
		return
	}

	// 系统托盘（阻塞）。无桌面环境（如 SSH/服务会话）下托盘初始化失败时
	// 捕获 panic，提示用户使用 --no-tray 参数启动。
	func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "Tray init failed: %v\n", r)
				fmt.Fprintf(os.Stderr, "Use --no-tray to run in headless mode\n")
				stop()
				appInst.Stop()
				os.Exit(1)
			}
		}()
		systray.Run(trayIconICO, "wild-work — 渠道聚合代理", systray.Actions{
			OpenUI: func() {
				// 本机打开面板：通配监听时浏览器访问不了 0.0.0.0，用局域网 IP 兑底
				_ = platform.OpenURL(fmt.Sprintf("http://%s:%d/", openHost(cfg), cfg.Listen.Port))
			},
			OpenLog: func() {
				if err := appInst.OpenLogFile(); err != nil {
					log.Printf("open log: %v", err)
				}
			},
			Quit: func() {
				if platform.AskYesNo("wild-work", "确定退出 wild-work 吗？") {
					stop()
					appInst.Stop()
					os.Exit(0)
				}
			},
		})
	}()
}

// workDir 解析数据目录（config.json / auths/ / data/ 的落点）。
//
//   - WILDWORK_HOME 显式指定时优先（测试/自定义布局用）；
//   - 运行在 macOS .app bundle 内（.../Xxx.app/Contents/MacOS）时改用
//     ~/Library/Application Support/WildWork：bundle 内容对普通用户只读，
//     且写到包内会在替换/升级 app 时连同账号一起丢失；
//   - 其余形态（CLI 二进制、Windows 便携包、Linux）沿用 exe 所在目录。
func workDir() string {
	if v := os.Getenv("WILDWORK_HOME"); v != "" {
		if err := os.MkdirAll(v, 0o755); err == nil {
			return v
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	dir := filepath.Dir(exe)
	if strings.Contains(dir, ".app/Contents/MacOS") {
		if home, err := os.UserHomeDir(); err == nil {
			p := filepath.Join(home, "Library", "Application Support", "WildWork")
			if err := os.MkdirAll(p, 0o755); err == nil {
				return p
			}
		}
	}
	return dir
}

// displayHost 展示用途的主机名。
// 通配监听（0.0.0.0/::/空）是用户主动配置的意图，如实显示 0.0.0.0；
// localhost 归一为 127.0.0.1。不做任何「善意」替换——面板顶栏同理（见 app.js renderTopbar）。
// 注意：浏览器/托盘打开面板时用此值，0.0.0.0 在浏览器不可直接访问，
// 故 OpenUI 场景改用 lanIPOrLocalhost() 兑底（见 main.go 调用处）。
func displayHost(cfg *config.Config) string {
	if cfg.Listen.Host == "" || cfg.Listen.Host == "::" {
		return "0.0.0.0"
	}
	if cfg.Listen.Host == "localhost" {
		return "127.0.0.1"
	}
	return cfg.Listen.Host
}

// lanIPOrLocalhost 返回局域网出口 IP，取不到时兑底 127.0.0.1。
// 用于「本机打开面板」场景：监听 0.0.0.0 时浏览器不能访问 0.0.0.0，
// 用局域网 IP（或 127.0.0.1）代替，用户可从本机或局域网其他机器访问。
func lanIPOrLocalhost() string {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil || addr.IP.IsLoopback() {
		return "127.0.0.1"
	}
	return addr.IP.String()
}

// openHost 返回适合「本机浏览器打开」的主机：通配监听时用局域网 IP 兑底。
func openHost(cfg *config.Config) string {
	switch cfg.Listen.Host {
	case "", "0.0.0.0", "::":
		return lanIPOrLocalhost()
	case "localhost":
		return "127.0.0.1"
	}
	return cfg.Listen.Host
}

// fatal 记录日志并弹出系统提示后退出。
func fatal(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("%s", msg)
	platform.InfoBox("wild-work", msg)
	os.Exit(1)
}

// migrateStateFiles 旧版 state.json 兼容：旧版 WorkBuddy 状态文件名为 state.json，
// 新版改为 state-workbuddy.json。若旧文件存在且新文件不存在，则重命名。
// TraeWork 状态文件始终是 state-traework.json，无需迁移。
func migrateStateFiles(oldStateFile, stateDir string) {
	newFp := filepath.Join(stateDir, "state-workbuddy.json")
	if _, err := os.Stat(newFp); err == nil {
		return // 新文件已存在
	}
	if _, err := os.Stat(oldStateFile); err != nil {
		return // 旧文件不存在
	}
	// 读旧文件确认是有效的 state JSON
	raw, err := os.ReadFile(oldStateFile)
	if err != nil {
		return
	}
	var sf struct {
		Accounts map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &sf); err != nil {
		log.Printf("state.json 解析失败，跳过迁移: %v", err)
		return
	}
	if err := os.WriteFile(newFp, raw, 0o600); err != nil {
		log.Printf("迁移 state.json 失败: %v", err)
		return
	}
	_ = os.Rename(oldStateFile, oldStateFile+".old")
	log.Printf("已迁移 state.json → state-workbuddy.json")
}
