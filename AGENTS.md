# Wild-Work 项目提示词（AGENTS.md）

> 本文件面向 AI Agent 与开发者，是**索引 + 红线**层：只保留必读纪律、关键不变量与外部引用。
> 详细正文（决议全文、渠道协议、API 清单、上游取证）已拆到 `docs/`，按需再读，避免常驻上下文膨胀。
>
> **项目渊源**：3 个分离的 xxx2api 仓库 → workbuddy-wild v0.1.x（wails GUI）→ v0.2.x（+traework）
> → v2.0.x（弃 wails，改系统托盘 daemon + Web UI）→ v2.1.x（+WorkBuddy 国际版）。
> 当前 `master` 为 v2.8.x；v0.2.x 在 `legacy-wails` 分支（不再维护）。
>
> **一句话**：去掉 wails/WebView2，改为**系统托盘 daemon + 系统浏览器 Web UI** 的多平台（Win/mac）
> 多渠道账号聚合工具。

---

## 0. 最高优先级纪律（违反即视为严重缺陷）

### 0.1 ⛔ 严禁用穷举/批量探测上游接口（本次封号教训）

**2026-10-16 现场教训**：为验证 issue #133，对 `www.workbuddy.ai` 连发数十次真实对话请求
（消元矩阵 8 组 × 多轮重复、多账号对照），当日本机账号即被上游风控封禁。

**纪律（硬性）**：

1. **验证上游行为，最小化请求数**。首选**单发 A/B 对照**（同一凭据、同一 body、只差一个变量，
   各发 1~2 次即可定性）；**严禁**「逐项消元矩阵 + 每格重复 N 次」这类穷举法。
2. **严禁对生产账号做压力/批量测试**。需要重复验证时，先问「单次实验能否证伪」，
   不能证伪的假设**记为待观察**，不要用加大样本量去逼近结论。
3. **优先离线取证**：能用抓包（`.saz` / devdump）、已有日志、参考实现源码得出答案的，
   一律不要打上游。`internal/devdump`（见 §5）就是为此而设。
4. **验证他人修复时，先读代码与既有证据**。若参考实现/上游抓包已给出明确判据
   （如 PR 附带的 A/B 实测、抓包字段清单），**采纳证据链即可**，不必在本机重跑一遍。
5. 需要真实上游验证时，**只用一个专用小号**，且总请求数控制在个位数；**绝不动主力账号池**。
6. 风控是**账号级、可能不可逆**的代价（`pool.Disable` 后需人工恢复，甚至永久失效），
   远高于「少验证一条假设」的成本。**宁可结论标注「未实证」，也不要拿账号去试。**

> 判据来源可分层标注：**实测** > **抓包** > **参考实现源码** > **推断**。
> 写决议时如实标注层级（R23b 同款纪律）；**不得把推断写成实测**。

### 0.2 其他硬性红线

- **零 token 落盘**：日志/面板/消息框**绝不输出** access/refresh token（调试用假 token）。
- **不推翻已定决议**：除非有强理由，并**同步更新** `docs/决议项.md` 与下方索引表。
- **每次代码变更后重建 `dist/wild-work.exe`**（见 §4）。
- **不模拟 App 登录/跨端伪装**去触发额度发放（R32）；**不做薅羊毛式自动化**（R30）。

---

## 1. 决议项索引（R1–R79）

**全文（含证据链、反例、已知限制）见 [`docs/决议项.md`](docs/决议项.md)** —— 改动相关代码前必读对应条目。

