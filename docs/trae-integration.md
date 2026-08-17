# TraeWork 上游接入调研报告（docs/traework-integration.md）

> 状态：**调研完成，待接入**。本文记录 2026-08 对 `traework2api`（Sliverkiss）与 `Trae2api-cn`（autumnsentiment）两个项目的逆向成果，以及它们与本工具（workbuddy-wild，WorkBuddy/CodeBuddy 上游）的集成方案。
> 目的：避免后续接入时上下文过大，关键事实、已验证端点、设计决策全部沉淀于此。
> 配套源码副本：`docs/traework2api/`（Go 实现，主要参考）、`docs/Trae2api-cn/`（Python 实现，交叉验证）。

---

## 0. TL;DR（结论先行）

- **原理与 workbuddy 同构**：都是把免费对话通道包装成 OpenAI 兼容 API + 多账号轮转 + 自动签到。traework 上游是 **Trae SOLO（CN）**，走 `trae-api-cn.mchost.guru` 网关 + `api.trae.cn`（签到/积分）+ `api.trae.com.cn`（OAuth 换 token）。
- **协议已实测可用**：6 个端点全部 HTTP 200 验证通过（详见 §3），SSE 事件结构与 traework2api `solosse.go` 逐字段一致，**可直接搬用其代码**。
- **集成方式**：新增一个"分组"（group = traework），与现有 workbuddy 分组**共用同一个 HTTP 端口**，模型名加前缀区分（`workbuddy/*` vs `traework/*`），每组内部多账号轮转/冷却复用现有 `pool`。
- **关键差异**：认证体系完全不同（`Cloud-IDE-JWT` 头 vs `Bearer`）、登录流程不同（127.0.0.1 回调 vs state 轮询）、上游 host 不同。**这些差异需要新增独立的上游客户端**，但账号池/调度器/服务器层可复用。
- **本机已有一个有效 Trae 账号凭证**（用户 2026-08 登录，token 有效期到 2026-08-31）——接入时可直接用真实 token 联调，但**不得把 token 写入任何文档/日志**。

---

## 1. 项目定位与目标

### 1.1 本工具现状（workbuddy-wild）

- Go + Wails v2 双进程托盘应用：主进程 = HTTP 代理 + 调度器 + 托盘（无 WebView2 常驻）；面板进程 = Wails GUI（按需拉起）。
- 上游：CodeBuddy CN（`copilot.tencent.com`），`Bearer <accessToken>` 认证。
- 已实现：多账号池（积分最高优先 + 冷却状态机）、无痕浏览器登录、自动签到（9/21 点）、积分展示、OpenAI 兼容 API。

### 1.2 目标

1. **新增账号类型"traework"**：面板"添加账号"时可选 WorkBuddy 或 TraeWork，登录流程各自独立（无痕浏览器），凭证落不同前缀文件。
2. **共用同一 HTTP 端口**：`/v1/chat/completions` 按请求的 `model` 字段路由到对应分组上游。
3. **模型命名**：分组内模型名透传；跨组同名模型用前缀区分（见 §5.3）。
4. **组内多账号轮转**：复用现有 `pool`（积分最高 + 冷却/禁用），错误分类按上游语义微调。
5. **自动签到 + 积分展示**：两个分组各自签到/查积分，面板分组展示。

### 1.3 非目标（本次不做）

- 不做 Trae 网页版 remote 会话（`chat_sessions`，Trae2api-cn 的 OmniRoute 通道）——只做 traework2api 的 `llm_utils_chat` 单通道。
- 不做 Trae CLI 子进程通道（Trae2api-cn 的 cli 模式）。
- 不做国际版（SG/trae.ai）——只做 CN（`trae-api-cn.mchost.guru`）。本机账号是 CN（userRegion=cn），符合。

---

## 2. 上游体系对比（traework vs workbuddy）

