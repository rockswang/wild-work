# WorkBuddy Wild — 开发者文档（面向 AI Agent）

> 本文件为**代码维护者（含 AI Agent）**而写，普通用户请看 [README](../README.md)。
> 修改本仓库前，先读"关键不变量"一节；涉及平台差异时，先读"跨平台移植"一节。

## 0. 项目速览

- **是什么**：WorkBuddy/CodeBuddy 账号的 OpenAI 兼容代理 + 自动签到 Windows 托盘应用。单进程 = HTTP 代理 + 调度器 + 托盘面板。
- **技术栈**：Go 1.25+ / Wails v2.14（WebView2 桌面壳）/ energye/systray（托盘）/ 纯静态前端（无 npm）。
- **模块**：`github.com/rockswang/workbuddy-wild`（模块名别改，import 全量依赖它）。
- **当前平台**：仅 Windows amd64。核心业务纯 Go，可移植；平台耦合集中在 `internal/winutil` 与 `main.go` 的 wails 选项。
- **状态文件**：`config.json`（配置）、`auths/workbuddy-<uid>.json`（凭证，勿外泄）、`data/state.json`（冷却/签到状态）、`data/app.log`（日志）、`data/webview/`（WebView2 profile）。

## 1. 代码地图

| 包 | 职责 | Agent 改动的注意点 |
|---|---|---|
| `main.go` | 装配：chdir 到 exe 目录 → 加载配置 → 组装 pool/upstream/scheduler/handler → 启动 HTTP → 启动托盘 → wails.Run | `--autostart` 参数决定是否弹"已启动"提示；**WebView2 用户数据目录固定 `data/webview`**；孤儿清理在此触发；`Windows` 选项是平台相关（macOS 见 §6） |
| `internal/app` | wails 绑定层：面板数据/操作/事件、登录编排、端口热切换、日志 | 所有方法都是 wails Bind（导出方法）；`ShowPanel` 必须先等 `domReadyCh`（防白窗口）且托盘回调需 `go` 化（防卡死）；**任何写日志的地方不得含 token** |
| `internal/config` | 配置加载/校验/原子写回 | `Listen` 兼容旧字符串格式与对象格式；`os.IsNotExist` 不穿透 `%w` 包装，判断文件缺失必须用 `errors.Is(err, fs.ErrNotExist)` |
| `internal/pool` | 账号池：挑号（余额最高）、冷却/禁用状态机、state.json 持久化 | `Disabled` 同时排除调度与挑号；state 格式**向后兼容**（只加字段别删） |
| `internal/scheduler` | 定时签到 + token 保活 + 冷却解冻 + 签到记录 | 支持运行时改时间（`SetCheckinHours` + wake 通道）；`CheckinAccount` 单账号手动签到 |
| `internal/upstream` | 上游 HTTP（chat/billing/auth）+ 错误分类 | **`PrepareBody` 三件套勿动**（见 §2）；账单接口用短超时 `BillingHTTP`(30s)；聊天 120s |
| `internal/server` | OpenAI 兼容 HTTP handler | APIKey 运行时可变（`SetAPIKey` 带锁）；`/status` 需鉴权 |
| `internal/login` | CN OAuth 登录（state/token/account）+ 登录页跳转链解析 | state 落 `data/login-state.json`（不用系统临时目录） |
| `internal/auth` | auth 文件解析（嵌套/扁平双形态）+ 原子写回 | 落盘格式嵌套形 `{auth,account}`，与登录写盘一致 |
| `internal/winutil` | **Windows-only**：工作区/任务栏/无痕浏览器/开机自启/MessageBox/孤儿清理 | macOS/Linux 移植的集中改造点（§6） |
| `cmd/server|login|signin|credit|genicon` | 无头 CLI / 图标生成 | 无 GUI 依赖，`cmd/server` 可作无头模式调试 |
| `frontend/dist` | 纯静态前端（HTML/JS/CSS） | 直接调 `window.go.app.App.*` 与 `window.runtime.*`，无 wailsjs 生成物 |

