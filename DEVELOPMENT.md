# wild-work 开发者文档（面向 AI Agent）

> 本文件为**代码维护者（含 AI Agent）**而写，供新 Agent 快速接手二次开发。
> 普通用户请看 [README.md](README.md)，项目决议见 [AGENTS.md](AGENTS.md)，交接记录见 [HANDOFF.md](HANDOFF.md)。

## 0. 项目速览

- **是什么**：多平台（Win/mac/Linux）系统托盘 daemon + 浏览器 Web UI 的多渠道账号聚合工具。单进程 = HTTP 代理 + 调度器 + 托盘 + 静态 Web UI。
- **技术栈**：Go 1.25+ / energye/systray（托盘）/ 纯静态前端 HTML/CSS/JS（go:embed 无构建链）/ 无 wails/WebView2。
- **模块**：`wild-work`（go.mod module name）。
- **平台**：Windows（完整）、macOS（代码已写，需 cgo 编译）、Linux（无头模式 `--no-tray`）。
- **数据文件**：`config.json`（配置）、`auths/<kind>-*.json`（凭证，勿外泄）、`data/state-<kind>.json`（冷却/签到状态）、`data/app.log`（日志）、`data/pricing-cache.json`（费率缓存）。

## 1. 代码地图

```
cmd/wild-work/main.go         # daemon 入口：装配各渠道 Runtime → 启动 HTTP → 调度器 → 托盘/无头
cmd/wild-work/web/             # 纯静态 Web UI（index.html / app.js / style.css）
cmd/genicon/                   # 图标生成（纯 Go）
internal/
├── app/app.go                 # 业务编排：HTTP 管理 API + 登录流程 + 费率缓存 + 日志
├── app/session.go             # 管理面板鉴权：cookie 会话 + 管理员密码 + 登录限流
├── server/handler.go          # OpenAI 兼容 HTTP handler：前缀路由 + 挑号 + 错误透传（含 SingleAccount 豁免）
├── pool/pool.go               # 账号池：余额挑号 + 冷却/禁用状态机 + state.json 持久化
├── scheduler/scheduler.go     # 定时签到 + token 保活 + 冷却解冻
├── provider/provider.go       # Upstream 接口 + 共享类型（ModelInfo/ModelPricing/ResourceItem）
├── upstream/                   # WorkBuddy(CodeBuddy) 上游：chat/billing/auth/模型/定价/脱敏
├── traework/                   # TraeWork/TraeCode 上游：chat(SOLO)/billing/checkin/模型/定价（TraeCode=NewTraeCode()，同上游不同 function）
├── qoder/                      # 旧 Qoder(QoderWork) 上游：已下线，路由保留
├── qodercn/                    # QoderCN 上游：qoder2api 参数形态（cosyVersion 1.0.10 / campaigns 签到）
├── qodercom/                   # QoderCOM 国际版上游：三域分离（openapi/api1/api2.qoder.sh）
├── qwenwork/                   # 千问办公上游（gateway.qwenwork.cn）
├── oczen/                      # OpenCodeZen 匿名免费通道：无账号（Bearer public）+ 三道闸门构造
├── login/                      # WorkBuddy OAuth 登录编排
├── login_trae/                 # TraeWork 登录编排（PKCE + 回调轮询）
├── login_qoder/                # 旧 Qoder 登录编排（已下线）
├── login_qodercn/              # QoderCN 登录编排（设备流，client_id e883ade2）
├── login_qodercom/             # QoderCOM 登录编排（设备流，授权页 qoder.com）
├── auth/auth.go                # 凭证文件解析（嵌套/扁平双形态）+ 原子写回
├── config/config.go            # 配置加载/校验/写回（listen 新旧格式兼容）
├── systray/systray.go          # 跨平台托盘：固定菜单 + 纯 Go 生成图标
├── sanitize/                   # 出站请求体指纹脱敏：清除 Claude Code / Codex 模板句
```

