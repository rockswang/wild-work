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

## 1. 决议项索引（R1–R112）

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
| R78 | ~~网关 HTML 401 归 `ErrSessionDead` + `recoverSession` 先 refresh~~ **已随上游 `1828467` 回滚**：R76 已覆盖全部 401，网关页判定属重复且需随上游 HTML 文案维护；`recoverSession` 的真实场景 refresh 同样失败（`invalid_grant` 是 Keycloak 缓存不一致，约 6h 自愈）。**保留**：目录/费率接口返回类型化 `*provider.Error`（上游拆为 `87e60c7`） |
| R79 | **目录/费率接口遇 HTTP ≥400 必须返回类型化 `*provider.Error`**（裸 `fmt.Errorf` 会让 `errors.As` 取不到分类 ⇒ 模型面/费率面的 401 自愈永不生效） |
| R80 | **折线图的 0 值必须画成 `null` 断点**（画成 0 会在底部连出贴轴长线），并过滤「区间总量为 0」的系列；`connectNulls:false` |
| R81 | **自定义渠道独立成一级弹层**（主面板第 18 个按钮）+ **模型一键导入**（拉 `/models` 勾选批量加）；设置弹层里已移除该节 |
| R82 | **自定义渠道一行式列表 + 新增/修改全走弹窗**（渠道/模型/单模型三层）+ **下拉框 `appearance:none` 自绘箭头** |
| R83 | **模型与 provider 同行展示**（chip 是文字按钮，点开编辑）+ **删除改文案按钮**（不用 🗑/×）+ **移除「停用」概念**（`enabled` 字段删除） |
| R84 | **弹窗宽度统一三档**（400/720/1180，禁止第四档）+ **自定义渠道搬回设置弹层**（独立弹层只是绕过「设置太窄」的伪问题）+ 设置加吸顶跳转条 |
| R85 | **自定义模型改独立弹窗**（设置里只留入口按钮）+ **Provider 一行、其下每个模型各一行**，两类行都 `nowrap`（折行用横向滚动替代）；R83「模型与 provider 同行」被推翻，删除改文案/移除停用保留 |
| R86 | **设置弹层宽度回原样（720px）** + **自定义模型入口移到主工具栏「＋ CodeArts」右侧**；删掉 R84 的吸顶跳转条（搬走那块后失去理由） |
| R87 | **自定义模型弹层与设置同宽（720px）** + **去掉「测试」** + **每行按钮组整体右对齐**（信息区 `flex:1` + 按钮组 `margin-left:auto`，不是上下按列对齐） |
| R88 | **自定义模型弹窗深度统一项目原生风格与对齐修复**：Provider 行移除 `<div class="cm-actions">` 彻底修好按钮右对齐 Bug；底栏操作并列右下角；视觉设计全面回归项目原生 CSS 变量与设计令牌（标准 `.hint`、`.btn.small` 与从属树线），危险按钮改柔和描边消除刺眼红斑，与全局面板浑然一体 |
| R89 | **设置弹窗视觉细节规整化**：表单 Label（90px）与子行（98px）严格缩进对齐；签到时间改流式容器化，时间 chips 与「＋ 时间」按钮同高紧凑排列（消除加号孤立掉落缺陷）；开关行垂直居中规整化；滚动条极细化 |
| R90 | **弹窗长篇说明文字角标化（`?` 悬浮说明）**：自定义模型与设置弹窗中原占用大篇幅排版的说明段落收敛为右上/行内 `?` 角标（`.help-mark`），鼠标悬停（hover）展示带透明衔接桥的自适应浮动气泡（`.help-pop`，支持 rich text），消除长文本挤占纵向空间并提升表单通透度 |
| R91 | **修复设置内点「查询模型」导致设置弹窗消失 Bug**：移除 `openModelSearch` 误收起设置的逻辑；`modelSearchOverlay` 设为独立高层（`z-index: 120`），打开时以浮层叠加在设置上方，关闭后自然回到设置原位置，已填表单与滚动状态完全保留；补全标题右上角 `✕` 关闭按钮与 Escape 监听 |
| R92 | **设置面板降级链与思考强度编辑区视觉精细化重构**：① 标题区使用 flex 排序，使 `?` 角标紧随区标题（左侧），「🔍 查询模型」从虚线框升级为实线胶囊操作按钮（右侧）；② 修复全局默认输入框 `.grow` 坍缩截断 Bug，自适应撑满行宽；③ 降级链与思考强度规则录入区封装为独立 `.fb-compose-panel` 浅底卡片，操作区边界清晰；④ 空态提示升格为轻柔虚线卡片，已生效代理采用绿点徽标（`● 已生效`） |
| R93 | **修复降级链与思考强度输入框点击下拉菜单缺失 Bug**：① 修复 style.css 中注释意外闭合导致的 CSS 语法破坏，恢复 `.pk-wrap` 相对定位与 `.pk-menu.open { display: block }` 核心样式，并提级 `z-index: 100` 消除层叠遮挡；② 补充输入框 `"click"` 事件监听，使输入框已获焦点时再次点击亦能即刻唤起候选菜单；③ 设置打开时若费率未就绪自愈触发 `loadFees()` |
| R94 | **模型降级链 = 模型维度的替换机制**，只在「换号已用尽」后出手；三维归因（账号/模型/请求）对应四类处置（request / account-transparent / account-rotate / model），同维替换、跨维禁止；**模型饱和（14003/6004）与积分耗尽（14018）同属「值得请求内换模型」**，且请求内重入**只跨渠道**（同渠道兄弟模型共享同一批账号）；详见 [`docs/模型降级链定义.md`](docs/模型降级链定义.md) |
| R95 | **Qoder 双区签到：活动资格是上游按账号授予的**（实测：同账号在官方客户端里同样看不到入口）⇒ 无资格必须直说「该账号无资格、不是本工具的问题」，不得含糊成「上游未下发」；**且按活动 `startAt/endAt` 有效窗口过滤**（窗口外不参与「可领/已领」判定，缺字段则不过滤）；面板结果分「领到 / 已领 / 还没发 / 失败」四档，**不得一律写「签到成功」**（详见 `docs/决议项.md`） |
| R96 | **账号登录态失效必须可被用户看见与修复：`session_dead` 是「可自愈标记」不是「禁用」，且签到失败必须如实反映（R76 的落地补强）**——① `RecordCheckin` 的 map 查找变量不得叫 `ok`（遮蔽函数参数会让 `last_checkin_ok` 恒真、每次都清掉 `session_dead`、`else if sessionDeadLike` 成死代码）；② `MarkSessionDead` 已标记时**更新原因但保留时间锚**（时间锚是「重登后自动清除」的判据）；③ CodeArts 这类**回调写死回环**的渠道要做异地登录兜底 |
| R97 | **LobsterAI 异地登录：上游 portal 只认 `http://127.0.0.1:<port>/auth/callback`（hostname 严格等值，否则丢弃并改发 `lobsterai://` deep-link）⇒ 改 redirect host 到 LAN IP 无效（已实测并回滚，留护栏测试）**。可行方案：① 面板「粘贴回调地址」（`SubmitCallbackURL`，与自动回调同一结论通道、state 不放宽）；② 全自动 = 固定端口（`config.login.lobsterai_callback_port`）+ 浏览器侧 `ssh -L` 端口转发（本机已装 `ww-lobsterai-tunnel.service`，38055）；③ 局域网模式下不得因错 state 写终态（防 DoS）已随方案 ② 一并回滚，因为监听仍只绑环回 |
| R98 | **用量折线图的横轴必须是「固定整窗刻度」**：`days=1` ⇒ 00:00~23:00 共 24 个小时桶，`days>1` ⇒ 窗口内每天一个（**含无流水的空桶**）。不得按「当前几点」或「只有数据的时段」截断——旧实现 `for h := 0; h <= now.Hour(); h++` 让今日视图在 11 点只画到 11:00，横轴长度随时刻变化、刻度本身也在变，既无法横向比较也看不出「后面还没跑」；积分图更严重（自己按流水聚合，横轴只有有消耗的日子）。补桶是**后端唯一来源**（`ledger.Query`），前端只消费 `by_day`；积分图用 `usageAxisKeys(usageDays)` 对齐同刻度 |
| R99 | **渠道配色必须单一来源**：`:root` 的 `--ch-<类名>` 是唯一真源，`.badge.x` 一律 `var(--ch-x)`（**禁止写死 hex**），徽章统一走 `chBadge()`（底色 = `chColor()`，与折线图 series 同源，物理上无法漂移）；**新增渠道必须同时补 `--ch-<类名>`**，漏加会让 `chColor()` 静默回退 `#6366f1` ⇒ 多个渠道在图上撞成同色（10-17 连加 6 个渠道时就踩了）。自定义渠道无品牌色，按列表序号黄金角均分色相（不用固定色池，色池在渠道数超过池长时必撞）。**已知限制（实测，未修）**：master 走品牌色路线，两两辨识度靠「无完全同色」把关，**未做 CIEDE2000 校验**——实测最小 ΔE2000 = 6.9（loomy vs minimax），共 15 对 <14；`.badge` 文字色恒为白，traecode（4.1）/raccoon（3.79）白字对比度低于 WCAG AA 的 4.5。要收紧就得改色值，需先做视觉评估。**完整规范见 [`docs/渠道配色规范.md`](docs/渠道配色规范.md)**（色值基线表 + 新增渠道必补三处 + 变更流程；`ui-static-check` 逐值比对文档与 `:root`，改色不同步文档即测试失败） |
| R101 | **积分流水差分必须区分「池子缩水」与「用掉更多」**：`remain = total - used` ⇒ `Δremain = Δtotal - Δused`。旧实现只比 remain，「余额下降 + 无到期日」恒记 spend ⇒ QoderCN 一个 100 分包整包到期（带走了包内已用 1 分后剩的 99）被面板显示成「今日消耗 99」（2026-10-19 用户实测，当天真实调用只有 1 次 480 token，上游 quota 实测 `used` 反而从 1 归零）。修法：快照新增 `t`(total)，下降时多一道 `poolShrankExpiry` 判据，三门槛缺一不可——① 该条目无到期日（有到期日的渠道继续走 `expired()`，那一路本来就对）；② **该渠道**被证实会给 total/used 分别赋值（`channelBooksUsage`，持久化为快照 `"@ch/<channel>"` 标记；qwenwork/raccoon/minimax/monkeycode/loomy/glm 六个渠道 `Total=Remain`、`Used` 恒 0，无此门槛会把它们的真实消耗全记成过期。**必须按渠道判、不能按 key/账号判**：只签到不花钱的号 `used` 恒 0，按 key 判会把它的整包到期漏修）；③ `total` 确实下降且上次快照有 total（老快照无 `t` 字段 = 零值 ⇒ 自动退回旧口径，不改写历史归因，符合不变量 5）。详见 [`docs/决议项.md`](docs/决议项.md) |
| R102 | **「今日作废」必须是平台表的一等列，不能只藏在合计行 tooltip 里**：`credit_expired`（往回看：今天已经到期消失的额度）与 `expire_today`（往前看：今晚余额里还有多少会没）是**相反方向**的两个口径，混成一列会把「已经没了」说成「还没发生」。R100 补删掉顶部汇总横条时把唯一可见的「今日作废」一起删了，只剩合计行「今日消耗」的悬浮说明里带一句，等于不可见（2026-10-19 用户实测：「今日作废在哪看？」）。修法：平台表在「今日消耗」右侧加 `st-plat-expd` 列，逐渠道取 `platforms[].credit_expired`、合计行取顶层 `credit_expired`，配色用 `--danger` 与「今日消耗」的 `--warn` 区分。`ui-static-check` 里把 `FAKE_STATS` 的 `expire_today` 从 100 改成 77——两者取同值时断言无法分辨口径是否被接错（变异验证实证：同值时漏改 `credit_expired→expire_today` 不报警） |
| R103 | **「不适用」账号默认收起，但入口必须留在原地且一键可逆**：无积分概念的渠道（oczen/kilo/cline/自定义渠道）在账号页上每号占一整张只有「不适用」三字的卡片、汇总条上还各占一枚同样空洞的 chip，把真正有余额的渠道稀释掉（2026-10-19 用户：「把不适用的默认隐藏掉，但有时候又得看一下」）。修法：① 判据只用后端下发的 `credits_na`（`isNoCreditAcct`），**不在前端另维护渠道名单**——自定义渠道是运行期可增删的，手写名单必漏收起；② 汇总条收起这些 chip，改在**原位**放一枚「不适用 N号 展开/收起」开关 chip（不额外占工具栏一格）；③ 账号卡片在 `renderAccounts` 里就带 `hidden`（首帧不闪），并带 `data-na` 供 `applyAcctFilter` 二次过滤；④ `toggleNoCredit` 必须**重渲染卡片**而不只改 class（可见性只有 `naHiddenClass` 一个真相，两处各写一份必漂移）；⑤ 收起时若正筛选着某个「不适用」渠道必须自动清筛选，否则网格空白且「全部」chip 也随汇总条消失；⑥ 选择持久化 `localStorage["acct-show-na"]`，全部被收起时给 `#acctHiddenTip` 可执行提示。`ui-static-check` 新增场景 1.7（13 条断言），4 处变异（默认展开 / 去 `data-na` / 不重渲染 / 不清筛选）全部确认变红 |
| R104 | **回环回调渠道跨机登录 = 五渠道统一人工粘贴兜底；CDP 浏览器化已回滚**：traework/codearts/lobsterai/qwenwork/gemini 的回调写死回环，dashboard ≠ 网关机时唯一通用解是把浏览器地址栏 URL 粘回面板（`manualCallbackKind` 白名单单一来源，qwenwork/gemini 的 `SubmitCallbackURL` 本轮补齐）。CDP 方案（网关机拉浏览器+捕获导航）已完整实现后实测回滚——只解决「人在网关机前」，对主流的远程 dashboard + 无人值守网关无解。**跨机方案动手前必须先确认「人在哪台机器前」**。详见 docs/全渠道登录授权方式梳理.md §4 |
| R105 | **到期判定必须按「精确时刻 + 自然日」，「到期日字符串比大小」是错的**：七个渠道把上游到期时刻截成 `YYYY-MM-DD`（qwenwork `valid_to`、traework `expire_time`、workbuddy `CycleEndTime`、lobsterai/officeace/zcode 的日期串），面板再拿日期字符串比 `today`/`tomorrow`。后果有两个：① 上游每日类额度的真实形态是**次日 00:00 失效**，截成日期后落到「明日到期」桶，实测 415 分显示「明天到期」而实际只剩 5.7 小时（2026-10-19 用户实测）；② 「7 日内到期」只取**每账号最早到期日那一天**的额度，workbuddy/traework 每账号 20+ 个每日包全被漏掉，真实敞口 4938 分被低估成 1788 分。修法：① 七个渠道的到期值统一改成 **RFC3339 保留时分秒**，`loomy` 上游无到期字段就**留空不伪造**；② 判据收口到 `provider.ExpireBucketOf` / `ExpireWithinDays` / `ExpiringWithinDays`：先 `ExpireInstant` 取精确时刻，减 `expireMidnightGrace`（**5 分钟**，实测上游抖动 0～7 秒）后取**自然日**分桶——**次日 00:00 归今日**；③ `stats` 侧把「每账号最早日+额度」换成**逐条目** `[]ExpireItem`，三个桶逐条目累加；④ 「最近临期」按**剩余时长**升序并显示 `5.7 小时后`（同一天里也要能看出谁先没）；⑤ 临期阈值（设置项 + 调度器）同步改成自然日语义（今天算第 1 天），不用 `now + N×24h`——那个窗口会随设置时的钟点漂移。**伪键必须保持日期粒度**（`provider.ExpireDate`），否则升级瞬间每包产生一对假的 expire+earn 流水。顺带修掉 lobsterai 无时区串按 UTC 解析导致的 8 小时偏差。测试：`TestExpireBucketOf`（19 例边界）、`TestExpireWithinDays`、`TestExpiringWithinDaysSum`、`TestExpireBucketsByInstant`、`TestExpiryRowsHoursLeft`；7 处变异全部确认变红 |
| R106 | **glm 渠道整体迁 IDE(Electron) 指纹 + 对话语义简化为一来一回 + images 生图端点**（2026-10-20）：凭据与 CDP 授权链路不变，仅调用侧换 IDE 指纹（WEB 指纹触发 WAF 对抗，IDE 实测 100% 通过、不需要 refer__991/退避重试；cookie jar 共享、X-Device-Id 取 JWT device_id、40012→ErrSoftRate）；对话仅支持单 user 一来一回（system 忽略、assistant/多 user 本地 400 `glm_unsupported_conversation` 透传不罚号、不支持 tool_calls）；保活改 say hello + 对话后立即 bulk_delete 删会话；新增 `POST /v1/images/generations` 与 `/v1/images/edits`（依赖且仅依赖 glm、不支持换模型；size 严格映射五档、非法 400；默认 b64_json；edits 多参考图 = imgutil 拼版 + sang.pub 图床托管、单 http(s) URL 直传、注入排版不复刻约束；不请求内换号轮转）；生图与对话产物不自动删除。详见 R106 全文与 `internal/server/handler_images_test.go` |