## 2. 关键不变量（改了会出事）

1. **`upstream.PrepareBody` 的三种改写缺一不可**：强制 `stream=true`（上游拒非流式）、`tool_choice` 归一化（对象形式会 400 code=11101）、**`role=developer → system`**（上游对 developer 角色误触发内容过滤，返回"检测到敏感内容"；pi 等客户端对推理模型使用该角色）。
2. **日志/面板零 token**：任何输出不得包含 access token / refresh token / GitHub token（调试时用假 token 代替）。
3. **auth 文件格式**：`{auth:{accessToken,refreshToken,expiresAt,domain}, account:{uid,enterpriseId,nickname}}`，`internal/auth` 读取与 `internal/login.SaveAuth` 写入必须一致。
4. **config.listen 兼容**：新对象格式 `{"host","port"}` + 旧字符串格式 `":7863"` 都要能解析。
5. **state.json 向后兼容**：只增不减字段，旧文件缺失字段按零值处理。
6. **托盘回调必须 `go a.ShowPanel()`**：systray 消息循环线程上禁止执行任何重量逻辑（wails 调用会 marshal，阻塞即卡死托盘）。
7. **面板显示前等 `domReadyCh`**：冷启动 WebView2 很慢，直接 `WindowShow` 会白窗口。
8. **WebView2 用户数据目录**固定 `data/webview`，启动前检测 `SingletonLock` 并清理孤儿（强杀残留会锁 profile 导致假死/白窗口）。

## 3. 数据流

```
【请求】客户端 → /v1/chat/completions → server(鉴权) → pool.PickExcluding(余额最高)
      → upstream.ChatStream(PrepareBody 三改写) → copilot.tencent.com/v2/chat/completions
      → SSE 流回 → 错误按分类驱动冷却状态机
【签到】scheduler(9/21点或面板触发) → DailyCheckin → UserResource(30s短超时)
      → ReenableIfCredits 解冻 → RecordCheckin 落 state.json → 事件推面板
【登录】面板"添加账号" → login.Start(auth/state) → ResolveAuthURL 跳转链 → 无痕浏览器打开
      → 轮询 auth/token(2s) → 成功写 auths/ 文件 → pool 重载 → 异步签到
【配置】面板修改 → config.Save 原子写回 → 运行时生效（SetListen 热切换监听 / SetCheckinHours 唤醒调度）
```

## 4. 常用命令（Agent 实际操作）

```bash
go build ./... && go vet ./... && go test ./...     # 全量校验（6 个包）
go run ./cmd/server -config config.json              # 无头模式起服务（调试 HTTP 链路，无需 GUI/桌面）
wails build -platform windows/amd64 -skipbindings    # 出 build/bin/workbuddy-wild.exe
go run ./cmd/genicon                                 # 重新生成图标
# 桌面 GUI 测试：本机如无交互桌面（SSH/服务会话），WebView2 无法创建，
# go-webview2 的 errorCallback 会直接 os.Exit(1)（recover 无效）——必须用真实桌面会话验证 GUI。
```

## 5. 上游接口（CN，均无跨域跳转，Go 客户端自动跟随）

| 用途 | 端点 |
|---|---|
| 登录发起 | `POST copilot.tencent.com/v2/plugin/auth/state?platform=CLI` |
| 登录轮询 | `GET /v2/plugin/auth/token?state=...`（pending 时业务 code≠0） |
| 账号信息 | `GET /v2/plugin/login/account?state=...` |
| 刷新 token | `POST /v2/plugin/auth/token/refresh`（头带 X-Refresh-Token） |
| 动态模型 | `GET /console/enterprises/personal/models` |
| 余额 | `POST www.codebuddy.cn/v2/billing/meter/get-user-resource` |
| 签到 | `POST /v2/billing/meter/daily-checkin` |
| 聊天 | `POST copilot.tencent.com/v2/chat/completions`（强制 stream） |

