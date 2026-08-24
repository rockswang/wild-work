# wild-work 交接文档（HANDOFF）

> 交接时间：2026-08-23
> 交接目标：**继续开发 wild-work（WorkBuddy-Wild 前身的重构版）**，或按本文档恢复上下文后接手后续任务。

---

## 0. 一句话现状

**新项目 `wild-work` 已完成首版可运行骨架**：跨平台（Win/mac）托盘 daemon + 系统浏览器 Web UI 的多渠道账号聚合工具。
去掉 wails/WebView2，复用上一代的账号池/调度/上游/登录核心。**代码可编译、测试全绿、端到端验证通过**（Linux 无头模式），但**尚未在 Windows/macOS 真机验证托盘交互**。

## 1. 环境（重要）

| 项 | 值 |
|---|---|
| 项目目录 | `/root/pi-cwd-20260823/wild-work` |
| 上一代源码 | `/root/pi-cwd-20260823/docs/workbuddy-wild`（克隆备查，勿改） |
| Go 工具链 | go1.26.7，`/usr/local/go`，PATH 已全局（`/etc/profile.d/go.sh`） |
| 依赖代理 | **必须** `export https_proxy=http://10.126.126.1:10809 http_proxy=http://10.126.126.1:10809`（拉模块/网络用） |
| GOPROXY | `https://goproxy.cn,direct`（已 go env -w） |
| 全局提示词 | `/root/AGENTS.md` 已更新（Go 工具链 + 代理③章节） |

> 注：直接 `go` 命令需 `export PATH=$PATH:/usr/local/go/bin`（新 shell 自动生效）。

## 2. 已完成工作

### 2.1 架构改造
- 新模块 `wild-work`（go.mod module name = `wild-work`），33 个 .go 文件
- **移除 wails/WebView2**，改为：托盘 daemon + `http.Server` 单端口（OpenAI 端点 + `/api/*` 管理 API + 静态 Web UI embed）
- 跨平台封装 `internal/platform`（`_windows.go`/`_darwin.go`/`_other.go` build tag 拆分）
- 托盘封装 `internal/systray`（energye/systray，**固定菜单**）

### 2.2 品牌重命名
- 代码/资源（go/html/js/css/json）**零残留** `workbuddy-wild`/`WorkBuddy2API`/`WB2A_` 字样
- 默认 APIKey：`WorkBuddy2API` → `WildWorkAPI`；环境变量前缀 `WB2A_` → `WILDWORK_`
- 文档（README/AGENTS）仅保留与前代/上游的**关系提及**（README 3 处 + AGENTS 1 处）

### 2.3 复用自上一代（原样迁移）
`internal/pool` / `scheduler` / `upstream` / `traework` / `login` / `login_trae` / `auth` / `config` / `provider` / `server`(handler) —— 测试全绿

## 3. 关键决策（AGENTS.md R1-R11，勿推翻）

| # | 决议 |
|---|---|
| R1-R3 | 托盘菜单**固定**：打开主界面 / 刷新积分 / 查看日志 / 退出；刷新积分异步 + 系统消息框通知；单击/双击托盘=打开主界面 |
| R4 | 管理 API **无鉴权**（个人单机），0.0.0.0 风险用户自负 |
| R5 | Web UI 纯静态 HTML/CSS/JS，无编译链，`go:embed` |
| R6 | 托盘库保留 energye/systray |
| R7 | 移除 wails/WebView2 |
| R8 | 单 http.Server 多用途 |
| R9 | 核心业务复用，数据格式零迁移 |
| R10 | 渠道扩展 = 实现 `provider.Upstream` 接口 + auth 加载器 + 注册 Runtime（前缀路由 `channel/<model>`） |
| R11 | Windows 产物 WSL 交叉编译；macOS 产物走 CI macos-latest（cgo 必需） |

## 4. 代码地图（wild-work）

```
cmd/wild-work/main.go        # daemon 装配 + 托盘 + 无头兜底（recover 托盘 panic 后 HTTP 继续跑）
cmd/wild-work/web/           # 静态 Web UI（index.html/style.css/app.js/logo64.png）
cmd/wild-work/build/         # 托盘图标 embed（trayicon.ico）
cmd/genicon/                 # 图标生成（go run ./cmd/genicon，需根目录 icon.png）
internal/app/app.go          # 管理 API（/api/*）+ 登录编排 + 刷新积分汇总 + 配置热更新
internal/server/handler.go   # OpenAI 端点 + WebUI 挂载 + AttachAPI 钩子
internal/platform/           # 跨平台能力（浏览器/自启/消息框/日志/工作区）
internal/systray/systray.go  # 固定菜单托盘
internal/{pool,scheduler,upstream,traework,login,login_trae,auth,config,provider}  # 继承核心
build/                       # appicon.png / trayicon.ico / icon.ico / logo64.png
dist/wild-work.exe           # Windows 交叉编译产物（11MB）
```

