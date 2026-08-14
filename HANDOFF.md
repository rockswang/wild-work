# WorkBuddy2API 交接文档

## 交接目标

**已达成**：Windows 用户不依赖 Go/Python/Docker/Bash，双击单 exe 即可新增 WorkBuddy/CodeBuddy 账号（无痕浏览器登录）、配置自动签到、查看签到状态与积分，并正常使用 OpenAI 兼容接口。

## 项目概况

项目根目录：`D:/work/wbapi`

这是一个 Go 编写的非官方 OpenAI 兼容代理，已改造成 **Windows 托盘 GUI 应用**（Wails v2 + systray 单进程）：

- 对外提供：`/v1/models`、`/v1/chat/completions`、`/status`、`/healthz`
- 内部维护多个 WorkBuddy/CodeBuddy 账号
- 按余额轮转账号
- 支持流式/非流式响应
- 支持 token 刷新、错误冷却、余额查询、定时签到
- GUI：托盘图标左键弹出 webview 面板，支持添加账号（无痕浏览器登录）、配置签到时间、查看签到状态与积分、修改 API 监听主机端口、开机自启

**目标用户零依赖**：无需 Go/Python/Docker/Bash，双击 `build/bin/workbuddy2api.exe` 即用（需 Win10 21H2+ 或自动装 WebView2）。

## 已确认的上游接口

源码实际使用的 CN 上游为：

- Chat：`POST https://copilot.tencent.com/v2/chat/completions`
- 登录起始：`POST /v2/plugin/auth/state?platform=CLI`
- 登录换 token：`GET /v2/plugin/auth/token?state=...`
- 查询账号：`GET /v2/plugin/login/account?state=...`
- 刷新 token：`POST /v2/plugin/auth/token/refresh`
- 动态模型：`GET /console/enterprises/personal/models`
- 余额：`POST /v2/billing/meter/get-user-resource`
- 签到：`POST /v2/billing/meter/daily-checkin`

聊天请求由 `internal/upstream/client.go::ChatStream()` 发出；请求体由 `internal/upstream/payload.go` 强制改为 `stream=true`，并处理 `tool_choice`。

认证主要使用：

- `Authorization: Bearer <accessToken>`
- `X-User-Id`
- 可选 `X-Enterprise-Id`、`X-Tenant-Id`、`X-Domain`
- CN 的 `Origin/Referer` 为 CodeBuddy 域名

## 与网页版抓包的结论

用户提供的抓包文件：`docs/workbuddy_app.saz`

已解压到项目内：`docs/_saz_work/`。

抓包明确是网页版 `/app`，其中对话接口主要是：

- `POST /console/as/conversations/`
- `POST /console/as/conversations/<id>`
- `GET /console/as/conversations/<id>/session`
- `GET /console/enterprises/personal/models`

抓包中**没有**发现：

- `/v2/chat/completions`
- `/v2/plugin/auth/token`
- `/v2/billing/meter/daily-checkin`

因此当前判断：项目不是直接复刻网页版 `/app` 的对话协议，而是使用另一套 CLI/插件风格的 `/v2/chat/completions` 接口，再包装成 OpenAI 兼容 API。网页版和项目只部分共享账号/模型等后端体系。

## 已完成验证

源码测试：

```bash
go test ./...
```

全部通过。

曾经在本机启动临时服务并验证：

```text
GET /healthz                 -> 200 ok
GET /v1/models               -> 200，动态模型列表
POST /v1/chat/completions    -> 200，SSE 流正常返回 Hello
GET /status                  -> 能看到加载的账号
```

自动签到/余额链路也已验证：

```bash
./signin.sh <auth-dir>
```

已签到时返回 `ALREADY`，随后余额查询成功。一次验证结果为约 `2588/2600`，不要把具体账号身份或 token 写入后续文档。

`internal/scheduler/scheduler.go` 的逻辑：

- 默认本地时间每天 09:00、21:00 执行签到
- 签到后查询余额
- 有余额时解除账号冷却
- 默认 22:00 执行 token keepalive/refresh
- 禁用账号不参与签到；冷却账号仍参与签到以便恢复

临时验证文件、临时 auth 文件、临时服务和含凭据的测试文件已清理。

## Windows 现状（已解决）

原问题：用户在 Windows 无 Go 环境，login.sh/signin.sh 依赖 Bash+Go+python3+date；Docker 方案要求宿主机装 Go。

**现状**：已删除全部 sh 脚本与 Docker 文件，GUI 单 exe 内嵌登录（`internal/login`，OAuth 状态落 `data/login-state.json`）、签到（`internal/scheduler` 支持运行时改时间）、余额查询（复用 `internal/upstream`）。

代码组织：

- `main.go` — wails 入口（隐藏窗口 + 托盘 + 装配，Session 0 无桌面时捕获后无头兜底）
- `internal/app` — wails 绑定层（面板数据/操作/事件）
- `internal/config` — 配置加载/写回（`listen` 支持新对象格式与旧字符串格式）
- `internal/login` — CN OAuth 登录流程
- `internal/winutil` — 工作区/任务栏隐藏/默认浏览器无痕拉起/开机自启
- `frontend/dist` — 纯静态 HTML/JS（无 npm，直接调 `window.go`/`window.runtime`）

构建：`wails build -platform windows/amd64 -clean -skipbindings`（开发机需 Go ≥ 1.25 + wails CLI，不需 Node）。

## 推荐后续实现（已完成，保留备查）

GUI 化方案已于本次改造落地，替代原 Python/CMD 计划：托盘面板 + 无痕浏览器登录 + 运行时可配签到时间 + 积分/签到状态展示。

仍保留的独立 CLI 工具（源码构建，可选）：`cmd/server`（无头）、`cmd/login`、`cmd/signin`、`cmd/credit`、`cmd/genicon`（图标生成）。

安全红线不变：不要在文档、聊天或日志中输出 access token/refresh token；测试令牌不要复用。

## 建议技能

- `fiddler抓包分析`：继续分析 SAZ 时使用；SAZ 默认解压到当前项目 `docs/` 子目录，避免外部路径授权。
- `ast-outline`：需要继续梳理 Go 源码调用链时使用。
- `diagnose`：如果新增 Windows 工具或定时签到出现故障，按复现→定位→修复→回归测试流程处理。
- `tdd`：如果实现 Python 登录工具并要求测试，采用测试先行。
