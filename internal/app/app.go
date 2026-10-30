// Package app 业务编排层：HTTP 管理 API + 托盘动作 + 登录编排 + 日志。
// 由 daemon 入口（cmd/wild-work）装配，替代旧版 wails 绑定层。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/backuplink"
	"wild-work/internal/cdp"
	"wild-work/internal/cline"
	"wild-work/internal/comate"
	"wild-work/internal/config"
	"wild-work/internal/custom"
	"wild-work/internal/ledger"
	"wild-work/internal/lingxi"
	"wild-work/internal/login"
	logincodearts "wild-work/internal/login_codearts"
	"wild-work/internal/login_comate"
	logingemini "wild-work/internal/login_gemini"
	"wild-work/internal/login_glm"
	loginlingxi "wild-work/internal/login_lingxi"
	loginlobsterai "wild-work/internal/login_lobsterai"
	loginloomy "wild-work/internal/login_loomy"
	loginminimax "wild-work/internal/login_minimax"
	loginqoder "wild-work/internal/login_qoder"
	loginqodercn "wild-work/internal/login_qodercn"
	loginqodercom "wild-work/internal/login_qodercom"
	loginqwenwork "wild-work/internal/login_qwenwork"
	loginraccoon "wild-work/internal/login_raccoon"
	logintrae "wild-work/internal/login_trae"
	"wild-work/internal/login_wbai"
	loginzcode "wild-work/internal/login_zcode"
	"wild-work/internal/loopguard"
	"wild-work/internal/oczen"
	"wild-work/internal/officeace"
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
	// 别名 statsEngine：/api/usage handler 内有同名局部变量 stats（遮蔽包名）
	statsEngine "wild-work/internal/stats"
	"wild-work/internal/systray"
)

// Version 版本号。
const Version = "2.11.1"

const (
	loginTimeout   = 5 * time.Minute
	loginPollEvery = 2 * time.Second
)

// Runtime 一个渠道的运行时资源。
type Runtime struct {
	Kind      provider.Kind
	Pool      *pool.Pool
	Upstream  provider.Upstream
	Scheduler *scheduler.Scheduler

	// Alias 标记该渠道与另一渠道共用同一账号池（如 TraeCode 复用 TraeWork 的 trPool）。
	// 有别于上游/路由独立性，账号聚合类操作（状态列表、账号计数、批量签到/刷新、
	// uid → 账号反查）必须跳过别名渠道，否则同一账号会被计入两次（见 issue #36）。
	// 别名渠道仍保留独立的模型/费率抓取（各自 upstream 的 function 不同）。
	Alias bool

	// Custom 标记该渠道为用户自定义 OpenAI 兼容第三方渠道（R66）。
	Custom bool
}

// Options 构建 App 的依赖。
type Options struct {
	ConfigPath string
	Config     *config.Config
	Runtimes   map[provider.Kind]*Runtime
	Handler    *server.Handler
	// Ledger 流水记账器（main 装配时创建，与 scheduler 共用同一实例）；nil 时 App 自建。
	Ledger *ledger.Ledger
}

// App 应用编排。
type App struct {
	cfgPath string
	cfg     *config.Config
	// runtimes 渠道运行时表。启动时装配完成，此后仅在「面板保存自定义渠道」时
	// 经 UpdateCustomRuntimes 增删（R66）——这是唯一一处 post-init 写。
	// 读写一律经 runtimesMu 保护：后台统计喂入器（30s ticker）与 /api/state 轮询
	// 都会遍历此表，若不加锁，与保存操作重叠时即 Go 的
	// `fatal error: concurrent map read and map write`（不可 recover，daemon 直接死）。
	runtimes   map[provider.Kind]*Runtime
	runtimesMu sync.RWMutex
	handler    *server.Handler // 内层（保留强类型，用于 SetAPIKey/ChannelModels 等）
	httpRoot   http.Handler    // 对外暴露的根 handler，可能为外层兼容层 mux（见 SetRootHandler）

	mu         sync.Mutex // 保护 httpSrv / cfg 修改
	httpSrv    *http.Server
	listenAddr string // 当前监听地址；同地址重复 serve 直接复用，避免自撞端口

	muLogin     sync.Mutex
	loginBusy   bool
	loginCtx    context.Context
	loginCancel context.CancelFunc
	loginClient *http.Client
	// loginLabel 用户在登录弹窗里填的「账号标识」（可选，2026-10-17 新增）。
	// 上游不提供稳定身份字段的渠道（当前是 CodeArts）靠它区分「同一账号重新登录」与
	// 「新增第二个账号」——否则方案 B 的复用逻辑会把第二个账号覆盖到第一条上（用户实测报告）。
	loginLabel string
	// loginBackendOpened 本次登录的授权页是否已由本工具（无痕窗口）拉起。
	// 前端据此**不再** window.open —— 否则会在日常浏览器里再开一个带登录态的标签页，
	// 无痕的努力就白费了（授权页照样会复用已登录的华为云账号）。
	loginBackendOpened bool
	loginStateFP       string
	loginKind          provider.Kind
	// loginErr 上一次登录流程的终态错误（成功/取消/超时为空串）。
	// 面板的登录轮询只看 login_busy 就宣告「登录完成」，于是失败也会显示成功——
	// 必须把真实结果带出去（否则用户只看到「登录完成」但账号没加进来）。
	loginErr  string
	pricingFP string

	// 智谱清言自动登录会话（独立于通用 OAuth 登录流程：
	// 它不轮询 state 文件，而是经 CDP 从浏览器 Cookie 捕获凭据）。
	muGLMLogin sync.Mutex
	glmLogin   *loginglm.AutoSession

	// WPS Comate 扫码登录会话（服务端 HTTP，不需要桌面/浏览器；见 internal/login_comate）。
	muComateLogin sync.Mutex
	comateLogin   *logincomate.QRSession
	// comateBrowser 接管本机浏览器登录的会话（与扫码会话互斥使用，但各自独立存放：
	// 用户可能先用扫码、再改用浏览器，两条会话不应互相清状态）。
	comateBrowser *logincomate.BrowserSession

	muOALogin sync.Mutex
	oaLogin   *officeace.LoginSession

	// auth 管理面板会话（cookie）；详见 session.go
	auth *authState

	logFile *os.File

	// compatSyncer 面板保存 compat 后同步给外层兼容层（热更新路由表）。
	// 由 main.go 注入；nil 时仅写配置不热更（下次启动生效）。
	compatSyncer func(defaultChannel string, maxTokensCap int, modelMap map[string]string)

	// oczenSyncer 面板保存 oczen key 后写入渠道 client（热更新凭证）。
	// 由 main.go 注入；nil 时仅写配置不热更。
	oczenSyncer func(key string)

	// fbSyncer 面板保存模型降级链后推给 server.Handler（热更新降级路由）。
	// 由 main.go 注入；nil 时仅写配置不热更。
	fbSyncer func(rules []config.ModelFallbackRule)

	// efSyncer 面板保存思考强度映射后推给 server.Handler（热更新注入表）。
	// 由 main.go 注入；nil 时仅写配置不热更。
	efSyncer func(efforts map[string]string)

	// customSyncer 面板保存自定义渠道后热更新（R66）。
	// 由 main.go 注入；nil 时仅写配置不热更。
	customSyncer func(channels []custom.ChannelConfig)

	refreshMu  sync.Mutex // 防并发刷新积分
	refreshing bool

	// ledger 用量/积分双流水（data/ledger/）；锦上添花统计，nil = 未启用
	ledger     *ledger.Ledger
	ledgerStop chan struct{}

	// stats 运行统计引擎（data/stats.json）：面板「运行统计」tab 的数据面，
	// 数据注入全部进程内直调（见 internal/stats 包注释与 AGENTS.md R13）；nil = 未启用
	stats     *statsEngine.Stats
	statsStop chan struct{}

	pricingMu      sync.Mutex
	pricingCache   []provider.ModelPricing // 本地缓存
	pricingFetched time.Time
	pricingErr     string // 最近一次拉取错误
	pricingBusy    bool   // 防并发刷新（启动刷新与面板触发可能重叠）
}

