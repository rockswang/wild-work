# Wild-Work 项目提示词（AGENTS.md）

> 本文件面向 AI Agent 与开发者，记录**工具设计、架构选型、决议项**与开发约定。
> **项目渊源**：原始上游为 3 个分离的 xxx2api 仓库 → workbuddy-wild v0.1.x（wails GUI 包装器）→ v0.2.x（增加 traework 集成）→ v2.0.x（大改版弃用 wails，改用系统托盘 daemon + Web UI，增加 qoder 渠道）。
> 当前 `master` 分支为 v2.0.x 版本；v0.2.x 已迁移至 `legacy-wails` 分支。

---

## 0. 项目渊源

Wild-Work 是 WorkBuddy（国内版+国际版）/TraeWork/Qoder 多渠道账号聚合工具，演进历程：

1. **上游 API 仓库**：3 个独立仓库 (`wild-work-buddy-api`, `wild-work-traework-api`, `wild-work-qoder-api`) → 提供各渠道基础 API 封装
2. **v0.1.x (workbuddy-wild)**：Wails GUI 包装器，仅支持 WorkBuddy 单渠道
3. **v0.2.x**：增加 TraeWork 集成，仍用 Wails
4. **v2.0.x (当前 master)**：大改版弃用 Wails，改用**系统托盘 daemon + 浏览器 Web UI**；新增 Qoder 渠道
5. **v2.1.x**：新增 **WorkBuddy 国际版**（`www.workbuddy.ai`）渠道，与国内版 `workbuddy` 完全独立

> ⚠️ v0.2.x 代码已迁移至 `legacy-wails` 分支，不再维护。

## 0. 项目一句话

去掉 wails/WebView2，改为 **系统托盘 daemon + 系统浏览器 Web UI** 的多平台（Win/mac）多渠道账号聚合工具。

## 1. 已定决议项（不要推翻，除非有强理由并更新本节）