| # | 决议摘要 |
|---|----------|
| R1–R3 | 托盘菜单固定（打开主界面/查看日志/退出），不做动态刷新；「刷新积分」已移到 Web UI |
| R4 | 管理 API 用 cookie 会话鉴权（监听非环回时强制要求设密码） |
| R5–R8 | Web UI 纯静态 embed；托盘用 energye/systray；移除 wails/WebView2；daemon 单进程单 `http.Server` |
| R9 | 核心业务整体复用，config/auth/state 格式零迁移（向后兼容） |
| R10 | 新增渠道 = 实现 `provider.Upstream` + auth 加载器 + 注册 Runtime；模型前缀 `channel/<model>` 路由 |
| R11–R12 | Win 产物 WSL 交叉编译、mac 走 CI（cgo）；无桌面 Linux 用 `--no-tray` |
| R13–R15 | 三接口兼容用 `internal/gateway` 边缘层 + in-process（`io.Pipe`）；`model` 显式传参；Responses/Anthropic 规范形状 |
| R16 | 无账号渠道（oczen）不建 auth、不进 `reloadAccounts`、不参与账号级惩罚 |
| R17 | 用量/积分**双流水分口径**（`ledger`），不强关联不折算 |
| R18 | 临期阈值可配（默认 24h，下限 24h） |
| R19 | TraeWork 专用池判据仅 `available_endpoint==1`（pid==209 已撤回） |
| R20–R21 | 千问办公 body 必带 `business` 段；`expires_in` 单位是**秒**，`expiresAt` 可用 JWT `exp` 校正 |
| R22–R33 | 智谱清言：网页版私有接口 + CDP 自动登录；签到=保活对话；新号额度需 App 登录；SSE 是增量 delta；模型名带 `:moe_53f` 后缀 |
| R23b | **分析纪律：观察性数据只能提假设，定因果必须做对照实验** |
| R24 | 清言是正常多账号渠道（`SingleAccount` 不设）；`SingleAccount` 是结构性声明，不是保险丝 |
| R29 | 轮换语义 = 粘性路由 + 错误惩罚 + **同请求内换号**（账号级错误不留给客户端重试） |
| R30/R32 | 不自动化「伙伴/群聊」任务；不模拟 App 登录（跨端伪装风控风险高） |
| R31 | 每日登录积分走 `daily_login_score`（幂等，`10001 今日已领取` 是正常语义） |
| R34 | **新增渠道必须补 `go xxxSch.Run(sctx)`**，否则自动签到/保活静默不运行 |
| R35 | 流式请求必须用独立的 `StreamHTTP`（**不设** `Client.Timeout`） |
| R36 | 流被截断不得补 `[DONE]` 伪装正常收尾 |
| R37 | TraeWork AuthCode 4xx 属终态（不换 origin 掩盖首因）；设备号作用域 = 账号 |
| R38 | 上游**按模型**限流时不得软化整个账号（→ `ErrSoftRate` 模型级冷却） |
| R39 | `scheduler.Config` 时间字段：`nil`=未配置落默认，`[]int{}`=本渠道无此类定时任务 |
| R40 | 流内业务错误不得伪装正常收尾；兼容层不得吞 error 帧（协议枚举合规） |
| R41 | 小浣熊协议回调须由 `main.go` 初始化前拦截（`--raccoon-callback`） |
| R42 | 本地模型校验取「静态表 ∪ 动态目录」并集 |
| R43 | `stats`（运行统计）与 `ledger`（用量流水）是**两套并列口径**，不合并不互校 |
| R44/R67 | 小浣熊登录走网页版 CDP 捕获（迁站后 cookie 名 `xhx_token`） |
| R45 | 明细与刷新路径同源回写 pool 快照（`creditSnapshot`） |
| R46–R47 | Cline 渠道（`sk_` Key 池 + 免费模型）；429 分两类，共享上限**不得**软化账号 |
| R48 | assistant 补 `content:""` 不再以 tool_calls 为前提（治 4001 中毒历史） |
| R49 | **思考档位只透传上游声明，绝不编造** |
| R50 | 思考链死循环检测（`loopguard`）：中断 SSE + **不罚账号** |
| R51 | OpenRouter 系思考字段归一化到 `reasoning_content` 单字段 |
| R52/R59 | 模型 ID 拼接规则单一来源（`provider.PublicModelID/SplitModelID`）；Cline 直通 `cline-free/*` |
| R53/R53b | usage 缓存计量只归一形状不改语义；oczen 会话头须是官方 `Identifier.descending()` 编码 |
| R54 | 模型降级链：原模型无容量（含整渠道全灭）时触发，不粘滞自动切回，降级必须可感知 |
| R55 | traework 模型元数据取上游下发；展示字段容错承接（`json.RawMessage`）不拖垮整张目录 |
| R56 | tool schema `$ref` **全量内联**消毒（moonshot 系对任何保留的 `$ref` 都不认账） |
| R57 | 代理改为「**全局备用上游通道**」：默认不参与任何请求，仅渠道按需取用（cline/oczen） |
| R58 | 流被截断必须**自证原因**（空闲看门狗 vs 上游断连）；`stream_idle_seconds` 配置项从未存在 |
| R60 | 单号渠道 402 处置 = **模型级**冷却（非账号级、非纯透传） |
| R61 | 模型降级链必须在**启动时**载入（不得只靠面板热更新通道） |
| R62 | oczen 同接备用通道；编排抽到 `backuplink.Replay` 与 cline 共用 |
| R63 | 思考强度映射是「出站注入」，与 R49「只透传声明」互补 |
| R64 | 面板「测试」类探针必须与真实请求**同源**取用备用通道，并如实回传实际出口 |
| R65 | 小浣熊「每日积分」是惰性落账，签到 = 调一次 `setting_info`；判据用 `point_grant_popups` |
| R66 | 自定义 OpenAI 兼容渠道（`custom_channels`）：面板配置即接入，统一流水/统计/路由；**无积分概念**，全面退出积分体系（v2.8.1：RefreshAll 跳过、单账号刷新/明细明确报「不适用」、前端动态判定隐藏无意义按钮） |
| R68 | **启动时 HTTP 监听失败必须终止进程**（不得只记日志继续跑） |
| R69 | oczen 匿名车道走 `/inference/v1`；不发 `x-opencode-request`；`x-opencode-project` 用裸 40 位 hex |
| R70 | 华为云果办 OfficeAce：双层凭据（网关 Basic 不过期 + 临时 AK/SK ~2h 需 refresh 续期） |
| R71 | WorkBuddyAI 对话出站必带会话头族（缺 `X-Conversation-ID` 即 `400 11-128`） |
| R72 | devdump 落盘必须 tee 流式（不得 `io.ReadAll`）：读全量再转发会把 SSE 攒批成「正文突然整段出现」 |
| R73 | 硬冷却（402）必须截到「下一个每日发奖窗口 + 缓冲」：qwenwork 每日 00:00 发额度（实测 00:01~00:20 到账），固定 12h 会把 19:07 的 402 白锁到次日 07:07 |
| R74 | 内建渠道扩到 20 条（Kilo/MiniMax/LobsterAI/ZCode/Gemini/CodeArts）；CodeArts 无上游身份字段 ⇒ 用户指定「账号标识」复用 UID |
| R75 | 429+code 14018（积分耗尽）归 `hard_credit`；候选耗尽时可在**请求内**按降级链重入一次（封顶一次、单账号豁免） |
| R76 | 登录态失效走可自愈的 `SessionDead` 标记（不 `Disable`）；重登后按 auth 文件 mtime 自动清除标记 |
| R77 | 上游声明可重试的 429（14003 瞬时拥塞，retry_policy.retry / actions 含 RETRY）必须请求内重试一次（独立预算，20s 退避）；6004 用量配额无信号，维持冷却到重置点 |
| R78 | **网关 HTML 401 也是「登录态失效」**（APISIX/openresty 拦截页 → `ErrSessionDead`，不是 `ErrClient`）；对话路径先 refresh 再决定是否落失效标记；目录/费率接口必须返回类型化错误 |
| R79 | **Qoder 双区签到：活动资格是上游按账号授予的**（实测：同账号在官方客户端里同样看不到入口）⇒ 无资格必须直说「该账号无资格、不是本工具的问题」，不得含糊成「上游未下发」；**且按活动 `startAt/endAt` 有效窗口过滤**（窗口外不参与「可领/已领」判定，缺字段则不过滤）；面板结果分「领到 / 已领 / 还没发 / 失败」四档，**不得一律写「签到成功」**（详见 `docs/决议项.md`） |