// New 构建 App 并接管全局日志（写文件 + 环形缓冲）。
func New(opts Options) (*App, error) {
	a := &App{
		cfgPath:  opts.ConfigPath,
		cfg:      opts.Config,
		runtimes: opts.Runtimes,
		handler:  opts.Handler,
		auth:     newAuthState(),
	}
	a.loginStateFP = filepath.Join(filepath.Dir(opts.Config.StateFile), "login-state.json")
	a.pricingFP = filepath.Join(filepath.Dir(opts.Config.StateFile), "pricing-cache.json")

	// 用量/积分双流水（data/ledger/）：优先复用 main 装配的实例
	if opts.Ledger != nil {
		a.ledger = opts.Ledger
	} else if lg, err := ledger.New(filepath.Join(filepath.Dir(opts.Config.StateFile), "ledger")); err == nil {
		a.ledger = lg
	} else {
		log.Printf("ledger init failed（统计功能不可用）: %v", err)
	}
	if a.ledger != nil {
		a.ledgerStop = make(chan struct{})
		go a.ledger.AutoFlush(a.ledgerStop)
		// 请求体摘要开关（诊断用，默认关）：进程级全局，启动时与 config.body_log 同步
		ledger.SetBodyTrace(a.cfg.BodyLog)
	}

	// 运行统计引擎（data/stats.json）：初始化失败不致命——端点回 disabled、插桩/喂入全判 nil
	if st, err := statsEngine.New(filepath.Dir(opts.Config.StateFile), log.Printf); err == nil {
		a.stats = st
		a.statsStop = make(chan struct{})
		go st.RunFlusher(a.statsStop)
	} else {
		log.Printf("stats init failed（运行统计不可用）: %v", err)
	}

	// 日志文件 data/app.log
	logFP := filepath.Join(filepath.Dir(opts.Config.StateFile), "app.log")
	if err := os.MkdirAll(filepath.Dir(logFP), 0o755); err == nil {
		f, err := os.OpenFile(logFP, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			a.logFile = f
		}
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(&logWriter{app: a})
	a.loadPricingCache()
	a.healRaccoonProtocol()
	// Loomy 凭据历史估算 expiresAt 归一化（无 refresh 端点时短期值只会触发必失败刷新）。
	a.healLoomyExpiry()
	return a, nil
}

// stateDir 数据目录（loginStateFP / pricingFP 都在这里）。
func (a *App) stateDir() string { return filepath.Dir(a.cfg.StateFile) }

// healRaccoonProtocol 启动自愈：小浣熊授权登录会在 data/ 留一份注册表备份，
// 正常结束时会自行删除。若启动时它还在，说明上次登录中途退出/崩溃/被强杀，
// 此时立刻把 office-raccoon 注册表恢复原状，避免协议长期指向本程序。
func (a *App) healRaccoonProtocol() {
	dir := a.stateDir()
	if !raccoon.HasPendingHijack(dir) {
		return
	}
	restored, err := raccoon.RestoreProtocol(raccoon.BackupPath(dir))
	switch {
	case err != nil:
		log.Printf("启动自愈：小浣熊协议注册表恢复失败：%v（可手动检查 HKCU\\%s）", err, raccoon.ProtocolKeyPath)
	case restored:
		log.Printf("启动自愈：检测到上次登录残留的协议注册改写，已恢复 %s 注册表", raccoon.ProtocolScheme)
	default:
		log.Printf("启动自愈：残留备份存在但注册表已被官方客户端重写，仅清理备份文件")
	}
	_ = raccoon.ClearCallback(dir)
}

// Close 关闭日志文件。
func (a *App) Close() {
	// 停 ledger 自动落盘并冲刷句柄（丢最后几秒流水可接受，但不能一直丢）
	if a.ledgerStop != nil {
		select {
		case <-a.ledgerStop:
		default:
			close(a.ledgerStop)
		}
	}
	if a.ledger != nil {
		a.ledger.Close()
	}
	// 停统计引擎并同步冲刷：RunFlusher 的 stop 分支是异步收尾，
	// 进程退出路径必须 FlushNow 兜底（os.Exit 抢跑会丢最后几秒快照）
	if a.statsStop != nil {
		select {
		case <-a.statsStop:
		default:
			close(a.statsStop)
		}
	}
	if a.stats != nil {
		a.stats.FlushNow()
	}
	if a.logFile != nil {
		_ = a.logFile.Close()
		a.logFile = nil
	}
}

// Ledger 返回流水记账器（未启用时 nil；供 main 装配传给 server handler）。
func (a *App) Ledger() *ledger.Ledger { return a.ledger }

// Stats 返回运行统计引擎（未启用时 nil；供 main 装配传给 server handler 插桩）。
func (a *App) Stats() *statsEngine.Stats { return a.stats }

func (a *App) runtime(kind provider.Kind) *Runtime {
	a.runtimesMu.RLock()
	defer a.runtimesMu.RUnlock()
	if a.runtimes == nil {
		return nil
	}
	return a.runtimes[kind]
}

// runtimesSnapshot 返回运行时表的浅拷贝（调用方据此遍历，期间不受热更新影响）。
// 热更新只增删 map 键、不修改 Runtime 本体，故拷贝元素指针安全。
func (a *App) runtimesSnapshot() []*Runtime {
	a.runtimesMu.RLock()
	defer a.runtimesMu.RUnlock()
	out := make([]*Runtime, 0, len(a.runtimes))
	for _, rt := range a.runtimes {
		out = append(out, rt)
	}
	return out
}

func (a *App) firstRuntime() *Runtime {
	// oczen 排在末位：它无调度器活动，不应成为「签到时间/下次签到」的展示来源。
	// 顺序即展示优先级：新渠道追加在 Oczen 之前（Oczen 恒末位）。
	for _, k := range []provider.Kind{provider.WorkBuddy, provider.WorkBuddyAI, provider.TraeWork, provider.Qoder, provider.QoderCN, provider.QoderCOM, provider.QwenWork, provider.GLM, provider.MonkeyCode, provider.Raccoon, provider.Loomy, provider.Minimax, provider.LobsterAI, provider.ZCode, provider.Gemini, provider.CodeArts, provider.Kilo, provider.Oczen} {
		if rt := a.runtime(k); rt != nil {
			return rt
		}
	}
	return nil
}

func (a *App) allStatuses() []pool.Status {
	out := []pool.Status{}
	for _, rt := range a.runtimesSnapshot() {
		// 别名渠道（TraeCode）复用同一 Pool：计入会导致每个账号重复一条（issue #36）。
		if rt != nil && rt.Pool != nil && !rt.Alias {
			out = append(out, rt.Pool.List()...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

// channelDisplayRank 渠道展示名次（索引即排序键），从 provider.ChannelDisplayOrder 派生 ——
// 与前端 CH_LABEL / CH_SUMMARY_ORDER、面板按钮序、费率 tab 序全部同源（R142）。
var channelDisplayRank = func() map[provider.Kind]int {
	m := make(map[provider.Kind]int, len(provider.ChannelDisplayOrder))
	for i, k := range provider.ChannelDisplayOrder {
		m[k] = i
	}
	return m
}()

// channelSortKey 面板渠道展示排序键（R142）：
//   - tier 0 = 内建渠道，按**显示名的 UTF-8 字节序**（rank 即 ChannelDisplayOrder 下标）；
//   - tier 1 = 自定义渠道 / 未登记渠道，**硬编码钉在最末**，组内按渠道标识字节序，
//     保证同一批自定义渠道每次渲染顺序一致（不依赖入参顺序）。
//
// 旧 Qoder 已从界面下线（代码与路由保留）：它仍是内建渠道、有显示名，故落在「Q」位置，
// 存量账号不再被特殊钉到最末——统一规则不为单个已下线渠道开例外。
type channelSortKey struct {
	tier int
	rank int
	name string
}

func channelSortKeyOf(k provider.Kind) channelSortKey {
	if r, ok := channelDisplayRank[k]; ok {
		return channelSortKey{tier: 0, rank: r}
	}
	return channelSortKey{tier: 1, name: k.String()}
}

func (a channelSortKey) less(b channelSortKey) bool {
	if a.tier != b.tier {
		return a.tier < b.tier
	}
	if a.tier == 0 {
		return a.rank < b.rank
	}
	return a.name < b.name
}

// feeChannelOrder 「模型列表和费率」面板的渠道分组顺序。
// 与面板账号卡片/汇总条同序（R142：按渠道显示名字节序），旧 Qoder 已下线故不列
// （buildFeesChannels 也会跳过它）。原先「Oczen/Kilo 置首位」的特例被统一规则取代。
var feeChannelOrder = func() []provider.Kind {
	out := make([]provider.Kind, 0, len(provider.ChannelDisplayOrder))
	for _, k := range provider.ChannelDisplayOrder {
		if k == provider.Qoder {
			continue
		}
		out = append(out, k)
	}
	return out
}()

// sortChannelsByOrder 按展示序稳定排序渠道条目（泛型，复用于各展示处）。
func sortChannelsByOrder[T any](items []T, kindOf func(T) provider.Kind) {
	sort.SliceStable(items, func(i, j int) bool {
		return channelSortKeyOf(kindOf(items[i])).less(channelSortKeyOf(kindOf(items[j])))
	})
}

// creditsNAKind 判定该渠道的「积分」概念是否不适用（面板显示「不适用」而不是 0）。
//
// 两种成因不同，但 UI 处理一致：
//   - oczen / kilo：免密免费池，根本没有账号/余额概念；
//   - cline：上游**确无余额端点**（/credits /usage /billing /quota /me 等 12 个候选实测全 404），
//     属于「查不到」。把这种情况显示成 0 会被误读成「额度已用光」，故同样走 NA。
//   - 自定义渠道（R66）：无积分与额度接口，同样走 NA。
//
// ⚠️ comate **不在此列**（R136 起）：R130 曾据「/credits /userinfo /me 全 404」
// 判成 NA，但真实额度端点是 `/llmproxy/v1/user/token-usage`（每日额度
// limit/remain/used + 周期明细），额度只有「每日」一种口径
// `lingxi.kdocs.cn/api/public/v1/credits/balance`。两者都在 internal/comate/credits.go
// 里解析，面板因此能显示真实剩余并支持「刷新积分」。
//
// 注意：**ZCode 不走这里**（尽管它的额度也不是积分）——它有真实的 token 桶要展示，
// 走 creditsUnit="token" 的「Token 计数」面板，而不是「不适用」（见 creditsUnit）。
func (a *App) creditsNAKind(group string) bool {
	if group == provider.Oczen.String() || group == provider.Kilo.String() ||
		group == provider.Cline.String() {
		return true
	}
	return a.isCustomChannel(group)
}

// creditsUnit 该渠道「额度」的计量单位：空串 = 积分（credits，默认），`"token"` = Token 计数。
//
// 为什么做成渠道级：面板的**卡片与汇总条**按渠道渲染，它们只知道渠道名，需要先知道
// 「这个渠道的数字是什么单位」才能选对文案与合计口径（token 绝不并入积分合计）；
// 条目级的单位见 provider.ResourceItem.Unit（弹层逐条显示用）。
//
// ⚠️ 这是**单一来源**：前端不维护渠道名单，全靠本函数下发的 credits_unit 字段 ——
// 以后新增以 token 计量的渠道，只在这里加一行即可（用户 2026-10-17 的明确要求）。
func (a *App) creditsUnit(group string) string {
	if group == provider.ZCode.String() {
		return "token"
	}
	return "" // 积分（credits）
}

// isCustomChannel 报告该分组名是否是自定义渠道。先查运行时表（热更新后的权威态），
// 再回退查配置（供应器未装配/已停用的渠道）。
// 两把锁分开取：runtimesMu 只管运行时表，a.mu 只管 cfg——不可与外层已持 a.mu 的调用嵌套。
func (a *App) isCustomChannel(group string) bool {
	a.runtimesMu.RLock()
	rt := a.runtimes[provider.Kind(group)]
	a.runtimesMu.RUnlock()
	if rt != nil && rt.Custom {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, cc := range a.cfg.CustomChannels {
		if strings.EqualFold(cc.Name, group) {
			return true
		}
	}
	return false
}

// UpdateCustomRuntimes 热更新 app 持有的自定义渠道运行时（R66）。
// 只碰 runtimes 表（不读 cfg），故用 runtimesMu 而非 a.mu。
func (a *App) UpdateCustomRuntimes(customRts map[provider.Kind]*Runtime) {
	a.runtimesMu.Lock()
	defer a.runtimesMu.Unlock()
	for k, rt := range a.runtimes {
		if rt != nil && rt.Custom {
			delete(a.runtimes, k)
		}
	}
	for k, rt := range customRts {
		rt.Custom = true
		a.runtimes[k] = rt
	}
}

// SetCustomChannelsSyncer 注入自定义渠道热更新回调。
func (a *App) SetCustomChannelsSyncer(fn func(channels []custom.ChannelConfig)) {
	a.customSyncer = fn
}

// noExplicitCheckin 不支持显式签到（手动按钮）的渠道。
// WorkBuddy 国际版改为定时自动对话保活并领取日活奖励（详见 workbuddyai.DailyCheckin），
// 无需用户手动触发，故也不提供手动签到入口；
// 千问办公无签到活动且每日积分服务端被动发放，无需领取/保活。
// QoderCN/QoderCOM 已实现 campaigns 签到，支持手动按钮（上游同一套机制）。
// 智谱清言已实现 activity-api 每日签到，支持手动按钮。
// 小浣熊已实现 setting_info 触发每日积分落账（见 internal/raccoon/checkin.go，R65），支持手动按钮。
// MonkeyCode 已实现签到（POST {console}/api/v1/users/wallet/checkin，见 internal/monkeycode/checkin.go），
// 支持手动按钮并参与批量签到 —— 故**不在**本名单内（2026-10-17 起，此前误列为「无签到」）。
// Loomy 已实现签到（宠物每日打工奖励，POST /api/v1/pet-work/rewards，0~1000 分），
// 支持手动按钮并参与批量签到 —— 故**不在**本名单内（2026-10-17 接线完成）。
// ⚠️ 它的判据是该账号当日**有效对话轮次**（由上游计算，见 internal/loomy/checkin.go 文件头）：
// 本工具是 API 网关、不产生轮次归属，故未在官方客户端用过该账号时，领取结果是
// 「未达档位」（可重试 error），**不是**「无活动」——这是如实状态，不是缺陷。
// Cline 走官方 API Key：无签到活动、Key 不过期 ⇒ 既不需手动签到也不需保活。
// OpenCodeZen 匿名通道无账号概念，既无签到也无积分。
// Kilo 免费池同为免密渠道：无账号、无签到、无积分。
// 自定义渠道无签到活动（R66）；内建名单固定，故抽成无状态的包级函数（可脱离 App 断言），
// 方法体仅补上「是否自定义渠道」这一动态判定。
func noExplicitCheckin(k provider.Kind) bool {
	return k == provider.WorkBuddyAI || k == provider.QwenWork || k == provider.Oczen || k == provider.Kilo || k == provider.ZCode || k == provider.Gemini || k == provider.Cline || k == provider.Comate
}

func (a *App) noExplicitCheckinKind(k provider.Kind) bool {
	return noExplicitCheckin(k) || a.isCustomChannel(k.String())
}

func (a *App) findRuntimeAuth(uid string) (*Runtime, *auth.Auth) {
	for _, rt := range a.runtimesSnapshot() {
		// 跳过别名渠道：map 遍历无序，若命中 TraeCode 会拿到错误 upstream（function=solo_agent），
		// 且 accountGroup 会误报 traecode；账号级操作一律归主渠道（TraeWork）。
		if rt == nil || rt.Pool == nil || rt.Alias {
			continue
		}
		if au := rt.Pool.AuthByUID(uid); au != nil {
			return rt, au
		}
	}
	return nil, nil
}

func (a *App) checkinTimes() []string {
	if rt := a.firstRuntime(); rt != nil && rt.Scheduler != nil {
		return rt.Scheduler.CheckinTimes()
	}
	return nil
}

func (a *App) keepaliveHours() []int {
	if rt := a.firstRuntime(); rt != nil && rt.Scheduler != nil {
		return rt.Scheduler.KeepaliveHours()
	}
	return nil
}

func (a *App) nextFire() time.Time {
	if rt := a.firstRuntime(); rt != nil && rt.Scheduler != nil {
		return rt.Scheduler.NextFire()
	}
	return time.Time{}
}

// SetHandler 注入内层 HTTP handler（在 HandleAPI 注册后调用）。
func (a *App) SetHandler(h *server.Handler) {
	a.handler = h
	if a.httpRoot == nil {
		a.httpRoot = h
	}
}

// PanelAuthEnabled 管理面板是否启用密码鉴权（供启动提示使用）。
func (a *App) PanelAuthEnabled() bool { return a.panelAuthEnabled() }

// AdminPassIsWeak 管理员密码是否过弱（< 8 位）：仅用于启动告警，不阻断。
func (a *App) AdminPassIsWeak() bool {
	p := a.adminPass()
	return p != "" && len(p) < 8
}

// SetRootHandler 注入对外服务的根 handler（通常是「兼容层 mux + 内层 handler」的组合）。
// httpRoot 为 nil 时回退到内层 handler。
func (a *App) SetRootHandler(h http.Handler) { a.httpRoot = h }

// ---------------------------------------------------------------------------
// HTTP 服务
// ---------------------------------------------------------------------------

// serveHandler 返回实际对外服务的 handler（优先外层组合，其次内层）。
func (a *App) serveHandler() http.Handler {
	// 最外层统一加 Host 校验（未启用管理密码时只接受环回 Host，防 DNS rebinding 读写面板）；
	// 挂在最外层而非只挂 /api/：面板静态页面与 /v1/* 同样不该被跨站域名访问。
	if a.httpRoot != nil {
		return a.hostGuard(a.httpRoot)
	}
	if a.handler == nil { // 避免 typed-nil 接口导致 http.Server 请求时 panic
		return nil
	}
	return a.hostGuard(a.handler)
}

// StartServer 按当前配置启动 HTTP 服务。
func (a *App) StartServer() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.serveLocked(a.cfg.Listen.Addr())
}

// serveLocked 在 newAddr 上启动新服务并切换；调用方需持有 a.mu。
func (a *App) serveLocked(addr string) error {
	// 地址未变时直接复用现有 listener：否则「先 Listen 再 Shutdown 旧」在同端口上
	// 会撞自身（Only one usage of each socket address），导致面板保存映射/密钥时报 400。
	if a.httpSrv != nil && a.listenAddr == addr {
		return nil
	}
	// 先停旧服务再绑新地址：避免同端口切换时新旧 listener 短暂重叠报「端口被占用」。
	if old := a.httpSrv; old != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = old.Shutdown(shutdownCtx)
		cancel()
		a.httpSrv = nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           a.serveHandler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	a.httpSrv = srv
	a.listenAddr = addr
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("http serve %s: %v", addr, err)
		}
	}()
	return nil
}

// Stop 停止 HTTP 服务（退出时调用）。
func (a *App) Stop() {
	a.mu.Lock()
	srv := a.httpSrv
	a.httpSrv = nil
	a.listenAddr = ""
	a.mu.Unlock()
	if srv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}
	a.Close()
}

// Quit 退出整个程序（Web UI“退出”按钮调用）。
func (a *App) Quit() {
	// 先摘托盘图标：回收发生在托盘消息循环里，放在停机之后会被 Close 关掉日志、
	// 也可能被 os.Exit 抢在前面，Windows 任务栏就会留下幽灵图标。
	systray.Quit(3 * time.Second)
	a.Stop()
	os.Exit(0)
}

// ---------------------------------------------------------------------------
// 渠道登录能力：单一来源表
// ---------------------------------------------------------------------------
//
// 为什么需要这张表（2026-10-18 深档排障，H3/H9 的根因）：
// 改造前「渠道 → 登录能力」被复制成了 5-7 份手工维护的表，全在同一条登录链路上：
//
//	A 表 app.go:634   准入判定（混了 5 种语义：真编排 / 导入型拒绝 / 匿名拒绝 /
//	                  只给链接不轮询 / 两步流程指路）
//	B 表 app.go:683   loginClient 工厂
//	C 表 app.go:719   Start() 调用
//	CancelLogin     6 个 `if kind == provider.X`（各自 Shutdown）
//	pollLogin       13 个 `if kind == provider.X`（各自 Poll + 收尾）+ defer 里 5 个 Shutdown
//
// 同一渠道要在这 5-7 处各登记一次，全靠注释约定「必须与前端对齐」。事故已发生 3 次：
// officeace 只加前端没加后端（死按钮）、7 渠道只加后端没加前端（无 ⟳ 入口）、
// Loomy 的 Shutdown 被误插进 Raccoon 的 if 块（取消 Loomy 时不关引导页）。
// 前两次的「修复」都是再手工对齐一遍 —— 这就是架构问题，不是细心问题。
//
// 本表把「一个渠道的登录能力」收成一条记录。新增渠道 = 加一行；
// 少写一项 = 该字段为 nil，行为由 loginFlows 的调用点统一兜底，不会静默失效。
type loginFlow struct {
	// Start 发起登录并**立即**返回可展示的授权页地址。
	// nil 表示本渠道不通过本表发起（见 noFlowReason）。
	Start func(c *http.Client, statePath string) (string, error)

	// Poll 单次查询授权结果，unfinished 表示「还没完成」（各包用自己的 ErrPending）。
	// done 负责把结果落成账号（即原来的 complete*Login）。
	// nil 表示本渠道不需要轮询（如 GLM 只给链接）。
	Poll func(c *http.Client, statePath string) (done func(*App) error, unfinished bool, err error)

	// Shutdown 释放登录期间占用的本机资源（浏览器 / 回调监听端口）。幂等，可为 nil。
	Shutdown func()

	// clientFactory 创建本渠道的 HTTP 客户端；nil 表示用 login.NewClient()。
	// 改造前这是一整张 switch（B 表），实际上除默认 client 外并无差异。
	clientFactory func() *http.Client

	// openIncognito 表示授权页必须由本工具用**无痕窗口**拉起，而不是交给前端的
	// window.open()。适用面：授权页会复用日常浏览器里已登录会话的渠道——
	// 否则「想添加第二个账号，却总被授权成已登录的那个」（2026-10-17 用户实测）。
	openIncognito bool

	// pollTimeout 轮询登录结果的超时上限；零值 = 用通用 loginTimeout。
	//
	// 为什么需要：部分渠道的本机回调存活时间比通用 5m 长——CodeArts 是 flowTTL（10m）。
	// 以较短者为上限会在用户还在授权页时先放弃并关掉回调口，浏览器随后跳回
	// 127.0.0.1:<port> 只会得到「连接被拒」（2026-10-18 真机实测）。
	//
	// 该特例原先写在 pollLogin 里当成一个 `if a.loginKind == provider.CodeArts`，
	// 属表外第二来源；现收进本表，保持「一个渠道一条记录」的约束。
	pollTimeout func() time.Duration

	// OnlyForRegion 非空时仅在该 region 生效（目前无人使用，留给地区差异渠道）。
	OnlyForRegion string

	// noFlowReason 非空表示本渠道**不通过本表发起登录**，字符串即给用户的说明。
	// 用于统一表达改造前 A 表的四种「拒绝/特例」语义：
	//   - 导入型渠道（凭据来自本机客户端）
	//   - 匿名/免密渠道（无账号可登）
	//   - 两步流程渠道（走专属入口）
	noFlowReason string

	// DisplayURL 非空表示「只把用户引到某个页面，不发起轮询」。
	// 这是 GLM 的形态：凭据由用户从浏览器 Cookie 取出后粘贴（SubmitGLMToken）。
	// 与 Start 的区别是**不置 loginBusy、不起 pollLogin** —— 改造前它靠「返回非空 URL」
	// 伪装成正常登录，前端据此启动了轮询，而轮询见 loginBusy=false 便提示
	// 「登录完成，正在同步账号…」，实际什么都没发生（H9 假成功）。
	DisplayURL string
}

// loginFlows 渠道登录能力的唯一来源。
//
// 约定：每条记录要么有 Start（真发起登录），要么有 DisplayURL（只引导不轮询），
// 要么有 noFlowReason（明确拒绝并说明原因）。三者至少有一，由 init 校验。
var loginFlows = map[provider.Kind]*loginFlow{}

// newClient 各渠道创建 HTTP 客户端的方式。改造前是一整张 switch，
// 实际上只有 Raccoon 不用 client、其它都是 NewClient()，故用 nil 表示「用默认 client」。
func newLoginClient(k provider.Kind) *http.Client {
	f, ok := loginFlows[k]
	if !ok || f.clientFactory == nil {
		return login.NewClient()
	}
	return f.clientFactory()
}

func init() {
	// —— 走标准 HTTP 授权流（Start + Poll），签名与 login_* 包一一对应 ——
	loginFlows[provider.WorkBuddy] = &loginFlow{
		Start: func(c *http.Client, sp string) (string, error) {
			ep := login.EndpointsForRegion(regionForLogin)
			url, err := login.Start(c, sp, ep)
			if err != nil {
				return "", err
			}
			if resolved, rerr := login.ResolveAuthURL(c, url, ep); rerr == nil && resolved != "" {
				url = resolved
			}
			return url, nil
		},
		Poll: func(c *http.Client, sp string) (func(*App) error, bool, error) {
			r, err := login.Poll(c, sp, login.EndpointsForRegion(regionForLogin))
			if err != nil {
				if errors.Is(err, login.ErrPending) {
					return nil, true, nil
				}
				return nil, false, err
			}
			return func(a *App) error { a.completeLogin(r); return nil }, false, nil
		},
	}
	loginFlows[provider.TraeWork] = &loginFlow{
		Start: logintrae.Start,
		Poll:  wrapPoll(logintrae.Poll, logintrae.ErrPending, func(a *App, r logintrae.Result) error { a.completeTraeLogin(r); return nil }),
	}
	loginFlows[provider.Qoder] = &loginFlow{
		Start: loginqoder.Start,
		Poll:  wrapPoll(loginqoder.Poll, loginqoder.ErrPending, func(a *App, r loginqoder.Result) error { a.completeQoderLogin(r); return nil }),
	}
	loginFlows[provider.QoderCN] = &loginFlow{
		Start: loginqodercn.Start,
		Poll:  wrapPoll(loginqodercn.Poll, loginqodercn.ErrPending, func(a *App, r loginqodercn.Result) error { a.completeQoderCNLogin(r); return nil }),
	}
	loginFlows[provider.QoderCOM] = &loginFlow{
		Start: loginqodercom.Start,
		Poll:  wrapPoll(loginqodercom.Poll, loginqodercom.ErrPending, func(a *App, r loginqodercom.Result) error { a.completeQoderCOMLogin(r); return nil }),
	}
	loginFlows[provider.WorkBuddyAI] = &loginFlow{
		Start: loginwbai.Start,
		Poll:  wrapPoll(loginwbai.Poll, loginwbai.ErrPending, func(a *App, r loginwbai.Result) error { a.completeWbaiLogin(r); return nil }),
	}
	loginFlows[provider.QwenWork] = &loginFlow{
		Start:    loginqwenwork.Start,
		Poll:     wrapPoll(loginqwenwork.Poll, loginqwenwork.ErrPending, func(a *App, r loginqwenwork.Result) error { a.completeQwenWorkLogin(r); return nil }),
		Shutdown: loginqwenwork.Shutdown,
	}
	loginFlows[provider.Minimax] = &loginFlow{
		Start: loginminimax.Start,
		Poll:  wrapPoll(loginminimax.Poll, loginminimax.ErrPending, func(a *App, r loginminimax.Result) error { a.completeMinimaxLogin(r); return nil }),
	}
	loginFlows[provider.LobsterAI] = &loginFlow{
		// LobsterAI 回调端口可固定（config.login.lobsterai_callback_port）：portal 只认
		// http://127.0.0.1:<port>/auth/callback，固定端口才能让「浏览器所在机器」预先
		// 建一条端口转发（ssh -L），从而在局域网/远程访问面板时也自动完成回调（R97）。
		// 由 StartLoginFor 在发起前写入（与 regionForLogin 同款模式，避免改表函数签名）。
		Start: func(c *http.Client, sp string) (string, error) {
			return loginlobsterai.StartWithPort(c, sp, lobsterAICallbackPort)
		},
		Poll:     wrapPoll(loginlobsterai.Poll, loginlobsterai.ErrPending, func(a *App, r loginlobsterai.Result) error { a.completeLobsterAILogin(r); return nil }),
		Shutdown: loginlobsterai.Shutdown,
		// R97：本机回调口存活 flowTTL（10m）也比通用 loginTimeout（5m）长，同款问题
		// （用户还在授权页就先放弃并关掉回调口），超时上限同样收进表。
		pollTimeout: loginlobsterai.FlowTTL,
	}
	loginFlows[provider.ZCode] = &loginFlow{
		Start: loginzcode.Start,
		Poll:  wrapPoll(loginzcode.Poll, loginzcode.ErrPending, func(a *App, r loginzcode.Result) error { a.completeZCodeLogin(r); return nil }),
	}
	loginFlows[provider.Gemini] = &loginFlow{
		Start:    logingemini.Start,
		Poll:     wrapPoll(logingemini.Poll, logingemini.ErrPending, func(a *App, r logingemini.Result) error { a.completeGeminiLogin(r); return nil }),
		Shutdown: logingemini.Shutdown,
	}
	loginFlows[provider.CodeArts] = &loginFlow{
		Start:    logincodearts.Start,
		Poll:     wrapPoll(logincodearts.Poll, logincodearts.ErrPending, func(a *App, r logincodearts.Result) error { a.completeCodeArtsLogin(r); return nil }),
		Shutdown: logincodearts.Shutdown,
		// 授权页会复用日常浏览器里已登录的华为云会话 ⇒ 必须用无痕窗口拉起，
		// 否则「想添加第二个账号却总被授权成已登录的那个」。
		openIncognito: true,
		// 本机回调存活 10m（flowTTL）比通用 loginTimeout（5m）长；超时上限对齐该包，
		// 否则用户还在授权页时就被放弃、回调口已关，浏览器跳回只会「连接被拒」。
		pollTimeout: logincodearts.FlowTTL,
	} // Loomy：本机引导页 + 短信验证码，持有 127.0.0.1 监听端口。
	loginFlows[provider.Loomy] = &loginFlow{
		Start:    loginloomy.Start,
		Poll:     wrapPoll(loginloomy.Poll, loginloomy.ErrPending, func(a *App, r loginloomy.Result) error { a.completeLoomyLogin(r); return nil }),
		Shutdown: loginloomy.Shutdown,
	}
	// Raccoon：网页版 CDP 捕获，签名与其它包同构（两个参数用不上）。
	// clientFactory 为 nil 表示用默认 client（它根本不发 HTTP 轮询）。
	loginFlows[provider.Raccoon] = &loginFlow{
		Start:    loginraccoon.Start,
		Poll:     wrapPoll(loginraccoon.Poll, loginraccoon.ErrPending, func(a *App, r loginraccoon.Result) error { a.completeRaccoonLogin(r); return nil }),
		Shutdown: loginraccoon.Shutdown,
	}
	// Lingxi：WPS 灵犀网页版 CDP 捕获 wps_sid（与 Raccoon 同款范式）。粘贴路径
	// （SubmitLingxiSID，F12 复制 Cookie）是等价入口，两条路径共用 persistLingxiAccount
	// 入库，文件格式与同账号告警语义完全一致。
	// pollTimeout 对齐包内 10m 等待上限：通用 5m 会先到、关掉还在等用户登录的浏览器。
	loginFlows[provider.Lingxi] = &loginFlow{
		Start:       loginlingxi.Start,
		Poll:        wrapPoll(loginlingxi.Poll, loginlingxi.ErrPending, func(a *App, r loginlingxi.Result) error { return a.completeLingxiLogin(r) }),
		Shutdown:    loginlingxi.Shutdown,
		pollTimeout: loginlingxi.PollTimeout,
	}

	// —— 不通过本表发起的渠道（改造前 A 表的四种语义，现统一为说明文字）——
	loginFlows[provider.MonkeyCode] = &loginFlow{
		noFlowReason: "MonkeyCode 渠道无需登录：请在面板点「从本机客户端导入」（复用本机已登录的官方客户端凭据）",
	}
	loginFlows[provider.Oczen] = &loginFlow{
		noFlowReason: "OpenCodeZen 为匿名通道，无需也无法添加账号",
	}
	loginFlows[provider.Kilo] = &loginFlow{
		noFlowReason: "Kilo 免费池无需登录，也没有可添加的账号",
	}
	// GLM：无编程式登录，只把用户引到页面，凭据由粘贴 Cookie 获得（见 DisplayURL 注释）。
	loginFlows[provider.GLM] = &loginFlow{
		DisplayURL: loginglm.LoginURL,
	}
	loginFlows[provider.OfficeAce] = &loginFlow{
		noFlowReason: "OfficeAce 登录是两步流程（华为云授权码 + 云端轮询捕获），请用面板的「浏览器登录」入口",
	}

	// 自检：每条记录必须恰好表达一种「发起方式」，防止将来加渠道时漏填。
	for k, f := range loginFlows {
		n := 0
		if f.Start != nil {
			n++
		}
		if f.DisplayURL != "" {
			n++
		}
		if f.noFlowReason != "" {
			n++
		}
		if n != 1 {
			panic(fmt.Sprintf("loginFlows[%s] 必须恰好表达一种发起方式（Start / DisplayURL / noFlowReason），实际 %d 种", k, n))
		}
	}
}

// wrapPoll 把「各包各形的 Poll + complete」适配成表中统一的形状。
// 各包的 Result 类型互不相同，只能在这里用泛型收口，避免 13 份重复的
// 「err == nil / ErrPending / 不报错就 continue」样板。
func wrapPoll[R any](
	poll func(*http.Client, string) (R, error),
	pending error,
	done func(*App, R) error,
) func(*http.Client, string) (func(*App) error, bool, error) {
	return func(c *http.Client, sp string) (func(*App) error, bool, error) {
		r, err := poll(c, sp)
		if err != nil {
			if errors.Is(err, pending) {
				return nil, true, nil
			}
			return nil, false, err
		}
		return func(a *App) error { return done(a, r) }, false, nil
	}
}

// regionForLogin 登录时使用的地区（workbuddy 系授权端点分地区）。
// 由 App 在发起登录前写入，避免把 region 塞进每张表的函数签名。
var regionForLogin = "cn"

// lobsterAICallbackPort LobsterAI 回调端口（0=随机）；由 App 发起登录前写入。
// 写入点与 regionForLogin 同款（见 loginFlows[provider.LobsterAI] 的 Start）。
var lobsterAICallbackPort int

// lookupLoginFlow 取某渠道的登录能力；不存在时返回 nil（调用方报「未知渠道」）。
func lookupLoginFlow(kind string) (*loginFlow, bool) {
	f, ok := loginFlows[provider.Kind(kind)]
	return f, ok
}

// needsPollFor 该渠道发起登录后是否需要前端轮询结果。
//
// 判据只有一条且来自单一来源表：**只给链接、不跑流程**的渠道（DisplayURL 形态，如 GLM）
// 不需要轮询；其余真发起登录流程的渠道都需要。
// 这是 H9（GLM 假成功）的唯一判定处——改造前前端靠「auth_url 是否非空」猜，
// 而 GLM 恰恰返回了非空 URL，于是轮询读不到 login_busy 便谎报「登录完成」。
//
// 未知渠道返回 false：既然连登录能力都没登记，本就不该进入轮询。
func (a *App) needsPollFor(kind string) bool {
	f, ok := loginFlows[provider.Kind(kind)]
	if !ok {
		return false
	}
	return f.DisplayURL == "" && f.Start != nil
}

// ---------------------------------------------------------------------------
// 账号操作
// ---------------------------------------------------------------------------

// manualCallbackKind 报告该渠道的授权回调是否**写死回环地址**，因而需要一条
// 「把浏览器地址栏 URL 粘回来」的人工兜底通道。
//
// 为什么要有这个单一来源：/api/login/start 用它决定 `manual_callback`（前端显隐输入行），
// /api/login/callback 用它决定是否接受提交。两处各写一份条件，漏改一处就是
// 「输入框显示了、提交却 400」或「后端支持、前端没入口」的半接线状态。
//
// 判据（不是所有渠道都需要）：portal 侧只能把浏览器跳回 `http://127.0.0.1:<port>/…`
// 的，才是「浏览器必须与网关同机」——
//   - CodeArts：授权页只接受 `port` 参数，跳转目标由 portal 写死（见 internal/login_codearts）；
//   - LobsterAI：`redirect_uri` 由本工具给出，但回调口绝不监听非环回（见 internal/login_lobsterai）；
//   - TraeWork：回调写死 `http://127.0.0.1:<随机端口>/authorize`，且监听只有 5 分钟寿命，
//     局域网/超时场景下凭证都到不了网关（见 internal/login_trae，issue #98）；
//   - QwenWork：PKCE(Hydra) 回环回调（`/callback`），redirect_uri 白名单制不可改（R104 前置）；
//   - Gemini：installed-app 政策限 redirect host 为 localhost（不可改，见 internal/login_gemini）。
//
// 其余渠道的回调**不是**这种形状：有的由网关侧轮询/捕获（glm、小浣熊、OfficeAce），
// 有的是本机引导页（Loomy），有的是纯服务端轮询无回调环节（workbuddy/qoder/zcode/minimax 系），
// 不接受也不知道怎么消费用户粘回来的 URL，故一律为 false。
func manualCallbackKind(kind provider.Kind) bool {
	switch kind {
	case provider.CodeArts, provider.LobsterAI, provider.TraeWork, provider.QwenWork, provider.Gemini:
		return true
	}
	return false
}

// StartLoginFor 发起指定渠道登录：workbuddy / workbuddyai / traework / qoder / qodercn。
func (a *App) StartLoginFor(kind string) (string, error) {
	k := provider.Kind(strings.TrimSpace(kind))
	if k == "" {
		k = provider.WorkBuddy
	}

	// 准入与形态判定：全部来自 loginFlows 这一张表。
	flow, known := loginFlows[k]
	if !known {
		return "", fmt.Errorf("unknown login provider %s", kind)
	}
	// 不通过本表发起的渠道：统一用 noFlowReason 说明原因（改造前是 4 个手写 case）。
	if flow.noFlowReason != "" {
		return "", errors.New(flow.noFlowReason)
	}
	// 只引导不轮询的渠道（GLM）：返回地址但**不置 loginBusy、不起 pollLogin**。
	// 前端据 /api/state 下发的 capabilities 判定「是否需要轮询」，
	// 不再靠「返回值非空」猜（改造前正是这个猜法导致 H9 假成功）。
	if flow.DisplayURL != "" {
		return flow.DisplayURL, nil
	}
	regionForLogin = a.cfg.Region
	a.muLogin.Lock()
	if a.loginBusy {
		a.muLogin.Unlock()
		return "", errors.New("已有登录流程进行中，请先完成或取消")
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.loginCtx, a.loginCancel = ctx, cancel
	a.loginKind = k
	// B 表已并入 loginFlows：Raccoon 不发 HTTP 轮询，clientFactory 为 nil 时
	// 仍给一个默认 client（它用不到，但保持非 nil 可避免各处 nil 判断）。
	a.loginClient = newLoginClient(k)
	a.loginBusy = true
	a.loginErr = ""
	a.muLogin.Unlock()

	// C 表已并入 loginFlows：一条记录一个 Start，结构上不可能再出现
	// 「两行挤进同一个 case、前者被覆盖」（H2）或「case 只有注释」（H1）。
	// LobsterAI 的回调端口配置在发起前写入（R97：portal 只认 127.0.0.1:<port>，
	// 固定端口才能让浏览器侧预建 ssh -L 转发实现全自动回调）。
	lobsterAICallbackPort = a.cfg.Login.LobsterAICallbackPort
	authURL, err := flow.Start(a.loginClient, a.loginStateFP)

	if err != nil {
		a.finishLogin()
		return "", err
	}
	// 浏览器打开通常由前端 Web UI 通过 window.open() 完成；但 CodeArts 例外：
	// 它的授权页会**复用日常浏览器里已登录的华为云会话**，于是「想添加第二个账号，
	// 却总被授权成已登录的那个」（2026-10-17 用户实测报告）。故由本工具用**无痕窗口**拉起。
	if flow.openIncognito {
		a.openAuthURLIncognito(authURL)
	}
	go a.pollLogin(ctx)
	log.Printf("%s 登录流程已发起", k)
	return authURL, nil
}

// openAuthURLIncognito 用无痕窗口打开授权页（用于必须避开日常浏览器登录态的渠道）。
//
// 成功时置 loginBackendOpened，让前端**不再** window.open —— 否则会在日常浏览器里再开一个
// 带登录态的标签页，无痕那一步就白做了。
// 探测不到支持无痕的浏览器 / 启动失败时**退回系统默认打开**：可用性优先于隔离性，
// 并在日志里说明原因，便于排查「为什么还是登录成了老账号」。
func (a *App) openAuthURLIncognito(url string) {
	exe, flag, ok := platform.DefaultBrowserIncognito()
	if !ok {
		log.Printf("登录：未探测到支持无痕的默认浏览器，退回系统默认打开（可能带上已有登录态）")
		_ = platform.OpenURL(url)
		return
	}
	if err := platform.LaunchIncognito(exe, flag, url); err != nil {
		log.Printf("登录：无痕窗口启动失败（%v），退回系统默认打开", err)
		_ = platform.OpenURL(url)
		return
	}
	a.muLogin.Lock()
	a.loginBackendOpened = true
	a.muLogin.Unlock()
	log.Printf("登录：已用无痕窗口打开授权页（避免复用日常浏览器的登录态）")
}

// CancelLogin 取消当前登录流程。
func (a *App) CancelLogin() error {
	a.muLogin.Lock()
	cancel := a.loginCancel
	kind := a.loginKind
	a.muLogin.Unlock()
	if cancel == nil {
		return errors.New("没有进行中的登录")
	}
	cancel()
	_ = os.Remove(a.loginStateFP)
	// 释放本机资源（浏览器 / 127.0.0.1 回调监听端口）：由 loginFlows 的 Shutdown 统一表达。
	// 改造前这里是 6 个平铺的 `if kind == provider.X`，且注释记载「Loomy 的 Shutdown 曾被
	// 误插进 Raccoon 的 if 块里，取消 Loomy 时引导页不关」—— 同一个坑的第三次复发。
	a.shutdownLoginFlow(provider.Kind(kind))
	log.Printf("登录已取消")
	return nil
}

// shutdownLoginFlow 释放某渠道登录期间占用的本机资源（幂等，未登记或无 Shutdown 则无操作）。
// 取代了改造前散落在 CancelLogin 与 pollLogin defer 里的 11 个手工 if 分支
// （两处各写一遍，且曾因插错层级而漏关）。
func (a *App) shutdownLoginFlow(k provider.Kind) {
	if f, ok := loginFlows[k]; ok && f.Shutdown != nil {
		f.Shutdown()
	}
}

// pollLogin 后台轮询登录结果，写日志。
func (a *App) pollLogin(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("login poll panic: %v", r)
		}
		// 释放本机资源：由 loginFlows 的 Shutdown 统一表达（幂等）。
		// 改造前这里是 5 个平铺的逐渠道 if，且与 CancelLogin 里那 6 个各写一遍 ——
		// 两处不一致就会漏关（Loomy 引导页就漏过一次）。
		a.shutdownLoginFlow(a.loginKind)
		a.finishLogin()
	}()
	// 超时上限由 loginFlows 的 pollTimeout 决定（零值用通用 loginTimeout）。
	// 原先这里是 `if a.loginKind == provider.CodeArts` 的表外特例，现收进表。
	// R97：LobsterAI 的回调口存活 flowTTL（10m）也比通用 loginTimeout（5m）长，
	// 同款问题（用户还在授权页就先放弃并关掉回调口），同样收进表。
	pollBudget := loginTimeout
	if f, ok := loginFlows[a.loginKind]; ok && f.pollTimeout != nil {
		pollBudget = f.pollTimeout()
	}
	deadline := time.Now().Add(pollBudget)
	log.Printf("登录流程已发起，等待浏览器完成…")
	t := time.NewTicker(loginPollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("登录已取消")
			a.setLoginError("登录已取消")
			return
		case <-t.C:
		}
		if time.Now().After(deadline) {
			log.Printf("登录超时，请重新发起")
			a.setLoginError("登录超时（未收到授权回调），请重新发起登录")
			return
		}
		// 分派：由 loginFlows 表决定，不再逐渠道 if。
		// 改造前这里是 13 个几乎相同的块（每块 5-8 行样板：Poll → 判 err → 判 ErrPending
		// → 不报错就 continue → setLoginError），且块序与 A/B/C 三张表的顺序无关，
		// 新增渠道时极易漏写其中一处（漏了就是「登录永远等超时」）。
		flow, known := loginFlows[a.loginKind]
		if !known || flow.Poll == nil {
			// 表里没登记 Poll：DisplayURL / noFlowReason 的渠道已在 StartLoginFor 提前返回，
			// 走不到这里。兜底成「立即失败」而不是死循环等超时。
			a.setLoginError("登录失败：该渠道未提供轮询能力")
			return
		}
		done, unfinished, perr := flow.Poll(a.loginClient, a.loginStateFP)
		// 分派契约（loginFlow.Poll / wrapPoll 的三态，2026-10-18 9f8f4a95 回归教训）：
		//   pending  → (nil, true, nil)：继续轮询。**不得**调 done —— 该回归曾写成
		//              `if perr == nil` 就调 done(a)，而 pending 的 done 恒为 nil，
		//              首个 tick（2s）必 nil 闭包 panic；panic 被 recover 吞掉后
		//              finishLogin 清 busy，表现是「所有轮询渠道发起登录 2 秒后静默死亡，
		//              面板无任何响应」。
		//   成功     → (done, false, nil)：落账号收尾。
		//   终态错误 → (nil, false, err)：立即结束轮询（继续只会重复报同一个错）。
		switch {
		case unfinished:
			// 还没完成：下一跳再看。
		case perr == nil:
			if err := done(a); err != nil {
				a.setLoginError("登录失败：" + err.Error())
			}
			return
		default:
			// 终态错误（下发验证码失败 / 引导页起不来 / 未捕获到账号 / 换取令牌失败）：
			// 立即结束轮询，由 defer 释放资源。
			log.Printf("%s login poll failed: %v", a.loginKind, perr)
			a.setLoginError("登录失败：" + perr.Error())
			return
		}
	}
}