| # | 决议 | 说明 |
|---|------|------|
| R1 | **托盘菜单固定，不做动态内容、不做定时/事件刷新** | 用户在自己客户端操作无法捕捉，动态展示无意义 |
| R2 | ~~托盘提供「刷新积分」菜单~~ **已移除**。刷新积分改为 Web UI 面板操作 | 托盘菜单精简为：打开主界面 / 查看日志 / 退出 |
| R3 | 托盘固定菜单项：**打开主界面 / 查看日志 / 退出** | 双击托盘 = 打开主界面；不再弹"已启动"提示框 |
| R4 | **管理 API 采用 cookie 会话鉴权**（2026-09-24 修订原「不设鉴权」） | 新增 `config.admin_password`（默认缺失 = 不鉴权，向前兼容）；**监听非环回地址时强制要求设置**（启动层 fatal + `SetListen` 拒绝），否则局域网任何设备可无凭据访问面板/退出程序。会话实现见 `internal/app/session.go`：token 随机 + 内存态（只存 SHA-256），HttpOnly + SameSite=Lax cookie，7 天滑动续期，改密码即失效，登录失败 5 次/IP 锁定 5 分钟。`/api/auth/*` 不鉴权，其余 `/api/*` 走守卫；OpenAI 侧 `api_key` 不受影响 |
| R5 | **Web UI 用纯静态 HTML/CSS/JS**（无前端编译链） | `go:embed` 打进单文件；实用 + 大众审美即可 |
| R6 | 托盘库：**保留 energye/systray**（已跨平台 Win/mac/Linux） | 各菜单项使用不同颜色纯 Go 生成图标，无需外部图标文件 |
| R7 | **移除 wails / WebView2 全部依赖** | 省内存与运行时；平台能力封装进 `internal/platform`（build tag 拆分） |
| R8 | daemon 单进程：一个 `http.Server` 同时服务 OpenAI 端点 + 管理 API + 静态 UI | 沿用 server 现有 ServeMux 扩展 |
| R9 | 核心业务（pool/scheduler/upstream/traework/server/login/config/auth/provider）**整体复用**，格式零迁移 | config.json / auths/ / data/state.json 兼容旧版；旧 state.json 自动迁移到 state-workbuddy.json |
| R10 | 新增渠道扩展方式：实现 `provider.Upstream` 接口 + auth 加载器 + 注册 Runtime | 模型前缀 `channel/<model>` 路由；已实现 WorkBuddyCN(国内) + WorkBuddyAI(国际) + TraeWork + TraeCode(与 TraeWork 共账号，function=solo_agent) + QoderCN + QoderCOM(国际) + 千问办公(qwenwork) + MonkeyCode(monkeycode，平台托管模型) + 小浣熊(raccoon) + Loomy(loomy) + 智谱清言(glm) + OpenCodeZen(oczen 匿名) + Cline(cline，官方 sk_ Key 免费池) 十三渠道 + 用户自定义 OpenAI 兼容渠道（custom，R66，面板配置不限个数；名称不得与内建渠道同名）；旧 Qoder（`qoder/*`，QoderWork）已从界面下线但路由保留 |
| R11 | Windows 产物在 WSL 交叉编译（`GOOS=windows CGO_ENABLED=0`，已验证可行）；macOS 产物走 GitHub Actions macos-latest（cgo 必需） | WSL 无法编 darwin cgo；CI 增加 darwin job |
| R12 | **无桌面 Linux 使用 `--no-tray` 参数** | 无参启动在无 DBus 环境托盘 panic 直接 exit 并提示；`--no-tray` 跳过托盘打印信息阻塞等待 Ctrl+C |
| R13 | **三接口兼容采用两层结构：内层 handler 不动，新增 `internal/gateway` 边缘层**，经 **in-process 调用**（`io.Pipe` + ResponseWriter 形状）复用内层 | 代码量比内联重构多 20%，但改动面小一个数量级（主链路仅 2 处调用点 + 1 个访问器），回归风险低、可脱离 pool 单测。**不得用 HTTP 自环**（`0.0.0.0` 监听不可作目标、鉴权双份、启动竞态） |
| R14 | **`Stream`/`Aggregate` 的 model 由调用方显式传入**，渠道不得用实例字段记忆「上次请求的模型名」 | 旧实现 qoder 用全局 `lastModel`，多账号并发会串号；traework 恒为空串。详见 `docs/三接口兼容改造备忘.md` §3 |
| R15 | **Responses 的 `function_call` 必须是独立 output item**（带 `call_id`），Anthropic 的 tool_use 参数必须走 `input_json_delta` | 参考实现 `tokligence-gateway` 两处写法不合规范（塞进 `message.content`、start 里一次性给完整 input），Codex/Claude Code 会解析失败 |
| R16 | **无账号渠道（oczen）不建 auth 文件、不进 `reloadAccounts`、不参与禁用/冷却惩罚** | 匿名凭证是常量 `public`；`SyncToDir` 会剔除磁盘上不存在的虚拟账号，故只在装配时注入一次。单账号 + 不可重登 ⇒ 任何账号级惩罚都等于整渠道下线，故 4xx 一律走 `ErrPassthrough`（原文透传、不计错不冷却）。**2026-09-24 修订：429 也不再冷却**——原「429 短冷却是唯一需要的背压」经实测证伪：单账号无号可轮换，冷却后后续请求在挑号阶段被挡成 `503 no_healthy_account`，反而不如透传 429 让客户端按 `Retry-After` 自行退避；同理**传输层错误也不再累计 `errCount`**（默认 3 次网络抖动即冷却唯一账号）。两者由新增的 `server.Runtime.SingleAccount` 统一豁免（结构属性，不硬编码渠道名），启动时另调 `Pool.ClearPenalty` 自愈旧版遗留的冷却。详见 `docs/opencodezen渠道接入备忘.md` |
| R17 | **用量/积分双流水分口径统计，不强关联、不折算** | `internal/ledger` 双 JSONL（usage 按渠道×模型 / credit 按账号 earn·spend·expire）；写入仅 append 缓冲句柄（30s AutoFlush），读取仅在 UI 请求 `/api/usage` 时按月分段扫描聚合，常驻内存 ≈0。`Upstream.Stream` 返回末帧 usage（R14 同款显式传参哲学）。首见账号只记一条「存量额度」baseline，不逐条展开。**同 key 重复条目（WorkBuddy 伪键一对多）先聚合求和再差分**，每 key 每次刷新最多一条事件；升级首启将旧错误流水一次性归档为 `old-credit-*.jsonl` 并删快照重建 baseline（issue #38）。**2026-10-08 补丁（issue #67）**：WorkBuddy 签到包落在「明日到期」key 上且提前一天以 r=0 建档，「发放即消耗」场景同 key 差分 delta=0 → 当日 earn 被抵消为 0（面板今日收入恒 0）。两层修复：① 差分补丁——快照增加 `used` 字段，delta=0 且 used 上涨时按 `发放额=usedDelta` 补记 earn（数学依据 remain=total-used；used 恒 0 的渠道零影响）；② 权威入账——新增 `provider.CheckinGranter` 可选接口（对齐 CheckinReporter 模式），WorkBuddy 实现 `DailyCheckinGrant`，调度器以它为唯一签到执行路径并用回执 `credit/today_credit` 直接记 earn（回执缺失/为 0 静默降级回差分）。详见 `docs/用量积分流水记账备忘.md` |
| R18 | **临期阈值可配（默认 24h，下限 24h）** | `config.schedule.expiring_threshold_hours`，normalize 钳下限（日期粒度到期判定低于一天无意义）；scheduler 与 app.creditTotals 同源取 `cfg.ExpiringThresholdDur` |
| R19 | **TraeWork 专用池判据仅 `available_endpoint==1`**（2026-09-26 修订，issue #44 撤回 pid==209） | 演进：09-18 专用池下发 ep=1 → 09-23（f6f41a4）上游不再下发 ep=1，改判 `pid==209` → **09-26 实锤证伪**：pid=209「200 档每日签到」已升级为通用积分。本仓日志铁证：09-23 15:20–16:10 连发 99 次对话期间 remain（208/221 池）恒为 3086，而「不可用」小计 2200→1846（-354=99 次对话消耗量），即**扣费实际发生在被判为不可用的池上**；继续排除会让 pool 按虚低余额选号、面板误标「不可用」。故判据回退为 `ep==1`。pid=208/209/221 均可消耗 |
| R20 | **千问办公推理 body 必须携带 `business` 段**（`{product:"qoder_work",type:"agent",version:"1",feature_switches:{}}`） | 2026-09-24 上游 1.0.4 起网关按 `body.business.{product,type}` 解析模型目录，缺失 → 对话恒 HTTP 200 + envelope 503 `Model catalog unavailable`（模型列表/余额/费率不受影响）。**仅补 `Cosy-Business-*` 静态头不能替代**。已实测四组对照隔离变量：body 缺 business 时「本项目透传 body」与「上游原生重构造 body」均 503，补上后均 200 ⇒ 原生 body 结构、官方 `Encode=1` WASM 组包、机器指纹（machineId/Token）**均非必要条件**，故本项目只补字段、不引入 wasmtime 级依赖。参考 Buddy2api PR #84（v2.1.15） |
| R21 | **千问办公 `expires_in` 单位是秒**，且 `expiresAt` 可被 access token 的 JWT `exp` 校正 | 回归：早期按毫秒处理（`*time.Millisecond`），把 7 天压成 604.8 秒 → 落盘 `expiresAt` 比真实寿命少 ~7 天 → `NeedsRefresh(10min)` 几乎恒为真 → **每次请求都刷 token**，与千问办公 App 高频互踩，直至 refresh token 被作废、账号被禁用。证据链：上游 `expires_in=604800` 按秒算 = access token JWT 的 `iat→exp`（整 7 天，吻合）；按毫秒算 = 文件里的值（吻合）。且同仓 workbuddy/trae/workbuddyai 的 auth 文件 `expiresAt` 与 JWT `exp` 逐秒一致，**仅 qwenwork 偏离 6.99 天**。修复：①refresh 按秒解释，优先取绝对字段 `expires_at`，两字段都缺失时回退 84h（JWT 实测 7 天的一半）；②`LoadQwenWorkDir` 调 `Auth.AdoptJWTExpiry()`，用上游签名的 JWT `exp` 原地校正历史脏值（仅内存、只增不减、非 JWT 不动）。**注意：同族 dt-/drt- 渠道（qoder/qodercn/qodercom）token 为不透明串、无 JWT 可交叉验证，其 `// ms` 标注未被本次改动触及**（无证据不做改动） |
| R22 | **智谱清言（`glm/*`）走网页版私有接口；登录以 CDP 自动捕获为主、手工粘贴为兜底** | 清言无可编程登录接口，凭据是浏览器 Cookie 里的 `chatglm_refresh_token`。自动路径见 R27/R28；手工路径保留 `POST /api/login/glm_token`。**不为它引入 WebView2**（R7 已删除该依赖，为单渠道加回是架构倒退且 Windows 专属）——CDP 走系统已装的 Edge/Chrome，零新依赖。协议要点见 `docs/智谱清言渠道接入备忘.md` |
| R23 | **清言「签到」的实质是保活对话；新账号额度需 App 侧登录才发放** | `member_info.score_rule` 原文「免费用户，登录赠送200积分/天」，**但对照实验证明这个「登录」指 App 登录**：新账号加进来后 `left_score=0`，**必须在智谱清言 App 登录一次**才发放（+3000，随后再 +500）。证据：某账号创建后独立监控 **15.5 分钟全程为 0**，App 登录后 **30 秒内**变 300000（见 `docs/智谱清言渠道接入备忘.md` §2.16）。**影响**：`pool.Pick()` 按 credits 降序选号，而 `healthy()` 不要求 credits>0 ⇒ **额度为 0 的账号不算被禁用，但永远排最后、实际轮不到**。**代码层面无解**（额度发放是上游行为，Web 端无「领取额度」端点）。故 `DailyCheckinReport` 的实质动作 = 保活对话；`UserResource` 从 `member_info.left_score` 读真实积分（**单位「分」，÷100 得积分**）。旧的 `activity-api` 签到活动已下线，保留调用作尽力而为、**完全静默失败** |
| R23b | **分析纪律：观察性数据只能提假设，定因果必须做对照实验** | R23 的结论我**连续错了四次**（详见 `docs/智谱清言渠道接入备忘.md` §4.3）：①「服务端延迟」②「App 登录是原因」③「14 分钟没到账⇒需要 App」④「自己到账，与 App 无关」（**忘了是我自己让用户去登录的**，把实验干预当自然现象）。共同病根：**拿观察当因果 + 样本量 1 就下结论**。最终靠**对照实验**才定性：不干预观察 15.5 分钟全 0 → 引入单一变量（App 登录）→ 30 秒内到账。**可复用判据**：下结论前先问「还有哪些变量在同一窗口内变动」；**自己做过干预的实验必须把干预当变量** |
| R24 | **清言渠道为正常多账号渠道（`SingleAccount` 不设）** | **2026-09-26 修正**：初版误设 `SingleAccount=true`，理由是「凭据轮换后不可人工恢复」。但 ① 现在可用 CDP 自动登录随时补账号，该理由不成立；② `SingleAccount` 会**跳过所有账号级惩罚**（handler 两处短路），导致账号 A 失效/限流时**永远不切账号 B**，多账号形同虚设。故改为正常多账号：错误分类惩罚 + 池内轮换。唯一真实约束「refresh_token 轮换必须落盘」由 `glm.RefreshToken` 保证。**教训：`SingleAccount` 是结构性声明（该渠道只有一个号且不可恢复），不是「觉得渠道脆」的保险丝** |
| R25 | **清言 SSE 是「增量 delta」，但 `part.status=="finish"` 时给的是全文** | **2026-09-26 逐帧实测纠正**：`part.content[].text/.think` 的语义取决于 `part.status`——`init` 帧是**增量片段**（每帧几个字符），`finish` 帧是该段落**完整全文**。即 init 帧拼接 == finish 帧全文。实现：init 帧直接透传为 OpenAI delta 并累加；finish 帧与已发出内容比对，仅在全文更长时**补发差额**（防漏兜底）。**曾因照抄参考实现「假定全量快照」的注释而写错**，实测拼出 `"1, 3, 4, 5, 5"`（正确应为 `"1, 2, 3, 4, 5"`）。回归测试 `TestStreamRealFramesNoDuplication` 用真实抓包帧锁死。**注意参考实现 GLM-Free-API 的流式路径本身也是错的**（`substring` 求差在 delta 语义下丢字），不可照抄 |
| R26 | **清言 `accessToken` 允许为空（凭据本体是 refresh_token）** | 用户手填时通常只给 refresh_token，而 `auth.Parse` 要求 accessToken 非空 → `LoadGLMDir` 用放宽版解析器 `parseAllowMissingAccessToken`，由 `glm.acquireToken` 在首次请求时补齐。且 refresh 会轮换 refresh_token，**必须落盘**（落盘失败显式报错，对应不变量 19/20） |
| R27 | **清言登录走 CDP 自动捕获 Cookie，独立 profile，手工粘贴仅作兜底** | 凭据是浏览器 Cookie 里的 `chatglm_refresh_token`。**不读浏览器 Cookie 数据库**——实测 Edge 运行时对其持独占锁（20 进程），既不能读也不能复制，且值为 DPAPI+AES-GCM 加密。改为：`internal/cdp`（手写最小 WebSocket，零新依赖）拉起**独立 profile** 的 Edge/Chrome + 调试端口 → 用户正常登录 → CDP `Network.getAllCookies` 读回。独立 profile 天然隔离，**多账号逐个添加互不干扰**（无需手动开无痕）。手工粘贴路径保留为兜底。见 `internal/login_glm/auto.go` |
| R28 | **自动登录的完成判据是「凭据验证通过」，不是「Cookie 出现」** | **实测发现**：清言对全新访客会自动下发 `chatglm_refresh_token`（423 字符），该 token 调 `user/refresh` 被拒（`访客账号不可用`）。若以「Cookie 出现」为完成判据，会抓到一个**永远不可用的访客账号**。故实现为：轮询 Cookie → 尝试验证 → 只有验证通过（非访客）才落盘收工；超时且只见到访客凭据时给出明确提示。见 `TestLiveGuestTokenBehavior`（实盘验证） |
| R29 | **多账号轮换的真实语义：粘性路由 + 错误惩罚 + 同请求换号**（2026-10-08 修订，PR #82） | 轮换**不是**「积分低就切」：① **粘性路由**优先复用上次成功的账号，直到冷却/禁用或连续 50 次成功；积分只在**选新号**时作排序键（临期 → 总余额）。② **错误惩罚照旧**（冷却/禁用/计错），但自 PR #82 起**账号级错误改为同请求内自动换号**：`ErrKind.Rotatable()` 族（限流/欠费/登录态死/404/5xx/账号故障/ErrClient）在惩罚后 continue 换下一个候选（`MaxRotate` 默认 3 封顶）；内容类错误（content_blocked/prompt_too_long/passthrough）**仍透传不换号**——换号必然复现；候选全部耗尽时透传**最后一个上游原始响应**（保 6004 重置时间等细节）而非包装 503。只有**传输层错误**照旧不计 Rotatable、直接 continue 换号。**变更动机**：旧「错误下次生效」语义下，客户端重试又被粘性路由钉回同一池，本可避免的失败原样甩给客户端（2026-09-23 生产事故：13 个健康号在场却返回 429）。单账号渠道（`SingleAccount`）完全豁免不变。回归测试 `internal/server/glm_rotation_test.go`（旧断言已按新语义更名改写）、`rotate_on_account_error_test.go` |
| R30 | **清言「伙伴/群聊」任务不自动化** | 接口已探明（`mainchat-api/claw_agent` 完整 CRUD，`claw` = 「伙伴」），但**决定不实现**：① 每天 500 积分（≈5 积分实际价值）vs. 风控风险不对称；② 这些任务的设计意图是引导**真人**使用产品，脚本刷属典型薅羊毛特征；③ 用户手动点两下成本极低。**故本渠道自动化边界 = 保活对话 + 积分读取，不碰创建伙伴/绑定 IM/群聊等写操作** |
| R31 | **每日登录积分走 `member-api/member/daily_login_score`，已接入保活流程** | 初版探测的路径名**全错**（`login_bonus`/`daily` 均 404），真实端点是 **`daily_login_score`**（2026-09-26 从 Web 主包挖出）。主包里它是**页面加载时自动调用**的（`errorMessageShow:false`）⇒ 调用它 ≈ 打开一次网页，特征与网页端一致，**不是跨端伪装**。响应：`status=0` 领取成功；`status=10001 "今日已领取"` = **幂等非错误**。**故无论上游是「自动发放」还是「需主动调用」，调它都安全且有益**。⚠️ **实现陷阱**：`doJSON` 在 `env.Status != 0` 时抛 error，会把 `10001` 误报成「领取失败」→ 新增 `doJSONEnvelope`（只把 HTTP ≥400 与非法 JSON 转 error，业务码交调用方解释）。回归测试 `internal/glm/daily_score_test.go` |
| R32 | **不模拟 App 登录来触发额度发放** | 用户设想「模拟 App 登录动作」以避免手动开 App。**实测否决**：① Web 端**无**任何额度激活接口（8 个候选全 404）；② 换 App 头访问同一接口反而 401（`member_info` Web 头 200 / App 头 401）⇒ App 走**另一套认证**（设备指纹、App 签名等），不是加 UA 就能冒充；③ App 域名 `api.chatglm.cn` 独立存在。**风险不对称**：模拟 App 登录是**跨端伪装**，风控风险比只读 Web 私有接口高一个量级，且触发额度发放正是薅羊毛特征（与 R30 同一逻辑）。**结论：手动开 App 登录一次即可**（一次性成本，零风险，零开发） |
| R33 | **三个 chatglm 变体的模型名带上游代号后缀 `:moe_53f`；旧名保留为别名** | **实测**：6 个模型名逐个测服务端上报的 `parts[].model` —— 三个 chatglm 变体（普通/`zero` 推理/`deep_research` 沉思）**都上报 `moe_53f`**，即**同一底层模型的不同推理等级**（`moe`=MoE 架构、`53` 很可能指 5.3）；search=`ai-search`、ppt/video=`all-tools-glms-glms-v2`。故给三个变体加 `:<model>` 后缀（`glm/chatglm:moe_53f` 等），**search/ppt/video 不改**（其代号是工具标识非模型版本）。**两个必须同步的点**：① `resolveAssistant` 查表前**剥掉 `:` 后缀**（同时保留完整名查表），否则 `chatglm:moe_53f` 只能靠兜底命中；② **旧名保留为别名**，已配置旧名的客户端不断，但 `/v1/models` 只列新名。新增 `UpstreamModel()` 辅助函数。回归测试 `TestResolveAssistantStripsUpstreamSuffix` 断言带/不带后缀解析结果一致 |
| R34 | **新增渠道必须补 go xxxSch.Run(sctx)，否则该渠道的自动签到/保活从不运行** | **2026-09-27 实测发现**：加 glm 渠道时创建了 glmSch、注册进 runtimes、设了观察者，**但漏了 Run()** ⇒ GLM 的自动签到与保活**从未执行**。危害特征：**不报错、不崩溃**，且**手工触发仍可用**（面板按钮 / RunCheckinNow 走的是另一条路径），故极难发现——用户是手工签到后才察觉。**回归测试** cmd/wild-work/scheduler_start_test.go：静态扫描「定义了 xxxSch := scheduler.New(...) 就必须有 go xxxSch.Run(」，并交叉校验 runtimes 里声明的 Scheduler 都已启动。**新增渠道清单应加一项：创建 → 注册 runtimes → 设观察者 → **go Run** → 补测试** |
| R35 | **流式请求必须用独立的 StreamHTTP（不设 Client.Timeout）** | **2026-09-27 生产日志实证**：GLM 对话流走带 Client.Timeout 的 client，触发 context deadline exceeded **5 次**（09/26 22:42、09/27 00:03/00:09/00:12/00:18），**且无终止帧** ⇒ 客户端表现为「回答到一半停住」。根因：Go 的 Client.Timeout **覆盖整个请求生命周期（含读 body）**，对 SSE 意味着「长回答必被掐断」。**修法**（照 traework 既有模式）：Client 加 StreamHTTP *http.Client{Transport: tr}（**共用 Transport 复用连接池、但不设 Timeout**），ChatStream 改用它；main.go 两处 applyProxies 都要把 {glmUp.HTTP, glmUp.StreamHTTP} 一起传（SetTransportProxy 会**新建** Transport，只套一个会让流式漏掉代理）。**非流式 client 仍保留 Timeout**（一问一答需兜底）。回归测试 internal/glm/stream_timeout_test.go：证明流能跑过 HTTP.Timeout，且对照证明非流式仍会超时。**2026-09-29 推广到全部流式渠道**（issue #42）：此前只剩 glm/traework 有 StreamHTTP，qwenwork/qodercn/qodercom/qoder/workbuddy/ workbuddyai/oczen 的 ChatStream 全用带 Timeout 的 HTTP ⇒ 长流式同样会被掐断。现已给全部 7 个渠道补 StreamHTTP（共用 Transport、不设 Timeout，流式里 Select{StreamHTTP 非 nil 则用之}），main.go **两处** applyProxies（启动 + 面板热更新）都要把 {HTTP, BillingHTTP?, StreamHTTP} 一起传，否则只套 HTTP 会让流式漏掉代理。**2026-10-14 修订（R57）**：按渠道套代理的机制已整体删除，`applyProxies`/`SetTransportProxy` 不复存在，接线表只剩 `wrapDumps`（devdump）**一处**且仅在启动跑一次 ⇒ 本条的「两处都要传」风险自然消失；但「流式必须用不带 Client.Timeout 的 StreamHTTP」本身不变，且备用通道取用的 client **同样不得设 Timeout**（`backuplink.Client` 已按此实现）。 |
| R36 | **流被截断不得靠补 `data: [DONE]` 伪装成正常收尾**（issue #42） | **实证**：qwenwork/qodercn/qodercom/qoder 四份 sse.go 的 `sawDone` 是**死变量**（声明后从不赋值）⇒ 上游连接中断/读超时被掐断、或断在半个帧上（末行无换行），出口照样补 `data: [DONE]`，客户端拿到半截 tool_call arguments 报 "tool input was not fully received"，而网关日志**无任何错误**，极难排查。**修法**：parseNestedSSE 增加 `truncated` 出参（读错误/半帧 → true），`streamAsOpenAI` 里截断时改发一帧 OpenAI 规范 error（`code: upstream_truncated`）且**不发** [DONE]——调用方 `err!=nil` 同时保留。通用路径上游/sse.go streamCore 也做了同样的截断→error帧改造。刻意保留的行为：上游「正常 EOF 但漏发 [DONE]」（帧完整、末行有换行）仍兜底补 [DONE]，避免误伤本就补的渠道。回归测试：qwenwork/qodercn/qodercom/qoder 各 `stream_truncation_test.go` + 通用 `TestStreamTruncationNotDisguisedAsDone`，覆盖四种输入（传输层错误/半帧/正常 [DONE]/EOF 漏发） |
| R37 | **TraeWork AuthCode 交换：4xx 属终态，不得换 origin 重试掩盖首因；设备号作用域 = 账号**（issue #50；2026-10-08 二次修订 issue #60/#70） | 旧实现 `ExchangeAuthCode` 对 4xx 也 `continue` 试下一个 origin，把首选 api.trae.cn 的真实错误（403/20401 设备数上限）覆盖成回退 origin 的 400/10101「无效参数」后再打印 ⇒ 表象彻底指向参数写错，且 authCode 一次性、4xx 后重试纯属刷屏。**修法**：4xx（除 429/408）立即返回携带**首个** origin 真实响应的 `*AuthCodeRejectedError`（含 origin/status/body）；`IsDeviceLimitReached` 单表识别 20401（可行动提示「去其它设备登出释放名额，最多 10 台」）；`login_trae.Poll` 把终态错误固化到 `login-state.json` 的 `Err` 并清空 AuthCode，后续轮询短路不再每 2s 重消耗。**设备号持久化**（初版）：官方客户端 `getDeviceId()` 是持久的，本仓每次登录 `randHex(32)` 随机新生成 = 每点一次都是新设备。改为从 `device-id.json`（`login-state.json` 同目录、独立文件）复用，缺失才生成并落盘（不复用 login-state，它登录成功/取消即删）。**二次修订（2026-10-08，#60/#70 实证推翻本机级共享）**：`device-id.json` 让一机多号共享同一设备号——#70 实测 15 号对照：共享设备号的 3 个账号全部撞 4017 common risk control（仅对话入口被拒、积分正常），一机一号的 12 个零风控；#60 实测同一编造设备号被多账号绑定后新账号授权恒 20401 Device limit reached（换设备号立即成功）。**修法**：设备号作用域从「本机」改为「登录会话」——每次 `Start()` 新生成一对 machineId/deviceId，登录成功随 `auths/trae-<uid>.json` 落盘成为该账号的稳定注册设备（满足 #3 的 x-device-id 稳定性，签到/对话头从 auth 文件读、不经过 Start）；`Poll` 成功后顺手删除遗留的 `device-id.json`（老用户自愈）。**#60 建议的「读官方客户端真实设备号」不做**：需移植 storage.json TC 解密且依赖本机装官方客户端，而 #70 数据已证明随机号本身可用，会话级新号即可消解两者。**权衡披露**：同账号重登会消耗一个设备名额（上限 10），但远低于旧随机版的消耗速度。回归测试 `internal/login_trae/login_device_test.go` |
| R38 | **上游按模型限流时不得软化整个账号**（issue #53） | **2026-09-29 最小修复（不做大架构改动）**：WorkBuddyAI 上游模型级 429（6004 / body 含 `switch to the other models` / `usage exceeds frequency limit`）在 `Classify` 里判为 `ErrPassthrough`（请求级、不冷却账号），透传原文让客户端按 Retry-After 自退避或换模型——**不**把该账号其它还能用的模型一起拖进 SoftCooldown。与 R16 单账号渠道同哲学。**不做的部分**：pool 升级到 model 维度状态、`stickyKey(kind)`→`(kind,model)`、state v4（issue 作者提议的两种更大改造），它们动 pool/state/handler 三核心、~370 行，需单独评估（作者已主动提出可拆两个小 PR）。注意此判据仅限 WorkBuddyAI：其它渠道的 429 语义仍是账号级。**2026-10-14 修订（PR #114）**：当时“只有账号级冷却一种手段”的前提已不复存在——#82 引入 `Pool.CooldownModel`（账号×模型粒度）、#106 引入降级链后，本条改为 **`ErrSoftRate`**：handler 走 `CooldownModel` 只冷却「账号×该模型」到响应体重置点，同号其他模型不受牵连（#53 的担忧已被模型级冷却化解），且模型全冷时降级链自动顶替、重置后自动切回（`softRateResetTs` 补识国际版英文形态「will reset at <ts> UTC+8,」）。修订前 `ErrPassthrough` 什么都不冷却 ⇒ 「模型全冷」判据永不成立 ⇒ 降级链永不触发，客户端只能对着重置时间干等 |
| R39 | **`scheduler.Config` 的时间字段：`nil` = 未配置（落默认），`[]int{}` = 本渠道没有这类定时任务** | **2026-09-23 实证**：`scheduler.New` 原用 `len(...) == 0` 判默认值，把两者混为一谈 ⇒ 传 `nil` 想关掉定时任务的渠道被静默补上默认时间。`main.go` 里命中四处：**qoder / oczen** 每天 09:00、21:00 各跑一次必然失败的签到（面板按 `last_checkin_ok` 渲染出红色「签到失败」标签，消息是「qoder 暂无签到活动」/「no refresh token」）；**workbuddyai** 注释写「Keepalive 关闭」但 22:00 照常每日刷 token；**qwenwork 最严重** —— 注释明写「定时保活反而会与千问办公 App 互踩」，而保活批次每天照跑。同一个「无签到活动」概念在 `app.go` 有 `noExplicitCheckin()` 守卫（手动签到、首次签到路径都据此跳过），唯独 `scheduler.New` 的零值兜底把它抹掉。**修法**：两处 `len(...) == 0` 改 `== nil`；四处调用点改传 `[]int{}`；`Run()` 补「两个列表都为空时阻塞等 ctx/配置变更」—— 否则 `nextFireMinutes` 返回零值走 `IsZero` 兜底，变成每天 1440 次空转。回归测试 `internal/scheduler/scheduler_defaults_test.go`（零值落默认 / 显式空关闭 / `CheckinHours` 旧字段兼容 / 无任务时 `Run` 阻塞且响应取消） |
| R40 | **流内业务错误不得伪装成正常收尾；兼容层不得吞 error 帧**（2026-10-03） | **背景**：traework 的 `event:error`（3004 限流/1005 权益）旧实现写成 `delta.content` + `finish_reason:stop` + 补 `[DONE]`，且函数返回 `nil` ⇒ 客户端看到「正常结束、内容莫名其妙」、agent 拿半截回答继续跑，且 handler 无从得知失败 ⇒ **账号不冷却**；更糟的是 `NoteSuccess`/`stickySuccess` 在读流**之前**已执行，粘性路由把请求钉死在中招账号上，重试必然再撞。**修法**：① `solosse` 错误分支改发 OpenAI 规范 error 帧（限流标记 `upstream_rate_limited`）、**不补 `[DONE]`**、把错误返回调用方；② 新增 `provider.StreamErrorClassifier`（`Kind()`），handler 用 `errors.As` 取值后按 kind 冷却账号（软 60s / 硬 12h / 禁用），粘性路由见 `status.Cooling` 自动让位，无需显式 `stickyClear`；③ **兼容层必须识别 error 帧**——`gateway.parseChatSSELine` 此前只认 `choices`，error 帧被静默丢弃，致 `/v1/messages` 发 `end_turn`+`message_stop`、`/v1/responses` 发 `response.completed`+`[DONE]`，把失败伪装成成功（该缺口对内层所有渠道通用，含 R36 的 `upstream_truncated`）。**协议合规要点**：Anthropic `error.type` 是 **9 元判别联合**（`api_error`/`rate_limit_error`…），**不得填内层私有码**——限流映射为 `rate_limit_error`；Responses 用 `response.failed`（不补 `response.completed`/`[DONE]`），`response.error.code` 亦须映射到枚举（`server_error`/`rate_limit_exceeded`），私有码只放 `error` 事件的自由 `code` 字段。**3004 的限流维度是待强化假设**（R23b）：观察仅 1 例且来自 checkin 路径，只足以排除 IP/全局级；因软冷却仅 60s、判据不成立时最坏影响是闲置一分钟，故按账号级处置。回归：`traework/solosse_test.go`、`server/stream_error_penalty_test.go`、`gateway/stream_error_frame_test.go`、`scripts/ui-static-check.mjs` |
| R41 | **小浣熊协议回调必须由 `main.go` 在初始化前拦截（`--raccoon-callback`）**（2026-10-05） | **背景**：小浣熊「浏览器授权登录」的授权码经自定义深链 `office-raccoon://auth/callback` 回传，实现方式是登录期间临时改写 HKCU 的 `office-raccoon` 注册表命令行为 `"<wild-work.exe>" --raccoon-callback "%1"`。`internal/raccoon` 把 `CallbackFlag` 与 `SaveCallback()` 都写好了，**但 `cmd/wild-work/main.go` 从未解析该 flag** ⇒ Windows 唤起回调子进程后，它当作普通启动又拉了一份 daemon：端口被占（`listen … bind: Only one usage…`）、**且它的启动自愈看到「残留的协议改写」立刻 `RestoreProtocol`+`ClearCallback`** ⇒ 授权码从未落盘，常驻进程轮询到 5 分钟超时。**全程不报错不崩溃**，用户只看到「点完授权但账号没加进来」。**修法**：`main.go` 在 `os.Chdir(workDir())` 之后、**任何初始化（config/日志/服务/托盘）之前** 拦截 `handleRaccoonCallback(os.Args[1:])`，命中即「落盘后 `os.Exit`」，绝不启动第二份服务。**同时**：`windowsgui` 构建无控制台，回调结果必须追加写进 `data/app.log`（否则失败时零线索）。**同类教训**：这与 R34（新增渠道漏 `go xxxSch.Run()`）同族 —— **「声明了却没接线」的疏漏在源码层就能判定**，故补静态回归 `cmd/wild-work/raccoon_callback_test.go`（断言 main.go 调用了 handler、位置在服务初始化前、分支含 `os.Exit`），去掉修复即失败。**附带**：`/api/state` 新增 `login_error` —— 此前前端只看 `login_busy`，把**所有渠道**的登录失败/超时都显示成「登录完成」，正是它掩盖了本次问题；现由 `peekLoginError()` 非破坏式回传真实原因（读取即清空会被 `/api/state` 的其它调用方偷走） |
| R42 | **渠道本地模型校验必须取「静态表 ∪ 动态目录」的并集**（2026-10-05） | **背景**：raccoon / loomy 都做本地模型名校验（上游对未知模型名**静默回落到默认模型**并返回 200，不校验会让用户以为在用 A、实际扣 B 的额度），但实现只查**静态表**（`KnownModel(id)`）⇒ 上游目录新增的模型会被本地 400 误拒，而**同一个模型正被 `/v1/models` 正常列出**（后者读的是动态目录 `FetchModels` 结果）——表现为「面板列出却调不动」。**实测（2026-10-05）**：raccoon 账户 `/v1/models` 列出 9 个模型，其中 `sn-sensenova-6-8-flash` 调对话被本地 400 `model_not_found`，但直连上游同一模型 **HTTP 200 正常服务**（`sn-sensenova-6-8-flash-lite` 在静态表里、`sn-sensenova-6-8-flash` 不在，二者仅差一个后缀）。**修法**：`fetchCatalog` / `fetchModels` 成功后把目录里的模型名记进 Client（`liveIDs`，**单调扩大、只增不减**——避免目录瞬时抖动把可用模型判成未知），`ChatStream` 改查 `c.knownModel()`（静态表 ∪ liveIDs）。两渠道同款修复 + 回归测试 `TestKnownModelAcceptsLiveCatalog`（去掉并集即失败）。**顺带**：给两个 Client 加 `Base` 字段（默认常量端点，测试注入 httptest 假上游），避免为写这条测试而把 `LLMBase`/`GatewayBase` 从 const 改成 var |
| R43 | **运行统计（`internal/stats`）与用量流水（`internal/ledger`）是两套并列口径，不合并、不互校**（2026-10-07，PR #71–#73） | **背景**：新增「运行统计」tab 时引入了第二套统计面。分工：`ledger` = 磁盘 JSONL 双流水（`data/ledger/{usage,credit}-*.jsonl`），账号**条目差分**口径，按需扫描聚合（R17）；`stats` = 内存实时 + 按日归档 `data/stats.json`，「今日」口径——消耗走**余额下降差值**、收入走**日志签到事件**（`events.go` 正则解析 `app.log`，因「登录即自动签到」等场景条目晚于入账建立，差值恒 0 漏记）、逐请求 token 走 **usage 帧精确值**（`usage.go`）。**决议**：两者**零共享状态、零互相写入**（stats 仅只读 `ledger.Query(1)` 取当日作废），各自标注口径、互不引用；**数字天然不同是设计而非缺陷**（差值含积分包到期作废，ledger 已拆 spend/expire），**不追求对齐**。**边界**：`ledger` 仍是面板「用量与积分」的数据源与 issue #67 等待修的口径；`stats` 只服务「运行统计」tab。新增统计需求时**先明确归属**（精确拆分→ledger；实时/今日/趋势→stats），**不得**为对齐数字而改动另一侧。**注入纪律**：stats 全部数据输入走**进程内直调**（`allStatuses` / `handler.StickySnapshot` / `ledger.Query`），**不得** HTTP 自环（R13）；`main.go` 须 `go appInst.StartStatsFeeders(sctx)`（漏接线即静默无数据，同 R34 家族） |
| R44 | **小浣熊登录改用「网页版 CDP 捕获」，废弃桌面协议回调**（2026-10-10；2026-10-15 上游迁站后入口与凭据 cookie 名更新，见 R67） | **背景**：原「浏览器授权登录」靠改写 HKCU 的 `office-raccoon://` 协议注册来接住深链授权码（R41），仅 Windows 可用、要求本机装官方客户端、且与客户端抢协议。**实测**（`ref/xiaohuanxiong_web_20261010.saz`）：网页版登录成功后前端 JS 会把凭据写进 Cookie `raccoon_refresh_token`（域 `.xiaohuanxiong.com`），且该凭据与桌面端**同一套**（JWT 无平台声明）——实测目录/余额/推理/刷新四项全通，`model_catalog` 的 **9 个模型全部可用**。**修法**：`internal/login_raccoon` 改为网页版流程（`web.go`）——拉起**独立 profile** 的 Edge/Chrome（复用 `internal/cdp`）打开 `office.xiaohuanxiong.com/home?loginModal=true` → CDP 轮询 Cookie → **判据是 JWT `owner_type==users`**（访客也下发 Cookie，见 R28 同款教训）→ 调 `/api/web/auth/v1/refresh` 换取并落盘凭据。**跨平台**（纯 HTTP + 系统浏览器，不再依赖注册表）；桌面协议登录代码（`protocol*.go` 的 Hijack/Backup/restore、`--raccoon-callback` 入口）**保留但不再调用**，仅作旧版注册表残留的启动自愈（`healRaccoonProtocol`）。**「从客户端导入」路径保留**（`import_local.go`）。**已知限制（面板弹窗已告知）**：上游 refresh **轮换 refresh_token**，若同账号凭据被本工具与浏览器/客户端**共用**会互踢；要并存须用本流程**单独登录一次**（新建独立会话，`sid` 不同）。回归测试 `internal/login_raccoon/login_test.go`（owner_type 甄别 / Poll 状态机 / 并发拒绝 / 幂等 Shutdown） |
| R45 | **明细与刷新路径同源回写 pool 快照（`creditSnapshot`）**（2026-10-11） | **背景**：面板「同屏两个数字对不上」——账号卡片/汇总条的积分与 hover 明细弹层的数字不一致。**实测**：同一响应内上游汇总字段==明细池相加，确认非上游口径漂移；差异实为时差——旧实现里 pool 快照只被「刷新事件」（手动刷新/全部刷新/自动循环）更新，而明细是 hover 时的实时值，两者必然相差区间内消耗量。**修法**：`app.creditSnapshot` 取数成功即 `SetCreditDetail` + ledger 差分（`creditTotals` 与 `ResourceDetail` 共用，删掉三处各自回写）；`ResourceDetail` 扩展返回 expiring/unusable，`/api/account/resource_detail` 响应新增 `expiring_remain`；前端 `showCreditDetail` 用响应的 `usable_remain/expiring_remain/unusable_remain` 回写卡片与汇总条（`data-uid` 定位，不全量重绘以免丢失正在 hover 的 tooltip 锚点），`checkin/refreshOne/checkinAll/refreshAll` 成功后清 `detailCache`。回归测试 `TestResourceDetailWritesBackPoolSnapshot`（去掉回写即失败） |
| R46 | **Cline 渠道（`cline/*`）= 多账号官方 API Key 池 + 免费模型调度器**（2026-10-10；入口路径 2026-10-12 用户订正） | 凭据是**官方文档化的 `sk_` API Key**（入口：`app.cline.bot` → 注册账号 → `Account / API Keys` → `Create API Key`；旧记「Settings → API Keys」有误），**不是** cline2api 那套 WorkOS device-code OAuth：实测两者模型面等价，而 Key 不过期、**不轮换** ⇒ R26/R44 的「同凭据多处使用互踢」在本渠道不存在，且零登录编排。**必带非空 `X-CLIENT-TYPE` 头**（值随意，实测 `bogus-value` 也放行；缺失/空值 ⇒ 全部 `cline-free/*` 返回 403 `API_REQUEST_ERROR_CODE`，且**不报错不崩溃**、只表现为免费模型消失，属 R34 家族的静默失效；由 `TestChatStreamAlwaysSendsClientTypeHeader` + live 的 `TestLiveAliasNeedsClientType` 双重锁死）。模型面**不做任何本地假设**（2026-10-12 简化，用户裁定）：**绑死上游** `recommended-models` 的 `free[]` 数组**原样透传**——ID 保留 `cline-free/*` 别名不改写（不用厂商真实名：用户在 Cline 后台实测，免费入口是别名条目而真实名会扣费，但计价逐请求抽签，本渠道不承诺价格）、不做基名去重、不按 tag/后缀过滤；**删除**硬编码静态表与 `/api/v1/models` 拉取（458 条 OpenRouter 镜像对免费池定位无信息量）。本地校验集 = 暴露集 = `free[]`（构造即一致）；**冷启动放行**（目录未拉到过时不拦，否则 daemon 重启后第一批请求全被本地 404，放行的最坏结果只是让上游给权威错误）；上游抖动时用已知集兑底不掏空面板。上游**确无余额/额度端点**（`/credits` `/usage` `/billing` `/quota` `/me` 等 12 个候选实测全 404，只有 `/api-keys` 与 `/plans` 存在）⇒ `UserResource` 恒 0 且**不得返回 error**，面板积分位「不适用」（`creditsNAKind`），实际花费只来自逐请求 `usage.cost`（入 ledger）。签到/保活/定时任务**全关**（`CheckinHours`/`KeepaliveHours` = `[]int{}`，R39）+ **`go clineSch.Run(sctx)`**（R34）；`noExplicitCheckin` 含 Cline。**不承诺价格**：`free` 命名空间与 `is_byok`/`provider` 实测**都不能**当判据（同前缀有扣费、非 free 后缀反倒 cost=0，且同一 ID 相邻两笔计价会漂移）⇒ 费率表标「不适用」，一切以实测 `usage.cost` 为准。面板文案必须写明「额度按**账号**计（官方字段 `…UsageCostUSDPerUser`），同账号多把 Key 共享一份额度」，并在检测到同账号时告警（依据 `/api-keys` 返回本账号全部 Key）。**2026-10-12 压测订正三条**：① **发往上游的 body 必须强制 `stream:true`**——handler 对上游响应一律按 SSE 解析（流式 Stream / 非流式 Aggregate 都只认 `data:` 帧），客户端发 `stream:false` 时上游回非流式 JSON 信封 → `errEmptyStream` → 502 `upstream_parse` → 并当作账号连续错误冷却整条渠道（实测一次非流式请求后 4 个免费模型全部 503 约 60s）；`sse.go` 的 Aggregate 注释早已假定该不变量却未落实，已补 `forceStream` + 回归测试。② **地区限制模型不得罚号**：`muse-spark-1.3-contributor` 国内直连恒回 403 `not available in your region`，须归 `ErrPassthrough`（原先落 ErrClient 可轮换 ⇒ 重试耗尽冷却账号，同账号其余 3 个模型一起挂）。**2026-10-14 补充（R57）**：不罚号之外，现在还会在**同一次请求内**自动经「备用上游通道」重放一次（配了才重放；未配/重放也失败 ⇒ 仍透传直连首因）；判据是单独的保守表 `regionRetryMarkers`，**不等于** `regionBlockedMarkers`（后者宽包 `access forbidden`，只用来决定惩罚口径）；③ **上游不返回上下文/能力字段**（free[] 仅 id/name/description/tags），客户端靠 `/v1/models` 的 `context_length` / `architecture.input_modalities` 判断压缩时机与能否收图，故本渠道维护 `measuredCaps` **实测表**（三暗号头/中/尾回忆验证排除静默截断；>700K prompt 首字节 >90s 会被本工具 90s 响应头超时中断，取保守下界；`ContextFromAPI` 保持 false 不冒充上游声明）：实测值为「本工具 90s 响应头超时」压制下的下界，按用户裁定 2026-10-12 **向上取一档**：mimo 实测 728K→报 1M、muse 实测 928K→报 1M、step-5 实测 800K→报 1M、solar 实测 400K→报 512K（客户端按 context_length 判断压缩时机，略宽报比偏窄更贴合真实容量，超限时上游会给权威错误）；图像能力：mimo/step-5/muse ✅、solar ❌（上游 404 no image endpoints，muse 另需备用上游通道，见 R57）；四者 tool_calls 均实测通过。**2026-10-15 裁定（PR #116 评估，已关闭）**：本渠道**只认官方 `sk_` Key，不接受 WorkOS refreshToken 号池**——桌面个人工具下 Key 严格占优（不过期、不轮换、添加零消耗、无刷新链路），refreshToken 的收益只在大号池兑现，而免费模型实测多走 Cline→OpenRouter 共享出口（R47），撞共享日限时换号必复现，大池扩容本身存疑；批量 refreshToken 的来源（第三方注册机）也与 R30/R32 的自动化边界冲突。**留存的实测事实**（将来若做 device-code 自动登录可直接复用，勿再挖一遍）：WorkOS OAuth 换出的 access token 写进 `Authorization` 时**必须带 `workos:` 前缀**（裸 token 恒 401，与官方 SDK 的 `WORKOS_TOKEN_PREFIX` 一致），而 `sk_` Key 反过来**不能**加前缀；`POST /auth/refresh` body 为 `{refreshToken,grantType:"refresh_token"}`，`expiresAt` 实测是 ISO8601 字符串（参考实现 `ref/cline2api` 按 `any` 承接，说明形状不确定，强类型 string 会让整次刷新反序列化失败）。 |
| R47 | **Cline 的 429 分两类：OpenRouter 侧共享上限不得软化账号**（2026-10-10，R38 同族扩展） | 上游把真实状态**压平成 HTTP 500**（`… from Openrouter: request failed with status 429 …`）⇒ `Classify` 必须正则取**内层**三位状态码，否则 429 被归成 5xx（违反不变量 15 的「429 优先」）。挖开后是两种语义相反的 429：① **共享出口上限**——`limit_rpd` / `Daily limit reached` / `temporarily rate-limited`，落在「Cline 自己的 OpenRouter key」上（路由判据：响应 `id` 前缀 `gen-` = OpenRouter，`gen_` = Cline 自有网关，实测 9 个 ID 零反例，是成功路径上**唯一可用**的聚合器标识；`is_byok`/`provider` 均已被证伪）⇒ **换 Cline 账号必然复现** ⇒ 归 `ErrPassthrough`（不计账号错、不冷却、不换号），并把该 model 记入渠道内 `capped`（期间从 `/v1/models` 摘除，TTL 1h 自愈；实测 `gemma-4-26b` 上轮 429、本轮 200，故必须可恢复）；② 无上述文案的 429 ⇒ 才是真账号级（依据 `plans` 的 `…UsageCostUSDPerUser`）⇒ `ErrSoftRate` 冷却 + 同请求换号（R29 全量适用，本渠道**不置** `SingleAccount`）。**不区分的后果**：渠道只有 3 个模型，`pool.CooldownModel` 在 ≥2 模型冷却时升级整号冷却 ⇒ 所有 Key 被逐个轮空 → `503 no_healthy_account`（正是 R16/R38 的误伤形态）。另一族必须处理的是 **HTTP 200 + 流内 `error` 帧 + 上游自己发 `[DONE]`**（`stream_initialization_failed`，实测另有数字 `502`）：R36 的「截断时不发 [DONE]」在此不适用（上游已发），需要识别 error 帧 → 返回 `StreamError`（实现 `StreamErrorClassifier`，R40）；为此在 `internal/upstream` 新增最小导出 `StreamWith`（同时带 hintFn 与 onUsage 两个钩子，既有三个导出各只带一个）。`max_tokens` 实测：8→上游 400、16→500 `empty response content`（推理吃光预算 ⇒ 可见内容为 0）、64~200000 全通、1e6→拒 ⇒ `PrepareBody` 缺省**不传**、<64 抬到 64、>200000 压到 200000；**强制 stream=true 并不能豁免这条**。 |
| R48 | **`normalizeToolTurnContent` 给 assistant 补 content 不再以 tool_calls 为前提**（2026-10-11） | **实测**：CodeBuddy 客户端在「思考中途被打断」时，会把半截思考存成 `{"role":"assistant","reasoning":"…"}`（**无 content、无 tool_calls**）；该空壳留在历史里后，traecode/SOLO 对**每一发**请求都回流内 `event:error` `4001 the param is invalid`（与「中毒进历史」同类，重启客户端也不恢复，只能新建会话）。旧实现只在 assistant **带 tool_calls** 时补空串，普通 assistant 刻意放过（注释「无证据会出问题」）⇒ 空壳原样透传。**修法**：`case "assistant", "tool"` 一律补 `content:""`，不再判 tool_calls。**重放坐实**（一份 129 条消息的完整会话，同一条畸形消息）：原样 → 0.6s 即 4001；删掉空壳或补 `content:""` → 均正常出流 ⇒ 上游接受空串，故补空串（保留历史结构）而非丢消息。回归测试 `internal/server/handler_tool_content_test.go`（含 reasoning-only 空壳用例，断言 reasoning 字段不被误删） |
| R49 | **思考档位只透传上游声明，绝不编造**（2026-10-12，issue #96） | 用户要求数据面透出各模型的思考档位（Buddy2api v2.1.11 同款能力）。**原则**：`provider.ModelInfo` 新增 `ReasoningEfforts`/`DefaultReasoningEffort`/`CanDisableThinking`（三态指针），**上游目录声明了才透出，未声明时整个 reasoning 对象不出现**——把「未声明」显示成任何具体档位都是编造（同 ContextFromAPI 哲学）。接线三处：workbuddy CN（`/console/enterprises/personal/models` 的 `reasoning.supportedEfforts/defaultEffort/canDisableThinking`，deepseek 系只有老式 `effort` 默认档时回退取它）、workbuddyai（v2/v3 目录同形状，mergeV3 非空者优先）、loomy（`reasoning_efforts` 是上游权威档位，顺带 clampEffort 仍用静态表——目录抖动不该改变出站行为）。**透出面**：`/v1/models` 加 `supports_reasoning` + `reasoning{supported_efforts,default_effort,can_disable_thinking}`（Buddy2api 蛇形命名）；`/api/fees` 加 `reasoning_efforts/default_reasoning_effort`；面板费率表模型 ID 后 `[low·high·max]` 紫色小字标 + 模型搜索框同款。**清洗**：`provider.CleanEfforts` 去空白/去重/去空串/全无效归 nil（上游实测下发过 `"max","max",""` 这种脏列表）。qoder 系/qwenwork/glm/raccoon/oczen/cline/monkeycode 上游无档位声明，不参与。回归：`internal/upstream/reasoning_test.go`、`internal/workbuddyai/reasoning_test.go`、`internal/loomy/reasoning_test.go`、`TestModelEntryReasoning*`、`scripts/ui-static-check.mjs` 档位断言 |
| R50 | **思考链死循环（发癫）检测：流式逐帧判据 + 中断 SSE + 不罚账号**（2026-10-13，防癫需求） | deepseek v4.1 flash 等模型出现思维链死循环（复读/空转），需在抽象层实时拦截。**算法移植自 dsh-attention-health 的 `lib/guard.js`**（只取死循环检测，忽略健康评估/工作交接/会话日志）。新模块 `internal/loopguard`：四条判据（同行连续重复≥8 次且行长≥12 / 短行高频交替窗口 60 内同短行≥30 / 滑动窗口 40 行唯一率≤0.25 / 行内周期复读 p∈[2,40] 重复≥16 次且累计≥160 字），护栏同源移植（周期单元须含字母/汉字、十六进制字节周期豁免、代码围栏内豁免、pending 保留长度与探测长度同源）。**接线点（全渠道）**：`internal/upstream/sse.go` streamCore（workbuddy CN/AI、qwenwork、qoder、qodercn、qodercom、monkeycode、oczen、cline 九渠道）+ traework/solosse.go（TraeCode/TraeWork，deepseek-v4.1-flash 所在）+ qwenwork/qoder/qodercn/qodercom 的 streamAsOpenAI + raccoon/loomy 透传 Stream；**glm 跳过**（即将废弃）。**命中行为**：写一帧 OpenAI 规范 error（`code=upstream_reasoning_loop`）、**不补 [DONE]**、返回 `*loopguard.Error`。**不罚账号**：`*loopguard.Error` 刻意不实现 `provider.StreamErrorClassifier`——死循环是模型/请求级问题，换号必然复现，冷却只会把后续请求挡成 503 no_healthy_account（R16/R38 同族）；handler 只记日志、错误透传客户端，由下游 agent 自行重试恢复。**零误伤兼容**：只在 `reasoning_content` 非空时喂入（无思维链的模型/配置不经过守卫，零开销零误伤）。**配置开关**：`config.features.loop_guard`（默认 true）+ 面板「设置→思考链防癫」开关 + `POST /api/config/loop_guard` 热切换（atomic 全局开关，无需重启）+ env `WILDWORK_LOOP_GUARD=0`。回归：`internal/loopguard/loopguard_test.go`（正反例并重）、`internal/upstream/sse_loopguard_test.go`、`internal/traework` solosse 死循环测试、`internal/raccoon` 死循环测试。**2026-10-14 误伤修订（现场：agent 在思维链里正常生成 Go 代码，60 行里 30 行 `}` 被 short-line-repeat 掐断）**：DSH 原实现为思维链散文标定，未预见「思维链里写代码」。两层修复：① **围栏豁免从 ④ 扩展到 ②③**——原实现只豁免行内判据，围栏内代码的 `}` 短行照样进短行/唯一率窗口被误杀；现围栏内的行不进 ②③ 窗口（① 同行连续保持不豁免，对齐 DSH：连续 8 行相同长行即使围栏内也几乎必是发癫）；② **结构行豁免**（兜住无围栏裸代码，DeepSeek 系思维链常不挂围栏、fence 状态机管不到）——trim 后无字母/数字的行（`}` `{` `});`）与无空白/标点的连续词行（break/return）不进 ②③ 窗口；发癫短句必含标点/空白（「做。」、ok then.）不受影响。已知残留风险：`else {` 这类带结构符号的关键字组合仍计窗（密度低，遇实锤再收）。回归：`TestCodePunctShortLinesDoNotTrip` / `TestFenceExemptsWindowCriteria`（围栏内外同文本对照）/ `TestPunctDoesNotDiluteShortLineRepeat`（防修过头）/ `TestBareCodePunctLinesDoNotTrip`，双向验证（去掉任一豁免对应用例即失败）|
| R51 | **OpenRouter 系思考字段归一化到 `reasoning_content` 单字段下发**（2026-10-13，实抓定位） | Cline 免费池（`cline/*`，上游 Cline→OpenRouter→Meta）实测思考增量的字段名是 `delta.reasoning` 与 `delta.reasoning_details`（OpenRouter 归一化字段），**不是**本仓库历史唯一形态 `delta.reasoning_content`。旧白名单只认后者 ⇒ 思考文本被**静默丢弃**（客户端看不到思考、`usage.reasoning_tokens` 却非零），且 loopguard 在该渠道**永远喂不进**（判据只读 `reasoning_content`）——同一个根因的两面。**归一化**：新增 `upstream.reasoningDeltaOf`（优先级 `reasoning_content` > `reasoning` > `reasoning_details[type=reasoning.summary].summary`；三者同帧并存时内容逐字相同，实测拼接长度 179==179 且全等，取首个避免重复拼接；`reasoning.encrypted` 是回放用密文、无可读文本，不取）。方向选归一化**到** `reasoning_content` 而非透出 `reasoning`：前者是本仓库九渠道白名单、loopguard、gateway Anthropic 转换共同认定的唯一字段，且 `@ai-sdk/openai-compatible` 的读取式本就是 `delta.reasoning_content ?? delta.reasoning`（下游兼容两者，归一化后无客户端受损）。接线三处：`normalizeFrame`（流式逐帧）、`Aggregate` 的 delta 分支与 `mergeMessageFields`（非流式聚合）各改为调 `reasoningDeltaOf`；`loopguard.ReasoningDeltaOf` 同步兼容三字段（优先级一致，否则同一帧两包取值不同步）。回归：`internal/upstream/openrouter_reasoning_test.go`（实抓帧 fixture：summary 形态/encrypted 形态/正文/usage 四帧，断言归一化、去重、密文不外泄、聚合路径并入）、`internal/loopguard/openrouter_reasoning_test.go`（三字段提取 + 优先级 + 形状容错） |
| R52 | **面板「完整模型 ID」一律补渠道前缀，不得以「ID 含 `/`」判为已带前缀**（2026-10-13，PR #102 + 补漏） | **背景**：上游 ID 自带命名空间（Cline 免费档 `cline-free/*`）时，旧判据 `raw.includes("/") ? raw : chPrefix+"/"+raw` 误把上游命名空间当成本工具的渠道前缀 ⇒ 复制出的 ID 是 `cline-free/mimo-v2.6-flash`，而 `runtimeForModel` 按**第一个 `/`** 拆渠道 ⇒ `provider "cline-free" is not configured`。**契约**：客户端提交的模型 ID 必须是 `<渠道kind>/<上游ID>`，即 **kind 永远是第一个路径段**；上游 ID 自带斜杠时得双层路径 `cline/cline-free/*`，这与 `/v1/models` 的 `buildModelEntry`（无条件 `k.String()+"/"+mi.ID`）一致。**修法**：`chPrefix && !raw.startsWith(chPrefix+"/") ? chPrefix+"/"+raw : raw`（仅当裸名已以渠道前缀开头时原样保留，防重复拼接）。**接线两处（PR #102 只修了一处）**：① 费率表 `modelCellHtml`；② **模型查找面板 `collectAllModels`**——同一 `raw.includes("/")` 误判、同样把 `full` 写进 `data-mid` 供点击复制，PR #102 漏了它 ⇒ 从搜索框复制的 cline ID 仍不可用。故**两处必须同源**；任何新增「完整 ID」拼接点都要照此判据。回归：`scripts/ui-static-check.mjs`（费率表 + 搜索面板各一条断言，断言 `data-mid="cline/cline-free/*"`；去掉修复即失败，已双向验证）。**2026-10-14 修订（R59）**：本条的「kind 永远是第一个路径段」对 **Cline 改为直通上游命名空间**（对外 ID `cline-free/*`，不再拼 `cline/`）——见 R59；本条剩下的通用教训（«拼接规则必须单一来源、新增拼接点要同源»）仍然适用，现已把判据收敛到 `provider.PublicModelID/SplitModelID`，面板改为直接用服务端下发的 `m.id` |

| R53b | **OpenCodeZen 免费档会话头须是官方 `Identifier.descending()` 编码，非仅 `ses_` 外形**（2026-10-14，用户报 `403 FreeTierError`，上游官方 PR #69/#70） | **现象**：oczen 全部对话恒 403「OpenCode's free tier can only be used from within OpenCode」。**根因**：免费档不仅校验会话外形 `ses_<12位小写hex><14位Base62>`，还把 12 位 hex 时间戳段**解码**成官方客户端 `Identifier.descending()` 的值——`~(unix_ms<<12|counter)` 取低 48 位，故末三位是 `0xFFF-counter`；实测只接受 **counter 1–63**（末三位 `0xFC0–0xFFF`），超出即判非官方客户端 403。旧实现的时间戳段是 `sha256` 摘要前 6 字节（随机 hex），落进被拒窗口 ⇒ 恒 403。**修法**（移植上游 #70 终态，非 #69 的钟表铸造版）：`canonicalSessionID` 保留真正 descending 编码的官方 ID，其余（摘要段 / 升序 ID）确定性重编码——哈希前 6 字节按 `descendingTimePart` 渲染成 36 位时间戳移位进 48 位字高位、OR counter=1、按位取反（末三位恒 `ffe`，counter 落在允许窗口）。判定用 `descendingTimePartPattern = ^[0-9a-f]{9}f[c-f][0-9a-f]$`（覆盖 counter 1–63）。确定性 ⇒ 同一会话仍稳定映射同一 session（prompt cache / key 亲和不破）。**接线点唯一**：`internal/oczen/client.go` `canonicalSessionID`（ChatStream / TestKey / FetchModels 三处共用）。**合并期实测复核（2026-10-14，本仓）**：直连上游逐值对照——`ses_ecc5beb7cffe…`（counter=1）200、`ses_4892557ffffe…`（counter=48）200、`ses_ecc1f76c9f9b…`（counter=100）403、两个旧摘要段形态均 403 ⇒ 上表判据与修复均成立，非推断。回归：`TestCanonicalSessionIDKeepsGenuineDescendingIDs`（官方编码原样保留）/ `TestCanonicalSessionIDReencodesForeignEncodings`（摘要段+升序 ID 重编码且不保留原值）/ `TestDescendingTimePartLayout`（末三位 `ffe`）+ 实盘 `internal/oczen/live_test.go`（`-tags live` + `OCZEN_LIVE=1`，已实跑通过） |
| R53 | **usage 缓存计量只归一化「形状」不得改语义；取数逻辑收敛为唯一真源**（2026-10-13，issue #104） | **背景**：trae 系（traework/traecode）上游下发的是 **Anthropic 命名**的缓存字段（`cache_read_input_tokens` / `cache_creation_input_tokens`），而内层原样透传 ⇒ 下游按 OpenAI 规范解析（DSH 实测读法 `prompt_tokens_details?.cached_tokens ?? prompt_cache_hit_tokens ?? 0`）两条都命中不了 ⇒ **命中率恒 0%、prompt 全按未缓存计价**（以其价表 input 0.165 vs cacheRead 0.0033 计，费用高估约 50 倍）。**注意 issue 的定位不准**：它指向 `internal/upstream/sse.go`，但 trae 系根本不走那里（走 `internal/traework/solosse.go`），照抄会修不到。**决策一：归一化方向是「内层补齐 OpenAI 形状」，不是「让下游兼容 Anthropic」**——后者要改所有客户端，且与 OpenAI SDK / AI SDK / DSH 的实际读法相悖。新增 `provider.NormalizeUsageShape`（优先级 `prompt_cache_hit_tokens` > 顶层 `cached_tokens` > `cache_read_input_tokens`；**只在目标子字段缺失时写入**、**原始字段一律保留**、**认「字段存在」而非「值 > 0」**——0 是有效值）。接线：traework 流式在 **token_usage 捕获点**归一化（覆盖 onUsage 回调 / 返回值 / 不含 done 的异常收尾三条路径）+ 非流式 Aggregate 出口。**决策二：缓存取数收敛为唯一真源 `provider.ExtractUsageCache`**——此前面板 `server.cacheUsageOf`、`/v1/responses`、`/v1/messages` **各写一套优先级且已开始漂移**（两个转换器先读顶层裸 `cached_tokens`，而它在 workbuddy 系实测恒 0 是噪声，会把真值掩盖；且用 `> 0` 判定会把「明确未命中」丢成「未提供」，违反不变量 24）。收敛后三处必然同源。**决策三：`/v1/messages` 的 `input_tokens` 必须扣减缓存**——Anthropic 的 `input_tokens` 是「未命中且未写入」的那部分，客户端自行相加还原总输入；而内层 `prompt_tokens` **已是总输入**（命中是其子集）。**不下发 cache 字段**（费用高估）与**下发了却不减**（客户端求和近两倍）都是错的，故 `input_tokens = prompt_tokens − cached − write`，三段相加恒等于原 `prompt_tokens`（钳 0 防坏数据）。回归：`internal/provider/usage_test.go`（含 trae 真帧 / 显式 0 / 幂等 / 坏数据）、`internal/traework/usage_cache_shape_test.go`（双出口）、`internal/gateway/usage_cache_test.go`（responses：噪声字段优先级 + 显式 0）、`internal/gateway/anthropic_usage_test.go`（messages：三段守恒 + 非负 + 流式），四文件回退到修复前**新测试全部失败**（已反向验证） |

| R54 | **模型降级链在原模型无容量时触发（含整渠道全灭）：不粘滞自动切回，降级必须可感知**（2026-10-14，PR #106 + 收尾；2026-10-15 扩展跨渠道触发） | **需求**：主模型被时段性平台频控全池冷却（6004/14003）时 fail-fast 503 打断 agent 任务；用户可预配有序备胎链（`config.model_fallback[]`，`{from,to[]}` 全局唯一、不自环、`channel/model` 全路径，面板可视化编辑 + `POST /api/config/model_fallback` 热生效）。**触发判据**：`runtimeCapacity` 一次 `Pool.List()` 同时得 hasHealthy 与 modelAvail；**只要原模型无容量（`!modelAvail`）就查链，不再要求渠道内有健康账号**——①「模型全冷」（口径同 no_healthy_account 同名分支）与 ②「整渠道全灭」（全部禁用、或 pool 的「多模型接连限流升级整号冷却」）都触发；此时跨渠道备胎正是必需（渠道全灭时同渠道备胎会被 `modelHasCapacity` 自然跳过），否则只能 503。未配置规则时零探测（「正常路由零开销」承诺）。**不粘滞**：每请求按原模型重判，解冻自动切回（与 R29 分层：粘性是账号级、降级是模型级且用户显式配置）。**感知通道双保险**：① 响应头 `X-Model-Fallback: 请求模型 -> 备胎`（内层 WriteHeader 前设置；**兼容层必须在 `callChat` 统一转写**——/v1/responses、/v1/messages 响应体由网关重建且 model 回填请求名，不转写 = 对 Codex/Claude Code 静默换模型，PR 原版漏了这层）；② chat 面响应体 `model` 字段写备胎全路径（agent 读 chunk.model 即可感知）。**恢复探针只认 primaryAvail**：「未降级」≠「已恢复」——原模型仍全冷但备胎也没容量、或整渠道无健康账号时不得关事件（PR 原版 else 分支无条件调 NoteFallbackRecovery ⇒ 面板先显示恢复再重开一条、持续时长失真，已修）。**记账口径**：token 流水仍按客户端请求名（面向「这一发花多少」），实际执行模型走日志行 + 面板「降级记录」区块（内存环形 200 条、重启清零）。**已知限制**：无规则时不做容量探测 ⇒ 删规则后残留进行中事件不自动关（环形淘汰）；To 不做模型存在性校验（错名吃上游权威错误或本地 400）；备胎配额/余额不预判（容量仅看冷却态）。回归：`model_fallback_test.go`（触发/链序/热更新/切回/跨渠道隔离 + 恢复不谎报）、`stats/fallbacks_test.go`、`gateway/fallback_relay_test.go`（四端点转写 + 无降级不造头）、`scripts/ui-static-check.mjs` 面板接线 9 条 |
| R55 | **traework/traecode 模型元数据取上游下发；展示用字段必须容错承接，不得拖垮整张目录**（2026-10-14，PR #108 + 收尾） | **背景**：`poolEntry` 只解析 config_name + display_name ⇒ 两渠道 118 个模型全无 context_length/max_output_tokens、图片能力恒 false（客户端误判不能发图、或因上下文未知过度压缩）。**上游证据**（实测 `get_detail_param` HTTP 200/1.35MB/71 条）：`context_window_tokens.{dev,max}`、`display_config.multimodal`、`model_detail_list[].max_tokens`。**折算口径**（对齐不编造原则）：ctx 取 max 档、缺省回 dev、双缺省不填；max_tokens 为 0 不填；`ContextFromAPI` 仅在解析到容量时置 true；multimodal 直取上游布尔。**关键教训——爆炸半径**：强类型结构会因上游形状突变（对象→标量）让整批 unmarshal 失败 ⇒ `fetchModelsForPool` 报错 ⇒ 本渠道池整体拉不到；而这两个字段只是展示用增量信息，故用 `json.RawMessage` 承接、解析失败只丢该字段（同 qodercn.parseContextConfig 既有手法，不变量 5 同族哲学）。回归：`modelmeta_test.go`（容量折算四形态 + 三种形状突变，去掉 RawMessage 即失败）。`display_config.model_capability`（reasoning_model/chat_model）已解析未消费，仅记录上游形态 |
| R56 | **tool schema `$ref` 全量内联消毒：moonshot 系对任何保留的 `$ref` 都不认账**（2026-10-14，PR #109 + 收尾） | **背景**：Kimi/Moonshot 系（trae solo 后端）对 `tools.function.parameters` 含 `$ref` 即按「moonshot flavored json schema」展开校验，不收敛整体 400 `code=4027 detected infinite recursion`；官方客户端不发 `$ref`，代理客户端（经 MCP 接入）会发。**两版演进**：v1 只打断成环、保留非环 `$ref` —— 生产实测被否（3 个 `ref_before=1` 的 tool 放行后仍 4027）；v2 全量内联：所有本地 `$ref` 就地替换为目标消毒副本，展开链成环处（含 `#`）截断为空 schema（JSON Schema 语义 = 接受任意值），非本地/不可达/超预算（4096）同样截断，结果零残留；语义损失仅限递归分支处。**要点**：不按模型名门控（客户端可用不含 kimi 的自定义别名）；支持 parameters 预序列化字符串形态（原样透传会绕过消毒）；时序在 `normalizeTools` 字符串化之前；`$ref` 兄弟键按 draft-07 丢弃。**零改动原则（收尾补上）**：无 `$ref` 的请求一个字节不动——回写 Marshal 结果会重排键序、改写数字字面量（1e9→1e+09）污染上游 prompt 缓存前缀，string 形态旧版还按工具数刷日志（agent 一发 20 工具 = 20 行 app.log）；同样口径下空串 parameters 不记「str-unparsed」。**PrepareBody 改写数由三变四**（不变量 1）。回归：`schemacycle_test.go`（自引用/互递归/菱形/根引用/非本地/预算/字符串形态/端到端 14 用例）+ `TestBreakToolSchemaCycles_LeavesRefFreeParamsUntouched`（无 `$ref` 逐字节不动）；生产实测同形态请求消毒后 0 次 4027 |
| R57 | **代理配置改为「全局备用上游通道」：默认不参与任何请求，仅渠道按需取用（当前 cline 与 oczen）**（2026-10-14，用户裁定） | **动机**（旧「每渠道代理」的三处实证缺陷）：① 面板 12 行输入，而热更新接线表只覆盖 9 行（`monkeycode/raccoon/loomy` 改代理不生效、需重启），`trCodeUp`（TraeCode 自己的 client 实例）**两张表都没进** ⇒ 该渠道代理从未生效（R34 同族静默失效）；② `SetTransportProxy` 每次保存都**新建 Transport**，杀光 idle 连接且要手工继承 `ResponseHeaderTimeout`（oczen 120s 的特例注释就是这个补丁）；③ 对用户是 12 个概念，实际只需要一个。 **新语义**：`config.backup_proxy` 单一地址（http/https/socks5）= **备份链路**，由 `internal/backuplink` 原子持有；**不影响本工具的任何请求**，仅由渠道自行判定后按需取用（`backuplink.Client(base)` 克隆渠道自己的 Transport、只改出口 ⇒ 除 IP 外与该渠道出厂行为同构，不引入新超时/协议差异）。当前唯一取用方 cline：`ChatStream` 直连命中 `IsRegionBlocked`（403/451 + 保守文案表 `regionRetryMarkers`，比 `regionBlockedMarkers` 严——判据宽了会把非地区错误也发到第三方出口）⇒ **同一次请求内**用备用链路重放同一 body。 **三条硬约束**：① 未配置 / 重放拨不动 / 重放仍被地区拒 ⇒ **透传直连那份**原始 status+body（R37：不得用第二次尝试的错误掩盖首因；代理故障只进日志）；重放拿到**不同的**上游答复则以它为准；② 重放只换出口 IP、**不是换账号** ⇒ handler 挑号/惩罚/粘性路由/R29 轮换语义零改动，地区限制继续不罚号（R46 的 `ErrPassthrough` 不变）；③ 命中一次后 6h 内该模型**直接走备用**（免每发先白撞一发 403：既省 0.3~2s，也避免在上游留重复失败特征），`backuplink.Generation()` 一变即作废；备用链路自身不可用（拨不动、或上游改到只对备用出口挂地区限制）⇒ 立即作废学习结果并**本轮回落直连**（代理故障不得把本来可用的模型钉死，也不能等 TTL 到期）；反过来主链路（直连）拨不动仍按传输层错误冒泡，交给 handler 计错/换号。 **oczen 不接**：其上游对抗策略已变更、渠道当前不可用，`RegionError` 继续按 R16/不变量 22 原文透传；待恢复后自行取用 backuplink（本包零接线，无需改 main）。 **清理**：删 `config.proxies`、`applyProxies`/`applyProxyToKind`/`SetTransportProxy`/`proxySyncer`、`POST /api/config/proxies`、面板 12 行输入。旧 `proxies` 段在 `normalize` 一次性迁移进 `backup_proxy`（优先 cline 的值，其余渠道地址**丢弃**并在日志点名：除 cline 外的渠道不再支持代理）；旧值不合法则不迁移只记日志（不把启动拉死）。新端点 `POST /api/config/proxy`（同时承载 oczen key），`/api/state` 回 `backup_proxy`/`backup_proxy_active`。 **已知限制（如实披露）**：登录编排（`login_*/NewClient`）、CDP、面板内凭证测试仍不吃备用通道（保持「只服务上游通信」的边界）；若某渠道连登录都被地区拒，本设计无法解决。 **回归**：`internal/backuplink/backuplink_test.go`（判据唯一来源/未配置零影响/generation 递增/按 base 复用连接池）、`internal/cline/backup_proxy_test.go`（假上游 + 假转发代理 8 例，去掉重试分支即失败）、`cmd/wild-work/transport_test.go`（凡进 runtimes 的渠道必须进 wrapDumps 表，并禁止再出现按渠道套代理的接线）、`internal/config/backup_proxy_test.go`、`internal/app/backup_proxy_test.go` |
| R58 | **流被截断必须自证原因：空闲看门狗掐的 ≠ 上游断的；`stream_idle_seconds` 配置项从未存在，注释是空头支票**（2026-10-14，现场 QoderCOM 截断排查） | **现场**：`Qwen3.8-Flash (QoderCOM)` 输出一部分后客户端报 `Error: upstream stream truncated`。**根因一（诊断缺口）**：qoder/qodercn/qodercom/qwenwork 四个 `streamAsOpenAI` 把 `parseNestedSSE` 出参 `truncated` 当成唯一信息，收尾分支写**固定串** `"upstream stream truncated"` 且 `return errors.New("upstream stream truncated")` —— **原始 err 被彻底丢弃**，而该 `truncated` 同时覆盖「传输层读错误」「EOF 停在半帧」两种来源，于是三种完全不同的故障在日志与客户端**完全同形**：① 空闲看门狗主动掐的（`internal/idle` 连续无字节 300s，疑似上游排队/卡死）；② 上游主动断连（connection reset / unexpected EOF）；③ 读到半个帧就 EOF。与 `internal/upstream/sse.go:718` 保留 `err.Error()` 的做法不一致（R36 家族）。**根因二（判据不可达）**：`internal/idle` 看门狗唯一的动作是 `cancel()` 请求 context，在途 `Read` 返回的是 **`context canceled`**（httptest 真实链路实测，非推断），而 `provider.IsIdleTimeout` 只认 `errors.Is(err, ErrIdleTimeout)`（那个变量只有 `IdleReader` 家族会返回）⇒ **看门狗掐断在语义上根本认不出来**，即便保留了 err 也只是印一句 `context canceled`，反而更容易被误读成「客户端断开」。**修法**：① `provider.IsIdleTimeout` 扩展识别 idle 看门狗族（`context.Canceled` / `context.DeadlineExceeded` / `net.ErrClosed`），并在注释里写明「认不出则两种故障同形」；② 新增 `provider.TruncationCause/TruncationError/TruncationCode/TruncationFrame/WriteTruncationFrame` 一族：错误文本保留原因、空闲超时**统一归一成 `upstream stream idle timeout` 措辞**（不直写 `context canceled`，否则日志检索漏一半）、出口帧 `code` 分流为 `upstream_timeout` / `upstream_truncated`、帧体走 `json.Marshal` 而非字符串拼接（原因文本含引号/反斜杠时会造出坏 JSON 帧）；③ 四个渠道的截断分支改调该族，与 `internal/upstream/sse.go` 口径对齐。**⚠️ 刻意不动 qwenwork 的两处差异**：它的截断处理与 qoder 三兄弟**是两套独立实现**（`qwReadErr`/`qwCut` 测试夹具与 qoder 不同，行号 333 vs 279），故四处分别改而非抽公共包——本仓渠道包历来不共享 SSE 实现（R36 同款处置）。**根因三（空头支票注释）**：cline/loomy/raccoon/monkeycode 的 `Client.IdleTimeout` 与 `DefaultIdleTimeout` 注释都写着「由 main 注入 `config.upstream.stream_idle_seconds`」，但 `config.Upstream` **只有 `timeout_seconds` 一个字段**、`main.go` **零处赋值** ⇒ 阈值恒为硬编码（idle 包 300s / 四渠道 90s）。按用户裁定**清理注释而非补配置项**（不新增用户概念）：改成如实写明「目前没有配置项，恒取默认值」。**日志补强（同一现场的可观测性）**：`stream relay end` 原先只打 `err`，现补 `model` / `elapsed=%.1fs` / `ttfb=%dms` —— 只有这两个数字能把原因分开：**耗时 ≈ 空闲阈值且 TTFB 远小于它 ⇒ 看门狗掐的；耗时远小于阈值 ⇒ 上游自己断的**。**已知限制（如实披露）**：`elapsed` 含上游首字节等待时间，若「排队」发生在响应头之前（qoder 三渠道 Transport **未设** `ResponseHeaderTimeout`，该阶段实际无上限）则耗时会被首字节等待抬高，判据需结合 TTFB 一起看；空闲阈值仍不可配（要改需改常量或补配置项）。**回归**：`internal/provider/truncation_test.go`（看门狗族识别 + 四种原因的文本/错误码/`errors.Is` 穿透 + 恶意原因文本仍产出合法 JSON + 帧不含 `[DONE]`）、`internal/qoder/idle_truncation_test.go` 及 qodercn/qodercom/qwenwork 三份同源副本（**走真实 httptest + `idle.Monitor` 链路**，而非假 reader——只有真实链路能证明「看门狗 cancel → 在途 Read 返回 `context canceled` → `IsIdleTimeout` 认出 → `code=upstream_timeout`」这条链成立；假 reader 直接返回 `provider.ErrIdleTimeout` 会绕开最关键的一环）+ 对照用例「上游主动断连必须落 `upstream_truncated`」。**已双向验证**：临时把 `IsIdleTimeout` 的扩展判定改回 `return false`，四渠道用例与 provider 用例全部失败（错误文本退化成 `context canceled`、出口帧 code 变 `upstream_truncated`）。 |
| R59 | **Cline 渠道对外模型 ID 直通上游命名空间：`cline-free/*`（不再拼 `cline/`），旧双层写法保留路由兼容**（2026-10-14，用户裁定） | **背景**：Cline 上游免费池的模型 ID 自带命名空间（`recommended-models` 的 `free[]` 原样透传，见 R46），而 R52 的「kind 永远是第一个路径段」把对外 ID 变成了 `cline/cline-free/*` 双层路径。用户裁定：**本渠道特殊处理，模型 ID 直接透传上游前缀**（面板点击复制的就是客户端要填的串），避免双层路径这种反直觉形态。**契约（唯一判据在 `provider`）**：`provider.PublicModelID(kind, upstreamID)` = 上游 ID 自带已声明命名空间时原样返回，否则拼 `<kind>/`；`provider.SplitModelID(model)` 反向解析为 (渠道, 上游模型名)，接受两种写法：`cline-free/x` → (cline, **整串**) 与 `cline/cline-free/x` → (cline, 去一层后的 `cline-free/x`)。**兼容**：旧写法不失效——老配置的 `model_map`、降级链规则、老客户端都是那种写法，路由/网关/降级链匹配全部经同一解析器。**接线（6 处必须同源，R52 就是漏了一处）**：① `server.buildModelEntry`（`/v1/models`）；② `server.runtimeForModel`（同时它是**降级链** `pickModelFallback` 的解析器）；③ `gateway.Router.Resolve`（三接口兼容层的前缀校验 + `default_channel` 兜底拼接改用 `provider.ModelIDPrefix`，否则裸名会被拼成 `cline/<x>`）；④ `app.buildFeesChannels` 新增 `id` 字段（客户端要填的完整 ID），面板两处复制改为**直接用 `m.id`**、不再在前端拼前缀；⑤ `stats.channelOfModel`（否则 `cline-free/*` 不进平台表——顺带修了一个既有 bug：`channelKind` 白名单漏 `cline`，cline 的 token 一直不进平台行）；⑥ handler 的统计/流水归行改用 `provider.CanonicalModelID`（新旧写法必须落同一行，否则同一模型在用量榜裂成两行）。**回归**：`internal/provider/modelid_test.go`（两个方向的解析 + 归一 + 前缀）、`internal/server/model_id_contract_test.go`（`/v1/models` 只广告上游 ID；两种写法端到端都能 200 且**发给上游的 model 是 `cline-free/*`**）、`internal/gateway/gateway_test.go:TestModelResolutionClineNamespace`（命名空间放行 + default 前缀）、`internal/stats/usage_test.go`（两种写法都归到 cline）、`scripts/ui-static-check.mjs`（两处复制断言 `cline-free/*`） |
| R60 | **「单号渠道 402」的处置是模型级冷却，不是账号级、也不是纯透传**（2026-10-14，PR #113 + #114 合并期裁定） | **背景**：402（权益不足/欠费）此前不在任何冷却/降级触发条件内，存在两个缺口。**缺口一（流内嵌错误无法分类）**：qwenwork 等渠道的 402 以 HTTP 200 + 流内 envelope 形式返回，`parseNestedSSE` 剥壳后以裸 `fmt.Errorf("upstream %d: %s")` 返回 ⇒ handler 三条错误路径的 `errors.As` 全部错过。**修法**：改抛 `*sseError` 并实现 `provider.StreamErrorClassifier`（`Kind()` 返回 `Classify(status, msg)`），错误文案保持 `upstream <status>: <msg>` 不变（客户端可见文本零变化）。**缺口二（单号渠道 402 只透传、不冷却）**：原策略「单账号不罚账号、原文透传」对 402 不适用——账号本身健康，死的只是「账号×该模型」组合，不冷却则每次请求继续撞 402、降级链永不触发。**修法**：三处接入 `CooldownModel(uid, model, HardCooldown)`——① 预流式 `SingleAccount` 分支（`kind == ErrHardCredit` 时冷却该模型、本次仍原文透传）；② 流内 `len(Pool.List()) <= 1` 分支（**去掉 `&& !rt.SingleAccount` 限定**，冷却后透传）；③ `Aggregate`（非流式）出口用 `errors.As` 取分类后同样冷却。**关键边界（必须与不变量 22 合读）**：冷却的是**模型级**条目，账号本身照旧不罚 ⇒ 同号其他模型照常可用，不变量 22 的「不得施加任何账号级惩罚」未被违反；仅当短时间内第二个模型也撞墙时由 `CooldownModel` 既有逻辑升级整号（那才是账号级）。**适用范围比字面更宽**：`SingleAccount` 渠道与「池内只有一个账号」的普通渠道（qwenwork 单号）同受益。回归：`internal/qwenwork/stream_truncation_test.go` 分类断言、`internal/server` 相关用例 |
| R61 | **模型降级链必须在启动时载入，不得只靠面板保存的热更新通道**（2026-10-14，PR #112） | **缺口**：`server.Config.ModelFallbacks` 字段注释声明为「降级链初始值」，但 `cmd/wild-work/main.go` 构造 `server.Config` 时**从未赋值**；降级链唯一生效通道是 `appInst.SetModelFallbackSyncer`（面板保存触发）。⇒ **进程重启后 handler 内降级规则为空，已配置的降级链静默失效**，直到用户在设置面板重新保存一次才恢复（R34/R41 同族的「声明了却没接线」）。**修法**：装配处补 `ModelFallbacks: cfg.ModelFallback`，与热更新通道并存（后续保存仍走 syncer 覆盖）。**通用教训**：凡「配置项 → 内存态」的注入，**启动路径与热更新路径必须各有一条**，只实现后者等于把功能做成「保存过才生效」。 |
| R62 | **OpenCodeZen 同接「备用上游通道」：免费档地域受限模型同请求内自动换出口重放；编排抽到 `backuplink.Replay` 与 cline 共用**（2026-10-14，用户裁定） | **背景**：2.7.2 修复 oczen 免费档后，该渠道的部分免费模型（`muse-spark-*-contributor-free` 实测）直连仍回 403 `{"type":"error","error":{"type":"RegionError"…}}`；用户要求 oczen 也走备用通道（未配全局代理时本次验证用 `http://127.0.0.1:10809`）。**为什么抽公共编排而不是复制一份**：这套规则的语义很细（① 未配/拨不动/备用同样受限 ⇒ 交出**直连那份**原文，首因不被第二次尝试掩盖；② 备用自身不可用 ⇒ 作废学习结果并**本轮**回落直连；③ 只有备用确实给出可用答复才记学习结果；④ 学习结果随 `backuplink.Generation()` 作废），分散两份必然改一处漏一处（R52 的教训）。故新增 `internal/backuplink/route.go`：`Replay`（编排）+ `Routes`（学习路由，6h TTL、按 generation 作废），渠道只提供 `Attempt`（用给定出口发一发）与 `Blocked`（什么算地区限制）两件渠道相关内容。cline 改为调用它（行为不变，原有 8 例回归全绿），oczen 新接入。**oczen 侧判据**（宁窄勿宽：命中会多发一发经第三方出口的请求）：仅 403/451 + 文案含 `regionerror`/`not available in your region|country`/`geo-block` 才算——`FreeTierError`（会话形态）与 429 一律不重放。**与单账号豁免的关系**：备用通道只换出口 IP、**不涉及账号**，故 `SingleAccount` 语义不变；改道发生在渠道内部（`ChatStream`），成功时 handler 完全看不到 403，失败时仍按 R16/不变量 22 原文透传（`Classify` 的 `ErrPassthrough` 注释已同步说明）。**实现细节**：oczen 的 `attempt` 把 `idle.WithCancel` 的 cancel **随本次响应走**（在 attempt 内包 `idle.Monitor`，非 200/失败路径就地 cancel），不依赖「最后一次 attempt 恰是成功那次」的隐式顺序，也避免 `ErrNoBackup` 提前返回路径漏调 cancel 泄漏 context。**实盘验证（独立实例 + 真实上游 + 10809）**：未配代理时 mimo 200 / muse 403 原文透传（日志「未配置备用上游代理，原样透传」）；配代理后 mimo 仍直连 200（不误伤），muse 日志变为「经备用通道拿到上游答复 status=400」（= 流量确已换出口、拿到上游**新**答复）；关代理后回到 403 透传。**已知限制（如实披露）**：`muse-spark-*-contributor-free` 经该出口被上游判 400 `ModelProtocolUnsupported`（同一出口下 mimo/big-pickle/nemotron 均 200）⇒ 本工具的职责边界是「换了出口、把上游权威答复原样交出」，模型在某个出口下能否用由上游决定，本工具不承诺。**回归**：`internal/oczen/backup_proxy_test.go`（7 例：无代理透传 / 配代理改道成功 / 学习路由免白撞 / 双链路受限保留首因且不学习 / 非地域 403 绝不重放 / 备用拨不动保留首因 / 判据单测）、`internal/cline/backup_proxy_test.go`（重构后仍全绿）、`internal/backuplink/route.go` 由两渠道共同覆盖。 |
| R63 | **思考强度映射是「出站注入」，与 R49 的「只透传上游声明」互补**（2026-10-15，PR #118） | **背景**：issue #96 要求数据面透出思考档位，R49 已解决「对外**暴露**上游声明了什么」（未声明时整个 reasoning 对象不出现，绝不编造）。本决议解决另一半：「客户端**没说话**时，是否按用户配置**注入**档位」。**契约**：`config.reasoning_efforts`（`map[string]string`，键=`channel/model` 全路径或 `*` 全局默认，omitempty）→ 启动经 `server.Config.ReasoningEfforts` 载入（**R61 同款教训：只有热更新通道等于「保存过才生效」**），面板保存走 `POST /api/config/reasoning_efforts` → `app.SetReasoningEfforts`（trim 校验键值非空 → 落盘 → `SetReasoningEffortsSyncer` 推给 `handler.SetReasoningEfforts`，`efMu` RWMutex 保护）。**注入三态（哑管道）**：① 顶层**已有** `reasoning_effort` 键（任意值含 `null` = 客户端显式意图）⇒ 一律不碰；② 未带且精确命中 ⇒ 注入；③ 精确未命中回退 `*`；皆无 ⇒ 不注入（上游吃模型默认档）。**不校验档位取值合法性**（支持什么档位由渠道模型自己决定），注入失败仅记日志、**原 body 照发不中断请求**。**接线位置**：`handler.chatCompletions` 在 `runtimeForModel` 之后、**降级链选完备胎之后**、`rewriteModel` 之后调 `applyEffort(body, clientModel)`——注入键用**客户端请求模型名**（用户是按“我请求的那个模型”配的档位），且与 R54 降级链无耦合。**零配置零开销**：空 map 时 `lookupEffort` 首行即返回 false（不发日志/不改 body/不 Marshal），对齐 R54「正常路由零开销」。**已知限制（如实披露）**：`none` 档在 WorkBuddy 上游疑似回落默认档（哑管道照实注入不纠正）；**TraeWork 渠道协议无 reasoning 注入点，注入无害但无效**（面板 hint 已写明，不做事前拦截——渠道模型面变化时不应让配置失效）。**回归**：`internal/server/handler_effort_test.go`（7 例注入行为锁定，含键已存在/null/精确命中/回退 `*`/热更新/失败不中断）、`internal/app/reasoning_efforts_test.go`（配置面）、`scripts/ui-static-check.mjs`（编辑区/datalist 建议集/三态文案/接线共 10 项断言）。 |
| R64 | **面板「测试」类探针必须与真实请求同源取用备用通道，并如实回传实际出口**（2026-10-15，issue #117） | **现场**：用户配了备用代理，面板 OC Zen「测试」仍 403，代理日志零访问记录，上游看到真实地区；日志 `kind=passthrough（单账号渠道不罚账号）`。**逐条核实：用户观察全部属实，但根因不是 backuplink 失效，而是测试路径从未经过编排** —— ① `oczen.TestKey` 直接用 `c.HTTP` 直连（`fetchModelIDs` 同理）⇒ 配了代理也不改道，用户看到 403 无从判断「代理没生效」还是「上游本来就不给这个出口用」；② 那行日志写死「不罚账号」，对 403 `FreeTierError` 这种**与账号无关**的错误照印，把真实原因（免费档形态校验/地域）掩盖成「工具的分类问题」。**修法**：① `backuplink.Replay` 新增可选 `Note(viaBackup bool)` 钩子——`Do()` 有 8 处 return，逐处手工标记「这次答复来自哪个出口」必漏（**R52 教训**），故只在两处「交出备用答复」处置 true，由 `defer` 统一上报；② `TestKey` 改走同一份 `Replay`（含 `Routes` 学习路由），签名加 `viaBackup` 出参 → `App.TestOczenKey` → `/api/config/oczen_test` 的 `via_backup` → 面板 toast 标明「直连 / 经备用通道」，403 且走直连时给可行动提示；③ 单账号分支日志补 `body=`（上游原文），文案改为说明**为何**不冷却（无号可轮换），不再断言「所以这不是账号问题」。**通用教训（已升格为规则）**：凡「面板探针/测试按钮」这类只读入口，**要么走与真实请求同一条链路，要么在 UI 上明写它测的是哪条链路**——否则用户会把探针结果当成真实链路的结论（同 R34/R41 家族的「声明了却没接线」，此处是「接线了但不是同一条」）。**边界**：不改 `fetchModelIDs`（模型目录拉取失败本就有静态兜底，且 `/v1/models` 是只读目录非“服务可用性”声明）。**回归**：`TestOczenTestKeyUsesBackupOnRegionBlock`（地区拒→备用 200 且 `via=true`）、`TestOczenTestKeyWithoutBackupReportsDirect`（未配→直连 403 且 `via=false`）、`scripts/ui-static-check.mjs` 面板出口文案断言；既有 8 例 backuplink 用例全绿（编排语义零变更）。 |
| R65 | **小浣熊「每日积分」是惰性落账，签到 = 调一次 `setting_info`（非独立领取端点）；状态判据用 `point_grant_popups` 而非余额差分**（2026-10-15，用户实测驱动） | **背景**：用户提出「小浣熊调用一次就能获取积分，做成签到」。初版结论「无签到活动」**不完整**——上游确实没有领取端点，但每日 300 积分的发放是**惰性**的：名义记账 `grant_at` 是当天 00:00，实际落账 `created_at` 是账号当天第一次访问**工作台业务接口**的时刻（网页端文案原文：「每日积分每天 00:00 发放，默认当日 24:00 清零，**当天登录后**有效期延长至 7 天」）。**接口矩阵（逐项实测，三账号对照）**：`GET /api/web/office/v3/setting_info` ✅ 触发（各入账 300，`point_grant_popups` 非空）；`POST /api/web/llm/v2/chat/completions`（对话）❌ 只扣「桌面端 LLM 对话消耗 -2」；`GET points/v1/balance|bills`（只读）❌；`GET /api/web/auth/v1/user_info` ❌；`POST /api/web/auth/v1/iam/confirm` ❌（HTTP 200 但无发放）；猜测的领取端点（`daily_grant`/`checkin`/`claim`/`login_reward`）**全 404**。⇒ 只有 **office/v3（工作台业务域）**的访问才算「当天登录」，auth/v1 只是身份校验、llm/v2 与 points/v1 无此副作用。**幂等已实测**：同一账号当天重复调用不再发放（第二次 `popups` 为空数组）⇒ **非空 popups ⇔ 本次调用刚触发新发放**，可直接作签到结果判据。**为什么不用余额差分**：同期可能有对话扣费（实测 -2）抵消，差值不可靠；popups 是本次发放的**直接回执**。**实现**：`internal/raccoon/checkin.go` —— `FetchSettingInfo` + `DailyCheckinReport`（`CheckinReporter` 结构化上报）：popups 含 `points>0` → `CheckinClaimed`（`Amount` 为求和）、空 → `CheckinAlready`（**不是** `no_campaign`：本渠道空即代表已完成，判成可重试会导致整天重试）、401 → 类型化 `ErrSessionDead`（供调度器 refresh 自愈，不变量 19）、其它 → `CheckinError`。**接线**：`main.go` 的 `rcSch` 从 `CheckinMinutes: []int{}` 改为全局 `checkinMinutes` + 补 `SetCheckinObserver`/`SetRefreshObserver`（此前**从未设置**——签到接入后不设观察器则面板与 stats 都读不到结果）；`app.go` 的 `noExplicitCheckin` 移除 Raccoon（开放手动按钮）+ `completeRaccoonLogin`/`importRaccoon` 补首次签到；`web/app.js` 的 `NO_EXPLICIT_CHECKIN` 移除 `"raccoon"`。**刻意不设 `CheckinRetryUntil`**：窗口重试是为 Qoder「活动 10:00 后才创建」设计的，本渠道无此语义（发放恒在，只是惰性）。**与网页端同构**：`setting_info` 是网页端每次打开工作台首页都自动调用的接口，故本实现 ≈ 替用户「打开一次网页」，不是跨端伪装（同 R31 的判据）。**回归**：`internal/raccoon/checkin_test.go`（8 例：claimed/已领/字段缺失/401 类型化/5xx 可重试/多 popups 求和/旧通道包装/走 AuthBase 前缀）。 |
| R66 | **支持自定义 OpenAI 兼容渠道接入（`custom_channels`）：统一用量流水、运行统计与模型路由**（2026-10-15，用户驱动） | **背景**：用户已有第三方 OpenAI 兼容 API（如官方 StepFun、本地 CLIProxy、DeepSeek、SiliconFlow 等），希望在 WildWork 统一聚合调度，通过 `渠道标识/模型名` 统一访问，并在面板集中查看用量流水（ledger）与运行统计（stats，含 TTFB、吞吐、请求日志）。**设计**：新增 `internal/custom` 包，实现 `provider.Upstream` 接口（`New(ChannelConfig)`），流式与非流式分别走 `StreamHTTP` 与 `HTTP`（对齐 R35），流式末帧捕获 usage 记账，SSE 逐帧重写 model 名回填客户端请求名。**单账号属性**：置 `SingleAccount: true` 与 `NoCooldownOnServerError: true`（对齐 R16/R24），错误原文透传不拖垮整条渠道。**配置面**：`config.json` 顶层 `custom_channels[]`（`name` / `base_url` / `api_key` / `models` / `enabled`）；设置弹层支持可视化增删改、连通性测试（`POST /api/config/custom_channel_test` 探测 `/models`）；保存走 `POST /api/config/custom_channels` 并热生效（`SetCustomChannelsSyncer` 同步热更新 `server.Handler`、`app.Runtime`、`gateway.Compat` 路由白名单与 `stats.RegisterChannelKind`，无需重启）。**合并期修复的五条缺陷（均有代码证据、非推断）**：① **`a.runtimes` 并发读写会在保存时打死进程**——`UpdateCustomRuntimes` 是本仓**首个 post-init 写**（此前该表只在 init 赋值一次），而 9 处读点（含 30s ticker 的 `allStatuses` 与 `/api/state`）全程不加锁 ⇒ 面板保存与后台喂入重叠即 Go 的 `fatal error: concurrent map read and map write`（不可 recover）。修法：新增 `runtimesMu` + `runtimesSnapshot()`，全部读点经其取浅拷贝遍历。回归 `TestRuntimeTableConcurrentAccess`（反向验证：去掉锁 5/5 崩溃）。② **compat syncer 捕获启动快照会丢新渠道**——`SetCompatSyncer` 闭包写死 `initialChannels`，于是「先加自定义渠道、之后单独改模型映射并保存」会把白名单重置回启动态，`/v1/responses` 与 `/v1/messages` 报 unknown channel（R61 同族的「只实现一条通道」）。修法：抽出可变共享态 `channelList`（`cmd/wild-work/main.go`），两条 syncer 共读 `snapshot()`。回归 `TestChannelListSharedAcrossSyncers`。③ **自定义渠道名可与内建同名**——`channelNameRegex` 放行 `workbuddy`/`cline`/`oczen` 等，而 server 侧 `getRuntime` 内建优先、app 侧 `UpdateCustomRuntimes` 无条件覆盖 ⇒ 两层指向不同 Runtime（面板操作打到自定义池、对话走内建渠道，静默出错）。修法：`provider.BuiltinKinds()`（含 `ModelNamespaces` 键）+ `ChannelConfig.Validate` 拒绝。回归 `TestChannelConfigValidateRejectsBuiltinNames`。④ **「费率表全线展示自定义渠道」是假承诺**——`FeesInfo` 只遍历静态 `feeChannelOrder`，自定义 kind 永不出现（与 `/v1/models`/ledger/stats 的可见性不一致）。修法：`feeChannelOrderWithDynamic` 把 `modelsByKind` 中未登记的渠道按名排序追加到末尾。回归 `TestFeesOrderIncludesDynamicChannels`。⑤ **`/api/state` 明文回传上游 api_key**——`st.CustomChannels = a.cfg.CustomChannels` 直出配置，而 oczen key 是脱敏回显。修法：`maskCustomChannels` 脱敏（保留 `…` 供识别「未改动」）+ `SetCustomChannels` 按渠道名找回真实密钥（否则脱敏值落盘会把密钥换成废值，下次请求全 401）。回归 `TestCustomChannelKeyMaskedRoundTrip`；且 `/api/config/custom_channel_test` 收到含 `…` 的 key 时按 `name` 找回真实密钥（否则未改动过的渠道点「测试」恒 401，同 R64 的探针不同源），回归 `TestCustomChannelTestEndpointResolvesMaskedKey`。**合并 #120 后的两轮收尾**：⑥ **设置弹层打不开**——#120 旧 head 的 `openSettings` 悬空引用已废弃的 `inCustomModels` 节点（`$()` 返回 null，取 `.value` 抛 `TypeError`），且 index.html 从无该节点；已删引用并补**通用悬空 DOM 引用静态检查**（扫 app.js 全部 `$("id")` 与 index.html 的 id 集比对，动态创建的 `creditTip` 白名单），双向验证过。⑦ **#121 的三条**（R66 延续，merge 9767a78）：字符串形态模型 `SupportsTools` 落 false（字符串分支提前 return 跳过缺省赋值 ⇒ `/v1/models` 静默不带 supports_tools）、显式 `supports_tools:false` 落盘即丢（bool+omitempty 会省掉 false，读回变 true ⇒ 改 `*bool` + `ToolsEnabled()`）、单位串解析错误被吞（`"1OO"` 静默落 0 ⇒ 改为上抛，配置层报出渠道与字段）；另 `style.css` 被整文件改成 LF 造成 1090 行伪 diff（仓库无 .gitattributes、其余 Web 文件均 CRLF），合并时恢复 CRLF。**回归**：`internal/custom/custom_test.go`（配置校验/流式/非流式/模型发现/测试连接/内建同名拒绝）、`internal/server/custom_channel_test.go`（路由/模型列表/热更新）、`internal/app/custom_channel_test.go`（API/设置保存/视图/密钥脱敏回环/费率表含动态渠道/运行时表并发）、`cmd/wild-work/custom_channels_test.go`（装配/共享渠道清单）、`scripts/ui-static-check.mjs`。 |
| R67 | **小浣熊网页登录跟进上游迁站（`office.xiaohuanxiong.com` → `xiaohuanxiong.com`；凭据 cookie `raccoon_refresh_token` → `xhx_token`）**（2026-10-15，上游改版） | **背景**：上游把小浣熊登录从 `office.xiaohuanxiong.com` 迁到 `xiaohuanxiong.com`，改用独立 `/login` 页。**实测**（CDP 探针）：① 旧入口 `office.xiaohuanxiong.com/home?loginModal=true` 已 302 到 `xiaohuanxiong.com/work/home?loginModal=true`，而 `loginModal=true` 在新版前端引发 `/work/home?loginModal=true` ↔ `/login?redirect_url=…loginModal=true` **重定向死循环**，Cookie 永远写不进来（面板表现为「登录成功那一刻起一直跳转」+ 接口 429 限流）；② **凭据 cookie 改名**：登录态为 `xhx_token`（新格式 OIDC JWT：`sid/ver/iss/sub/aud/exp/nbf/iat/jti`，≈30 天），而旧名 `raccoon_refresh_token` **访客态仍下发**（旧格式 `owner_type=visitors`）且**能成功调 refresh** —— 监听旧名会一打开就抓到访客凭据并落盘成假账号（R28 同款坑，现在必现）；③ refresh 端点 `/api/web/auth/v1/refresh` 在新域名下仍存活。**修法**：`WebLoginURL` 指向 `xiaohuanxiong.com/login?product_code=work&redirect_url=…%2Fwork%2Fhome`（**不带 loginModal**）、`CookieName` 改 `xhx_token`；判据从「JWT `owner_type==users`」降级为「`xhx_token` 出现即登录，仅显式 `visitors` 才拒绝」（新版 JWT 无 `owner_type`，原判据会恒判访客 → 永不捕获）；uid 提取加 `sub` 回落（新版无 `name` claim）。**合并期补正**：PR 原版只给「登录落盘」与「web.go」两处补了 `sub` 回落，而 `internal/raccoon/clientsession.go`（客户端会话自愈）与 `internal/app/import_local.go`（从客户端导入）仍只读 `name` ⇒ 两路恒空：前者报「uid 不匹配」使自愈永久失效（不变量 19），后者把账号落成 `raccoon-default.json`。现收敛为**唯一取数点 `raccoon.JWTUID`**（旧版取 `name`、非空才认，否则回落 `sub`），四处共用并删掉 `app` 包内已成为死代码的 `jwtClaim`；静态回归 `TestRaccoonUIDExtractionSingleSourceOfTruth` 锁死「不得再各写一份」。回归测试 `internal/login_raccoon/login_test.go`、`internal/app/raccoon_weblogin_test.go`（网页版登录/R44）、`internal/raccoon/clientsession_test.go`（`TestJWTUID` 新旧两形态 + `TestRescueFromClientSession_AdoptsNewOIDCToken`）。 |
| R68 | **启动时 HTTP 监听失败必须终止进程，不得只记日志继续跑**（2026-10-15） | **现场**：第二次双击启动时没有任何「端口已被占用」提示，托盘图标照常出现、面板也能打开 —— 打开的是**第一个实例**，用户以为多开成功。**根因不在 socket 层**（Windows 下 `net.Listen` 确实报 `WSAEADDRINUSE`），而在 `cmd/wild-work/main.go` 只把失败写进日志就继续往下跑：11 个调度器（签到/保活/积分刷新/费率刷新）与托盘全部照常启动，`Ready` 钩子还无条件弹「wild-work 已启动」；于是第二个进程在后台重复签到，并与第一个实例**共享同一数据目录**（`workDir()` = exe 所在目录，config.json / auths / data），刷新出的 token 互相覆盖。**生产日志实证**：`data/app.log` 两次 `listen 127.0.0.1:7863 failed: bind: Only one usage of each socket address`（2026-09-18 23:32:49、2026-10-11 19:42:15），第二次之后 19:42:16 起照常打出一串 `credit auto-refresh` —— 失败的进程确实活着。**修法**：改用启动层已有的 `fatal()`（写日志 + `platform.InfoBox` + `os.Exit(1)`），与「配置加载失败」「对外暴露但缺管理密码」两条 `fatal` 同一口径；位置在托盘创建与调度器启动之前，故不会留下幽灵托盘图标。**不区分 `--autostart`**：自启时端口被占同样要让用户知道，静默退出只会把「静默假活」换成「静默真死」，问题一样不可见。**A/B 实测**（`--no-tray` + 各自独立 `WILDWORK_HOME` + 独立端口 17899，分别编 `upstream/master` 与分支两份 exe，各拉起两次）：修复前第二个进程**存活**、stdout 打出 `wild-work headless mode started`、日志在 bind 失败 1 秒后照常出现 `pricing fetched platform=oczen count=11`（后台任务确实在跑）；修复后 stdout 无该行、日志只剩新的失败消息、**无任何后台任务日志** —— 进程停在 `InfoBox` 上，点掉即 `os.Exit(1)`（与「配置加载失败」「缺管理密码」两条既有 `fatal` 行为一致）。**回归**：`cmd/wild-work/listen_fail_test.go` 静态锁定失败分支必须调用 `fatal()`（反向验证：改回 `log.Printf` 即变红）。 |
| R69 | **入库 prepbody 工具**（`cmd/prepbody`） | 入站 dump → 出站请求体转换：把客户端原始请求按渠道出站改写规则离线重放，用于定位「上游按出站 body 拒答」类故障；不参与运行时链路 |
| R70 | **档位注入走上游的 efforts 映射，能力表只做校验/展示** | 注入机制 = `config.ReasoningEfforts`（模型→档，`*` 通配，`handler.applyEffort`）；本仓 `internal/reasoning` 保留为能力表（`Clamp`/`Listing`）供渠道校验与面板展示，请求归一化（`reasoning_effort` 键存在即不碰）与上游一致 |
| R71 | **DeepSeek 思考开关与多轮回填**（`internal/upstream/thinking.go`） | 客户端表达思考意图时补 `thinking.type` + 默认档；每条 assistant 消息回填 `reasoning_content`（字符串）否则上游按「不思考」应答 |

## 2. 架构选型（依据）

| 主题 | 选型 | 理由 |
|------|------|------|
| GUI 壳 | **无**（删除 wails） | WebView2 内存开销大 + Windows 绑定；托盘 + 浏览器足够 |
| 托盘 | energye/systray v1.0.3（现有） | 已跨平台；菜单固定方案规避其不可删菜单项限制 |
| 管理后端 | 现有 http.Server 扩展 /api/* | 单端口、复用鉴权中间件（cookie 会话守卫）、零新服务 |
| Web UI | 纯静态 embed + fetch | 无 Node 构建链，单 exe 双击即用 |
| 登录 | 复用 internal/login + login_trae | 纯 HTTP + 本地回调端口，跨平台 |
| 平台能力 | internal/platform + build tag（windows/darwin/other） | 浏览器无痕/开机自启/消息框/日志/工作区，接口同名 |
| 构建 | WSL 交叉编译 win；CI macos-latest 编 darwin | 见 R11 |

## 3. 托盘菜单设计（当前形态）

```
wild-work
──────────
打开主界面          → 系统浏览器打开 http://<listen>/
查看日志            → 系统默认编辑器打开 data/app.log
──────────
退出                → 退出 daemon（确认框）
```

- 单击/双击/右击：右击弹菜单；**单击与双击 = 打开主界面**
- 各菜单项使用不同颜色纯 Go 生成图标（蓝色=打开、灰色=日志、红色=退出）
- 刷新积分功能已移至 Web UI 面板操作

## 4. Web UI 页面规划（纯静态，一个 index.html + app.js + style.css）

| 页面/区块 | 内容 |
|-----------|------|
| 顶部栏 | 品牌名/版本号、API 地址（点击弹窗配置）、API-Key（点击弹窗修改）、帮助/关于 |
| 登录层 | 启用管理密码时先登录（HttpOnly cookie 会话）；设置弹层内置「退出登录」 |
| 账号管理 | 双列卡片网格，账号名/UID/积分/签到状态，图标按钮操作（签到/刷新/停用/删除）；顶部积分汇总条 chip 可点击筛选（PR #68） |
| 自动签到 | 签到时间（HH:MM 多组）+ 开机自启开关（左右布局） |
| 用量与流水 | token 与积分流水（ledger 口径，R17）；token / 积分两个子 tab；token 表的模型榜带**缓存利用率**列（不变量 24）与**平均速度**列（stats 口径同源，R43）；区间选择（今日/7日）localStorage 记忆 |
| 费率 | **第四个主 tab**：各渠道模型定价表（按渠道分组），渠道标签带余额角标、模型名点击复制（PR #69） |
| 运行统计 | 实时统计（stats 口径，R43）：今日情况 / 平台概览 / 使用中接棒 / 最近临期 / 模型消耗 / 请求日志 / 异常账号 / 降级记录（R54，空态常显）/ 运行日志，30s 轮询 |

管理 API（REST，均挂 `/api/*`；除 `/api/auth/*` 外均需有效面板会话——密码为空时不校验）：

```
POST /api/auth/login               # {password} → {session}；下发 HttpOnly cookie
POST /api/auth/logout              # 注销当前会话
GET  /api/auth/state               # 会话探针（不返回 401）：全量状态 + auth_enabled/auth_session/auth_required
GET  /api/state                    # 全量状态（账号/积分/签到/配置）
POST /api/login/start              # {channel} → {auth_url}
POST /api/login/glm_auto           # 智谱清言自动登录：拉起独立 profile 浏览器（见 R27）
GET  /api/login/glm_auto_status    # → {status: idle|pending|success|failed|cancelled, uid?, nickname?, error?}
POST /api/login/glm_auto_cancel    # 取消自动登录并关闭浏览器
POST /api/login/glm_token          # {refresh_token} → {uid} 智谱清言手工兜底（见 R22）
POST /api/login/cline_key           # {api_key} → {uid,warning} Cline 粘贴官方 sk_ Key（见 R46；只调 /api-keys 验证，零 token 消耗；warning=同账号告警）
POST /api/login/cancel
POST /api/account/checkin          # {uid}
POST /api/account/checkin_all
POST /api/account/refresh          # {uid}
POST /api/account/refresh_all
POST /api/account/remove           # {uid}
POST /api/account/disable          # {uid,disabled} 停用/启用
POST /api/account/resource_detail  # {uid} → 积分明细
POST /api/account/nickname          # {uid,nickname} 修改显示名
POST /api/config/checkin_times     # {times:["09:00","21:30"]}
POST /api/config/listen            # {host,port,admin_password?}（非环回时必须带密码）
POST /api/config/admin_password    # {password}（空 = 关闭面板鉴权，仅环回监听允许）
POST /api/config/api_key           # {key}
POST /api/config/autostart         # {on:bool}
POST /api/config/compat            # 模型路由（默认渠道 / 封顶 tokens / 映射表）
POST /api/config/model_fallback    # {rules:[{from,to:[]…}]} 模型降级链（R54，保存即热生效）
POST /api/config/reasoning_efforts # {efforts:{"channel/model":"high","*":"medium"}} 思考强度注入映射（R63，保存即热生效）
POST /api/config/custom_channels   # {channels:[{name,base_url,api_key,models,enabled}]} 自定义 OpenAI 兼容渠道（R66，保存即热生效；api_key 提交脱敏值 = 不修改）
POST /api/config/custom_channel_test # {name,base_url,api_key} 自定义渠道连通性测试（探上游 GET /models）
POST /api/config/expiring_days     # {days} 临期阈值
POST /api/config/proxy             # {proxy, oczen_api_key} 全局备用上游通道（R57：默认不参与任何请求，仅命中地区拒绝时自动重放）
POST /api/config/oczen_test        # {api_key} OpenCodeZen 凭证连通性测试
GET  /api/fees                     # 渠道费率（本地缓存 + 按需刷新）
POST /api/fees/refresh             # 异步刷新费率
GET  /api/usage                    # 用量/积分流水聚合（R17，ledger 口径；token 段含 cr/cr_cov/cr_reqs，不变量 24）
GET  /api/stats                    # 运行统计快照（R43，stats 口径）
GET  /api/stats/logs?limit=15      # 运行统计的最近请求日志行
GET  /api/logs                     # 最近 300 行日志
POST /api/quit                     # 退出程序
```

## 5. 渠道（已实现 WorkBuddyCN + WorkBuddyAI 国际版 + TraeWork + QoderCN + QoderCOM 国际版 + 千问办公 + MonkeyCode + 小浣熊 + Loomy + 智谱清言 + OpenCodeZen 匿名 + Cline；旧 Qoder 已下线）

1. 新建 `internal/<channel>/` 包，实现 `provider.Upstream` 接口
2. `internal/auth` 增加对应 `Load<Channel>Dir()`（文件名前缀 `<channel>-*.json`；
   **glob 边界**：`qoder*.json` 会吞掉 `qodercn-`/`qodercom-` 前缀，LoadQoderDir 必须显式排除）
3. 装配处注册 `server.Runtime{Kind, Pool, Upstream, StaticModels}` + `app.Runtime{..., Scheduler}`
4. 前端渠道选择器加一项；`internal/login_<channel>` 实现登录编排（如需）
   > **无账号渠道（oczen）跳过第 2、4 步**：不建 auth 文件与加载器，虚拟账号由 `main` 装配时注入 pool，
   > 且 `app.reloadAccounts` 不得纳入（否则 `SyncToDir` 会把它剔除）。

> provider.Kind 即模型名前缀；server 按 `channel/<model>` 前缀路由，无需改接口。
> **QoderCN（`qodercn/*`）**：qoder2api 参数形态（cosyVersion 1.0.10、18 头含 cosy-scene 族、
> session_type=qoder、identity userType 实测回填）；签到仅 campaigns（legacy daily-check-in 已 DISABLED，
> 其 claim 恒返回 409 会造成假成功，故不再使用）；每日 10:00 开放 + 10:00–12:00 窗口内重试（见不变量 23）；
> 动态模型表（无静态兑底，上次成功缓存）。详见 `docs/qoderCN渠道接入备忘.md`。
> **QoderCOM（`qodercom/*`）**：国际版（qoder.com/openapi.qoder.sh/api1+api2.qoder.sh 三域分离）；
> 凭据与 CN 区完全隔离（双向 401）；签到仅 campaigns（无 daily-check-in，实测 404）；
> 每日 10:00 开放 + 10:00–12:00 窗口内重试（同 CN，见不变量 23）。详见 `docs/qoderCOM渠道抓包分析与接入计划.md`。
> **旧 Qoder（`qoder/*`，QoderWork）已从界面下线**：代码与路由保留，存量账号仍可用；不新增功能，后续可移除。
> WorkBuddyAI 国际版：`DailyCheckin` 实现为「免费模型对话保活 + 签到探测」（对用户透明，无前端界面）；
> token 有效期 365 天，故 KeepaliveHours 设为 nil。详见 `docs/workbuddy国际版渠道接入备忘.md`。
> **MonkeyCode（`monkeycode/*`，平台托管模型）**：凭据由面板「从本机客户端导入」从官方客户端
> （ohmyagent）的 settings.json 取得，一个账号 = `oma_` api_key + `omas_` signing_secret；
> 按模型声明的 type 分流到 `{base}/messages`（Anthropic 形状）或 `{base}/responses`（Responses 形状），
> 两路都是无状态 SSE；上游无目录/额度/刷新接口 → 模型表静态、额度恒 0、`RefreshToken` 空实现。
> 协议对照参考：`codkeep/MonkeyCodeReverseEngineer`（Apache-2.0）。
> **小浣熊（`raccoon/*`）**：两条添加路径 —— 面板登录按钮走**网页版自动登录**（拉起独立 profile 的
> Edge/Chrome，登录后经 CDP 捕获 Cookie `xhx_token`，见 internal/login_raccoon/web.go；
> 跨平台，不再依赖 Windows 注册表，详见 R44），弹窗次按钮「从客户端导入」读本机
> `.box-agent/config/auth.json`。凭据 access_token ≈1h + refresh_token ≈30d，保活每 4 小时一次
> 以减少「请求先 401 再刷新」的往返。**签到走 `setting_info`**（每日积分的惰性落账触发点，
> 见 R65 与 internal/raccoon/checkin.go）。**注意互踢**：上游 refresh 轮换 refresh_token，
> 同账号凭据共用（本工具 + 浏览器/客户端）会互踢，需各自单独登录（独立 `sid`）才能并存。
> 上游默认档即最深思考，故不接档位面。
> 接口结构整理：`xxhhlk/raccoon2api`（MIT）。
> **Loomy（`loomy/*`）**：导入型渠道 —— 凭据由面板「从本机客户端导入」读
> `C:\Users\Public\Loomy\<hash>\userData\auth-session.json`。上游**无 refresh 端点**，
> session 约 14 天，到期需重新导入；故 `RefreshToken` 留空、定时任务全关（保活会被
> 按「无 refresh token」跳过，但每天仍空跑一次并记失败）。鉴权失败是 **HTTP 200 +
> 业务码 `100002`**（不是 401），业务码判定必须排在状态码判定之前。
> 接口结构整理：`xxhhlk/loomy2api`（MIT）。
> **Cline（`cline/*`）**：**粘贴型渠道** —— 凭据是官方文档化的 `sk_` API Key
> （入口：`app.cline.bot` → 注册账号 → `Account / API Keys` → `Create API Key`，
> 形如 `sk_` + 64 hex，**只在创建时完整显示一次**；2026-10-12 用户订正，
> 旧记「Settings → API Keys」有误）。
> 一个 Cline 账号一把 Key；**Key 不过期、不轮换** ⇒ 无 `RefreshToken`、无保活、无签到，
> 也没有 R26/R44 那种「同凭据多处使用互踢」的问题（见 R46）。
> **模型面不做任何本地假设**（2026-10-12 简化）：绑死上游 `recommended-models` 的 `free[]`
> 原样透传——ID 保留 `cline-free/*` 别名不改写（用户后台观察：免费入口是别名条目，
> 厂商真实名会扣费；注意此为观察级证据，计价逐请求抽签）、无静态表、无本地配置、
> 不拉 `/api/v1/models`（458 条 OpenRouter 镜像）。本地校验集 = 暴露集 = `free[]`，
> 冷启动放行；上游抖动用已知集兑底。
> **两个必须遵守的实现约束**：① 出站必带**非空 `X-CLIENT-TYPE`**，否则 `cline-free/*` 全量 403
> 且**不报错**（值随意，缺失/空值才拒）；② `Classify` 必须从被压平成 500 的 body 里取**内层**状态码，
> 并把 `limit_rpd`/`Daily limit reached`/`temporarily rate-limited` 归成**模型级**处置
> （`ErrPassthrough` + 模型冷却），**绝不软化账号**（见 R47）。
> **`max_tokens`**：客户端未给则**不传**（不传最稳），<64 抬到 64、>200000 压到 200000
> ——实测 8 被上游拒、16 会造成 `empty response content`（推理吃光预算，流式同样中招）。
> 上游**无余额/额度端点** ⇒ `UserResource` 恒 0（**不得返回 error**），面板显示「不适用」，
> 实际花费来自逐请求 `usage.cost`。**不承诺价格**：`free` 前缀 / `is_byok` / `provider` 实测都不能
> 当免费判据（同前缀有扣费、非 free 后缀反倒 cost=0，且同 ID 相邻两笔会漂移）。
> 添加时只调 `GET /api/v1/api-keys` 验证（**零 token 消耗**），uid 取其 `apk-…` id；
> 官方配额字段名是 `…UsageCostUSDPerUser`（按**账号**计）⇒ 同账号多把 Key 共享一份额度，须告警。
> 依据与实测全表见 `docs/cline渠道接入计划.md` 与 `docs/cline2api线索项目评估.md`。

> **OpenCodeZen（`oczen/*`，匿名免费）**：凭证固定字面量 `public`，无账号/无签到/无积分；
> 免费档有三道闸门（`ses_<12hex><14Base62>` 会话头**且其 12 位 hex 时间戳段须为官方
> `Identifier.descending()` 编码**，见 R53b；`stream:true` 且 tools 含 `bash`/`read` +
> OpenCode CLI 伪装头），缺一即 403 FreeTierError；面板固定一项「[OpenCodeZen] 匿名」、积分显示「不适用」，
> 不可增删停用。详见 `docs/opencodezen渠道接入备忘.md`。
> **智谱清言（`glm/*`）**：网页版私有接口（**非**开放平台 open.bigmodel.cn），凭据是浏览器 Cookie 里的
> `chatglm_refresh_token`；所有私有接口需签名 `X-Sign = md5(ts-nonce-secret)`；模型 = `assistant_id`
> （24 位 hex 智能体 ID）+ `chat_mode`（`zero` 推理 / `deep_research` 沉思 / `ppt` / `video`）。
> **模型清单是「智能体清单」不是「模型版本清单」**——清言客户端无法选模型版本，
> 服务端按账号分配（实测主对话恒为 `moe_53f`）；GLM-5.3 的具体版本控制只能走官方开放平台。
> 静态表**只收录实测可用的 4 个智能体**（ChatGLM / AI搜索 / 清言PPT / 视频助手）；
> AI画图（需 cogview 参数）、AI阅读（需上传文件）、学习搭子（上游 10025 报错）**不收录**——
> 想用未收录的智能体直接填其 24 位 hex ID。
> **积分机制**：`member_info.score_rule` = 「免费用户，登录赠送200积分/天」。
> **新账号激活**：加进来后 `left_score=0`，**必须在智谱清言 App 登录一次**才发放（+3000 随后 +500）——
> 对照实验证实（R23）。**代码层面无解**（Web 端无激活接口，R32 实测），
> 故额度为 0 的账号会永远排最后、实际轮不到。
> **每日积分**：走 `member-api/member/daily_login_score`（**已接入保活流程**，R31）——
> 该接口网页版打开时自己就会调，幂等安全（`status=10001 今日已领取` 是正常语义）。
> 「签到」的实质是**保活对话**。**操作建议**：面板加完账号 → 手机 App 登录同账号一次 → 之后每天自动领。
> SSE 为**增量 delta**（`finish` 帧给全文，见 R25）。登录走 CDP 自动捕获 Cookie（R27/R28），
> 支持多账号（独立 profile 逐个添加）。详见 `docs/智谱清言渠道接入备忘.md`。
> **自定义渠道（`custom_channels`）**：不写代码、只在面板配置的 OpenAI 兼容渠道（R66）——
> 用户填 `name`/`base_url`/`api_key` 与模型清单（含上下文/思考档位/多模态能力，**配什么返回什么、
> 绝不向上去拉或推测**），即可以 `name/<model>` 访问并统一纳入流水与运行统计。
> **名称不得与内建渠道（或上游命名空间如 `cline-free`）同名**：两者同名会让 server/app 两层
> 指向不同 Runtime（面板操作打到自定义池、对话请求走内建渠道），故 `Validate` 直接拒绝。
> 单账号 + `NoCooldownOnServerError`（R16/R24 哲学）；`api_key` 在 `/api/state` 脱敏回显，
> 面板未改动时提交脱敏值 = 不修改（后端按渠道名找回真实密钥）。费率表/流水/运行统计均展示。

### 5.1 思考档位有效性对照表（2026-09-25；2026-10-10 补 traecode 换模型复测）

「面板声明档位」= `reasoning.ListingForKind` 是否对该渠道放行（决定 `/v1/models` 与费率表是否显示档位选择器）；
「实测是否生效」= 有**权威指标**（`reasoning_tokens`，而非思考字符数/块数）的实测证据。

| 渠道 | 面板声明 | 投影方式 | 实测是否生效 | 证据 |
|---|---|---|---|---|
| workbuddy (CN) | ✅ | `reasoning_effort` + DeepSeek `thinking.type` | ⚠️ **未获证据** | 静态表已复核（`b3e0b9d`），但档位是否真改变思考量没测出 |
| workbuddyai | ✅ | 同上（`RealmGlobal`） | ⚠️ **未获证据** | 同上；上游对非法档位**静默接受**不报错 |
| qoder（旧，已下线） | ✅ | `is_reasoning` + `parameters.reasoning_effort` + `enable_thinking` 三件套 | ✅ **生效** | low 2206 字 < medium 2502 字 < xhigh 180s 超时截断 |
| qodercn | ✅ | 同上 | ✅ **生效** | 2026-09-22 真实账号线上实测 |
| qodercom | ✅ | 同上 | ✅ **生效** | 协议同源 CN（未独立实测） |
| loomy | ✅ | `reasoning_effort` + 三件套 + `Clamp` | ✅ **生效** | `deepseek-v4-flash-0731` 三档单调递增（173/297/459，≈2.6×） |
| raccoon | ❌ **不声明** | **主动剥离** `reasoning_effort` | ✅ **剥离即最深**（反向生效） | 无字段 rtok 2300+ vs `high` 142（~90% 降幅） |
| qwenwork | ❌ | **不投影** | N/A（刻意不做，非缺口） | 官方客户端本身无思考控制 UI，抓包确认请求体不带该字段 |
| traework | ❌ | 不投影 | ❌ **不生效**（结论不变，理由已更正）：字段**被 schema 接受但不改变思考量**；旧口径「未被反序列化」只对辅助端点 `llm_utils_chat` 成立 | 本地 docs/TraeWork-api.md §4.1–§4.5 |
| traecode | ❌ | 同上 | ❌ 同上；客户端主链路（native RPC）实测亦不改变工作量（生成空档 p=0.700）；**2026-10-10 换 4 个模型复测**（`deepseek-v4.1-flash` / `glm-5.3` / `Doubao-Seed-2.1-Pro` / `glm-5.2`，覆盖不同 ladder，n=6 交替）**仍无分离**（p 0.300~1.000） | 同上；§4.7 |
| monkeycode | ❌ **不声明** | anthropic 面 `thinking.type`（两态）／responses 面 `reasoning.effort` | ⚠️ **按模型分型**：客户端按内置能力目录决定——`binary` 型 anthropic 面只开/关；`effort` 型（anthropic 面仅 kimi-k2.6）走 `adaptive` + `output_config.effort`（wild-work 目前会丢该档位）；responses 面 `none`/`low`/`high` 有效 | 评估文档 §3.13 |
| oczen | ❌ | 不投影 | N/A | — |

**读表要点**：
- **未获证据 ≠ 不生效**，也 **≠ 生效** —— WorkBuddy 双面属此列，别在文档里写成「已验证」。
- **上游接受 ≠ 生效**：TraeWork 传错类型都回 200，说明字段根本没被反序列化。
- **度量纪律**：验证档位必须用 `reasoning_tokens`；思考**字符数/块数都不可用**
  （块数取决于上游分块粒度，字符数同档内方差可达 20 倍）。单次采样也会得出错误结论
  （易题下 low≈medium≈high 全撞下限），必须用吃推理的难题 + 重复采样。
- **不声明档位 ≠ 缺功能**：raccoon（不下发才最深）、qwenwork（官方无此 UI）都是**刻意**的。

### 5.2 TraeCode / TraeWork 延迟无渠道差异（2026-10-11 实测）

背景：用户两次主观反馈「traecode 渠道响应慢」。用本机运行实例（`127.0.0.1:7863`）对**两渠道动态表都下发**的同一模型 `mimo-v2.6-pro` 做交替串行 A/B（`traework → traecode` 轮转，共 5 轮、每渠道 33 次），测 TTFT（首字延迟）与总耗时。

**结论：无渠道级差异。**
- TTFT 两侧一致（~1s），逐轮互有胜负。
- 总耗时的「重尾」**两侧都出现**：>10s 的请求 traework **4/33**、traecode **5/33**（不可区分）。
- **关键反证**：第 5 轮（n=10）尖峰**全落在 traework**（23.2 / 17.5 / 10.2s），traecode 一次都没有 —— 前几轮「traecode 总出尖峰」纯属**小样本 + 时间聚集**造成的错觉。
- **机制**：上游**偶发**进入「慢速生成」窗口（累计约 15% 请求），探针 `maxgap` 显示是**均匀拖慢**（如 23s 那次相邻块间隔均值 ~0.4s）而非单点卡死；该窗口**不分渠道**，谁赶上谁慢。代码层面两渠道同池、同账号、同端点、同 HTTP 客户端，仅 `function` 字段不同（`solo_work_lite` vs `solo_agent`），无任何能产生系统性差异的机制。

**判据**：
- 觉得某渠道慢时**原样重发一次**——第二次正常即为上游瞬时抽风，换渠道也一样。
- 判「渠道差异」必须**交替串行 + 样本够大**：本例每边 33 次才敢下结论。单次/几次对比会得出**完全相反**的结论（第 1 轮像 traecode 慢、第 5 轮像 traework 慢）。
- 探针 `.gotmp/trae-ab/probe.py`（`python probe.py <模型> <轮数>`），带 `maxgap` 可区分「卡顿」与「均匀慢」。

## 6. 关键不变量（改动前必读）

0. **每次代码变更后必须本地重新构建 `dist/wild-work.exe`**（见 §8）。
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
      通用份也有专用份。早期仅用 ep 判定的实测证据见 `docs/upstream-reverse-engineering.md` §2.3。
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
    （`app.refreshIfSessionDead`，scheduler 的 checkin 路径同理）。
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
    - **2026-10-14 澄清（PR #113）**：“不施加账号级惩罚”不等于“什么都不冷却”——`ErrHardCredit`（402 权益不足）的处置是**模型级**冷却（`CooldownModel`，账号×该模型），账号本身仍不动。适用面比字面更宽：`SingleAccount` 渠道与「池内只有一个账号」的普通渠道（如 qwenwork 单号）都在其列。这是对 R16「4xx 一律 `ErrPassthrough`」的局部例外，理由见 R60。
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

## 7. 平台能力差异表（internal/platform）

| 能力 | Windows | macOS | Linux |
|------|---------|-------|-------|
| 打开浏览器 | rundll32 url.dll | `open <url>` | xdg-open |
| 系统消息框 | MessageBoxW | osascript display dialog | stderr |
| 开机自启 | 注册表 Run | LaunchAgent plist | 未实现 |
| 打开日志文件 | notepad | open -a TextEdit | xdg-open |
| 确认框 | MessageBoxW YESNO | osascript buttons | 默认否 |
| 无头模式 | --no-tray | --no-tray | --no-tray（推荐） |

## 8. 构建

> ⚠️ **本地构建是日常约束**：任何代码变更后都要重新构建 `dist/wild-work.exe`
> （见 §6 第 0 条）。CI 不生成该文件，且 `dist/` 不入版本控制。
> 标准流程：`go build -tags devdump ./... && go vet -tags devdump ./... && go test -tags devdump ./...` 全绿后再构建。
> 本机设过 `WILDWORK_DUMP_DIR`（常驻实例取证用）时，跑测试要显式清空它——测试进程会继承该变量，把 fixture 请求也 dump 进同一目录（见 §8.5）：`WILDWORK_DUMP_DIR= go test -tags devdump ./...`。
> 默认构建统一带 `-tags devdump`（含 CI / 发版；取证能力常态编入，运行期由 `WILDWORK_DUMP_DIR` 控制，见 §8.5）。

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
python -c "b=open('dist/wild-work.exe','rb').read(); print('new:',b.count(b'2.5.3'),'old:',b.count(b'2.5.2'))"
# 期望：new >= 1 且 old == 0。若旧版本号仍在，说明构建未生效。
```

### 发版（tag 触发）

push `v*` tag → GitHub Actions 构建五平台产物并创建正式 release。
**release note 用仓库根目录的 `RELEASES.md`**（统一维护，按版本从旧到新排列——新版本说明
**append 到文件末尾**；旧的分版本 `RELEASE-vX.Y.Z.md` 已并入该文件并删除）。
release note 从 `RELEASES.md` 截取对应版本段落（`# wild-work vX.Y.Z` 起）作为手写摘要，
而不是 `--generate-notes`（那只给 commit 链接列表）；文件/段落缺失时回退自动生成，不阻塞发版。

```bash
# 发版前确认：版本常量已 bump（internal/app/app.go const Version）、
# RELEASES.md 末尾已追加对应版本段落（# wild-work vX.Y.Z）且与 tag 名一致、
# dist/wild-work.exe 已本地重建验证
git tag vX.Y.Z && git push origin vX.Y.Z
```

## 8.5 开发期上游取证（devdump）

`internal/devdump` 把各渠道出站对话请求的**上游原始响应**落盘，供离线分析
「上游到底回了哪些 usage 字段」（如缓存利用率所需的 `cached_tokens` /
`cache_read_input_tokens` / `prompt_cache_hit_tokens`）。

**两层开关（缺一不可）**：

1. **编译期**：整包被构建标签切成两半——默认构建（含 CI / 发版，统一带
   `-tags devdump`）编入真实实现（`devdump_on.go`），不带 tag 构建时替换为
   **常量桩**（`devdump_off.go`：`Wrap` 原样返回、`Enabled()` 恒 false）。
   不带 tag 的产物中不存在任何 dump 代码（实测二进制里查不到
   `WILDWORK_DUMP_DIR` 字样）。
2. **运行期**：仅当环境变量 `WILDWORK_DUMP_DIR` 非空才真正落盘。

> ⚠️ 别让测试继承：`go test` 跑 `internal/server` 的 handler 测试时会落盘自己的合成请求
> （并发 fixture 单轮 200+ 条：`qoder/model-0..31`、`oczen/big-pickle` 等），`internal/server/dump.go`
> 的入站/失败对 dump 也吃同一个变量——与真实流量混在同一目录、只能靠时间戳分辨。
> 跑测试一律显式清空：`WILDWORK_DUMP_DIR= go test -tags devdump ./...`。

**接线点只有一个**：`cmd/wild-work/transport.go` 的 `wrapDumps`（R57 后唯一统一改 Transport 的地方，
不再涉及代理；启动时一次性包住全部 14 个渠道 client，面板热更新无需再接），
故各渠道包**零改动**；漏项由 `cmd/wild-work/transport_test.go` 静态锁死。
文件落 `$WILDWORK_DUMP_DIR/<channel>/*.http`，
头部带 `# usage=` 速览行（SSE 取**末帧**），非常量部分截断保留头尾。

```bash
export WILDWORK_DUMP_DIR="$PWD/temp/dump"
go build -o dist/wild-work-devdump.exe ./cmd/wild-work   # 默认构建已带 devdump（见 §6）
./dist/wild-work-devdump.exe --no-tray              # 需先停掉常驻实例或改端口
bash scripts/probe-dump.sh run                      # 逐渠道发一轮真实对话
bash scripts/probe-dump.sh summarize                # 汇总各渠道 usage 字段
```

> ⚠️ 快照含 access token 等敏感头，仅限本机分析、不得外传。

## 9. 文档索引

入库文档（`docs/` 白名单制下反选入版本控制）：

- [README.md](README.md) — 用户文档
- [DEVELOPMENT.md](DEVELOPMENT.md) — 开发者文档（面向 AI Agent）
- [AGENTS.md](AGENTS.md) — 本文件：决议项（R1–R53b，另含 R54–R71）、架构选型、不变量
- [docs/三接口兼容改造备忘.md](docs/三接口兼容改造备忘.md) — 三接口（Chat/Responses/Anthropic）兼容层架构决策、实施记录、验证清单、已知限制
- [docs/用量积分流水记账备忘.md](docs/用量积分流水记账备忘.md) — 双流水统计（token/积分）架构、差分算法、实测验证、已知限制（R17）

以下备忘被 R16 / 不变量 22 / 不变量 23 等决议引用，但**尚未入库**（`.gitignore` 白名单未反选，仅本地可见）：
`docs/opencodezen渠道接入备忘.md`、`docs/qwenwork渠道接入备忘.md`、`docs/qoderCN渠道接入备忘.md`、
`docs/qoderCOM渠道抓包分析与接入计划.md`、`docs/智谱清言渠道接入备忘.md`（R22–R38 引用，随 PR #46 新增，
作者未提交入库）。（部分可能已丢失，仅存在于历史会话中）。
如需转为本仓可查，在 `.gitignore` 补 `!docs/<文件名>` 并在上方列表添链接。
**例外**：glm 渠道的协议依据（R22–R38 引用）**不在本地**——作者已将其开源为独立仓库
[glm2api](https://github.com/ttales430/glm2api)（签名算法/认证流程/SSE 语义/积分机制/智能体清单），
引用一律指向该仓库，不要找本地文件。

> **docs/ 采用白名单制**：`.gitignore` 中 `docs/*` 默认忽略全部文档，仅 `!docs/<文件名>` 显式反选的才入库。
> 逆向分析类文档一律**只保留本地、不入库**。新增需要入库的文档时，追加一行 `!docs/<文件名>`。
> 未入库的本地文档（`ref/`、`docs/` 其余文件、`HANDOFF.md`、`GO多平台发布备忘.md` 等）仅供本地参考，
> 不要在本文件中作为可点击链接引用。