登录页跳转链：`copilot.tencent.com/login → 301 加斜杠 → www.codebuddy.cn/login`，用 `login.ResolveAuthURL` 预解析。

## 6. 跨平台移植（重点：macOS）

### 6.1 总体分层

当前 Windows 独占，根因是 `internal/winutil`（Win32 直调）+ `main.go` 的 `Windows: &windows.Options{}`。移植方案：

- `internal/winutil` 按 build tag 拆分：`winutil_windows.go`（现状）+ `winutil_darwin.go` + `winutil_other.go`，**保持同名导出函数**（接口不变量），`internal/app` 无需改。
- `main.go` 的 wails 选项拆 `main_windows.go` / `main_darwin.go`（`runGUI` 平台实现），公共装配留在 `main.go`（`//go:build` 不冲突即可）。
- 调度器/登录/池/上游/server/config 纯 Go，**零改动**。
- 前端零改动（wails 跨平台壳）。

### 6.2 winutil 能力 × macOS 替代方案

| Windows 实现 | macOS 替代 |
|---|---|
| `WorkArea()`（user32 SPI_GETWORKAREA） | 用 wails runtime `ScreenGetAll`（跨平台屏幕 API），或 cgo `NSScreen.mainScreen.visibleFrame` |
| `PanelAnchor`（Shell_TrayWnd 任务栏锚定） | macOS 无任务栏：托盘在**右上角菜单栏**，面板锚定主屏右上角（`visibleFrame` 右上 - 尺寸 - 8px）；`PanelAnchor` 只按平台给偏移即可 |
| `HideFromTaskbar`（WS_EX_TOOLWINDOW） | 无任务栏，无需实现（空函数）；窗口置顶用 wails `AlwaysOnTop` |
| `MainWindow`/`FocusWindow`（FindWindowW/SetForegroundWindow） | macOS：`NSApp.activateIgnoringOtherApps(true)`（cgo）；wails `WindowShow` 本身隐含激活，可空实现 |
| `SetAutostart`（注册表 Run） | `~/Library/LaunchAgents/com.rockswang.workbuddy-wild.plist`（`RunAtLoad=true` + `ProgramArguments` 含 `--autostart`）+ `launchctl bootstrap`；检测读 plist 是否存在且路径匹配 |
| `DefaultBrowserIncognito`（注册表 UserChoice） | `defaults read com.apple.LaunchServices/com.apple.launchservices.secure LSHandlers` 解析默认浏览器 bundleId → Chrome/Edge/Firefox；无痕打开：`open -na "Google Chrome" --args --incognito <url>`（Edge 用 `--inprivate`，Firefox 用 `-private-window`） |
| `LaunchIncognito`（exec chrome --incognito） | `exec.Command("open","-na",app,"--args",flag,url)` |
| `OpenURL`（rundll32 url.dll） | `open <url>` |
| `OpenWithNotepad` | `open -a TextEdit <file>`（或 `open <file>` 走默认关联） |
| `InfoBox`/`AskYesNo`（MessageBoxW） | **优先改用 wails `runtime.MessageDialog`（跨平台，一处实现）**；或 `osascript -e 'display dialog "…" buttons {"…","…"}'` |
| `KillOrphanWebViews`（PowerShell 清孤儿） | **不需要**：macOS 用系统 WKWebView，无独立进程残留/锁 profile 问题；空实现 |

### 6.3 systray 行为差异（macOS）

- macOS 菜单栏图标**点击即显示原生菜单**，没有 Windows 的"单击/右击"区分，`SetOnClick` 语义不同。
- 现设计"单击/双击/右击皆弹面板"在 macOS 需要改为：**保留原生菜单**（打开面板 / 退出），菜单项回调仍用 `item.Click(...)`（energye/systray 跨平台 API 相同）。
- 菜单栏图标用**模板图**（黑色 + alpha，含 @2x），生成尺寸 16/18px；`genicon` 需扩展输出 `.icns`（macOS 构建必需，wails 打包用 `build/appicon.icns`）。

### 6.4 wails macOS 构建与分发