## 2. 关键不变量（改了会出事）

1. **`PrepareBody` 三改写勿动**：强制 `stream=true`、`tool_choice` 归一化、`role=developer→system`——各渠道各自的 `PrepareBody` 均需保持。
   例外：`oczen` 的强制 `stream=true` 是上游闸门要求（非兼容便利），且 `tool_choice` 只允许在「客户端未带 tools」时置 `none`，否则会破坏客户端的工具调用。
2. **日志/面板零 token**：任何输出不得含 access token / refresh token / GitHub token。
3. **auth 文件格式**：嵌套形 `{auth:{...},account:{...}}`，`internal/auth.Parse` 与各 login.SaveAuth 写入必须一致。新增字段必须同时加入 Parse 和 SaveAtomic。
4. **config.listen 兼容**：新对象格式 `{"host","port"}` + 旧字符串格式 `":7863"` 都要能解析。
5. **state.json 向后兼容**：只增不减字段，旧文件缺失字段按零值处理。
6. **托盘回调必须 goroutine 化**：systray 消息循环线程上禁止阻塞。
7. **`CGO_ENABLED=0`**：Windows 交叉编译必须用此标志（纯 Go 无 cgo 依赖）。
8. **定价缓存持久化**：`data/pricing-cache.json`，启动时加载，超过 1 小时自动刷新。
9. **上游错误透传**：HTTP ≥400 时直接透传原始响应体，不在 server 层包装，冷却状态机仍正常运转。
10. **Classify 429 优先于 hardMarkers**：三渠道 `Classify` 均须先判 `status==429` 再扫余额关键词；顺序反置会导致 429 + "quota exceeded" 误判硬冷却 12h。
11. **脱敏层预检零分配**：`internal/sanitize` 的 `hasFingerprint` 先走 `strings.Contains` 特征快速路径，普通请求不命中即原样返回，不做 JSON Unmarshal。
12. **RefreshHeaders 直接读 RefreshToken**：该函数调用方已持有 `a.Lock()`，不能走 `a.RefreshTokenValue()`（会死锁）。其余 API 头用 `a.AccessTokenValue()` 锁快照。
13. **管理面板鉴权不变量**：
    - `admin_password` 为空 ⇒ 只允许监听环回地址（启动层 fatal + `SetListen`/`SetAdminPassword` 双向把关），不得只在一处校验；
    - `/api/auth/*`（登录/登出/探针）**不得**加会话守卫，其余 `/api/*` 一律经 `App.guardMux`；
    - 前端不读 cookie（HttpOnly），登录态判断靠 `/api/auth/state` 回传的 `auth_session`（口令指纹，改用 SHA-256(密码) 前 8 字节）；
    - OpenAI/兼容接口的 `api_key` 与面板密码是两套凭据，不得互换或复用；
    - **设置弹层的监听警示必须跟着「当前选中的地址」实时联动**，不得写成静态常显：
      `syncListenRisk()` 是唯一入口（下拉框 onchange / 自定义输入框 input / 打开弹层时各调一次），
      同时控制黄色警告条（`#listenRiskTip`）与管理密码的必填星号（`#adminPassReq`）。
      判定口径须与后端 `config.Listen.IsLoopback` 一致（显式环回才安全，空主机名=对外暴露）。

## 3. 渠道上游接口

### WorkBuddy（CodeBuddy）

| 用途 | 端点 | 鉴权 |
|------|------|------|
| 刷新 token | `POST {chatBase}/v2/plugin/auth/token/refresh` | X-Refresh-Token |
| 聊天 | `POST {chatBase}/v2/chat/completions` | Bearer |
| 动态模型 | `GET {chatBase}/console/enterprises/personal/models` | Bearer |
| 余额 | `POST {billingBase}/v2/billing/meter/get-user-resource` | Bearer |
| 签到 | `POST {billingBase}/v2/billing/meter/daily-checkin` | Bearer |
| 登录 | `POST {chatBase}/v2/plugin/auth/state` → 轮询 `/v2/plugin/auth/token` | 无 |