| 维度 | workbuddy（现有） | traework（要接入） |
|---|---|---|
| 产品 | CodeBuddy / WorkBuddy | Trae Solo（免费对话） |
| Chat 端点 | `POST copilot.tencent.com/v2/chat/completions` | `POST trae-api-cn.mchost.guru/api/agent/v3/llm_utils_chat` |
| 免费通道标记 | 无（直接 chat） | `function: "solo_work_lite"`（**免费通道**） |
| 认证头 | `Authorization: Bearer <accessToken>` | `Authorization: Cloud-IDE-JWT <token>` + `X-Cloudide-Token` + `x-uid` + `x-app-id` + 设备指纹头 |
| token 刷新 | `POST /v2/plugin/auth/token/refresh`（X-Refresh-Token 头） | `POST api.trae.com.cn/cloudide/api/v3/trae/oauth/ExchangeToken`（body 带 RefreshToken） |
| 登录 | `auth/state → auth/token?state=` 轮询（无 PKCE） | 构造 `www.trae.cn/authorization?auth_from=solo&...&auth_callback_url=http://127.0.0.1:18080/authorize`，回调带 `refreshToken`/`userInfo`，再 ExchangeToken |
| 签到 | `POST www.codebuddy.cn/v2/billing/meter/daily-checkin` | `POST api.trae.cn/trae/api/v2/ug/checkin_credits/{status,claim}` |
| 积分 | `POST /v2/billing/meter/get-user-resource` | `POST api.trae.cn/trae/api/v2/pay/ide_user_ent_usage` |
| 模型表 | `GET /console/enterprises/personal/models` | `POST trae-api-cn.mchost.guru/api/ide/v1/get_detail_param` |
| SSE 事件 | 标准 OpenAI chunk（delta.content/tool_calls） | 专有：`metadata/timing_cost/output/extra_info/token_usage/done` |
| 错误语义 | 402/余额关键词 → 长冷却 | `code:1005`（plan 权益不足）→ 长冷却；429 → 短冷却；401 → session 失效禁用 |

**结论**：认证、上游协议、SSE 格式全部不同，**需要独立的上游客户端**；账号池/调度/路由/前端可复用。

---

## 3. 实测验证记录（2026-08-17，真实账号）

> 使用用户提供的回调链接（`refreshToken`）+ `ExchangeToken` 换出 access token 后，对 traework2api 全部端点做了真实调用。

### 3.1 端点逐项验证

| # | 端点 | HTTP | 结果 |
|---|---|---|---|
| 1 | `POST api.trae.com.cn/cloudide/api/v3/trae/oauth/ExchangeToken` | 200 | 换出新 Token(1004 字) + 新 RefreshToken + `TokenExpireAt`(ms) + `TokenExpireDuration`(ms)。响应结构 = `ResponseMetadata` + `Result{Token, RefreshToken, TokenExpireAt, TokenExpireDuration, RefreshExpireAt, ClientID, UserJwt}` |
| 2 | `POST api.trae.com.cn/cloudide/api/v3/trae/GetUserInfo` | 200 | `Result{UserID, ScreenName, TenantID, Region, AIRegion, AvatarUrl, ...}`。头带 `x-cloudide-token` |
| 3 | `POST trae-api-cn.mchost.guru/api/ide/v1/get_detail_param` | 200 | `config_info_list` **33 个 config**（glm-5.2/GLM-5-Turbo/Doubao-Seed-2.1-Pro/DeepSeek-V4-Pro/kimi-k2.6 等），字段 `config_name` + `display_config.display_name` |
| 4 | `POST api.trae.cn/trae/api/v2/ug/checkin_credits/status` | 200 | `{checked_in:true, credits:200, enable:true, code:0, message:"success"}` |
| 5 | `POST api.trae.cn/trae/api/v2/pay/ide_user_ent_usage` | 200 | `user_entitlement_pack_list` 5 个包，`entitlement_base_info.available_endpoint` 0/1 分层（通用/Work），`quota.credits_limit` 累计 4700+ |
| 6 | `POST trae-api-cn.mchost.guru/api/agent/v3/llm_utils_chat` | 200 | **SSE 完整流**：`metadata → timing_cost → 60×output → extra_info → token_usage(558 tokens) → done(stop)`，`provider_model_name:"glm-5.2"`，含 `reasoning_content` 思考链 |

