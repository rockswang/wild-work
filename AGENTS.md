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
| R10 | 新增渠道扩展方式：实现 `provider.Upstream` 接口 + auth 加载器 + 注册 Runtime | 模型前缀 `channel/<model>` 路由；已实现 WorkBuddyCN(国内) + WorkBuddyAI(国际) + TraeWork + TraeCode(与 TraeWork 共账号，function=solo_agent) + QoderCN + QoderCOM(国际) + 千问办公(qwenwork) + 商汤小浣熊(raccoon) + Loomy(loomy) + MonkeyCode(monkeycode，平台托管模型) + 智谱清言(glm) + OpenCodeZen(oczen 匿名) 十二渠道；旧 Qoder（`qoder/*`，QoderWork）已从界面下线但路由保留 |
| R11 | Windows 产物在 WSL 交叉编译（`GOOS=windows CGO_ENABLED=0`，已验证可行）；macOS 产物走 GitHub Actions macos-latest（cgo 必需） | WSL 无法编 darwin cgo；CI 增加 darwin job |
| R12 | **无桌面 Linux 使用 `--no-tray` 参数** | 无参启动在无 DBus 环境托盘 panic 直接 exit 并提示；`--no-tray` 跳过托盘打印信息阻塞等待 Ctrl+C |
| R13 | **三接口兼容采用两层结构：内层 handler 不动，新增 `internal/gateway` 边缘层**，经 **in-process 调用**（`io.Pipe` + ResponseWriter 形状）复用内层 | 代码量比内联重构多 20%，但改动面小一个数量级（主链路仅 2 处调用点 + 1 个访问器），回归风险低、可脱离 pool 单测。**不得用 HTTP 自环**（`0.0.0.0` 监听不可作目标、鉴权双份、启动竞态） |
| R14 | **`Stream`/`Aggregate` 的 model 由调用方显式传入**，渠道不得用实例字段记忆「上次请求的模型名」 | 旧实现 qoder 用全局 `lastModel`，多账号并发会串号；traework 恒为空串。详见 `本地 docs/三接口兼容改造备忘.md` §3 |
| R15 | **Responses 的 `function_call` 必须是独立 output item**（带 `call_id`），Anthropic 的 tool_use 参数必须走 `input_json_delta` | 参考实现 `tokligence-gateway` 两处写法不合规范（塞进 `message.content`、start 里一次性给完整 input），Codex/Claude Code 会解析失败 |
| R16 | **无账号渠道（oczen）不建 auth 文件、不进 `reloadAccounts`、不参与禁用/冷却惩罚** | 匿名凭证是常量 `public`；`SyncToDir` 会剔除磁盘上不存在的虚拟账号，故只在装配时注入一次。单账号 + 不可重登 ⇒ 任何账号级惩罚都等于整渠道下线，故 4xx 一律走 `ErrPassthrough`（原文透传、不计错不冷却）。**2026-09-24 修订：429 也不再冷却**——原「429 短冷却是唯一需要的背压」经实测证伪：单账号无号可轮换，冷却后后续请求在挑号阶段被挡成 `503 no_healthy_account`，反而不如透传 429 让客户端按 `Retry-After` 自行退避；同理**传输层错误也不再累计 `errCount`**（默认 3 次网络抖动即冷却唯一账号）。两者由新增的 `server.Runtime.SingleAccount` 统一豁免（结构属性，不硬编码渠道名），启动时另调 `Pool.ClearPenalty` 自愈旧版遗留的冷却。详见 `docs/opencodezen渠道接入备忘.md` |
| R17 | **用量/积分双流水分口径统计，不强关联、不折算** | `internal/ledger` 双 JSONL（usage 按渠道×模型 / credit 按账号 earn·spend·expire）；写入仅 append 缓冲句柄（30s AutoFlush），读取仅在 UI 请求 `/api/usage` 时按月分段扫描聚合，常驻内存 ≈0。`Upstream.Stream` 返回末帧 usage（R14 同款显式传参哲学）。首见账号只记一条「存量额度」baseline，不逐条展开。**同 key 重复条目（WorkBuddy 伪键一对多）先聚合求和再差分**，每 key 每次刷新最多一条事件；升级首启将旧错误流水一次性归档为 `old-credit-*.jsonl` 并删快照重建 baseline（issue #38）。详见 `docs/用量积分流水记账备忘.md` |
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
| R29 | **多账号轮换的真实语义：粘性路由 + 错误下次生效** | 轮换**不是**「积分低就切」：① **粘性路由**优先复用上次成功的账号，直到冷却/禁用或连续 50 次成功；积分只在**选新号**时作排序键（临期 → 总余额）。② `handler.go` 的 `status>=400` 分支是「记惩罚 → 透传 → return」，**不在同一请求内换号**；只有**传输层错误**才 continue 换号。故故障转移是「下次请求生效」：请求1 用 A 失败并禁用 A，请求2 自动切 B。**这是全渠道统一行为**，回归测试 `internal/server/glm_rotation_test.go` |
| R30 | **清言「伙伴/群聊」任务不自动化** | 接口已探明（`mainchat-api/claw_agent` 完整 CRUD，`claw` = 「伙伴」），但**决定不实现**：① 每天 500 积分（≈5 积分实际价值）vs. 风控风险不对称；② 这些任务的设计意图是引导**真人**使用产品，脚本刷属典型薅羊毛特征；③ 用户手动点两下成本极低。**故本渠道自动化边界 = 保活对话 + 积分读取，不碰创建伙伴/绑定 IM/群聊等写操作** |
| R31 | **每日登录积分走 `member-api/member/daily_login_score`，已接入保活流程** | 初版探测的路径名**全错**（`login_bonus`/`daily` 均 404），真实端点是 **`daily_login_score`**（2026-09-26 从 Web 主包挖出）。主包里它是**页面加载时自动调用**的（`errorMessageShow:false`）⇒ 调用它 ≈ 打开一次网页，特征与网页端一致，**不是跨端伪装**。响应：`status=0` 领取成功；`status=10001 "今日已领取"` = **幂等非错误**。**故无论上游是「自动发放」还是「需主动调用」，调它都安全且有益**。⚠️ **实现陷阱**：`doJSON` 在 `env.Status != 0` 时抛 error，会把 `10001` 误报成「领取失败」→ 新增 `doJSONEnvelope`（只把 HTTP ≥400 与非法 JSON 转 error，业务码交调用方解释）。回归测试 `internal/glm/daily_score_test.go` |
| R32 | **不模拟 App 登录来触发额度发放** | 用户设想「模拟 App 登录动作」以避免手动开 App。**实测否决**：① Web 端**无**任何额度激活接口（8 个候选全 404）；② 换 App 头访问同一接口反而 401（`member_info` Web 头 200 / App 头 401）⇒ App 走**另一套认证**（设备指纹、App 签名等），不是加 UA 就能冒充；③ App 域名 `api.chatglm.cn` 独立存在。**风险不对称**：模拟 App 登录是**跨端伪装**，风控风险比只读 Web 私有接口高一个量级，且触发额度发放正是薅羊毛特征（与 R30 同一逻辑）。**结论：手动开 App 登录一次即可**（一次性成本，零风险，零开发） |
| R33 | **三个 chatglm 变体的模型名带上游代号后缀 `:moe_53f`；旧名保留为别名** | **实测**：6 个模型名逐个测服务端上报的 `parts[].model` —— 三个 chatglm 变体（普通/`zero` 推理/`deep_research` 沉思）**都上报 `moe_53f`**，即**同一底层模型的不同推理等级**（`moe`=MoE 架构、`53` 很可能指 5.3）；search=`ai-search`、ppt/video=`all-tools-glms-glms-v2`。故给三个变体加 `:<model>` 后缀（`glm/chatglm:moe_53f` 等），**search/ppt/video 不改**（其代号是工具标识非模型版本）。**两个必须同步的点**：① `resolveAssistant` 查表前**剥掉 `:` 后缀**（同时保留完整名查表），否则 `chatglm:moe_53f` 只能靠兜底命中；② **旧名保留为别名**，已配置旧名的客户端不断，但 `/v1/models` 只列新名。新增 `UpstreamModel()` 辅助函数。回归测试 `TestResolveAssistantStripsUpstreamSuffix` 断言带/不带后缀解析结果一致 |
| R34 | **新增渠道必须补 go xxxSch.Run(sctx)，否则该渠道的自动签到/保活从不运行** | **2026-09-27 实测发现**：加 glm 渠道时创建了 glmSch、注册进 runtimes、设了观察者，**但漏了 Run()** ⇒ GLM 的自动签到与保活**从未执行**。危害特征：**不报错、不崩溃**，且**手工触发仍可用**（面板按钮 / RunCheckinNow 走的是另一条路径），故极难发现——用户是手工签到后才察觉。**回归测试** cmd/wild-work/scheduler_start_test.go：静态扫描「定义了 xxxSch := scheduler.New(...) 就必须有 go xxxSch.Run(」，并交叉校验 runtimes 里声明的 Scheduler 都已启动。**新增渠道清单应加一项：创建 → 注册 runtimes → 设观察者 → **go Run** → 补测试** |
| R35 | **流式请求必须用独立的 StreamHTTP（不设 Client.Timeout）** | **2026-09-27 生产日志实证**：GLM 对话流走带 Client.Timeout 的 client，触发 context deadline exceeded **5 次**（09/26 22:42、09/27 00:03/00:09/00:12/00:18），**且无终止帧** ⇒ 客户端表现为「回答到一半停住」。根因：Go 的 Client.Timeout **覆盖整个请求生命周期（含读 body）**，对 SSE 意味着「长回答必被掐断」。**修法**（照 traework 既有模式）：Client 加 StreamHTTP *http.Client{Transport: tr}（**共用 Transport 复用连接池、但不设 Timeout**），ChatStream 改用它；main.go 两处 applyProxies 都要把 {glmUp.HTTP, glmUp.StreamHTTP} 一起传（SetTransportProxy 会**新建** Transport，只套一个会让流式漏掉代理）。**非流式 client 仍保留 Timeout**（一问一答需兜底）。回归测试 internal/glm/stream_timeout_test.go：证明流能跑过 HTTP.Timeout，且对照证明非流式仍会超时。**2026-09-29 推广到全部流式渠道**（issue #42）：此前只剩 glm/traework 有 StreamHTTP，qwenwork/qodercn/qodercom/qoder/workbuddy/ workbuddyai/oczen 的 ChatStream 全用带 Timeout 的 HTTP ⇒ 长流式同样会被掐断。现已给全部 7 个渠道补 StreamHTTP（共用 Transport、不设 Timeout，流式里 Select{StreamHTTP 非 nil 则用之}），main.go **两处** applyProxies（启动 + 面板热更新）都要把 {HTTP, BillingHTTP?, StreamHTTP} 一起传，否则只套 HTTP 会让流式漏掉代理 |
| R36 | **流被截断不得靠补 `data: [DONE]` 伪装成正常收尾**（issue #42） | **实证**：qwenwork/qodercn/qodercom/qoder 四份 sse.go 的 `sawDone` 是**死变量**（声明后从不赋值）⇒ 上游连接中断/读超时被掐断、或断在半个帧上（末行无换行），出口照样补 `data: [DONE]`，客户端拿到半截 tool_call arguments 报 "tool input was not fully received"，而网关日志**无任何错误**，极难排查。**修法**：parseNestedSSE 增加 `truncated` 出参（读错误/半帧 → true），`streamAsOpenAI` 里截断时改发一帧 OpenAI 规范 error（`code: upstream_truncated`）且**不发** [DONE]——调用方 `err!=nil` 同时保留。通用路径上游/sse.go streamCore 也做了同样的截断→error帧改造。刻意保留的行为：上游「正常 EOF 但漏发 [DONE]」（帧完整、末行有换行）仍兜底补 [DONE]，避免误伤本就补的渠道。回归测试：qwenwork/qodercn/qodercom/qoder 各 `stream_truncation_test.go` + 通用 `TestStreamTruncationNotDisguisedAsDone`，覆盖四种输入（传输层错误/半帧/正常 [DONE]/EOF 漏发） |
| R37 | **TraeWork AuthCode 交换：4xx 属终态，不得换 origin 重试掩盖首因**（issue #50） | 旧实现 `ExchangeAuthCode` 对 4xx 也 `continue` 试下一个 origin，把首选 api.trae.cn 的真实错误（403/20401 设备数上限）覆盖成回退 origin 的 400/10101「无效参数」后再打印 ⇒ 表象彻底指向参数写错，且 authCode 一次性、4xx 后重试纯属刷屏。**修法**：4xx（除 429/408）立即返回携带**首个** origin 真实响应的 `*AuthCodeRejectedError`（含 origin/status/body）；`IsDeviceLimitReached` 单表识别 20401（可行动提示「去其它设备登出释放名额，最多 10 台」）；`login_trae.Poll` 把终态错误固化到 `login-state.json` 的 `Err` 并清空 AuthCode，后续轮询短路不再每 2s 重消耗。**设备号持久化**：官方客户端 `getDeviceId()` 是持久的，本仓每次登录 `randHex(32)` 随机新生成 = 每点一次都是新设备。改为从 `device-id.json`（`login-state.json` 同目录、独立文件）复用，缺失才生成并落盘（不复用 login-state，它登录成功/取消即删）。**暂未做**版本常量对齐 0.1.69（issue 自标「未证实是成因」，行为侧有风险，无证据不动） |
| R38 | **上游按模型限流时不得软化整个账号**（issue #53） | **2026-09-29 最小修复（不做大架构改动）**：WorkBuddyAI 上游模型级 429（6004 / body 含 `switch to the other models` / `usage exceeds frequency limit`）在 `Classify` 里判为 `ErrPassthrough`（请求级、不冷却账号），透传原文让客户端按 Retry-After 自退避或换模型——**不**把该账号其它还能用的模型一起拖进 SoftCooldown。与 R16 单账号渠道同哲学。**不做的部分**：pool 升级到 model 维度状态、`stickyKey(kind)`→`(kind,model)`、state v4（issue 作者提议的两种更大改造），它们动 pool/state/handler 三核心、~370 行，需单独评估（作者已主动提出可拆两个小 PR）。注意此判据仅限 WorkBuddyAI：其它渠道的 429 语义仍是账号级。**本仓未采用**：master 保留按 (账号, 模型) 粒度冷却的实现（`provider.IsModelScopedSoftRate` → `pool.CooldownModel`，见 R47），`Classify` 对模型级 429 仍返回 `ErrSoftRate` |
| R39 | **`scheduler.Config` 的时间字段：`nil` = 未配置（落默认），`[]int{}` = 本渠道没有这类定时任务** | **2026-09-23 实证**：`scheduler.New` 原用 `len(...) == 0` 判默认值，把两者混为一谈 ⇒ 传 `nil` 想关掉定时任务的渠道被静默补上默认时间。`main.go` 里命中四处：**qoder / oczen** 每天 09:00、21:00 各跑一次必然失败的签到（面板按 `last_checkin_ok` 渲染出红色「签到失败」标签，消息是「qoder 暂无签到活动」/「no refresh token」）；**workbuddyai** 注释写「Keepalive 关闭」但 22:00 照常每日刷 token；**qwenwork 最严重** —— 注释明写「定时保活反而会与千问办公 App 互踩」，而保活批次每天照跑。同一个「无签到活动」概念在 `app.go` 有 `noExplicitCheckin()` 守卫（手动签到、首次签到路径都据此跳过），唯独 `scheduler.New` 的零值兜底把它抹掉。**修法**：两处 `len(...) == 0` 改 `== nil`；四处调用点改传 `[]int{}`；`Run()` 补「两个列表都为空时阻塞等 ctx/配置变更」—— 否则 `nextFireMinutes` 返回零值走 `IsZero` 兜底，变成每天 1440 次空转。回归测试 `internal/scheduler/scheduler_defaults_test.go`（零值落默认 / 显式空关闭 / `CheckinHours` 旧字段兼容 / 无任务时 `Run` 阻塞且响应取消） |
| R40 | **流内业务错误不得伪装成正常收尾；兼容层不得吞 error 帧**（2026-10-03） | **背景**：traework 的 `event:error`（3004 限流/1005 权益）旧实现写成 `delta.content` + `finish_reason:stop` + 补 `[DONE]`，且函数返回 `nil` ⇒ 客户端看到「正常结束、内容莫名其妙」、agent 拿半截回答继续跑，且 handler 无从得知失败 ⇒ **账号不冷却**；更糟的是 `NoteSuccess`/`stickySuccess` 在读流**之前**已执行，粘性路由把请求钉死在中招账号上，重试必然再撞。**修法**：① `solosse` 错误分支改发 OpenAI 规范 error 帧（限流标记 `upstream_rate_limited`）、**不补 `[DONE]`**、把错误返回调用方；② 新增 `provider.StreamErrorClassifier`（`Kind()`），handler 用 `errors.As` 取值后按 kind 冷却账号（软 60s / 硬 12h / 禁用），粘性路由见 `status.Cooling` 自动让位，无需显式 `stickyClear`；③ **兼容层必须识别 error 帧**——`gateway.parseChatSSELine` 此前只认 `choices`，error 帧被静默丢弃，致 `/v1/messages` 发 `end_turn`+`message_stop`、`/v1/responses` 发 `response.completed`+`[DONE]`，把失败伪装成成功（该缺口对内层所有渠道通用，含 R36 的 `upstream_truncated`）。**协议合规要点**：Anthropic `error.type` 是 **9 元判别联合**（`api_error`/`rate_limit_error`…），**不得填内层私有码**——限流映射为 `rate_limit_error`；Responses 用 `response.failed`（不补 `response.completed`/`[DONE]`），`response.error.code` 亦须映射到枚举（`server_error`/`rate_limit_exceeded`），私有码只放 `error` 事件的自由 `code` 字段。**3004 的限流维度是待强化假设**（R23b）：观察仅 1 例且来自 checkin 路径，只足以排除 IP/全局级；因软冷却仅 60s、判据不成立时最坏影响是闲置一分钟，故按账号级处置。回归：`traework/solosse_test.go`、`server/stream_error_penalty_test.go`、`gateway/stream_error_frame_test.go`、`scripts/ui-static-check.mjs` |
| R41 | **思考强度统一走 `internal/reasoning`**：客户端各写法（`reasoning_effort` / `reasoning.effort` / `thinking.*` / `output_config.effort` / `enable_thinking` / `disable_reasoning` / `think`）在**内层 handler**（`prepareChatBody`）归一化成顶层 `reasoning_effort`，非法/自相矛盾回 400 `invalid_reasoning_control`；**渠道层只做方言投影**（WorkBuddy 家族 → low/high/max；Qoder → `is_reasoning` + `parameters.reasoning_effort`/`enable_thinking`；TraeWork 协议无此字段），不重复做兼容字段解析 | 移植自 Buddy2api `reasoning_controls.py`。规则只应存在一处：渠道各自解析会让「同一客户端写法在不同渠道表现不同」。默认档 `compat.reasoning_effort` 只在**客户端未表达**时注入，对 WorkBuddy 双面与 Qoder 生效（Qoder 的具体档位由渠道层按模型 ladder 就近降级）。Loomy 按 `RealmLoomy` ladder Clamp；**raccoon 例外：必须剥离该字段**（其实测默认档最深，下发档位反而削弱思考量，见 §13 备忘） |
| R42 | **Responses 的思考链是独立 `reasoning` output item，且必须排在 `output_index=0`**；`output_index` 按实际输出顺序动态分配（思考 → 文本 → 工具），不得硬编码 | 官方顺序要求思考先于回答；硬编码 message=0 会让带思考的响应出现倒序 item。思考增量只在文本开始前接受，文本开始后到达的片段丢弃 |
| R43 | **档位必须按模型能力就近降级，不得按模型家族固定压档**：能力表优先取上游目录接口的 `reasoning.supportedEfforts`/`defaultEffort`（`provider.ModelInfo.SupportedEfforts` → `internal/reasoning.Caps`），缺失才回落 `internal/reasoning/catalog.go` 的 realm 静态表；未收录模型档位原样透传 | 上游各模型可接受档位差异很大（国内版 `deepseek-v4-pro` 无 `max`、国际版 `deepseek-v4.1-flash` 只认 `high`、`glm-5.1` 只认 `medium`），旧的「deepseek 家族固定压 low/high/max」会发出非法档位。国内版/国际版**分表**，绝不混用（同一模型两面档位不同） |
| R44 | **DeepSeek 系「开思考」= `thinking:{"type":"enabled"}` + 档位，二者缺一上游按不思考应答**；网关在客户端表达开思考时自动补 `thinking.type`，并给 assistant 消息回填 string 类型的 `reasoning_content`（多轮一致性）。由 `compat.deepseek_thinking`（默认开）统一开关，客户端显式给出的 `thinking.type` 绝不覆盖 | 逆向官方客户端 `codebuddy.js`（`thinkingFormat:"deepseek"` + `requiresReasoningContentOnAssistantMessages`）的结论；此前只发 `reasoning_effort`，DeepSeek 思维链可能一直为空。与参考实现差异：**不在客户端未表达时强行开思考**，避免给不需要思考的请求增加延迟与额度开销 |
| R45 | **Qoder 思考投影按官方客户端 `bve()` 的三处同源写法**：`model_config.is_reasoning` + `parameters.reasoning_effort` + `parameters.enable_thinking`，三者必须同源（绝不出现 `is_reasoning=true` 配 `enable_thinking=false`）；档位能力来自上游模型目录的 `thinking_config`（`provider.ModelInfo` → `reasoning.Caps` 的 `RealmQoder` 面，**与 WorkBuddy 分表**，无静态兜底）；客户端要关闭但该模型无 `disabled` 节点（如 `glm-5.3`）时**降到最低档**而不是发上游不认的 `none`；`parameters` 恒下发（见 R21） | 逆向 Qoder CN 桌面版内置 SDK（`@qoder-ai/qoder-cn-agent-sdk` 的 `qoder-worker-runtime.obf.mjs`，CLI v1.1.53）拿到；实测 `parameters.reasoning_effort` 确实改变生成量（`none` 2402 < 基线 2952 < `medium` 3455 tokens）。**R21 已推翻「legacy 端点不下发思考链」这条结论**（当时是因为请求体/请求头没对齐桌面版） |
| R46 | **Qoder 请求体与请求头按桌面版「实测抓包」逐字段对齐**（不再只参考 SDK 源码）。body：补顶层 `system` 数组（从 system 消息抽文本块，与 `messages[0]` 同构）、`task_id:"common"`、`source:1`、`version:"3"`、`is_retry:false`、`session_type:"app"`、`aliyun_user_type:""`、完整 `model_config`（`key/display_name/model/format/is_vl/is_reasoning/api_key/url/source/max_input_tokens`）、`business` 富对象（`product/version/type/id(=request_set_id)/name/begin_at/stage`）、`tools` 恒为数组、`chat_context.text` 与 `extra.originalContent` 为**字符串**；`parameters` **恒下发**且含 `max_tokens`（目录 `max_output_tokens`，实测目录无此字段 → 常量 32000）与 `context_length`（`context_config` 中标 `is_default` 的档，未知则不下发）。headers：`cosy-clienttype: 10`、`cosy-data-policy: disagree`、`cosy-version: 1.1.57`（签名 payload `cosyVersion` 必须同改）、补 `cosy-business-product/-type/-scene`、`cosy-machineos: x86_64_win32`、`cosy-machinehostname`、`accept-language`，去掉桌面端没有的 `cosy-clientip` | 依据 `_spy/http-bodies/*.json`（7 个真实请求体）+ `_spy/qoder-real-request.json`（27 个真实请求头）。**对齐后 legacy `agent_chat_generation` 立刻开始下发可见思考链**：探针 `reasoning_content` 1876（medium）/26145（xhigh）字，生产链路端到端 36845 字，`usage.completion_tokens_details.reasoning_tokens` 1435–11913 —— 这是「思考强度终于可见」的关键修复。回归护栏：`TestLiveProbeProductionPath`（走 `ChatStream` 全链路）。**唯一刻意保留的差异**：`accept-encoding` 固定 `identity`（桌面端是 `br,gzip,deflate`；Go 手动设置该头后不会自动解压，brotli 需额外依赖）。**版本同步要求**：`clientVersion` 同时出现在请求头与 `business.version`，改动必须成对 |
| R47 | **冷却与粘性都必须按 (账号, 模型) 粒度**：`pool` 冷却分两层 —— `until` 账号级（token/session/余额/风控等与模型无关），`modelUntil` 模型级（`CooldownModel`）；`Pick(model)` / `PickExcluding(tried, model)` 同时排除在该模型上冷却的账号，`stickyKey(kind, model)` 按模型独立粘性。**粒度判定**：429 软限流按 `provider.IsModelScopedSoftRate(body)` 分流 —— body 含 `switch to the other models`（WorkBuddy 系 6004 频率限流原文）→ 模型级；否则保守走账号级（账号级最坏多冷却一个号，有号可轮换；误判成模型级会让同账号 N 个模型各白撞一次）。`ErrModelBlocked`（11102）改为兑现注释承诺的 (账号,模型) 负缓存。**必须保持账号级的**：`ErrHardCredit`（429+14018 / 402 —— 误按模型级会让 N 个模型各撞一次长硬冷却）、`ErrWafBlock` / `ErrAccountFault`、refresh 失败、5xx 计错。state 升 **v4**（新增 `model_until`）；因余额口径未变，`creditsStale` 判据改用 `creditsLayoutVersion`(=3)，v3 文件读入不再置 stale。`Status.ModelCooling` 单独暴露模型级冷却，且不写账号级 `reason`（合并展示会让人误以为整个账号被限流）。`ClearPenalty` / `ReenableIfCredits` 一并清 `modelUntil`。**面板链路要单独过一层**：`app.AccountView` 是与 `pool.Status` 平行的独立视图结构，`pool.Status` 新增字段必须同步映射过去 —— 漏了不编译报错、单测也照绿，只表现为前端标签永不显示（2026-09-28 第二实例注入实测抓到） | **2026-09-28 生产日志实证**：A 账号的 `deepseek-v4.1-flash` 自 02:57 撞 6004 起被限流，而同一账号的 `gpt-5.6-luna`（倍率 0.14）在 21:07 仍能成功；上游 6004 原文即 `alternatively, you can switch to the other models to continue using it`（当月 48 次）——「限流按模型独立」是上游自己声明的。此前账号级冷却导致日志出现 13 次「选中 A → 1 秒后 429 → 换 B」，A 每次轮换只得到 1 次请求机会。回归测试：`internal/server/handler_model_cooling_test.go`（4 例）+ `internal/pool/pool_test.go`（8 例）+ `internal/provider/provider_test.go`（判定表）+ `internal/app/account_model_cooling_test.go`（视图映射 / omitempty） |

| R48 | **小浣熊登录改用「网页版 CDP 捕获」，废弃桌面协议回调**（2026-10-10，移植自上游 R44） | **背景**：原「浏览器授权登录」靠改写 HKCU 的 `office-raccoon://` 协议注册接住深链授权码，仅 Windows 可用、要求本机装官方客户端、且与客户端抢协议。**实测**：网页版登录成功后前端 JS 把凭据写进 Cookie `raccoon_refresh_token`（域 `.xiaohuanxiong.com`），与桌面端同一套（JWT 无平台声明）。**修法**：`internal/login_raccoon` 改为网页版流程（`web.go`）——拉起**独立 profile** 的 Edge/Chrome（复用 `internal/cdp`）打开 `office.xiaohuanxiong.com/home?loginModal=true` → CDP 轮询 Cookie → **判据是 JWT `owner_type==users`**（访客也下发 Cookie，见 R28 同款教训）→ 调 `/api/web/auth/v1/refresh` 换取并落盘凭据。**跨平台**（纯 HTTP + 系统浏览器，不再依赖注册表）；桌面协议登录代码（`protocol*.go`、`--raccoon-callback` 入口）**保留但不再调用**，仅作旧版注册表残留的启动自愈（`healRaccoonProtocol`）。**「从客户端导入」路径保留**。**已知限制（面板弹窗已告知）**：上游 refresh 轮换 refresh_token，同账号凭据共用（本工具 + 浏览器/客户端）会互踢。回归测试 `internal/app/raccoon_weblogin_test.go` |
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
| 账号管理 | 双列卡片网格，账号名/UID/积分/签到状态，图标按钮操作（签到/刷新/停用/删除） |
| 自动签到 | 签到时间（HH:MM 多组）+ 开机自启开关（左右布局） |
| 渠道费率 | 七渠道模型定价表（按渠道分组，合并单元格），刷新按钮 |

**外部静态资源只有一处：echarts（用量折线图），走 ZStatic `s4.zstatic.net` + SRI。**
不得改用 BootCDN / Bootcss / Staticfile / Polyfill.io —— 这几家已被黑产收购并实控，
2024-07 起发生供应链投毒（uBlock Origin 等已直接屏蔽），实测 `cdn.staticfile.org`
现已完全不可达；`cdn.staticfile.net` 属同一家，一并避开。
- `index.html` 的 `<script>` 必须同时带 `integrity`（SRI）与 `crossorigin="anonymous"`：
  CDN 或链路被劫持时浏览器会拒绝执行篡改后的脚本，退化成 `app.js` 的降级提示
  （`typeof echarts === "undefined"`），不会执行恶意代码。
- **SRI 值必须取自 npm 官方 tarball 逐字节校验**（`registry.npmjs.org/<pkg>/-/<pkg>-<ver>.tgz`），
  不得照抄第三方页面或凭 CDN 当前返回内容直接采信 —— 否则等于把「是否被篡改」的判据
  也交给同一个可能被投毒的源。
- 跨域 SRI 要求响应带 `access-control-allow-origin`（zstatic 实测 `*`）；
  换 CDN 前先确认该头，否则脚本会被 CORS 拒绝、整块图表静默失效。
- **升级 echarts 版本时必须重新计算 SRI**，否则脚本被浏览器拒绝执行。
  算法：`openssl dgst -sha384 -binary f.js | openssl base64 -A`（前缀 `sha384-`）。

管理 API（REST，均挂 `/api/*`）：

```
GET  /api/state                    # 全量状态（账号/积分/签到/配置）
POST /api/login/start              # {channel} → {auth_url}
POST /api/login/glm_auto           # 智谱清言自动登录：拉起独立 profile 浏览器（见 R27）
GET  /api/login/glm_auto_status    # → {status: idle|pending|success|failed|cancelled, uid?, nickname?, error?}
POST /api/login/glm_auto_cancel    # 取消自动登录并关闭浏览器
POST /api/login/glm_token          # {refresh_token} → {uid} 智谱清言手工兜底（见 R22）
POST /api/login/cancel
POST /api/account/checkin          # {uid}
POST /api/account/checkin_all
POST /api/account/refresh          # {uid}
POST /api/account/refresh_all
POST /api/account/remove           # {uid}
POST /api/account/disable          # {uid,disabled} 停用/启用
POST /api/account/resource_detail  # {uid} → 积分明细
POST /api/config/checkin_times     # {times:["09:00","21:30"]}
POST /api/config/listen            # {host,port,admin_password?}（非环回时必须带密码）
POST /api/config/admin_password    # {password}（空 = 关闭面板鉴权，仅环回监听允许）
POST /api/config/api_key           # {key}
POST /api/config/autostart         # {on:bool}
GET  /api/fees                     # 渠道费率（本地缓存 + 按需刷新）
POST /api/fees/refresh             # 异步刷新费率
GET  /api/logs                     # 最近 300 行日志
POST /api/quit                     # 退出程序
```

## 5. 渠道（已实现 WorkBuddyCN + WorkBuddyAI 国际版 + TraeWork + TraeCode + QoderCN + QoderCOM 国际版 + 千问办公 + 商汤小浣熊 + Loomy + MonkeyCode + 智谱清言 + OpenCodeZen 匿名；旧 Qoder 已下线）

1. 新建 `internal/<channel>/` 包，实现 `provider.Upstream` 接口
2. `internal/auth` 增加对应 `Load<Channel>Dir()`（文件名前缀 `<channel>-*.json`；
   **glob 边界**：`qoder*.json` 会吞掉 `qodercn-`/`qodercom-` 前缀，LoadQoderDir 必须显式排除）
3. 装配处注册 `server.Runtime{Kind, Pool, Upstream, StaticModels}` + `app.Runtime{..., Scheduler}`
4. 前端渠道选择器加一项（`web/app.js` 的 `CH_LABEL` + `CH_CLASS`，前缀/帮助清单自动跟随，见 §6 第 31 条）；
   `internal/login_<channel>` 实现登录编排（如需）
   > **无账号渠道（oczen）跳过第 2、4 步**：不建 auth 文件与加载器，虚拟账号由 `main` 装配时注入 pool，
   > 且 `app.reloadAccounts` 不得纳入（否则 `SyncToDir` 会把它剔除）。

> provider.Kind 即模型名前缀；server 按 `channel/<model>` 前缀路由，无需改接口。
> **流式（所有对话渠道）**：走独立的 `StreamHTTP`（**无总超时**）+ `provider.IdleReader`
> 空闲兜底，流中断补 error 帧 + `[DONE]`（见 §6 第 32 条）；非流式调用（目录/积分/刷新/签到）保留总超时。
> **QoderCN（`qodercn/*`）**：qoder2api 参数形态（cosyVersion 1.0.10、18 头含 cosy-scene 族、
> session_type=qoder、identity userType 实测回填）；签到双路径（campaigns 主 + daily-check-in 兜底，
> 实测 legacy 已 DISABLED）；每日 10:00 开放 + 10:00–12:00 窗口内重试（见不变量 23）；动态模型表（无静态兜底，上次成功缓存）。详见 `docs/qoderCN渠道接入备忘.md`。
> **QoderCOM（`qodercom/*`）**：国际版（qoder.com/openapi.qoder.sh/api1+api2.qoder.sh 三域分离）；
> 凭据与 CN 区完全隔离（双向 401）；签到仅 campaigns（无 daily-check-in，实测 404）；每日 10:00 开放 + 10:00–12:00 窗口内重试（同 CN，见不变量 23）。详见 `本地 docs/qoderCOM渠道抓包分析与接入计划.md`。
> **旧 Qoder（`qoder/*`，QoderWork）已从界面下线**：代码与路由保留，存量账号仍可用；不新增功能，后续可移除。
> 旧 Qoder 无签到活动：`DailyCheckin` 返回错误，调度器只做 token keepalive；故 `noExplicitCheckin`
> 排除 `qoder`（面板不显示手动签到按钮），web/app.js 的 `NO_EXPLICIT_CHECKIN` 与之同源。
> 签到定时任务须用**显式空切片** `CheckinMinutes: []int{}` 关闭（2026-09-23 修复：原为 `nil`，
> 被 `scheduler.New` 补成默认 9:00/21:00，每天两次必然失败的签到调用与失败日志）。
> QoderCN / QoderCOM 已实现签到（campaigns 主路径），保留手动按钮。
> WorkBuddyAI 国际版：`DailyCheckin` 实现为「免费模型对话保活 + 签到探测」（对用户透明，无前端界面）；
> token 有效期 365 天，故 KeepaliveHours 设为 nil。详见 `docs/workbuddy国际版渠道接入备忘.md`。
> **MonkeyCode（`monkeycode/*`，平台托管模型）**：凭据由面板「从本机客户端导入」从官方客户端
> （ohmyagent）的 settings.json 取得，一个账号 = `oma_` api_key + `omas_` signing_secret；
> 按模型声明的 type 分流到 `{base}/messages`（Anthropic 形状）或 `{base}/responses`（Responses 形状），
> 两路都是无状态 SSE；上游无目录/额度/刷新接口 → 模型表静态、额度恒 0、`RefreshToken` 空实现。
> 协议对照参考：`codkeep/MonkeyCodeReverseEngineer`（Apache-2.0）。
> **小浣熊（`raccoon/*`）**：两条添加路径 —— 面板登录按钮走**网页版自动登录**（拉起独立 profile 的
> Edge/Chrome，登录后经 CDP 捕获 Cookie `raccoon_refresh_token`，见 internal/login_raccoon/web.go；
> 跨平台，不再依赖 Windows 注册表，详见 R48），弹窗次按钮「从客户端导入」读本机
> `.box-agent/config/auth.json`。凭据 access_token ≈1h + refresh_token ≈30d，保活每 4 小时一次
> 以减少「请求先 401 再刷新」的往返；无签到端点。**注意互踢**：上游 refresh 轮换 refresh_token，
> 同账号凭据共用（本工具 + 浏览器/客户端）会互踢，需各自单独登录（独立 `sid`）才能并存。
> 上游默认档即最深思考，故不接档位面。
> 接口结构整理：`xxhhlk/raccoon2api`（MIT）。
> **Loomy（`loomy/*`）**：导入型渠道 —— 凭据由面板「从本机客户端导入」读
> `C:\Users\Public\Loomy\<hash>\userData\auth-session.json`。上游**无 refresh 端点**，
> session 约 14 天，到期需重新导入；故 `RefreshToken` 留空、定时任务全关（保活会被
> 按「无 refresh token」跳过，但每天仍空跑一次并记失败）。鉴权失败是 **HTTP 200 +
> 业务码 `100002`**（不是 401），业务码判定必须排在状态码判定之前。
> 接口结构整理：`xxhhlk/loomy2api`（MIT）。
> **OpenCodeZen（`oczen/*`，匿名免费）**：凭证固定字面量 `public`，无账号/无签到/无积分；
> 免费档有三道闸门（规范 `ses_<12hex><14Base62>` 会话头 + `stream:true` 且 tools 含 `bash`/`read` +
> OpenCode CLI 伪装头），缺一即 403 FreeTierError；面板固定一项「[OpenCodeZen] 匿名」、积分显示「不适用」，
> 不可增删停用。渠道特性（匿名凭证 `public`、三道免费档闸门、伪装头清单）见
> `internal/oczen/constants.go` 包注释与 §6 不变量 29/30。

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

## 6. 关键不变量（改动前必读）

0. **每次代码变更后必须本地重新构建 `dist/wild-work.exe`**（见 §8）。
   `dist/` 在 `.gitignore` 中，CI 只产出带平台后缀的 `wild-work-<os>-<arch>`，
   **不会**生成 `dist/wild-work.exe`——该文件只能手动构建。
   不重建会导致：本地运行的二进制与源码不一致（例如改了版本号但仍显示旧版本）。
1. `PrepareBody` 三改写勿动：强制 `stream=true`、`tool_choice` 归一化、`developer→system`
2. 日志/面板/消息框**零 token**：不得输出 access/refresh token（调试用假 token）
3. auth 文件嵌套格式 `{auth:{...},account:{...}}`，`internal/auth.Parse` 与 login.SaveAuth 必须一致
4. `config.listen` 兼容新对象格式 + 旧字符串格式 `":7863"`
5. `data/state-*.json` 只增不减字段，向后兼容；旧 state.json 自动迁移
6. 托盘回调必须 goroutine 化
7. `config.example.json` 与 `config.Default()` 同步
8. 上游 HTTP ≥400 错误直接透传原始响应，不包装
9. 定价缓存持久化到 `data/pricing-cache.json`，启动加载，超 1h 自动刷新
10. 无桌面 Linux 必须 `--no-tray`，不带参数 panic 直接 exit 提示
11. **粘性路由**：`pickWithSticky` 优先复用上次账号，直至连续成功请求达 50 次或遭遇错误冷却。成功时 `stickySuccess` 递增计数，错误时 `stickyClear` 清除粘性记录。不使用 credits 阈值（pool 中余额是 stale 数据）。**粘性 key = (渠道, 模型)**（`stickyKey`）：上游按模型独立限流/计费，只按渠道粘性会让「A 在 flash 上被限流」把 A 上仍可用的 luna 一起粘到 B。粘性表设容量上限 `maxStickyEntries`（key 含模型后条目数不再有天然上界，= 渠道数 × 客户端请求过的模型数），超出时淘汰最久未用者。
12. **`internal/server` 主链路不得被绕过**：`POST /v1/chat/completions` 与 `GET /v1/models` 由内层直接服务，`internal/gateway` 只接管 `/v1/responses`、`/v1/messages`、`/v1/messages/count_tokens`。
13. **兼容层调用内层只能经 `Gateway.call()`**（`io.Pipe`），调用方读完必须 `res.Close()`，否则内层 goroutine 可能阻塞在 Write 上泄漏。
14. **`pipeRW.Flush()` 为空操作是刻意的**：`io.Pipe` 无缓冲，Write 即送达；不要改成缓冲 + 定时 flush。
15. **错误分类 429 必须优先于 hardMarkers**：限流 body 高频带 `quota exceeded`，先判 hardRule 会把限流误归余额耗尽 → 12h 硬冷却。三渠道 `Classify` 均已修复此顺序。
    - **唯一例外：429 + 业务码 14018**（积分耗尽 —— 结构化码，不是文案）→ 硬冷却弃号。业务码判定必须走 `provider.CodeMarker`（容忍 `{"code": 14018}` 的 JSON 空白与引号形态），字面量 `strings.Contains` 会漏判。
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
    - **`InfoOnly`（2026-09-25 新增）**：条目**只展示、不入任何算术**（`Summarize` /
      `ExpiringWithin` / `ledger.DiffCredits` 三处一律跳过），前端加「仅展示」角标。
      用于**同账号下计量单位不同的另一套额度**——MonkeyCode 的「每日 Token 额度」（单位是 token）
      与「积分余额」（credits）都该显示，但相加无意义。构造时须同时置 `Usable=true`。
    - **明细路径与刷新路径同源回写 pool 快照（`creditSnapshot`，2026-10-11）**：`ResourceDetail`
      与 `creditTotals` 共用 `creditSnapshot`——取数成功即 `SetCreditDetail` + ledger 差分，故 hover
      明细也会刷新顶层快照；前端 `showCreditDetail` 用响应的 `usable_remain/expiring_remain/
      unusable_remain` 回写卡片与汇总条（`data-uid` 定位，不全量重绘），`refreshOne/refreshAll/
      checkin/checkinAll` 成功后清 `detailCache`。背景：面板「同屏两数字对不上」实为**时差**
      （顶层仅刷新事件/30min 循环更新，明细是 hover 实时值），已实测同响应内上游汇总字段==明细池
      相加，非上游口径问题。守门测试 `TestResourceDetailWritesBackPoolSnapshot`。
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
21. **Qoder 的客户端版本号只有一处定义（`internal/qoder/clientVersion`）**：它同时出现在请求头 `cosy-version`、
    签名 payload 的 `cosyVersion`、以及 body 的 `business.version`。三处必须同值 —— 只改头会让上游看到
    「头说 1.1.57、body 说别的」的自相矛盾身份。桌面版升级后照 `_spy/qoder-real-request.json` 重新取值。
22. **Qoder 的 `parameters` 恒下发**（至少带 `max_tokens`），且**请求体/请求头形状以实测抓包为准，不以 SDK 源码为准**：
    R20 时期按源码推的形状少了顶层 `system`、`task_id`、完整 `model_config` 等字段，导致 legacy 端点一直不下发
    `reasoning_content`，被误判成「上游不支持」。改动 Qoder 请求形状前后，必须跑 `TestLiveProbeProductionPath`
    （走 `ChatStream` 全链路，断言 `reasoning_content` 非空）。
23. **档位「对外声明」只有一处入口（`reasoning.ListingForKind`）**：`/v1/models` 与面板费率表
    （`/api/fees`）共用它取档位，其中已含「该渠道是否有档位能力」的判断（TraeWork / 千问办公必须为空）。
    新增渠道或更换档位来源时只改这一处——两处各写一份判断必然漂移，面板显示的档位就会与实际下发的档位不一致。
    - **`SupportsEffortKind` 与 `RealmForKind` 必须同一次改完**：前者放行而后者未登记该渠道时，
      能力会写进 `RealmCN`（`RealmForKind` 的 default 分支），污染 WorkBuddy 国内版的档位表；
      且 `HasRemote("cn")` 变真后 `ensureEffortCaps` 对真实国内版渠道直接 return，远端权威源被饿死。
      守门测试：`TestEffortKindHasOwnRealm`。
    - **Qoder / QoderCN / QoderCOM 三个渠道各占一个 realm**（`RealmQoder` / `RealmQoderCN` / `RealmQoderCOM`）：
      `Catalog.SetRemote` 是「整桶替换」（`remote[realm] = bucket`），共用面时后拉到的渠道会整份覆盖
      先拉到的，而三者模型目录互不相通（同名模型 ladder 未必相同）→ 按错 ladder 降级发非法档位。
      守门测试：`TestQoderRealmIsolation`（各包另有投影侧隔离用例）。
    - **QoderCN / QoderCOM 的档位下发已线上实测**（2026-09-22，QoderCN 真实账号）：上游接受
      `parameters.reasoning_effort` 与 `parameters.enable_thinking`，档位梯度真实存在
      （low 2206 字 < medium 2502 字 < xhigh 180s 超时截断）。**保守守卫保留**：模型未声明
      `thinking_config` 时只翻 `model_config.is_reasoning`、不下发档位字段，与「面板/`/v1/models`
      不声明该模型档位」严格对齐。完整实测矩阵见 `docs/qoderCN渠道接入备忘.md` §8。
    - ⚠️ **`parameters.enable_thinking` 必须恒下发**（与 `model_config.is_reasoning` 同源），
      不能只在档位非空时写：实测只发 `is_reasoning=false` 而缺 `enable_thinking` 时上游
      **关不掉思考**，反而思考爆炸（3127 个 reasoning 块 / 1.06MB，180s 未收尾、正文 0 块）；
      补上 `enable_thinking=false` 后同一请求 53.6s 收尾、reasoning 块 0、正文 5196 字。
      守门测试：`TestBuildAgentBodyReasoningFields`（含「未表达时 enable_thinking 必须为 false」）。
    - **`internal/qoder`（QoderWork 渠道）已同样实测并修复**（2026-09-22；与 QoderCN 同一上游目录、
      同 14 个模型、同 `qfmodel` = qwen3.8-flash ladder `[low medium xhigh]`）：只发 `is_reasoning=false`
      而缺 `enable_thinking` 时 **1793 个 reasoning 块 / 608KB、180s 超时截断、content 0 块**；
      同模型同 prompt 补上 `enable_thinking=false`（官方关闭形态）后 48.2s 收尾、reasoning 块 0、
      正文 4358 字。三处投影现均恒写 `enable_thinking`。
      复现/验证：`WILDWORK_PROBE_CASES=1,12 go test -tags live ./internal/qoder/ -run TestLiveProbeEffort -v`
      （用例 1 = 缺陷形态，用例 12 = 官方关闭形态；修复后用例 1 应正常收尾、reasoning 块 0）。
    - **Qoder 双区签到只能走 campaigns，且状态必须结构化上报**：
      legacy `daily-check-in` 已 DISABLED（其 claim 恒返回 409 会造成「假成功」，故不再使用）；
      每日 10:00 开放 + **10:00–12:00 窗口内每分钟重试**（活动可能在整点后才创建，只触发一次会漏领一天）。
      `CheckinMinutes=[10:00]` + `CheckinRetryUntil=12:00`；完成标记按「**日期+时段**」而非仅日期
      （避免早间成功吞掉晚间时段）。新增 `provider.CheckinReporter` 结构化状态
      （claimed/already/no_campaign/no_token/error）取代 error 文本判断 —— 旧实现无法区分
      「活动未上线」与「真出错」，且「无可用活动」不再当成功（漏领根因）。
      守门测试：签到窗口/完成状态用例（已验证暂时关闭修复时失败）。

24. **千问办公推理 body 必须带 `business.product`**（`internal/qwenwork/constants.go::BusinessProduct`）：
    上游按它选「模型目录」，缺省时推理端点恒回 HTTP 200 + envelope
    `{"code":"503","message":"Model catalog unavailable"}`，且**与请求头集合、与 `Encode=1`/body 编码、
    与 body 其余字段（model_config / system / tools / parameters / chat_context / session_type）全部无关**。
    实测矩阵与取证方法见 `本地 docs/千问办公QwenWork逆向对比备忘.md` §10。
    改本渠道请求形状前后必须跑 `TestLiveProbeReasoning`（`-tags live`，走 `ChatStream` 全链路）。
    **本渠道刻意不投影任何思考字段**：千问办公官方客户端本身没有思考控制设置（无档位/开关 UI），
    抓包确认其请求体也不带 `reasoning_effort` / `enable_thinking` —— 这是符合官方行为、**不是缺口**，
    不要为它补档位投影（用户 2026-09-22 确认）。
    - **`expires_in` 单位是【秒】，且 `expiresAt` 可被 access token 的 JWT `exp` 校正**：
      回归：早期按毫秒处理（`*time.Millisecond`），把 7 天压成 604.8 秒 → 落盘 `expiresAt` 比真实寿命少 ~7 天
      → `NeedsRefresh(10min)` 几乎恒为真 → **每次请求都刷 token**，与千问办公 App 高频互踩，
      直至 refresh token 被作废、账号被禁用（面板报 `qwenwork: token refresh failed`）。
      证据链：上游 `expires_in=604800` 按秒算 = access token JWT 的 `iat→exp`（整 7 天，吻合）；
      按毫秒算 = 文件里的值（吻合）。且同仓 workbuddy/trae/workbuddyai 的 auth 文件 `expiresAt` 与 JWT `exp`
      逐秒一致，**仅 qwenwork 偏离 6.99 天**。修复两处：①refresh 按秒解释，优先取绝对字段 `expires_at`，
      两字段都缺失时回退 84h（JWT 实测 7 天的一半）；②`LoadQwenWorkDir` 调 `Auth.AdoptJWTExpiry()`，
      用上游签名的 JWT `exp` 原地校正历史脏值（仅内存、只增不减、非 JWT 不动）——**没有这一步，存量账号即使修了代码仍是坏的**。
      **注意：同族 dt-/drt- 渠道（qoder/qodercn/qodercom）token 为不透明串、无 JWT 可交叉验证，
      其 `// ms` 标注未被本次改动触及**（无证据不做改动）。

25. **导入型渠道（`raccoon` / `loomy` / `monkeycode`）不做登录编排**：凭据由面板「从本机客户端导入」产生
    （`internal/app/import_local.go`，写 `auths/<渠道>-<uid>.json`，路径**自适应探测**多候选目录）。
    由此产生四条硬约束：
    - **未知模型必须本地拒绝**：三个上游对未知模型名都会**静默回落到默认模型并返回 200**
      （raccoon/loomy 阶段 C 实测；monkeycode 由 `staticSet` 白名单拦，见其 `client.go` 注释），
      渠道层不校验就会让用户以为在用 A 模型、实际消耗 B 模型的额度。monkeycode 另在
      `Classify` 里把上游回的 `model_not_found` 归 `ErrBadParams`（请求级、不罚号）。
      守门测试：raccoon/loomy 的 `TestChatStreamRejectsUnknownModel`、
      monkeycode 的 `TestUnknownModelRejectedLocally`。
    - **聚合必须同时支持 JSON 与 SSE**：上游对非流式请求可能直接返回 JSON（Loomy 实测），
      只按 SSE 解析会得到「content 空 + created 用 time.Now() 兜底」的假响应。
    - **小浣熊的 refresh_token 单次消费、会话可多份并存**（2026-09-25 实测更正）：
      浏览器授权登录新建独立会话（新 `sid`），可与官方客户端并存；
      导入器复制客户端同一份 token（同一 `sid`），两边会抢着消费同一个 refresh_token，
      后刷者报 `refresh_conflict`（实测 400）。判据是 `sid`，详见
      `本地 docs/loomy-raccoon接入记录.md` §6.3。
    - **`monkeycode` 有两层凭据、生命周期差 5 倍**：`oma_`/`omas_`（agent，无刷新端点）
      是长期凭据，控制台会话（积分用）只活 ≈6 天，但可由**百智云会话**（≈29 天，同一 bundle
      目录的 `baizhi-cookies.json`）经 OAuth 派生续期 —— 故导入器**必须两支 cookie 都取**，
      缺 baizhiCookie 时积分到期就只剩「重新导入」一条路。凭据链与实测见
      `本地 docs/MonkeyCode渠道接入评估.md` §3.14 ③④。
26. **`loomy` 的档位面独占 `RealmLoomy`**（`RealmForKind` / `SupportsEffortKind` 已同时登记）：
    档位来自上游 `/models` 的 `reasoning_efforts`（无静态兜底）；投影时补 `enable_thinking` 与
    `chat_template_kwargs.enable_thinking` 三件套。实测该系列模型**无法完全关闭思考**
    （最低档仍产生约 59 字），故 `ReasoningCanDisable` 保持 false。
27. **上游错误分类里「请求级错误」不得罚号**（`internal/upstream/client.go` + `internal/server/handler.go`）：
    内容拦截 / 上下文超限（11115）/ 图片格式无效（11135）/ 出站 body 畸形（11101）都是**请求内容**的问题 ——
    同一 body 换任何账号结果都一样。这几类必须在 `Classify` 里判成 `ErrContentBlocked` / `ErrPromptTooLong` /
    `ErrImageInvalid` / `ErrBadParams`，由 `handler.chatCompletions` 走「不冷却、不计数、原文透传」分支；
    一旦落到 `ErrClient`，`default:` 分支的 `NoteError` 就会把健康账号喂到冷却。
    - **业务码一律走 `provider.CodeMarker`，不用字面量 marker**：上游信封形态不统一
      （`"code":11135` / `{"code": 11135}` / `"code":"11135"`），字面量只覆盖紧凑形态，漏判即退化成 `ErrClient`。
      `codeMarker` 同时排除 `"code":111350` 这类前缀误命中。
    - **这些判定必须排在 `status == 404` / `status >= 500` 之前**：404 上的 11115 若落到 `ErrNotFound` 会软冷却账号
      （上下文超限与账号健康无关）。
    - **图片解析失败的信封 code 也是 11101**（`Parse message failed: invalid image_url content ...`）：
      图片判定必须先于 11101 判定，否则「图片有问题」被误归「body 畸形」。
    - 出站 `image_url` 形状在 `payload.normalizeImageURL` 归一（上游只认 OpenAI 对象形态，
      字符串形态会 400 code=11101）；只转形状，空串/缺失/类型不对一律不动，让上游报真实错误。
    守门测试：`TestUpstreamRequestLevelErrorsDoNotPunishAccount`（含「未知 4xx 仍罚号」对照，防止断言空转）、
    `TestCodeMarkerTolerance`、`TestNormalizeImageURL`。
    - **业务码判定全渠道共用 `provider.CodeMarker`**（`internal/provider/codemarker.go`，upstream 包内别名
      `codeMarker`）：upstream / qoder / qodercn / qodercom / workbuddyai 的 Classify 一律走它，禁止裸
      `Contains(lower,"11115")`——request_id 等任意含这五个数字的文本会误命中，把该罚号的错误透传出去。
      超限判定在各渠道 Classify 里同样必须排在 404 兜底之前（qoder 系 4 渠道守门：
      `TestClassifyPromptTooLong11115`）。qwenwork / traework 不认 11115 码（其上游无该信封，
      只认文案形态），维持现状。
28. **token 刷新必须按账号单飞**（`internal/provider/refresh.go`）：渠道的 `RefreshToken` 只在写字段时持
    `auth.mu`，HTTP 调用在锁外 —— 该锁只防数据竞争，**拦不住「两次刷新都真的打上游」**。同一个
    refresh_token 被并发消费必然一方报 `refresh_conflict`（raccoon `200822` / qwenwork `invalid_grant`），
    失败方还会被 `Pool.Cooldown` 推进冷却，对外表现为「一并发就 503」。所有刷新调用点
    （app.go 3 处 / scheduler.go 2 处 / handler.go 1 处）一律走 `provider.RefreshOnce(a, fn)`，
    **禁止直接调 `Upstream.RefreshToken`**（`internal/login_trae` 的登录内联刷新除外）。
    - fn 内固定做「重检 `NeedsRefresh` → 刷新 → `SaveAtomic`」：等待者醒来重检发现已被刷过就直接返回，
      同时封住「上一轮 flight 刚删除」的窗口。唯一例外是 401 自愈（session 已作废但 expiresAt 未到），
      那类 fn 无条件刷新。
    - 单飞键取 **auth 文件路径**（回落 UID）：跨渠道天然隔离，且「同账号在两个目录各有一份文件」
      本就代表两个独立会话，应各自单飞。
    - 实现在 map + channel 上，**不引入 `golang.org/x/sync`**。
    守门测试：`internal/provider/refresh_test.go`（含 panic 唤醒等待者、无粘性缓存两例）。
29. **匿名渠道的虚拟账号不得进入任何「能把它弄没」的路径**：`reloadAccounts` 不纳入（`SyncToDir` 会剔除），
    启动时 `SetDisabled(uid,false)` 兜底自愈；`RemoveAccount`/`DisableAccount` 对 `provider.Oczen` 硬拒（后端拒 + 前端无入口）。
    其 `Auth.ExpiresAt` 必须为远期值（不得为 0），否则 `NeedsRefresh` 恒真 → 反复 `RefreshToken` + 冷却。
30. **单账号渠道（`Runtime.SingleAccount`）不得施加任何账号级惩罚**：唯一账号且不可重登 ⇒ 任何惩罚
    （冷却/计数/禁用）都等于整条渠道下线。故 `Runtime.SingleAccount=true` 的渠道（当前 oczen）在 handler 里
    对传输层错误与 `>=400` 一律**原文透传、不冷却不计数不禁用**；启动时另调 `Pool.ClearPenalty(uid)`
    清除旧版遗留的冷却。`Classify` 仍做语义分类（供日志），但不再用于决定惩罚。
    - **2026-09-24 修订（原「错误分类只能依赖 429」已作废）**：原「429 短冷却是唯一需要的背压」经实测证伪 ——
      单账号无号可轮换，冷却后后续请求在挑号阶段被挡成 `503 no_healthy_account`，反而不如透传 429
      让客户端按 `Retry-After` 自行退避；同理**传输层错误也不再累计 `errCount`**（默认 3 次网络抖动即冷却唯一账号）。
      两者由 `server.Runtime.SingleAccount` 统一豁免（结构属性，不硬编码渠道名）。
    - **严禁**把 401/403 归为 `ErrSessionDead`（会 `pool.Disable` 永久禁用且无法人工恢复）。
31. **面板上「客户端要填的渠道前缀」只有一处出口（`web/app.js` 的 `chPrefix()` / `chPrefixChip()`）**：
    客户端模型名必须带渠道前缀（`provider.Kind` 即前缀，如小浣熊填 `raccoon/<模型名>`，无前缀且无映射时网关报
    `model "xxx" needs an explicit channel prefix`），面板在**账号卡片**（badge 旁）、**费率表分组表头**、
    **帮助弹层前缀清单**三处展示，全部走同一函数；费率表的模型 tooltip 首行给完整调用 ID（可直接照抄）。
    帮助清单与路由报错文案（`server.prefixHint`）**同源**：都取 `cfg.Runtimes` 的 key（前端即 `state.compat.channels`），
    故渠道路由清单增删会自动跟随。
    - 渠道**显示名**同样只有 `CH_LABEL` 一处数据源，但取用有两档语义，别混：
      `chLabel(k)`（固定入口：账号卡片 / 费率表 / 代理行，未收录时兜底 `"WorkBuddy"`，保证有名字可看）
      与 `chNameOf(k)`（**数据驱动**入口：用量面板 / 流水 / 图表，未收录时回退原 id —— 这些值来自历史流水，
      可能是新增或已删渠道，回退 id 才有诊断价值）。原有的 `CH_NAMES`（用量面板）与
      `PROXY_HINT`（代理行）是另外两份副本，新增渠道只改一处必然漏 ——
      `raccoon`/`loomy`/`monkeycode`/`traecode` 曾在用量面板显示成英文 id（2026-09-24 已删除两份副本）。
    - **新增渠道只需在 `CH_LABEL` / `CH_CLASS` 各加一行**；前缀、帮助清单、tooltip 均自动生成，不要再手写清单
      （帮助文案曾手写 4 个渠道且含已下线的 `qoder`）。
32. **流式请求不得用 `http.Client.Timeout`；流内故障必须补 error 帧 + `[DONE]`**（2026-09-24 修复 loomy「突然无响应」）：
    - **`http.Client.Timeout` 是整请求上限**（计时器在 `Do()` 返回后继续跑，直到 body 读完），
      而 SSE 整个生成期都在读 body ⇒ 「慢但一直在出字」的请求会被**从流中间掐断**。
      实测 loomy 两次中断都恰好 `120.00s`（= `config.upstream.timeout_seconds`），
      客户端表现为「突然无响应」，且 `usage` 末帧从未到达（ledger 里该请求记成 `src=none / pt=0`）。
      故流式必须走**无总超时**的独立 client（`StreamHTTP`，照 `traework` 既有范式），
      由 Transport 的 `ResponseHeaderTimeout`（连头都等不到时兜底）+
      `provider.IdleReader`（`ErrIdleTimeout`，默认 90s，可配 `upstream.stream_idle_seconds`，
      下限 10s）两级承担。非流式调用（目录/积分/刷新）**保留**总超时 —— 短请求的总超时是恰当的。
      覆盖全部对话渠道：workbuddy / workbuddyai / traework / traecode / qoder 三兄弟 / qwenwork /
      raccoon / loomy / monkeycode / oczen（main 各注入 `IdleTimeout = cfg.StreamIdleDur`，
      `proxyTargets()` 均登记 `StreamHTTP`）。
    - ⚠️ **`StreamHTTP` 无总超时后必须有 `ResponseHeaderTimeout`**：两者都去掉就是无限挂住。
      顺带补齐了 loomy/raccoon 此前缺失的 Transport（原先未设 Transport，实际共用
      `http.DefaultTransport`：h2 开启、无 `ResponseHeaderTimeout`）。
    - **流中断（空闲超时 / 连接被切断）必须补一帧 error + `data: [DONE]`**：响应头早已按 200 发出，
      流内帧是唯一能表达故障的通道。只 `return err` = 客户端收到无收尾的截断流（只能一直等）；
      只补 `[DONE]` 则把故障伪装成正常结束。统一走 `provider.WriteTruncationFrames`，
      错误码由 `TruncationErrorCode` 归一（空闲超时 `upstream_timeout` / 其它 `upstream_stream_error`）。
      覆盖：`internal/loomy/sse.go`、`internal/raccoon/sse.go`、`internal/upstream/sse.go`
      （workbuddy/workbuddyai 与 monkeycode/oczen 共用）、`internal/traework/solosse.go`、
      `internal/{qoder,qodercn,qodercom,qwenwork}/sse.go`。
      守门测试：`TestStreamTruncationEmitsFrames`（各流式包各一）+ `TestStreamReadErrorEmitsTruncationFrames`。
    - **代理渠道清单有三处，必须同步**：`config.go` 的 `Proxies` 注释、`main.go` 的 `proxyTargets()`、
      `web/app.js` 的 `PROXY_CHANNELS`。`App.SetProxies` 是**整份替换** `cfg.Proxies`，
      面板漏登记的渠道在保存时会被**静默清空**（手改 config.json 的代理会丢）——
      `raccoon`/`loomy`/`monkeycode`/`traecode` 曾同时漏在热更新名单与面板清单里（2026-09-24 已补）。
      `main.go` 的启动与热更新现已共用同一个 `proxyTargets()`，不再有两份名单。

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
> 标准流程：`go build ./... && go vet ./... && go test ./...` 全绿后再构建。
> 注意：`go test ./...` **不编译** `//go:build live` 的探针文件，改动其调用的
> 函数签名后探针会静默腐烂（2026-09-26 实测：`qoder` / `qodercn` / `qodercom`
> 三个探针都因 `buildAgentBody` 增参而编译不过，长期无人察觉）。故标准流程为：
> `go build ./... && go vet ./... && go test ./... && go vet -tags live ./...`。

```bash
# Windows（本机直接构建，或 WSL 交叉编译）
# exe 文件图标来自 cmd/wild-work/rsrc_windows_amd64.syso（已入库，go build 自动链接）。
# 换 build/windows/icon.ico 后必须重新生成并提交，否则本地 exe 还是旧图标：
#   go run github.com/akavel/rsrc@latest -ico build/windows/icon.ico -o cmd/wild-work/rsrc_windows_amd64.syso
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-H windowsgui" -o dist/wild-work.exe ./cmd/wild-work

# macOS（需 macOS 真机或 CI，cgo 必需）
GOOS=darwin GOARCH=arm64 go build -o dist/wild-work-darwin ./cmd/wild-work

# Linux 无头
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dist/wild-work-linux ./cmd/wild-work
```

构建后核对产物是不是刚编的（防止拿到旧文件）：

```bash
# 首选：看嵌入的 VCS 信息（go 1.18+ 默认写入），比字节计数可靠
go version -m dist/wild-work.exe | grep -E "vcs.revision|vcs.modified"
# vcs.revision 应等于 git rev-parse HEAD；带 vcs.modified=true 说明构建时有未提交改动

# 次选：版本号字节计数。旧版本号可能巧合出现在编译器生成的闭包符号名里
# （如 crypto/tls 的 `.1.2.2.1`），所以「旧版本号出现 0 次」不是必须条件，
# 只有「新版本号完全没出现」才说明构建没生效
python -c "b=open('dist/wild-work.exe','rb').read(); print('new:',b.count(b'2.3.1'))"
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

## 9. 文档索引

- [README.md](README.md) — 用户文档
- [LICENSE](LICENSE) — MIT 许可
- [NOTICE](NOTICE) — 第三方组件版权与许可声明
- [DEVELOPMENT.md](DEVELOPMENT.md) — 开发者文档（面向 AI Agent）
- [AGENTS.md](AGENTS.md) — 本文件：决议项（R1–R47）、架构选型、不变量
- [docs/三接口兼容改造备忘.md](docs/三接口兼容改造备忘.md) — 三接口（Chat/Responses/Anthropic）兼容层架构决策、实施记录、验证清单、已知限制
- [docs/三端点四能力矩阵.md](docs/三端点四能力矩阵.md) — 三渠道横向能力矩阵（模型/档位/签到/积分）
- [docs/qoder2api端点覆盖度对比.md](docs/qoder2api端点覆盖度对比.md) — qoder2api 端点覆盖度对比
- [docs/qoderCN渠道接入备忘.md](docs/qoderCN渠道接入备忘.md) — QoderCN 协议取证与实测矩阵
- [docs/用量积分流水记账备忘.md](docs/用量积分流水记账备忘.md) — 双流水统计（token/积分）架构、差分算法、实测验证、已知限制（R17）
- [docs/用量积分流水记账备忘.md](docs/用量积分流水记账备忘.md) — 双流水统计（token/积分）架构、差分算法、实测验证、已知限制（R17）

以下备忘被 R16 / 不变量 29/30 等决议引用，但**尚未入库**（`.gitignore` 白名单未反选，仅本地可见）：
`docs/opencodezen渠道接入备忘.md`、`docs/qwenwork渠道接入备忘.md`、`docs/qoderCN渠道接入备忘.md`、
`docs/qoderCOM渠道抓包分析与接入计划.md`、`docs/智谱清言渠道接入备忘.md`（R22–R38 引用，随 PR #46 新增，
作者未提交入库）。（部分可能已丢失，仅存在于历史会话中）。
如需转为本仓可查，在 `.gitignore` 补 `!docs/<文件名>` 并在上方列表添链接。
**例外**：glm 渠道的协议依据（R22–R38 引用）**不在本地**——作者已将其开源为独立仓库
[glm2api](https://github.com/ttales430/glm2api)（签名算法/认证流程/SSE 语义/积分机制/智能体清单），
引用一律指向该仓库，不要找本地文件。

> **docs/ 采用白名单制**：`.gitignore` 中 `docs/*` 默认忽略全部文档，仅 `!docs/<文件名>` 显式反选的才入库。
> **入库判据（2026-09-25 收紧）**：只放「接口结构与端点清单」类文档。含**凭据原值/客户端内嵌常量、
> 可复现的攻击面细节（协议劫持/注册表改写/回调截获/端点不校验身份）、抓包产物原文、
> 真实账号标识（uid/昵称/邮箱/机器指纹/内网 IP/本机用户名）、客户端插桩或中间人取数方法**
> 的文档一律**只保留本地、不入库**（判据同步写在 `.gitignore` 注释里）。
>
> 以下备忘**仅本地可见**（未反选入库），代码注释中引用它们时按「见本地 docs/xxx.md」理解：
> `TraeWork-api.md`、`loomy-raccoon接入记录.md`、`loomy渠道接入备忘.md`、`raccoon渠道接入备忘.md`、
> `upstream-reverse-engineering.md`、`三接口兼容改造备忘.md`、`千问办公QwenWork逆向对比备忘.md`、
> `qoder签到补齐改造方案.md`、`qoderCOM渠道抓包分析与接入计划.md`、`qoderCN-IDE抓包分析备忘.md`、
> `MonkeyCode渠道接入评估.md`。
>
> **脱敏守门（提交前必跑）**：历史上有过三次「改完又回归」的脱敏遗漏，故新增/修改文档后跑一次
> （只查**入库**文档，本地未入库的文档命中属正常）：
> ```bash
> git ls-files docs/ -z | xargs -0 grep -nE "uid=[0-9]|C:\\\\Users\\\\[^\\\\]+|machine_id=[0-9a-f]{8}-|dt-[A-Za-z0-9]{20,}|drt-[A-Za-z0-9]{20,}"
> ```
> 命中即视为泄露，先脱敏再入库（真实 uid / 本机用户名 / 机器指纹 / 凭据前缀均不得出现）。