// completeRaccoonLogin 小浣熊网页版登录成功：把捕获并换取的 token 落成本工具凭据。
// 凭据形态与「从本机客户端导入」完全一致（两者拿到的是同一个上游账号）：
// uid 取 access_token 的 JWT name claim，domain/apiHost 对齐 internal/raccoon 的调用前缀。
// access_token 约 1h，靠 refresh_token 续期；签到走 setting_info 触发每日积分落账
// （见 internal/raccoon/checkin.go），故登录后立即签到一次，让当日积分马上落账。
func (a *App) completeRaccoonLogin(r loginraccoon.Result) {
	// uid 提取统一走 raccoon.JWTUID：旧版 JWT 取 name，新版（2026-10-15 上游迁站后）
	// 标准 OIDC 无 name claim，回落 sub。与客户会话自愈、导入路径同源，避免多处各自漂移。
	uid := raccoon.JWTUID(r.AccessToken)
	if uid == "" {
		uid = "default"
	}
	expiresAt := r.ExpiresAt
	if expiresAt <= 0 {
		expiresAt = time.Now().Add(time.Hour).Unix() // 实测 access_token 寿命约 1h
	}
	log.Printf("raccoon 登录成功 uid=%s org=%s refresh_token=%t", uid, r.OfficeOrgName, r.RefreshToken != "")
	doc := authDoc{
		Auth: authSection{
			AccessToken:  r.AccessToken,
			RefreshToken: r.RefreshToken,
			ExpiresAt:    expiresAt,
			Domain:       "/api/web/llm/v2",
			ApiHost:      raccoon.MainSite,
		},
		Account: accountSection{
			UID:          uid,
			EnterpriseID: r.OfficeIdentity,
			Nickname:     uid,
		},
	}
	fp, err := a.writeAuthFile("raccoon", uid, doc)
	if err != nil {
		log.Printf("raccoon 登录保存凭证失败 uid=%s err=%v", uid, err)
		return
	}
	log.Printf("raccoon 登录凭证已保存 uid=%s file=%s", uid, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.Raccoon)
	// 首次签到（对齐 workbuddy/traework）：setting_info 触发当日每日积分落账，
	// 不签到的话新账号要等下一个签到时刻才会把当天该给的 300 分记上。
	a.safeGo(func() { a.firstCheckinRaccoon(uid) })
}

// firstCheckinRaccoon 新账号首次签到 + 积分兜底（幂等：已领过时上游返回空 popups）。
func (a *App) firstCheckinRaccoon(uid string) {
	rt := a.runtime(provider.Raccoon)
	if rt == nil || rt.Scheduler == nil {
		return
	}
	if res, err := rt.Scheduler.CheckinAccount(uid); err != nil {
		log.Printf("raccoon 新账号签到失败 %s: %v", uid, err)
	} else {
		log.Printf("raccoon 新账号签到完成 %s：%s", uid, res.Msg)
	}
	// 积分兜底：签到路径失败（余额查询失败/签到异常）时 pool 内 credits 仍为 0，
	// 独立刷一次保证新账号首屏正确（幂等：多刷无害）。
	if _, err := a.RefreshCredits(uid); err != nil {
		log.Printf("raccoon 新账号积分兜底失败 %s: %v", uid, err)
	}
}

// completeLogin 登录成功：写 auth 文件、重载账号池，异步签到。
// completeLoomyLogin Loomy 浏览器登录（短信验证码）成功后的收尾。
//
// 凭据形态与「从本机客户端导入」（import_local.go 的 importLoomy）**完全一致** ——
// 两者拿到的是同一个上游 session，只是来源不同：
//   - uid 优先取上游 userid，缺失时由 login_loomy 从前缀派生（账号池需要稳定去重标识）；
//   - domain/apiHost 对齐 internal/loomy 的调用前缀；
//   - refreshToken 留空：Loomy **没有任何 refresh 端点**，session 约 14 天到期后
//     只能重新登录（或重新从客户端导入）。留空还能让 scheduler 跳过必然失败的保活。
//
// 刻意**不**做登录即签到：internal/loomy 没有签到端点（见 noExplicitCheckin 的说明），
// 硬加一次请求只会产生一条无意义的失败流水。
func (a *App) completeLoomyLogin(r loginloomy.Result) {
	uid := strings.TrimSpace(r.UID)
	if uid == "" {
		// login_loomy 已保证非空（上游缺 userid 时会派生）；这里是防御，
		// 避免把半成品凭据写成 loomy-default.json 污染账号池。
		log.Printf("loomy 登录失败：缺少账号标识（上游未下发 userid）")
		a.finishLogin()
		return
	}
	// LogFields 只含 uid / 脱敏手机号 / 到期时刻，**不含 token**（零 token 落盘红线）。
	log.Printf("loomy 登录成功 %s", r.LogFields())
	doc := authDoc{
		Auth: authSection{
			AccessToken: r.AccessToken,
			// 留空：Loomy 无 refresh 端点（与 importLoomy 同口径）。
			RefreshToken: "",
			ExpiresAt:    r.ExpiresAt,
			Domain:       r.Domain,
			ApiHost:      r.ApiHost,
		},
		Account: accountSection{
			UID:      uid,
			Nickname: r.Nickname,
		},
	}
	fp, err := a.writeAuthFile("loomy", uid, doc)
	if err != nil {
		log.Printf("loomy 登录失败：写凭据出错 uid=%s err=%v", uid, err)
		a.finishLogin()
		return
	}
	log.Printf("loomy 登录成功，凭据已落盘 uid=%s file=%s", uid, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.Loomy)
	// 登录后立即领一次宠物打工奖励：此时凭据刚拿到，若该账号今天在官方客户端已有
	// 有效对话轮次，这一步就能把已达档位的奖励领掉，不必等到下一个签到时刻。
	// 与 raccoon 同款（firstCheckinRaccoon）：**走调度器**，领到的积分才会进流水账；
	// 失败只记日志，不影响「登录成功」这个主结果。
	a.safeGo(func() { a.firstCheckinLoomy(uid) })
}

// firstCheckinLoomy 新账号首次签到（领宠物打工奖励）+ 积分兜底。
//
// 与 firstCheckinRaccoon 同形：走调度器的 CheckinAccount，因而领到的积分会经
// Ledger.RecordCheckinEarn 进流水；随后独立刷一次余额，保证新账号首屏数字正确。
// 幂等：未达档位时上游返回 granted=false，不会重复发放。
func (a *App) firstCheckinLoomy(uid string) {
	rt := a.runtime(provider.Loomy)
	if rt == nil || rt.Scheduler == nil {
		return
	}
	if res, err := rt.Scheduler.CheckinAccount(uid); err != nil {
		log.Printf("loomy 新账号签到失败 %s: %v", uid, err)
	} else {
		log.Printf("loomy 新账号签到完成 %s：%s", uid, res.Msg)
	}
	// 积分兜底：签到路径失败（余额查询失败/未达档位）时 pool 内 credits 仍为 0，
	// 独立刷一次保证新账号首屏正确（幂等：多刷无害；Loomy 已在积分自动刷新名单里）。
	if _, err := a.RefreshCredits(uid); err != nil {
		log.Printf("loomy 新账号积分兜底失败 %s: %v", uid, err)
	}
}