> ⚠️ **R71 附带的实测纪律**（也是 §0.1 的来源）：上游校验的是**头族齐全**而非单点
> （只补 `X-Conversation-ID` 仍被拒）；且 `X-Conversation-ID` 的**取值形态不被校验**
> （UUIDv4 / 32hex / 裸串都通过）。

---

## 2. 关键不变量（改动前必读）

0. **每次代码变更后必须本地重新构建 `dist/wild-work.exe`**（见 §4）。
   `dist/` 在 `.gitignore` 中，CI 只产出带平台后缀的 `wild-work-<os>-<arch>`，
   **不会**生成 `dist/wild-work.exe`——该文件只能手动构建。
   不重建会导致：本地运行的二进制与源码不一致（例如改了版本号但仍显示旧版本）。
1. `PrepareBody` 四改写勿动：强制 `stream=true`、`tool_choice` 归一化、`developer→system`、tool schema `$ref` 消毒（R56，无 `$ref` 时零改动）
2. 日志/面板/消息框**零 token**：不得输出 access/refresh token（调试用假 token）
3. auth 文件嵌套格式 `{auth:{...},account:{...}}`，`internal/auth.Parse` 与 login.SaveAuth 必须一致
4. `config.listen` 兼容新对象格式 + 旧字符串格式 `":7863"`
5. `data/state-*.json` 只增不减字段，向后兼容；旧 state.json 自动迁移
6. 托盘回调必须 goroutine 化
7. `config.example.json` 与 `config.Default()` 同步
8. 上游 HTTP ≥400 错误直接透传原始响应，不包装
9. 定价缓存持久化到 `data/pricing-cache.json`，启动加载，超 1h 自动刷新
10. 无桌面 Linux 必须 `--no-tray`，不带参数 panic 直接 exit 提示
11. **粘性路由**：`pickWithSticky` 优先复用上次账号，直至连续成功请求达 50 次或遭遇错误冷却。成功时 `stickySuccess` 递增计数，错误时 `stickyClear` 清除粘性记录。不使用 credits 阈值（pool 中余额是 stale 数据）。
12. **`internal/server` 主链路不得被绕过**：`POST /v1/chat/completions` 与 `GET /v1/models` 由内层直接服务，`internal/gateway` 只接管 `/v1/responses`、`/v1/messages`、`/v1/messages/count_tokens`。
13. **兼容层调用内层只能经 `Gateway.call()`**（`io.Pipe`），调用方读完必须 `res.Close()`，否则内层 goroutine 可能阻塞在 Write 上泄漏。
14. **`pipeRW.Flush()` 为空操作是刻意的**：`io.Pipe` 无缓冲，Write 即送达；不要改成缓冲 + 定时 flush。
15. **错误分类 429 必须优先于 hardMarkers**：限流 body 高频带 `quota exceeded`，先判 hardRule 会把限流误归余额耗尽 → 12h 硬冷却。三渠道 `Classify` 均已修复此顺序。
16. **脱敏层仅做文本替换不做语义变更**：`internal/sanitize` 只改模板句、不改用户内容语义；预检不命中时零分配原样通过。将来配置 `features.sanitize_fingerprints` 可一键关闭（逃生门）。
17. **积分「可用/不可用」拆分统计**：`provider.ResourceItem.Usable` 标记条目是否属于本工具可消耗的额度池，`provider.Summarize()` 汇总小计。
    - TraeWork 判据（2026-09-26 修订，R19 / issue #44）是 **`available_endpoint==1` 为不可用**
      （pid==209 判据已于 09-26 撤回，见下）：
      上游已不再下发 ep=1（专用池也标 0），ep 判据仅作历史兑底。**pid==209 判据已于 2026-09-26
      撤回**（issue #44）：pid=209「200 档每日签到」升级为通用积分，实测扣费就发生在这个池上
      （09-23 99 次对话期间 remain 恒 3086、「不可用」小计 -354），继续排除会让 pool 按虚低余额选号、
      面板把真实可用积分误标「不可用」。**不得用 `group_type` 判定**——同名「每日签到」既有
      通用份也有专用份。早期仅用 ep 判定的实测证据（未入库的本地备忘 `upstream-reverse-engineering.md` §2.3）。
    - `UserResource` / `UserResourceDetail` 返回的 remain **只能是可消耗余额**，
      否则 pool 会按虚高余额选号。含专用池的总量（`usage_summary.total_amount`）不能作路由依据。
    - 不可消耗额度仅用于面板展示（`pool.Status.UnusableCredits`），不参与 `Pick()` 排序；
      展示的唯一目的是让用户看到的总积分能和官网对上。