| R107 | **渠道「每日类额度」必须下发逐笔到期时刻，否则到期清零会被账本记成「消耗」**（2026-10-20 用户实测驱动）：面板显示小浣熊「今日消耗 580」，用户质疑「我没用这么多」。上游 `points/v1/bills` 实测三账号各一笔 `daily_expire`（-200/-191/-189，`grant_at` 均 10-13，事件名「每日积分到期清零」），当天 raccoon 真实对话请求 **0 次**（`usage-*.jsonl` 无记录，最近一次是 10-18 21:45）。根因链：① 渠道给「每日额度」的 `ExpireAt` **为空** ⇒ `ledger.expired()` 恒 false；② 该渠道 `Total=Remain`、`Used` 恒 0 ⇒ R101 的 `poolShrankExpiry` 门槛 2（`channelBooksUsage`）不通过。两条到期判据同时哑火 ⇒「余额下降」被无条件记 `spend`。**同一处缺失还让面板三列到期恒为「—」**（实际 2100 分 = 7×300 全在 7 天内到期）。修法：`internal/raccoon` 的 `UserResourceDetail` 在 balance 之外**补一次 `points/v1/bills`**，把聚合的「每日额度」按**逐笔未到期发放**拆分、各带自己的 `expire_at`（RFC3339）。三条不变量缺一不可：① **余额口径不变**——按到期升序（= 上游先进先出消耗顺序，实测：三账号 10-13 那笔各花 100/109/111，其后笔笔满额）逐笔扣减，余量挂最后一笔，`Remain` 之和恒等于 `daily_points`，否则 pool 快照与面板余额对不上；② **账本差分键不变**——所有拆分条目共用 `Name`（`dailyItemName` 常量锁死）且**不设 `Key`**，`ledger.DiffCredits` 的按 key 聚合把它们合回一条，升级**零假流水**，同时聚合后的 `Expire` 取到**最早**那笔 ⇒ `expired()` 终于有判据；③ **降级不阻塞**——bills 失败只退回旧口径（无到期信息）并打日志，绝不让锦上添花的展示维度搞挂积分刷新。**不得**给整池静态补「今天到期」（RELEASES v2.8.x 的旧做法）：7 笔发放有 7 个到期日，全报今日等于虚高 7 倍，是编造。测试：`internal/raccoon` 的 `TestFetchDailyGrants`（过滤 + 升序）、`TestWithDailyExpiryFIFO` / `...PoolExceedsGrants` / `...NoGrantsUnchanged` / `...SharesOneLedgerKey`、`TestUserResourceDetailAttachesDailyExpiry` / `...BillsFailureDegrades`、`TestDailyItemNameStable`、`TestDailyExpiryFeedsExpireBuckets` / `...NotAllToday`、`TestSplitAggregatesToSameLedgerKey`；`internal/ledger` 的 `TestRaccoonDailyExpireNotSpend` / `TestRaccoonNoExpiryIsSpend`（反向护栏：不带到期日必记 spend，渠道侧回退时它会红）/ `TestRaccoonRealSpendStillSpend` / `TestRaccoonSplitItemsSameKey`。3 处变异（去掉到期填充 / FIFO 改均分 / 给拆分条目加 Key）全部确认变红 |
| R108 | **Qoder 三渠道（qoder/qodercn/qodercom）流式必须剥壳识别信封内嵌 `statusCodeValue>=400`**：上游把失败塞在 HTTP 200 的 SSE 信封里，实测有**两种形态**——内嵌 403 + `10605`/`RESOURCE_LIMIT`（带 `retryAfterSeconds`）与内嵌 429 + 纯文本 rate limit，原先 `parseNestedSSE` 只解 `body` → 两种同形地表现为「连续无字节被空闲看门狗掐成 truncated」，上游若发完即断连更被收尾 `[DONE]` 伪造成空成功流；现补结构化 error 帧（含 `retry_after_seconds`）、**不补 `[DONE]`**，两类均归 `ErrSoftRate`（2026-10-21 压测实证：被拒请求 0.4s 返回，修前 90.7s 白等） |
| R109 | **入库 prepbody 工具**（`cmd/prepbody`）：入站 dump → 出站请求体离线重放，定位「上游按出站 body 拒答」类故障；不参与运行时链路 |
| R110 | **档位注入走上游 efforts 映射，能力表只做校验/展示**：注入 = `config.ReasoningEfforts`（`*` 通配，`handler.applyEffort`）；`internal/reasoning` 保留为能力表（`Clamp`/`Listing`）供渠道校验与面板展示 |
| R111 | **DeepSeek 思考开关与多轮回填**（`internal/upstream/thinking.go`）：客户端表达思考意图时补 `thinking.type` + 默认档；每条 assistant 消息回填 `reasoning_content`（字符串），否则上游按「不思考」应答 |
| R112 | **loomy 流式路径必须识别「HTTP 200 + 业务错误体」**：整段非 SSE 时补 error 帧、**不补 `[DONE]`**、不计成功；单号渠道流内登录态失效**不硬禁用**（走 SessionDead 标记；实测 100002 会自愈，硬禁用 = 整渠道下线） |