### 3.2 SSE 事件格式（实测，与 solosse.go 注释一致）

```
event:metadata  data:{"model":"","session_id":"...","prompt_completion_id":0,...}
event:timing_cost data:{"name":"llm_raw_chat_v2",...,"provider_model_name":"glm-5.2",...}
event:output    data:{"response":"<内容增量>","reasoning_content":"<思考链增量>","tool_calls":null,...}
event:extra_info data:{"model":"glm-5.2","content":"{完整 reasoning JSON}","input_token":511,...}
event:token_usage data:{"prompt_tokens":19,"completion_tokens":539,"total_tokens":558,"reasoning_tokens":511,...}
event:done      data:{"finish_reason":"stop"}
```

- `output` 事件中 `response` 与 `reasoning_content` 是**增量**（流式拼接）。
- `tool_calls` 字段：null 或对象/数组；SOLO 内部用 `function_call` 字段名（非 OpenAI `function`）。
- `done` 事件后无 `[DONE]` 哨兵——上游 SSE 靠连接关闭结束，转换层需自己补 `[DONE]`。

### 3.3 请求头（实测必带）

```
Authorization: Cloud-IDE-JWT <token>
X-Cloudide-Token: <token>
X-Ide-Token: <token>
X-Uid: <uid>
X-App-Id: 6eefa01c-1036-4c7e-9ca5-d891f63bfcd8
X-App-Version: default
X-Ide-Version: 0.1.43
X-Ide-Version-Code: 20260716
X-App-Version-Code: 20260716
X-Ide-Version-Type: stable
X-Device-Type: windows
X-OS-Version: Windows 11 Pro
X-Device-Brand: 83DG
X-Machine-Id: <hex32>
X-Device-Id: <hex32>
Request-Traffic-Type: prod
User-Agent: Trae/0.1.43
Accept: text/event-stream（chat）/ application/json（其余）
```

- `machine_id`/`device_id` 为 32 位 hex 随机串，**实测随机生成可用**（上游不强制绑定真实设备）。
- 签到/积分（`api.trae.cn`）头更简单：`Authorization: Cloud-IDE-JWT` + `X-User-Region: CN` + `X-Device-Id`。
- OAuth（`api.trae.com.cn`）头：仅 `Content-Type` + `User-Agent`（无需认证头），`GetUserInfo` 额外带 `x-cloudide-token`。

### 3.4 Chat 请求体（traework2api `PrepareBody` 改写规则）

```json
{
  "model": "glm-5.2",
  "config_name": "glm-5.2",
  "function": "solo_work_lite",
  "stream": true,
  "messages": [{"role":"user","content":[{"type":"text","text":"..."}]}]
}
```

改写规则（traework2api `payload.go`，全部实测通过）：
1. `messages[].content` 字符串 → `[{"type":"text","text":...}]`；已是数组 → 透传。
2. `stream` 强制 `true`（上游拒非流式；非流式由本地聚合）。
3. `model` → 同时写 `config_name` + `model`（都填上游 config_name，如 `glm-5.2`）。
4. `function` 固定 `"solo_work_lite"`。
5. `tool_choice` 归一化：`"none"` 删 tools/functions；`{"type":"auto"|"required"}` → 字符串；`{"type":"function","function":{"name":"x"}}` → `"x"`；其他删除。
6. `tools[].function.parameters` 对象 → **JSON 字符串**（上游 struct 该字段是 string）。
7. assistant 消息 `tool_calls[].function` → 上游 `function_call`（字段名差异）；无 name 的剔除。

### 3.5 错误分类（traework2api `client.go`）

