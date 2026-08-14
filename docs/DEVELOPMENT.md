# WorkBuddy Wild — 开发者文档

面向开发者的底层说明；普通用户请看 [README](../README.md)。

## 目录结构

```
workbuddy-wild.exe     # 主程序（wails + systray 单进程，Windows amd64）
config.json            # 配置（首次运行自动生成，也可手写）
auths/                 # 账号凭证 workbuddy-<uid>.json（登录后自动生成，勿外泄）
data/                  # state.json（冷却/签到状态）、app.log（日志）、login-state.json
build/                 # 构建资产：trayicon.ico / windows/icon.ico（cmd/genicon 生成）
frontend/dist/         # 纯静态前端（HTML/JS/CSS，无 npm 构建）
internal/
  app/                 # wails 绑定层：面板数据/操作/事件/端口热切换/登录编排
  config/              # 配置加载与原子写回（listen 兼容新旧格式）
  login/               # CN OAuth 登录流程（含登录页跳转链解析）
  winutil/             # Win32 工具：任务栏定位/无痕浏览器/开机自启/MessageBox
  auth/ pool/ scheduler/ upstream/ server/   # 核心业务（纯 Go，跨平台）
cmd/
  server/ login/ signin/ credit/   # 无头 CLI 工具（可选）
  genicon/             # 图标生成工具（构建期）
```

## 架构

单进程：HTTP 服务（OpenAI 兼容代理）+ 定时签到调度器 + 托盘管理面板。

```
托盘图标(单击/双击/右击→面板)
   │  wails 绑定（internal/app）
   ├─ HTTP 服务 :7863  ── /v1/chat/completions /v1/models /status /healthz
   ├─ 调度器 ── 每日签到(默认 9/21 点) + token 保活(22 点) + 冷却解冻
   └─ 面板数据 ── 账号状态/积分/签到记录（state.json 持久化）
```

- 请求处理：`pool` 按余额挑号 → `upstream.ChatStream`（强制 `stream=true`，CN 上游 `copilot.tencent.com/v2/chat/completions`）
- 错误分类：余额不足 402/关键词 → 12h 硬冷却；429 → 60s；连续错误 → 10m；session 失效(12153) → 禁用
- 调度器支持运行时改签到时间（`SetCheckinHours` + wake 通道），配置写回 `config.json`

## 配置项

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen.host` | `127.0.0.1` | API 监听主机；`0.0.0.0`=全部接口（面板可选/自定义） |
| `listen.port` | `7863` | API 端口 |
| `api_key` | `WorkBuddy2API` | 客户端 Bearer 密钥；空 = 不鉴权 |
| `auth_dir` | `./auths` | 账号凭证目录 |
| `state_file` | `./data/state.json` | 冷却/签到状态持久化 |
| `region` | `cn` | 上游区域（仅 cn） |
| `schedule.checkin_hours` | `[9,21]` | 每日自动签到整点（面板可改，即时生效） |
| `schedule.keepalive_hours` | `[22]` | token 保活时间（自动） |
| `cooldown.hard_credit` | `12h` | 余额不足冷却 |
| `cooldown.soft_rate` | `60s` | 429 冷却 |
| `cooldown.err_threshold` / `err_cooldown` | `5` / `10m` | 连续错误冷却 |
| `upstream.timeout_seconds` | `120` | 上游聊天超时（账单接口固定 30s 短超时） |

兼容旧格式：`"listen": ":7863"` 字符串写法仍可解析。
环境变量覆盖：`WB2A_LISTEN / WB2A_API_KEY / WB2A_AUTH_DIR / WB2A_STATE_FILE / WB2A_REGION / WB2A_*`（同名字段）。

## 构建（开发机）

需要 Go ≥ 1.25 与 wails CLI，**不需要 Node**（前端纯静态）：

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@v2.14.0
go run ./cmd/genicon          # 生成 build/trayicon.ico、build/windows/icon.ico
wails build -platform windows/amd64 -clean -skipbindings
# 产物：build/bin/workbuddy-wild.exe
```

`wails build` 默认即 production 模式（Devtools=false、资源内嵌）。

## 命令行工具（可选，源码构建）

仅面向高级用户/无头场景，无任何脚本依赖：

| 命令 | 说明 |
|---|---|
| `go run ./cmd/server` | 无头模式启动 HTTP 服务 + 调度器（不用 GUI） |
| `go run ./cmd/login url\|poll` | 手动 OAuth 登录流程（state 落 data/login-state.json） |
| `go run ./cmd/signin <auth-dir>` | 批量签到 |
| `go run ./cmd/credit [-pretty]` | 积分汇总 JSON / 日报 |
| `go run ./cmd/genicon` | 重新生成图标资产 |

## 上游接口（CN）

| 用途 | 端点 |
|---|---|
| 登录发起 | `POST copilot.tencent.com/v2/plugin/auth/state?platform=CLI` |
| 登录轮询 | `GET /v2/plugin/auth/token?state=...` |
| 账号信息 | `GET /v2/plugin/login/account?state=...` |
| 刷新 token | `POST /v2/plugin/auth/token/refresh`（头带 X-Refresh-Token） |
| 动态模型 | `GET /console/enterprises/personal/models` |
| 余额 | `POST www.codebuddy.cn/v2/billing/meter/get-user-resource` |
| 签到 | `POST /v2/billing/meter/daily-checkin` |
| 聊天 | `POST copilot.tencent.com/v2/chat/completions`（强制 stream） |

登录页存在跳转链（`copilot.tencent.com/login → 301 加斜杠 → www.codebuddy.cn/login`），GUI 用 `internal/login.ResolveAuthURL` 预解析直达最终地址；其余 API 端点均无跨域跳转，Go 客户端自动跟随。

## 已知限制

- **仅 Windows amd64**：`internal/winutil` 依赖 Win32（注册表/user32/无痕浏览器探测/开机自启），Linux/macOS 需按 build tag 拆实现
- 需要交互式桌面会话（远程桌面正常；SSH/服务会话无桌面时 WebView2 无法创建，GUI 不可用）
- WebView2 运行时：Win10 21H2+ 自带；更早版本首次运行会自动安装（需联网）
- 登录凭据只落盘 `auths/`，面板与日志不输出 token
- 强杀进程可能残留孤儿 WebView2 进程锁住用户数据目录，导致下次启动变慢；正常退出（面板"退出"）无此问题
- 开机自启注册表项指向 exe 绝对路径，exe 移动后需重新开启
