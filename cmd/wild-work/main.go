// Command wild-work 系统托盘 daemon 入口。
// 单进程 = OpenAI 兼容 HTTP 服务 + 自动签到调度器 + 系统托盘 + 静态 Web UI。
// 双击 exe 启动常驻托盘；托盘菜单：打开主界面 / 刷新积分 / 查看日志 / 退出；
// 打开主界面或双击托盘 → 系统浏览器打开 Web UI。
//
// 另有一个**非守护**入口：`--raccoon-callback <深链>`（小浣熊授权登录的协议回调，
// 由 Windows 按 HKCU 的 office-raccoon 注册表唤起）。该分支只把深链解析后落盘再退出，
// **绝不**启动第二份服务/托盘——详见 handleRaccoonCallback。
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
	"wild-work/internal/custommodels"
	"wild-work/internal/gateway"
	"wild-work/internal/glm"
	"wild-work/internal/ledger"
	"wild-work/internal/loomy"
	"wild-work/internal/monkeycode"
	"wild-work/internal/oczen"
	"wild-work/internal/platform"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/qoder"
	"wild-work/internal/qodercn"
	"wild-work/internal/qodercom"
	"wild-work/internal/qwenwork"
	"wild-work/internal/raccoon"
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

	// 小浣熊授权登录的协议回调：由 Windows 按注册表命令行唤起本 exe。
	// 必须在**任何**初始化（config/日志/服务）之前拦下——它只落盘一个文件就退出，
	// 若走后面流程会启动第二份 daemon（日志与本进程抢锁、还可能在收到授权码后
	// 立刻把回调文件删掉），导致授权完成后账号加不进来。
	if handled, err := handleRaccoonCallback(os.Args[1:]); handled {
		if err != nil {
			log.Printf("小浣熊回调处理失败：%v", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

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
	// 管理面板鉴权：监听非环回地址（0.0.0.0 / 网卡 IP）时必须设置管理员密码，
	// 否则局域网任何设备都能无凭据打开面板、退出程序。启动层与面板保存层双重把关。
	if !cfg.Listen.IsLoopback() && strings.TrimSpace(cfg.AdminPass) == "" {
		fatal("监听 %s 已对外暴露，但 config.json 中未设置 admin_password。\n\n"+
			"请设置 admin_password（或在面板设置中修改监听地址）后重新启动；\n"+
			"若只想本机使用，把 listen.host 改回 127.0.0.1。", cfg.Listen.Addr())
	}
	if cfg.AdminPass != "" && len(cfg.AdminPass) < 8 {
		log.Printf("警告：管理员密码不足 8 位，建议使用更长口令（面板鉴权已生效）")
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
	// 智谱清言：凭据是浏览器 Cookie 里的 chatglm_refresh_token（glm-*.json）。
	glmAuths, err := auth.LoadGLMDir(cfg.AuthDir)
	if err != nil {
		fatal("读取智谱清言账号目录失败：%v", err)
	}
	// MonkeyCode：同为「导入型」渠道，凭据由面板「从本机客户端导入」写入 auths/。
	mcAuths, err := auth.LoadMonkeyCodeDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 MonkeyCode 账号目录失败：%v", err)
	}
	// 小浣熊：导入型渠道，凭据由面板「从本机客户端导入」或浏览器授权登录写入 auths/。
	rcAuths, err := auth.LoadRaccoonDir(cfg.AuthDir)
	if err != nil {
		fatal("读取小浣熊账号目录失败：%v", err)
	}
	// Loomy：导入型渠道，凭据由面板「从本机客户端导入」写入 auths/。
	lmAuths, err := auth.LoadLoomyDir(cfg.AuthDir)
	if err != nil {
		fatal("读取 Loomy 账号目录失败：%v", err)
	}
	// OpenCodeZen 匿名通道无凭证文件：全程只有一个虚拟账号，
	// 不经目录扫描、不参与 reload（见 app.reloadAccounts 的说明）。
	ocAuths := []*auth.Auth{oczen.AnonymousAuth()}
	log.Printf("loaded accounts: workbuddy=%d %s, traework=%d, qoder=%d, qodercn=%d, qodercom=%d, workbuddyai=%d, qwenwork=%d, glm=%d, monkeycode=%d, raccoon=%d, loomy=%d, oczen=%d(匿名) from %s",
		len(wbAuths), cfg.Region, len(trAuths), len(qdAuths), len(qcnAuths), len(qcmAuths), len(wbaAuths), len(qwAuths), len(glmAuths), len(mcAuths), len(rcAuths), len(lmAuths), len(ocAuths), cfg.AuthDir)

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
	// 智谱清言：与其它渠道同为多账号池（可加多个 refresh_token），
	// 但每个账号不可人工恢复（refresh_token 轮换后旧值即失效），
	// 故 Runtime 置 SingleAccount —— 见 runtimes 装配处的说明。
	glmPool := pool.New(filepath.Join(stateDir, "state-glm.json"))
	for _, a := range glmAuths {
		glmPool.Add(a)
	}
	mcPool := pool.New(filepath.Join(stateDir, "state-monkeycode.json"))
	for _, a := range mcAuths {
		mcPool.Add(a)
	}
	rcPool := pool.New(filepath.Join(stateDir, "state-raccoon.json"))
	for _, a := range rcAuths {
		rcPool.Add(a)
	}
	lmPool := pool.New(filepath.Join(stateDir, "state-loomy.json"))
	for _, a := range lmAuths {
		lmPool.Add(a)
	}
	// oczen：整池只有一个匿名虚拟账号，state 仅用于记冷却（无积分、无签到）
	ocPool := pool.New(filepath.Join(stateDir, "state-oczen.json"))
	for _, a := range ocAuths {
		ocPool.Add(a)
	}
	// 安全网：匿名账号不允许被禁用或被冷却（任何历史 state 或异常路径写脏都可自愈）。
	// 冷却也要清：该渠道只有这一个账号且无号可轮换，冷却即等于整条渠道下线。
	ocPool.SetDisabled(oczen.AnonymousUID, false)
	ocPool.ClearPenalty(oczen.AnonymousUID)

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
	mcUp := monkeycode.New()
	mcUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	rcUp := raccoon.New()
	rcUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	lmUp := loomy.New()
	lmUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	ocUp := oczen.New()
	glmUp := glm.New()
	// ⚠️ 只改 HTTP（非流式）的 Timeout。
	// **不要**给 glmUp.StreamHTTP 设 Timeout —— 流式请求一旦有整体超时，
	// 长回答会在超时点被掐断且无终止帧（实测 2026-09-27 触发 5 次）。
	// 两者出厂共用同一 Transport（下方先切独立再统一套代理/中间层，见 applyUpstreamChain）。
	glmUp.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second

	// traework / glm 的 StreamHTTP 与主 client 共用出厂 Transport，先切独立再套
	// 代理/中间层，避免共享底座在热更新时被半替换（SetTransportProxy 新建 Transport，
	// 只套一个会让流式漏掉代理，见 R35 与下方 upstreamClients）。
	trUp.StreamHTTP.Transport = trUp.HTTP.Transport
	glmUp.StreamHTTP.Transport = glmUp.HTTP.Transport
	// 单渠道上游代理与通用中间层共用的装配目标表：kind → 该渠道全部 HTTP client。
	// R35：HTTP / BillingHTTP? / StreamHTTP 必须一起传——SetTransportProxy 与
	// 中间层包装都会整体替换 Transport，只套一个会让另一个（通常流式）漏掉。
	// 启动（下方 applyUpstreamChain）与两处热更新（SetProxySyncer /
	// SetMiddlewareSyncer）共用同一张表，杜绝两处清单漂移。
	upstreamClients := map[string][]*http.Client{
		provider.WorkBuddy.String():   {wbUp.HTTP, wbUp.BillingHTTP, wbUp.StreamHTTP},
		provider.WorkBuddyAI.String(): {wbaUp.HTTP, wbaUp.StreamHTTP},
		provider.TraeWork.String():    {trUp.HTTP, trUp.StreamHTTP},
		provider.TraeCode.String():    {trCodeUp.HTTP, trCodeUp.StreamHTTP},
		provider.Qoder.String():       {qdUp.HTTP, qdUp.StreamHTTP},
		provider.QoderCN.String():     {qcnUp.HTTP, qcnUp.StreamHTTP},
		provider.QoderCOM.String():    {qcmUp.HTTP, qcmUp.StreamHTTP},
		provider.QwenWork.String():    {qwUp.HTTP, qwUp.StreamHTTP},
		provider.MonkeyCode.String():  {mcUp.HTTP, mcUp.StreamHTTP},
		provider.Raccoon.String():     {rcUp.HTTP, rcUp.StreamHTTP},
		provider.Loomy.String():       {lmUp.HTTP, lmUp.StreamHTTP},
		provider.Oczen.String():       {ocUp.HTTP, ocUp.StreamHTTP},
		// glm 两个 client 都要传（SetTransportProxy 会**新建** Transport，
		// 只套 HTTP 会让 StreamHTTP 仍走直连（代理/中间层对流式不生效）。
		provider.GLM.String(): {glmUp.HTTP, glmUp.StreamHTTP},
	}
	// 铺设出站链路：先按 config.proxies 重建代理底座，再按 config.middleware
	// 给全部渠道套中间层包装（全局开关，见 cmd/wild-work/middleware.go）。
	applyUpstreamChain(cfg, upstreamClients)
	checkinMinutes, err := config.ParseClockTimes(cfg.Schedule.CheckinTimes)
	if err != nil {
		fatal("解析签到时间失败：%v", err)
	}

	// Qoder 双区签到窗口（对齐上游 qoder2api 的调度机制）：
	// 每日 10:00（UTC+8）开放，活动可能在整点之后才创建，故 10:00–12:00 每分钟重试。
	const (
		qoderCheckinMinute     = 10 * 60 // 10:00
		qoderCheckinRetryUntil = 12 * 60 // 12:00（上游同款兑底截止）
	)

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
	// Qoder 无签到活动：调度器只做 token keepalive（每日 refresh 保活）。
	// CheckinMinutes 用显式空切片关闭（传 nil 会被 scheduler.New 补成默认 9:00/21:00）。
	qdSch := scheduler.New(scheduler.Config{Pool: qdPool, Upstream: qdUp, Name: "qoder", CheckinMinutes: []int{}, KeepaliveHours: cfg.Schedule.KeepaliveHours, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// WorkBuddy 国际版：无显式签到（ActivitiesOnly 模式）。
	// 定时仍对每个账号调用 DailyCheckin，其实现为「免费模型对话保活 + 签到探测」；
	// 不记录/上报签到状态，保持对用户透明。
	// Keepalive 关闭（token 有效期 365 天，无需每日刷新），用显式空切片关闭
	// （传 nil 会被 scheduler.New 补成默认 22:00）。
	wbaSch := scheduler.New(scheduler.Config{Pool: wbaPool, Upstream: wbaUp, Name: "workbuddyai",
		CheckinMinutes: checkinMinutes, KeepaliveHours: []int{}, ActivitiesOnly: true, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// 千问办公：无签到活动（每日积分服务端被动发放，无需保活/领取）；
	// CheckinMinutes + KeepaliveHours 均为显式空切片，本渠道没有任何定时任务
	// （token 由 deviceToken/refresh 按需轮换，定时保活反而会与千问办公 App
	// 互踩 —— 见备忘 §7.5 风险 1）。
	// 余额/费率靠 StartCreditAutoRefresh 循环拉取。
	qwSch := scheduler.New(scheduler.Config{Pool: qwPool, Upstream: qwUp, Name: "qwenwork",
		CheckinMinutes: []int{}, KeepaliveHours: []int{}, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// QoderCN：仅 campaigns 活动路径（legacy daily-check-in 已 DISABLED，其 claim 恒返回
	// 409 会造成「假成功」，故不再使用）。签到窗口对齐上游 qoder2api：10:00 开放，
	// 活动可能在整点后才创建，故 10:00–12:00 之间每分钟重试，直到真正领到或超时。
	qcnSch := scheduler.New(scheduler.Config{Pool: qcnPool, Upstream: qcnUp, Name: "qodercn", CheckinMinutes: []int{qoderCheckinMinute}, CheckinRetryUntil: qoderCheckinRetryUntil, KeepaliveHours: cfg.Schedule.KeepaliveHours, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// QoderCOM：仅 campaigns 活动路径（无 daily-check-in）；其余同 QoderCN。
	qcmSch := scheduler.New(scheduler.Config{Pool: qcmPool, Upstream: qcmUp, Name: "qodercom", CheckinMinutes: []int{qoderCheckinMinute}, CheckinRetryUntil: qoderCheckinRetryUntil, KeepaliveHours: cfg.Schedule.KeepaliveHours, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// MonkeyCode：无签到、无 refresh 端点，定时任务全关。
	// 必须传**显式空切片**：传 nil 会被 scheduler.New 当作「未配置」补成默认
	// 9:00/21:00 签到与 22:00 保活，每天产生两次必然失败的调用与失败日志。
	mcSch := scheduler.New(scheduler.Config{Pool: mcPool, Upstream: mcUp, Name: "monkeycode",
		CheckinMinutes: []int{}, KeepaliveHours: []int{}, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// 小浣熊：无签到（CheckinMinutes 用显式空切片，nil 会被补成 9:00/21:00）；
	// access_token 实测仅 ≈2h，而 refresh_token ≈30 天，
	// 故保活比其它渠道更密（每 4 小时一次）以减少「请求先 401 再刷新」的额外往返；
	// 请求路径上的 refreshIfSessionDead 仍会兜底自愈。
	rcSch := scheduler.New(scheduler.Config{Pool: rcPool, Upstream: rcUp, Name: "raccoon",
		CheckinMinutes: []int{}, KeepaliveHours: []int{1, 5, 9, 13, 17, 21}, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// Loomy：无签到，且**上游无 refresh 端点**（session 14 天，到期需重新导入）；
	// 定时任务全部关闭（显式空切片）：refreshToken 为空，保活虽会按「无 refresh token」
	// 跳过、不会误禁用账号，但每天仍空跑一次并记录失败，故直接关掉。
	lmSch := scheduler.New(scheduler.Config{Pool: lmPool, Upstream: lmUp, Name: "loomy",
		CheckinMinutes: []int{}, KeepaliveHours: []int{}, ExpiringThreshold: expiringThreshold, Ledger: lg})
	// OpenCodeZen 匿名：无账号、无签到、无 token 可保活（凭证是常量 public）。
	// CheckinMinutes/KeepaliveHours 均为显式空切片 → 调度器不发生任何上游调用。
	ocSch := scheduler.New(scheduler.Config{Pool: ocPool, Upstream: ocUp, Name: "oczen",
		CheckinMinutes: []int{}, KeepaliveHours: []int{}})
	// 智谱清言：签到与对话联动（当日需先对话才能领打卡进度），故保活与签到
	// 合成一个流程（glm.Client.Keepalive 先发最小对话再签到）。
	// 用 DailyCheckin 通道驱动：CheckinMinutes 用全局签到时间，
	// KeepaliveHours 也用同一时段（保活=签到，不必额外一次）。
	glmSch := scheduler.New(scheduler.Config{Pool: glmPool, Upstream: glmUp, Name: "glm",
		CheckinMinutes: checkinMinutes, KeepaliveHours: cfg.Schedule.KeepaliveHours,
		ExpiringThreshold: expiringThreshold, Ledger: lg})

	runtimes := map[provider.Kind]*server.Runtime{
		provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: wbPool, Upstream: wbUp, StaticModels: server.WorkBuddyStaticModels()},
		provider.WorkBuddyAI: {Kind: provider.WorkBuddyAI, Pool: wbaPool, Upstream: wbaUp, StaticModels: workbuddyai.StaticModels(),
			// 国际版网关实测间歇性 502/503/504，账号本身健康，不计入账号错误
			NoCooldownOnServerError: true},
		provider.TraeWork: {Kind: provider.TraeWork, Pool: trPool, Upstream: trUp, StaticModels: server.TraeWorkStaticModels()},
		// TraeCode 与 TraeWork 共享 trPool（同一账号体系，避免 refresh token 轮换冲突），
		// 仅上游客户端不同（function=solo_agent）。
		provider.TraeCode:   {Kind: provider.TraeCode, Pool: trPool, Upstream: trCodeUp, StaticModels: server.TraeCodeStaticModels()},
		provider.Qoder:      {Kind: provider.Qoder, Pool: qdPool, Upstream: qdUp, StaticModels: qoder.StaticModels()},
		provider.QoderCN:    {Kind: provider.QoderCN, Pool: qcnPool, Upstream: qcnUp, StaticModels: qodercn.StaticModels()},
		provider.QoderCOM:   {Kind: provider.QoderCOM, Pool: qcmPool, Upstream: qcmUp, StaticModels: qodercom.StaticModels()},
		provider.QwenWork:   {Kind: provider.QwenWork, Pool: qwPool, Upstream: qwUp, StaticModels: qwenwork.StaticModels()},
		provider.MonkeyCode: {Kind: provider.MonkeyCode, Pool: mcPool, Upstream: mcUp, StaticModels: monkeycode.StaticModels()},
		provider.Raccoon:    {Kind: provider.Raccoon, Pool: rcPool, Upstream: rcUp, StaticModels: raccoon.StaticModels()},
		provider.Loomy:      {Kind: provider.Loomy, Pool: lmPool, Upstream: lmUp, StaticModels: loomy.StaticModels()},
		provider.GLM:        {Kind: provider.GLM, Pool: glmPool, Upstream: glmUp, StaticModels: glm.StaticModels()},
		// 注意：GLM **不**置 SingleAccount。
		//
		// 早期误判：以为「凭据轮换后不可人工恢复」就该按单账号处理。但
		// ① 现在可用 CDP 自动登录随时补账号；② 单账号语义会**跳过所有账号级惩罚**，
		// 导致账号 A 失效/限流时不会切到账号 B —— 多账号完全失效（实测确认）。
		// 故按正常多账号渠道处理：错误分类惩罚 + 池内轮换。
		//
		// 唯一真实约束是「refresh_token 轮换必须落盘」，已由 glm.RefreshToken 保证。
		// oczen：整池只有一个匿名虚拟账号且不可重登——任何账号级惩罚（冷却/计数/禁用）
		// 都等于整条渠道下线。SingleAccount 声明该约束，handler 对所有错误一律原文透传；
		// NoCooldownOnServerError 保留（语义已被 SingleAccount 涵盖，留着不依赖顺序）。
		provider.Oczen: {Kind: provider.Oczen, Pool: ocPool, Upstream: ocUp, StaticModels: oczen.StaticModels(),
			SingleAccount: true, NoCooldownOnServerError: true},
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
		provider.MonkeyCode:  {Kind: provider.MonkeyCode, Pool: mcPool, Upstream: mcUp, Scheduler: mcSch},
		provider.Raccoon:     {Kind: provider.Raccoon, Pool: rcPool, Upstream: rcUp, Scheduler: rcSch},
		provider.Loomy:       {Kind: provider.Loomy, Pool: lmPool, Upstream: lmUp, Scheduler: lmSch},
		provider.GLM:         {Kind: provider.GLM, Pool: glmPool, Upstream: glmUp, Scheduler: glmSch},
		provider.Oczen:       {Kind: provider.Oczen, Pool: ocPool, Upstream: ocUp, Scheduler: ocSch},
	}

	// 自定义模型代理配置（data/custom-models.json，两级：API 源 + 模型）。
	// 文件损坏不阻断启动——Store 仍返回可用空表并附带 error，未命中的请求照旧
	// 走渠道路径；损坏文件在下次面板保存时被覆盖、自动自愈。
	customStore, cmErr := custommodels.Load(stateDir)
	if cmErr != nil {
		log.Printf("custom models load failed（自定义模型按空配置运行）: %v", cmErr)
	}

	appInst, err := app.New(app.Options{
		ConfigPath: cfgPath,
		Config:     cfg,
		Runtimes:   appRuntimes,
		Ledger:     lg,
		Custom:     customStore,
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
	glmSch.SetCheckinObserver(func(r scheduler.CheckinResult) { appInst.NotifyCheckin("glm", r) })
	glmSch.SetRefreshObserver(func(uid string, ok bool, msg string) { appInst.NotifyRefresh("glm", uid, ok, msg) })

	// HTTP handler：OpenAI 端点 + Web UI + 管理 API
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		fatal("embed web: %v", err)
	}
	inner := server.NewHandler(server.Config{
		Runtimes:     runtimes,
		APIKey:       cfg.APIKey,
		Ledger:       appInst.Ledger(),
		Stats:        appInst.Stats(), // 转发路径插桩 AddUsage/AddRequestRow（运行统计）
		Custom:       customStore,     // 自定义模型直转（裸名命中 → 直接转发第三方）
		HardCooldown: cfg.HardCreditDur,
		SoftCooldown: cfg.SoftRateDur,
		ErrThreshold: cfg.Cooldown.ErrThresh,
		ErrCooldown:  cfg.ErrCooldownDur,
		WebUI:        sub,
		AttachAPI:    appInst.HandleAPI,
	})
	// 自定义模型直转同样走中间层（全局开关 = 全部出站）：渠道出站已在上方
	// applyUpstreamChain 铺好，这里给直转双 client（流式/非流式）补同一开关；
	// 两处热更新（代理保存/中间层保存）同拍重铺，见下方 syncer。
	applyCustomMiddleware(cfg, inner.CustomClients()...)

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
	// 面板保存代理配置时热更新各渠道 HTTP client（重建 Transport，无需重启）。
	// 必须走完整链路（applyUpstreamChain）：只重铺代理会把已开启的中间层包装
	// 静默弄丢（SetTransportProxy 每轮新建 Transport）。
	appInst.SetProxySyncer(func(proxies map[string]string) {
		next := *cfg
		next.Proxies = proxies
		applyUpstreamChain(&next, upstreamClients)
		applyCustomMiddleware(&next, inner.CustomClients()...)
	})
	// 面板保存中间层配置时热更新：完整重铺出站链路（代理底座 + 中间层包装）。
	// 同理不能只套中间层：不重建底座就无法「关闭中间层」（旧包装残留）。
	appInst.SetMiddlewareSyncer(func(mw config.Middleware) {
		next := *cfg
		next.Middleware = mw
		applyUpstreamChain(&next, upstreamClients)
		applyCustomMiddleware(&next, inner.CustomClients()...)
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
	go mcSch.Run(sctx)
	go rcSch.Run(sctx)
	go lmSch.Run(sctx)
	go ocSch.Run(sctx)
	// ⚠️ 新增渠道时必须在这里补 Run()。
	// 漏了**不会报错**——只表现为「该渠道的自动签到/保活从不运行」，
	// 而手工触发（面板按钮 / RunCheckinNow）仍然可用，极难发现。
	go glmSch.Run(sctx)

	// 积分自动刷新覆盖全部渠道：
	// - workbuddyai / qoder 无签到活动，不自动刷就会一直显示旧值或 0；
	// - traework / workbuddy(CN) 虽有签到顺带拉余额，但一天只有两次，
	//   其间 token 若在别处被轮换（401）也无法自愈；统一纳入循环才能
	//   启动即出真实拆分数字，并靠 401 自愈（refreshIfSessionDead）及时恢复。
	// oczen 不在其中：匿名通道无积分可刷（面板显示「不适用」），
	// 其 FetchModelPricing 已在费率刷新路径单独覆盖。
	// glm 纳入：其凭据本体是 refresh_token（accessToken 可为空），
	// 积分靠 member_info 拉取，不刷就永远是 0。
	appInst.StartCreditAutoRefresh(sctx, []provider.Kind{
		// 旧 Qoder 渠道已下线：不自动刷积分/token（避免周期性 token refresh failed 噪音）
		provider.WorkBuddy, provider.WorkBuddyAI, provider.TraeWork, provider.QoderCN, provider.QoderCOM, provider.QwenWork, provider.GLM,
		provider.MonkeyCode,
		provider.Raccoon,
		provider.Loomy,
	}, app.CreditRefreshInterval)

	// 启动即刷新「模型列表 + 费率」，之后每 30 分钟。
	// 否则刚启动时费率缓存为空，面板下方全是 unknown（需手动点刷新才正常）。
	appInst.StartPricingAutoRefresh(sctx, app.PricingRefreshInterval)

	// 运行统计喂入器（账号状态差值/粘性路由/今日作废，30s 一轮）：
	// 进程内直调各数据源，面板「运行统计」tab 即时可用。
	appInst.StartStatsFeeders(sctx)

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
		if appInst.PanelAuthEnabled() {
			fmt.Printf("  Panel auth:    admin_password enabled (cookie session, 7d)\n")
		}
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
					// 先摘托盘图标：NIM_DELETE 由托盘消息循环执行，返回即已回收。
					// 直接 os.Exit 会跳过这一步，Windows 任务栏留下幽灵图标。
					systray.Quit(3 * time.Second)
				}
			},
		})
	}()

	// Run 返回 = 托盘图标已回收，停机清理后进程自然退出。
	stop()
	appInst.Stop()
}

// handleRaccoonCallback 处理 `--raccoon-callback <深链>` 这一**非守护**入口。
//
// 背景：小浣熊登录期间会把 HKCU 的 `office-raccoon` 协议命令行临时改成
//
//	"<wild-work.exe>" --raccoon-callback "%1"
//
// 用户授权完成后 Windows 按该命令行再拉起一份本 exe。该子进程的**唯一职责**
// 是把深链解析后原子落盘（raccoon.SaveCallback），由常驻 daemon 的轮询读取。
//
// 返回 handled=true 表示本次进程是回调子进程（调用方应直接退出）；
// err 非 nil 时仍属 handled（已接管，只是落盘失败）。
//
// state 目录取自 config.json 的 state_file 所在目录；config 读不到时退回
// raccoon 包的默认值（./data），与 config.Default 一致。
func handleRaccoonCallback(args []string) (handled bool, err error) {
	// 只认「第一个参数恰为 CallbackFlag」：避免把深链里的字符串误判成入口。
	if len(args) == 0 || args[0] != raccoon.CallbackFlag {
		return false, nil
	}

	stateDir := ""
	if cfg, cerr := config.Load("config.json"); cerr == nil && cfg != nil {
		stateDir = filepath.Dir(cfg.StateFile)
	}
	// Windows 的 windowsgui 构建**无控制台**，stderr 看不到——把结果追写到
	// data/app.log（与 daemon 同一文件、O_APPEND 个位数写入安全），否则回调失败时
	// 用户与运维都无任何线索（只能看到主进程 5 分钟后「登录超时」）。
	defer func() {
		msg := "raccoon 回调已落盘（等待主进程兑换 token）"
		if err != nil {
			msg = "raccoon 回调处理失败：" + err.Error()
		}
		appendCallbackLog(raccoon.StateDir(stateDir), msg)
	}()

	if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
		return true, errors.New("缺少深链参数")
	}
	if serr := raccoon.SaveCallback(stateDir, args[1]); serr != nil {
		return true, serr
	}
	return true, nil
}

// appendCallbackLog 追写一行到 <stateDir>/app.log（失败静默，日志不能反过来阻断主流程）。
func appendCallbackLog(stateDir, msg string) {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(stateDir, "app.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format("2006/01/02 15:04:05.000000"), msg)
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