| 条件 | 分类 | pool 动作 |
|---|---|---|
| body 含 `"code":1005` 或 `1005`+`plan` | `plan_limit` | 长冷却 12h |
| HTTP 401 + body 含 login/token 失效/session/unauthorized | `session_dead` | 禁用（需重登） |
| HTTP 429 | `soft_rate` | 短冷却 60s |
| HTTP 404 | `not_found` | 短冷却 60s（不累计 errCount） |
| HTTP 5xx | `server` | 累计错误冷却 |
| 其他 4xx | `client` | 累计错误冷却 |
| SSE 流内 `event:error`（code=1005 等） | 同上 | 流式途中也能冷却并轮转 |

---

## 4. 参考实现分析

### 4.1 traework2api（Go，纯标准库）—— 主要参考

模块结构与本工具**几乎一一对应**，移植成本低：

| traework2api 包 | 内容 | 本工具对应 | 复用策略 |
|---|---|---|---|
| `internal/upstream/constants.go` | AgentHost/UgHost/OAuthHost/ClientID/AppID/IdeVersion/Function/各端点 | `internal/upstream`（workbuddy） | **原样搬入新文件**（不与现有 workbuddy 常量冲突） |
| `internal/upstream/headers.go` | SOLOHeaders/UgHeaders/OAuthHeaders | `internal/upstream/headers.go` | 新增 `trae_headers.go` |
| `internal/upstream/payload.go` | PrepareBody 改写 | `internal/upstream/payload.go` | 新增 `trae_payload.go` |
| `internal/upstream/solosse.go` | SSE 解析（Aggregate/Stream/StreamWithError） | `internal/upstream/sse.go` | 新增 `trae_sse.go` |
| `internal/upstream/client.go` | ChatStream/FetchModels/RefreshToken/CheckinStatus/CheckinClaim/UserEntUsage/GetUserInfo + Classify | `internal/upstream/client.go` | 新增 `trae_client.go` |
| `internal/auth/auth.go` | trae auth 文件解析（嵌套/扁平）+ 原子写回 | `internal/auth/auth.go` | **可复用**（字段兼容：accessToken/refreshToken/expiresAt/domain/apiHost/machineId/deviceId/uid） |
| `internal/pool/pool.go` | 账号池（积分最高 + 冷却/禁用） | `internal/pool/pool.go` | **完全复用**（字段语义一致） |
| `internal/scheduler/scheduler.go` | 定时签到 + token 预刷新 | `internal/scheduler/scheduler.go` | **完全复用**（改签到 API 调用点） |
| `internal/server/handler.go` | OpenAI 兼容路由 | `internal/server/handler.go` | 复用 + 模型路由改造（§5.3） |
| `cmd/server|signin|credit` | CLI 工具 | 不需要（GUI 已覆盖） |
| `login.sh` | 登录脚本 | `internal/login` | 改造 `internal/login`（§5.2） |

### 4.2 Trae2api-cn（Python）—— 交叉验证

- 同一套端点/头/模型表的**独立实现**（代码无互相引用），协议一致性 = 强互证。
- 额外能力（本次不做但可参考）：3 条上游通道（CLI 子进程 / OmniRoute 网页 remote 会话 / IDE chat）、`storage.json` 自动解密（`trae_decrypt.py`，可解出本机 Trae 客户端登录态）、web OAuth 登录页、会话 slot 管理、设备指纹轮换。
- `trae_decrypt.py` 已在本机验证：能解密 `%APPDATA%/Trae/User/globalStorage/storage.json` 的 `iCubeAuthInfo://icube.cloudide`（tc 格式 AES-128-CBC），拿到 token/refreshToken/userId/region/host——**可作为"自动导入本机 Trae 登录态"的备选方案**（本次不做，但值得后续考虑）。

---

## 5. 集成设计

### 5.1 总体架构（双分组）

```
                ┌─────────────────────────────┐
                │  OpenAI 兼容 HTTP (127.0.0.1:7863) │
                │  /v1/chat/completions        │
                │  model 解析:                  │
                │    workbuddy/* → 组 A 上游    │
                │    traework/*  → 组 B 上游    │
                └──────────┬──────────────────┘
                           │
              ┌────────────┴────────────┐
              ▼                         ▼
     pool[workbuddy]              pool[traework]
     (CodeBuddy 账号)             (Trae 账号)
     Bearer 认证                  Cloud-IDE-JWT 认证
```

