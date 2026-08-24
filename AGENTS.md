# Wild-Work 项目提示词（AGENTS.md）

> 本文件面向 AI Agent 与开发者，记录**工具设计、架构选型、决议项**与开发约定。
> 重构基于 [workbuddy-wild](https://github.com/rockswang/workbuddy-wild)（v0.2.4），源码克隆在 `../docs/workbuddy-wild/`。

---

## 0. 项目一句话

去掉 wails/WebView2，改为 **系统托盘 daemon + 系统浏览器 Web UI** 的多平台（Win/mac）多渠道账号聚合工具。

## 1. 已定决议项（不要推翻，除非有强理由并更新本节）

| # | 决议 | 说明 |
|---|------|------|
| R1 | **托盘菜单固定，不做动态内容、不做定时/事件刷新** | 用户在自己客户端操作无法捕捉，动态展示无意义 |
| R2 | 托盘提供「**刷新积分**」菜单：点击后**异步**获取全部渠道全部账号积分，完成后用**系统消息框**通知用户 | 不在菜单内展示结果 |
| R3 | 托盘固定菜单项：**打开主界面 / 刷新积分 / 查看日志 / 退出** | 双击托盘 = 打开主界面；不再弹"已启动"提示框 |
| R4 | **不设管理 API 鉴权** | 单机个人工具；监听 0.0.0.0 的风险由用户承担，UI/文档给一句风险提示 |
| R5 | **Web UI 用纯静态 HTML/CSS/JS**（无前端编译链） | `go:embed` 打进单文件；实用 + 大众审美即可 |
| R6 | 托盘库：**保留 energye/systray**（已跨平台 Win/mac/Linux） | 不引入 fyne；菜单固定所以其"不可删除菜单项"的限制无影响 |
| R7 | **移除 wails / WebView2 全部依赖** | 省内存与运行时；平台能力封装进 `internal/platform`（build tag 拆分） |
| R8 | daemon 单进程：一个 `http.Server` 同时服务 OpenAI 端点 + 管理 API + 静态 UI | 沿用 server 现有 ServeMux 扩展 |
| R9 | 核心业务（pool/scheduler/upstream/traework/server/login/config/auth/provider）**整体复用**，格式零迁移 | config.json / auths/ / data/state.json 兼容旧版 |
| R10 | 新增渠道扩展方式：实现 `provider.Upstream` 接口 + auth 加载器 + 注册 Runtime | 模型前缀 `channel/<model>` 路由；已实现 qoder 渠道（qoderwork2api），deepseek2api/chatgpt2api 仍未实现但架构支持 |
| R11 | Windows 产物在 WSL 交叉编译（`GOOS=windows CGO_ENABLED=0`，已验证可行）；macOS 产物走 GitHub Actions macos-latest（cgo 必需） | WSL 无法编 darwin cgo；CI 增加 darwin job |

## 2. 架构选型（依据）

| 主题 | 选型 | 理由 |
|------|------|------|
| GUI 壳 | **无**（删除 wails） | WebView2 内存开销大 + Windows 绑定；托盘 + 浏览器足够 |
| 托盘 | energye/systray v1.0.3（现有） | 已跨平台；菜单固定方案规避其不可删菜单项限制 |
| 管理后端 | 现有 http.Server 扩展 /api/* | 单端口、复用鉴权中间件（无鉴权）、零新服务 |
| Web UI | 纯静态 embed + fetch | 无 Node 构建链，单 exe 双击即用 |
| 登录 | 复用 internal/login + login_trae | 纯 HTTP + 本地回调端口，跨平台 |
| 平台能力 | internal/platform + build tag（windows/darwin/other） | 浏览器无痕/开机自启/消息框/日志/工作区，接口同名 |
| 构建 | WSL 交叉编译 win；CI macos-latest 编 darwin | 见 R11 |

## 3. 托盘菜单设计（最终形态）

```
wild-work
──────────
打开主界面          → 系统浏览器打开 http://<listen>/（防重复实例）
刷新积分            → 异步刷新全部账号积分，完成后系统消息框汇总
查看日志            → 系统默认编辑器打开 data/app.log
──────────
退出                → 退出 daemon（确认框）
```

- 单击/双击/右击：右击弹菜单；**单击与双击 = 打开主界面**（保持与旧版一致的用户习惯）。
- 「刷新积分」回调必须 **goroutine 异步**执行（托盘消息循环线程只做投递，绝不阻塞——旧版原则保留）。
- 消息框通知内容：成功 N / 失败 M，失败账号名单（脱敏）。

## 4. Web UI 页面规划（纯静态，一个 index.html + app.js + style.css）

| 页面/区块 | 内容 |
|-----------|------|
| 概览 | 各渠道账号数、总积分、下次签到时间、API 地址、版本 |
| 账号管理 | 多渠道多账号列表（渠道分组）：昵称/UID(脱敏)/积分/签到状态/冷却/禁用；添加账号（渠道选择→无痕浏览器登录→自动签到）、单账号签到/刷新积分/删除 |
| 自动签到 | 签到时间（HH:MM 多组）、保活时间、下次触发 |
| 渠道费率 | 各渠道模型 + 费率表（上游可查部分）与说明 |
| 配置说明 | 监听地址、API-Key、开机自启、日志、风险提示（0.0.0.0） |

管理 API（REST，均挂 `/api/*`）：

```
GET  /api/state                 # 全量状态（账号/积分/签到/配置/费率）
POST /api/login/start           # {channel} → {auth_url}
POST /api/login/cancel
POST /api/account/checkin       # {uid}
POST /api/account/checkin_all
POST /api/account/refresh       # {uid}
POST /api/account/remove        # {uid}
POST /api/config/checkin_times  # {times:["09:00","21:30"]}
POST /api/config/listen         # {host,port}
POST /api/config/api_key        # {key}
POST /api/config/autostart      # {on:bool}
GET  /api/fees                  # 渠道费率说明（静态 + 可查部分）
```

## 5. 渠道扩展点（已实现 qoder；后续 deepseek2api / chatgpt2api）

1. 新建 `internal/<channel>/` 包，实现 `provider.Upstream` 接口：
   `RefreshToken / ChatStream / FetchModels / FetchModelPricing / UserResource / UserResourceDetail / DailyCheckin / Classify / Stream / Aggregate`
2. `internal/auth` 增加对应 `Load<Channel>Dir()`（文件名前缀 `<channel>-*.json`）
3. 装配处注册 `server.Runtime{Kind, Pool, Upstream, StaticModels}` + `app.Runtime{..., Scheduler}`
4. 前端渠道选择器加一项；`internal/login_<channel>` 实现登录编排（如需）

> provider.Kind 即模型名前缀；server 按 `channel/<model>` 前缀路由，无需改接口。
> Qoder 渠道无签到活动：`DailyCheckin` 直接返回错误，调度器只做 token keepalive，
> 前端签到按钮/批量签到对 qoder 账号灰掉并跳过。

## 6. 关键不变量（沿用旧版，改动前必读）

1. `PrepareBody` 三改写勿动：强制 `stream=true`、`tool_choice` 归一化、`developer→system`
2. 日志/面板/消息框**零 token**：不得输出 access/refresh token（调试用假 token）
3. auth 文件嵌套格式 `{auth:{...},account:{...}}`，`internal/auth.Parse` 与 login.SaveAuth 必须一致
4. `config.listen` 兼容新对象格式 + 旧字符串格式 `":7863"`
5. `data/state.json` 只增不减字段，向后兼容
6. 托盘回调必须 goroutine 化
7. `config.example.json` 与 `config.Default()` 同步

## 7. 平台能力差异表（internal/platform）

| 能力 | Windows | macOS |
|------|---------|-------|
| 打开浏览器 | rundll32 url.dll / 无痕探测 | `open <url>` / `open -na "Chrome" --args --incognito` |
| 系统消息框 | MessageBoxW | osascript display dialog（或后续换原生） |
| 开机自启 | 注册表 Run（`--autostart`） | ~/Library/LaunchAgents plist（RunAtLoad + --autostart） |
| 打开日志文件 | notepad | open -a TextEdit |
| 确认框 | MessageBoxW YESNO | osascript buttons |
| 无头兜底 | HTTP+调度器继续跑 | 同左 |

## 8. 构建与验证（本机 WSL）

```bash
# 环境
export PATH=$PATH:/usr/local/go/bin
export https_proxy=http://10.126.126.1:10809 http_proxy=http://10.126.126.1:10809   # 拉依赖才需要

# 全量校验（Linux 下跑业务逻辑测试）
go build ./... && go vet ./... && go test ./...

# 交叉编译 Windows（已验证可行，纯 Go 无 cgo）
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o dist/wild-work.exe ./cmd/wild-work

# macOS 只能 CI（macos-latest）构建
```

## 9. 风险与待办

- [ ] 托盘图标跨平台资产（.ico / .icns 模板图）
- [ ] macOS 未签名/未公证分发说明（Gatekeeper）
- [ ] CI release.yml 增加 macOS job
- [ ] 浏览器「打开主界面」与 daemon 单实例锁（防重复启动）
- [ ] 监听 0.0.0.0 时 Web UI 管理 API 暴露风险提示文案