```bash
# 必须在一台 macOS 上构建（wails v2 的 darwin 打包依赖 macOS，不可交叉编译）
wails build -platform darwin/universal -skipbindings   # 或 darwin/arm64 / darwin/amd64
# 需要 build/darwin/Info.plist + build/appicon.icns（genicon 生成）
# 产物：build/bin/workbuddy-wild.app（目录）
```

- 无 WebView2：用系统 WKWebView，无需额外运行时（比 Windows 简单）。
- 分发：压缩为 `.dmg` 或 zip；**未签名/未公证的 app 会被 Gatekeeper 拦截**（用户需"右键→打开"或 `xattr -cr`），发布前建议签名+公证。
- CI：`.github/workflows/release.yml` 目前只构建 windows；加 macOS 需加 `macos-latest` job。

### 6.5 其他平台注意

- Linux：托盘走 systray 的 AppIndicator 依赖；面板定位按 GTK 工作区；开机自启用 `~/.config/autostart/*.desktop`；无头服务器（`cmd/server`）完全跨平台，可作为各平台基础。
- 登录浏览器探测、开机自启、对话框三类能力是平台差异的全部；其余逻辑零改动。

## 7. 常见坑（Agent 实操作业必读）

1. **GUI 测试需要真实交互桌面**：SSH/服务会话（Session 0）无桌面 → WebView2 创建失败 → go-webview2 `errorCallback` **直接 `os.Exit(1)`**，defer/recover 都救不回来（HTTP 服务也一并被杀）。测试 GUI 用 schtasks /IT 拉到用户会话，或直接信任 `cmd/server` 无头路径。
2. **强杀进程 → 孤儿 WebView2 锁 profile**：表现为下次启动假死/白窗口。已内置 `SingletonLock` 检测 + `KillOrphanWebViews` 自动清理；若仍异常，手动结束 `msedgewebview2.exe`（按命令行含 `data/webview` 过滤，勿动其他应用的 webview）。
3. **`os.IsNotExist` 不穿透 `fmt.Errorf("%w")`**：判断配置文件缺失必须 `errors.Is(err, fs.ErrNotExist)`（踩过：首次运行静默退出）。
4. **`role=developer` 会被上游误杀**：`normalizeRoles` 勿回退；新增任何"角色改写"逻辑前先读 §2 不变量 1。
5. **Windows git-bash 环境怪癖**：curl 用 `C:/Windows/System32/curl.exe`（mingw 版 `-w` 会报错）；`schtasks /Create` 等带 `/X` 参数需 `MSYS_NO_PATHCONV=1`；`taskkill` 同理；`schtasks /Run` 在个别环境可能静默不执行任务（改用 `cmd /c start` 或直接跑）。
6. **中文输出乱码**：Python 脚本先 `set PYTHONIOENCODING=utf-8`；git-bash 直接 print 中文会乱码。
7. **`config.example.json` 与 `config.Default()` 必须同步**（默认 `127.0.0.1:7863` + `api_key: WorkBuddy2API`）。

## 8. 配置项速查

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen.host` / `listen.port` | `127.0.0.1` / `7863` | 监听主机/端口（面板可热切换） |
| `api_key` | `WorkBuddy2API` | 客户端 Bearer 密钥；空 = 不鉴权 |
| `auth_dir` / `state_file` | `./auths` / `./data/state.json` | 相对 exe 目录 |
| `region` | `cn` | 上游区域（仅 cn） |
| `schedule.checkin_hours` | `[9,21]` | 每日签到整点（面板可改，即时生效） |
| `schedule.keepalive_hours` | `[22]` | token 保活时间 |
| `cooldown.*` | 12h/60s/5/10m | 余额不足/429/连续错误 冷却 |
| `upstream.timeout_seconds` | `120` | 聊天超时（账单接口固定 30s） |

环境变量覆盖：`WB2A_LISTEN / WB2A_API_KEY / WB2A_AUTH_DIR / WB2A_STATE_FILE / WB2A_REGION / WB2A_*`。