- **两个 `pool.Pool` 实例**（各自 state 文件：`data/state.json` + `data/state-trae.json`，或共用一份 + group 字段）。
- **两个 `upstream.Client`**（workbuddy 现有 + trae 新增）。
- **一个 `server.Handler`**：按 model 前缀路由到对应 pool + upstream。
- **一个 `scheduler`** 或两个（各签各的，共用调度时间配置）。

### 5.2 登录流程（面板"添加账号"）

**WorkBuddy（现有，不变）**：`auth/state` → 无痕浏览器 → `auth/token?state=` 轮询 → 落 `auths/workbuddy-<uid>.json`。

**TraeWork（新增）**，复用无痕浏览器 + 轮询框架，替换协议：
1. 生成 `machine_id`/`device_id`（hex32，登录与落盘共用）。
2. 构造登录 URL：
   `https://www.trae.cn/authorization?login_version=1&auth_from=solo&login_channel=native_ide&plugin_version=2.3.62834&auth_type=local&client_id=en1oxy7wnw8j9n&redirect=0&login_trace_id=<hex16>&auth_callback_url=http://127.0.0.1:18080/authorize&machine_id=<hex32>&device_id=<hex32>&x_device_id=<hex32>&x_machine_id=<hex32>&x_device_brand=PC&x_device_type=PC&x_os_version=1.0&x_app_version=0.1.43&x_app_type=stable`
3. 无痕浏览器打开；用户登录后浏览器跳 `http://127.0.0.1:18080/authorize?isRedirect=true&scope=solo&data=...&refreshToken=<jwt>&loginTraceID=...&host=https://api.trae.com.cn&refreshExpireAt=<ms>&userRegion=cn&userJwt=&userInfo=...`
4. **回调 URL 由本地监听器捕获**（复用现有登录 state 轮询思路：面板进程起一个临时 HTTP 监听 127.0.0.1:18080，或改成轮询一个落盘文件——本工具用轮询 state 文件，可改造为"回调 URL 写入 state 文件，面板轮询读取"）。
5. 解析回调 `refreshToken` + `host` → `POST host/cloudide/api/v3/trae/oauth/ExchangeToken`（body：`{ClientID, RefreshToken, ClientSecret:"-", UserID:""}`）→ 拿 `Result.Token` + 新 `RefreshToken` + `TokenExpireAt`。
6. `POST host/cloudide/api/v3/trae/GetUserInfo`（头 `x-cloudide-token: <token>`）→ 拿 uid/ScreenName/TenantID。
7. 落 `auths/trae-<uid>.json`（格式与 workbuddy 相同的嵌套形，额外带 `apiHost`/`machineId`/`deviceId`）。
8. 自动签到 + 查积分（与 workbuddy 登录后一致）。

> **注意**：回调 `refreshToken` 里带完整 jwt（用户会看到"打不开的 127.0.0.1 页面"）。本工具面板进程**监听 127.0.0.1 端口接收回调**即可，用户无需手动复制粘贴（比 login.sh 体验好）。

### 5.3 模型路由（核心设计决策）

现有 `/v1/models` 返回单组模型列表；`/v1/chat/completions` 按 model 挑号。

**方案：model 前缀路由 + 前缀剥离**

- 客户端请求 `model: "traework/glm-5.2"` → 路由到 trae 组，上游收到 `config_name="glm-5.2"`。
- `model: "workbuddy/glm-5.2"` → 路由到 workbuddy 组（也支持无前缀，向后兼容 → 默认 workbuddy）。
- `/v1/models` 返回两组模型合并列表，`id` 带前缀：`workbuddy/glm-5.2`、`traework/glm-5.2`、`traework/DeepSeek-V4-Pro` 等。

**备选（不推荐）**：`__` 后缀（workbuddy 现有 `glm-5.2__dev` 风格）——前缀更直观且与现有逻辑无冲突。