18. **到期时间字段因渠道而异，缺失则不显示**：WorkBuddy 系是 `CycleEndTime`（**上游从不下发 `PackageEndTime`**，旧判据恒 miss），
    TraeWork 是 `expire_time`（Unix 秒），Qoder 无此字段。均按 **UTC+8 墙钟**解析（`softRateResetLoc`），
    用 `time.Local` 会在非 UTC+8 机器上算错一天。上游未下发时 `ResourceItem.ExpireAt` 必须为空串，
    前端据此隐藏整列——**不得用零值时间冒充「永不过期」**。
19. **401 必须自愈，不能只信本地 `expiresAt`**：上游刷新会作废旧 access token（refresh token 同步轮换）。若新 token 未落盘、
    或同一账号在别处被刷新，本地文件里的 token `expiresAt` 仍在未来，但上游已拒绝 → `NeedsRefresh` 恒为假、永不刷新、
    积分恒 0、明细恒空。因此积分/明细/费率路径遇 `ErrSessionDead` 必须「refresh + 落盘 + 重试一次」
    （`app.refreshIfSessionDead`，scheduler 的 checkin 路径同理；**对话路径**见 R78）。
    - **401 的形态不止业务码**：网关（APISIX/openresty）会在认证前拦下请求并回 HTML 页，
      这种形态必须同样归 `ErrSessionDead`（`upstream.IsGatewayAuthPage`，见 R78）；
      归成 `ErrClient` 时上述自愈链全部静默失效。
    - **`FetchModels`/`FetchModelPricing` 必须返回类型化 `*provider.Error`**，不得用裸 `fmt.Errorf`——
      否则 `errors.As` 取不到分类，模型面与费率面的 401 自愈永远不触发（2026-10-17 现场缺口）。