> **R79–R82 为本仓独有决议**（上游无对应编号）；**R108 已按同一编号合入 `upstream/master`**（同号同内容，上游提交 `e3bc93f`）。上游 R83–R107 尚未合并进本仓，下次合并时本仓独有段按「顺延」约定调整：


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
    - **两种写法并存，消费方必须同时支持**：`YYYY-MM-DD`（该日**全天可用**，次日 00:00 起失效；
      全仓主口径，当日有效池走 `provider.ExpireDateToday()`）与 **RFC3339**（精确时刻，如 gemini 的
      5 小时/周配额窗口重置）。判定统一走 `provider.ExpireInstant`（是否已失效）与
      `provider.ExpiringWithin`（是否临期），**不要各自 Parse**——旧实现各处自解析且吞掉错误，
      RFC3339 的条目被静默当成「永不过期」（2026-10-18 核对）。
19. **401 必须自愈，不能只信本地 `expiresAt`**：上游刷新会作废旧 access token（refresh token 同步轮换）。若新 token 未落盘、
    或同一账号在别处被刷新，本地文件里的 token `expiresAt` 仍在未来，但上游已拒绝 → `NeedsRefresh` 恒为假、永不刷新、
    积分恒 0、明细恒空。因此积分/明细/费率路径遇 `ErrSessionDead` 必须「refresh + 落盘 + 重试一次」
    （`app.refreshIfSessionDead`，scheduler 的 checkin 路径同理）。
    - **上游错误必须类型化**：自愈链全靠 `errors.As` 取 `*provider.Error` 判 `Kind`。
      目录/费率接口曾用裸 `fmt.Errorf` ⇒ 模型面与费率面的 401 自愈**永远不生效**（R79）。
      新增任何上游调用时，≥400 一律返回 `&provider.Error{Kind: Classify(...), …}`。
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