**模型名映射**（trae 组）：
- `auto` / 空 → 默认 `glm-5.2`（traework2api 的 `DefaultConfigName`）。
- 已知 config_name 直通（`DeepSeek-V4-Pro`、`kimi-k2.6` 等，见 §6 完整表）。
- 未知模型**透传**上游（上游自己判断；traework2api 是 400 拒绝未知——需要决定：透传 or 400。建议**透传**，与 workbuddy 现有"未知模型透传"一致，避免新模型上线要改代码）。

### 5.4 账号池与 state 文件

- **方案 A（推荐）**：两个独立 `pool`，state 文件 `data/state.json`（workbuddy）+ `data/state-trae.json`（trae）。隔离清晰，现有代码零改动。
- **方案 B**：一个 pool + `Group` 字段。需改 pool 的 LoadDir/挑号逻辑（按 group 过滤），改动面大。

选 **A**：`pool.New(stateTrae)` 第二个实例即可，`Pick`/`Cooldown`/`Disable` 全复用。

### 5.5 调度器（签到/保活）

- 两个 scheduler 实例（各绑一个 pool + upstream），**共用** `config.schedule.checkin_hours` / `keepalive_hours`（同一个 Config 传两份）。
- 主进程 `syncLoop` 需同步两个 pool（分别 Reload）。

### 5.6 前端（面板）

- 账号列表加**平台图标**：workbuddy（蓝 W）/ traework（青 T）——**此项已在前一轮 UI 改造中实现**（`AccountView.Group` + `.acct-icon.icon-wb/.icon-trae`）。
- "添加账号"需选平台：WorkBuddy / TraeWork，分别走 §5.2 的登录流程。
- 账号展示：分组、积分、签到状态（两组并排或按组折叠）。

### 5.7 配置

```jsonc
{
  // ...现有配置不变...
  "trae": {                       // 新增（可选整段，缺省 = 不启用 trae）
    "enabled": true,
    "auth_dir": "./auths",        // 复用同一 auths 目录（trae-*.json）
    "state_file": "./data/state-trae.json",
    "default_model": "glm-5.2",
    "checkin_enabled": true
  }
}
```

---

## 6. Trae SOLO 模型表（实测 33 个 config_name）

> 来自 2026-08-17 `get_detail_param` 实测 + traework2api 静态表（32 个）+ Trae2api-cn alias 表交叉核对。

| config_name | 显示名（display_name） |
|---|---|
| `glm-5.2` | GLM-5.2 |
| `glm-5-turbo` | GLM-5-Turbo |
| `glm-5` | GLM-5 |
| `glm-4.7` | GLM-4.7（Trae2api-cn 提及） |
| `DeepSeek-V4-Pro` | DeepSeek-V4-Pro |
| `DeepSeek-V4-Flash` | DeepSeek-V4-Flash |
| `DeepSeek-V4-Flash-Official` | DeepSeek-V4-Flash（正式版） |
| `kimi-k2.6` | Kimi-K2.6 |
| `kimi-k2.7-code` | Kimi-K2.7-Code |
| `kimi-k3` | Kimi-K3 |
| `minimax-m2.1` / `m2.7` / `m3` | MiniMax-M2.1/M2.7/M3 |
| `qwen-3.6-plus` / `qwen-3.7-plus` | Qwen3.6/3.7-Plus |
| `qwen3.8-max` | Qwen3.8-Max |
| `qwen3-coder` | Qwen3-Coder |
| `Doubao-Seed-2.1-Pro` / `-Turbo` | Seed-2.1-Pro/Turbo |
| `Doubao-Seed-2.0-Code` | Seed-2.0-Code |
| `seed-code-pro-0430` | Seed-2.1-Pro |
| `browser_use_subagent` | （浏览器子代理） |
| `sagitta` / `aquila` | （内部） |
| `mimo-v2.5` / `mimo-v2.5-pro` | Mimo-V2.5（Trae2api-cn 提及，可能已下架） |
| `custom_model_*`（gemini/kimi/claude/gpt-5/deepseek_* 等） | 自定义模型占位 |
| `explore_sub_agent_v2` / `v13` | 探索子代理 |
| `summary` | 摘要 |

