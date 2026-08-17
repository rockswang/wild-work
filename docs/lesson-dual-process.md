# 双进程改造失败经验教训（docs/lesson-dual-process.md）

> 状态：**已回退**。本文记录 2026-08-17 尝试将 WorkBuddy2API 从单进程改为双进程（主进程 proxy + 独立 GUI 进程）的完整失败过程、根因分析和经验教训，避免后续重复踩坑。
> 结论：**放弃双进程，恢复单进程（c70230b 可用版本），仅保留纵向高度自适应优化。**

---

## 1. 目标与动机（为什么想改双进程）

原单进程架构：一个 exe 内同时跑 HTTP proxy + scheduler + systray + Wails GUI（WebView2）。

问题：
1. **常驻内存高**：进程启动即创建 WebView2（`StartHidden` 只是隐藏窗口，`chromium.Embed` 在启动时就创建了 WebView2 环境/controller，msedgewebview2.exe 全家桶常驻，~150-250MB）。
2. **托盘卡死**：长时间运行后单击/双击/右击托盘无反应（systray 与 wails 消息循环共存问题）。

双进程设想：主进程只跑 proxy + systray（无 WebView2，~15MB）；点托盘 → `exec` 启动独立 GUI 进程（WebView2 按需创建）；GUI 关闭即退出。

## 2. 尝试过程

### 阶段 1：同 exe + `--panel` 参数分支
- `main.go` 里 `--panel` 参数进入面板逻辑，否则主进程逻辑。
- 结果：**面板进程第一次启动窗口不显示**；第二次点击（SingleInstanceLock 唤起）才显示。

### 阶段 2：`--panel` 分支 + 各种"修复"
- 加了 `runtime.LockOSThread`、StartHidden 切换、OnDomReady 显示逻辑、ctxReadyCh 等待、失焦看门狗、托盘看门狗、`GetForegroundWindow` 探测……全部无效，反而引入更多问题。

### 阶段 3：两个独立 exe（用户建议）
- `workbuddy-wild.exe`（主进程，纯 Go 无 wails）+ `workbuddy-panel.exe`（独立 wails 应用，build tag `panel`）。
- 结果：面板进程 `StartHidden: false` 后窗口仍不显示。

### 用户实测反馈
- 第一次点击托盘：光标闪烁，界面不出现；再次点击才出现。
- 交互几次后：托盘图标在，但无任何反应（卡死）。
- 越改越复杂，用户明确要求踩刹车。

## 3. 根因分析（关键）

### 3.1 面板进程窗口不显示的直接原因
`CreateCoreWebView2Controller(parentWindow)` 返回 **80070578 = ERROR_INVALID_WINDOW_HANDLE**：
```
[WebView2] Environment created successfully
→ CreateCoreWebView2ControllerCompleted → errorCallback
→ error creating controller with 80070578: Invalid window handle
→ os.Exit(1)（go-webview2 的 errorCallback 直接退出进程）
```

- `chromium.Embed(f.mainWindow.Handle())` 传入的窗口句柄无效/已销毁。
- go-webview2 的 `errorCallback` 直接 `os.Exit(1)`，进程静默退出（无日志），表现就是"窗口不显示"。
- 部分情况下（schtasks 启动）日志显示 Environment created + OnStartup 但窗口 `MainWindowHandle=0` 且 `EnumWindows` 找不到任何窗口——**窗口创建后立即被销毁**。

### 3.2 可能的深层原因
1. **COM 线程模型**：`go-webview2/corewebview2.go` 的 `init()` 里 `runtime.LockOSThread()` + `CoInitializeEx(COINIT_APARTMENTTHREADED)`。WebView2 的 COM 对象是 STA，环境/controller 创建回调必须与 init 在同一 OS 线程。双进程（尤其 build tag 方式）下 Go 的 init 线程与后续 wails.Run 线程可能不一致 → controller 回调丢失/失败。
2. **`HideWindow` 影响**：`exec.Command(...).SysProcAttr{HideWindow: true}` 设置 STARTUPINFO.wShowWindow=SW_HIDE，可能影响 GUI 子进程窗口首次显示（已移除，但未能验证是否根因）。
3. **systray 与 wails 消息循环**：energye/systray 的 `nativeLoop`（GetMessage）与 wails 的 `RunMainLoop` 是两个线程级消息泵，Go 调度器可能把它们调度到同一 OS 线程 → 消息互相干扰 → 托盘卡死。这是**单进程也存在的隐患**（用户最初报告的问题 2），双进程想绕开，但主进程 systray 单独跑后仍出现"交互几次卡死"。