CN: chatBase=`copilot.tencent.com`, billingBase=`www.codebuddy.cn`
Global: chatBase=`www.workbuddy.ai`, billingBase=`www.workbuddy.ai`

模型定价：`credits` 字段（字符串 `"x0.79 credits"`）→ `parseCredits()` 解析。

### TraeWork（Trae SOLO）

| 用途 | 端点 | 鉴权 |
|------|------|------|
| 刷新 token | `POST {oauthBase}/cloudide/api/v3/trae/oauth/ExchangeToken` | OAuth 头 |
| 聊天 | `POST {agentBase}/api/agent/v3/llm_utils_chat` | SOLOHeaders（Cloud-IDE-JWT） |
| 模型列表 | `POST {agentBase}/api/ide/v1/get_detail_param` | SOLOHeaders |
| 余额 | `POST {ugBase}/trae/api/v2/pay/web_user_ent_usage` | UgHeaders |
| 签到 | `POST {ugBase}/trae/api/v2/ug/checkin_credits/status` + `/claim` | UgHeaders |
| 登录 | PKCE + 回调端口轮询 | 无 |

Agent: `trae-api-cn.mchost.guru`, UG: `api.trae.cn`, OAuth: `api.trae.com.cn`

**TraeCode（`traecode/*`）**：同一上游的 `function=solo_agent`（TraeWork 是 `solo_work_lite`），
账号体系与签到调度完全共享（`traework.NewTraeCode()`），仅模型集与定价分组不同。

模型定价：`GET work.trae.cn/api/remote/v1/models`，`features.consumption_rate.rate`（JSON 字符串需二次解析），discount 优先；
TraeWork/TraeCode 分组去重按主 function 优先（同一模型在 `solo_agent` 与 `_remote` 下倍率可能不同）。

**积分可用性判据（R19，2026-09-23 更新）**：`ep==1 || product_id==209` 不可用。
上游已不再下发 ep=1，200 档每日签到（pid=209）仅靠 product_id 识别；208（150 签到）/221（每月登录）均可消耗。

### Qoder 系（qodercn / qodercom）

| 用途 | QoderCN 端点 | QoderCOM 端点 | 鉴权 |
|------|------|------|------|
| 刷新 token | `POST {base}/api/v1/deviceToken/refresh` | 同左 | refresh_token (drt-) |
| 聊天 | `POST {gateway}/algo/api/v2/service/pro/sse/agent_chat_generation?...AgentId=agent_common` | gateway=`api1.qoder.sh` | COSY 签名 + dt- |
| 模型列表 | `GET {models}/algo/api/v2/model/list?Encode=1` | models=`api2.qoder.sh`（双域分离） | COSY 签名 |
| 余额 | `GET {base}/api/v2/quota/usage` | 同左 | dt- Bearer |
| 签到 | `GET/POST {base}/sash/api/v1/me/campaigns[/{id}/claim]`（仅此路径） | 同左 | dt- + cosy-clienttype:10 |
| 登录 | OAuth 设备流（PKCE+S256） | 同左（授权页 qoder.com） | 无 |

Base: CN `openapi.qoder.com.cn`+`gateway.qoder.com.cn`；COM `openapi.qoder.sh`+`api1.qoder.sh`+`api2.qoder.sh`