**各渠道协议要点、凭据模型、已知限制与踩坑**见 [`docs/渠道接入备忘.md`](docs/渠道接入备忘.md)
（含 §5.1 **思考档位有效性对照表**与 §5.2 TraeCode/TraeWork 延迟实测结论——判「档位是否生效」
或「某渠道是否更慢」前先读该文件，避免用不可靠指标重复验证）。

---

## 4. 构建与发版

> ⚠️ **本地构建是日常约束**：任何代码变更后都要重新构建 `dist/wild-work.exe`（见 §2 第 0 条）。
> CI 不生成该文件，且 `dist/` 不入版本控制。
> 标准流程：`go build -tags devdump ./... && go vet -tags devdump ./... && go test -tags devdump ./...` 全绿后再构建。
> 前端（`cmd/wild-work/web`）另需 `node --check cmd/wild-work/web/app.js && node scripts/ui-static-check.mjs`。
> 其中 `internal/app/frontend_routes_test.go` 把「app.js 里出现的每个 `/api/…` 字面量」与后端路由注册逐条比对：
> 端点被改名/删除而前端没跟上即失败。本仓 web 三件套自维护（B 决策），上游改端点不会连带改它——
> R57 把「每渠道代理」换成单一 `POST /api/config/proxy` 后，面板仍调已删除的 `POST /api/config/proxies`，
> 保存静默失败到 2026-10-21 才发现；上游 R100 的 `GET /api/stats` 在合并中被吞也是同类（面板 404）。
> **默认构建统一带 `-tags devdump`**（含 CI 与发版）：取证能力常态编入，运行期由 `WILDWORK_DUMP_DIR` 控制（未设置时零开销）。
> 本机设过 `WILDWORK_DUMP_DIR`（常驻实例取证用）时，跑测试要显式清空它——
> 测试进程会继承该变量，把 fixture 请求也 dump 进同一目录（见 §5）：`WILDWORK_DUMP_DIR= go test -tags devdump ./...`。