20. **凡是调 `Upstream.RefreshToken` 的地方必须紧跟 `SaveAtomic`**：refresh token 会轮换，不落盘 = 下次启动用旧 refresh token，
    重回上一条的死锁（`RefreshPricing` 曾漏，已补）。
21. **匿名渠道的虚拟账号不得进入任何「能把它弄没」的路径**：`reloadAccounts` 不纳入（`SyncToDir` 会剔除），
    启动时 `SetDisabled(uid,false)` 兜底自愈；`RemoveAccount`/`DisableAccount` 对 `provider.Oczen` 硬拒（后端拒 + 前端无入口）。
    其 `Auth.ExpiresAt` 必须为远期值（不得为 0），否则 `NeedsRefresh` 恒真 → 反复 `RefreshToken` + 冷却。
22. **单账号渠道（`Runtime.SingleAccount`）不得施加任何账号级惩罚**：唯一账号且不可重登 ⇒ 任何惩罚
    （冷却/计数/禁用）都等于整条渠道下线。故 `Runtime.SingleAccount=true` 的渠道（当前 oczen）在 handler 里
    **传输层错误与 `status>=400` 一律原文透传**，不走 `NoteError`/`Cooldown`/`Disable`。
    - **429 也不冷却**（2026-09-24 修订，推翻早期「429 短冷却是唯一需要的背压」）：无号可轮换，
      冷却后后续请求在挑号阶段被挡成 `503 no_healthy_account`，不如透传 429 让客户端按 `Retry-After` 退避。
    - **传输层错误不累计 `errCount`**：否则 3 次网络抖动（默认 `ErrThreshold`）即冷却唯一账号。
    - 启动时 `Pool.ClearPenalty(uid)` 自愈旧版遗留冷却（`SetDisabled` 只清禁用，不清冷却）。
    - **严禁**把 401/403 归为 `ErrSessionDead`（会 `pool.Disable` 永久禁用且无法人工恢复）。
    - `oczen.Classify` 仍做语义分类（供日志），但**不再用于决定惩罚**（惩罚判定前已短路）。
    - **2026-10-14 澄清（PR #113）**："不施加账号级惩罚"不等于"什么都不冷却"——`ErrHardCredit`（402 权益不足）的处置是**模型级**冷却（`CooldownModel`，账号×该模型），账号本身仍不动。适用面比字面更宽：`SingleAccount` 渠道与「池内只有一个账号」的普通渠道（如 qwenwork 单号）都在其列。这是对 R16「4xx 一律 `ErrPassthrough`」的局部例外，理由见 R60。
23. **Qoder 双区签到只能走 campaigns，且状态必须结构化上报**：
    - **绝不调用 legacy `daily-check-in/claim`**：该端点已全局 DISABLED，却对未领取日恒返回 409，
      会被误判成「今日已领取」而跳过真实领取 → 假成功、零积分（上游 `99ab022` 同款结论，2026-09-21 抓包实测）。
      只有 `GET /sash/api/v1/me/campaigns` → `POST .../campaigns/{id}/claim` 会真实发放 100 Credits。
    - **401 必须返回 `*provider.Error{Kind: ErrSessionDead}`**，不得用裸 `fmt.Errorf`：调度器
      `isSessionDead` 走 `errors.As` 类型断言，裸 error 会让它恒为 false，自愈失效（不变式 19）。
    - **结果状态经 `provider.CheckinReporter` 上报**（`CheckinClaimed`/`Already`/`NoCampaign`/`NoToken`/`Error`）：
      `error` 通道无法区分 `no_campaign` 与 `error`——两者都不是「已签到」，却都需在窗口内重试。
      **`Msg` 必须透出**，不得被调度器覆盖成 `"ok"`，否则面板无法区分「真领到 100」与「活动还没上线」。
    - **签到窗口必须带重试**：每日 10:00（UTC+8）开放，活动可能在整点后才创建，
      故 `CheckinMinutes=[10:00]` + `CheckinRetryUntil=12:00`，窗口内每分钟重试，
      直到全部账号达 `claimed`/`already` 才算当日完成（上游 `ae3d42f` 修的就是漏领一天）。
      完成标记按「日期+时段」而非仅日期——本工具支持一天多时段（09:00/21:00），
      只按日期标记会让早间成功吞掉晚间时段。
