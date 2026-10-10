# wild-work 发布说明（RELEASES）

本文件汇总各版本发布说明，按版本**从旧到新**排列（v2.0.1 → 最新）。
发布新版本时，将新版本说明 **append 到本文件末尾**（保持从旧到新，日志自然增长）。

---

# wild-work v2.0.1

- Fix https://github.com/rockswang/wild-work/issues/9：TraeWork 模型列表过滤  第三方代理模型
- Web UI 顶部交互优化：点击文本框复制值，末尾新增 ✎ 修改图标

---

# wild-work v2.1.0

## 新增 WorkBuddy 国际版渠道

- 新增 **WorkBuddy 国际版**（`www.workbuddy.ai`）渠道，与国内版完全独立，可同时使用
- **免费使用 `deepseek-v4.1-flash`**（限时两周），另有 `hy3`、`hy4-preview` 等免费模型
- 无需科学上网即可体验 **GPT 系列模型**，含 **GPT-6**（`gpt-6-astra`）等
- **自动领日活奖励**：定时自动对话保活并领取每日活跃奖励，无需手动签到

## 优化模型列表和费率显示

- 模型列表与费率为同一份数据，逐行对应，一目了然
- 免费模型清晰高亮，上游未提供定价的模型明确标注
- 悬停模型名可查看上下文窗口等详情

---

> 提示：`deepseek-v4.1-flash` 为官方限时免费活动，到期后可能恢复计费。

---

# wild-work v2.2.0 — 稳健性大版

> 吸收上游 [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) 近期 100+ 提交中的关键修复，
> 聚焦「Codex / Claude Code 能跑通」「错误分类不误伤好号」「Token 竞态与连接半死不传染」三类问题。

## 新增功能

### Claude Code / Codex 请求体指纹脱敏 (`internal/sanitize/`)
- 出站前自动清除 Claude Code（`You are Claude Code...`、`x-anthropic-billing-header`、`Main branch (...)`、`github.com/anthropics/issues`、`11128` 裸码）与 Codex CLI（`Codex CLI, a terminal-based coding assistant.`）的模板指纹
- 覆盖 `messages[].content`（string / 多模态 array 的 `text` part）、`reasoning_content`、`tool_calls[].function.arguments` 三处
- 预检不命中时零分配原样通过，不影响正常请求；默认开启，未来可通过配置关闭（escape hatch）

### 模型多模态能力透传 (`/v1/models`)
- 上游目录的 `supportsImages`/`supportsReasoning`/`supportsToolCall` → `provider.ModelInfo` 能力字段 → `/v1/models` 输出 `architecture.input_modalities`（OpenRouter / llama.cpp 通行形状，`["text"]` 或 `["text","image"]`）
- WorkBuddy 国内版/国际版：解析上游目录接口的 `supportsImages` × `disabledMultimodal` 联合判定
- Qoder：`IsVL` → `SupportsImages`、`IsReasoning` → `SupportsReasoning`
- TraeWork：上游模型列表不返回能力字段，回退不声明
- 静态兜底表/extraModels：无上游数据，不声明任何能力；**原则是不自造数据、不入为纠正上游与实际不符的声明**
- Web UI 费率表：模型 ID 后增加 👁🧠🔧 能力图标 + tooltip；措辞用「上游未声明」而非「不支持」

### 错误分类精细化
- 新增 5 个 `ErrKind`：`ErrContentBlocked`（内容拦截，不罚号）、`ErrPromptTooLong`（上下文超限，请求级错误不轮转）、`ErrWafBlock`（WAF 403，账号软冷却）、`ErrAccountFault`（11140/14017，冷却轮换）、`ErrModelBlocked`（11102，后续可做模型级负缓存）
- 三渠道 `Classify` 全部修复：**429 判定移到 hardRule 之前**（限流 body 带 "quota exceeded" 不再误判余额耗尽 12h）
- 新增非 429 状态码软限流词表（`rate limit` / `too many requests` / `usage limit` / `请求过于频繁`）
- 账号级故障词表（`request illegal` / `trial not activated`）

### 出站协议头补齐
- `X-CodeBuddy-Request: 1` —— 官方客户端风控闸门头，所有 API 请求生效
- `Accept-Language` —— CN → `zh-CN`，global → `en-US`
- `X-Machine-ID` / `X-Session-ID` —— 按 `sha256("wb2a:"+purpose+":"+uid)` 稳定派生，跨重启固定
- `X-Auth-Refresh-Source: plugin` —— 对齐官方客户端 refresh 标识

### 请求体改写增强
- `max_completion_tokens → max_tokens` 翻译 —— 新客户端（o-series / DeepSeek Harness）只发别名时不再被上游忽略而输出截断
- `stream_options: {include_usage: true}` 缺省注入 —— 保障上游末帧返回 `usage`，客户端用量统计不再缺失

## Bug 修复

### Token 并发读写竞态
- `auth.Auth` 新增 `AccessTokenValue()` / `RefreshTokenValue()` 锁内快照
- 三渠道出站请求头全部改为锁快照读取，消除与 keepalive 刷新写回的 `-race` 数据竞争

### `Aggregate` SSE 聚合修复（5 项）
- 空 content 帧不再置 latch（阻止后续 message 帧合并）
- message 回退分支补 gotAnyContent 守卫（避免正文重复拼接）
- `tool_calls` 缺 index 时按「id 优先 / lastIdx 兜底 / 跳号分配」分派（不再一律归 0 导致数据污染）
- EOF / `length` 截断时丢弃残缺 `tool_calls.arguments`（不把非法 JSON 交给客户端）
- `usage.total_tokens` 缺时用 `prompt+completion` 合成补齐

### `Stream` 空流检测
- 上游返回 200 但 0 有效帧时不再合成空 content 假成功，改为报 `upstream_parse` 错误

### Transport 连接层加固
- **真禁 h2**：`TLSNextProto: make(map...)`（`ForceAttemptHTTP2=false` 实测无效）
- Dial 超时 10s + keepalive 15s + TLS 握手超时 10s + ResponseHeaderTimeout 60s
- IdleConnTimeout 从 90s 收到 30s
- WorkBuddy 国内版与国际版统一使用同一加固 Transport

### TraeWork 流式/聚合 model 回填
- `Aggregate` / `Stream` 拆分出 `WithModel` 变体，上游 SOLO 事件不带的模型名由调用方回填（对齐 R14）
- qoder 多模态能力也一并跟上（`IsVL → SupportsImages`、`IsReasoning → SupportsReasoning`）

### `truncate` 防 panic 守卫
- 补 `n<=0` 守卫（负数不再 panic）

## 交互体验

### Web UI
- 费率表加 👁🧠🔧 能力图标（图像输入 / 思考模式 / 工具调用）
- API 配置弹窗增加 API-Key 可见性切换按钮
- 新增 FAQ 常见问题说明
- 帮助面板新增微信群二维码

## 文档更新
- AGENTS.md：补 v2.2.0 渊源 + Classify 新不变量 + sanitize 逃生门
- DEVELOPMENT.md：补 `internal/sanitize/` 包说明 + Transport/Classify 新不变量
- README.md：功能描述补脱敏/错误分类/协议头仿真 + 版本号更新
- 新增完整 FAQ

---

**全量校验**：`go build ./... && go vet ./... && go test ./...` 全绿。

**构建**：`GOOS=windows CGO_ENABLED=0 go build -ldflags "-H windowsgui" -o dist/wild-work.exe ./cmd/wild-work`

**该版本无配置格式变更**：config.json 与 state-*.json 兼容 v2.0.x / v2.1.x。
---

# wild-work v2.2.1 — 积分显示修正：可用/不可用拆分 + 有效期 + 明细翻页 + 401 自愈

> 本版聚焦「积分到底有多少能用」这一件事，修正 TraeWork 可用余额虚高与面板数字误导，
> 解决「本地 token 看似有效但上游已拒绝」导致的积分恒 0，
> 并补齐面板的映射编辑与浮窗翻页体验。

## 修复

### API 配置对话框「映射表-编辑」无效 + 保存链路损坏（重要）

**现象**：点「编辑」无任何反应；就算改了映射/渠道/密钥，点保存也永远失败。

**根因**（三层叠加）：

1. `btnCompatMap` 从未绑定 onclick；
2. `serveLocked` 先 `net.Listen` 新地址再关闭旧 listener——同端口保存时撞自身
   （`Only one usage of each socket address`）→ `listen` 恒 400，
   **映射/渠道/密钥修改从未真正保存成功过**；
3. 映射编辑用的是 `prompt()`，语法错误只能弹窗循环。

**修复**：

- 同地址直接复用现有 listener；切地址先关闭旧服务再绑定（`App.listenAddr` 跟踪）；
- 对话框内嵌映射编辑器，就地校验（缺 `=`、目标非 `channel/model`、未知渠道、重复键），
  错误红字显示在编辑器下方，不再弹窗循环；
- **缺省建议按钮**：按已接入渠道给出可点选的映射起点（如
  `Claude Code → workbuddy: claude-* = workbuddy/glm-5.2`），
  一键插入并去重；未绑定渠道不显示建议，避免误导；
- **compat 热更新**：原实现路由表只在启动时加载一次，运行中改映射要重启才生效
  （这正是「有的机器没配映射也能用」的原因——config.json 启动时已带映射）。
  现面板保存后经 `gateway.SetCompat` 立即生效；
- 客户端请求路由到无账号渠道时，错误信息区分「未绑定账号」与「全部冷却/禁用」，
  引导用户去面板添加账号或换已接入渠道。

**映射写法**（已验证 CC/Codex 可正常使用）：

```
claude-* = workbuddy/glm-5.2          # CC 主模型
claude-sonnet-* = workbuddy/kimi-k2.7 # 更长前缀优先，盖过 claude-*
codex-* = traework/DeepSeek-V4-Pro    # Codex
gpt-5* = traework/glm-5.2            # gpt-5 / gpt-5.1 / gpt-5-codex 均命中
```

注意：未命中映射的裸名走 `default_channel` + 原始模型名（如 `workbuddy/gpt-4.1`），
上游多半无此模型会报错，需自行补映射。

### 本地 token 看似有效、上游已拒绝 → 永久卡在 401（重要）

**现象**：账号积分恒为 0，hover 不弹明细（接口 400）。

**根因**：刷新时上游会**作废旧 access token**（refresh token 同时轮换）。若新 token 未落盘，
或同一账号在另一实例/客户端上被刷新过，本地文件里就是**已被作废、但 `expiresAt` 仍在未来**的 token：

```
本地 expiresAt = 未来某时（看起来还有很多天）
NeedsRefresh(10min) → false  ← 永远不会去刷新
上游实际返回     → 401      ← 但 token 早已被作废
```

于是 `NeedsRefresh` 永远为假、永不重试，积分/明细永久为空。

**修复**：不再只信任本地过期时间，对 401 本身做一次「refresh + 重试」，成功后写回磁盘。
覆盖五条路径：积分自动刷新、单账号刷新（`RefreshCredits`）、批量刷新（`RefreshAll`）、
明细查询（`ResourceDetail`）、费率拉取（`RefreshPricing`，
原先连 refresh 结果都没落盘，已一并补上）。

**验证**（把 `accessToken` 改坏、保留有效 `refreshToken` 后启动）：

```
session dead, refreshing platform=traework uid=1096660468371514
traework refresh success uid=1096660468371514 refresh_rotated=true expires_at=1790917166
→ 积分 2710/不可用 2600，明细 29 条正常，新 token 已写回磁盘
```

### TraeWork 可消耗余额虚高（重要）

早期实现用 `group_type != 1` 判定可消耗额度，把 `available_endpoint=1`
（官方客户端专用池）的「用户福利」「签到奖励」也计入了可消耗余额。

**实测证据**（2026-09-18，三账号各做一次 `glm-5.2` 对话后对比用量）：

```
账号 3066985700146732（对话前 → 对话后）
  gt=1 ep=0 每日签到   used 420.7852 → 422.1424  (+1.3568)  ← 本工具只扣这里
  gt=1 ep=1 每日签到   used   0.0000 →   0.0000  (不动)
  gt=4 ep=1 用户福利   used   0.0000 →   0.0000  (不动)
```

判据已修正为 **`available_endpoint == 0`**。影响：