## 5. 管理 API 契约（已实现，Web UI 在用）

```
GET  /api/state                    # 全量状态
POST /api/login/start              # {channel:"workbuddy"|"traework"} → {auth_url}
POST /api/login/cancel
POST /api/account/checkin          # {uid}
POST /api/account/checkin_all      # → {results:[...]}
POST /api/account/refresh          # {uid} → {remain}
POST /api/account/refresh_all      # → {busy?,total,ok,failed,platforms}
POST /api/account/remove           # {uid}
POST /api/config/checkin_times     # {times:["08:30","20:15"]}
POST /api/config/listen            # {host,port}（热切换）
POST /api/config/api_key           # {key}
POST /api/config/autostart         # {on:bool}
GET  /api/fees                     # 渠道费率说明
GET  /api/logs                     # 最近 300 行日志
POST /api/quit                     # 退出程序
```

## 6. 已验证（Linux 无头模式实测）

- `go build ./... && go vet ./... && go test ./...` 全绿（6 个包）
- Windows 交叉编译 `GOOS=windows GOARCH=amd64 CGO_ENABLED=0` 成功
- 端到端：healthz ok / api/state 正常 / Web UI 200 / fees / 签到时间热更新 / APIKey 更新 / **监听热切换 7863→7864** / api/quit 退出
- 无头兜底：无桌面环境托盘 panic → recover → HTTP 服务继续（日志可见 "托盘初始化失败，以无头模式继续运行"）

## 7. 下一步待办（建议顺序）

1. **Windows 真机验证**（最重要，当前无法在本环境做）：
   - 托盘图标/右键菜单交互、单击/双击打开浏览器、刷新积分消息框、开机自启注册表
   - 登录流程（无痕浏览器拉起 + 回调轮询）实际跑通
2. **macOS 构建**：GitHub Actions 加 macos-latest job（`darwin/arm64`+`amd64`，需签名/公证说明）；darwin 平台层已写好但未编译验证（cgo 必需，本地不可交叉）
3. **CI 完善**：`.github/workflows/release.yml` 双平台自动发布（参考上一代，但去掉 wails）
4. **单实例锁**：双击 exe 防止重复启动（旧版有 SingleInstanceLock，现在没了）
5. **日志查看**：Web UI 有 /api/logs 接口但前端还没展示入口（托盘菜单"查看日志"用系统编辑器）
6. **费率页完善**：目前 /api/fees 是静态说明，可考虑接上游动态模型列表
7. 后续渠道扩展（qoderwork2api/deepseek2api/chatgpt2api）——架构已支持，见 AGENTS.md §5

## 8. 注意事项 / 坑

- **托盘在无桌面环境必 panic**：energye/systray linux 依赖 DBus（`systray_unix.go`），main.go 已有 recover 兜底，测试无头用 `--autostart`
- **托盘库 darwin 是 cgo**（`systray_darwin.m` Objective-C），本地 WSL 无法交叉编译 macOS，只能 CI
- `go:embed` 路径相对源文件：web 和 build 图标都必须在 `cmd/wild-work/` 下（不要移回根目录，除非改 embed）
- 登录流程落 `data/login-state.json`（继承上一代，勿泄漏 token 到日志）
- 配置 `config.json` / auths/ / data/state.json 格式与上一代完全兼容，可无缝迁移用户数据

## 9. 建议技能（后续 Agent 参考）

- `ast-outline`：继续梳理调用链/新增渠道时用
- `diagnose`：Windows 真机托盘/登录问题按复现→定位→修复→回归
- `tdd`：新增渠道（qoderwork2api 等）建议测试先行
- `fiddler-saz-analysis`：分析上游接口变化时用
- `doc-coauthoring`：完善 README/用户文档时用

## 10. 敏感信息提醒

- 全程零 token 原则：日志/文档/消息框不得出现 access/refresh token
- auths/ 目录含真实凭证，任何测试/文档不得引用具体账号身份