24. **缓存字段是「可选上报」，不得把「不提供」显示成『0%』**：上游 usage 里是否回缓存命中字段
    因渠道而异（实测：WorkBuddy 系 `prompt_cache_hit_tokens`、Trae 系 `cache_read_input_tokens`、
    其余 `prompt_tokens_details.cached_tokens`；小浣熊回**空对象**、清言/Loomy 整条流无 usage）。
    判定集中在 `internal/server/cacheusage.go`，必须区分两种情形：
    - 「字段存在但为 0」（如 `prompt_cache_hit_tokens:0`）= 上游明确的「本轮未命中」⇒ 显示 0%；
    - 「字段不存在 / details 为空对象」= 上游不提供该维度 ⇒ 显示「—」，**绝不得显示 0%**。
    落地细则：`UsageEntry.CRC` / `ModelStat.CRCov` / `TokenStats.CRCov` 为零即代表「无样本」；
    利用率 = `CR ÷ CRCov` 的**加总口径**（未上报的请求整条不进分母，不稀释命中率）；
    三个字段一律 `omitempty`，保证旧流水读出来就是「未上报」（不变量 5 的向后兼容落地）。
    **分母不得做 `PT + cacheRead`**：实测 workbuddy 系 `hit+miss==prompt_tokens`、
    trae 系 `total==prompt+completion` ⇒ 命中数是 prompt_tokens 的**子集**（与 Anthropic
    官方口径相反）。副产品：利用率恒 ∈ [0,1]。回归测试 `internal/server/cacheusage_test.go`、
    `internal/ledger/cache_test.go`、`internal/stats/cache_test.go`、`scripts/ui-static-check.mjs`。
    **唯一会重建 usage 的渠道是 monkeycode**（只拼 pt/ct/total）⇒ 搬字段必须显式加，
    否则上游给的缓存数据在网关内部就被丢掉（本项就是它被发现的原因）。

---

## 3. 架构与渠道（摘要）

**架构选型**：

| 主题 | 选型 | 理由 |
|------|------|------|
| GUI 壳 | **无**（删除 wails） | WebView2 内存开销大 + Windows 绑定；托盘 + 浏览器足够 |
| 托盘 | energye/systray v1.0.3 | 已跨平台；菜单固定方案规避其不可删菜单项限制 |
| 管理后端 | 现有 http.Server 扩展 `/api/*` | 单端口、复用鉴权中间件（cookie 会话守卫）、零新服务 |
| Web UI | 纯静态 embed + fetch | 无 Node 构建链，单 exe 双击即用 |
| 登录 | `internal/login` + `login_*` | 纯 HTTP + 本地回调端口（或 CDP 捕获），跨平台 |
| 平台能力 | `internal/platform` + build tag | 浏览器无痕/开机自启/消息框/日志/工作区，接口同名 |
| 构建 | WSL 交叉编译 win；CI macos-latest 编 darwin | 见 R11 |

**托盘菜单**（固定，不做动态内容）：`打开主界面` / `查看日志` / `退出`；
单击与双击 = 打开主界面；各菜单项用不同颜色纯 Go 生成图标。

**平台能力差异表**（`internal/platform`）：

| 能力 | Windows | macOS | Linux |
|------|---------|-------|-------|
| 打开浏览器 | rundll32 url.dll | `open <url>` | xdg-open |
| 系统消息框 | MessageBoxW | osascript display dialog | stderr |
| 开机自启 | 注册表 Run | LaunchAgent plist | 未实现 |
| 打开日志文件 | notepad | open -a TextEdit | xdg-open |
| 确认框 | MessageBoxW YESNO | osascript buttons | 默认否 |
| 无头模式 | --no-tray | --no-tray | --no-tray（推荐） |

**面板区块与 API 全量清单**见 [`docs/接口参考.md`](docs/接口参考.md)。

### 渠道清单（20 条内建 + 自定义）

WorkBuddyCN（`workbuddy/*`）、WorkBuddyAI（`workbuddyai/*`）、TraeWork（`traework/*`）、
TraeCode（`traecode/*`）、QoderCN（`qodercn/*`）、QoderCOM（`qodercom/*`）、
千问办公（`qwenwork/*`）、MonkeyCode（`monkeycode/*`）、小浣熊（`raccoon/*`）、
Loomy（`loomy/*`）、智谱清言（`glm/*`）、OpenCodeZen（`oczen/*`，匿名）、
Kilo（`kilo/*`，免密免费池、无账号）、MiniMax Code（`minimax/*`，设备码授权）、
LobsterAI（`lobsterai/*`，本机回调授权）、ZCode（`zcode/*`，设备授权；积分领取依赖网页验证码，本工具不自动领取）、
Gemini（`gemini/*`，本机回调授权）、CodeArts（`codearts/*`，PKCE 授权）、
Cline（`cline-free/*`）、华为云果办 OfficeAce（`officeace/*`）；
另有用户自定义 OpenAI 兼容渠道（`custom_channels`，R66）。
旧 Qoder（`qoder/*`）已从界面下线，路由保留。