- `pool.Pick()` 不再按虚高余额选号（原先可能选中一个实际可用余额已耗尽的账号）
- 签到后解冻判定（`ReenableIfCredits`）同样只看可用池，不再被专用池额度误放行

### 总分与分项「对不上」的澄清

不是计算错误。分项合计与 `usage_summary` 严格自洽：

| 账号 | Σlimit | total_amount | Σused | consumed_amount |
|------|--------|--------------|-------|-----------------|
| 1096660468371514 | 5350 | 5350 | 40.4864 | 40.49 |
| 1342951362670730 | 9750 | 9750 | 3675.0232 | 3675.02 |
| 3066985700146732 | 9150 | 9150 | 2920.7852 | 2920.79 |

`used` 的小数尾差来自上游 `consumed_amount` 只保留两位小数。真正的坑是
**`total_amount` 是含 ep=1 专用池的总量**，把它当可用余额就会得出「账号有 9150 积分」的错觉
（该账号实际可用仅 1828）。现已把两者分开显示。

### 到期时间

各渠道字段不同，统一解析为 `YYYY-MM-DD` 并按 UTC+8 墙钟处理：

| 渠道 | 字段 | 说明 |
|------|------|------|
| WorkBuddy / 国际版 | `CycleEndTime` | **上游从不下发 `PackageEndTime`**（旧判据恒 miss） |
| TraeWork | `expire_time` | Unix 秒 |
| Qoder | 无 | 不显示有效期列 |

上游未下发时该列整体隐藏，不会出现一列空白或用零值冒充「永不过期」。

## 改进

### 面板：积分数字拆成「可用 / 不可用」

- 账号卡片：`1828可用积分/4400不可用`（不可用额度降级为次要色），主界面直接可见，无需 hover
- 明细 tooltip 底部：`可用 1828　不可用 4400`，与卡片口径一致
- 明细表中不可用额度整行淡显 + 「不可用」角标

### 面板：明细分页与有效期列

- 每页 8 条，翻页器在浮窗**顶部居中**（`‹ 1 / 4 ›`），标题/条数分列两端；
  鼠标从卡片到达按钮路径最短，不跨出浮窗边界
- 浮窗宽度固定 380px，翻页时不再变形/宽度跳动；单元格超长省略号
- 翻页只重绘不重新请求（响应已缓存）
- 新增「有效期」列（仅当上游确实下发到期时间时出现）
- 明细缓存随 `loadState()` 失效，避免刷新后 tooltip 仍显示旧余额
- 限额为 0 的包（如 TraeWork 的免费 0 限额包）不再出现在明细里

### 积分自动刷新覆盖全部渠道

原来自动刷新只覆盖 WorkBuddy 国际版，其余渠道积分只随签到更新——
签到间隔过长或查询失败时，面板数字长期不更新。现四渠道统一纳入循环：

- 启动立即首刷一次，之后每 30 分钟；
- qoder / workbuddyai 无签到活动，不自动刷就会一直显示旧值或 0；
- traework / workbuddy(CN) 签到间隔过长，统一纳入才能及时自愈 401。

### 旧版 state 兼容：无需删除 data 目录

v2.2.0 的 `state-*.json` 无 `unusable` 字段，直接换二进制会让卡片只显示
「xxx可用积分」而无不可用拆分（旧数字残留）。现已：

- state 文件增加 `version` 字段（v2 = 可用/不可用拆分口径）；旧格式读入后标记余额口径不可信；
- 卡片对这类账号显示「待刷新」（带说明），不把旧值当真值；
- 配合全渠道首刷，启动几秒内自动变为真实拆分数字。

**升级时保留 `data/` 目录即可**，无需删除或手工迁移。

## 内部改动

- `provider.ResourceItem` 增加 `ExpireAt` / `Usable`；新增 `provider.Summarize()` 统一汇总小计
- `pool.Status` / `AccountView` 增加 `UnusableCredits` / `CreditsStale`，随 `state-*.json` 持久化
- `pool.ReenableIfCredits(uid, remain, unusable)` 签名变更；`SetCredits` 并入 `SetCreditDetail`
- 积分刷新路径改走 `UserResourceDetail` 单次请求，同时拿到可用余额与小计（原先只调 `UserResource`）
- 三渠道 `UserResourceDetail` 增加 `Usable: true`（国内版/国际版/Qoder 无端点分区）
- gateway 增加 `SetCompat`（路由表热更新，带 RWMutex）与 `App.SetCompatSyncer` 注入点
- CI：tag 发版的 release note 改用仓库内 `RELEASE-<tag>.md`（缺失时回退自动生成）

## 升级说明

- **保留 `data/` 目录直接替换二进制即可**：旧 `state-*.json` 缺 `unusable` 字段时,
  面板会先显示「待刷新」，启动后自动刷新即变为真实拆分数字，无需手工迁移或删除目录；
- 若某账号的 refresh token 本身已失效（日志出现 `refresh token is invalid`），
  该账号需重新登录——这是唯一无法自愈的情况。

---

# wild-work v2.2.2 — 会话自愈安全网 + 国际版登录适配 + macOS .app 打包

> 本版重点解决两类问题：**坏会话把整条对话搞死**（客户端工具执行失败后，
> 每条消息都被上游 400 拒绝）与 **region=global 实例的登录/费率接口适配**。
> 另外带来 macOS 可双击的 .app 打包形态。

## 新增

### 会话自愈：孤儿 tool_call / tool 结果对称裁剪（重要）

**现象**：客户端（Codex / Claude Code 等）工具执行失败时，会把 `tool_calls`
持久化进历史却写不回对应结果。此后这条会话的**每一条**新消息都会被上游以
HTTP 400（`11148 tool_call_sequence_broken`）拒绝——对话报废，只能新建会话。

**修复**：请求送出前做双侧对称安全网处理：

- `repackToolResultBlocks`：把插在 tool 结果中间的非 tool 消息（如 Codex 的
  `image_resize_notice`）挪到结果块之后，修复「被判定断裂」的假阳性；
- `cleanupOrphanToolCalls`：按 id 双侧对称裁剪——无法配对的 `tool_calls` 与
  无法配对的 tool 结果一并剔除。旧的「整批齐才保留」策略会造出
  「无 tool_calls 的 assistant + 孤儿 tool」半截配对，照样 400，已弃用；
- 带 13 个回归用例（含真实会话卡死形态）。

**已知限制**：此安全网目前挂在 workbuddy(CN) 渠道的预处理管线；
workbuddyai / traework 渠道有各自的预处理入口，尚未接入，后续评估下沉共用层。

### normalizeRoles 兼容 role 变体

`developer` 角色归一为 `system` 的判定从精确匹配放宽为忽略大小写/首尾空白
（`"Developer"`、`" developer "` 等变体不再漏网，漏网会命中上游 role
白名单校验 400）。

### 面板登录支持国际版 WorkBuddy 账号（yanghuan，PR #20）

`region=global` 的实例此前点「+ WorkBuddy」仍打开国内版登录页，保存的账号
domain 为 CN，不会被国际版实例加载。现登录流程按 `region` 配置选择端点：

- 新增 `login.Endpoints` / `EndpointsForRegion`，CN 与 global 各自的
  API 基址与 Origin/Referer；
- `Start` / `Poll` / `ResolveAuthURL` 透传端点；
- 国际版 token 响应缺 `domain` 字段时按区域兜底，避免账号被误判为 CN。

### 国际版模型/费率接口路径适配（yanghuan，PR #20）

国际版网关下 `GET /console/enterprises/personal/models` 返回 500，正确路径为
`/v2/enterprises/personal/models`（实测 200，含 credits 定价）。现按账号
region 自动选择路径，CN 保持原路径不变；同时修正 `FetchModels`（动态模型
列表）与 `FetchModelPricing`（费率）两条链路。

### macOS：可双击的 .app 打包（geekxi，PR #25）

- 新增 `build/macos/build-app.sh`：把 `dist/wild-work-darwin-<arch>` 打包成
  `WildWork.app` 并生成 `.dmg`，图标由仓库内 `build/appicon.png` 派生，
  全程 ad-hoc 签名；
- 修复菜单栏（托盘）无图标问题；
- `.app` 形态下数据目录自动改用 `~/Library/Application Support/WildWork`
  （bundle 内容对普通用户只读）；`WILDWORK_HOME` 显式指定时优先。

## 内部改动

- 出站请求体脱敏（剥离上游内容审核黑名单指纹）与 SSE 出站逐帧规范化实现
  从上游参考项目 ref/workbuddy2api 整体拉齐（PR #23 / #24），
  本仓库与参考实现保持同口径，便于后续同步上游修复；
- `max_completion_tokens → max_tokens` 翻译规则注释拉齐（行为不变：
  显式 `max_tokens` 优先、非正数值不翻译、翻译后删别名）；
- 请求 payload 预处理管线顺序：stream 强制 → tokens 别名翻译 →
  stream_options 补全 → tool_choice 归一 → roles 归一 →
  tool 配对安全网 → 出站脱敏 → Marshal。

## 升级说明

- 直接替换二进制即可，`data/` 目录无需任何处理（v2.2.1 的 state 格式向后兼容）；
- macOS 用户可改用 `.app` 形态：首次启动数据目录为
  `~/Library/Application Support/WildWork`，如需沿用旧目录，
  设置环境变量 `WILDWORK_HOME` 指向原目录后启动。

---

# wild-work v2.3.1 — 千问办公渠道 + 临期积分优先路由

> 本版两大主题：**新增千问办公（qwenwork）渠道**（第五个渠道），以及
> **积分展示与选号算法改版**——临期积分醒目提示、路由优先消耗快过期的额度。

## 新增

### 千问办公（qwenwork）渠道

接入阿里千问办公（chat.qwen.ai）作为第五个渠道：

- 面板点「＋ 千问办公」登录：浏览器已登录则自动完成，否则扫码一次；
- 完整上游能力：对话（流式/聚合）、动态模型列表、模型费率、余额明细、token 按需刷新；
- 每日积分由服务端被动发放，无签到/保活动作（定时保活会与官方 App 互踩 token，已刻意关闭）；
- 青蓝色渠道主题；兼容层映射建议 `gpt-* = qwenwork/flash`、`claude-* = qwenwork/pro`。

### 界面：积分三段式展示

账号卡片积分改为「可用积分 2128 **(临期253)** (不可用2200)」：

- 可用积分黑色大字；临期红色小一号（24h 内到期，提示尽快消耗）；不可用灰色小一号；
- 临期/不可用仅在数值 > 0 时显示，不渲染无意义的 0。

### 路由：临期积分优先消耗

多账号选号从「剩余可用积分最高」升级为两级排序：
**先按临期额度（优先烧掉快过期的积分），同组内再按总可用余额**。

- 临期只统计可消耗额度，不会虚高选号；临期烧完自动回退总余额排序；
- 渠道不下发到期时间（如 Qoder）时临期恒为 0，自动退化为原有排序，
  五渠道共用同一算法，无需各自适配；
- 50 次粘性路由行为不变。

## 修复

- **Web UI 缓存**：静态资源改 `Cache-Control: no-cache`。此前浏览器可长期缓存
  旧版界面，替换二进制后面板不更新（需 Ctrl+F5）。**本版升级后普通刷新（F5）一次，
  之后版本升级界面自动跟随**；