```bash
# Windows（本机直接构建，或 WSL 交叉编译）
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -tags devdump -ldflags "-H windowsgui" -o dist/wild-work.exe ./cmd/wild-work

# exe 文件图标来自 cmd/wild-work/rsrc_windows_amd64.syso（已入库，go build 自动链接）。
# 换 build/windows/icon.ico 后必须重新生成并提交，否则本地 exe 还是旧图标：
#   go run github.com/akavel/rsrc@latest -ico build/windows/icon.ico -o cmd/wild-work/rsrc_windows_amd64.syso

# macOS（需 macOS 真机或 CI，cgo 必需）
GOOS=darwin GOARCH=arm64 go build -tags devdump -o dist/wild-work-darwin ./cmd/wild-work

# Linux 无头
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -tags devdump -o dist/wild-work-linux ./cmd/wild-work
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

1. **编译期**：整包被构建标签切成两半——**默认构建**（含 CI / 发版，统一带 `-tags devdump`）
   编入真实实现（`devdump_on.go`），不带 tag 构建时替换为**常量桩**
   （`devdump_off.go`：`Wrap` 原样返回、`Enabled()` 恒 false）。不带 tag 的产物中
   查不到任何 dump 代码；带 tag 的产物亦仅在 `WILDWORK_DUMP_DIR` 非空时才落盘。
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
| [docs/决议项.md](docs/决议项.md) | **R1–R112 决议全文**（证据链、反例、已知限制） |
| [docs/渠道接入备忘.md](docs/渠道接入备忘.md) | 各渠道协议要点、凭据模型、已知限制与踩坑 |
| [docs/接口参考.md](docs/接口参考.md) | 面板区块规划 + 管理 API 全量清单 |
| [docs/模型降级链定义.md](docs/模型降级链定义.md) | **模型降级链的唯一权威定义**：三维归因、错误类型全量归属、两条触发路径、三条硬规则、已知限制（R94） |
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