func (a *App) completeLogin(r login.Result) {
	log.Printf("workbuddy 登录成功 uid=%s nickname=%s expires_in=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresIn, r.RefreshToken != "")
	fp, err := login.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("workbuddy 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("workbuddy 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.WorkBuddy)
	a.safeGo(func() {
		rt := a.runtime(provider.WorkBuddy)
		if rt == nil || rt.Scheduler == nil {
			return
		}
		if res, err := rt.Scheduler.CheckinAccount(r.UID); err != nil {
			log.Printf("新账号签到失败 %s: %v", r.Nickname, err)
		} else {
			log.Printf("新账号签到完成 %s：%s", r.Nickname, res.Msg)
		}
		// 积分兑底：签到路径失败（余额查询失败/签到异常）时 pool 内 credits 仍为 0，
		// 独立刷一次保证新账号首屏正确（幂等：多刷无害）。
		if _, err := a.RefreshCredits(r.UID); err != nil {
			log.Printf("workbuddy 新账号积分兑底失败 %s: %v", r.Nickname, err)
		}
	})
}

// afterAccountAdded 新账号就绪后的统一收尾：刷新模型列表与费率。
// 原因：模型列表与费率都按「已接入账号」填充，新渠道/新账号加入后，
// 若不主动失效缓存，两处都要等 TTL（1h / 1h）才更新，面板上显示不完整。
// 故每个渠道登录成功后统一调用。
func (a *App) afterAccountAdded(kind provider.Kind) {
	// 1) 失效模型缓存后【必须】立即重建——否则其它渠道的模型缓存被误杀：
	//    InvalidateModels 清空的是【全部渠道】的缓存，而 RefreshPricing 只拉费率不拉模型，
	//    若只失效不重建，qodercn/qodercom 等无静态兑底渠道会从面板整组消失，
	//    直到下一个 30min ticker（生产 2026-09-28 实测：登录 qwenwork 后 qoder 双渠道消失 28min）。
	if a.handler != nil {
		a.handler.InvalidateModels()
		a.refreshChannelModels()
	}
	// 2) 立即刷新费率为后台任务，不阻塞登录流程
	a.safeGo(func() {
		a.RefreshPricing()
		log.Printf("account added platform=%s: models+pricing refreshed", kind)
	})
}

// completeWbaiLogin 国际版登录成功：写 auth 文件、重载账号池、异步拉取初始积分。
// 不触发签到（国际版无签到活动）；但必须拉一次余额，
// 否则新账号在面板上显示 0 积分，需用户手动点「刷新积分」才正确。
func (a *App) completeWbaiLogin(r loginwbai.Result) {
	log.Printf("workbuddyai 登录成功 uid=%s nickname=%s expires_in=%d refresh_token=%t",
		r.UID, r.Nickname, r.ExpiresIn, r.RefreshToken != "")
	if r.UID == "" {
		log.Printf("workbuddyai 登录失败：响应缺 uid")
		a.finishLogin()
		return
	}
	fp, err := loginwbai.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("workbuddyai 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		a.finishLogin()
		return
	}
	log.Printf("workbuddyai 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.WorkBuddyAI)
	// 新账号首次拉取余额（异步，不阻塞登录流程）
	a.safeGo(func() {
		if remain, err := a.RefreshCredits(r.UID); err != nil {
			log.Printf("workbuddyai 新账号积分获取失败 %s: %v", r.Nickname, err)
		} else {
			log.Printf("workbuddyai 新账号积分获取完成 %s: %d", r.Nickname, remain)
		}
	})
}

// completeMinimaxLogin MiniMax 设备码登录成功后的收尾。
//
// 刻意**不**像 trae 登录那样顺带签到：MiniMax 是 7 天签到面板，用户在面板点一次即可，
// 少发一次上游请求（AGENTS.md §0.1「验证上游行为最小化请求数」的同源纪律）。
// completeLobsterAILogin LobsterAI 本机回调登录成功后的收尾。
//
// 与 minimax 同样**不**登录即签到：签到是面板上的显式动作，少发一次上游请求
// （AGENTS.md §0.1 的同源纪律）。
// completeZCodeLogin ZCode 设备授权登录成功后的收尾。
//
// 与 minimax / lobsterai 同样**不**登录即签到：ZCode 的积分领取依赖阿里云验证码
// （headful 浏览器），本仓不实现；签到走面板手动或干脆不做，少发一次上游请求。
// completeGeminiLogin Gemini loopback OAuth 登录成功后的收尾。
//
// 与其他渠道一致**不**登录即签到：Gemini 本渠道就没有签到（DailyCheckin 返回「不支持」）。
// completeCodeArtsLogin CodeArts PKCE OAuth 登录成功后的收尾。
//
// 本渠道**有**每日签到（三步、走 HMAC 签名，渠道包实现了 CheckinReporter/CheckinGranter），
// 但与其他渠道一致**不**在登录时顺手签到：定时任务会在签到时间自行领取，少发一次上游请求。
func (a *App) completeCodeArtsLogin(r logincodearts.Result) {
	// 带上用户在登录弹窗里填的「账号标识」：上游不提供稳定身份字段（实测），只能靠它区分账号
	// ——留空 = 覆盖该渠道已有条目（同一账号重新登录）；填写 = 作为独立账号并存。
	// 取完即清，避免污染下一次登录。
	a.muLogin.Lock()
	r.Label = a.loginLabel
	a.loginLabel = ""
	a.muLogin.Unlock()
	log.Printf("codearts 登录成功 uid=%s nickname=%s label=%q expires_at=%d refresh_token=%t", r.UID, r.Nickname, r.Label, r.ExpiresAt, r.RefreshToken != "")
	fp, err := logincodearts.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("codearts 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("codearts 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.CodeArts)
}

func (a *App) completeGeminiLogin(r logingemini.Result) {
	log.Printf("gemini 登录成功 uid=%s nickname=%s expires_at=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresAt, r.RefreshToken != "")
	fp, err := logingemini.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("gemini 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("gemini 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.Gemini)
}

func (a *App) completeZCodeLogin(r loginzcode.Result) {
	log.Printf("zcode 登录成功 uid=%s nickname=%s expires_at=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresAt, r.RefreshToken != "")
	fp, err := loginzcode.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("zcode 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("zcode 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.ZCode)
}

func (a *App) completeLobsterAILogin(r loginlobsterai.Result) {
	log.Printf("lobsterai 登录成功 uid=%s nickname=%s expires_at=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresAt, r.RefreshToken != "")
	fp, err := loginlobsterai.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("lobsterai 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("lobsterai 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.LobsterAI)
}

func (a *App) completeMinimaxLogin(r loginminimax.Result) {
	log.Printf("minimax 登录成功 uid=%s nickname=%s expires_at=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresAt, r.RefreshToken != "")
	fp, err := loginminimax.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("minimax 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("minimax 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.Minimax)
}

func (a *App) completeTraeLogin(r logintrae.Result) {
	log.Printf("traework 登录成功 uid=%s nickname=%s expires_at=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresAt, r.RefreshToken != "")
	fp, err := logintrae.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("traework 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("traework 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.TraeWork)
	a.safeGo(func() {
		rt := a.runtime(provider.TraeWork)
		if rt == nil || rt.Scheduler == nil {
			return
		}
		if res, err := rt.Scheduler.CheckinAccount(r.UID); err != nil {
			log.Printf("TraeWork 新账号签到失败 %s: %v", r.Nickname, err)
		} else {
			log.Printf("TraeWork 新账号签到完成 %s：%s", r.Nickname, res.Msg)
		}
		// 积分兑底（对齐 workbuddy）：签到路径失败时 pool 内 credits 仍为 0
		if _, err := a.RefreshCredits(r.UID); err != nil {
			log.Printf("TraeWork 新账号积分兑底失败 %s: %v", r.Nickname, err)
		}
	})
}

// completeQoderLogin 登录成功：生成机器指纹、写 auth 文件、重载账号池。
// Qoder 当前无签到活动，不做首次签到。
func (a *App) completeQoderLogin(r loginqoder.Result) {
	log.Printf("qoder 登录成功 uid=%s nickname=%s expires_in=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresIn, r.RefreshToken != "")
	au := &auth.Auth{Kind: "qoder", AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, UID: r.UID, Nickname: r.Nickname}
	qoder.EnsureFingerprint(au)
	fp, err := loginqoder.SaveAuth(a.cfg.AuthDir, r, au.MachineID, au.MachineToken, au.MachineType)
	if err != nil {
		log.Printf("qoder 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("qoder 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.Qoder)
	// 新账号首次拉取余额（对齐 workbuddyai/qwenwork：qoder 登录后无签到流程，
	// 不主动刷则面板显示 0 直到下个 auto-refresh 周期）
	a.safeGo(func() {
		if remain, err := a.RefreshCredits(r.UID); err != nil {
			log.Printf("qoder 新账号积分获取失败 %s: %v", r.Nickname, err)
		} else {
			log.Printf("qoder 新账号积分获取完成 %s: %d", r.Nickname, remain)
		}
	})
}

// completeQoderCNLogin 登录成功：生成机器指纹、写 auth 文件、重载账号池、首次签到。
func (a *App) completeQoderCNLogin(r loginqodercn.Result) {
	log.Printf("qodercn 登录成功 uid=%s nickname=%s expires_in=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresIn, r.RefreshToken != "")
	au := &auth.Auth{Kind: "qodercn", AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, UID: r.UID, Nickname: r.Nickname}
	qodercn.EnsureFingerprint(au)
	fp, err := loginqodercn.SaveAuth(a.cfg.AuthDir, r, au.MachineID, au.MachineToken, au.MachineType)
	if err != nil {
		log.Printf("qodercn 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	au.FilePath = fp // 回填落盘路径，后续 SaveAtomic（显示名回填）依赖它
	log.Printf("qodercn 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.QoderCN)
	// 新账号首次拉取余额 + 实测 userType + 回填显示名。
	// 设备流响应里无 nickname，显示名（邮箱/昵称）只能从 userinfo 拿：
	// 拉到后写回 au.Nickname 并 SaveAtomic 落盘，面板即显示真实用户名。
	a.safeGo(func() {
		if rt := a.runtime(provider.QoderCN); rt != nil {
			if cu, ok := rt.Upstream.(*qodercn.Client); ok {
				if name, _, err := cu.FetchUserInfo(au); err != nil {
					log.Printf("qodercn 新账号 userType 实测失败 %s: %v", r.UID, err)
				} else if name != "" && au.Nickname == "" {
					au.Nickname = name
					if err := au.SaveAtomic(); err != nil {
						log.Printf("qodercn 显示名回写失败 %s: %v", r.UID, err)
					} else {
						log.Printf("qodercn 显示名已回填 %s: %s", r.UID, name)
						a.reloadAccounts() // 池内 nickname 刷新，面板即时可见
					}
				}
			}
		}
		if remain, err := a.RefreshCredits(r.UID); err != nil {
			log.Printf("qodercn 新账号积分获取失败 %s: %v", r.Nickname, err)
		} else {
			log.Printf("qodercn 新账号积分获取完成 %s: %d", r.Nickname, remain)
		}
	})
}

// completeQoderCOMLogin 登录成功：生成机器指纹、写 auth 文件、重载账号池、首次签到（仅 campaigns）。
func (a *App) completeQoderCOMLogin(r loginqodercom.Result) {
	log.Printf("qodercom 登录成功 uid=%s nickname=%s expires_in=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresIn, r.RefreshToken != "")
	au := &auth.Auth{Kind: "qodercom", AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, UID: r.UID, Nickname: r.Nickname}
	qodercom.EnsureFingerprint(au)
	fp, err := loginqodercom.SaveAuth(a.cfg.AuthDir, r, au.MachineID, au.MachineToken, au.MachineType)
	if err != nil {
		log.Printf("qodercom 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	au.FilePath = fp // 回填落盘路径，后续 SaveAtomic（显示名回填）依赖它
	log.Printf("qodercom 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.QoderCOM)
	// 新账号首次拉取余额 + 实测 userType + 回填显示名。
	// 设备流响应里无 nickname，显示名（邮箱/昵称）只能从 userinfo 拿：
	// 拉到后写回 au.Nickname 并 SaveAtomic 落盘，面板即显示真实用户名。
	a.safeGo(func() {
		if rt := a.runtime(provider.QoderCOM); rt != nil {
			if cu, ok := rt.Upstream.(*qodercom.Client); ok {
				if name, _, err := cu.FetchUserInfo(au); err != nil {
					log.Printf("qodercom 新账号 userType 实测失败 %s: %v", r.UID, err)
				} else if name != "" && au.Nickname == "" {
					au.Nickname = name
					if err := au.SaveAtomic(); err != nil {
						log.Printf("qodercom 显示名回写失败 %s: %v", r.UID, err)
					} else {
						log.Printf("qodercom 显示名已回填 %s: %s", r.UID, name)
						a.reloadAccounts() // 池内 nickname 刷新，面板即时可见
					}
				}
			}
		}
		if remain, err := a.RefreshCredits(r.UID); err != nil {
			log.Printf("qodercom 新账号积分获取失败 %s: %v", r.Nickname, err)
		} else {
			log.Printf("qodercom 新账号积分获取完成 %s: %d", r.Nickname, remain)
		}
	})
}

// SubmitGLMToken 提交用户粘贴的智谱清言 refresh_token：验证 → 落盘 → 重载账号池。
//
// 这是智谱清言的**手工兜底**入口（自动路径见 StartGLMAutoLogin）。
// 返回保存后的 uid 供面板定位新账号。
func (a *App) SubmitGLMToken(refreshToken string) (string, error) {
	r, err := loginglm.ValidateAndSave(a.cfg.AuthDir, refreshToken)
	if err != nil {
		return "", err
	}
	log.Printf("glm 账号已添加（手工）uid=%s nickname=%s", r.UID, r.Nickname)
	a.reloadAccounts()
	a.afterAccountAdded(provider.GLM)
	return r.UID, nil
}

// SubmitClineKey 提交用户粘贴的 Cline 官方 API Key（sk_…）：验证 → 落盘 → 重载账号池。
//
// 验证只调 GET /api/v1/api-keys —— **零 token 消耗**，避免「添加账号即烧额度」。
// uid 取该端点中 key_prefix 与本 Key 相符那条的 apk- id（实测 key_prefix = Key 前 10 字符）。
//
// 第二个返回值是**告警**（非错误，账号照样保存）：官方配额字段名是 `…UsageCostUSDPerUser`，
// 即上限按 Cline **账号**计 ⇒ 同一账号的多把 Key 共享一份额度。
// 由于 api-keys 返回的是本账号**全部** Key，可据此发现「两把 Key 同属一个账号」并提醒
// 用户去注册独立账号，而不是误以为多 Key 等于多额度。
func (a *App) SubmitClineKey(apiKey string) (string, string, error) {
	key := strings.TrimSpace(apiKey)
	res, err := cline.ValidateKey(key)
	if err != nil {
		return "", "", err
	}

	// 同账号检测：新 Key 所在账号的 Key 列表 ∩ 已入库账号的 Key 前缀
	warn := ""
	if existing, lerr := auth.LoadClineDir(a.cfg.AuthDir); lerr == nil {
		family := append([]string{res.KeyPrefix}, res.SiblingKeys...)
		for _, e := range existing {
			p := e.AccessTokenValue()
			if len(p) > cline.KeyPrefixLen {
				p = p[:cline.KeyPrefixLen]
			}
			if p == "" {
				continue
			}
			for _, kp := range family {
				if kp == p {
					warn = fmt.Sprintf("这把 Key 与已添加的账号 %s 同属一个 Cline 账号，共享同一份额度"+
						"（官方按账号计），不会增加可用额度。请注册独立 Cline 账号并各自建 Key。", e.UID)
					break
				}
			}
			if warn != "" {
				break
			}
		}
	}

	name := res.AccountName
	if name == "" {
		name = "API Key " + res.KeyPrefix
	}
	_ = os.MkdirAll(a.cfg.AuthDir, 0o755)
	ca := &auth.Auth{
		Kind:        "cline",
		UID:         res.UID,
		Nickname:    name,
		AccessToken: key,
		// Key 无过期概念：ExpiresAt 必须远期，0 会让 NeedsRefresh 恒真（不变量 21 同款陷阱）
		ExpiresAt: cline.FarFutureExpiry(),
		FilePath:  filepath.Join(a.cfg.AuthDir, "cline-"+res.UID+".json"),
	}
	if err := ca.SaveAtomic(); err != nil {
		return "", "", fmt.Errorf("保存 Cline 凭据失败：%w", err)
	}
	// 日志零 token（不变量 2）：只打 uid 与展示名，绝不打完整 Key
	log.Printf("cline 账号已添加（粘贴 Key）uid=%s name=%s 同账号告警=%v", res.UID, name, warn != "")
	a.reloadAccounts()
	a.afterAccountAdded(provider.Cline)
	return res.UID, warn, nil
}

// SubmitComateSID 提交用户粘贴的 wps_sid Cookie：验证 → 落盘 → 重载账号池。
//
// 验证只调 GET /models —— **零积分消耗**，避免「添加账号即烧积分」。
// 上游 /models 不回传任何身份字段（实测），uid 用 sid 的 sha256 前 16 位兜底
//（与 cline probeKey 的 hash 兜底同款：只需稳定且不含明文）。
//
// 同账号检测：wps_sid 与已入库账号逐字比对 —— 同一份 sid 重复添加不扩额度
//（积分按 WPS 账号计），提示用户换账号而不是重复粘贴。
func (a *App) SubmitComateSID(sid string) (string, string, error) {
	// 容错：用户常把整段 Cookie / DevTools 表格一行 / 带引号的值一起粘进来，
	// 这里先规整成裸值（否则报「sid 无效」，用户完全无从自查）。
	s := comate.NormalizeSID(sid)
	if s == "" {
		return "", "", errors.New("wps_sid 不能为空")
	}
	uid, models, err := comate.ValidateSIDWithModels(s)
	if err != nil {
		return "", "", err
	}

	// 同账号检测：与已入库 comate 账号的 sid 逐字比对。
	// 顺带取回「已存在的这个账号的昵称」——用户可能在面板上手工改过名（实测就有人改成
	// 「business」），重贴 sid 时不能把人家改的名字冲掉。
	warn := ""
	prevNick := ""
	if existing, lerr := auth.LoadComateDir(a.cfg.AuthDir); lerr == nil {
		for _, e := range existing {
			if e.AccessTokenValue() == s {
				warn = fmt.Sprintf("这个 wps_sid 与已添加的账号 %s 相同（积分按 WPS 账号计），"+
					"重复添加不会增加可用积分。请换一个 WPS 账号的 wps_sid。", e.UID)
				break
			}
		}
		for _, e := range existing {
			if e.UID == uid {
				prevNick = e.Nickname
				break
			}
		}
	}

	_ = os.MkdirAll(a.cfg.AuthDir, 0o755)
	// 昵称用上游真实身份（R136）：/api/coserve/v1/users/current 会给昵称与企业名，
	// 比「WPS 账号 2f4da9」可读得多——多账号时一眼能分清企业号与个人号。
	// 取不到就回退 uid 尾号（不让装饰性请求挡住保存凭据）。
	nick := comateNicknameFor(uid, prevNick, func() (string, error) {
		nickName, company, perr := comate.FetchProfile(s)
		if perr != nil {
			return "", perr
		}
		return comate.DisplayName(nickName, company), nil
	})
	ca := &auth.Auth{
		Kind:        "comate",
		UID:         uid,
		Nickname:    nick,
		AccessToken: s,
		// Cookie 无过期概念：ExpiresAt 必须远期，0 会让 NeedsRefresh 恒真（不变量 21 同款陷阱）
		ExpiresAt: comate.FarFutureExpiry(),
		FilePath:  filepath.Join(a.cfg.AuthDir, "comate-"+uid+".json"),
	}
	if err := ca.SaveAtomic(); err != nil {
		return "", "", fmt.Errorf("保存 Comate 凭据失败：%w", err)
	}
	// 日志零 token（不变量 2）：只打 uid，绝不打完整 sid
	log.Printf("comate 账号已添加（粘贴 wps_sid）uid=%s 模型数=%d 同账号告警=%v", uid, models, warn != "")
	a.reloadAccounts()
	a.afterAccountAdded(provider.Comate)
	if models == 0 {
		// R135：目录为空 = 这个 WPS 账号在 Comate 侧一个模型都没有（典型：个人号「注册了但没开通」）。
		// 把它留在池里的代价是整条渠道瘫痪：实测每次对话都被它接走、返回 mock 帧「未配置可用模型」，
		// 而该帧被归为模型级错误（ErrNotFound），账号永远不会冷却 ⇒ 6/6 全失败。
		// 故落盘后立刻停用：账号仍可见、可手动启用，但没有模型就不参与路由。
		// 走 Pool.Disable 而非 DisableAccount，是为了让停用原因写在账号卡片上（面板启用时自动清空）。
		if rt, _ := a.findRuntimeAuth(uid); rt != nil && rt.Pool != nil {
			rt.Pool.Disable(uid, "该账号没有可用模型（未开通 Comate），已自动停用")
			log.Printf("comate 账号暂无可用模型，已自动停用 uid=%s", uid)
		} else {
			log.Printf("comate 账号暂无可用模型，但未找到运行时（无法自动停用）uid=%s", uid)
		}
		noModels := "该账号在 Comate 侧没有任何可用模型（多半还没开通 Comate，个人号常见），" +
			"已自动停用以免拖累整个渠道；开通后在这个账号上点『启用』即可。"
		if warn == "" {
			warn = noModels
		} else {
			warn += " " + noModels
		}
	}
	return uid, warn, nil
}

// comateNicknamePrefix 占位昵称前缀：R136 之前的账号昵称只有这个 + uid 尾号。
const comateNicknamePrefix = "WPS 账号"

// comateNickname 账号展示名（uid 末尾 6 位足够区分同一台机器上的多个 WPS 账号）。
// R136 起这是**兜底**：正常路径用 comate.FetchProfile 拿真实昵称与企业名。
func comateNickname(uid string) string {
	if len(uid) <= 6 {
		return comateNicknamePrefix
	}
	return comateNicknamePrefix + " " + uid[len(uid)-6:]
}

// comateNicknameFor 决定保存凭据时写的昵称：
//  1. 已存在且**不是占位昵称**的名字优先 —— 那是用户自己改的，不能因为重贴 sid /
//     自动巡检就冲掉（实测有用户把企业号改成「business」）；
//  2. 否则取上游真实身份（昵称（企业名））；
//  3. 再不行回退 uid 尾号，绝不让装饰性请求挡住保存凭据。
func comateNicknameFor(uid, prev string, fetch func() (string, error)) string {
	if p := strings.TrimSpace(prev); p != "" && !strings.HasPrefix(p, comateNicknamePrefix) {
		return p
	}
	if fetch != nil {
		if disp, err := fetch(); err == nil {
			if d := strings.TrimSpace(disp); d != "" {
				return d
			}
		}
	}
	return comateNickname(uid)
}

// SweepComateEmptyAccounts 启动后扫一遍 comate 账号的模型目录，
// 把「一个模型都没有」的账号停用（R135）；顺带把 R136 之前入库的
// 占位昵称（「WPS 账号 xxxxxx」）换成上游真实身份。
//
// 为什么需要巡检：SubmitComateSID 只挡得住**新增**的空目录账号，而线上已有一批
// 在 R135 之前加进来的（正是「登进去看不到模型」那批用户）。这类账号留在池里会
// 让整条渠道瘫痪 —— 它按 uid 顺序被选走、返回 mock 帧「未配置可用模型」，而该帧
// 是 HTTP 200 + 正常 SSE 头，handler 无法在同一请求内换号，于是所有人看到失败。
//
// 判据必须严格：只有 /models 明确 `code==0` 且 `items` 为空才算空目录。
// 网络错误、非 200、业务码非 0 一律跳过 —— 上游抖动不该把好账号停掉。
func (a *App) SweepComateEmptyAccounts() {
	auths, err := auth.LoadComateDir(a.cfg.AuthDir)
	if err != nil {
		log.Printf("comate 目录巡检跳过：读取账号失败 %v", err)
		return
	}
	renamed := false
	for _, au := range auths {
		rt, _ := a.findRuntimeAuth(au.UID)
		if rt == nil || rt.Pool == nil {
			log.Printf("comate 目录巡检：未找到运行时，跳过 uid=%s", au.UID)
			continue
		}
		// 昵称补全（R136）：占位昵称换成「昵称（企业名）」。失败静默保持原样 ——
		// 这只是展示层装饰，不该在启动日志里制造噪声。
		if strings.HasPrefix(au.Nickname, comateNicknamePrefix) {
			if nick, company, perr := comate.FetchProfile(au.AccessTokenValue()); perr == nil {
				if disp := comate.DisplayName(nick, company); disp != "" {
					au.Nickname = disp
					if serr := au.SaveAtomic(); serr == nil {
						renamed = true
						log.Printf("comate 昵称已补全 uid=%s", au.UID)
					}
				}
			}
		}
		// 已停用的账号不重复探测（用户手工停用的，尊重现状）。
		if st, ok := rt.Pool.Status(au.UID); ok && st.Disabled {
			continue
		}
		_, models, verr := comate.ValidateSIDWithModels(au.AccessTokenValue())
		if verr != nil {
			log.Printf("comate 目录巡检跳过 uid=%s：%v", au.UID, verr)
			continue
		}
		if models > 0 {
			continue
		}
		rt.Pool.Disable(au.UID, "该账号没有可用模型（未开通 Comate），已自动停用")
		log.Printf("comate 目录巡检：账号无可用模型，已自动停用（R135）uid=%s", au.UID)
	}
	if renamed {
		// 昵称已落盘，重载一次让面板生效（否则要等下一次账号变更才刷新）。
		a.reloadAccounts()
	}
}

// completeLingxiLogin CDP 自动登录（login_lingxi）成功后的收尾：入库。
//
// 与其它 CDP 渠道的 complete*Login 同构（wrapPoll 的 done 闭包上抛错误由 pollLogin 展示）；
// 落盘走 persistLingxiAccount，与粘贴路径 SubmitLingxiSID 完全一致。
func (a *App) completeLingxiLogin(r loginlingxi.Result) error {
	warn, err := a.persistLingxiAccount(r.SID, r.UID)
	if err != nil {
		// persist 内部（SaveAtomic 失败）已带「保存 Lingxi 凭据失败」前缀，这里不再重复包装。
		return err
	}
	// 日志零 token（不变量 2）：只打 uid，绝不打完整 sid
	log.Printf("lingxi 账号已添加（自动登录）uid=%s 同账号告警=%v", r.UID, warn != "")
	return nil
}

// SubmitLingxiSID 提交用户粘贴的 wps_sid Cookie（灵犀渠道）：验证 → 落盘 → 重载账号池。
//
// 与 comate 的差异：验证走 GET plans 端点（零智点消耗），且能在验证阶段就发现
// 「灵犀侧未开通」（result=UserUnauthorized）——同一个 wps_sid 在 comate 与灵犀
// 是两道独立门禁，提前拦在这里可避免入库一个必然空目录的账号。
//
// 同账号检测：wps_sid 与已入库 lingxi 账号逐字比对（智点按 WPS 账号计）。
func (a *App) SubmitLingxiSID(sid string) (string, string, error) {
	s := strings.TrimSpace(sid)
	if s == "" {
		return "", "", errors.New("wps_sid 不能为空")
	}
	uid, err := lingxi.ValidateSID(s)
	if err != nil {
		return "", "", err
	}
	warn, err := a.persistLingxiAccount(s, uid)
	if err != nil {
		return "", "", err
	}
	// 日志零 token（不变量 2）：只打 uid，绝不打完整 sid
	log.Printf("lingxi 账号已添加（粘贴 wps_sid）uid=%s 同账号告警=%v", uid, warn != "")
	return uid, warn, nil
}

// persistLingxiAccount lingxi 账号入库的唯一路径：同账号检测 → 落盘 → 重载账号池。
// 与 persistComateAccount 同构；返回同账号告警文案（空串=无告警）与入库错误。
func (a *App) persistLingxiAccount(sid string, uid string) (string, error) {
	// 同账号检测：与已入库 lingxi 账号的 sid 逐字比对。
	warn := ""
	if existing, lerr := auth.LoadLingxiDir(a.cfg.AuthDir); lerr == nil {
		for _, e := range existing {
			if e.AccessTokenValue() == sid {
				warn = fmt.Sprintf("这个 wps_sid 与已添加的账号 %s 相同（智点按 WPS 账号计），"+
					"重复添加不会增加可用智点。请换一个 WPS 账号的 wps_sid。", e.UID)
				break
			}
		}
	}

	_ = os.MkdirAll(a.cfg.AuthDir, 0o755)
	la := &auth.Auth{
		Kind:        "lingxi",
		UID:         uid,
		Nickname:    "WPS 账号 " + uid[len(uid)-6:],
		AccessToken: sid,
		// Cookie 无过期概念：ExpiresAt 必须远期，0 会让 NeedsRefresh 恒真（不变量 21 同款陷阱）
		ExpiresAt: lingxi.FarFutureExpiry(),
		FilePath:  filepath.Join(a.cfg.AuthDir, "lingxi-"+uid+".json"),
	}
	if err := la.SaveAtomic(); err != nil {
		return "", fmt.Errorf("保存 Lingxi 凭据失败：%w", err)
	}
	a.reloadAccounts()
	a.afterAccountAdded(provider.Lingxi)
	return warn, nil
}

// StartComateQRLogin 发起 WPS Comate「扫码登录」：服务端向 qr.wps.cn 申请二维码，
// 面板显示二维码，用户用手机 WPS 扫码确认后，服务端自己换出 wps_sid 并落盘。
//
// 为什么不像 GLM 那样用 CDP 拉浏览器：CDP 只能拉起「运行 wild-work 的那台机器」的
// 浏览器，NAS / docker / systemd 没有桌面，窗口永远弹不出来；而用户自己浏览器里的
// Cookie 又受同源策略保护读不到。扫码这条路把登录放在服务端完成，与部署环境无关。
//
// 返回后立即进入 pending；前端轮询 GetComateQRLoginStatus 获知二维码与扫码进度。
// 校验 + 落盘复用 SubmitComateSID —— 自动与手工两条入口共用同一份验证、
// 同账号告警与日志纪律，不各写一份。
func (a *App) StartComateQRLogin() error {
	a.muComateLogin.Lock()
	defer a.muComateLogin.Unlock()
	if a.comateLogin != nil {
		switch a.comateLogin.State() {
		case logincomate.QRStatePending, logincomate.QRStateScanned:
			return errors.New("已有 Comate 扫码登录进行中，请在手机上完成确认，或先取消")
		}
	}
	sess, err := logincomate.StartQR(func(sid string) (logincomate.Result, error) {
		uid, warn, serr := a.SubmitComateSID(sid)
		if serr != nil {
			return logincomate.Result{}, serr
		}
		return logincomate.Result{UID: uid, Nickname: comateNickname(uid), Warn: warn}, nil
	})
	if err != nil {
		return err
	}
	a.comateLogin = sess
	return nil
}

// GetComateQRLoginStatus 查询扫码登录状态：state（idle/pending/scanned/success/failed/
// cancelled）、二维码图片地址 qr、错误信息、成功时的 uid。
func (a *App) GetComateQRLoginStatus() map[string]any {
	a.muComateLogin.Lock()
	sess := a.comateLogin
	a.muComateLogin.Unlock()
	if sess == nil {
		return map[string]any{"state": "idle"}
	}
	return sess.Snapshot()
}

// CancelComateQRLogin 取消扫码登录。
func (a *App) CancelComateQRLogin() error {
	a.muComateLogin.Lock()
	sess := a.comateLogin
	a.muComateLogin.Unlock()
	if sess == nil {
		return errors.New("没有进行中的 Comate 登录")
	}
	sess.Cancel()
	return nil
}

// ComateBrowserInfo 报告「用本机浏览器登录」这条路是否可用。
//
// configured=false（没配 login.comate_browser_cdp）时前端根本不显示入口；
// configured=true 但 probe 失败时仍显示入口，但把原因写进 hint ——
// 用户自己开浏览器时忘加 --remote-debugging-port 是最常见的坑，
// 静默隐藏会让人以为功能不存在。
func (a *App) ComateBrowserInfo() map[string]any {
	ep := strings.TrimSpace(a.cfg.Login.ComateBrowserCDP)
	out := map[string]any{"configured": ep != "", "endpoint": ep}
	if ep == "" {
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := cdp.Probe(ctx, ep); err != nil {
		out["available"] = false
		out["hint"] = err.Error()
		return out
	}
	out["available"] = true
	return out
}

// StartComateBrowserLogin 接管配置好的本机浏览器：开一个 WPS 登录页标签，
// 用户登录后由后台轮询该浏览器的 Cookie，读到 wps_sid 即校验并落盘。
//
// 与扫码的分工：扫码不需要任何浏览器（服务器无桌面也能用）；
// 这条路复用用户天天在用的浏览器，登录一次、多号可切，适合桌面用户。
func (a *App) StartComateBrowserLogin() error {
	ep := strings.TrimSpace(a.cfg.Login.ComateBrowserCDP)
	if ep == "" {
		return errors.New("未启用「用本机浏览器登录」：请在 config.json 的 login.comate_browser_cdp 填写浏览器调试地址（如 http://127.0.0.1:9222）")
	}
	a.muComateLogin.Lock()
	defer a.muComateLogin.Unlock()
	if a.comateBrowser != nil {
		switch a.comateBrowser.State() {
		case logincomate.QRStatePending, logincomate.QRStateScanned:
			return errors.New("已有 Comate 浏览器登录进行中，请先在浏览器里完成登录，或先取消")
		}
	}
	sess, err := logincomate.StartBrowserLogin(ep, func(sid string) (logincomate.Result, error) {
		uid, warn, serr := a.SubmitComateSID(sid)
		if serr != nil {
			return logincomate.Result{}, serr
		}
		return logincomate.Result{UID: uid, Nickname: comateNickname(uid), Warn: warn}, nil
	})
	if err != nil {
		return err
	}
	a.comateBrowser = sess
	return nil
}

// GetComateBrowserLoginStatus 查询「用本机浏览器登录」的状态。
func (a *App) GetComateBrowserLoginStatus() map[string]any {
	a.muComateLogin.Lock()
	sess := a.comateBrowser
	a.muComateLogin.Unlock()
	if sess == nil {
		return map[string]any{"state": "idle"}
	}
	return sess.Snapshot()
}

// CancelComateBrowserLogin 取消「用本机浏览器登录」（不关闭用户的浏览器）。
func (a *App) CancelComateBrowserLogin() error {
	a.muComateLogin.Lock()
	sess := a.comateBrowser
	a.muComateLogin.Unlock()
	if sess == nil {
		return errors.New("没有进行中的 Comate 浏览器登录")
	}
	sess.Cancel()
	return nil
}

// StartGLMAutoLogin 发起智谱清言自动登录：拉起独立 profile 的浏览器，
// 用户在真实浏览器里登录，后台经 CDP 自动捕获 Cookie。
//
// 返回后立即进入 pending；前端轮询 GetGLMAutoLoginStatus 获知结果。
func (a *App) StartGLMAutoLogin() error {
	a.muGLMLogin.Lock()
	defer a.muGLMLogin.Unlock()
	if a.glmLogin != nil {
		if st, _ := a.glmLogin.Status(); st == "pending" {
			return errors.New("已有智谱清言登录流程进行中，请先在浏览器完成登录或取消")
		}
	}
	sess, err := loginglm.AutoLogin(a.cfg.AuthDir, func(r loginglm.Result) {
		// 落盘成功后重载账号池（在后台 goroutine 里执行）
		a.reloadAccounts()
		a.afterAccountAdded(provider.GLM)
		log.Printf("glm 自动登录完成，账号池已重载 uid=%s", r.UID)
	})
	if err != nil {
		return err
	}
	a.glmLogin = sess
	return nil
}

// GetGLMAutoLoginStatus 查询自动登录状态。
// 返回 status（idle/pending/success/failed/cancelled）、错误信息与成功时的 uid。
func (a *App) GetGLMAutoLoginStatus() map[string]any {
	a.muGLMLogin.Lock()
	sess := a.glmLogin
	a.muGLMLogin.Unlock()
	if sess == nil {
		return map[string]any{"status": "idle"}
	}
	status, err := sess.Status()
	out := map[string]any{"status": status}
	if err != nil {
		out["error"] = err.Error()
	}
	if r, ok := sess.Result(); ok {
		out["uid"] = r.UID
		out["nickname"] = r.Nickname
	}
	return out
}

// CancelGLMAutoLogin 取消自动登录并关闭浏览器。
func (a *App) CancelGLMAutoLogin() error {
	a.muGLMLogin.Lock()
	sess := a.glmLogin
	a.muGLMLogin.Unlock()
	if sess == nil {
		return errors.New("没有进行中的智谱清言登录")
	}
	sess.Cancel()
	return nil
}

// StartOfficeAceLogin 发起 OfficeAce 自助登录（华为云 OAuth 授权码 + PKCE + DPoP）。
// 返回待打开的授权 URL；后台轮询完成登录并落盘。前端轮询 GetOfficeAceLoginStatus。
//
// 不在服务端开浏览器（#143）：授权页由前端 window.open 在「用户正在使用的设备」打开
// —— 捕获走云端轮询 state→code，与授权页在哪台设备打开无关；服务端弹窗对局域网用户
// 不可见，反而造成「点了没反应」的困惑。前端有「复制链接」兜底。
//
// 并发约定（2026-10-17 修）：只有「进行中」的会话才拒绝新请求；已进入终态的旧会话
// 必须**先清空再解锁**，否则并发的状态轮询会读到上一轮的 done/failed，把用户新点的
// 「新增账号」直接顶成旧终态（表现为「点了没反应」）。
func (a *App) StartOfficeAceLogin() (string, error) {
	a.muOALogin.Lock()
	if a.oaLogin != nil {
		if st, _ := a.oaLogin.Status(); oaLoginActive(st) {
			url := a.oaLogin.URL()
			a.muOALogin.Unlock()
			return url, errors.New("已有 OfficeAce 登录流程进行中，请先完成或取消")
		}
		// 上一轮已到终态：清空，让本次新建的会话立刻成为唯一可见状态。
		// 刻意在上锁期间清空 —— 否则并发的 GetOfficeAceLoginStatus 会读到旧会话。
		a.oaLogin = nil
	}
	sess, err := officeace.StartLogin(a.cfg.AuthDir, func(r officeace.LoginResult) {
		a.reloadAccounts()
		a.afterAccountAdded(provider.OfficeAce)
		log.Printf("officeace 自助登录完成，账号池已重载 uid=%s", r.UID)
	})
	if err != nil {
		a.muOALogin.Unlock()
		return "", err
	}
	a.oaLogin = sess
	// 不在服务端开浏览器：授权页由前端 window.open 在「用户正在使用的设备」打开
	//（捕获走云端轮询 state→code，与授权页在哪台设备打开无关；服务端弹窗对
	// 局域网用户不可见，反而造成「点了没反应」的困惑）。前端有复制链接兜底。
	return sess.URL(), nil
}

// GetOfficeAceLoginStatus 查询 OfficeAce 自助登录状态。
// GetOfficeAceLoginStatus 查询 OfficeAce 自助登录状态。
func (a *App) GetOfficeAceLoginStatus() officeAceLoginStatus {
	a.muOALogin.Lock()
	sess := a.oaLogin
	a.muOALogin.Unlock()
	if sess == nil {
		return officeAceLoginStatus{Status: "idle"}
	}
	status, err := sess.Status()
	out := officeAceLoginStatus{Status: status}
	if status == "" {
		// 会话刚建立、run() 还没写首个状态：按「进行中」上报，避免前端误判成空闲。
		out.Status = "pending"
	}
	if err != nil {
		out.Error = err.Error()
	}
	if r, ok := sess.Result(); ok {
		out.UID = r.UID
		out.Nickname = r.Nickname
	}
	return out
}

// CancelOfficeAceLogin 取消 OfficeAce 自助登录。
func (a *App) CancelOfficeAceLogin() error {
	a.muOALogin.Lock()
	sess := a.oaLogin
	a.muOALogin.Unlock()
	if sess == nil {
		return errors.New("没有进行中的 OfficeAce 登录")
	}
	sess.Cancel()
	return nil
}

// officeAceLoginStatus OfficeAce 自助登录状态（**单一来源**：前端读的字段名以此为准）。
//
// 字段契约（app.js 的 startOfficeAcePoll 依赖它）：
//
//	status: pending | exchanging | done | failed | expired | canceled | idle
//	可选 error / uid / nickname / warning
//
// 历史：这里原先返回 map[string]any，字段名靠手写字符串维护 —— 改一个拼写前端就静默读不到
// （表现为「永远转圈」）。改成结构体后由 json 标签约束，并有静态断言测试锁死。
type officeAceLoginStatus struct {
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
	UID      string `json:"uid,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	AuthURL  string `json:"auth_url,omitempty"`
	Warning  string `json:"warning,omitempty"`
}

// oaLoginActive 该状态是否表示「流程进行中」（此时不允许再起一条）。
func oaLoginActive(st string) bool { return st == "pending" || st == "exchanging" }

// completeQwenWorkLogin 登录成功：写 auth 文件、重载账号池。
// 千问办公无签到活动（每日积分服务端被动发放），不做首次签到。
func (a *App) completeQwenWorkLogin(r loginqwenwork.Result) {
	log.Printf("qwenwork 登录成功 uid=%s nickname=%s expires_in=%d refresh_token=%t",
		r.UID, r.Nickname, r.ExpiresIn, r.RefreshToken != "")
	fp, err := loginqwenwork.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("qwenwork 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		return
	}
	log.Printf("qwenwork 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	a.finishLogin()
	a.afterAccountAdded(provider.QwenWork)
	// 首屏初始化（顺序敏感，串行执行）：
	// 1) 刷新余额 —— 若 Poll 兑换的首个 token 无效（实测 OAuth 兑换 token 调 /user/info
	//    会 401 invalid-credential），refreshIfSessionDead 会自动换新 token；
	// 2) 之后再用有效 token 拉昵称兑底（Poll 兑换的 JWT 实测无 username 字段，
	//    refresh 后的 device_token 才有），写回 auth 文件，否则面板显示 hex uid。
	a.safeGo(func() {
		var nickname = r.Nickname
		if remain, err := a.RefreshCredits(r.UID); err != nil {
			log.Printf("qwenwork 新账号积分获取失败 %s: %v", r.Nickname, err)
		} else {
			log.Printf("qwenwork 新账号积分获取完成 %s: %d", r.Nickname, remain)
		}
		if nickname == "" {
			rt := a.runtime(provider.QwenWork)
			if rt == nil || rt.Upstream == nil {
				return
			}
			au := rt.Pool.AuthByUID(r.UID)
			if au == nil {
				return
			}
			qw, ok := rt.Upstream.(*qwenwork.Client)
			if !ok {
				return
			}
			if nick, nerr := qw.FetchNickname(au); nerr == nil && nick != "" {
				nickname = nick
				au.Nickname = nick // AuthByUID 返回池内指针，改字段即生效；SaveAtomic 自带锁
				_ = au.SaveAtomic()
				log.Printf("qwenwork 昵称兑底成功 uid=%s nickname=%s", r.UID, nick)
			} else if nerr != nil {
				log.Printf("qwenwork 昵称兑底失败 uid=%s err=%v", r.UID, nerr)
			}
		}
	})
}

func (a *App) finishLogin() {
	a.muLogin.Lock()
	a.loginBusy = false
	a.loginCtx, a.loginCancel, a.loginClient = nil, nil, nil
	a.loginKind = ""
	a.muLogin.Unlock()
}

// setLoginError 记录本次登录的终态错误（供面板展示）。调用方需持有 a.muLogin 或在其外调用。
func (a *App) setLoginError(msg string) {
	a.muLogin.Lock()
	a.loginErr = msg
	a.muLogin.Unlock()
}

// peekLoginError 读取本次登录的终态错误（不清空）。
//
// 故意**非破坏式**：`/api/state` 有多个调用方（页面加载、其他轮询），
// 若读取即清空，面板的登录轮询可能拿不到而被别的调用偷走，toast 就永远不显示了。
// 由下一次 startLogin 置空。
func (a *App) peekLoginError() string {
	a.muLogin.Lock()
	defer a.muLogin.Unlock()
	return a.loginErr
}

// reloadAccounts 用 auths 目录最新文件对齐账号池。
// 注意：oczen 与 kilo 不在此列——两者的虚拟账号都没有凭证文件，
// SyncToDir 会因「目录扫描不到」而把它从池里剔除，故只能由 main 装配时注入一次。
func (a *App) reloadAccounts() {
	if rt := a.runtime(provider.WorkBuddy); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadWorkBuddyDir(a.cfg.AuthDir, a.cfg.Region)
		if err != nil {
			log.Printf("reload workbuddy accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.TraeWork); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadTraeDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload traework accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.Qoder); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadQoderDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload qoder accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.QoderCN); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadQoderCNDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload qodercn accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.QoderCOM); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadQoderCOMDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload qodercom accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.WorkBuddyAI); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadWorkBuddyAiDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload workbuddyai accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.Minimax); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadMinimaxDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload minimax accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.LobsterAI); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadLobsterAIDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload lobsterai accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.ZCode); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadZCodeDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload zcode accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.Gemini); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadGeminiDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload gemini accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.CodeArts); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadCodeArtsDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload codearts accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.GLM); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadGLMDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload glm accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.QwenWork); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadQwenWorkDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload qwenwork accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.MonkeyCode); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadMonkeyCodeDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload monkeycode accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.Raccoon); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadRaccoonDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload raccoon accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.Loomy); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadLoomyDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload loomy accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	// Cline：一把官方 API Key = 一个账号，凭据由面板「粘贴 API Key」写入。
	if rt := a.runtime(provider.Cline); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadClineDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload cline accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	// Comate：一个 WPS 账号一份 wps_sid，凭据由面板「粘贴 wps_sid」写入。
	if rt := a.runtime(provider.Comate); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadComateDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload comate accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	// Lingxi：一个 WPS 账号一份 wps_sid（灵犀侧开通），凭据由面板「粘贴 wps_sid」写入。
	if rt := a.runtime(provider.Lingxi); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadLingxiDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload lingxi accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	// OfficeAce：华为云果办，凭据由面板「浏览器登录」或「从客户端导入」写入 auths/。
	// 两条添加路径（OAuth onDone / importOfficeAce）都依赖本分支把新账号同步进池，
	// 缺了它账号要等 daemon 重启才出现（R34/R41 家族的「声明了却没接线」）。
	if rt := a.runtime(provider.OfficeAce); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadOfficeAceDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload officeace accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
}

// CheckinAccount 单个账号立即签到。
func (a *App) CheckinAccount(uid string) (scheduler.CheckinResult, error) {
	rt, _ := a.findRuntimeAuth(uid)
	if rt == nil || rt.Scheduler == nil {
		return scheduler.CheckinResult{}, fmt.Errorf("unknown account %s", uid)
	}
	if a.noExplicitCheckinKind(rt.Kind) {
		if rt.Kind == provider.WorkBuddyAI {
			return scheduler.CheckinResult{}, fmt.Errorf("workbuddyai 渠道无需手动签到，已定时自动领取日活奖励")
		}
		return scheduler.CheckinResult{}, fmt.Errorf("%s 渠道不支持手动签到", rt.Kind)
	}
	res, err := rt.Scheduler.CheckinAccount(uid)
	if err != nil {
		log.Printf("checkin failed uid=%s err=%v", uid, err)
		return res, err
	}
	log.Printf("checkin uid=%s ok=%t retryable=%t msg=%s remain=%d has_remain=%t", uid, res.OK, res.Retryable, res.Msg, res.Remain, res.HasRemain)
	return res, nil
}

// CheckinAll 全部账号立即签到。
func (a *App) CheckinAll() []scheduler.CheckinResult {
	results := make([]scheduler.CheckinResult, 0)
	for _, rt := range a.runtimesSnapshot() {
		// 别名渠道复用同一 Pool 且无独立 Scheduler：计入会重复签到同一批账号。
		if rt == nil || rt.Pool == nil || rt.Scheduler == nil || rt.Alias {
			continue
		}
		if a.noExplicitCheckinKind(rt.Kind) { // 无签到活动渠道，跳过
			continue
		}
		for _, st := range rt.Pool.List() {
			if st.Disabled {
				results = append(results, scheduler.CheckinResult{UID: st.UID, Msg: "账号已禁用"})
				continue
			}
			res, err := rt.Scheduler.CheckinAccount(st.UID)
			if err != nil {
				res = scheduler.CheckinResult{UID: st.UID, Msg: err.Error()}
			}
			results = append(results, res)
		}
	}
	ok := 0
	for _, r := range results {
		if r.OK {
			ok++
		}
	}
	log.Printf("批量签到完成：total=%d ok=%d failed=%d", len(results), ok, len(results)-ok)
	return results
}

// CreditRefreshInterval 积分自动刷新间隔。
// 无签到活动的渠道（如 WorkBuddy 国际版）积分不随签到更新，
// 若不定期刷新：面板长期显示旧值（新账号则一直为 0），
// 且 Pool.Pick() 的最早到期时刻会因此长期陈旧而选错账号。
//
// 2026-10-20 由 30min 调到 10min（用户要求）：代价是每账号的
// UserResourceDetail 请求数与 state.json 落盘次数 ×3，换来「到期时刻已过」更快被纠正
// （过期后该账号必须靠下一次刷新才会从队首退到余额兜底组）。
const CreditRefreshInterval = 10 * time.Minute

// refreshIfSessionDead 上游报「登录态失效」时刷新一次 token 并写回，返回是否已刷新。
//
// 为何不能只依赖 NeedsRefresh：它比的是本地 expiresAt，而本地时钟可能是错的——
// 例如 token 已在别处（另一实例/客户端）被轮换、或上次刷新后未落盘，
// 此时本地仍显示「有效」而上游已拒绝 → 不刷新就永久卡在 401（积分恒为 0、明细恒为空）。
// 故必须对 401 本身做一次「刷新 + 重试」。
func (a *App) refreshIfSessionDead(rt *Runtime, au *auth.Auth, err error) bool {
	var ue *provider.Error
	if !errors.As(err, &ue) || ue.Kind != provider.ErrSessionDead {
		return false
	}
	if au.RefreshToken == "" {
		return false
	}
	log.Printf("session dead, refreshing platform=%s uid=%s", rt.Kind, au.UID)
	if rerr := rt.Upstream.RefreshToken(au); rerr != nil {
		log.Printf("session dead refresh failed platform=%s uid=%s err=%v", rt.Kind, au.UID, rerr)
		return false
	}
	// 刷新成功必须落盘：否则下次启动又拿旧 token，重回 401。
	if serr := au.SaveAtomic(); serr != nil {
		log.Printf("session dead refresh save failed platform=%s uid=%s err=%v", rt.Kind, au.UID, serr)
	}
	return true
}

// creditSnapshot 实时拉取明细并算出「可消耗余额」「临期额度」「不可消耗余额」三口径，
// 成功即回写 pool 快照 + ledger 差分。三者同源于 UserResourceDetail 的单次响应：remain 即
// pool 路由口径的可消耗余额，临期额度为其中 provider.ExpiringDays 个自然日内到期的部分
// （**仅展示**，R112 起不参与选号），不可消耗部分由条目的 Usable 标记汇总得到
// （渠道不区分专用池时为 0；渠道不下发到期时间时临期为 0，如 Qoder）。
// 同一响应里还算出「最早到期时刻」一并写入 pool——那是 Pick() 的唯一排序键。
// 遇 401 自动刷新 token 并重试一次（见 refreshIfSessionDead）。
//
// 顶层（卡片/汇总条/Pick 选号）与明细弹层从此同源：旧实现里 pool 快照只被「刷新事件」
// （手动刷新/全部刷新/自动循环）更新，而明细是 hover 时的实时值，两者必然相差区间内消耗量
// ——面板「同屏两个数字对不上」实为时差而非上游口径问题（已实测同响应内上游汇总字段==明细池
// 相加）。hover 明细也回写后，弹层展开那一刻全屏数字同源于同一响应。
func (a *App) creditSnapshot(rt *Runtime, au *auth.Auth) (remain int64, items []provider.ResourceItem, expiring, unusable int64, err error) {
	remain, items, err = rt.Upstream.UserResourceDetail(au)
	if err != nil && a.refreshIfSessionDead(rt, au, err) {
		remain, items, err = rt.Upstream.UserResourceDetail(au)
	}
	if err != nil {
		return 0, nil, 0, 0, err
	}
	_, unusable = provider.Summarize(items)
	// 临期额度：自然日口径（provider.ExpiringDays 个自然日内到期，次日 00:00 失效算今日）。
	// R112 起**仅展示**（面板三口径/汇总条），不参与选号——选号用下面的 earliest。
	now := time.Now()
	expiring = provider.ExpiringWithinDays(items, provider.ExpiringDays, now)
	// 最早到期时刻：Pick() 的第一排序键（R112）。ok=false（如 Qoder 不下发到期时间）
	// 时返回零值，pool 会把它排到「有到期信息」的账号之后按余额兜底。
	earliest, _ := provider.EarliestExpire(items, now)
	// 差分记账：与上次快照对比产出 earn/spend/expire 流水（锦上添花，失败不干扰）
	// 积分流水只记「积分口径」的余额变化：token 计量渠道（credits_unit="token"）的 remain
	// 是 token 数，记进积分流水会污染统计（2026-10-17 用户实测报告）。
	// Token 用量本身另有 token 流水（按请求实测计量），不受影响。
	if a.ledger != nil && a.creditsUnit(rt.Kind.String()) == "" {
		a.ledger.DiffCredits(rt.Kind.String(), au.UID, remain, items)
	}
	rt.Pool.SetCreditDetail(au.UID, remain, expiring, unusable, earliest)
	return remain, items, expiring, unusable, nil
}

// creditTotals 取三口径小计（不含 items），并在同一响应上喂入运行统计的最近临期。
func (a *App) creditTotals(rt *Runtime, au *auth.Auth) (usable, expiring, unusable int64, err error) {
	remain, items, expiring, unusable, err := a.creditSnapshot(rt, au)
	if err != nil {
		return 0, 0, 0, err
	}
	// 运行统计：账号最近临期喂入（复用同一响应的 items，免二次上游调用）
	a.applyExpiryFromItems(au.UID, items)
	return remain, expiring, unusable, nil
}

// StartCreditAutoRefresh 后台定期刷新指定渠道的账号积分。
// 启动立即刷一次，之后每隔 interval 一次；单账号失败不影响其他账号。
//
// 范围限制在传入的 kinds：国内版 workbuddy 有自己的签到刷新途径，
// 不纳入此循环，以免改变其行为。
func (a *App) StartCreditAutoRefresh(ctx context.Context, kinds []provider.Kind, interval time.Duration) {
	if interval <= 0 {
		interval = CreditRefreshInterval
	}
	go func() {
		refresh := func() {
			for _, k := range kinds {
				rt := a.runtime(k)
				// 别名渠道复用主渠道 Pool：纳入会重复刷新同一批账号。
				if rt == nil || rt.Pool == nil || rt.Upstream == nil || rt.Alias {
					continue
				}
				for _, st := range rt.Pool.List() {
					if st.Disabled {
						// 停用账号不再刷资源：清掉引擎里的临期残留（sidecar 对 disabled 账号
						// 仍轮询 resource；此处写空值达到同等效果，防「最近临期」行冻结旧值）
						a.applyExpiryFromItems(st.UID, nil)
						continue
					}
					au := rt.Pool.AuthByUID(st.UID)
					if au == nil {
						continue
					}
					// token 临近过期时先刷新（国际版 token 有效期长，通常不触发）
					if au.NeedsRefresh(10 * time.Minute) {
						if err := rt.Upstream.RefreshToken(au); err != nil {
							log.Printf("credit auto-refresh token refresh failed platform=%s uid=%s err=%v", k, st.UID, err)
							continue
						}
						if err := au.SaveAtomic(); err != nil {
							log.Printf("credit auto-refresh token save failed platform=%s uid=%s err=%v", k, st.UID, err)
						}
					}
					usable, expiring, unusable, err := a.creditTotals(rt, au)
					if err != nil {
						log.Printf("credit auto-refresh failed platform=%s uid=%s err=%v", k, st.UID, err)
						continue
					}
					log.Printf("credit auto-refresh platform=%s uid=%s remain=%d expiring=%d unusable=%d", k, st.UID, usable, expiring, unusable)
				}
			}
		}
		refresh() // 启动即刷：新账号不必等下一个周期
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refresh()
			}
		}
	}()
}

// PricingRefreshInterval 模型列表与费率的后台自动刷新间隔。
// 两者都随上游变动，且面板依赖它们展示，故定期刷新保持新鲜。
const PricingRefreshInterval = 30 * time.Minute

// StartPricingAutoRefresh 后台定期刷新「模型列表 + 费率」。
// 启动立即刷一次：否则刚启动时费率缓存为空，面板下方全是 unknown，观感不佳。
// 之后每隔 interval 一次；取消 ctx 即停止。
func (a *App) StartPricingAutoRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = PricingRefreshInterval
	}
	go func() {
		// 启动刷新：同时失效动态模型缓存，保证模型列表也是新的
		if a.handler != nil {
			a.handler.InvalidateModels()
		}
		a.refreshChannelModels() // 填充模型缓存（issue #40：面板只读缓存，必须有填充源）
		a.RefreshPricing()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				a.refreshChannelModels() // 同上：随费率一起周期性拉新
				a.RefreshPricing()
			}
		}
	}()
}

// refreshChannelModels 拉取各渠道模型列表并填充 handler 的模型缓存。
//
// 为什么必须有这一步（issue #40）：费率面板读 CachedChannelModels（只读缓存、
// 不发网络请求），而缓存 rt.models 此前**只有 /v1/models 请求会填充**——
// RefreshPricing 只拉费率不拉模型。于是冷启动后，凡无静态兑底表的渠道
// （qodercn/qodercom）在面板上整组消失，直到某个客户端碰巧请求了 /v1/models。
// 故模型列表的填充必须与费率刷新同样走后台定时路径。
//
// 网络开销可控：ChannelModels 内部 fetchRuntimeModels 自带 1h TTL 与
// 5min 失败负缓存，与本函数的 30min 调度配合，不会打爆上游。
func (a *App) refreshChannelModels() {
	if a.handler == nil {
		return
	}
	a.handler.ChannelModels()
}

// RefreshCredits 刷新单个账号积分（返回可消耗余额）。
func (a *App) RefreshCredits(uid string) (int64, error) {
	rt, au := a.findRuntimeAuth(uid)
	if rt == nil || au == nil {
		return 0, fmt.Errorf("unknown account %s", uid)
	}
	if rt.Kind == provider.Oczen {
		// 匿名通道无积分概念，面板显示「不适用」，无刷新语义。
		return 0, fmt.Errorf("OpenCodeZen 匿名通道无积分，不适用")
	}
	if rt.Kind == provider.Kilo {
		// 免密免费池同样没有积分概念（面板显示「不适用」）。
		return 0, fmt.Errorf("Kilo 免费池无积分，不适用")
	}
	if rt.Custom {
		// 自定义渠道（R66）无积分接口：凭据本体是渠道级 api_key（对话转发用），
		// 不拒绝会落入 creditSnapshot 把 pool 快照写成 0（UserResourceDetail 恒返回空），
		// 让人误读成「额度已用光」。
		return 0, fmt.Errorf("%s 为自定义渠道，无积分概念，不适用", rt.Kind)
	}
	// 注意：ZCode **不**在这里拒绝 —— 它的额度是 token 口径，同样需要刷新才能看到用量；
	// 它的数字由 credits_unit="token" 标注，不进积分小计与流水（见 creditsUnit）。
	log.Printf("credits refresh start platform=%s uid=%s", rt.Kind, uid)
	usable, expiring, unusable, err := a.creditTotals(rt, au)
	if err != nil {
		log.Printf("credits refresh failed platform=%s uid=%s err=%v", rt.Kind, uid, err)
		return 0, err
	}
	log.Printf("credits refresh success platform=%s uid=%s remain=%d expiring=%d unusable=%d", rt.Kind, uid, usable, expiring, unusable)
	return usable, nil
}

// RefreshAll 刷新全部账号积分，返回汇总（供托盘消息框 / Web UI）。
func (a *App) RefreshAll() RefreshSummary {
	a.refreshMu.Lock()
	if a.refreshing {
		a.refreshMu.Unlock()
		return RefreshSummary{Busy: true}
	}
	a.refreshing = true
	a.refreshMu.Unlock()
	defer func() {
		a.refreshMu.Lock()
		a.refreshing = false
		a.refreshMu.Unlock()
	}()
	sum := RefreshSummary{Platforms: map[string]PlatformSummary{}}
	for _, rt := range a.runtimesSnapshot() {
		// 别名渠道复用同一 Pool：计入会重复刷新同一批账号。
		if rt == nil || rt.Pool == nil || rt.Upstream == nil || rt.Alias {
			continue
		}
		// oczen / kilo 无「额度」可刷：纳入只会产生一条无意义的失败记录。
		// （ZCode 要走刷新：它的额度是 token 口径，同样需要更新才能看到用量。）
		if rt.Kind == provider.Oczen || rt.Kind == provider.Kilo {
			continue
		}
		// 自定义渠道（R66）同样无积分概念：凭据本体是渠道级 api_key（对话转发用），
		// 占位账号无 access/refresh token，且 UserResourceDetail 恒返回空。
		// 纳入只会把每个自定义渠道都记成一条「no token」假失败（面板报「失败 N」噪音）。
		if rt.Custom {
			continue
		}
		ps := PlatformSummary{Accounts: []AccountRefresh{}}
		for _, st := range rt.Pool.List() {
			au := rt.Pool.AuthByUID(st.UID)
			// 凭据可用性判据：accessToken 或 refreshToken 任一非空即可。
			// glm 的 accessToken 允许为空（凭据本体是 refresh_token，见 LoadGLMDir），
			// 只查 accessToken 会把它误判成「no token」而永不刷新。
			if au == nil || (au.AccessTokenValue() == "" && au.RefreshTokenValue() == "") {
				ps.Failed++
				ps.Accounts = append(ps.Accounts, AccountRefresh{UID: st.UID, OK: false, Msg: "no token"})
				continue
			}
			if usable, _, _, err := a.creditTotals(rt, au); err == nil {
				ps.OK++
				ps.Accounts = append(ps.Accounts, AccountRefresh{UID: st.UID, OK: true, Remain: usable})
			} else {
				ps.Failed++
				ps.Accounts = append(ps.Accounts, AccountRefresh{UID: st.UID, OK: false, Msg: shortErr(err)})
				log.Printf("credits refresh failed platform=%s uid=%s err=%v", rt.Kind, st.UID, err)
			}
		}
		sum.Total += ps.OK + ps.Failed
		sum.OK += ps.OK
		sum.Failed += ps.Failed
		sum.Platforms[rt.Kind.String()] = ps
	}
	return sum
}

// RefreshSummary 积分刷新汇总（托盘消息框内容）。
type RefreshSummary struct {
	Busy      bool                       `json:"busy"`
	Total     int                        `json:"total"`
	OK        int                        `json:"ok"`
	Failed    int                        `json:"failed"`
	Platforms map[string]PlatformSummary `json:"platforms"`
}

// PlatformSummary 单个渠道汇总。
type PlatformSummary struct {
	OK       int              `json:"ok"`
	Failed   int              `json:"failed"`
	Accounts []AccountRefresh `json:"accounts"`
}

// AccountRefresh 单账号刷新结果。
type AccountRefresh struct {
	UID    string `json:"uid"`
	OK     bool   `json:"ok"`
	Remain int64  `json:"remain"`
	Msg    string `json:"msg,omitempty"`
}

// RemoveAccount 删除账号（auth 文件 + 内存池）。
// oczen / kilo 是例外：免密渠道的虚拟账号不可删除（需求约束），返回明确错误而非默默失败。
func (a *App) RemoveAccount(uid string) error {
	rt, au := a.findRuntimeAuth(uid)
	if rt == nil || au == nil {
		return fmt.Errorf("unknown account %s", uid)
	}
	if rt.Kind == provider.Oczen {
		return fmt.Errorf("OpenCodeZen 匿名通道账号不可删除")
	}
	if rt.Kind == provider.Kilo {
		return fmt.Errorf("Kilo 免费池账号不可删除")
	}
	if rt.Custom {
		var next []custom.ChannelConfig
		for _, cc := range a.cfg.CustomChannels {
			if !strings.EqualFold(cc.Name, rt.Kind.String()) {
				next = append(next, cc)
			}
		}
		return a.SetCustomChannels(next)
	}
	if au.FilePath != "" {
		_ = os.Remove(au.FilePath)
	}
	rt.Pool.Remove(uid)
	log.Printf("已删除账号 %s", shortUID(uid))
	return nil
}

// DisableAccount 停用/启用账号。
// oczen / kilo 的虚拟账号不可停用：停用后无法从面板恢复，而这两条渠道各自只有一个
// 虚拟账号，停用等于禁用整条渠道。
func (a *App) DisableAccount(uid string, disabled bool) error {
	rt, _ := a.findRuntimeAuth(uid)
	if rt == nil || rt.Pool == nil {
		return fmt.Errorf("unknown account %s", uid)
	}
	if rt.Kind == provider.Oczen {
		return fmt.Errorf("OpenCodeZen 匿名通道账号不可停用/启用")
	}
	if rt.Kind == provider.Kilo {
		return fmt.Errorf("Kilo 免费池账号不可停用/启用")
	}
	// R83 起自定义渠道没有 enabled 概念（要么在列表里=启用，要么被删除），
	// 故不再走「停用即写配置」的特例；落到下面常规的 pool.SetDisabled。
	// 效果差异：面板重启后自定义渠道的停用状态不再持久化 —— 这正是删除该功能的本意。
	rt.Pool.SetDisabled(uid, disabled)
	if disabled {
		log.Printf("已停用账号 %s", shortUID(uid))
	} else {
		log.Printf("已启用账号 %s", shortUID(uid))
	}
	return nil
}

// ResourceDetail 查询单个账号积分明细（含可用/不可用/临期小计）。
// 返回的 remain 与 items 同时给出：remain 供 pool 口径对账，items 供 UI 明细展示。
// 取数经 creditSnapshot：实时拉取的同时把同一响应回写 pool 快照，故面板展开明细后
// 卡片顶层与弹层同源一致，不再依赖刷新按钮弥合时差；401 自愈亦在 creditSnapshot 内。
func (a *App) ResourceDetail(uid string) (remain int64, items []provider.ResourceItem, expiring, unusable int64, err error) {
	rt, au := a.findRuntimeAuth(uid)
	if rt == nil || au == nil || rt.Upstream == nil {
		return 0, nil, 0, 0, fmt.Errorf("unknown account %s", uid)
	}
	if rt.Kind == provider.Oczen {
		return 0, nil, 0, 0, fmt.Errorf("OpenCodeZen 匿名通道无积分，不适用")
	}
	if rt.Kind == provider.Kilo {
		return 0, nil, 0, 0, fmt.Errorf("Kilo 免费池无积分，不适用")
	}
	if rt.Custom {
		// 自定义渠道（R66）无明细接口：UserResourceDetail 恒返回空 items，
		// 不拒绝会让前端弹出一张空白明细层。
		return 0, nil, 0, 0, fmt.Errorf("%s 为自定义渠道，无积分明细，不适用", rt.Kind)
	}
	return a.creditSnapshot(rt, au)
}

// ---------------------------------------------------------------------------
// 配置操作
// ---------------------------------------------------------------------------

// SetBodyLog 开关请求体构成摘要记录（诊断用：定位上下文为何膨胀）。
func (a *App) SetBodyLog(on bool) error {
	a.mu.Lock()
	a.cfg.BodyLog = on
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	ledger.SetBodyTrace(on)
	if on {
		log.Printf("请求体摘要记录已开启（写 data/ledger/body-trace-*.jsonl）")
	} else {
		log.Printf("请求体摘要记录已关闭")
	}
	return nil
}

// SetCheckinTimes 更新自动签到时间（HH:MM）并立即唤醒两个调度器。
func (a *App) SetCheckinTimes(times []string) error {
	minutes, err := config.ParseClockTimes(times)
	if err != nil {
		return err
	}
	clean := normalizeMinutes(minutes)
	if len(clean) == 0 {
		return errors.New("请至少保留一个签到时间")
	}
	formatted := make([]string, 0, len(clean))
	for _, m := range clean {
		formatted = append(formatted, fmt.Sprintf("%02d:%02d", m/60, m%60))
	}
	a.mu.Lock()
	a.cfg.Schedule.CheckinTimes = formatted
	a.cfg.Schedule.CheckinHours = nil
	err = config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	for _, rt := range a.runtimesSnapshot() {
		if rt != nil && rt.Scheduler != nil {
			rt.Scheduler.SetCheckinMinutes(clean)
		}
	}
	log.Printf("自动签到时间已更新：%s", strings.Join(formatted, "、"))
	return nil
}

// SetLoopGuard 更新思考链死循环检测（发癫拦截）开关：保存配置并即时生效。
// 生效机制：loopguard 的全局 atomic 开关，各渠道 Stream 在每次请求时读取，
// 热切换无需重启（下一个流式请求即按新值）。
func (a *App) SetLoopGuard(on bool) error {
	a.mu.Lock()
	a.cfg.Features.LoopGuard = on
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	loopguard.SetEnabled(on)
	log.Printf("思考链死循环检测已%s", map[bool]string{true: "开启", false: "关闭"}[on])
	return nil
}

// SetListen 修改 API 监听主机 + 端口：保存配置并热切换监听。
func (a *App) SetListen(host string, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("端口无效：%d", port)
	}
	next := config.Listen{Host: host, Port: port}

	a.mu.Lock()
	defer a.mu.Unlock()
	// 对外暴露（非环回地址）必须同时设置管理密码：否则局域网任何设备
	// 都可无凭据访问面板（含登录入口、退出按钮）。与 SetAdminPassword 互成先决。
	if requireRemoteAuth(next) && strings.TrimSpace(a.cfg.AdminPass) == "" {
		return errors.New("监听非 127.0.0.1 时必须先设置「管理密码」（设置面板中一并保存）")
	}
	if err := a.serveLocked(next.Addr()); err != nil {
		return fmt.Errorf("监听 %s 失败（可能被占用）：%v", next.Addr(), err)
	}
	a.cfg.Listen = next
	if err := config.Save(a.cfg, a.cfgPath); err != nil {
		log.Printf("save config after listen change: %v", err)
	}
	log.Printf("API 监听已切换至 %s", next.Addr())
	return nil
}

// SetAdminPassword 修改管理面板密码（空串 = 关闭面板鉴权）。
// 当前监听为对外地址时不允许清空（见 SetListen）；改密码后所有旧会话立即失效。
func (a *App) SetAdminPassword(pass string) error {
	pass = strings.TrimSpace(pass)
	a.mu.Lock()
	if pass == "" && requireRemoteAuth(a.cfg.Listen) {
		a.mu.Unlock()
		return errors.New("监听非 127.0.0.1 时不允许清空管理密码")
	}
	a.cfg.AdminPass = pass
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	a.auth.dropAll() // 密码变更：旧 cookie 的 label 失配，已自动失效，这里同步清内存
	log.Printf("管理密码已更新：%s", map[bool]string{true: "已启用", false: "已关闭"}[pass != ""])
	return nil
}

// SetAPIKey 修改 API 密钥。
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
	if err := platform.SetAutostart(on); err != nil {
		return err
	}
	if on {
		log.Printf("已开启开机自启")
	} else {
		log.Printf("已关闭开机自启")
	}
	return nil
}

// AutostartEnabled 当前是否已开机自启。
func (a *App) AutostartEnabled() bool { return platform.AutostartEnabled() }

// ServerRunning 是否正在监听。
func (a *App) ServerRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.httpSrv != nil
}

// SetCompat 保存模型名路由配置（compat 段）并写回 config.json。
func (a *App) SetCompat(defaultChannel string, maxTokensCap int, modelMap map[string]string) error {
	if modelMap == nil {
		modelMap = map[string]string{}
	}
	for _, v := range modelMap {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("model_map 的值不能为空（应为 channel/model）")
		}
		if strings.Index(v, "/") <= 0 {
			return fmt.Errorf("model_map 值 %q 需为 channel/model 形式", v)
		}
	}
	a.mu.Lock()
	a.cfg.Compat.DefaultChannel = defaultChannel
	a.cfg.Compat.MaxTokensCap = maxTokensCap
	a.cfg.Compat.ModelMap = modelMap
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	// 热更新外层兼容层的路由表：不同步的话新映射要重启才生效。
	if a.compatSyncer != nil {
		a.compatSyncer(defaultChannel, maxTokensCap, modelMap)
	}
	log.Printf("模型名路由配置已更新：default_channel=%q max_tokens_cap=%d model_map=%d 条",
		defaultChannel, maxTokensCap, len(modelMap))
	return nil
}

// customRename 一条渠道改名记录（R122）：面板编辑渠道标识时，随整表一起声明
// 「New 这个新标识是从 Old 这个旧标识改过来的」。
//
// 为什么必须显式传：渠道标识**就是** provider.Kind（cmd/wild-work/main.go 里
// `k := provider.Kind(cc.Name)`、state 文件与虚拟账号 UID 全部拼名字），而只对比新旧两张表
// 无法区分「改名」与「删一个 + 新增一个」——不声明就会踩到：脱敏回来的密钥按新名找不回、
// 真密钥被空串覆盖落盘。
type customRename struct {
	New string `json:"new"`
	Old string `json:"old"`
}

// SetCustomChannels 整表替换自定义渠道配置（R66）。
//
// api_key 脱敏回写处理（同 oczen key）：面板回显的是脱敏值。用户没改动过某个渠道的 key 时，
// 提交回来的仍是脱敏串——若原样落盘就把真实密钥换成了废值。故：提交值是脱敏形态
// （isMaskedKey）则视为未修改，从现有配置按渠道名找回真实密钥；改名过的渠道另按旧名找回。
func (a *App) SetCustomChannels(channels []custom.ChannelConfig, renames ...customRename) error {
	_, err := a.setCustomChannels(channels, renames)
	return err
}

// setCustomChannels 同 SetCustomChannels，额外返回**真正生效**的改名条数（面板要如实告知用户
// 引用配置被联动了几处；非法/无效的改名声明会被丢弃，不能拿提交数当成绩）。
func (a *App) setCustomChannels(channels []custom.ChannelConfig, renames []customRename) (int, error) {
	seen := map[string]bool{}
	for i := range channels {
		if isMaskedKey(channels[i].APIKey) { // 脱敏形态 = 未改动，保留现值
			channels[i].APIKey = ""
		}
		if err := channels[i].Validate(); err != nil {
			return 0, err
		}
		if seen[channels[i].Name] {
			return 0, fmt.Errorf("重复的渠道标识 %q", channels[i].Name)
		}
		seen[channels[i].Name] = true
	}
	a.mu.Lock()
	// 找回被提交为脱敏形态的密钥（按渠道名匹配现值）
	prev := map[string]string{}
	prevNames := map[string]bool{}
	for _, cc := range a.cfg.CustomChannels {
		prev[strings.ToLower(cc.Name)] = cc.APIKey
		prevNames[strings.ToLower(cc.Name)] = true
	}
	// 改名映射（新→旧）：密钥找回与后续级联都靠它；非法项直接丢（旧名不存在或新旧相同不算改名）
	ren := map[string]string{}
	for _, r := range renames {
		nw, od := strings.ToLower(strings.TrimSpace(r.New)), strings.ToLower(strings.TrimSpace(r.Old))
		if nw == "" || od == "" || nw == od || !prevNames[od] || prevNames[nw] {
			continue
		}
		ren[nw] = od
	}
	for i := range channels {
		if channels[i].APIKey != "" {
			continue
		}
		k := prev[channels[i].Name]
		if k == "" { // 改名后按新名查不到 ⇒ 必须回退查旧名，否则真密钥被空串覆盖并落盘
			if od, ok := ren[channels[i].Name]; ok {
				k = prev[od]
			}
		}
		if k != "" {
			channels[i].APIKey = k
		}
	}
	compatTouched := 0
	if len(ren) > 0 {
		compatTouched = renameCustomRefs(a.cfg, ren)
	}
	a.cfg.CustomChannels = channels
	err := config.Save(a.cfg, a.cfgPath)
	syncer := a.customSyncer
	fbSyncer, efSyncer := a.fbSyncer, a.efSyncer
	fbRules, efforts := a.cfg.ModelFallback, a.cfg.ReasoningEfforts
	a.mu.Unlock()
	if err != nil {
		return 0, fmt.Errorf("保存配置失败：%w", err)
	}
	// 改名连带改写了降级链/思考强度映射，而这两张表在 server.Handler 里是**另一份内存副本**：
	// 只改 cfg 不推 syncer，热路径会继续拿旧 key 查（要到面板重存一次才能修好）。
	if len(ren) > 0 {
		if fbSyncer != nil {
			fbSyncer(fbRules)
		}
		if efSyncer != nil {
			efSyncer(efforts)
		}
	}
	for nw, od := range ren {
		renameCustomState(a.stateDir(), od, nw) // 旧 state 文件留 .old，不硬删
		log.Printf("自定义渠道标识已改名：%s → %s（引用配置联动 %d 处）", od, nw, compatTouched)
	}
	if syncer != nil {
		syncer(channels)
	}
	log.Printf("自定义渠道配置已更新：共 %d 个渠道", len(channels))
	return len(ren), nil
}

// renameCustomRefs 把以 `旧标识/` 为键或值前缀的引用配置改写成新标识（R122）：
// compat.model_map / compat.default_channel / model_fallback / reasoning_efforts。
//
// 为什么必须联动：这几类配置**只校形状不校渠道存在性**，改名后旧前缀不会报错、只会静默
// 失效（用户看到「配置还在、就是不起作用」）。返回改写条数，供面板如实告知。持 a.mu 调用。
func renameCustomRefs(cfg *config.Config, ren map[string]string) int {
	// ren 的方向是「新→旧」（密钥找回靠它），而引用配置里出现的是**旧名** ⇒ 必须先倒转。
	// 同一个旧名被声明改成两个新名是脏数据（无法判断哪个是真），跳过它，不猜。
	cnt := map[string]int{}
	for _, od := range ren {
		cnt[od]++
	}
	to := make(map[string]string, len(ren))
	for nw, od := range ren {
		if cnt[od] == 1 {
			to[od] = nw
		}
	}
	swap := func(s string) string {
		if i := strings.Index(s, "/"); i > 0 {
			if nw, ok := to[strings.ToLower(s[:i])]; ok {
				return nw + s[i:]
			}
		}
		return s
	}
	n := 0
	for k, v := range cfg.Compat.ModelMap {
		if nv := swap(v); nv != v {
			cfg.Compat.ModelMap[k] = nv
			n++
		}
	}
	if od := strings.ToLower(strings.TrimSpace(cfg.Compat.DefaultChannel)); od != "" {
		if nw, ok := to[od]; ok {
			cfg.Compat.DefaultChannel = nw
			n++
		}
	}
	for i := range cfg.ModelFallback {
		if v := swap(cfg.ModelFallback[i].From); v != cfg.ModelFallback[i].From {
			cfg.ModelFallback[i].From, n = v, n+1
		}
		for j := range cfg.ModelFallback[i].To {
			if v := swap(cfg.ModelFallback[i].To[j]); v != cfg.ModelFallback[i].To[j] {
				cfg.ModelFallback[i].To[j], n = v, n+1
			}
		}
	}
	for k, v := range cfg.ReasoningEfforts {
		if nk := swap(k); nk != k {
			delete(cfg.ReasoningEfforts, k)
			cfg.ReasoningEfforts[nk] = v
			n++
		}
	}
	return n
}

// renameCustomState 把 state-custom-<旧>.json 改名留档（照 migrateStateFiles 的先例：只 rename
// 不删除，便于回滚）。新标识的池子从零开始，不搬旧快照——旧文件里的 UID 是 `custom-<旧>`，
// 原样搬到新文件会把一个对不上号的账号条目弄进新池子里。
func renameCustomState(dir, oldName, newName string) {
	old := filepath.Join(dir, "state-custom-"+oldName+".json")
	if !fileExists(old) {
		return
	}
	if err := os.Rename(old, old+".old"); err != nil {
		log.Printf("迁移自定义渠道状态文件失败（%s）：%v", oldName, err)
	}
}

// resolveCustomKey 按渠道名取真实 api_key（供脱敏回显时的测试与保存路径使用）。
// 名称为空（面板新增渠道时只填了 URL/Key）则返回空串，交由调用方自行处理。
func (a *App) resolveCustomKey(name string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, cc := range a.cfg.CustomChannels {
		if strings.EqualFold(cc.Name, name) {
			return cc.APIKey
		}
	}
	return ""
}

// customChannelFeesMeta 返回渠道名 -> [接入点, 脱敏 key]，只给费率页用（R119）。
// 这里给的必须是 maskKey 后的脱敏串：费率页会被面板反复拉，批量发明文等于
// 把所有第三方 key 长期暴露在浏览器内存/抓包里。
func (a *App) customChannelFeesMeta() map[string][2]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string][2]string, len(a.cfg.CustomChannels))
	for _, cc := range a.cfg.CustomChannels {
		out[cc.Name] = [2]string{cc.BaseURL, maskKey(cc.APIKey)}
	}
	return out
}

// maskCustomChannels 返回配置的深拷贝，并将 api_key 脱敏（长头 + **** + 短尾，见 maskKey）。
// 脱敏串里的 **** 就是「未改动」标记，供 SetCustomChannels / 测试端点识别。
func maskCustomChannels(in []custom.ChannelConfig) []custom.ChannelConfig {
	out := make([]custom.ChannelConfig, 0, len(in))
	for _, cc := range in {
		if cc.APIKey != "" {
			cc.APIKey = maskKey(cc.APIKey)
		}
		if cc.Models != nil {
			cc.Models = append([]custom.ModelConfig(nil), cc.Models...)
		}
		out = append(out, cc)
	}
	return out
}

// SetCompatSyncer 注入 compat 热更新回调（由 main.go 注入，指向 gateway.SetCompat）。
func (a *App) SetCompatSyncer(fn func(defaultChannel string, maxTokensCap int, modelMap map[string]string)) {
	a.compatSyncer = fn
}

// SetModelFallbackSyncer 注入模型降级链热更新回调（由 main.go 注入：推给 server.Handler）。
func (a *App) SetModelFallbackSyncer(fn func(rules []config.ModelFallbackRule)) { a.fbSyncer = fn }

// SetModelFallback 保存模型降级链（设置面板可视化编辑）。
//
// 校验（后端是**唯一真相**，前端同一套校验只为即时反馈）：
//   - From/To 均须 "channel/model" 全路径；From 全局唯一；每链 To 至少一项、不含 From 自身
//   - Name（别名，R129）可选，空串 = 无别名；非空时按 validateAliases 的硬拒矩阵逐条拦下
//
// 返回的 warnings 是**软警告**（允许保存）：目前只有「别名与当前模型列表的某个裸名重合」。
// 空规则列表 = 清空禁用。保存即热生效（fbSyncer 推内层 handler 与兼容层，R129）。
func (a *App) SetModelFallback(rules []config.ModelFallbackRule) ([]string, error) {
	// 别名索引先建：下面校验 To 时要靠它识别「备胎里写了别名」。
	// 形态类硬拒（含 / 、空白/逗号/*、重名）仍由 validateAliases 统一点名，
	// 这里只取一份「合法别名 → 规则序号」用于 To 检查（脏名不进索引，与运行时同源）。
	aliasIdx := map[string]int{}
	for i, r := range rules {
		name := strings.TrimSpace(r.Name)
		if name == "" || strings.Contains(name, "/") || !config.ValidAliasName(name) {
			continue
		}
		if _, dup := aliasIdx[name]; !dup {
			aliasIdx[name] = i
		}
	}
	seen := map[string]bool{}
	for i := range rules {
		r := &rules[i]
		r.From = strings.TrimSpace(r.From)
		r.Name = strings.TrimSpace(r.Name)
		if strings.Index(r.From, "/") <= 0 {
			return nil, fmt.Errorf("规则 %d 的主模型 %q 需为 channel/model 形式", i+1, r.From)
		}
		if seen[r.From] {
			return nil, fmt.Errorf("主模型 %q 重复（每个主模型只允许一条规则）", r.From)
		}
		seen[r.From] = true
		tos := make([]string, 0, len(r.To))
		for _, to := range r.To {
			to = strings.TrimSpace(to)
			if to == "" {
				continue
			}
			// ⑥ 备胎链不得写别名（必须在「需为 channel/model 形式」之前判，否则
			// 用户只会看到一条不知道自己做错了什么的形态错误）。
			if j, isAlias := aliasIdx[to]; isAlias {
				return nil, fmt.Errorf("规则 %d 的备胎 %q 是规则 %d 的别名（备胎链只能写 channel/model 全路径，别名不参与备胎链）", i+1, to, j+1)
			}
			if strings.Index(to, "/") <= 0 {
				return nil, fmt.Errorf("主模型 %q 的备胎 %q 需为 channel/model 形式", r.From, to)
			}
			if to == r.From {
				return nil, fmt.Errorf("主模型 %q 的备胎不能是它自己", r.From)
			}
			tos = append(tos, to)
		}
		if len(tos) == 0 {
			return nil, fmt.Errorf("主模型 %q 的降级链为空（至少填一个备胎模型）", r.From)
		}
		r.To = tos
	}
	// ⑥ 的兜底（防御）：主模型也不得写别名——别名指向别名会让「一次请求最多
	// 降级一次 + 不自环」失去可静态证明性。
	for i, r := range rules {
		if _, isAlias := aliasIdx[r.From]; isAlias {
			return nil, fmt.Errorf("规则 %d 的主模型 %q 本身是别名（主力模型必须写 channel/model 全路径，别名指向别名不合法）", i+1, r.From)
		}
	}
	warns, err := a.validateAliases(rules)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.cfg.ModelFallback = rules
	err = config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if a.fbSyncer != nil {
		a.fbSyncer(rules)
	}
	log.Printf("模型降级链已更新（%d 条规则，保存即热生效）", len(rules))
	return warns, nil
}

// validateAliases 校验降级链别名（R129）。硬拒返回错误（文案点名哪条规则的哪个名字）；
// 软警告返回字符串列表（**不阻止保存**，只告知歧义）。
//
// 六条硬拒：① 含 `/`；② 含空白/逗号/`*`；③ 与其他规则的 Name 重名；
// ④ 与 compat.model_map 的任何 key 重名（含以 `*` 结尾的通配 key）；
// ⑤ 与任一渠道名重名；⑥ To 里出现别名（备胎链不得写别名）。
func (a *App) validateAliases(rules []config.ModelFallbackRule) ([]string, error) {
	// 先建索引：重名判定与 To 校验都靠它（派生口径与 config.AliasIndex 一致，脏名跳过）
	idx := map[string]int{}
	for i, r := range rules {
		name := strings.TrimSpace(r.Name)
		if name == "" {
			continue
		}
		if strings.Contains(name, "/") {
			return nil, fmt.Errorf("规则 %d 的别名 %q 不能含 /（别名是裸名，不是 channel/model 形式）", i+1, name)
		}
		if !config.ValidAliasName(name) {
			return nil, fmt.Errorf("规则 %d 的别名 %q 不能含空白、逗号或 *", i+1, name)
		}
		if j, dup := idx[name]; dup {
			return nil, fmt.Errorf("规则 %d 的别名 %q 与规则 %d 重名（别名不可重复，且大小写敏感：Fast ≠ fast）", i+1, name, j+1)
		}
		idx[name] = i
	}
	if len(idx) == 0 {
		return nil, nil
	}

	a.mu.Lock()
	modelMap := map[string]string{}
	for k, v := range a.cfg.Compat.ModelMap {
		modelMap[k] = v
	}
	a.mu.Unlock()
	channels := map[string]bool{}
	for _, rt := range a.runtimesSnapshot() {
		if rt != nil {
			channels[rt.Kind.String()] = true
		}
	}
	// 保留名（含上游命名空间如 cline-free）也算渠道名：它们参与前缀解析
	for k := range provider.BuiltinKinds() {
		channels[k] = true
	}

	for name, i := range idx {
		if _, hit := modelMap[name]; hit {
			return nil, fmt.Errorf("规则 %d 的别名 %q 与 compat.model_map 的键重名（别名优先，同名会造出「有时走映射、有时走别名」的幽灵行为）", i+1, name)
		}
		if channels[name] {
			return nil, fmt.Errorf("规则 %d 的别名 %q 与渠道名重名（会搅乱 channel/model 前缀解析）", i+1, name)
		}
	}
	// ⑥ 的兜底（防御）：To 里写别名已在上面（形态校验之前）拦下，这里只保留模型映射/渠道名冲突的判定。
	return a.aliasBareNameWarnings(idx), nil
}

// aliasBareNameWarnings 别名与当前模型列表里某个**裸名**重合 → 软警告（允许保存）。
// 运行时按别名走（别名优先于 model_map / default_channel），用户需要知情。
// 模型列表因上游不可达而暂时为空时静默跳过，绝不阻塞保存。
func (a *App) aliasBareNameWarnings(idx map[string]int) []string {
	if a.handler == nil {
		return nil
	}
	byKind := a.handler.CachedChannelModels()
	if len(byKind) == 0 {
		return nil
	}
	bare := map[string]bool{}
	for _, infos := range byKind {
		for _, mi := range infos {
			id := strings.TrimSpace(mi.ID)
			if id == "" {
				continue
			}
			bare[id] = true
			if i := strings.Index(id, "/"); i > 0 { // 命名空间形态（cline-free/x）也提供裸名视角
				bare[id[i+1:]] = true
			}
		}
	}
	var out []string
	for name := range idx {
		if bare[name] {
			out = append(out, fmt.Sprintf("别名 %q 与某个真实模型的裸名重合：运行时按别名走（别名优先于 model_map / default_channel）", name))
		}
	}
	sort.Strings(out) // 稳定输出（面板逐条提示，map 遍历序不确定）
	return out
}

// SetReasoningEffortsSyncer 注入思考强度映射热更新回调（由 main.go 注入：推给 server.Handler）。
func (a *App) SetReasoningEffortsSyncer(fn func(efforts map[string]string)) { a.efSyncer = fn }

// SetReasoningEfforts 保存思考强度注入映射（设置面板可视化编辑）。
// 校验：键（模型 ID，含 `*` 全局默认）与值（档位）trim 后均非空；删除 = 键缺席；
// 空 map = 清空禁用。哑管道：不校验档位取值合法性，上游吃什么档位由渠道模型自己说了算。
// 保存即热生效（efSyncer）。
func (a *App) SetReasoningEfforts(efforts map[string]string) error {
	clean := make(map[string]string, len(efforts))
	for k, v := range efforts {
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" {
			return fmt.Errorf("模型 ID 不能为空（`*` 为合法的全局默认键）")
		}
		if v == "" {
			return fmt.Errorf("模型 %q 的思考档位不能为空", k)
		}
		clean[k] = v
	}
	a.mu.Lock()
	a.cfg.ReasoningEfforts = clean
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if a.efSyncer != nil {
		a.efSyncer(clean)
	}
	log.Printf("思考强度映射已更新（%d 条，保存即热生效）", len(clean))
	return nil
}

// SetOczenSyncer 注入 oczen key 热更新回调（由 main.go 注入：写入渠道 client）。
func (a *App) SetOczenSyncer(fn func(key string)) { a.oczenSyncer = fn }

// TestOczenKey 验证 OpenCodeZen 凭证可用性（设置面板「测试」按钮）。
// apiKey 非空时先用该 key 临时写入渠道 client（测试后恢复原值），空则直接测当前生效凭证。
// 判定：200/429 均 return ok（200=配额正常，429=连通但限流，对匿名 key 属正常现象）。
//
// 测试后经同一个 backuplink.Replay 编排（与真实对话同源，见 issue #117）：直连被上游按地区
// 拒绝时机内自动改走备用通道；viaBackup 报告答复是否来自备用出口，面板据此如实展示——
// 原先只看 403 是看不出「代理到底有没有被用上」的。
func (a *App) TestOczenKey(apiKey string) (int, string, bool, error) {
	rt := a.runtime(provider.Oczen)
	if rt == nil {
		return 0, "", false, fmt.Errorf("oczen 渠道未启用")
	}
	c, ok := rt.Upstream.(*oczen.Client)
	if !ok {
		return 0, "", false, fmt.Errorf("oczen 渠道类型异常")
	}
	// 临时写入候选 key（含空串 = 测匿名）；结束后恢复配置值，保证面板「取消」不残留半生效状态
	orig := a.cfg.OczenAPIKey
	c.SetAPIKey(apiKey)
	defer c.SetAPIKey(orig)
	status, body, via, err := c.TestKey()
	if err != nil {
		return 0, body, via, fmt.Errorf("无法连接 OpenCodeZen（检查网络/代理）：%v", err)
	}
	return status, body, via, nil
}

// maskKey 脱敏形态：长头 + **** + 短尾（R121）。头取 12 个字符、尾取 4 个，但可见总量
// 不得超过原文一半（短 key 自动收缩头部，收到 4 字符以下就直接全遮）。
// 头部留长是硬需求：第三方 key 的**前缀**才是辨识度来源（百度千帆 bce-v3/ALTAK…、
// OpenAI sk-proj-…），只留 5 个字符根本看不出是哪家，不能假定密钥都形如 sk-…。
// 中间固定四个星（不泄漏长度），四星同时充当「面板未改动」标记（见 isMaskedKey）。
func maskKey(key string) string {
	r := []rune(strings.TrimSpace(key))
	if len(r) == 0 {
		return ""
	}
	if len(r) <= 12 {
		return maskedMark // 短到「露头 + 尾」就等于露大半原文，直接全遮
	}
	head, tail := 12, 4
	if head+tail > len(r)/2 {
		head = len(r)/2 - tail // 可见字符总量没有上限一半，短 key 自动收缩头部
	}
	if head < 4 {
		return maskedMark
	}
	return string(r[:head]) + maskedMark + string(r[len(r)-tail:])
}

// maskedMark 是脱敏串的中间标记，同时被当作「面板未改动过此字段」的记号（isMaskedKey）。
const maskedMark = "****"

// isMaskedKey 认一个字符串是不是脱敏形态（而不是用户真填的密钥）。
// 三种都认：新格式 ****、旧格式 …、更早的前端 … 占位——升级瞬间浏览器里可能还
// 渲染着旧脱敏串，当成真值落盘会把可用密钥洗成废值（这个判定同时活在 Go 与 JS 两侧）。
func isMaskedKey(s string) bool {
	return strings.Contains(s, maskedMark) || strings.Contains(s, "…") || strings.Contains(s, "••")
}

// SetBackupProxy 保存全局**备用上游通道**（单一代理地址，空串 = 关闭）并即时生效。
// 同时接收 oczenKey：设置 OpenCodeZen 自定义 API key（空 = 回匿名凭证）。
//
// 生效方式与旧的「每渠道代理」根本不同：此处**不重建任何渠道 Transport**，
// 只换 backuplink 内部的快照指针（决议 R57）——备用通道不参与常规请求，
// 渠道在命中地区拒绝时自己按需取用，因此没有「热更新接线表」可漏（旧实现
// 启动表 13 个渠道、热更新表只 10 个，traecode 两个表都没进）。
func (a *App) SetBackupProxy(raw, oczenKey string) error {
	raw = strings.TrimSpace(raw)
	if _, err := backuplink.Parse(raw); err != nil { // 空串合法（= 关闭）
		return err
	}
	a.mu.Lock()
	a.cfg.BackupProxy = raw
	if oczenKey != "\u0000unset\u0000" { // 哨兵：请求未携带该字段时不改动现有 key
		a.cfg.OczenAPIKey = strings.TrimSpace(oczenKey)
	}
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if err := backuplink.Set(raw); err != nil { // 已校验过，这里不应再失败
		return err
	}
	if a.oczenSyncer != nil {
		a.oczenSyncer(a.cfg.OczenAPIKey)
	}
	if raw == "" {
		log.Printf("备用上游通道已关闭（所有请求直连；上游回地区拒绝将原样透传）")
	} else {
		log.Printf("备用上游通道已配置：%s（仅渠道命中地区拒绝时自动取用，不影响常规请求）", raw)
	}
	return nil
}

// SetAccountNickname 修改账号显示名：写回 auth 文件 + 池内即时生效。
func (a *App) SetAccountNickname(uid, nickname string) error {
	nickname = strings.TrimSpace(nickname)
	if nickname == "" {
		return fmt.Errorf("显示名不能为空")
	}
	if len([]rune(nickname)) > 60 {
		return fmt.Errorf("显示名过长（最多 60 字符）")
	}
	rt, au := a.findRuntimeAuth(uid)
	if rt == nil || au == nil {
		return fmt.Errorf("unknown account %s", uid)
	}
	if rt.Kind == provider.Oczen {
		return fmt.Errorf("OpenCodeZen 匿名通道账号不可改名")
	}
	if rt.Kind == provider.Kilo {
		return fmt.Errorf("Kilo 免费池账号不可改名")
	}
	if au.FilePath == "" {
		return fmt.Errorf("账号 %s 无凭证文件，不可改名", uid)
	}
	a.mu.Lock()
	au.Nickname = nickname // 池内 AuthByUID 返回同一指针，改字段即生效
	err := au.SaveAtomic()
	a.mu.Unlock()
	if err != nil {
		return fmt.Errorf("保存显示名失败：%v", err)
	}
	log.Printf("账号显示名已更新 uid=%s nickname=%s", shortUID(uid), nickname)
	return nil
}

// LoginBusy 是否登录中。
func (a *App) LoginBusy() bool {
	a.muLogin.Lock()
	defer a.muLogin.Unlock()
	return a.loginBusy
}

// ---------------------------------------------------------------------------
// 面板数据（Web UI）
// ---------------------------------------------------------------------------

// AccountView 面板展示的账号（脱敏）。
type AccountView struct {
	UID      string `json:"uid"`
	Group    string `json:"group"` // workbuddy | workbuddyai | traework | qoder
	Nickname string `json:"nickname"`
	// Credits 本工具可消耗的积分余额（pool 路由依据）。
	Credits int64 `json:"credits"`
	// ExpiringCredits 可消耗余额中 24h 内（到期日≤明天）到期的部分，仅面板展示。
	ExpiringCredits int64 `json:"expiring_credits,omitempty"`
	// UnusableCredits 账号名下有、但本工具用不了的积分（如 TraeWork ep=1 专用池），
	// 仅面板展示；0 表示该渠道不区分或没有此类额度。
	UnusableCredits int64 `json:"unusable_credits"`
	// CreditsStale 余额口径不可信（旧版 state 或尚未完成首次成功刷新），UI 显示「待刷新」。
	CreditsStale bool `json:"credits_stale,omitempty"`
	// CreditsNA 积分概念不适用（如 OpenCodeZen 匿名通道），UI 显示「不适用」
	// 而非 0，并隐藏刷新积分与明细入口。
	CreditsNA bool `json:"credits_na,omitempty"`
	// CreditsUnit 该账号额度的计量单位：空 = 积分（credits），`"token"` = Token 计数。
	// 前端据此把它渲染成「Token 计数」面板（文案/单位/合计口径都随之切换），
	// 且**不并入积分合计**——即「与积分分开统计和显示」（用户 2026-10-17 的要求）。
	CreditsUnit string `json:"credits_unit,omitempty"`
	Cooling     bool   `json:"cooling"`
	Until       string `json:"until"`
	// UntilTS 解冻时刻的 unix 秒（0 = 无）。面板用它实时算「剩 X 时 X 分」——
	// until 文案是 "01-02 15:04"（24h 制），跨零点的次日 07:07 极易被误读成
	// 「19 点被显示成 7 点」；带相对时长后（issue：24h 制统一）歧义即消除。
	UntilTS  int64  `json:"until_ts,omitempty"`
	Reason   string `json:"reason"`
	Disabled bool   `json:"disabled"`
	// ModelCooling 模型级冷却（model → 解冻时刻）；仅该账号该模型限流，账号整体仍可用
	// （429/6004 上游按模型计）。面板据此区分「整号冷却」与「个别模型限流」。
	ModelCooling   map[string]string `json:"model_cooling,omitempty"`
	ErrCount       int               `json:"err_count"`
	LastCheckinOK  bool              `json:"last_checkin_ok"`
	LastCheckinAt  string            `json:"last_checkin_at"`
	LastCheckinMsg string            `json:"last_checkin_msg"`
	// SessionDead 登录态失效（401/登录已失效）：面板显示「需重新登录」+ 重新登录入口（#134）。
	SessionDead       bool   `json:"session_dead,omitempty"`
	SessionDeadReason string `json:"session_dead_reason,omitempty"`
	// RefreshTokenMissing 凭据缺 refresh_token —— 目前只有 OfficeAce 会这样：
	// 客户端导入只拿得到「不过期的网关 Basic」，而积分/签到要用的临时凭据（约 2h）
	// 必须靠 refresh_token 换 ⇒ 缺它时积分/签到**永久不可用**。它与 SessionDead 不同：
	// 不是「被上游拒了」，而是本地根本没这条凭据，故不会自愈，只能重新登录/重新导入补齐。
	// 面板据此显示**持久**徽标（原先只在导入那一刻弹一次提示文字，事后无从复查）。
	RefreshTokenMissing       bool   `json:"refresh_token_missing,omitempty"`
	RefreshTokenMissingReason string `json:"refresh_token_missing_reason,omitempty"`
}

// State Web UI 初始数据。
type State struct {
	Accounts       []AccountView `json:"accounts"`
	CheckinTimes   []string      `json:"checkin_times"`
	KeepaliveHours []int         `json:"keepalive_hours"`
	ListenHost     string        `json:"listen_host"`
	ListenPort     int           `json:"listen_port"`
	APIKey         string        `json:"api_key"`

	// AdminPassSet 是否已配置管理密码（不回显密码本身）；
	// AuthEnabled = 面板鉴权已生效（密码非空）；
	// AuthSession = 当前浏览器 cookie 对应的口令指纹（已登录时非空且等于服务端口令指纹）。
	// 前端靠 AuthSession 判断登录态：HttpOnly cookie 不可读，但指纹可回显。
	AdminPassSet bool   `json:"admin_pass_set"`
	AuthEnabled  bool   `json:"auth_enabled"`
	AuthSession  string `json:"auth_session"`
	AuthRequired bool   `json:"auth_required"` // 当前监听地址是否必须配置密码（非环回）

	LoginBusy   bool   `json:"login_busy"`
	LoginError  string `json:"login_error"` // 上次登录终态错误（非空时面板提示失败而非「登录完成」）
	NextCheckin string `json:"next_checkin"`
	Version     string `json:"version"`
	Autostart   bool   `json:"autostart"`
	Running     bool   `json:"running"`

	// LanIP 非环回出口 IP（如 192.168.x.x），供面板在监听 0.0.0.0 时提示局域网可用的 API 地址。
	// 取不到（无网络/全部环回）时为空串。
	LanIP string `json:"lan_ip"`

	// Compat 模型名路由配置（只读，保存走 POST /api/config/compat）
	Compat struct {
		DefaultChannel string            `json:"default_channel"`
		MaxTokensCap   int               `json:"max_tokens_cap"`
		ModelMap       map[string]string `json:"model_map"`
		Channels       []string          `json:"channels"` // 可用渠道列表（供 UI 下拉）
	} `json:"compat"`

	// BackupProxy 全局备用上游通道（只读，保存走 POST /api/config/proxy）。
	// 注意：它**不参与常规请求**，仅当渠道自行判定上游按地区拒绝时在同请求内重放一次（R57）。
	BackupProxy string `json:"backup_proxy"`

	// BackupProxyActive 备用通道是否已生效（与 BackupProxy 非空等价，分开回显
	// 是为了让面板能区分「地址合法但进程内未生效」这种理论上不应存在的状态）。
	BackupProxyActive bool `json:"backup_proxy_active"`

	// OczenAPIKey OpenCodeZen 自定义 API key（只读脱敏回显：sk-xxx…尾4位；保存走同端点）。
	OczenAPIKey string `json:"oczen_api_key"`

	// BodyLog 请求体摘要诊断是否开启（只读，保存走 POST /api/config/body_log）。
	BodyLog bool `json:"body_log"`

	// LoopGuard 思考链死循环检测（发癫拦截）是否开启（只读，保存走 POST /api/config/loop_guard）。
	LoopGuard bool `json:"loop_guard"`

	// ModelFallback 模型降级链（保存走 POST /api/config/model_fallback）。
	ModelFallback []config.ModelFallbackRule `json:"model_fallback"`

	// ReasoningEfforts 思考强度注入映射（键=模型 ID 或 `*` 全局默认；保存走 POST /api/config/reasoning_efforts）。
	ReasoningEfforts map[string]string `json:"reasoning_efforts"`

	// CustomChannels 自定义第三方渠道列表（保存走 POST /api/config/custom_channels，R66）。
	CustomChannels []custom.ChannelConfig `json:"custom_channels"`
}

// GetState 返回面板初始数据。
func (a *App) GetState() State {
	a.mu.Lock()
	pass, listen := a.cfg.AdminPass, a.cfg.Listen
	a.mu.Unlock()
	authOn := strings.TrimSpace(pass) != ""
	brief := passBrief(pass)
	session := ""
	if authOn {
		session = brief // 仅当请求通过 authGuard 时才会走到这里（无有效 cookie 已被 401 拦截）
	}
	st := State{
		CheckinTimes:   a.checkinTimes(),
		KeepaliveHours: a.keepaliveHours(),
		ListenHost:     listen.Host,
		ListenPort:     listen.Port,
		APIKey:         a.cfg.APIKey,
		AdminPassSet:   authOn,
		AuthEnabled:    authOn,
		AuthSession:    session,
		AuthRequired:   requireRemoteAuth(listen),
		LoginBusy:      a.LoginBusy(),
		LoginError:     a.peekLoginError(),
		NextCheckin:    fmtTime(a.nextFire()),
		Version:        Version,
		Autostart:      a.AutostartEnabled(),
		Running:        a.ServerRunning(),
		LanIP:          lanIP(),
		LoopGuard:      a.cfg.Features.LoopGuard,
	}
	st.ModelFallback = a.cfg.ModelFallback
	st.ReasoningEfforts = a.cfg.ReasoningEfforts
	if st.ReasoningEfforts == nil {
		st.ReasoningEfforts = map[string]string{}
	}
	st.Compat.DefaultChannel = a.cfg.Compat.DefaultChannel
	st.Compat.MaxTokensCap = a.cfg.Compat.MaxTokensCap
	st.Compat.ModelMap = a.cfg.Compat.ModelMap
	if st.Compat.ModelMap == nil {
		st.Compat.ModelMap = map[string]string{}
	}
	for _, rt := range a.runtimesSnapshot() {
		if rt != nil {
			st.Compat.Channels = append(st.Compat.Channels, rt.Kind.String())
		}
	}
	sort.Strings(st.Compat.Channels)
	st.BackupProxy = a.cfg.BackupProxy
	st.BackupProxyActive = backuplink.Enabled()
	// oczen key 脱敏回显（与自定义渠道同一套 maskKey，面板侧靠 **** 识别未改动）
	st.OczenAPIKey = maskKey(a.cfg.OczenAPIKey)
	st.BodyLog = a.cfg.BodyLog
	// 自定义渠道路由的 api_key 同样脱敏回显（不把上游密钥明文送进面板载荷）。
	// 返回深拷贝而非配置引用，避免调用方意外改写配置。
	st.CustomChannels = maskCustomChannels(a.cfg.CustomChannels)
	st.Accounts = a.accountViews()
	return st
}

func (a *App) accountViews() []AccountView {
	statuses := a.allStatuses()
	out := make([]AccountView, 0, len(statuses))
	for _, s := range statuses {
		group := a.accountGroup(s.UID)
		missingRT, missingRTWhy := a.refreshTokenMissing(s.UID)
		out = append(out, AccountView{
			UID:             s.UID,
			Group:           group,
			Nickname:        s.Nickname,
			Credits:         s.Credits,
			ExpiringCredits: s.ExpiringCredits,
			UnusableCredits: s.UnusableCredits,
			CreditsStale:    s.CreditsStale,
			// 匿名与自定义渠道无积分：前端据此显示「不适用」
			CreditsNA:                 a.creditsNAKind(group),
			CreditsUnit:               a.creditsUnit(group),
			Cooling:                   s.Cooling,
			Until:                     fmtTime(s.Until),
			UntilTS:                   unixOrZero(s.Until),
			Reason:                    s.Reason,
			Disabled:                  s.Disabled,
			ModelCooling:              fmtTimeMap(s.ModelCooling),
			ErrCount:                  s.ErrCount,
			LastCheckinOK:             s.LastCheckinOK,
			LastCheckinAt:             fmtTime(s.LastCheckinAt),
			LastCheckinMsg:            s.LastCheckinMsg,
			SessionDead:               s.SessionDead,
			SessionDeadReason:         s.SessionDeadReason,
			RefreshTokenMissing:       missingRT,
			RefreshTokenMissingReason: missingRTWhy,
		})
	}
	// 面板统一渠道序展示（R142）：按显示名字节序，自定义渠道钉最末。
	sortChannelsByOrder(out, func(v AccountView) provider.Kind { return provider.Kind(v.Group) })
	return out
}

// accountGroup 返回账号所属分组（workbuddy/traework）。
func (a *App) accountGroup(uid string) string {
	_, au := a.findRuntimeAuth(uid)
	if au != nil {
		if au.Kind != "" {
			return au.Kind
		}
		if au.FilePath != "" && strings.HasPrefix(filepath.Base(au.FilePath), "trae-") {
			return "traework"
		}
		if au.FilePath != "" && strings.HasPrefix(filepath.Base(au.FilePath), "qoder") {
			return "qoder"
		}
		if au.FilePath != "" && strings.HasPrefix(filepath.Base(au.FilePath), "workbuddyai-") {
			return "workbuddyai"
		}
	}
	return "workbuddy"
}

// refreshTokenMissing 判定该账号的凭据是否**缺 refresh_token**（面板持久徽标用，改进2）。
//
// 目前只有 OfficeAce 存在这种形态：其凭据分两层——网关 Basic（不过期，用于对话）与
// 临时 AK/SK（约 2h，用于积分/签到）。客户端导入只拿得到前者，后者必须由 refresh_token
// 经 DPoP 换取 ⇒ **没有 refresh_token 就没有积分/签到**，且不会自愈（不像 401 失效那样
// 能靠 refresh+重试恢复），只能「重新登录」或「先在客户端登录该账号再重新导入」补齐。
//
// 判定放在这里（而非各渠道自查）的原因：这是**账号卡片**要展示的状态，与 SessionDead
// 同源同层；只有明确「凭据模型依赖 refresh_token」的渠道才判定，避免给 Cline/oczen
// 这类无 refresh 概念的渠道误挂徽标（它们本来就不写 RefreshToken）。
//
// 返回 (是否缺失, 面板提示文案)。文案带**补救路径**，因为用户看到徽标后最想知道的就是
// 「我该做什么」；两条路径都实测可用（2026-10-17：浏览器登录补齐、客户端重登后重导入补齐）。
//
// 两种「等效于没有 refresh_token」的形态都要报（2026-10-18 独立复核补第二种）：
//   - 字段为空：本地根本没这条凭据；
//   - 字段非空但**已在死名单**：该 token 被上游明确拒过（STS5.1806/1807 "has been used"），
//     `internal/officeace/refresh.go` 会直接短路、`ensureCredential` 不会被调用 ⇒ 续期
//     **必然失败**。只判空会让这种账号在面板上看起来完全正常（复核用探针实证）。
func (a *App) refreshTokenMissing(uid string) (bool, string) {
	_, au := a.findRuntimeAuth(uid)
	if au == nil || au.Kind != string(provider.OfficeAce) {
		return false, ""
	}
	rt := strings.TrimSpace(au.RefreshTokenValue())
	if rt == "" {
		return true, "缺 refresh_token：积分/签到不可用（本工具不会自动恢复）。" +
			"补救一：先在 OfficeAce 客户端登录该账号，再回面板「从客户端导入」；" +
			"补救二：直接在本工具用「浏览器登录」重新添加该账号。"
	}
	// 只读一次 Extra（持锁快照，避免与续期写路径并发读 map）。
	au.RLock()
	dead := au.Extra["dead_tokens"]
	au.RUnlock()
	if csvContains(dead, rt) {
		return true, "refresh_token 已被上游拒绝（本工具已拉黑、不再重放）：积分/签到不可用，需重新登录。" +
			"补救一：先在 OfficeAce 客户端登录该账号，再回面板「从客户端导入」；" +
			"补救二：直接在本工具用「浏览器登录」重新添加该账号。"
	}
	return false, ""
}

// csvContains 判断 CSV 串里是否含某个值（逐项 TrimSpace 后精确比较）。
// 与 officeace 内部 splitCSV/containsCSV 同口径；这里只需判定、无需解析成切片以外的东西。
func csvContains(csv, want string) bool {
	if csv == "" || want == "" {
		return false
	}
	for _, part := range strings.Split(csv, ",") {
		if strings.TrimSpace(part) == want {
			return true
		}
	}
	return false
}

// OpenLogFile 用系统默认编辑器打开日志文件。
func (a *App) OpenLogFile() error {
	fp := filepath.Join(filepath.Dir(a.cfg.StateFile), "app.log")
	if _, err := os.Stat(fp); err != nil {
		_ = os.WriteFile(fp, []byte("(empty)\n"), 0o644)
	}
	abs, err := filepath.Abs(fp)
	if err != nil {
		return err
	}
	return platform.OpenWithTextEditor(abs)
}

// NotifyCheckin 记录自动签到结果（调度器观察器）。
func (a *App) NotifyCheckin(platformName string, r scheduler.CheckinResult) {
	log.Printf("checkin platform=%s uid=%s ok=%t msg=%s remain=%d has_remain=%t",
		platformName, r.UID, r.OK, r.Msg, r.Remain, r.HasRemain)
}

// NotifyRefresh 记录 token 刷新结果（调度器观察器）。
func (a *App) NotifyRefresh(platformName, uid string, ok bool, msg string) {
	log.Printf("refresh platform=%s uid=%s ok=%t msg=%s", platformName, uid, ok, msg)
}

// ---------------------------------------------------------------------------
// 管理 API（HTTP）
// ---------------------------------------------------------------------------

// writeJSON 写 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// apiError 统一错误响应。
func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

// HandleAPI 注册管理 API 路由（挂到 server handler 的 /api/* 上）。
// 开启管理密码（config.admin_password 非空）时，除 /api/auth/* 外全部须先登录；
// 密码为空 = 不鉴权，此时仅允许监听环回地址（见 SetListen / SetAdminPassword）。
func (a *App) HandleAPI(mux *http.ServeMux) {
	// 登录/登出/会话探针：挂在 /api/auth/* 上（比 /api/ 更具体，不经过会话鉴权）。
	mux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if !a.panelAuthEnabled() {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": ""})
			return
		}
		var req struct {
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		token, brief, ok, msg := a.panelLogin(req.Password, clientIP(r))
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": msg})
			return
		}
		setSessionCookie(w, r, token)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session": brief})
	})
	mux.HandleFunc("POST /api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		a.panelLogout(r)
		setSessionCookie(w, r, "") // 置空 + MaxAge<0 删除 cookie
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/auth/state", func(w http.ResponseWriter, r *http.Request) {
		// 会话探针：不返回 401，未登录时只回鉴权元信息（绝不回传 API-Key/账号/代理等
		// 已鉴权内容），供前端在首次加载时就弹出登录层。
		a.mu.Lock()
		pass, listen, version := a.cfg.AdminPass, a.cfg.Listen, Version
		a.mu.Unlock()
		// 未登录时只回鉴权元信息（结构体故意最小化：不回传 API-Key/账号/代理等任何已鉴权内容）
		authOn := strings.TrimSpace(pass) != ""
		probe := struct {
			AuthEnabled  bool   `json:"auth_enabled"`
			AuthSession  string `json:"auth_session"`
			AuthRequired bool   `json:"auth_required"`
			Version      string `json:"version"`
		}{authOn, "", requireRemoteAuth(listen), version}
		if authOn {
			if c, err := r.Cookie(sessionCookie); err == nil && a.auth.valid(c.Value, passBrief(pass)) {
				st := a.GetState() // 已持有有效会话：与 /api/state 等价
				writeJSON(w, http.StatusOK, st)
				return
			}
		}
		writeJSON(w, http.StatusOK, probe)
	})

	// 其余管理 API 统一挂到私有 mux，最后整体套一层会话鉴权中间件：
	// 路由注册代码零改动，也避免逐个 handler 漏包。
	api := http.NewServeMux()
	mux.Handle("/api/", a.guardMux(api))
	mux = api

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.GetState())
	})
	mux.HandleFunc("POST /api/login/start", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Channel string `json:"channel"`
			// AccountLabel 用户指定的「账号标识」（可选）。
			// 只对上游不提供稳定身份字段的渠道有意义（当前是 CodeArts）：填了就是独立账号，
			// 留空则按「同一账号重新登录」处理（覆盖该渠道已有条目）。
			AccountLabel string `json:"account_label"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		a.muLogin.Lock()
		a.loginLabel = strings.TrimSpace(req.AccountLabel)
		a.loginBackendOpened = false
		a.muLogin.Unlock()
		url, err := a.StartLoginFor(req.Channel)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		a.muLogin.Lock()
		opened := a.loginBackendOpened
		kind := a.loginKind
		a.muLogin.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{
			"auth_url":       url,
			"backend_opened": opened,
			// needs_poll（2026-10-18 H9）：告知前端这次登录是否需要轮询结果。
			// GLM 的形态是「只给链接、不跑流程」（凭据靠粘贴 Cookie），后端不置 loginBusy、
			// 不起 pollLogin；改造前前端只看 auth_url 非空就无条件 startLoginPoll()，
			// 于是轮询看到 login_busy=false 便提示「登录完成，正在同步账号…」——假成功。
			"needs_poll": a.needsPollFor(req.Channel),
			// manual_callback：该渠道的授权回调写死回环地址时（当前 CodeArts 与 LobsterAI，
			// 判据见 manualCallbackKind 单一来源，R96③/R97），浏览器与网关不同机
			// （局域网/远程访问面板）就收不到授权码，只能靠用户把地址栏 URL 粘回来，
			// 前端据此显示输入框。
			"manual_callback": manualCallbackKind(kind),
		})
	})
	// 手动提交授权回调地址（回环回调渠道的远程/局域网兜底）。
	//
	// 背景：回环回调五渠道的回调地址都写死 127.0.0.1/localhost——
	// CodeArts 的 portal 按 `port` 参数把浏览器跳到 `http://127.0.0.1:<port>/oauth/callback`，
	// LobsterAI 的 portal 按 `redirect_uri` 原样跳回 `http://127.0.0.1:<port>/auth/callback`，
	// TraeWork 的回调写死 `http://127.0.0.1:<随机端口>/authorize`（且监听只有 5 分钟寿命），
	// QwenWork 的 redirect_uri 白名单制不可改（`/callback`），
	// Gemini 的 installed-app 政策限 redirect host 为 localhost（`/oauth-callback`）。
	// 五者都只有在「浏览器与网关同一台机器」时授权码才自动回到网关。从局域网别处打开面板时，
	// 浏览器跳到的是**用户自己机器**的 127.0.0.1（连接被拒），流程只能等到超时。
	// 这里提供一条人工通道：把跳水后打不开的那条 URL 原样提交，网关解析出 code 后
	// 走与自动回调**完全相同**的兑换路径（凭据仍由网关侧换，用户不需要交出任何 token）。
	mux.HandleFunc("POST /api/login/callback", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			URL string `json:"url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		a.muLogin.Lock()
		busy, kind := a.loginBusy, a.loginKind
		a.muLogin.Unlock()
		if !busy {
			apiError(w, http.StatusBadRequest, "没有进行中的登录流程，请先发起登录")
			return
		}
		// 白名单与 /api/login/start 的 manual_callback 同源（manualCallbackKind），
		// 避免出现「前端显示了输入框、这里却 400」的半接线状态。
		if !manualCallbackKind(kind) {
			apiError(w, http.StatusBadRequest, fmt.Sprintf(
				"%s 渠道不需要手动回调：它的凭据由网关侧轮询/捕获，不接受回调地址", kind))
			return
		}
		switch kind {
		case provider.CodeArts:
			if err := logincodearts.SubmitCallbackURL(req.URL); err != nil {
				apiError(w, http.StatusBadRequest, err.Error())
				return
			}
		case provider.LobsterAI:
			if err := loginlobsterai.SubmitCallbackURL(req.URL); err != nil {
				apiError(w, http.StatusBadRequest, err.Error())
				return
			}
		case provider.TraeWork:
			// 状态在文件（login-state.json）而非内存会话，故把路径传进去；
			// 凭证解析/写入与自动回调同一条 state 通道，Poll 零分叉。
			if err := logintrae.SubmitCallbackURL(a.loginStateFP, req.URL); err != nil {
				apiError(w, http.StatusBadRequest, err.Error())
				return
			}
		case provider.QwenWork:
			// 内存 pending 通道（与自动回调同一条，见 internal/login_qwenwork SubmitCallbackURL）。
			if err := loginqwenwork.SubmitCallbackURL(req.URL); err != nil {
				apiError(w, http.StatusBadRequest, err.Error())
				return
			}
		case provider.Gemini:
			// 内存 session 通道（state 逐字比对，见 internal/login_gemini SubmitCallbackURL）。
			if err := logingemini.SubmitCallbackURL(req.URL); err != nil {
				apiError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		// 提交成功必须回 JSON：PR #163 的合并曾在 multi-parent 合并时把这一行弄丢，
		// handler 变成空 200（前端 api() 容错空 body 故测试全绿，用户却看到「已提交」后
		// 无任何响应）。合并不丢行：成功/失败都要给前端一个可判定的响应体。
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/login/cancel", func(w http.ResponseWriter, r *http.Request) {
		if err := a.CancelLogin(); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	// 导入型渠道（MonkeyCode）：从本机已登录的官方客户端读取凭据并写入 auths/。
	// 导入型渠道（小浣熊）：从本机已登录的官方客户端读取凭据并写入 auths/。
	// 导入型渠道（Loomy）：从本机已登录的官方客户端读取凭据并写入 auths/。
	mux.HandleFunc("POST /api/account/import_local", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Channel string `json:"channel"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		res, err := a.ImportLocalCredentials(req.Channel)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /api/login/glm_auto", func(w http.ResponseWriter, r *http.Request) {
		if err := a.StartGLMAutoLogin(); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/login/glm_auto_status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.GetGLMAutoLoginStatus())
	})
	mux.HandleFunc("POST /api/login/glm_auto_cancel", func(w http.ResponseWriter, r *http.Request) {
		if err := a.CancelGLMAutoLogin(); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/login/officeace_auto", func(w http.ResponseWriter, r *http.Request) {
		url, err := a.StartOfficeAceLogin()
		if err != nil {
			// 已有流程进行中时也回传 URL，便于前端继续打开/轮询
			if url != "" {
				writeJSON(w, http.StatusOK, map[string]any{"auth_url": url, "warning": err.Error()})
				return
			}
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"auth_url": url})
	})
	mux.HandleFunc("GET /api/login/officeace_auto_status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.GetOfficeAceLoginStatus())
	})
	mux.HandleFunc("POST /api/login/officeace_auto_cancel", func(w http.ResponseWriter, r *http.Request) {
		if err := a.CancelOfficeAceLogin(); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/login/glm_token", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RefreshToken string `json:"refresh_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, "请求体解析失败")
			return
		}
		uid, err := a.SubmitGLMToken(req.RefreshToken)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid})
	})
	mux.HandleFunc("POST /api/login/cline_key", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			APIKey string `json:"api_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, "请求体解析失败")
			return
		}
		uid, warn, err := a.SubmitClineKey(req.APIKey)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "warning": warn})
	})

	mux.HandleFunc("POST /api/login/comate_sid", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SID string `json:"sid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, "请求体解析失败")
			return
		}
		uid, warn, err := a.SubmitComateSID(req.SID)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "warning": warn})
	})
	mux.HandleFunc("POST /api/login/lingxi_sid", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SID string `json:"sid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, "请求体解析失败")
			return
		}
		uid, warn, err := a.SubmitLingxiSID(req.SID)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "uid": uid, "warning": warn})
	})
	// Comate 扫码登录（服务端申请二维码，手机确认后自动拿 wps_sid）：三段式。
	mux.HandleFunc("POST /api/login/comate_qr", func(w http.ResponseWriter, r *http.Request) {
		if err := a.StartComateQRLogin(); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, a.GetComateQRLoginStatus())
	})
	mux.HandleFunc("GET /api/login/comate_qr_status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.GetComateQRLoginStatus())
	})
	mux.HandleFunc("GET /api/login/comate_browser_info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.ComateBrowserInfo())
	})
	mux.HandleFunc("POST /api/login/comate_browser", func(w http.ResponseWriter, r *http.Request) {
		if err := a.StartComateBrowserLogin(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, a.GetComateBrowserLoginStatus())
	})
	mux.HandleFunc("GET /api/login/comate_browser_status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.GetComateBrowserLoginStatus())
	})
	mux.HandleFunc("POST /api/login/comate_browser_cancel", func(w http.ResponseWriter, r *http.Request) {
		if err := a.CancelComateBrowserLogin(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/login/comate_qr_cancel", func(w http.ResponseWriter, r *http.Request) {
		if err := a.CancelComateQRLogin(); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.HandleFunc("POST /api/account/checkin", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UID string `json:"uid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		res, err := a.CheckinAccount(req.UID)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("POST /api/account/checkin_all", func(w http.ResponseWriter, r *http.Request) {
		results := a.CheckinAll()
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
	})
	mux.HandleFunc("POST /api/account/refresh", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UID string `json:"uid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		remain, err := a.RefreshCredits(req.UID)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"remain": remain})
	})
	mux.HandleFunc("POST /api/account/refresh_all", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.RefreshAll())
	})
	mux.HandleFunc("POST /api/account/remove", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UID string `json:"uid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := a.RemoveAccount(req.UID); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/account/disable", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UID      string `json:"uid"`
			Disabled bool   `json:"disabled"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := a.DisableAccount(req.UID, req.Disabled); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/account/resource_detail", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UID string `json:"uid"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		remain, items, expiring, _, err := a.ResourceDetail(req.UID)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		// 可用/不可用小计在服务端算好，前端只负责展示，避免两边口径漂移。
		usable, unusableSum := provider.Summarize(items)
		writeJSON(w, http.StatusOK, map[string]any{
			"remain":          remain,
			"items":           items,
			"usable_remain":   usable,
			"unusable_remain": unusableSum,
			// 临期小计与 pool 快照同源于本次响应：前端回写卡片时三个数字一起更新，
			// 避免「可用已同步、临期还是旧值」的半新半旧展示。
			"expiring_remain": expiring,
		})
	})
	mux.HandleFunc("POST /api/config/checkin_times", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Times []string `json:"times"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := a.SetCheckinTimes(req.Times); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/config/loop_guard", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			On bool `json:"on"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := a.SetLoopGuard(req.On); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/config/listen", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Host      string  `json:"host"`
			Port      int     `json:"port"`
			AdminPass *string `json:"admin_password"` // 指针：区分「未携带」与「显式清空」
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// 同一请求内先存密码再切监听：面板把「改成 0.0.0.0」与「填密码」合并保存，
		// 避免中间态因缺少密码而被拒。
		if req.AdminPass != nil {
			if err := a.SetAdminPassword(*req.AdminPass); err != nil {
				apiError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if err := a.SetListen(req.Host, req.Port); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/config/admin_password", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Password string `json:"password"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := a.SetAdminPassword(req.Password); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/config/api_key", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Key string `json:"key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := a.SetAPIKey(req.Key); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/config/autostart", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			On bool `json:"on"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := a.SetAutostart(req.On); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/config/model_fallback", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Rules []config.ModelFallbackRule `json:"rules"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		warns, err := a.SetModelFallback(req.Rules)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		// 软警告随成功响应下发（允许保存，只告知歧义），面板用黄字提示。
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warnings": warns})
	})
	mux.HandleFunc("POST /api/config/reasoning_efforts", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Efforts map[string]string `json:"efforts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := a.SetReasoningEfforts(req.Efforts); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/config/compat", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			DefaultChannel string            `json:"default_channel"`
			MaxTokensCap   int               `json:"max_tokens_cap"`
			ModelMap       map[string]string `json:"model_map"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := a.SetCompat(req.DefaultChannel, req.MaxTokensCap, req.ModelMap); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/config/proxy", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Proxy       string  `json:"proxy"`         // 备用上游通道地址（空串 = 关闭）
			OczenAPIKey *string `json:"oczen_api_key"` // 指针：区分「未携带」与「显式清空」
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		key := "\u0000unset\u0000"
		if req.OczenAPIKey != nil {
			key = *req.OczenAPIKey
		}
		if err := a.SetBackupProxy(req.Proxy, key); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/config/body_log", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			On bool `json:"on"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := a.SetBodyLog(req.On); err != nil {
			apiError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("POST /api/config/oczen_test", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			APIKey string `json:"api_key"` // 空 = 用当前生效凭证（配置中的 key 或匿名）
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		status, body, via, err := a.TestOczenKey(req.APIKey)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "status": 0, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": status == 200 || status == 429, "status": status, "body": body, "via_backup": via})
	})
	mux.HandleFunc("POST /api/config/custom_channels", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Channels []custom.ChannelConfig `json:"channels"`
			Renames  []customRename         `json:"renames"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, "解析请求失败："+err.Error())
			return
		}
		n, err := a.setCustomChannels(req.Channels, req.Renames)
		if err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"channels": a.cfg.CustomChannels,
			"renamed":  n,
		})
	})
	mux.HandleFunc("POST /api/config/custom_channel_test", func(w http.ResponseWriter, r *http.Request) {
		var req custom.ChannelConfig
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, "解析请求失败："+err.Error())
			return
		}
		if req.BaseURL == "" {
			apiError(w, http.StatusBadRequest, "base_url 不能为空")
			return
		}
		// 面板回显的 api_key 是脱敏值：测试未改动过的渠道时必须用真实密钥，否则测试恒报 401
		//（与 #117 的「探针没走同一条链路」同类）。判定不能只认「…」：脱敏格式改成长头长尾后
		// 中间是四个星号，两种都得认，因为升级瞬间浏览器里可能还渲染着旧脱敏串。
		if isMaskedKey(req.APIKey) {
			req.APIKey = a.resolveCustomKey(req.Name)
		}
		// 探测前先做端点定位（R121）：用户粘的地址常带 /chat/completions 尾巴、漏协议头，
		// 直接拼会请求一个必死地址；Discover 同时回答「哪个根是真的」。
		// 除纯 ID 外还回能力清单（R119）：前端勾选导入时直接拿这些字段写配置；
		// 上游没声明的字段保持零值/nil ⇒ 前端标「未探测」，绝不拿 1M/64k 这类猜测值冒充上游声明（R49 同口径）。
		d, err := custom.Discover(req)
		var caps []custom.ModelCap
		if d != nil {
			caps = d.Caps
		}
		ids := make([]string, 0, len(caps))
		for _, c := range caps {
			ids = append(ids, c.ID)
		}
		resp := map[string]any{"models": ids, "caps": caps} // models 保持旧形状 []string，不破既有契约
		if d != nil {
			resp["base_url"], resp["adjusted"] = d.Base, d.Adjusted
			resp["renormed"], resp["models_ok"] = d.Renormed, d.ModelsOK
			resp["chat_ok"], resp["tries"] = d.ChatOK, d.Tries
			if d.Note != "" {
				resp["note"] = d.Note
			}
		}
		if err != nil {
			resp["ok"], resp["error"] = false, err.Error()
			writeJSON(w, http.StatusOK, resp)
			return
		}
		resp["ok"] = true
		writeJSON(w, http.StatusOK, resp)
	})
	// 面板「点击复制 api-key」：明文按需单条下发，不随渠道列表/费率接口批量返回
	//（否则每次刷新面板都把全部 key 送进浏览器，抓包/历史里全都是）。
	// 口径说明：自定义渠道的 key 是**用户自己填的第三方凭据**，不是本工具的上游会话 token
	//（不变量 2 管的是后者），且本端点同其它 /api/* 一样受 cookie 会话鉴权约束（R4）。
	// 日志只记渠道名，绝不落 key 本身。
	mux.HandleFunc("POST /api/config/custom_channel_key", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			apiError(w, http.StatusBadRequest, "解析请求失败："+err.Error())
			return
		}
		key := a.resolveCustomKey(req.Name)
		if key == "" {
			apiError(w, http.StatusNotFound, fmt.Sprintf("自定义渠道 %s 不存在或未配置 api key", req.Name))
			return
		}
		log.Printf("已下发自定义渠道 %s 的 api_key 明文（面板复制）", req.Name)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "api_key": key})
	})
	mux.HandleFunc("POST /api/account/nickname", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if err := a.SetAccountNickname(req.UID, req.Nickname); err != nil {
			apiError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/fees", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.FeesInfo())
	})
	mux.HandleFunc("POST /api/fees/refresh", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, a.FeesInfo())
		go a.safeGo(func() { a.RefreshPricing() })
	})
	mux.HandleFunc("GET /api/usage", func(w http.ResponseWriter, r *http.Request) {
		if a.ledger == nil {
			writeJSON(w, http.StatusOK, map[string]any{"disabled": true})
			return
		}
		days := 7
		if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil {
			days = d
		}
		// enrich：uid → 昵称/渠道（查 pool 状态，仅一次遍历）
		nameOf := map[string]string{}
		chOf := map[string]string{}
		for _, rt := range a.runtimesSnapshot() {
			if rt == nil || rt.Pool == nil || rt.Alias {
				continue
			}
			for _, st := range rt.Pool.List() {
				nameOf[st.UID] = st.Nickname
				chOf[st.UID] = rt.Kind.String()
			}
		}
		stats := a.ledger.Query(days, func(uid string) (string, string) { return nameOf[uid], chOf[uid] })
		writeJSON(w, http.StatusOK, stats)
	})
	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		fp := filepath.Join(filepath.Dir(a.cfg.StateFile), "app.log")
		raw, _ := os.ReadFile(fp)
		lines := strings.Split(string(raw), "\n")
		if len(lines) > 300 {
			lines = lines[len(lines)-300:]
		}
		writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
	})
	// 运行统计面板（web 第三个 tab）：快照 = internal/stats 引擎的实时聚合，
	// 载荷字段与 sidecar /capi/stats 对齐（前端 PR2 已按契约轮询）。
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		if a.stats == nil {
			writeJSON(w, http.StatusOK, map[string]any{"disabled": true})
			return
		}
		writeJSON(w, http.StatusOK, a.stats.Snapshot(time.Now()))
	})
	// 请求日志（最近 N 条，默认 15；rows 形状对齐 sidecar /capi/logs）
	mux.HandleFunc("GET /api/stats/logs", func(w http.ResponseWriter, r *http.Request) {
		if a.stats == nil {
			writeJSON(w, http.StatusOK, map[string]any{"rows": []any{}})
			return
		}
		limit := 15
		if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
			limit = n
		}
		writeJSON(w, http.StatusOK, map[string]any{"rows": a.stats.Logs(limit)})
	})
	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		go a.safeGo(func() { a.Quit() })
	})
}

// guardMux 会话鉴权中间件：密码为空（未启用）时原样放行，否则要求有效 cookie 会话。
// 另叠加 stdlib 的跨站请求保护（CSRF）：SameSite=Lax 已在浏览器侧拦跨站 POST，
// 这里是二次防御（非浏览器客户端无 Origin/Sec-Fetch-Site 头，不受影响）。
// 挂在内层 /api/ mux 上（/api/auth/* 由外层更具体的模式直接命中，不经过这里）。
func (a *App) guardMux(inner http.Handler) http.Handler {
	csrf := http.NewCrossOriginProtection()
	return csrf.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.panelAuthEnabled() {
			c, err := r.Cookie(sessionCookie)
			if err != nil || !a.auth.valid(c.Value, passBrief(a.adminPass())) {
				writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录或会话已过期", "need_login": true})
				return
			}
		}
		inner.ServeHTTP(w, r)
	}))
}

// setSessionCookie 下发/删除会话 cookie。
// HttpOnly 防脚本窃取（前端靠 /api/auth/state 的 auth_session 指纹判登录态，不读 cookie）；
// SameSite=Lax 挡跨站 CSRF；Secure 在 HTTPS 下自动开启（本服务默认 http，故按需）。
func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	c := &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	}
	if token == "" {
		c.MaxAge = -1 // 删除
	}
	http.SetCookie(w, c)
}

// clientIP 取登录限流用的客户端 IP（RemoteAddr 形如 host:port）。
func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// ---------------------------------------------------------------------------
// 渠道费率信息（本地缓存 + 按需刷新）
// ---------------------------------------------------------------------------

// feesChannel 一个渠道在「模型列表和费率」面板中的分组数据。
type feesChannel struct {
	Channel string         `json:"channel"`
	Models  []feesModelRow `json:"models"`
	// BaseURL/APIKeyMasked 仅自定义渠道填写（R119）：面板把费率页当「模型接入点总表」用，
	// 需要在一处看全每个模型的 endpoint 与 key。key 始终是**脱敏值**（maskKey），明文由前端点「复制」
	// 时另走 POST /api/config/custom_channel_key 按需取，不随本接口批量下发。
	BaseURL      string `json:"base_url,omitempty"`
	APIKeyMasked string `json:"api_key_masked,omitempty"`
}

// feesModelRow 一行模型：来自渠道 models 列表，尽可能配上费率。
type feesModelRow struct {
	Model string `json:"model"`
	// ID 是客户端实际要填的完整模型 ID（与 /v1/models 的 id、provider.PublicModelID 同源）：
	// 普通渠道为 `<渠道>/<模型>`，上游 ID 自带命名空间的渠道（cline 的 cline-free/*）
	// 直接就是上游 ID（R59）。面板一律拿它做复制，不在前端再拼前缀（R52 的教训：
	// 拼接规则散在多处必漏）。
	ID string `json:"id"`
	// Priced 表示上游费率接口对该模型返回了可用的倍率值。
	// 注意：credits 字段缺失（如 CN 的 auto 调度器）也算未定价，
	// 不能把「缺字段解析出的 0」当成免费。
	Priced bool    `json:"priced"`
	Rate   float64 `json:"rate"`           // 倍率（Priced=false 时无意义）
	Free   bool    `json:"free"`           // 上游明确标注倍率为 0（x0.00）
	Note   string  `json:"note,omitempty"` // 促销/标签说明（已剔除颜色）
	// Color 为促销标签颜色（形如 "#FF0000"），无则空。
	Color string `json:"color,omitempty"`
	// HasContext 标记该模型的上下文窗口/最大输出是否可展示：
	// 上游接口真实返回（ContextFromAPI）或本工具手工实测（HandMeasured）均置 true。
	// false 表示无可靠数据（硬编码估算或上游不返回且未实测），前端提示「未知」而非展示数字。
	HasContext    bool  `json:"has_context"`
	ContextWindow int64 `json:"context_window,omitempty"`
	MaxTokens     int64 `json:"max_tokens,omitempty"`
	// HandMeasured 标记上述上下文值是**本工具实测**（非上游声明），前端据此加「实测」标注。
	HandMeasured bool `json:"hand_measured,omitempty"`
	// CapSource 能力/上下文数据的来源，供前端逐行如实标注（R119）：
	// "api"=上游声明、"measured"=本工具实测、"user"=面板手填、""=未声明（前端显「未探测」）。
	// 只影响展示文案，不影响路由；与自定义渠道配置里的 cap_source 同源。
	CapSource string `json:"cap_source,omitempty"`
	// 能力标记（与 /v1/models 同源），供 UI 在模型 ID 后展示图标。
	SupportsImages    bool `json:"supports_images"`
	SupportsReasoning bool `json:"supports_reasoning"`
	SupportsTools     bool `json:"supports_tools"`
	// 思考档位（issue #96，与 /v1/models 同源）：上游声明的可选档位与默认档，
	// 空表 = 上游未声明（前端不展示，不编造）。
	ReasoningEfforts       []string `json:"reasoning_efforts,omitempty"`
	DefaultReasoningEffort string   `json:"default_reasoning_effort,omitempty"`
}

// feeChannelOrderWithDynamic 返回费率表渠道顺序：静态 feeChannelOrder + 未登记的动态渠道
// （自定义渠道，R66）追加到末尾（硬编码钉最末，组内按渠道标识排序）。仅当 modelsByKind
// 里确有该渠道的模型时才追加，避免空渠道占位。
func feeChannelOrderWithDynamic(modelsByKind map[provider.Kind][]provider.ModelInfo) []provider.Kind {
	seen := make(map[provider.Kind]bool, len(feeChannelOrder))
	for _, k := range feeChannelOrder {
		seen[k] = true
	}
	var extra []provider.Kind
	for k, infos := range modelsByKind {
		if !seen[k] && len(infos) > 0 {
			extra = append(extra, k)
		}
	}
	if len(extra) == 0 {
		return feeChannelOrder
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i] < extra[j] })
	return append(append([]provider.Kind{}, feeChannelOrder...), extra...)
}

// buildFeesChannels 以「渠道 models 列表」为基准组表：
//   - 每个渠道的模型行来自该渠道当前生效的 models（与 /v1/models 同源）；
//   - 再用该渠道费率接口返回的 model→rate 映射逐行填充；
//   - 未在费率接口中出现的模型标记 Priced=false（前端显示 unknown + tooltip）；
//   - 费率接口中多出的模型（models 列表中不存在）不展示，避免与 /v1/models 不一致。
func buildFeesChannels(modelsByKind map[provider.Kind][]provider.ModelInfo,
	pricing []provider.ModelPricing, order []provider.Kind) []feesChannel {
	// 渠道 → (model → pricing)
	byChannel := make(map[string]map[string]provider.ModelPricing)
	for _, p := range pricing {
		m := byChannel[p.Channel]
		if m == nil {
			m = make(map[string]provider.ModelPricing)
			byChannel[p.Channel] = m
		}
		m[p.Model] = p
	}
	out := make([]feesChannel, 0, len(order))
	for _, k := range order {
		// 旧 Qoder 渠道已从界面下线：不拉取/不展示其模型列表（路由保留，存量账号仍可用）
		if k == provider.Qoder {
			continue
		}
		infos := modelsByKind[k]
		if len(infos) == 0 {
			continue
		}
		prices := byChannel[k.String()]
		rows := make([]feesModelRow, 0, len(infos))
		for _, mi := range infos {
			row := feesModelRow{
				Model:         mi.ID,
				ID:            provider.PublicModelID(k, mi.ID),
				ContextWindow: mi.ContextWindow,
				MaxTokens:     mi.MaxTokens,
				// 仅上游接口返回的上下文可信；硬编码估算值不展示数字。
				// 上游声明（API）或本工具手工实测均视为「有上下文可展示」；
				// 前端据 hand_measured 区分并加「实测」标注，不把实测冒充成官方值。
				HasContext:   mi.ContextFromAPI || mi.HandMeasured || mi.ContextFromUser,
				HandMeasured: mi.HandMeasured,
				CapSource:    capSourceOf(mi),
				// 能力标记与 /v1/models 同源，保证 UI 图标与接口声明一致。
				SupportsImages:    mi.SupportsImages,
				SupportsReasoning: mi.SupportsReasoning,
				SupportsTools:     mi.SupportsTools,
				// 思考档位（issue #96）与 /v1/models 同源透传
				ReasoningEfforts:       mi.ReasoningEfforts,
				DefaultReasoningEffort: mi.DefaultReasoningEffort,
			}
			if p, ok := prices[mi.ID]; ok {
				row.Priced = p.IsExplicit() // 缺倍率字段（如 auto）不算已定价
				row.Rate = p.Rate
				row.Free = p.IsExplicit() && p.Rate == 0
				row.Note = p.Note
				row.Color = p.Color
			}
			rows = append(rows, row)
		}
		// 排序：免费在前（便于发现可用免费模型），其余按倍率升序，未定价置后
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := rows[i], rows[j]
			if a.Priced != b.Priced {
				return a.Priced // 已定价的排前
			}
			if a.Rate != b.Rate {
				return a.Rate < b.Rate
			}
			return a.Model < b.Model
		})
		out = append(out, feesChannel{Channel: k.String(), Models: rows})
	}
	return out
}

// FeesInfo 返回「模型列表和费率」面板数据。
// 以各渠道当前生效的模型列表为基准（与 /v1/models 同源），逐行配上费率：
// 上游未返回倍率的模型标记 priced=false，前端显示 unknown。
func (a *App) FeesInfo() map[string]any {
	a.pricingMu.Lock()
	cached := a.pricingCache
	errMsg := a.pricingErr
	stale := time.Since(a.pricingFetched) > time.Hour
	a.pricingMu.Unlock()

	// 缓存过期时后台静默刷新
	if len(cached) > 0 && stale {
		go a.safeGo(func() { a.RefreshPricing() })
	}

	// 面板秒开：模型列表只用缓存/静态兜底（不触发上游网络请求）。
	// 后台 StartPricingAutoRefresh 每 30 分钟拉新并写入缓存，用户看到的是最近一次结果。
	var modelsByKind map[provider.Kind][]provider.ModelInfo
	if a.handler != nil {
		modelsByKind = a.handler.CachedChannelModels()
	} else {
		modelsByKind = a.staticModelFallback()
	}
	if len(modelsByKind) == 0 {
		modelsByKind = a.staticModelFallback()
	}

	// 首次加载且无缓存：异步拉取费率，先展示（unknown 占位）
	if len(cached) == 0 {
		go a.safeGo(func() { a.RefreshPricing() })
	}

	// 展示顺序见 feeChannelOrder（R142：按渠道显示名字节序，旧 Qoder 跳过），**末尾追加动态自定义渠道**（R66）。
	// 必须追加：feeChannelOrder 是静态表，自定义渠道不在其中，若不追加则
	// 面板费率表永远不出现该渠道 tab（buildFeesChannels 只遍历传入的 order）——
	// 这与 /v1/models、用量流水、运行统计的可见性不一致。按渠道名排序保证稳定顺序。
	order := feeChannelOrderWithDynamic(modelsByKind)
	channels := buildFeesChannels(modelsByKind, cached, order)
	a.applyCustomFeesMeta(&channels)

	result := map[string]any{
		"note":       "本表以各渠道实际可用模型列表为准；倍率为空的模型表示上游未返回定价（未知）。",
		"channels":   channels,
		"disclaimer": "本工具仅聚合转发，不参与定价；渠道费率以各上游官方页面为准。",
	}
	if len(cached) > 0 {
		result["cached_at"] = a.pricingFetched.Format("01-02 15:04")
	}
	if errMsg != "" {
		result["error"] = errMsg
	}
	return result
}

// capSourceOf 把 ModelInfo 的三个互斥来源位归成一个字符串，给面板逐行标注（R119）。
// 优先级 api > measured > user：同时置位时以最权威的为准（与 provider.ModelInfo 注释一致）。
func capSourceOf(mi provider.ModelInfo) string {
	switch {
	case mi.ContextFromAPI:
		return "api"
	case mi.HandMeasured:
		return "measured"
	case mi.ContextFromUser:
		return "user"
	}
	return ""
}

// applyCustomFeesMeta 给费率表里的自定义渠道补上接入点与脱敏 key（R119）。
// 抽成方法而不是内联在 FeesInfo：单测要能单独证「明文没进来、脱敏串进来了」。
// 内建渠道的 endpoint 写死在代码里，无需展示 ⇒ meta 表不命中就保持空串。
func (a *App) applyCustomFeesMeta(channels *[]feesChannel) {
	meta := a.customChannelFeesMeta()
	if len(meta) == 0 {
		return
	}
	for i := range *channels {
		if m, ok := meta[(*channels)[i].Channel]; ok {
			(*channels)[i].BaseURL = m[0]
			(*channels)[i].APIKeyMasked = m[1]
		}
	}
}

// staticModelFallback 无 handler 时的渠道→静态模型表兜底。
// app.Runtime 不带静态表（那是 server.Runtime 的字段），但**自定义渠道例外**：
// 它的 FetchModels 是「配什么返回什么」，绝不发网络请求（R66，已有测试锁死），
// 因此可以在此安全取回——否则启动早期/单测里费率页拿不到自定义渠道的行。
// 内建渠道的目录接口会打上游，绝不在这里调 ⇒ 非 Custom 渠道仍返回空。
func (a *App) staticModelFallback() map[provider.Kind][]provider.ModelInfo {
	out := map[provider.Kind][]provider.ModelInfo{}
	for _, rt := range a.runtimesSnapshot() {
		if rt == nil || !rt.Custom || rt.Upstream == nil {
			continue
		}
		if ms, err := rt.Upstream.FetchModels(nil); err == nil && len(ms) > 0 {
			out[rt.Kind] = ms
		}
	}
	return out
}

// RefreshPricing 从所有已接入渠道拉取最新定价并更新缓存。
// 并发调用会被丢弃（已有一次在跑）——启动刷新与面板「刷新」可能重叠。
func (a *App) RefreshPricing() {
	a.pricingMu.Lock()
	if a.pricingBusy {
		a.pricingMu.Unlock()
		return
	}
	a.pricingBusy = true
	a.pricingMu.Unlock()
	defer func() {
		a.pricingMu.Lock()
		a.pricingBusy = false
		a.pricingMu.Unlock()
	}()

	allPricing := make([]provider.ModelPricing, 0)
	var errs []string

	for _, rt := range a.runtimesSnapshot() {
		// 旧 Qoder 渠道已从界面下线：不再自动刷新其模型/费率（也避免无谓的 token refresh 报错日志）
		if rt == nil || rt.Pool == nil || rt.Upstream == nil || rt.Kind == provider.Qoder || len(rt.Pool.List()) == 0 {
			continue
		}
		acct := rt.Pool.Pick()
		if acct == nil {
			continue
		}
		// 先确保 token 有效
		if acct.NeedsRefresh(10 * time.Minute) {
			if err := rt.Upstream.RefreshToken(acct); err != nil {
				errs = append(errs, fmt.Sprintf("%s: token refresh failed", rt.Kind))
				continue
			}
			// 必须落盘：refresh token 会轮换，不写回则下次启动用的是旧 refresh token，
			// 而旧 access token 已被上游作废 → 本地 expiresAt 仍显示有效 → 卡死在 401。
			if serr := acct.SaveAtomic(); serr != nil {
				log.Printf("pricing token save failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, serr)
			}
		}
		pricing, err := rt.Upstream.FetchModelPricing(acct)
		if err != nil {
			// 401 同样先刷新再重试一次，避免费率因过期 token 长期拉不到。
			if a.refreshIfSessionDead(rt, acct, err) {
				pricing, err = rt.Upstream.FetchModelPricing(acct)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %s", rt.Kind, shortErr(err)))
			log.Printf("pricing fetch failed platform=%s err=%v", rt.Kind, err)
			continue
		}
		allPricing = append(allPricing, pricing...)
		log.Printf("pricing fetched platform=%s count=%d", rt.Kind, len(pricing))
	}

	// 排序：按渠道 + 倍率
	sort.Slice(allPricing, func(i, j int) bool {
		if allPricing[i].Channel != allPricing[j].Channel {
			return allPricing[i].Channel < allPricing[j].Channel
		}
		return allPricing[i].Rate < allPricing[j].Rate
	})

	a.pricingMu.Lock()
	a.pricingCache = allPricing
	a.pricingFetched = time.Now()
	if len(errs) > 0 {
		a.pricingErr = strings.Join(errs, "; ")
	} else {
		a.pricingErr = ""
	}
	a.pricingMu.Unlock()
	a.savePricingCache()
}

// loadPricingCache 从文件加载定价缓存。
func (a *App) loadPricingCache() {
	raw, err := os.ReadFile(a.pricingFP)
	if err != nil {
		return
	}
	var cache struct {
		Models  []provider.ModelPricing `json:"models"`
		Fetched string                  `json:"fetched"`
	}
	if err := json.Unmarshal(raw, &cache); err != nil {
		return
	}
	a.pricingMu.Lock()
	a.pricingCache = cache.Models
	if cache.Fetched != "" {
		if t, err := time.Parse(time.RFC3339, cache.Fetched); err == nil {
			a.pricingFetched = t
		}
	}
	a.pricingMu.Unlock()
}

// savePricingCache 持久化定价缓存到文件。
func (a *App) savePricingCache() {
	a.pricingMu.Lock()
	cache := struct {
		Models  []provider.ModelPricing `json:"models"`
		Fetched string                  `json:"fetched"`
	}{
		Models:  a.pricingCache,
		Fetched: a.pricingFetched.Format(time.RFC3339),
	}
	a.pricingMu.Unlock()
	raw, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(a.pricingFP, raw, 0o644)
}

// ---------------------------------------------------------------------------
// 日志
// ---------------------------------------------------------------------------

// logWriter 写日志文件。
type logWriter struct{ app *App }

func (w *logWriter) Write(p []byte) (int, error) {
	if w.app.logFile != nil {
		_, _ = w.app.logFile.Write(p)
	}
	// 运行统计：签到事件实时喂入（log 库单次 Write 即完整一行；非事件行
	// 只做至多 5 个锚定正则匹配即跳过，开销可忽略。与启动时的全量
	// RescanEventLog 按 uid|day 幂等去重，不会双计）
	if w.app.stats != nil {
		w.app.stats.ObserveLogLine(string(p))
	}
	return len(p), nil
}

// safeGo 带 panic 兜底的 goroutine 启动器。
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

func normalizeMinutes(minutes []int) []int {
	seen := map[int]bool{}
	out := []int{}
	for _, m := range minutes {
		if m >= 0 && m < 24*60 && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Ints(out)
	return out
}

// lanIP 返回非环回的出站 IPv4/IPv6 地址（如 192.168.1.5），用于监听 0.0.0.0 时
// 面板提示局域网可用的 API 地址。UDP Dial 不实际发包，仅让内核选路由；
// 无网络/全部环回时返回空串，调用方自行兑底。
func lanIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil || addr.IP.IsLoopback() {
		return ""
	}
	return addr.IP.String()
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("01-02 15:04")
}

// unixOrZero 解冻时刻的 unix 秒（零值返回 0）。统一 24 小时制补充通道：
// "01-02 15:04" 文案虽是 24h 制，但跨零点的次日清晨时刻（如 07:07）仍易被
// 误读为「12 小时制丢了上午/下午」；面板拿到 until_ts 后可实时算出
// 「剩 11 时 53 分」，绝对时刻 + 相对时长并列展示，歧义自然消除。
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// fmtTimeMap 模型级冷却 map 的时间格式化（仅保留未过期条目；空 map 返回 nil 以便 omitempty）。
func fmtTimeMap(m map[string]time.Time) map[string]string {
	if len(m) == 0 {
		return nil
	}
	now := time.Now()
	out := make(map[string]string, len(m))
	for model, t := range m {
		if t.After(now) {
			out[model] = fmtTime(t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func shortUID(uid string) string {
	if len(uid) <= 10 {
		return uid
	}
	return uid[:10] + "…"
}

func shortErr(err error) string {
	s := strings.TrimSpace(err.Error())
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