- TraeWork 登录失败时回调页展示具体原因（此前失败也显示「登录已完成」）；
- `/api/state` 透传临期字段（内部中间层遗漏导致后端已统计但界面不显示）；
- **请求体超 8MiB 误报 invalid_model**（[#30](https://github.com/rockswang/wild-work/issues/30)）：
  长对话上下文累积超 8MiB 后，请求体被静默截断、JSON 解析失败被吞，
  误报成「模型名缺前缀」误导排查。现超限明确回 **413 request_too_large**，
  非法 JSON 回 400 invalid_request；`/v1/responses` 与 `/v1/messages` 同步修复。

## 升级说明

- 直接替换二进制即可；`data/state-*.json` 自动升级（新增临期字段），
  启动后自动刷新积分即显示临期拆分，无需手动处理；
- 使用千问办公渠道：面板登录即可，无需改配置。

---

# wild-work v2.4.1 — Qoder 体系重构：新增 QoderCN / QoderCOM 双渠道，旧 Qoder 界面下线

> 本版围绕 Qoder 渠道体系做了一次彻底重整：**新增 QoderCN（国内）与 QoderCOM（国际）两个独立渠道**，
> 并把上一代 Qoder（QoderWork，`qoder/*`）从界面上线撤下（代码保留、路由仍可用，平滑过渡）。
> 另外包含用量归属请求头注入、对话活跃上报、渠道更名等一批改进。
> 同时支持每日自动签到（**每日 +100 Credits**，实测已到账）。

## 新增

### QoderCN 渠道（`qodercn/*`）

基于 qoder2api（CN 分支）的抓包参数全新实现的独立渠道，与旧 Qoder 渠道并存：

- **OAuth 设备流登录**（PKCE+S256，client_id `e883ade2-...`），面板点「＋ QoderCN」弹出浏览器授权，回调自动绑号；
- **每日签到领 100 Credits**：campaigns 活动主路径（`act-YYYYMMDD-xxx` 动态匹配）+ daily-check-in 兜底双路径；
  幂等语义（409/CLAIMED/已领/无活动均视为成功），401 自动刷新重试；实测领取成功到账；
- **动态模型表**（含 `context_config` 上下文窗口解析、`is_vl`/`is_reasoning` 能力位），
  上次成功结果作进程内缓存，无静态兜底（模型名映射走本项目统一的兼容层机制）；
- 实测 `12345×54321` 算术题各模型推理全对（glm-5.3 思考 582 tokens）——思考链路工作正常；
- 紫色渠道主题；多轮对话、流式/聚合、余额分桶（套餐 + 赠送额度）全链路验证通过。

### QoderCOM 渠道（`qodercom/*`，国际版）

Qoder 国际版（`qoder.com` / `openapi.qoder.sh`），凭据与 CN 区完全隔离（已实测双向 401）：

- 复用 QoderCN 的协议实现（COSY 签名/QoderEncoding/嵌套 SSE 同源），**三域名分离**：
  业务 `openapi.qoder.sh`、推理 `api1.qoder.sh`、模型表 `api2.qoder.sh`；
- 签到仅 campaigns 活动路径（国际版无 legacy daily-check-in 系统，实测 404）；
- 橙金色渠道主题；GitHub SSO 账号实测：多轮对话、模型列表、签到幂等、余额分桶（300 套餐 + 100 赠送）全部通过。

### 请求头注入（WorkBuddyAI）

- 用量归属头：`X-Agent-Purpose: conversation`、`X-IDE-Name/Type: WorkBuddy`（对齐官方 IDE 形态）；
- 会话头族：`X-Conversation-Request-ID` / `X-Conversation-Message-ID` / `X-Request-ID` / `X-Root-Request-ID`（每请求生成）。

### 对话活跃上报（WorkBuddyAI）

新增 `ReportChatActivity`：一条 `chat_request_send` 事件（`/v2/report`）即可点亮
growth 连登与领养前置任务，无需真实对话，风控口径每号每天 1 次。

## 变更

### 渠道界面重整

- 「＋ WorkBuddy」→「＋ **WorkBuddyCN**」；「＋ WorkBuddy 国际版」→「＋ **WorkBuddyAI**」（显示名同步）；
- **旧 Qoder（`qoder/*`）从界面撤下**：删除添加入口，不再出现在渠道引导/兼容层预设中；
  代码与路由保留，存量账号照常显示与使用，可平滑迁移到 QoderCN；
- 添加按钮布局固化为：行1 `WorkBuddyCN | QoderCN | TraeWork`，行2 `WorkBuddyAI | QoderCOM | 千问办公`；
- QoderCN 紫色（`#6d28d9`）、QoderCOM 橙金（`#b45309`）主题贯穿徽标/按钮/费率表。

### Qoder 系签到定时

QoderCN / QoderCOM 自动签到固定每日 **10:15**（上游活动 10:00 UTC+8 刷新后的安全时点），
不再跟随全局签到时间配置。

## 修复

- QoderCN 登录后昵称不回填：设备流响应不含 nickname，改为登录后从 `/api/v1/userinfo`
  拉取显示名并写回凭据文件（修复首次绑定后面板显示 UID 的问题）；
- auth 凭据 glob 边界：`qoder*.json` 会吞掉 `qodercn-`/`qodercom-` 前缀文件，
  旧渠道加载器已显式排除，避免账号双计。

## 升级说明

- 旧 `qoder/*` 模型映射仍可用（后端保留），建议尽快切换到 `qodercn/*`；
- QoderCN/QoderCOM 凭据文件分别为 `auths/qodercn-<uid>.json`、`auths/qodercom-<uid>.json`，
  与旧渠道互不兼容（上游凭据区隔离，CN 的 dt- 在国际端点返回 401）；
- 若从 v2.3.x 升级：配置文件无破坏性变更，直接替换二进制即可。

---

# wild-work v2.5.2 — TraeCode 渠道 + 用量统计面板 + Qoder 上下文档位

> 本版包含 **两个社区贡献渠道能力**（TraeCode 渠道、Qoder 上下文档位透传，已按 GitHub 正式流程合并 PR，
> 感谢 @bibibiu-84 与 @youki258）、**用量与积分流水统计面板**、**OpenCodeZen 匿名免费渠道**、
> **单渠道上游代理**，以及 TraeWork 积分可用性判据的实测修正。
> 自 v2.4.1 以来累计 75 个文件变更，属于一个大版本的功能聚合，版本号直接从 2.4.x 跳到 2.5.2。

## 新增

### TraeCode 渠道（`traecode/*`，社区贡献 PR #33）

Trae **代码版**：与 TraeWork 是**同一上游的两个 function**（`solo_agent` vs `solo_work_lite`），
账号体系完全相同——**无需重新登录**，共用 TraeWork 的账号池：

- 与 TraeWork 共享账号与签到/保活调度，仅对话 function 与定价分组口径不同；
- 模型集与 TraeWork 不同：独有 `Doubao-Seed-Code`、`deepseek-v4.1-flash`、`glm-5.3-flash`、
  `kimi-k2.8-preview`、`qwen3.8-flash` 等新版代码模型；
- 定价按主 function 去重（同一模型在 `solo_agent` 与 `_remote` 分组下倍率可能不同，
  实测豆包 Seed-2.1-Pro：`solo_agent=0.08 / _remote=0.8`，对话按主 function 计费）；
- 蓝色渠道主题；映射预设提供「Codex → traecode」。

### 用量与积分流水统计（面板新增「用量与流水」区块）

- **双口径独立统计**：token 流水（渠道×模型）与积分流水（账号的入项/消耗/过期）各记各账，
  不做积分↔token 折算
- 面板提供 4 张汇总卡（Token 总量/请求数/积分消耗/积分入项）+ 时间范围切换（今日/7日）
  + Token 用量折线图（echarts，CDN 加载，离线时表格不受影响）+ 模型用量榜 + 账号积分小计
  + 最近 50 条积分流水（绿=入项 / 橙=消耗 / 红=过期作废）
- 积分过期从「猜」变成「对账」：基于上游余额条目快照差分（TraeWork 用 entitlement_id 精确对账）
- 数据落盘 `data/ledger/*.jsonl`（按月分段），统计仅在打开面板时读取聚合，
  常驻内存近乎为零；流水不含任何 token 凭证
- 注意：统计自本版本上线后开始记录，历史数据无法追溯；首日数据中的「存量额度」是
  各账号现有余额的基线入账

### OpenCodeZen 匿名免费通道（`oczen/*`）

新增第 7 个渠道 **OpenCodeZen（`oczen/*`）**：直接使用 OpenCode Zen 的**匿名免费通道**，
**无需注册、无需登录、无需添加账号**，启动即可用：

- 内置官方匿名凭证（`Bearer public`），开箱即用；也支持在设置中填入私有 API Key
- 自动从上游同步**免费模型清单**（只暴露免费模型 + `big-pickle`），上游增删免费模型时自动跟随
- 面板「账号管理」固定显示一项 **`[OpenCodeZen] 匿名`**，积分区域显示 **不适用**：
  不可添加、不可删除、不可停用、不支持签到与刷新积分
- 支持全部三种接口：Chat Completions、Responses、Anthropic Messages，流式/非流式均可
- 「模型列表和费率」面板中该渠道模型一律标为 **免费（✦ Free）**

常用免费模型（以 `/v1/models` 实际返回为准）：`oczen/big-pickle`、`oczen/mimo-v2.6-flash-free`、
`oczen/mimo-v2.5-free`、`oczen/nemotron-3.5-lightning-free`、`oczen/ling-3.0-flash-fin-free`。

> **说明与限制**：
> - **地域限制**：部分免费模型（如 `muse-spark-*-contributor-free`）对国内直连返回 403
>   `RegionError`。这类模型仍会出现在列表里，自备代理即可使用。
> - **稳定性**：该通道属于上游的非公开承诺接口，上游随时可能调整校验规则或增删免费模型；
>   如遇到大量 403 错误，请关注后续版本更新。

#### Jev 结构化决策端点（`POST /v1/systemone`）

OpenCode Zen 上还有一个免费的结构化决策模型 **Jev（`jev-1.13-free`）**，它不是聊天模型——
不能调 `/v1/chat/completions`（会 500），只能通过专用端点 `/v1/systemone` 调用。
本版网关已新增该端点的透明代理，用法：

```bash
curl -X POST "http://127.0.0.1:7863/v1/systemone" \
  -H "Authorization: Bearer WildWorkAPI" \
  -H "Content-Type: application/json" \
  -d '{"state":"...","questions":{"name":{"type":"noul|choice|score","criteria":...}}}'
```

- 网关负责鉴权 + body 解析 + model 注入 + 伪装头，客户端只需传 `state` + `questions`（含 `criteria`）
- 三种问题类型：**noul**（是/否概率 0~1）、**choice**（多选一 + 概率分布）、**score**（0~N-1 等级分）
- 实测 0.5~1.3s 完成一次决策，`cost:"0"` 免费
- 网关校验 `state`/`questions` 必填，缺则回 400；无 API Key 回 401
- 详见 README §5 Jev 结构化决策端点

### 单渠道上游代理

`config.json` 新增 `proxies` 段，**按渠道单独配置代理**（未配置 = 直连）：

```json
{ "proxies": { "workbuddyai": "http://127.0.0.1:7890", "oczen": "socks5://127.0.0.1:1080" } }
```

面板「设置」弹层可视化编辑；支持 http/https/socks5，热更新即时生效（无需重启）。
适合「仅国际版渠道需要代理、国内渠道直连」的混合场景。

### Qoder 系上下文档位透传（社区贡献 PR #34，修复 issue #27 / #32）

- 模型表解析补齐 `context_config`：默认档（`is_default`）与全部档位表（升序），
  三渠道（qoder/qodercn/qodercom）模型列表现展示真实上下文窗口；
- 请求侧支持客户端 `context_length` / `context_window` 提示透传（Chat / Responses / Anthropic
  三接口均支持），经官方同款校验后注入 `parameters.context_length` + `model_config.max_input_tokens`；
- 非法/缺省档位回落「最大档宁高勿低」口径，与官方客户端广告口径一致；
- `model_config.format` / `source` 透传上游真值——**修复思考过程不暴露**（issue #32：
  旧实现完全不下发 `model_config.source`，思考总开关失效）。

## 变更

- **临期阈值可配**：`config.json` → `schedule.expiring_threshold_hours`（默认 24，下限 24），
  面板设置弹层支持 1/2/3 天切换；`scheduler` 与积分刷新同源取值；
- **面板设置弹层重构**：签到时间/开机自启/监听地址/API-Key/代理/临期阈值统一收进一个设置弹层，
  帮助弹层增加风险声明区；
- TraeCode 渠道挂到费率表（`traecode` 蓝色主题）；
- **费率面板秒开**：入口不再在请求时触发上游网络拉取模型列表，改用后台缓存的最近一次结果
  （每 30 分钟自动刷新），面板打开即时渲染；
- **流水分段保留期收窄**：`data/ledger/*.jsonl` 由「保留 6 个月、单文件整月」改为
  「保留 1 个月、单文件裁剪 7 天」，启动时自动清理故磁盘占用更小；
  用量范围同步收敛为「今日 / 7日」（移除 30 日视图）。

## 修复

- **TraeWork 积分可用性判据修正（重要）**：上游自 2026-09-23 起不再下发 `available_endpoint=1`
  （专用池标记），导致 200 档每日签到积分被误计入可用余额、且被计入临期。经三账号实测
  （该批条目 used 恒为 0），改用 `product_id==209` 判定专用池——**面板显示的可用/不可用/临期
  三段与官网总账重新对上**；`available_endpoint` 判据保留为历史兜底；
- **TraeWork 账号在面板重复显示修复（issue #36）**：TraeCode 与 TraeWork 共用同一账号池，
  但聚合 `/api/state` 时被两个渠道各计一次，导致面板每个账号显示两张卡片。现按
  「别名渠道」去重，列表只显示一次；且账号级操作（刷新积分/明细/删除/停用）统一归到
  TraeWork 主渠道，避免误用 solo_agent 上游；
- 旧版 state 文件（v2.2.0 及之前无 unusable、v2 无 expiring）读入后显示「待刷新」，
  自动刷新首刷后自愈，无需删除 data 目录；
- OpenCodeZen 费率表去除冗余的「免费（匿名通道）」硬编码文案（`✦ Free` 徽章已足够表达）。

## 升级说明

- **从 v2.4.x 升级**：配置无破坏性变更，直接替换二进制即可；TraeWork 无需重新登录
  （TraeCode 与 TraeWork 共账号，升级后自动出现在渠道里）；
- **从 v2.3.x 及更早升级**：首次启动后 TraeWork 积分会显示「待刷新」，等待一轮自动刷新即可；
- 用量统计从本版本上线后开始记录，历史数据无法追溯；
- `data/state-*.json` 向后兼容（只增不减字段），旧文件自动迁移。

---

# wild-work v2.5.3 — 管理面板鉴权 + Qoder 双区签到重试 + 千问办公修复

> 本版以**安全加固**与**签到可靠性**为主：新增**管理面板密码鉴权**（监听非环回地址时强制启用），
> 修正 Qoder 双区（CN/COM）签到可能**整整漏领一天**的调度缺陷，并修复千问办公渠道
> 因上游升级导致的**对话恒 503**、以及一个会导致**账号被反复刷新直至作废**的 token 过期时间计算错误。

## 新增

### 管理面板密码鉴权（安全加固，重要）

面板此前完全无鉴权——只要把监听地址设为 `0.0.0.0`，**局域网内任何设备都能无凭据打开面板、
查看账号、甚至退出程序**。本版新增 `config.json` 的 `admin_password`：

- **为空 = 面板不鉴权**（默认，向前兼容）；此时**只允许监听环回地址**；
- **监听非环回地址（`0.0.0.0` / 网卡 IP）时强制要求设置密码**：未设置则**拒绝启动**并给出提示，
  面板里把监听地址改成 `0.0.0.0` 也会被拒绝（启动层 + 配置保存层双重把关）；
- 登录后下发 **HttpOnly cookie**（7 天有效期，使用中自动续期；SameSite=Lax 防跨站请求），
  服务端**只存 token 的 SHA-256**，重启即失效；**改密码后所有已登录会话立即失效**；
- 登录失败 **5 次锁定该来源 IP 5 分钟**；
- 前端不读取 cookie（HttpOnly），登录态靠 `/api/auth/state` 回传的口令指纹判断；
- **OpenAI/兼容接口的 `api_key` 完全不受影响**——面板密码是给「人」登录的，`api_key` 是给
  AI 客户端用的，两套凭据相互独立；
- 面板设置弹层新增「管理密码」输入框与「退出登录」「清除密码」按钮；低于 8 位会在启动时告警（不阻断）。
- **警示与必填标记跟随选择实时联动**：选 `127.0.0.1`（本机）时不显示警告条与必填星号；
  切到 `0.0.0.0` 或自定义地址时立即出现（黄色警告条 + 「管理密码」label 后的红色 `*`）。

支持 `WILDWORK_ADMIN_PASSWORD` 环境变量配置。

## 变更

### Qoder 双区签到改为「窗口内重试」，避免漏领一整天

上游签到活动**每日 10:00（UTC+8）开放，但活动可能在整点之后才创建**。旧实现每天只触发一次，
一旦那一刻活动还没上线，就会**整整漏领一天**（上游 qoder2api 亦为此专门修复）。

- QoderCN / QoderCOM 改为 **10:00–12:00 窗口内每分钟重试**，直到真正拿到 `claimed` / `already_claimed`；
- 完成状态按「**日期 + 时段**」而非仅日期标记——本工具支持一天多时段签到（如 09:00 / 21:00），
  只按日期标记会让早间成功吞掉晚间时段；
- 签到结果改为**结构化上报**（`provider.CheckinReporter`）：新增 `claimed` / `already_claimed` /
  `no_campaign` / `no_token` / `error` 五种状态。此前用 error 文本判断，**无法区分「活动还没上线」
  与「真出错」**，两者都需要重试；
- 「无可用活动」不再当作成功——这正是漏领的根因；
- 非 session dead 的失败（网络抖动、上游 5xx）在窗口内同样重试；session dead 仍走
  「刷新 token 后重试」的自愈路径；
- 面板「下次签到」时间与实际触发时机同源（含窗口重试分钟），不再显示成 10:01 这种误导值。

## 修复

### 千问办公对话恒返回 503 `Model catalog unavailable`（上游 1.0.4 变更）

上游千问办公 1.0.4 起改为**从请求体 `business` 段解析模型目录**，缺失即拒绝对话——
但**只在推理路径生效**（模型列表、余额、费率都正常），且**HTTP 状态码仍是 200**，
错误藏在 SSE envelope 的 `statusCodeValue=503` 里，因此表现为「对话永远失败但一切看起来正常」。

- 请求体补 `business: {product:"qoder_work", type:"agent", version:"1", feature_switches:{}}`；
- **仅补 `Cosy-Business-*` 静态头不能替代**该字段；
- 上游同时引入了走 WASM 的 `Encode=1` 组包（需额外运行时依赖）。本项目做了**四组对照隔离实验**：
  body 缺 `business` 时，「现有透传 body」与「上游原生重构造 body」**都 503**；补上 `business` 后
  **都 200**——即 **503 只由该字段引起**，原生 body 结构、`Encode=1` 组包、机器指纹均非必要条件。
  故本项目**只补一个字段即可**，无需引入 WASM 运行时依赖。

### 千问办公 token 过期时间单位算错，导致账号被反复刷新直至作废（重要）

上游刷新接口返回的 `expires_in` 单位是**秒**，旧实现按**毫秒**处理——把 7 天压成 604.8 秒：

- 后果链：落盘 `expiresAt` 比真实寿命少约 7 天 → `NeedsRefresh` 几乎恒为真 →
  **每次请求都去刷新 token** → 与千问办公官方 App 高频互踩（refresh token 是轮换的）→
  最终 refresh token 被上游作废、**账号被禁用，只能重新登录**；
- 证据链：上游 `expires_in=604800` 按秒算 = access token JWT 的 `iat→exp`（整 7 天，精确吻合）；
  按毫秒算 = 文件里的错误值（精确吻合）。横向对比同一 `auths/` 目录：workbuddy / trae /
  workbuddyai 的文件 `expiresAt` 与各自 JWT 逐秒一致，**只有 qwenwork 偏离 6.99 天**；
- 修复：refresh 按秒解释，优先取绝对字段 `expires_at`；并在加载凭据时用 access token 的
  **JWT `exp`（上游签名，权威）原地校正历史脏值**（仅内存、只增不减、非 JWT 不动）——
  **存量账号因此自动恢复，无需重新登录**。

> 同族的 `qoder` / `qodercn` / `qodercom` 也带着类似的单位标注，但它们的 token 是 `dt-`/`drt-`
> 不透明串，**没有 JWT 可交叉验证**，在拿到证据前**不做改动**。

### OpenCodeZen 匿名渠道不再因单次错误冷却（导致整条渠道下线）

OpenCodeZen 是**匿名免费渠道**，整池只有**一个虚拟账号且不可重登**。此前的错误处理把它
当作普通多账号渠道：任何账号级惩罚实际上都等于**整条渠道下线**——因为无号可轮换，
惩罚后请求会在挑号阶段直接返回 `503 no_healthy_account`，连「稍后重试」都做不到。

实测确认两条错误路径会惩罚唯一账号：

- **429（限流）→ 冷却**：旧设计认为「429 短冷却是唯一需要的背压」，但实际效果是
  **第 2 个请求被挡成 503**，而不是把上游 429 透传给客户端，反而丧失了按 `Retry-After` 退避的能力；
- **传输层错误 → 累计 `errCount`**：默认累计 3 次即冷却——网络抖动几下就足以让整条渠道下线。

修复：新增 `server.Runtime.SingleAccount`（**渠道结构属性，不硬编码渠道名**），
对该类渠道**任何错误一律原文透传**：不冷却、不累计错误、不禁用；
启动时额外调用 `Pool.ClearPenalty` **自愈旧版本遗留的冷却**（否则升级后渠道仍然下线）。

> 修正后「单账号就不可惩罚」成为一条统一约束，未来若再接入同类匿名/单账号渠道，
> 只需设标志，不必再动 handler。

## 验证

- 千问办公：`dist/auths` 本地凭据实测 `pro` / `flash` / `qwen3.8-max-preview` 三档，
  流式与非流式（经真实 daemon 的 `/v1/chat/completions`）**全部 200** 正常作答；
- 面板鉴权：环回无密码可直连、`0.0.0.0` 无密码拒绝启动、`0.0.0.0` + 密码时未登录 401 /
  错误密码拒绝 / 正确密码下发 cookie 后可访问、连续错误 5 次触发 IP 锁定；
- 设置弹层联动：Playwright 实测 `127.0.0.1` / `0.0.0.0` / 自定义 `localhost` /
  自定义 `192.168.1.100` 四种选择下的警告条与必填星号显隐均符合预期；
- `go build` / `go vet` / `go test`（全部 18 个包）通过；新增回归测试覆盖
  `expires_in` 秒语义、JWT 校正、签到窗口重试与完成状态、面板会话与限流；
- OpenCodeZen：遍历 400/401/402/403/404/409/429/5xx 断言唯一账号均不被冷却/计数/禁用
  且上游原文被透传；429 后第二个请求仍真正到达上游（旧实现会被挡成 503）。
  回归测试已验证：暂时关闭修复后 3 个用例失败，恢复后全部通过。

## 升级说明

- **配置无破坏性变更**，直接替换二进制即可；`admin_password` 缺省为空 = 面板不鉴权（保持旧行为）；
- **若你此前监听 `0.0.0.0`**：升级后需先在 `config.json` 补上 `admin_password`（或把监听改回
  `127.0.0.1`），否则程序会拒绝启动并给出明确提示；
- 千问办公账号无需重新登录（历史 `expiresAt` 会在加载时自动校正）；
- OpenCodeZen 无需任何操作（旧版遗留的冷却会在启动时自动清除）；
- `data/state-*.json` 向后兼容（只增不减字段）。

---

# wild-work v2.5.4

> 发布主题：**新增第 9 个渠道「智谱清言（glm）」** + 管理面板费率页多标签改版 + 一批由社区贡献与日志实证驱动的修复。
> 本版共 14 个提交、56 个文件（+9841/−1459）；含 1 个新渠道、3 个社区 PR、6 个 issue 修复。

---

## ✨ 新渠道：智谱清言（`glm/*`）

对接智谱清言（chatglm.cn）**网页版私有接口**（非 open.bigmodel.cn 开放平台），按「额度」计费而非 token。

- **4 个实测可用智能体**：ChatGLM / AI搜索 / 清言PPT / 视频助手；三个 chatglm 变体（普通/推理/沉思）共用底层模型 `moe_53f`，模型名带上游代号后缀（如 `glm/chatglm:moe_53f`），旧名保留为别名不断供
- **登录**：面板一键拉起**独立 profile** 的 Edge/Chrome（CDP 自动捕获 Cookie，零第三方依赖、零 WebView2），多账号逐个添加互不干扰；自动路径失败时自动降级「手工粘贴 `chatglm_refresh_token`」兜底
- **每日积分**：`daily_login_score` 已接入保活流程（该接口网页端打开页面时自己就会调，幂等安全）；「签到」的实质是保活对话，自动化边界 = 保活对话 + 积分读取，不碰任何写操作（伙伴/群聊任务明确不做）
- **跨平台**：Windows/macOS/Linux 均可（只需系统装有 Edge 或 Chrome）；
  贡献者 ttales430 已将协议逆向成果独立开源为 [glm2api](https://github.com/ttales430/glm2api)

**已知边界**（登录弹窗内有同等提示，请知悉后再启用）：

- ⚠️ 新账号添加后积分为 0 是正常的：需在**智谱清言手机 App** 登录一次才发放额度（+3000，随后 +500）——上游行为，对照实验证实，Web 端无激活接口
- 不支持 tool_calls（上游协议无此机制）、不支持图片输入（图像消息被剥离）、服务端对话不复用（上下文由本工具逐轮重放）
- 体验渠道定位：对话与搜索可用，依赖函数调用/多模态的客户端（如 Codex）请勿路由到此渠道

**接入过程中发现并上升为通用决议的两个 bug**：

- **R34 调度器漏启动**：新增渠道时漏写 `go xxxSch.Run(sctx)` 会导致该渠道自动签到/保活从不运行——不报错、不崩溃、手工触发仍可用，极难发现。新增静态扫描回归测试钉死此形态
- **R35 流式超时掐断长流**：`http.Client.Timeout` 覆盖整个响应体读取，长回答会在超时点被掐断且无终止帧（实测触发 5 次）。glm 采用独立 `StreamHTTP`（不设 Timeout，照 traework 模式）

## 🖥️ 界面改版

### 费率面板：单页长表 → 渠道多标签

9 个渠道的模型费率原本挤在一张长表里（几百行，翻找困难）。改为**每个渠道一个标签页**：

- 标签上带模型数量角标，点击即切，不重建 DOM（滚动位置不跳）
- 非当前渠道的面板直接隐藏，刷新费率后保持当前选中标签
- 各渠道表格列结构不变（双列模型/倍率、能力图标、上下文窗口标注）

### 智谱清言按钮归位 + 登录风险告知

- 渠道添加按钮改为两行布局，glm 与其余渠道对齐
- 「＋ 智谱清言」弹窗内置琥珀色风险告知块（已知边界如上），确认后才发起登录

## 🔧 修复

### 积分流水：同 key 重复资源重复记账（#38）

WorkBuddy 伪键「套餐名|到期日」可对应多条独立套餐，逐条差分会重复记 spend、快照只留末条导致下次继续错。修复：差分前按 key 聚合求和，每 key 每次刷新最多一条事件；升级首启一次性把旧错误流水归档为 `old-credit-*.jsonl`（不进查询口径、随保留窗口自清）并重建 baseline。

### WorkBuddyAI：9 个模型倍率从 unknown 变为真实值（#39）

并入上游 `/v3/config` 目录（实测国际版可用）：deepseek-v4.1-flash x0.00（免费）、gpt-6-astra x6.67、kimi-k2.8-preview x0.77、**deepseek-v4.1-flash-sg x0.03**（新增模型）、**glm-5.3-flash x0.06**（新增模型，官方客户端同价）等；同 id 冲突以 v3 为准（hy4-preview 修正为 x0.29）。上游 v2/v3 均未下发费率的模型继续显示 unknown——不编数据。

### 费率面板：模型列表不再整组消失（#40）

QoderCN/QoderCOM（按设计无静态兜底表）在缓存过期后整组从费率面板消失。三层修复：

- 过期缓存沿用展示（陈旧比空好），模型缓存获得后台定时填充源（随费率 30min 周期刷新）
- **冷启动 401 自愈**：模型拉取路径此前的 token 刷新缺失，账号 token 临期时静默失败并进 5 分钟负缓存，费率与模型两条路径行为不一致。现与费率路径同款——拉取前 refresh + 落盘，失败显式记日志
- **登录误杀缓存**：任何渠道登录新账号时会清空全部渠道的模型缓存，但随后的刷新只拉费率不拉模型 → 其它无静态兜底渠道从面板消失直到下个 30min 周期（生产实测消失 28 分钟）。现失效后立即同步重建

### TraeWork：专属积分已升级为通用积分，撤回「不可用」标注（#44）

2026-09-23 引入的 `product_id==209` 判据被本仓日志实锤证伪：99 次对话期间可扣池余额纹丝不动（恒 3086），而「不可用」小计 2200→1846（−354，恰为对话消耗量）——**扣费实际发生在被判为「不可用」的池上**。撤回后：面板不再误标，账号池路由恢复使用这批真实可扣积分（每账号约 +2000）。

### qoder 系：偶发 101 Signature invalid（PR #45）

签名串与 `cosy-date` 头各自取一次时间，长会话（数 MB 上下文）下跨秒概率随 body 增大而升高 → 上游 101。四处独立副本同改：`AuthHeader` 返回签名所用时间戳，`ApplyHeaders` 复用它，全链路只剩一次取时；回归测试以注入时钟锁死契约。

### TraeWork/TraeCode：模型目录被 1MB 上限截断（#41，PR #43）

`solo_agent` 的目录响应 1.28MB 被 `1<<20` 截断 → JSON 解析失败 → 静默回退静态表（面板只有 16 个模型）。上限提至 8MB 并改为**超限显式报错**（不再把「自己截断」伪装成「上游坏 JSON」）；实测 `/v1/models` 从 16 恢复到 49。

### 请求体上限 8MiB → 32MiB（#30 用户反馈）

原 8MiB 上限对纯文本绰绰有余，但多模态大图以 base64 内嵌请求体时体积膨胀约 1.37 倍，
十余 MB 的截图即触发 413。提至 32MiB；超限仍**明确回 413 说真话**
（v2.3.1 修复的「静默截断误报 invalid_model」行为不变），错误文案同步带上 MiB 数字。

## 🚄 性能

### 管理面板打开卡 3~10 秒（#47，PR #49）

echarts 的旧 CDN 源响应 `no-store`，**每次打开面板都重下 751KB**。换用 registry.npmmirror.com（同一份文件，sha256 一致）后：首次加载资源 ~100ms，二次打开 **0ms（缓存命中）**。

## 📦 升级说明

- 直接替换 `wild-work.exe`，配置/账号/流水**零迁移**；积分流水旧错误条目首启自动归档，无需手工处理
- 智谱清言：面板「＋ 智谱清言」→ 阅读风险告知 → 在弹出的独立浏览器窗口登录 → 自动完成；若自动路径不可用，界面会给出手工粘贴指引；**添加后记得手机 App 登录一次激活额度**
- 升级后建议在面板点一次「刷新积分」：TraeWork 账号的「不可用」积分将并入可用余额

## 🙏 致谢

本版包含 4 个社区贡献（均已合入 master 并保留作者署名）：

| 贡献者 | 内容 |
|--------|------|
| [@xxhhlk](https://github.com/xxhhlk) | #43 模型目录上限修复、#45 签名时间戳同源 |
| [@ttales430](https://github.com/ttales430) | #46 智谱清言渠道（含协议开源 [glm2api](https://github.com/ttales430/glm2api)） |
| [@jianzhangg](https://github.com/jianzhangg) | #49 echarts CDN 换源 |

以及 issue 报告者：[@HappyCode-HC](https://github.com/HappyCode-HC)（#38/#39/#40/#44）、
[@ciot-plus](https://github.com/ciot-plus)（#41）、[@jianzhangg](https://github.com/jianzhangg)（#47；#42 已定位待议，未随本版修复）。

## 📋 全部变更

```
c9b1313 fix(app): afterAccountAdded 失效模型缓存后立即重建
0bf1f4c feat(server): 请求体上限 8MiB → 32MiB
7dac299 fix(models): fetchRuntimeModels 补 401 自愈 + 失败显式日志
d77c874 fix(ui): 费率多标签——清理遗留的孤儿 table 开标签
8bef42c fix(ui): 费率标签切换——非 active 面板也渲染到 DOM
9128c4e ui(panel): 费率表收束为渠道多标签面板，tab 切换不重建 DOM
2367086 ui(panel): 智谱清言按钮归位两行布局 + 登录弹层增加体验渠道风险告知
b384648 fix(workbuddyai): 并入 /v3/config 目录，补齐 extraModels 的真实倍率 (issue #39)
ea7b8ae perf(panel): echarts CDN 换用 registry.npmmirror.com (issue #47)   [PR #49]
b2daf6e fix(panel): 费率面板模型列表不再因缓存过期整组消失 (issue #40)
080ee99 fix(traework): 200 档签到（pid=209）已升级为通用积分 (issue #44)
65ea72d feat(glm): 新增智谱清言渠道                                        [PR #46]
1fbc3f7 fix(cosy): 签名时间戳与 cosy-date 头同源                           [PR #45]
e58cc78 fix(traework): 模型目录读取上限 1MB → 8MB (issue #41)              [PR #43]
b719ff4 fix(ledger): 同 key 重复资源先聚合再差分 + 归档旧错误流水 (issue #38)
```

---

# wild-work v2.5.5

> 发布主题：**社区 3 个 PR 入库 + 一次性修复流式截断伪装成功（issue #42）、TraeWork 登录首因掩盖（issue #50）、WorkBuddyAI 模型级限流拖垮整账号（issue #53）**。
> 本版共：3 个已合 PR（来自 xxhhlk）+ 4 项变更（截断修复 / 登录修复 / 模型级限流 / 流式超时推广），上次 tag v2.5.4 之后累积 6 个提交。

---

## 🔧 已入库的社区 PR（来自 xxhhlk）

| PR | 内容 |
| --- | --- |
| #51 | **qoder 系带 tool_calls 的 assistant 消息 content 为 null 致上游整请求拒答**：在服务器层 `rewriteModel` 规范工具轮空 content；实测矩阵 + 回归测试锁定。放对位置（它覆盖不了 PrepareBody 的渠道，且兼容响应层复用内层） |
| #54 | **去掉告别弹窗上的 `MB_DEFAULT_DESKTOP_ONLY`**：弹窗回到屏幕中央、不再半屏出界，且避免锁屏/RDP/UAC 下 MessageBox 永不返回 |
| #55 | **Win 托盘菜单图标用 PNG+ICO 容器 + 退出前摘除图标**：修掉上游自研 PNG 编码器坏（长度恒 0/CRC 错）导致的图标空白，退出不再留幽灵图标 |

## 🐛 本版新增修复

### issue #42 — 流式截断被伪装成「正常收尾」（[DONE]）

**现象**：Zed 等严格客户端偶发报 *tool input was not fully received*，原样重试往往就成功；参数越长/并行 tool_call 越多越容易触发；直连复现排除网络跳。

**实证**：qwenwork / qodercn / qodercom / qoder 四份 `sse.go` 的 `sawDone` 是**死变量**（声明后从不赋值）。于是：
- 上游连接中断 / 读超时被掐断，或断在半个帧上（末行无换行），出口照样补 `data: [DONE]`；
- 客户端看到「以 `[DONE]` 完美收尾」的流，只是 tool_call `arguments` 少了后半截 → 只能报「输入没收全」，而网关日志**没有任何错误**，极难排查。

**修法**：
1. `parseNestedSSE` 增加 `truncated` 出参（读错误 / 半帧 → true），`streamAsOpenAI` 里截断时改发一帧 OpenAI 规范 `error`（`code: upstream_truncated`）且**不发** [DONE]。
2. 通用流式路径（WorkBuddy/WorkBuddyAI/oczen 共用 `upstream/sse.go`）同样改造。
3. 刻意保留：上游「正常 EOF 但漏发 [DONE]」（帧完整、末行有换行）仍兜底补 [DONE]，避免误伤本就补 DONE 的渠道。
4. **流式超时推广到全渠道**（对齐 R35）：之前只有 glm / traework 有独立 `StreamHTTP`（无 `Client.Timeout`），其余 7 个渠道（qwenwork/qodercn/qodercom/qoder/workbuddy/workbuddyai/oczen）的 `ChatStream` 全用带 Timeout 的 HTTP ⇒ 长流式会在超时点被掐断。现全部补上 `StreamHTTP`，并同步修正 main.go **两处** `applyProxies`（启动 + 面板热更新）都传入 `{HTTP, BillingHTTP?, StreamHTTP}`，避免流式漏掉代理。

**回归测试**：`internal/{qwenwork,qodercn,qodercom,qoder}/stream_truncation_test.go` + `internal/upstream.TestStreamTruncationNotDisguisedAsDone`，覆盖四种输入（传输层错误 / 半帧 / 正常 [DONE] / EOF 漏发）。

### issue #50 — TraeWork 登录恒报 400/10101（首因被换 origin 掩盖）

**现象**：浏览器授权完成，但后台用 AuthCode 换 token 恒失败，每 ~2s 刷一条 `400 {"Code":"10101","Message":"无效参数：{__Message.field}."}`（占位符原样返回），直到超时/取消，账号始终加不进去。

**实证根因**：
1. `ExchangeAuthCode` 对 **4xx 也 `continue` 换 origin** 重试，把首选 `api.trae.cn` 的真实错误（403/20401 设备数上限）覆盖成回退 origin `api.trae.com.cn` 的 `400/10101` 后再打印 ⇒ 表象彻底指向「参数写错了」；
2. authCode 一次性，4xx 属终态，旧版却每 2s 重试一次刷屏；
3. **设备号每次登录随机重新生成**：`login_trae.Start` 每次 `randHex(32)`，每点一次「+ TraeWork」都相当于新设备，逼近「同一账号最多 10 台设备」上限。

**修法**：
1. **4xx 立即终态**：`ExchangeAuthCode` 对 4xx（除 429/408）返回携带**首个** origin 真实响应的 `*AuthCodeRejectedError`（含 origin/status/body），不再换 origin 掩盖首因；429/408/5xx/传输层错误仍视为暂时性继续重试。
2. **单表识别 20401**：`IsDeviceLimitReached` 命中 `403 + 20401 / device limit` → 可行动提示「去其它设备登出释放名额（同一账号最多 10 台）」。
3. **终态固化 + 轮询短路**：`login_trae.Poll` 把终态错误写入 `login-state.json` 的 `Err` 并清空 AuthCode，后续轮询直接短路，不再每 2s 重消耗同一次性 authCode 刷屏。
4. **设备号持久化**：复用并落盘 `device-id.json`（`login-state.json` 同目录、独立文件、不复用一次性 state），官方客户端本就持久化设备号。

**暂未做**：版本常量对齐 0.1.69（issue 自标「未证实是成因」，行为侧有风险，无证据不动）。

### issue #53 — WorkBuddyAI 模型级 429 拖垮整账号（最小修复）

**现象**：A 的 `deepseek-v4.1-flash` 撞上游每日上限后，A 上还能用的 `gpt-5.6-luna` 也跟着不能用。

**根因**：上游按**模型**限流（6004 / body 含 *switch to the other models*），但 `Classify` 把它归进账号级 `ErrSoftRate` → 冷却整个账号，其它模型一起被拖进 60s 软冷却并被粘性路由跳过。

**修法（最小、低风险，不做大架构改动）**：`workbuddyai.Classify` 对含该标记的 429 判为 `ErrPassthrough`（请求级透传、不冷却账号），透传原文让客户端按 Retry-After 自退避或换模型——与 R16 单账号渠道同哲学。
**明确不做**：pool 升级到 model 维度、`stickyKey` 改为 `(kind,model)`、state v4——那是动 pool/state/handler 三核心、约 370 行的大改造（issue 作者已主动提出可拆两个小 PR，需单独评估立项）。

---

## ⚠️ 已知边界

- `MB_DEFAULT_DESKTOP_ONLY` 的移除：双屏定位本机无法实测（项目单显示器环境）；如需固定主显示器，正确做法是给 `MessageBoxW` 传 owner HWND（超出本版范围）。
- TraeWork 20401 的**确切成因**仍是 issue 里未证实项（同一账号装官方客户端能登录成功）——本版修复了「首因透出」与「设备号持久化」两个确定性正确点，真实错误码能否解现场需在修复后版本复现确认。
- 模型级限流判据（`switch to the other models`）**仅对 WorkBuddyAI**生效；其它渠道的 429 语义仍是账号级。

## 📎 升级提示

- 本版无需迁移 state（不动 `state-*.json` 格式）。
- 流式超时推广后，长回答不再因 `Client.Timeout` 被从中间掐断；若你的 `config.json` 里 `upstream.timeout_seconds` 调得很小，流式场景不再受限于它（非流式一问一答仍受保护）。
---

# wild-work v2.6.0 — 三个新渠道（MonkeyCode / 商汤小浣熊 / 讯飞 Loomy）+ 用量积分四处修复

> 发布主题：**社区贡献的三个新渠道一次性入库**（MonkeyCode、商汤小浣熊、讯飞 Loomy），渠道数升至**十二**；
> 同时修复**四个用量/积分统计问题**（月初看不到当月数据、签到收入不入账、Trae 系 token 流水恒空、流水排序错乱）。
> 上次 tag v2.5.5 之后累积 16 个提交。

---

## 🆕 新增渠道（社区贡献，来自 xxhhlk）

### MonkeyCode（`monkeycode/*`）— PR #65

平台托管的模型聚合服务（后端接 11 家提供商），一个渠道覆盖一批模型。

- **按模型声明的 `type` 分流**：Anthropic 形状走 `{base}/messages`，Responses 形状走 `{base}/responses`，两路都是「每轮带完整上下文」的无状态 SSE（无 VM、无任务会话、无会话 id 复用）。
- **导入型凭据**：面板「＋ MonkeyCode」读取本机官方客户端（ohmyagent）的 `settings.json`，一个账号 = `oma_` api_key + `omas_` signing_secret，另有控制台会话 Cookie（拿不到不影响对话）。
- 上游无 `GET /models`（返回 405）→ **模型表为静态清单**（25 个模型，basic/pro/ultra 三档）；无额度/刷新接口 → 额度恒 0、`RefreshToken` 为空实现。
- 未知模型本地 400（不请求上游，不罚号）。

### 商汤小浣熊（`raccoon/*`）— PR #64

- **两条添加路径**：① 面板主按钮走**浏览器授权登录**（登录期间临时把 `office-raccoon://` 协议回调指向本工具以接住授权码，结束立即恢复注册表；启动时另有残留自愈）；② 弹窗次按钮「从客户端导入」读本机 `.box-agent/config/auth.json`。两条路拿到同一账号，落盘形态一致。
- 凭据 `access_token` ≈2h + `refresh_token` ≈30d，保活每 4 小时一次以减少「请求先 401 再刷新」的往返；无签到端点；上游默认档即最深思考，故不接档位面。

### 讯飞 Loomy（`loomy/*`）— PR #63

- **导入型**：面板「＋ Loomy」读本机 `C:\Users\Public\Loomy\<hash>\userData\auth-session.json`（枚举 hash 子目录，跨账户跨版本都能命中）。
- **上游无 refresh 端点**，session ≈14 天，到期需重新登录并再次导入；故 `RefreshToken` 留空、定时任务全关（保活会被按「无 refresh token」跳过，但每天仍空跑一次并记失败）。
- **鉴权失败是 HTTP 200 + 业务码 `100002`**（不是 401）——业务码判定必须排在状态码判定之前，否则失败会被当成成功。

### 三条 PR 带来的共享增量

| 文件 | 增量 |
| --- | --- |
| `internal/provider/provider.go` | `ErrBadParams` / `ErrImageInvalid` 错误类（请求级，不罚号不轮转）；`ResourceItem.InfoOnly`（只展示、不计入小计） |
| `internal/provider/stream.go` | 空闲看门狗 `IdleReader` + 截断补帧（无总超时 client 的空闲兜底） |
| `internal/provider/codemarker.go` | `CodeMarker`：业务码判定收在一处 |
| `internal/auth/auth.go` | `Auth` 增 `SigningSecret` / `ConsoleCookie` / `BaizhiCookie` 字段；`LoadMonkeyCodeDir` / `LoadRaccoonDir` / `LoadLoomyDir` |
| `internal/app/app.go` | `POST /api/account/import_local`（导入型渠道统一入口） |

> 生产验证：三渠道均在**真实账号 + 真实上游**上跑过端到端（隔离实例，凭据用后即删）；合计 71 个测试函数，全部对假上游、不依赖网络。

---

## 🐛 本版修复

### 1. 面板「近 7 天」每月 1~6 号看不到当月数据（issue #59）— PR #58 / #66

**现象**：月初打开面板，Token 折线图停在月底，当月模型全部消失；积分 7 天视图全空。用户实测同一份数据少了约 2/3（194M → 549M token）。

**根因（两处叠加）**：

1. **月份枚举跳月**：`Query` 从 `from` 起按 **+15 天** 步进枚举月份分段。7 天窗口跨月的常态下（`from` 落在上月 25 号之后），步进一步就越过当月 ⇒ 当月分段整月不被扫描。每月 1~6 号必现，跨年同理。
2. **`entries` 未排序**：分段枚举用 `map`，`range` 顺序随机；跨月时会把当月条目排到上月之前，前端按「升序数组 + 倒序分页」渲染时表现为**最新记录排到后面几页**。

**修法**：① 改为按「`from` 所在月 → `now` 所在月」**逐月步进**；② `Query` 返回前显式 `sort.SliceStable` 按时间升序。

**回归测试**：`TestQueryCrossMonthSegments` / `TestQueryMonthEnum`（含跨年）/ `TestQueryEntriesTimeSorted`。

> 附带说明：仓库里 `TestQueryAggregation` / `TestDiffCreditsDuplicateKeys` 两个测试**一直只在月初变红**（往当月文件写数据、被测代码只读上月），正是这个 bug 的探针。修复后稳定通过。

### 2. 签到/周期发放的收入从不进账本（账本只降不升）— PR #61

**现象**：9 月账本 26 个账号整月 earn **只有 baseline 一条**——每日签到（+100/天）与月周期发放全部漏记，面板收入恒为 0。

**根因**：`DiffCredits` 对「快照中不存在的新 key」直接 `continue`，把上游每次发放都静默吞掉。

**修法**：非首次出现的新 key 且 `remain>0` 时记一条 earn；首次账号仍只记一条「存量额度」baseline，同 key 重复条目先聚合求和再差分（issue #38）的口径不变。

**实测**：修复后 26 账号每日签到 26/26 全入账（10-03 +2600、10-04 +2600），逐条累计与上游 remain 分毫不差。

### 3. Trae 系（traework/traecode）token 流水全丢 — PR #58

**现象**：模型在用、客户端自己显示的 token 数正常，但**面板 token 恒 0**（`src=none`）。

**根因**：SOLO 协议事件序 `output → token_usage → done`。`done` 分支旧序**先 `writeChunk` 再取 `pendingUsage`**——而 `writeChunk` 会把 `pendingUsage` 附加进末 chunk 后置 `nil`，于是 `usage = pendingUsage` 永远拿到 nil。客户端不受影响（usage 已随末 chunk 透传），只有网关自己记账的 `Stream()` 返回值恒空——这正是它难被发现的原因。

**修法**：`done` 分支先 `usage = pendingUsage` 再 `writeChunk`（两行换序）。

### 4. Trae 流内业务错误伪装成正常收尾 + 账号不冷却 — PR #61

**现象**：上游 SOLO 在流中途回 `event:error`（3004 限流 / 1005 权益不足）时，客户端看到「正常结束、内容莫名其妙」，拿着半截回答继续跑；账号不冷却，重试必然再撞同一号。

**根因**：错误分支写成 `delta.content` + `finish_reason:stop` + **补 `[DONE]`**，且函数返回 `nil`；更糟的是 `NoteSuccess`/`stickySuccess` 在读流**之前**已执行，粘性路由把请求钉死在中招账号上。

**修法三件套**：

1. `solosse` 错误分支改发 OpenAI 规范 error 帧（限流标记 `upstream_rate_limited`）、**不补 `[DONE]`**、把错误返回调用方；
2. 新增 `provider.StreamErrorClassifier`，handler 按 kind 冷却账号（软 60s / 硬 12h / 禁用），server 层不 import 具体渠道包；
3. 粘性路由见 `status.Cooling` 自动让位，无需显式 `stickyClear`。

**兼容层必须识别 error 帧**：`gateway.parseChatSSELine` 此前只认带 `choices` 的分片，error 帧被静默丢弃，致 `/v1/messages` 发 `end_turn`+`message_stop`、`/v1/responses` 发 `response.completed`+`[DONE]`，把失败伪装成成功（该缺口对内层所有渠道通用）。**协议合规**：Anthropic `error.type` 是 9 元判别联合，限流映射 `rate_limit_error`；Responses 用 `response.failed`，私有码只放自由 `code` 字段。

**生产实测**：09:13 真实 3004 触发完整链路——粘在 A 号 → 撞 3004 → `stream error cooled kind=soft_rate cooldown=1m0s` → **1 秒后**换到 B 号 → B 也撞 → 冷却 → C……当日 11 次冷却全部正确触发换号，60s 后自动恢复。

### 5. 四个「无定时任务」渠道不再每天空跑 — PR #56

**现象**：账号卡片每天固定出现一次红色「签到失败」标签（消息「qoder 暂无签到活动」/「no refresh token」）；千问办公更严重——注释写着「定时保活会与 App 互踩」，但每天 22:00 的保活批次照常跑。

**根因**：`scheduler.New` 用 `len(...) == 0` 判默认值，把 `nil`（未配置 → 落默认）与 `[]int{}`（本渠道没有这类任务）混为一谈，调用方「显式不要」的意图被兜底成默认时间。

**修法**：两处判据改 `== nil`；qoder / workbuddyai / qwenwork / oczen 四处调用点改传显式空切片；`Run()` 补「两个列表都为空时阻塞等 ctx/配置变更」（否则 `nextFireMinutes` 返回零值会变成每天 1440 次空转）。

---

## 🎨 面板

- 账号卡片新增**冷却状态**显示（「冷却至 MM-DD HH:MM」琥珀标签 + hover 原因）——此前后端早已透出 `cooling/until/reason` 但前端从未渲染，表现为「积分明明很多却报 503/限流」而卡片毫无提示。
- 新增**渠道积分汇总条**（每渠道可用/临期/合计，与账号卡片同源）。
- 积分流水与模型榜的渠道列改为彩色徽标；积分折线图系列名带渠道前缀（跨渠道同名昵称可区分）。

---

## ⚠️ 已知边界

- 三个新渠道均为**单作者验证**，无第三方用户验证；亦**不覆盖多账号并发**下的路由与冷却。MonkeyCode 的 `pro/*` / `ultra/*` 整档未复测（账号无权限，实测 403）。
- 依赖上游返回结构的字段（模型目录、费率、档位表）上游一改就得跟着调；MonkeyCode 的模型表是**静态清单**（上游 `GET /models` 返回 405）。
- 小浣熊的浏览器授权登录需临时改写 HKCU 的 `office-raccoon` 协议注册（登录结束即恢复，启动有自愈）；非 Windows 平台该路径为桩实现，只能用「从客户端导入」。
- TraeWork 3004 的限流维度**按账号级处置，但证据不足**：观察仅 1 例且来自 checkin 路径，只足以排除 IP/全局级。因软冷却仅 60s，判据不成立时最坏影响是闲置一分钟，故维持现处置。
- 本版不动 `state-*.json` 格式，无需迁移。

## 📎 升级提示

- 新增三渠道后面板渠道按钮为**两行五列**：
  行1 = WorkBuddyCN / QoderCN / TraeWork / MonkeyCode / Loomy；
  行2 = WorkBuddyAI / QoderCOM / 千问办公 / 小浣熊 / 智谱清言。
  导入型渠道点按钮即为「导入」动作（读本机已登录客户端凭据，不需要浏览器登录）。
- 若你的 `auths/` 下已有同名前缀文件不受影响；新增渠道各自使用独立 `state-*.json`。
- 账本修复生效后，**新产生**的签到收入会正常入账；早先漏记的历史收入不会追溯补录（上游已不提供可回算的快照）。

---

## 致谢

本版三个新渠道及多数量化修复来自社区贡献：

- **xxhhlk** — MonkeyCode / 商汤小浣熊 / 讯飞 Loomy 三个渠道（#65 / #64 / #63）、月初数据丢失的月份枚举修复（#58 的问题一）、`entries` 排序修复（#66）、scheduler nil-vs-empty 修复（#56）
- **jianzhangg** — 月份枚举与 Trae 系 token 流水修复（#58）
- **ttales430** — 签到收入入账与 Trae 流内错误处理修复（#61）

相关协议逆向成果已由作者开源独立维护：[`xxhhlk/raccoon2api`](https://github.com/xxhhlk/raccoon2api)、[`xxhhlk/loomy2api`](https://github.com/xxhhlk/loomy2api)、[`codkeep/MonkeyCodeReverseEngineer`](https://github.com/codkeep/MonkeyCodeReverseEngineer)。

---

# wild-work v2.6.1 — 运行统计面板（第三 tab）+ 面板两处易用性改进 + 托盘像素图标

> 发布主题：**面板新增「运行统计」tab**（今日收入/花费、平台概览、粘性接棒、临期预警、
> 模型消耗、请求日志、异常账号、运行日志，30s 自动刷新），渠道标签显示余额、
> 汇总条可点击筛选；托盘菜单图标改为像素画。
> 上次 tag v2.6.0 之后累积 11 个提交（含 5 个社区 PR）。

---

## 🆕 运行统计面板（社区贡献，来自 [@ezjbc](https://github.com/ezjbc)）— PR #71 / #72 / #73

面板第三个 tab，8 个区块，全部数据**进程内直调**（无 HTTP 自环、无额外进程/端口）：

| 区块 | 内容 |
| --- | --- |
| 今日情况 | 签到收入 / 今日花费 / 账号数 / 里程碑提示 |
| 平台概览 | 各渠道账号数、余额、今日进出、7 日内到期、token 消耗 |
| 使用中接棒 | 粘性路由当前钉住的账号与已连续成功请求数 |
| 最近临期 | 到期日升序 + 按今日消耗速度外推的预计作废量（风险号标红） |
| 模型消耗 | 按模型的 token / 积分 / 请求数 / TTFB / 吞吐均值 |
| 请求日志 | 最近请求行（模型 / 流式标记 / 状态码 / TTFB / 耗时 / 吞吐） |
| 异常账号 | 停用 / 冷却 / 错误累计，带摘要徽章 |
| 运行日志 | 尾 200 行，15s 刷新 |

- **口径独立**：`internal/stats`（内存 + `data/stats.json` 按日归档）与既有 `internal/ledger`
  （磁盘 JSONL 双流水）**并列存在、互不写入、不追求数字对齐**——「用量与流水」tab 仍是
  ledger 口径，两者各自标注。消耗走**余额下降差值**、收入走**日志签到事件**
  （「登录即自动签到」场景差值恒 0 漏记，故不能用差值）、逐请求 token 走 usage 帧精确值。
- **持久化**：`data/stats.json` 原子写（tmp+rename），跨重启保留按日历史；「今日」计数跨天自动清零。
- **前端健壮性**：30s 常驻轮询；拉取失败时标题显示红色「拉取失败 · 数据停止于 HH:MM:SS」角标而非静默空白。
- 新增端点 `GET /api/stats`、`GET /api/stats/logs?limit=15`（自动过既有 cookie 会话守卫）。

合计 49 个新单测（差值记账 / 事件解析 / 视图 / 持久化 / 端点守卫 / 喂入器）。

## 🎨 面板改进（社区贡献，来自 @ezjbc）

- **积分汇总条可点击筛选**（PR #68）：点某个渠道的汇总 chip → 账号区只显示该渠道卡片，再点取消；
  有筛选时出现「全部」重置 chip。账号多了以后不用再一张张翻。
- **费率面板渠道标签显示余额**（PR #69）：切到某渠道即可看到「余额 N 分」，不用切回账号页自己加；
  **模型名可点击复制**完整 `渠道/模型`，配客户端时直接粘。

## 🖥️ 托盘菜单图标改为像素画

三个菜单项图标由「纯色小方块 + 白边」改为 16×16 手绘像素画，色彩语义不变：

- 🔵 **打开主界面** — 外链图形（方框 + 右上斜箭头）
- ⚪ **查看日志** — 文档图形（白纸 + 折角 + 三条文本线）
- 🔴 **退出** — 电源符号（圆弧 + 顶部竖线）

仍符合「纯 Go 生成、无外部图标文件」的既定约束：不引入 emoji（Win32 菜单走 GDI 渲染，
不支持 COLR 彩色字体，文字里的 emoji 只能画成黑色单色轮廓，还可能出「豆腐块」），
也不为渲染彩色 emoji 引入 DirectWrite 级依赖。16×16 条目经 `LR_DEFAULTSIZE` 16→32 放大、
`DrawIconEx` 32→16 缩小，整数倍最近邻映射，像素画无损复原；透明背景经 32bpp DIB 保留 alpha，
菜单上不会出现黑底。测试逐像素锁死图形语义（去掉图形即失败）。

---

## 📄 文档

- `AGENTS.md` 新增 **R43**：正式确立「`internal/stats` 与 `internal/ledger` 两套统计口径并列，
  零共享状态、不互校、数字天然不同是设计而非缺陷」，并固化「统计输入必须进程内直调、
  禁止 HTTP 自环」与 `StartStatsFeeders` 接线要求（漏接线即静默无数据）。
- 面板/API 文档补「运行统计」tab 与两个新端点；README 补运行统计面板说明与
  **三渠道 Windows 平台限制**（MonkeyCode / 小浣熊 / Loomy 的官方客户端只有 Windows 版，
  小浣熊浏览器授权还依赖 HKCU 协议注册）。

---

## 🐛 修复

- **运行统计测试硬编码日期**：`TestLogWriterObserveEvents` 的日志行写死 `2026/10/06`，
  而引擎只把「今日」事件计入收入 ⇒ 合并次日即必红。改为动态取当日。

---

## ⚠️ 已知边界

- **本版不动 `state-*.json` 格式，无需迁移**；`data/stats.json` 为新增文件，可随时删除
  （删了只是丢掉按日历史，不影响功能）。
- 运行统计与「用量与流水」的数字**本就不同**（前者差值含积分包到期作废，后者已拆
  spend/expire），这是设计而非 bug，不做对齐。
- 运行统计的收入走日志事件口径，依赖 `data/app.log` 的签到日志行格式；上游/本仓改日志
  格式会导致该口径静默归零（引擎内置「松形状探针 vs 严格正则」比对以暴露该情况）。
- 待处理 issue（本版未含）：#67 WorkBuddy 每日签到包被条目差分抵消致当日收入显示 0（ledger 口径）、
  #70/#60 TraeWork 设备号策略（`device-id.json` 全局共享 vs 设备数上限，两者需合并设计）。

---

# wild-work v2.6.2 — 多账号轮换语义修订 + 模型级限流冷却 + TraeWork 设备号/模型目录修复 + 签到收入记账修复

> 发布主题：**账号级错误同请求自动换号**（客户端不再看到本可避免的 429/401）、
> **429 按模型冷却**（同号其它模型不再陪葬）、**长流式卡死看门狗**，
> 以及 **TraeWork 设备号策略修订**（修复 #60/#70）与 **签到收入被差分抵消**（修复 #67）。
> 上次 tag v2.6.1 之后累积 10 个提交（含 7 个社区 PR，全部来自 [@ezjbc](https://github.com/ezjbc)）。

---

## 🔁 账号级错误同请求换号重试（社区贡献 — PR #82，R29 语义修订）

此前账号出错时是「记惩罚 → 本次请求原样失败 → 下次请求才换号」：客户端（agent/CLI）立即重试
又被粘性路由钉回同一池，本可避免的失败原样甩给用户（2026-09-23 生产事故：13 个健康号在场
却返回 429）。本版改为：

- **账号级错误**（限流/欠费/登录态失效/404/5xx/账号故障）在惩罚该账号后**同请求内自动换下一个
  候选重试**，最多轮换 3 次（`MaxRotate`）；坏号照旧被冷却/禁用，后续请求不会再选中它；
- **内容类错误**（内容拦截/上下文超限/请求级拒绝）**仍透传不换号**——换号必然复现，不浪费好号配额；
- **候选全部耗尽时透传最后一个上游原始响应**（保留 6004 重置时间等细节），不再包装成 503；
- 单账号渠道（OpenCodeZen）完全豁免，行为不变；
- **6004 精确冷却**：从限流响应体解析「将在 <ts> 重置」（UTC+8），冷却到真实重置点而非固定 60s，
  消除「过期后被反复选中反复 429」（解析失败回退默认值，24h 上限防异常时间戳把号冻死）。

## 🧊 429 模型级冷却——同号其它模型不再陪葬（社区贡献 — PR #80）

上游 429/6004 的实际限流维度是**模型**（响应体明示「可切换其他模型继续使用」），但旧冷却是账号级：
A 号的 glm 撞每日上限后，A 号还能用的 deepseek/kimi 一起被拖死。本版：

- 429 只冷却「该账号 × 该模型」，同号其它模型继续参与选号；粘性路由对该模型自动让位；
- **短时间内第二个模型也撞墙 → 自动升级整号冷却**（防上游实际按账号计时，逐模型撞墙浪费时间）；
- 冷却状态随 `state-*.json` 持久化（重启不丢，6004 重置窗口可达数小时）；
- 全部账号健康但目标模型在所有账号上都冷却时，503 文案**明确给出最早解冻时刻**
  （「到点自动恢复，无需重新登录，也可先切换其他模型」），不再误导用户去重登；
- `/api/state` 新增 `model_cooling` 字段，面板/调用方可观测。

## 🐕 流中空闲监控——长流式卡死自动断流（社区贡献 — PR #79）

流式请求此前无总超时（防长回答被掐断，R35），但「TCP 连上了、上游却不吐字」的卡死流没有任何
看门狗：客户端无限转圈、日志无线索、连接与 goroutine 永不释放。本版给 9 个流式渠道
（workbuddy/workbuddyai/traework/traecode/qoder/qodercn/qodercom/qwenwork/oczen/glm）
统一挂上空闲监控：**字节间隔超 300s 即主动断流**，客户端拿到明确错误而非无限挂起；
正常流每次收到字节即续命，完全不受影响。各渠道 `IdleTimeout` 字段可调，`<=0` 禁用。

## 🧭 TraeWork 模型目录双池合并（社区贡献 — PR #81）

修复「`/v1/models` 里列得出、一调用就 `4001 param is invalid`」：上游 chat 接口按 `function`
选模型池，而目录接口会一并下发多个池的模型。本版改为双池各拉一遍合并去重，并登记每个模型的
实际归属池，调用时按归属自动覆盖 `function`——**列表里的每个模型都能调通**；另一池拉取失败
只降级不整报错。

## 🔧 TraeWork 设备号策略修订（修复 #60 / #70）

v2.6.0 起设备号以 `device-id.json` 在**本机级**复用，实测带来两个问题：

- **#70**：一机多号共享同一设备号 → 共享的账号撞上游风控 `4017`（15 账号对照：共享组 3 个
  全中、一机一号的 12 个零风控）；
- **#60**：同一编造设备号被多账号先后绑定后，新账号授权恒 `20401 Device limit reached`
  （报错字面「设备数上限」极具误导性，实际与真实设备数无关）。

本版把设备号作用域改为**登录会话**：每次「+ TraeWork」生成新设备号，登录成功后随
`auths/trae-<uid>.json` 落盘成为**该账号的稳定注册设备**（签到/对话头从 auth 文件读，
不受影响）；登录成功后自动删除遗留的 `device-id.json`，**老用户升级即自愈，无需手工处理**。

> 权衡披露：同一账号重新登录会消耗一个设备名额（同账号上限 10 台），但消耗速度远低于旧随机版
> （旧版每点一次登录消耗一个）。

## 💰 签到收入记账修复（修复 #67）

WorkBuddy 每日签到包落在「明日到期」的资源 key 上且提前一天以 0 余额建档——「发放即消耗」场景
下条目差分 `delta=0`，签到发放被当成「条目没变」，**面板「今日收入」恒为 0**（签到日志明明
全部 ok=true）。两层修复：

- **权威入账**：签到回执自带本次发放额（`credit` 字段），直接入账 earn，不再依赖差分时序；
- **差分兜底**：快照增加已用量字段，「发放即消耗」（余额持平、已用量上涨）按「发放额 = 已用增量」
  补记 earn；已用量恒 0 的渠道零影响。

---

## 🛠 其它修复（社区贡献 — PR #76 / #77 / #78）

- **#78（traework 错误分类顺序）**：429/401 的响应体也会携带业务码 1005（硬余额），
  旧判定顺序把限流误判成硬余额触发 12h 硬冷却、把登录失效误判后绕过自愈重登。
  现状态码优先，与 qodercn/qodercom/qwenwork 对齐。
- **#77（traework 定价目录 8MB）**：定价目录读取上限 1MB→8MB（对齐模型目录），超限显式报错
  而非截成半截后报误导性语法错误。
- **#76（webui 减噪）**：移除「用量与流水」tab 标题的「已记录 N 天」角标，窄窗口不再折行。

---

## 📄 文档

- **R29 修订**：多账号轮换语义从「错误下次生效」改为「错误惩罚照旧 + 账号级错误同请求换号」，
  固化 Rotatable 错误族划分与候选耗尽透传规则；
- **R37 二次修订**：设备号作用域 = 登录会话（非本机、非官方客户端值），
  记录 #60/#70 证据链与「不读官方客户端真实设备号」的理由；
- **R17 补丁**：签到发放权威入账（`CheckinGranter` 可选接口模式）与差分 `used` 补丁双层防线。

---

## ⬆️ 升级说明

- **无需迁移**：`state-*.json` 只增字段（模型级冷却旧文件零值安全）；
  ledger 快照新增字段为可选（旧快照无此字段按 0 处理）。
- **TraeWork 用户**：升级后直接使用即可；此前因 20401/4017 加不进或被风控的账号，
  重新「+ TraeWork」登录一次即可恢复（新会话设备号）。
- 遗留的 `data/device-id.json` 将在下一次 TraeWork 登录成功后自动删除，也可手工删除（无凭据价值）。

---

## 🐛 修复清单（对照 issue）

| Issue | 问题 | 状态 |
|---|---|---|
| #60 | TraeWork 加新账号恒 20401 | ✅ 本版修复（设备号会话级） |
| #67 | 面板今日签到收入恒 0 | ✅ 本版修复（权威入账 + 差分补丁） |
| #70 | 共享设备号致 4017 风控 | ✅ 本版修复（同 #60） |

---


---

# wild-work v2.6.3 — 小浣熊跨平台导入与会话自愈 + 模型查找面板 + 费率/明细易用性

> 发布主题：**小浣熊 macOS/Linux 可用**（移除导入 Windows 门禁 + 客户端会话自愈）、
> **模型查找面板**（全渠道按关键字定位）、**积分明细/费率表易用性**，
> 以及订阅的仓库全量测试转绿（CDP 挂死断言、ledger 日期炸弹）。上次 tag v2.6.2 之后累积 9 个提交
> （含 4 个 GitHub 合并 PR #86/#88/#89/#90 与 1 个 cherry-pick PR #85，来自
> [@bertchiangchiang](https://github.com/bertchiangchiang) 与 [@wu546526](https://github.com/wu546526)）。

---

## 🦝 小浣熊跨平台导入 + 客户端会话自愈（修复 #75 / 社区贡献 PR #85）

### macOS / Linux 导入放开（修复 #75）

此前「从本机客户端导入」有一个**整体 Windows 门禁**（`runtime.GOOS != "windows"` 一票否决），
但小浣熊官方其实有 macOS / Linux / 银河麒麟客户端，且其凭据路径探测（`~/.box-agent/config/auth.json`、
`BOX_AGENT_CONFIG_DIR` 覆盖）本身**跨平台**。本版：

- 移除 `import_local.go` 的整体 Windows 门禁，改由各渠道路径候选自行探测、找不到时返回明确报错；
- Raccoon 用 `os.UserHomeDir()` 天然跨平台；Loomy/MonkeyCode 以标准环境变量候选为主、尽力而为；
- 真正仅 Windows 的只有**小浣熊浏览器授权登录**（依赖 HKCU `office-raccoon://` 协议注册），
  macOS/Linux 请走「从客户端导入」或手动放置客户端凭据后导入。

### 客户端会话自愈（社区贡献 PR #85）

小浣熊上游对 refresh_token 做轮换，而客户端与 wild-work **共用同一会话**——客户端任一次续期都会
轮换 refresh_token，令本工具落盘的旧值立刻失效（refresh 401、失败累积触发账号自动停用）。本版：

- refresh 为空、或 refresh 被拒（401/403）时，从本机客户端会话文件 `~/.box-agent/config/auth.json`
  **实时取最新活凭证**自愈，不再需要重新导入；
- **uid 匹配才采纳**（客户端登着谁的号就只救谁，绝不把 A 账号令牌写进 B 账号）；
- 客户端 token 已过期（客户端登出残留）时不采纳，避免把必然失败的请求延迟到下一次 401。

## 🔍 模型查找面板（前端）

费率表标题行新增放大镜按钮，弹出查找对话框：从最近一次 `/api/fees` 结果**展平全渠道模型**，
按关键字（模型名 / 渠道 / 完整 ID）即时过滤，结果点击 / 回车**复制完整模型 ID**（客户端该填的名字）。
固定高度 + 上限 12 条 + 底部提示命中/未显示数，避免对话框随结果跳变。

## 🎨 积分明细 & 费率表易用性（社区贡献 PR #89 / #90）

- **#90 积分明细浮窗**：三处修复——① 异步返回无竞态守卫导致的「幽灵浮窗」显示错账号；② 切主 tab
  后浮窗残留其它页签；③ 临期额度与后端 `ExpiringWithin` 同口径标红（整行淡红 + 到期日红字 + 临期红章），
  并按「临期 → 可用 → 已用完/不可用垫底 + 到期日升序」排序。
- **#89 费率表模型单元格双行**：上行短名 + 上下文/能力/促销标记，下行**完整模型 ID**（裸名自动拼
  渠道前缀）一键复制，规避长 ID 与标记挤一行、易误点。

## 🧰 全量测试修复（社区贡献 PR #86 / #88）

- **#86**：CDP「挂死」断言窗口 12s → 20s（对齐 `writeTimeout=15s`），修掉一个必然失败的测试。
- **#88**：ledger 测试「日期炸弹」修复——签到用例硬编码日期已过期、跨月窗口用例每月固定红一个月；
  改为长期有效日期 + `今天−6天` 窗口起点，master `go test ./internal/ledger/` 转绿。

---

## 🛠 其它修复

- **#75 门禁移除**（见上行）；提交 `dc603db`。

---

## 📄 文档

- **RELEASE 说明整合**：14 个分版本 `RELEASE-vX.Y.Z.md` 并入统一 **`RELEASES.md`**（按版本从旧到新，
  未来发版 append 到末尾），删除旧分版本文件；AGENTS.md 发版流程同步为引用 RELEASES.md。
- **README**：删除过时的 FAQ（TraeWork「不可用」、DeepSeek V4 Flash 响应慢），新增
  「小浣熊在 macOS / Linux 上能用吗」跨平台说明（含仅 Windows 受限的浏览器授权、MonkeyCode/Loomy 平台情况）。

---

## ⬆️ 升级说明

- **无需迁移**：无状态文件格式变更。
- **小浣熊用户**：升级后 macOS / Linux 可正常「从客户端导入」；已在用的账号若因客户端续期被停用，
  后端会在 refresh 时自动从客户端会话自愈，无需手工处理。

---

## 🐛 修复清单（对照 issue）

| Issue/PR | 问题 | 状态 |
|---|---|---|
| #75 | 小浣熊 macOS/Linux 无法从客户端导入 | ✅ 本版修复（移除 Windows 导入门禁） |
| #85 | 客户端续期轮换致 refresh 失效、账号被停用 | ✅ 本版修复（客户端会话自愈） |
| #90 | 积分明细 tooltip 竞态/残留 & 临期不醒目 | ✅ 本版修复（社区 PR） |
| #88 | ledger 测试每月固定红 | ✅ 本版修复（社区 PR） |
| #86 | CDP 挂死断言必然失败 | ✅ 本版修复（社区 PR） |