**新增渠道步骤**：① `internal/<channel>/` 实现 `provider.Upstream`；
② `internal/auth` 加 `Load<Channel>Dir()`（注意 glob 边界：`qoder*.json` 会吞 `qodercn-`/`qodercom-`）；
③ 注册 `server.Runtime` + `app.Runtime`；④ 前端渠道选择器加项 + `internal/login_<channel>`；
⑤ **`go xxxSch.Run(sctx)`**（R34，漏了会静默不跑）；⑥ 补回归测试。
> 无账号渠道（oczen / kilo）跳过 ②④，虚拟账号由 `main` 装配时注入。

**各渠道协议要点、凭据模型、已知限制与踩坑**见 [`docs/渠道接入备忘.md`](docs/渠道接入备忘.md)。

---

## 4. 构建与发版

> ⚠️ **本地构建是日常约束**：任何代码变更后都要重新构建 `dist/wild-work.exe`（见 §2 第 0 条）。
> CI 不生成该文件，且 `dist/` 不入版本控制。
> 标准流程：`go build ./... && go vet ./... && go test ./...` 全绿后再构建。

```bash
# Windows（本机直接构建，或 WSL 交叉编译）
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-H windowsgui" -o dist/wild-work.exe ./cmd/wild-work

# macOS（需 macOS 真机或 CI，cgo 必需）
GOOS=darwin GOARCH=arm64 go build -o dist/wild-work-darwin ./cmd/wild-work

# Linux 无头
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dist/wild-work-linux ./cmd/wild-work
```

构建后核对版本号已进二进制（防止拿到旧文件）：

```bash
# Windows bash 下用 python 字节计数（strings 对 Go 二进制的长串不可靠）
python -c "b=open('dist/wild-work.exe','rb').read(); print('new:',b.count(b'2.8.0'),'old:',b.count(b'2.7.3'))"
# 期望：new >= 1 且 old == 0。若旧版本号仍在，说明构建未生效。
```

### 发版（tag 触发）

push `v*` tag → GitHub Actions 构建五平台产物并创建正式 release。
**release note 用仓库根目录的 `RELEASES.md`**（按版本从旧到新排列，新版本说明**append 到文件末尾**）。
CI 从 `RELEASES.md` 截取对应版本段落（`# wild-work vX.Y.Z` 起）作为手写摘要，
而不是 `--generate-notes`；段落缺失时回退自动生成，不阻塞发版。

```bash
# 发版前确认：版本常量已 bump（internal/app/app.go const Version）、
# RELEASES.md 末尾已追加对应版本段落（# wild-work vX.Y.Z）且与 tag 名一致、
# dist/wild-work.exe 已本地重建验证
git tag vX.Y.Z && git push origin vX.Y.Z
```

> **重发同一版本**：`gh release delete <tag> --yes --cleanup-tag`（或 API 删 release + 删 tag）
> → 重打 tag 指向新提交 → 推送。CI 的 `gh release create` 遇到已存在的 tag 会失败，**必须先删干净**。

---

## 5. 开发期上游取证（devdump）

`internal/devdump` 把各渠道出站对话请求的**上游原始响应**落盘，供离线分析
「上游到底回了哪些 usage 字段」（缓存利用率所需的 `cached_tokens` /
`cache_read_input_tokens` / `prompt_cache_hit_tokens` 等）。

**这是 §0.1 纪律的首选工具**：能用它离线取证，就不要打上游。

**两层开关（缺一不可）**：

1. **编译期**：整包被构建标签切成两半——`-tags devdump` 编入真实实现
   （`devdump_on.go`），无 tag 时替换为**常量桩**（`devdump_off.go`）。发布版产物中
   不存在任何 dump 代码（实测 `dist/wild-work.exe` 里 `WILDWORK_DUMP_DIR` 计数为 0）。
2. **运行期**：仅当环境变量 `WILDWORK_DUMP_DIR` 非空才真正落盘。

**接线点只有一个**：`cmd/wild-work/transport.go` 的 `wrapDumps`（R57 后唯一统一改 Transport 的地方；
启动时一次性包住全部渠道 client），故各渠道包**零改动**；漏项由
`cmd/wild-work/transport_test.go` 静态锁死。文件落 `$WILDWORK_DUMP_DIR/<channel>/*.http`，
尾部带 `# duration=… usage=…` 汇总行（SSE 取**末帧**）。

