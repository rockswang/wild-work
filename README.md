# WorkBuddy-Wild v2（代号 wild-work）

> 在 [workbuddy-wild](https://github.com/rockswang/workbuddy-wild)（v0.2.x）基础上重构的新版：
> **去掉 wails/WebView2 GUI，改为「系统托盘 daemon + 系统浏览器 Web UI」的轻量架构**。

多平台（Windows / macOS）WorkBuddy（CodeBuddy）+ TraeWork + Qoder 账号的 OpenAI 兼容代理桌面工具。
双击启动即常驻托盘，聚合多个账号为统一 OpenAI 兼容 API，按余额自动挑号、自动签到、托盘菜单一键查看/操作。

> 面向个人单机使用：不设管理端鉴权；监听 0.0.0.0 时局域网内任何设备可访问 API 与账号管理，风险由用户自行承担。

---

## 架构总览

```
┌────────────────────────────────────────────────────────────┐
│  workbuddy-wild.exe（daemon，单进程常驻托盘）                │
│                                                            │
│  ┌──────────┐   ┌──────────────────────────────────────┐   │
│  │  托盘     │   │  http.Server (单端口 127.0.0.1:7863)  │   │
│  │  systray  │   │                                      │   │
│  │  固定菜单  │   │  /v1/*          OpenAI 兼容代理       │   │
│  │          │   │  /api/*          Web UI 管理 API       │   │
│  │ 打开主界面 │   │  /              静态 Web UI(embed)    │   │
│  │ 刷新积分   │   │                                      │   │
│  │ 查看日志   │   │  ┌──────────┐ ┌──────────────────┐   │   │
│  │ 退出      │   │  │ server   │ │ app(业务编排)      │   │   │
│  └──────────┘   │  └──────────┘ └──────────────────┘   │   │
│                  │  pool / scheduler / upstream / login   │   │
│                  │  (workbuddy  +  traework 双渠道)       │   │
│                  └──────────────────────────────────────┘   │
└────────────────────────────────────────────────────────────┘
        ▲ 双击托盘 / 点菜单「打开主界面」
        │ 调系统默认浏览器
        ▼
┌──────────────────────────┐
│  Web UI（浏览器打开）      │
│  账号管理 / 签到 / 积分    │
│  渠道费率 / 配置说明       │
│  纯静态 HTML+JS，无构建链  │
└──────────────────────────┘
```

## 快速开始

1. 下载对应平台产物（Windows zip / macOS dmg 或 zip），解压后双击启动
2. 右下角托盘出现 W 图标，右键菜单可「打开主界面 / 刷新积分 / 查看日志 / 退出」
3. 浏览器打开 Web UI（默认 `http://127.0.0.1:7863/`）添加账号：
   - 选择渠道（WorkBuddy / TraeWork / Qoder）→ 自动拉起无痕浏览器登录 → 自动写凭证；
     WorkBuddy/TraeWork 立即自动签到，Qoder 当前无签到活动仅做 API 转发
4. AI 客户端指向：

   ```
   Base URL: http://127.0.0.1:7863/v1
   API Key:  WorkBuddy2API
   ```

   模型 ID 带渠道前缀：`workbuddy/<model>`、`traework/<model>`、`qoder/<model>`。

## 功能清单

- ✅ OpenAI 兼容代理：`/v1/models`、`/v1/chat/completions`（流式/非流式）、`/status`、`/healthz`
- ✅ 多渠道聚合：WorkBuddy（CodeBuddy）+ TraeWork + Qoder，模型前缀路由
- ✅ 托盘固定菜单：打开主界面 / 刷新积分（异步，完成后系统消息框通知）/ 查看日志 / 退出
- ✅ 双击托盘图标 → 系统浏览器打开 Web UI
- ✅ Web UI：多渠道多账号添加、账号管理、账号签到、积分查询、渠道费率查询、配置说明
- ✅ 自动签到（分钟级可配）、token 保活、冷却状态机、余额挑号
- ✅ 开机自启（Windows 注册表 / macOS LaunchAgent）
- ✅ 无前端编译链：纯静态 HTML/CSS/JS，`go:embed` 打进单文件

## 系统要求

| 平台 | 要求 |
|------|------|
| Windows | 10/11，无需 WebView2（不再依赖） |
| macOS | 10.15+（未签名需右键打开或 xattr -cr） |

## 目录结构

```
wild-work/
├── cmd/
│   ├── wild-work/            # daemon 入口（托盘 + HTTP + 调度）
│   │   ├── web/              # 纯静态 Web UI（go:embed）
│   │   └── build/            # 托盘图标等嵌入资产
│   └── genicon/              # 图标生成（纯 Go，无外部依赖）
├── internal/
│   ├── app/               # 业务编排：HTTP 管理 API + 托盘动作 + 登录编排
│   ├── server/            # OpenAI 兼容 handler（前缀路由到多渠道）
│   ├── pool/              # 账号池：余额挑号/冷却状态机/持久化
│   ├── scheduler/         # 定时签到/保活
│   ├── provider/          # 渠道接口（Upstream 最小接口）★扩展点
│   ├── upstream/          # WorkBuddy 上游实现
│   ├── traework/          # TraeWork SOLO 上游实现
│   ├── qoder/             # Qoder（qoder.com.cn）上游实现（COSY 签名 + QoderEncoding）
│   ├── login/             # WorkBuddy OAuth 登录编排
│   ├── login_trae/        # TraeWork 登录编排（本地回调 + PKCE）
│   ├── login_qoder/       # Qoder 登录编排（PKCE 设备授权流）
│   ├── auth/              # 凭证文件解析/原子写回
│   ├── config/            # 配置加载/写回
│   ├── systray/           # 跨平台托盘封装（固定菜单）
│   └── platform/          # 平台能力：浏览器/开机自启/消息框/日志（build tag 拆分）
├── config.example.json
├── go.mod
└── README.md / AGENTS.md
```

## 开发

- 开发者文档与约定见 [AGENTS.md](AGENTS.md)
- 旧版分析与重构决议见 [docs/](../docs/workbuddy-wild/README.md) 及本目录设计说明

## License

MIT