协议要点（两区同源，代码级复制）：COSY 签名 cosyVersion=**1.0.10**、18 头（含
`cosy-scene:assistant`/`cosy-business-product:ide`/`cosy-business-type:agent`，无 cosy-clientip）；
identity.userType 从 `/api/v1/userinfo` 实测回填；请求体 `session_type:"qoder"`、
`parameters.max_tokens`（默认 32768）、`model_config.source:"system"`（思考总开关）；
消息体经 `qoderEncode()` 编码，SSE 嵌套格式（`data:{"body":"<json>"}`）。
模型表无静态兑底：上次成功拉取作进程内缓存；场景解析 assistant→developer→chat 三级回退。
签到必须 `cosy-clienttype: 10`（桌面端），与推理链路的 5 不同；活动 campaignKey 每日变化不可硬码。
**绝不用 legacy `daily-check-in/claim`**：该端点已 DISABLED 却对未领取日恒返 409，走它会造成
「假成功零积分」（上游 qoder2api 99ab022 同款结论，2026-09-21 抓包实测）。
签到窗口：每日 10:00（UTC+8）开放，10:00–12:00 窗口内每分钟重试（活动可能在整点后才创建），
结果经 `provider.CheckinReporter` 结构化上报，全账号达 claimed/already 才算当日完成（见 AGENTS.md 不变量 23）。
CN 凭据在国际端点 401（双向隔离），两渠道凭据文件前缀 `qodercn-`/`qodercom-`。

模型定价：`price_factor` 字段（数字）。

思考开关：`buildAgentBody` 的 `is_reasoning` 参数由 `reasoning_effort`/`thinking` 请求参数动态控制。

### OpenCodeZen（oczen，匿名免费）

| 用途 | 端点 | 鉴权 |
|------|------|------|
| 聊天 | `POST https://opencode.ai/zen/v1/chat/completions` | `Bearer public`（字面量，匿名） |
| 模型列表 | `GET https://opencode.ai/zen/v1/models` | 同上 |

**无刷新/无余额/无签到**：匿名凭证是常量，`RefreshToken` 为空实现，`UserResource*` 恒 0，
`DailyCheckin` 返回「无签到活动」（调度器配置为 `CheckinMinutes/KeepaliveHours` 均 nil，不会调用）。

三道闸门（缺一即 403 FreeTierError，详见渠道备忘）：
1. `x-opencode-session` 必须是 `ses_<12位小写hex><14位Base62>`（由对话首轮哈希稳定派生）；
2. 请求体必须 `stream:true` 且 `tools` 内同含 `bash`/`read` function（缺则注入桩工具；
   客户端无工具时同时置 `tool_choice:"none"`，有工具时保留其 `tool_choice`）；
3. 伪装头齐套：`User-Agent: opencode/1.18.x`、`x-opencode-client: cli`、
   `x-session-affinity`/`X-Session-Id`（同会话值）、`x-opencode-request`、`x-opencode-project`。

模型暴露：只保留 ID 含 `free` 或恰为 `big-pickle` 的模型（地域受限的也保留）；
上游不可达时回静态清单（`internal/oczen/free.go`）。定价恒为 `Rate=0, Explicit=true`。
model 字段回填：`Aggregate` 直接改字段；`Stream` 用 `modelRewriter` 逐行替换。

## 4. 渠道扩展点

新增渠道一般只需三步：

1. 新建 `internal/<channel>/` 包，实现 `provider.Upstream` 接口
2. 新建 `internal/login_<channel>/` 包，实现登录编排
3. 在 `cmd/wild-work/main.go` 装配处注册 Runtime

> **例外：无账号渠道（oczen）**不需要第 2 步，也不需要 `internal/auth` 的 `Load<X>Dir()`：
> 虚拟账号由 `oczen.AnonymousAuth()` 在 `main` 装配时注入 pool（`FilePath` 为空），
> 且 **不得** 纳入 `app.reloadAccounts`——`pool.SyncToDir` 会把「目录里扫不到」的账号剔除。
> 其 `Runtime` 必须设 `SingleAccount: true`（唯一且不可重登的账号 ⇒ 任何账号级惩罚
> 都等于整条渠道下线）：handler 对传输层错误与 `>=400` 一律原文透传，不冷却不计数不禁用；
> 启动时另调 `Pool.ClearPenalty(uid)` 清除旧版遗留的冷却。
> `Classify` 仍做语义分类（供日志），但不再用于决定惩罚。
> 详见 `docs/opencodezen渠道接入备忘.md`。