> ⚠️ **落盘必须 tee，不得 `io.ReadAll(resp.Body)`**：后者会把整个响应缓冲到底
> 才交给调用方，对 SSE 流式响应等于「上游流完之后客户端才一次性收到全部内容」——
> 表现为**对话时正文突然整段出现**（常开 `WILDWORK_DUMP_DIR` 时必现）。现实现用
> `teeBody` + `respRecorder`：响应体原样逐帧转发（即时 flush 不受影响），落盘走 64 KiB
> 缓冲的独立写入（**延迟写，非逐字节写盘**）；内存只驻留最近 `tailKeep` 字节用于尾部与
> usage 速览，且汇总口径仍与旧 `truncateBody` 一致（前 `bodyCap` + 后 `tailKeep`）。
> 回归测试 `internal/devdump/devdump_on_test.go`：`TestDumpIsStreamingNotBuffered`
> （首帧到达即落盘头、不等流结束）+ `TestDumpConcurrentRecorders`（并发不串号/不丢帧）。

```bash
export WILDWORK_DUMP_DIR="$PWD/temp/dump"
go build -tags devdump -o dist/wild-work-devdump.exe ./cmd/wild-work
./dist/wild-work-devdump.exe --no-tray              # 需先停掉常驻实例或改端口
bash scripts/probe-dump.sh run                      # 逐渠道发一轮真实对话
bash scripts/probe-dump.sh summarize                # 汇总各渠道 usage 字段
```

> ⚠️ 快照含 access token 等敏感头，仅限本机分析、不得外传。

---

## 6. 文档索引

**入库文档**（`docs/` 白名单制下反选入版本控制；见 §6.1 规则）：

| 文档 | 内容 |
|------|------|
| [README.md](README.md) | 用户文档 |
| [DEVELOPMENT.md](DEVELOPMENT.md) | 开发者文档（面向 AI Agent） |
| [AGENTS.md](AGENTS.md) | **本文件**：纪律 + 决议索引 + 关键不变量 + 构建发版 |
| [docs/决议项.md](docs/决议项.md) | **R1–R79 决议全文**（证据链、反例、已知限制） |
| [docs/渠道接入备忘.md](docs/渠道接入备忘.md) | 各渠道协议要点、凭据模型、已知限制与踩坑 |
| [docs/接口参考.md](docs/接口参考.md) | 面板区块规划 + 管理 API 全量清单 |
| [docs/三接口兼容改造备忘.md](docs/三接口兼容改造备忘.md) | 三接口（Chat/Responses/Anthropic）兼容层架构决策与验证清单 |
| [docs/用量积分流水记账备忘.md](docs/用量积分流水记账备忘.md) | 双流水统计架构、差分算法、实测验证、已知限制（R17） |
| [docs/GitHub直连下载与加速代理调研备忘.md](docs/GitHub直连下载与加速代理调研备忘.md) | 自动更新取源调研：证伪 DNS 污染、IP 级秒级轮换、公共代理实测 |

**未入库**（仅本地可见，供离线参考，**不要**作为可点击链接引用）：
`docs/opencodezen渠道接入备忘.md`、`docs/cline渠道接入计划.md`、`docs/cline2api线索项目评估.md`、
`docs/chatglm_*.md`、`docs/工具调用能力增强层设计.md`、`docs/teleagent渠道可行性评估.md`、
`ref/`、`HANDOFF.md`、`GO多平台发布备忘.md` 等。
（逆向分析类文档一律只保留本地、不入库。）

**例外**：glm 渠道的协议依据**不在本地**——作者已开源为独立仓库
[glm2api](https://github.com/ttales430/glm2api)（签名算法/认证流程/SSE 语义/积分机制/智能体清单），
引用一律指向该仓库，不要找本地文件。

### 6.1 docs/ 白名单制规则

`.gitignore` 中 `docs/*` 默认忽略全部文档，仅 `!docs/<文件名>` 显式反选的才入库。

- **新增需要入库的文档时，必须同时做两件事**：① 在 `.gitignore` 追加一行 `!docs/<文件名>`；
  ② 在 §6 的表格里加一行链接。
- **验证入库状态**：`git check-ignore -v docs/<文件名>`（无输出 = 已入库；有输出 = 仍被忽略）。
  或 `git ls-files docs/` 查看实际入库清单。
- **`AGENTS.md` / `README.md` / `DEVELOPMENT.md` 中所有 `.md` 链接都必须指向已入库文档**，
  否则他人克隆仓库后会看到死链。提交前用下面这条自查：

```bash
# 只取 markdown 链接目标（]（docs/xxx.md））——不要把反引号里的"未入库文档"也算进来
rg -o '\]\((docs/[^)]+\.md)\)' -r '$1' AGENTS.md README.md DEVELOPMENT.md \
  | sort -u | while read f; do
  git check-ignore -q "$f" && echo "❌ 未入库（死链）: $f" || echo "✅ $f"
done
```