### 3.3 测试环境陷阱（重要）
- 本 agent 运行在 **Session 0（Services 会话，无交互桌面）**，WebView2 无法创建窗口（`error creating controller 80070578`）。
- `schtasks /Run` 虽能启动到 Session 1，但 schtasks 启动的进程**不继承用户交互桌面上下文**，窗口创建行为与用户真实双击不同。
- **结论：GUI 显示问题无法在 agent 环境可靠验证，必须依赖用户真实桌面测试。** 这导致反复"我改了 → 用户测 → 还不行"的低效循环。

## 4. 关键经验教训

1. **验证环境受限时，不要盲目迭代 GUI 问题**：Session 0 下无法验证 WebView2，每次"修复"都是猜。应先确认验证手段，或让用户一次提供完整日志（app.log 会显示 `error creating controller` 堆栈）。
2. **同一个 exe 同时跑 systray + wails 是可行但脆弱的**（消息循环共存），任何"优化"都要先确认不破坏窗口创建。
3. **wails v2 的 WebView2 创建是"进程启动即发生"**（`StartHidden` 只隐藏窗口不延迟创建 controller），想"常驻低内存 + 按需创建 WebView2"在 wails v2 原生架构下**没有干净的方案**：
   - 同进程延迟 `wails.Run`：wails 单次生命周期，二次 Run 有状态残留风险。
   - 双进程：面板进程的 controller 创建在独立 exe 下仍失败（COM 线程/窗口句柄问题）。
   - fork wails 改 Embed 延迟：维护成本高。
4. **过度工程是敌人**：本功能本质是"托盘点击 → 启动子进程 → 显示窗口 → 收起退出"。为了这个加了 LockOSThread、看门狗、失焦探测、去抖、ctxReadyCh、build tag 双入口……每一项都增加了失败面。用户明确说"这有任何超出一个普通 go 程序的行为吗？"——**应该保持朴素**。
5. **双进程状态同步（文件共享）本身是可行的**（pool.Reload/saveLocked 增量合并），但这是"另一层复杂"，在 GUI 都显示不了时毫无意义。

## 5. 最终决策

- **放弃双进程**，恢复单进程可用版本（`c70230b`）。
- 仅保留本次改造中有价值且已验证的优化：**前端纵向高度自适应**（账号列表随账号数伸缩、>4 个内部滚动）。
- 常驻内存问题、托盘卡死问题**暂缓**（记录为已知问题，后续单独攻关，不在 GUI 显示不稳定的前提下叠加改动）。

## 6. 保留的有价值资产（不随回退丢失）

- `docs/trae-integration.md`：traework 上游接入调研文档（完整、可复用）。
- `docs/Trae2api-cn/`、`docs/traework2api/`：两个参考项目源码副本。
- 前端 `AccountView.Group` 字段 + 平台图标样式（workbuddy/traework 区分）——**这部分保留**。
- 面板高度自适应逻辑（`panelRect` 基础高度 + 每账号增量 + 封顶）——**保留**。

## 7. 回退清单

- [x] `main.go` 恢复单进程（c70230b 结构：main → runGUI → wails.Run 阻塞）
- [x] 删除 `panel.go`、`panel_entry.go`、`temp_test_panel.go`、build tag
- [x] `winutil.go` 保留 `LaunchPanel`/`SystrayResponsive`/`TrayWatchdog` 等？——**不需要的删除**，保持单进程原有工具集
- [x] `app.go` 保留界面高度自适应（panelRect），删除失焦看门狗等
- [x] `frontend/dist` 保留两行紧凑账号卡片 + 平台图标 + 列表滚动
- [x] 构建验证单 exe

---

*撰写时间：2026-08-17。动机：双进程改造多次失败且越改越复杂，需沉淀教训防止重蹈覆辙。*