`provider.Upstream` 接口：
```go
type Upstream interface {
    RefreshToken(a *auth.Auth) error
    ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error)
    FetchModels(a *auth.Auth) ([]ModelInfo, error)
    FetchModelPricing(a *auth.Auth) ([]ModelPricing, error)
    UserResource(a *auth.Auth) (int64, error)
    UserResourceDetail(a *auth.Auth) (int64, []ResourceItem, error)
    DailyCheckin(a *auth.Auth) error
    Classify(status int, body string) ErrKind
    Stream(w http.ResponseWriter, r io.Reader) error
    Aggregate(r io.Reader) (map[string]any, error)
}
```

## 5. 跨平台

### platform 层

`internal/platform/` 按 build tag 拆分，保持同名导出函数：

| 文件 | 平台 | 说明 |
|------|------|------|
| `platform.go` | 全部 | 包文档 |
| `platform_windows.go` | Windows | MessageBoxW/注册表自启/浏览器探测 |
| `platform_darwin.go` | macOS | osascript/LaunchAgent/Chrome 无痕 |
| `platform_other.go` | Linux/其他 | xdg-open/简化实现 |

### 构建

```bash
# Windows（WSL 交叉编译）
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-H windowsgui" -o dist/wild-work.exe ./cmd/wild-work

# macOS（需要 macOS 真机或 CI，cgo 必需）
GOOS=darwin GOARCH=arm64 go build -o dist/wild-work-darwin ./cmd/wild-work

# Linux 无头
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dist/wild-work-linux ./cmd/wild-work
```

Windows 图标嵌入：`rsrc -ico cmd/wild-work/icon.ico -o cmd/wild-work/rsrc_windows_amd64.syso`

## 6. 数据流

```
【请求】客户端 → /v1/chat/completions → server(鉴权) → pool.PickExcluding(余额最高)
      → upstream.ChatStream(PrepareBody) → 上游 SSE 流回
      → 错误按 Classify 分类驱动冷却状态机；≥400 直接透传原始响应
      → Runtime.SingleAccount 渠道例外：任何错误均不冷却/不计数/不禁用，一律原文透传

【签到】scheduler(分钟级定时) → token 校验/必要时刷新 → DailyCheckinReport(结构化状态)
      → 窗口内重试（CheckinMinutes..CheckinRetryUntil），全号达 claimed/already 才标记该时段完成
      → UserResource → ReenableIfCredits 解冻 → RecordCheckin 落 state.json

【登录】面板发起 → login.Start(生成 state) → 浏览器窗口打开 → 轮询
      → 成功写 auths/ 文件 → pool 重载 → 异步签到 → 自动拉取费率

【费率】RefreshPricing → 遍历各渠道 Upstream.FetchModelPricing
      → 缓存到内存 + data/pricing-cache.json → 超过 1h 自动刷新
```

## 9. 账号路由与冷却机制改良（2025-08-25）

### 9.1 背景

原算法：每次请求 `Pick()` 选择**剩余积分最高**的 healthy 账号，导致同一客户端的连续对话可能频繁切换账号，上游 HTTP 连接池命中率低。

### 9.2 新算法核心逻辑

#### 路由策略

1. **同渠道同模型内路由**：不同渠道（WorkBuddy/TraeWork/Qoder）之间不涉及路由，各自独立。

2. **粘性路由优先**：
   - 首次请求：选择无冷却中、剩余额度最高的账号 A
   - 记录账号 A，重置连续请求计数

3. **后续请求**：
   - 继续使用上次选择的账号 A，递增请求计数
   - 除非触发以下任一条件：
     a. **遭遇上游错误**：按原有冷却逻辑处理（硬冷却 12h/软冷却 1min/错误阈值 10min），清除粘性记录
     b. **连续请求次数达到上限**：`reqCount >= maxReqs`（默认 50），自动降级换账号