**建议**：接入时 `/v1/models` 动态拉取（`get_detail_param`）为准，静态表兜底；不要把上表硬编码为唯一来源（模型会变）。

---

## 7. 接入清单（TODO）

### 7.1 新增文件（移植 traework2api）

- [ ] `internal/upstream/trae_constants.go` — 常量（hosts/ClientID/AppID/版本/Function/端点）
- [ ] `internal/upstream/trae_headers.go` — SOLOHeaders/UgHeaders/OAuthHeaders
- [ ] `internal/upstream/trae_payload.go` — PrepareBody 改写（含 tool_choice/tools/function_call）
- [ ] `internal/upstream/trae_sse.go` — Aggregate/Stream（`solosse.go` 移植，含 `[DONE]` 兜底）
- [ ] `internal/upstream/trae_client.go` — ChatStream/FetchModels/RefreshToken/CheckinStatus/CheckinClaim/UserEntUsage/GetUserInfo + TraeClassify
- [ ] `internal/login_trae/login.go`（或并入 `internal/login`）— Trae OAuth 登录（§5.2）
- [ ] `internal/config` — 新增 `trae` 配置段
- [ ] `internal/server` — 模型前缀路由改造（§5.3）

### 7.2 修改现有文件

- [ ] `main.go` — 装配第二个 pool/upstream/scheduler；`syncLoop` 同步两组
- [ ] `internal/app/app.go` — 添加账号选平台；trae 账号操作（签到/积分走 trae upstream）
- [ ] `frontend/dist/app.js` — 添加账号平台选择；分组展示
- [ ] `internal/pool` — 无改动（独立实例）
- [ ] `docs/DEVELOPMENT.md` — 更新架构说明（双分组）

### 7.3 验证计划

1. 无头模式起服务，`traework/glm-5.2` 发一条消息 → SSE 正常返回（用真实 token）。
2. `trae` 组签到 + 积分 → 面板正确展示。
3. 两组混合多账号 → 各自轮转/冷却互不干扰。
4. 未知模型透传 vs 400 的行为确认。

---

## 8. 风险与注意事项

1. **免费通道可能随时调整**：`function: "solo_work_lite"` 是逆向出的免费标记，Trae 服务端可改；一旦失效需回归抓包。Trae2api-cn / traework2api 的 git log 可追踪上游变化。
2. **device 指纹**：实测随机 id 可用，但上游可能加强风控（Trae2api-cn 有"自动轮换设备指纹"）。接入后保持登录时生成、落盘一致。
3. **每账号并发限制**：Trae 免费账号约 2 并发（Trae2api-cn 有 slot 管理，traework2api 没有）。workbuddy 无此限制；若接入后遇 429/排队，需考虑加并发控制。
4. **签到 claim 的 9074/9095**：9074=操作太频繁（重试），9095=已签到（no-op 成功）。traework2api 已处理；移植时保留。
5. **区域**：只做 CN（`trae-api-cn.mchost.guru` + `api.trae.cn` + `api.trae.com.cn`）。国际版（SG）账号 host 是 `api-sg-central.trae.ai`，**本机旧账号就是 SG 的**（5 月过期），若用户用国际版需扩展。
6. **token 有效期**：Trae access token 约 14 天（`TokenExpireDuration: 1209600000ms`），refreshToken 约 1 年（`refreshExpireAt` 2027-02）。`expiresAt` 用毫秒→秒归一化（`>1e12` 除以 1000）。
7. **不得泄漏 token**：本文档只记录端点/格式，不记录任何真实 token 值。

---

## 9. 参考链接

- traework2api 源码副本：`docs/traework2api/`（Go 标准库实现）
- Trae2api-cn 源码副本：`docs/Trae2api-cn/`（Python 实现）
- 登录抓包文件：`docs/traework_login_session_20260817.saz`（用户登录回调的抓包，可作抓包分析素材）
- 上游抓包工具技能：`fiddler抓包分析`（如后续需要分析新版本协议）