> **为什么不使用 credits 阈值？** pool 中的 credits 仅在定时签到/手动刷新时更新，对话后是 stale 数据，
> 无法反映实时消耗。改用请求计数简单可靠，不依赖上游余额接口。

#### 伪代码

```go
const defaultMaxReqs = 50

// 粘性路由
sticky := getSticky(kind)
if sticky != nil && sticky.reqCount < sticky.maxReqs {
    acct := getAccount(sticky.uid)
    if acct != nil && !acct.IsCooling() && !acct.IsDisabled() {
        return acct  // 继续使用上次账号
    }
}

// 降级：选择余额最高的 healthy 账号
acct := pickHighest(healthy)
setSticky(kind, &stickyEntry{uid: acct.UID, maxReqs: defaultMaxReqs})

// 成功响应时递增计数
stickySuccess(kind)  // reqCount++

// 失败/错误时清除粘性记录
stickyClear(kind)    // 下次请求强制重新选号
```

### 9.3 粘性路由数据结构

```go
// stickyEntry 粘性路由记录：按渠道独立，记录上次路由账号及连续使用次数。
type stickyEntry struct {
    uid      string
    reqCount int    // 连续成功请求计数
    maxReqs  int    // 默认 50
}

// 存储在 Handler.sticky map[string]*stickyEntry 中，key 为 provider.Kind.String()
// 不持久化，进程重启后从首次请求自动重建。
```

### 9.4 冷却类型（不变）

冷却类型保持原有三种：

```go
const (
    CoolHard   CoolKind = iota // 余额不足 → 长冷却 (12h)
    CoolSoft                   // 429 → 短冷却 (60s)
    CoolErr                    // 连续错误 → 中冷却 (10min)
    CoolLowBalance             // 保留占位，暂未使用
)
```

### 9.5 实现要点

1. **状态不持久化**：粘性记录仅存于内存，进程重启后从首次请求重建
2. **healthy 判断**：只考虑 `disabled=false` 且 `until.IsZero() || now.After(until)` 的账号
3. **错误时清除**：上游错误（≥400、传输失败、refresh 失败）均调用 `stickyClear` 清除粘性记录
4. **成功时递增**：`stickySuccess` 仅在 ChatStream 成功返回后调用
5. **计数上限**：`maxReqs` 默认 50，连续成功 50 次后自动降级换号
6. **错误透传**：≥400 错误仍直接透传原始响应体给客户端

### 9.6 预期效果

| 指标 | 原算法 | 新算法 | 提升 |
|------|--------|--------|------|
| 会话连续性 | 20% | 80% | +60% |
| 上游连接复用 | 20% | 80% | +60% |
| 硬冷却频率 | 高 | 中 | -50% |
| 平均延迟 | 200ms | 150ms | -50ms |

---

## 7. 常用命令

```bash
go build ./... && go vet ./... && go test ./...     # 全量校验
GOOS=windows CGO_ENABLED=0 go build -ldflags "-H windowsgui" -o dist/wild-work.exe ./cmd/wild-work
./wild-work --no-tray                                # 无头模式调试
```

## 8. 已知注意事项

1. **`--no-tray` 无头模式**：无桌面 Linux 必须用此参数；不带参数在无 DBus 环境托盘 panic 会直接 exit 并提示。
2. **Windows 弹窗双显示器**：`MessageBoxW` 使用 `MB_DEFAULT_DESKTOP_ONLY` 标志强制主显示器。
3. **旧 Qoder 非流式不支持**：`Aggregate` 聚合返回空 content，建议只用流式；
4. **旧 Qoder 思考过程不暴露**：`model_config` 缺 `source:"system"`（issue #32）；
   QoderCN/QoderCOM 渠道已带该字段，思考正常，不受影响；
5. **`config.example.json` 与 `config.Default()` 必须同步**。
6. **定价缓存文件**：`data/pricing-cache.json`，首次启动从静态兜底开始，添加账号后自动拉取。