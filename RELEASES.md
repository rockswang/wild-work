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

### UI 打磨（PR #121，R66 延续）

- **上下文/输出支持 k、M 单位**：配置可写 `"1M"`、`"64k"`、`128k` 或纯数字，面板表格按 `1M`/`64k` 展示。
- **补齐「工具」能力**：模型可声明 `supports_tools`（缺省 = 支持），`/v1/models` 如实下发。
- **紧凑单行布局**：模型表格固定列宽对齐、➕ 图标按钮、任一输入框回车即可添加。
- **合并期修复三条**：① 字符串形态模型（`["m"]`，最常用简写）的 `SupportsTools` 原会落成 `false`，
  `/v1/models` 静默不带 `supports_tools`；② 显式 `"supports_tools": false` 因 `bool`+`omitempty` 落盘即丢
  （读回变 true）；③ 单位串写错（如 `"1OO"`）原先静默落 0，现改为报错指出渠道与字段。

## 🐛 修复清单（对照 issue）

| Issue | 问题 | 状态 |
|---|---|---|
| #60 | TraeWork 加新账号恒 20401 | ✅ 本版修复（设备号会话级） |
| #67 | 面板今日签到收入恒 0 | ✅ 本版修复（权威入账 + 差分补丁） |
| #70 | 共享设备号致 4017 风控 | ✅ 本版修复（同 #60） |

---


---

# wild-work v2.6.3 — 小浣熊跨平台导入与会话自愈 + 模型查找面板 + 费率/明细易用性

> 发布主题：**小浣熊跨平台可用**（移除导入 Windows 门禁 + 客户端会话自愈 + **登录改用网页版**）、
> **模型查找面板**（全渠道按关键字定位）、**积分明细/费率表易用性**，
> 以及订阅的仓库全量测试转绿（CDP 挂死断言、ledger 日期炸弹）。上次 tag v2.6.2 之后累积 9 个提交
> （含 4 个 GitHub 合并 PR #86/#88/#89/#90 与 1 个 cherry-pick PR #85，来自
> [@bertchiangchiang](https://github.com/bertchiangchiang) 与 [@wu546526](https://github.com/wu546526)）。

---

## 🌐 小浣熊登录改用「网页版」（本次补发内容）

此前小浣熊的「浏览器授权登录」依赖改写 Windows 注册表（HKCU 的 `office-raccoon://` 协议回调），
**仅 Windows 可用**，且要求本机装有官方客户端。本次补发改用**网页版流程**：

- 点击「＋ 小浣熊」→「登录」后，拉起一个**独立 profile 的 Edge/Chrome**（不影响你日常浏览器），
  打开小浣熊网页版登录页；你在其中正常登录（扫码 / 手机号 / 微信均可）后，
  本工具经 CDP 自动捕获凭据并保存账号——**跨平台**（Windows / macOS / Linux 通用），
  不再依赖注册表，也不要求本机装客户端。
- 判据是凭据 JWT 里的 `owner_type == users`：小浣熊对**未登录访客也会下发 Cookie**，
  若以「Cookie 出现」为准会抓到一个不可用的访客凭据（此坑与智谱清言同源）。
- ⚠️ **登录态互踢**：上游刷新时会轮换 `refresh_token`。若同一账号的凭据被本工具与
  浏览器 / 官方客户端**共用**，两边会争抢同一个 `refresh_token`，可能出现其中一侧被挤下线。
  要两边同时用，请用「网页版登录」在独立浏览器窗口里**单独登录一次**（本流程新建独立会话），
  而不是从浏览器复制凭据粘进来。弹窗内已就此给出提示。
- 「从客户端导入」路径**保留不变**（不想新开浏览器时的替代方案）。
- 顺带修正刷新端点：实测 `/api/electron/auth/v1/refresh` 已 404，改为**先试**
  `/api/web/auth/v1/refresh`、404 才回落旧前缀；`access_token` 实测寿命约 1 小时（原注释写 2h）。

---

## 🦝 小浣熊跨平台导入 + 客户端会话自愈（修复 #75 / 社区贡献 PR #85）

### macOS / Linux 导入放开（修复 #75）

此前「从本机客户端导入」有一个**整体 Windows 门禁**（`runtime.GOOS != "windows"` 一票否决），
但小浣熊官方其实有 macOS / Linux / 银河麒麟客户端，且其凭据路径探测（`~/.box-agent/config/auth.json`、
`BOX_AGENT_CONFIG_DIR` 覆盖）本身**跨平台**。本版：

- 移除 `import_local.go` 的整体 Windows 门禁，改由各渠道路径候选自行探测、找不到时返回明确报错；
- Raccoon 用 `os.UserHomeDir()` 天然跨平台；Loomy/MonkeyCode 以标准环境变量候选为主、尽力而为；
- 小浣熊的**登录**路径亦已跨平台（见上节「登录改用网页版」），不再有仅 Windows 的功能。

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
  「小浣熊在 macOS / Linux 上能用吗」跨平台说明；本次补发同步更新登录方式与互踢提示
  （MonkeyCode / Loomy 平台情况不变）。

---

## ⬆️ 升级说明

- **无需迁移**：无状态文件格式变更，凭据文件格式不变（新旧凭据通用）。
- **小浣熊用户**：升级后 macOS / Linux 可正常「从客户端导入」；已在用的账号若因客户端续期被停用，
  后端会在 refresh 时自动从客户端会话自愈，无需手工处理。
  已添加的账号无需重登；新添加账号默认走**网页版登录**（不再需要本机装客户端）。

---

## 🐛 修复清单（对照 issue）

| Issue/PR | 问题 | 状态 |
|---|---|---|
| #75 | 小浣熊 macOS/Linux 无法从客户端导入 | ✅ 本版修复（移除 Windows 导入门禁） |
| #85 | 客户端续期轮换致 refresh 失效、账号被停用 | ✅ 本版修复（客户端会话自愈） |
| #90 | 积分明细 tooltip 竞态/残留 & 临期不醒目 | ✅ 本版修复（社区 PR） |
| #88 | ledger 测试每月固定红 | ✅ 本版修复（社区 PR） |
| #86 | CDP 挂死断言必然失败 | ✅ 本版修复（社区 PR） |
| — | 小浣熊登录仅 Windows 可用、需装客户端、与客户端抢注册表 | ✅ 本版修复（登录改用网页版 CDP 捕获，跨平台） |
---

# wild-work v2.7.0 — Cline 免费模型渠道 + 模型思考档位透传 + 缓存利用率 + 面板易用性

> 发布主题：**新增 Cline 渠道**（官方 `sk_` API Key 免费池，issue #62）、**模型思考档位透传**
> （上游声明才展示，issue #96）、**Token 用量缓存利用率列**（区分「命中 0%」与「未知」）、
> 以及**积分明细同源回写快照**、**空壳 assistant 消息修复**等一批修复与易用性改进，
> 并完成**手机竖屏响应式 UI**。
> 上次 tag v2.6.3 之后累积 19 个提交 + 3 个合并 PR（#92/#93/#94，来自
> [@bertchiangchiang](https://github.com/bertchiangchiang) 与 [@xxhhlk](https://github.com/xxhhlk)）。

---

## 🆕 新渠道：Cline 免费池（`cline/*`，修复 #62）

新增 **Cline 渠道**：粘贴官方 **API Key** 即用，无需在本机安装 Cline 客户端、零登录编排。

- **凭据**：官方文档化的 `sk_` API Key（`app.cline.bot` → 注册账号 → `Account / API Keys` →
  `Create API Key`）。Key 不过期、不轮换、不与浏览器/客户端互踢。一个 Cline 账号一把 Key。
- **额度按账号计**：官方字段 `…UsageCostUSDPerUser`，同账号多把 Key 共享一份额度。面板会自动识别
  同账号多把 Key 并告警。上游确无余额/额度端点（12 个候选实测全 404），故积分位显示「不适用」，
  实际花费来自逐请求 `usage.cost`（入 ledger）。
- **只调度免费模型**：绑死上游 `recommended-models` 的 `free[]` 数组原样透传（ID 保留 `cline-free/*`
  别名不改写），本地不设任何静态表/假设。本地校验集 = 暴露集，冷启动放行。
- **模型实测能力表**：上游 `free[]` 不返回上下文/能力字段，本渠道维护 `measuredCaps` 实测表——
  上下文按实测向上取一档（mimo/muse/step-5 报 1M、solar 报 512K），图像能力实测标注，tool_calls 实测通过。
- **关键实现约束**：出站必带非空 `X-CLIENT-TYPE` 头（缺失 ⇒ 全部免费模型 403 且不报错）；
  发往上游的 body 强制 `stream:true`（否则非流式请求 502 并冷却整条渠道）；地区限制模型归
  `ErrPassthrough` 不罚号；429 分两类处置（OpenRouter 共享出口上限不软化账号、账号级才冷却换号）；
  `max_tokens` 缺省不传、<64 抬到 64、>200000 压到 200000。
- 面板 Cline 添加弹层展示实测的免费模型上下文/能力表，并订正开卡路径话术（入口是
  `Account / API Keys` 而非 Settings）。

## 🧠 模型思考档位透传（issue #96）

**只透传上游声明，绝不编造**：上游模型目录里声明了可调档位才展示，未声明时整个 reasoning 对象不出现。

- `provider.ModelInfo` 新增 `ReasoningEfforts` / `DefaultReasoningEffort` / `CanDisableThinking`；
  `/v1/models` 新加 `supports_reasoning` + `reasoning{supported_efforts,default_effort,can_disable_thinking}`，
  `/api/fees` 新加 `reasoning_efforts` / `default_reasoning_effort`。
- 接线渠道：WorkBuddyCN、WorkBuddyAI（国际版）、Loomy（上游 `reasoning_efforts` 权威）。
  deepseek 系只有老式 `effort` 默认档时回退取它；mergeV3 非空者优先。
- **清洗**：`provider.CleanEfforts` 去空白/去重/丢空串，全部无效归 nil（上游实测下发过
  `"max","max",""` 这种脏列表）。
- 面板费率表模型 ID 后的 `[low·high·max]` 紫色小字标 + 模型搜索框同款，tooltip 标注默认档；
  客户端传 `reasoning_effort` 即可调档。
- qoder 系 / qwenwork / glm / raccoon / oczen / cline / monkeycode 上游无档位声明，不参与。

## 🗂 积分明细同源回写 pool 快照（R45，PR #94）

面板「同屏两个数字对不上」——账号卡片/汇总条的积分与 hover 明细弹层的数字时不时的差一点。
根因是**时差**：pool 快照只被「刷新事件」更新，明细是 hover 时的实时值，两者必然相差区间内消耗量。

- `creditSnapshot` 取数成功即 `SetCreditDetail` + ledger 差分（`creditTotals` 与 `ResourceDetail` 共用，
  删掉三处各自回写）；`/api/account/resource_detail` 新增 `expiring_remain`。
- 前端 `showCreditDetail` 用响应回写卡片与汇总条，签到/刷新成功后清 `detailCache`。

## 🧹 空壳 assistant 消息补 content（R48，PR #95）

CodeBuddy 在「思考中途被打断」时会存下 `{"role":"assistant","reasoning":…}` 的空壳消息；
留在历史里会让 traecode/SOLO 对**每一发**请求都回流内 `event:error` `4001 the param is invalid`
（重启客户端也不恢复，只能新建会话）。本版：`normalizeToolTurnContent` 对 `assistant`/`tool` 一律补
`content:""`（不再以 tool_calls 为前提），reasoning 字段保留不误删。

## 📱 手机竖屏响应式 UI

- 移动端竖屏响应式布局：账号卡片、费率表、渠道按钮等组件在窄屏下自动重排，不再溢出；
  触控目标仅在粗指针设备放大（桌面鼠标不吃这套）。
- 顶栏 `1fr` 下界解除（`minmax(0,1fr)`）、账号卡片允许收缩、费率表窄屏隐藏第 3/4 列、
  运行统计宽表给横向滚动容器。
- 积分明细浮窗改为**点击式**（X / Esc / 点外关闭），手机不再依赖 hover。
- 渠道按钮 6 列两行 + 紧凑档字号（Cline 移到第一行末位、保留列配对）；修复顶栏 650–750px 溢出带。

## 🛡 其它合并（PR #92 / #93 / #94）

- **#92 stream passthrough single account**：单账号渠道流透传（stream passthrough）。
- **#93 import macos**：macOS 客户端导入路径补全。
- **#94 credit detail snapshot sync**：见上行「积分明细同源回写快照」。

---

## ✨ 新功能：「缓存利用率」列（原 v2.6.4 草稿内容）

「用量与流水 → Token 用量」的模型表新增**缓存**列，并在汇总卡区新增**缓存利用率**卡片；
「运行统计 → 模型消耗」表同步新增该列。数值口径为

```
缓存利用率 = 缓存命中 token ÷ 「上报了缓存字段」的请求的 prompt token   （加总口径）
```

**为什么是加总而不是逐请求平均**：逐请求平均会让「一个 100k prompt 命中 95%」与
「十个 100 token 全部未命中」等权，与用户真正关心的「省了多少 token」不符。

**哪些渠道有数**（2026-10-12 逐渠道实测，24 份真实上游快照）：

| 状态 | 渠道 | 上游字段 |
|---|---|---|
| ✅ 有数 | WorkBuddyCN / WorkBuddyAI | `prompt_cache_hit_tokens`（实测命中 1218/1283 ≈ 94.9%） |
| ✅ 有数 | TraeWork / TraeCode | `cache_read_input_tokens` |
| ✅ 有数 | QoderCN / QoderCOM / 千问办公 / Cline / OpenCodeZen | `prompt_tokens_details.cached_tokens` |
| ✅ 有数 | MonkeyCode | `input_tokens_details.cached_tokens`（本版修复：此前转换时被丢弃） |
| ➖ 显示「—」 | 小浣熊 / 智谱清言 / Loomy | 上游不下发该维度 |

## 🎯 不支持的渠道如何降级（本版的设计重点）

**核心原则：把「命中率为零」与「不知道」严格区分开**——两者混同会误导用户去优化一个
上游根本不回报的指标。实现上有两层：

1. **取数层**（`internal/server/cacheusage.go`）把「字段存在但为 0」与「字段不存在」
   判成两种结果。特别地，小浣熊下发的是**空对象** `prompt_tokens_details:{}`，
   即「没这个维度」而非「命中 0」——若只判「有 details 对象就算上报」，面板会给它
   恒显 0%。
2. **展示层**（`app.js` 的 `cacheCell`）三态渲染：有上报样本 → 百分比（允许 0.00%，
   那是真未命中）；无上报样本或字段整体缺失（老流水）→ **「—」**，且不带任何百分比。

**统计本身完全降级**：未上报的请求**整条不进缓存列的分母**，不影响它原有的 token
统计与流水。故新列对任何渠道都不会「显示错数」，最坏情况只是「—」。

## 🔧 顺带修复

- **MonkeyCode 缓存字段被丢弃**：本渠道是唯一「重建 usage」的渠道（只拼
  prompt/completion/total），上游给的 `input_tokens_details.cached_tokens` 在网关内部
  就没了。现已显式搬运并回填为 OpenAI 形状的 `prompt_tokens_details.cached_tokens`。
- **Cline 非流式请求会冷却整条渠道**：本渠道把客户端 body 原样转发，客户端发
  `stream:false` 时上游回非流式 JSON，而 handler 对上游响应一律按 SSE 解析 ⇒ 502
  `upstream_parse` ⇒ 被当作账号连续错误冷却整条渠道约 60 秒。现已强制上游
  `stream:true`（非流式客户端由本地 Aggregate 聚合，与 OpenCodeZen/MonkeyCode 同款）。

## 🛠 开发期上游取证工具（devdump）

新增 `internal/devdump`：把各渠道出站对话请求的**上游原始响应**落盘，供离线分析
上游到底回了哪些 usage 字段。**两层开关确保发布版零开销**：整包被构建标签切成两半
（无 `-tags devdump` 时退化为常量桩），且需同时设置环境变量 `WILDWORK_DUMP_DIR`
才真正落盘。检验方式：发布版二进制里 `WILDWORK_DUMP_DIR` / `dumpTransport` 字符串计数为 0。

```bash
export WILDWORK_DUMP_DIR="$PWD/temp/dump"
go build -tags devdump -o dist/wild-work-devdump.exe ./cmd/wild-work
./dist/wild-work-devdump.exe --no-tray
bash scripts/probe-dump.sh run && bash scripts/probe-dump.sh summarize
```

快照含 access token 等敏感头，仅限本机分析。

## ⬆️ 升级说明

- **无需迁移**：流水新增的 `cr`/`cr_cov` 字段均为 omitempty，未上报时不写出——
  与旧版本产出的行**逐字节相同**，旧流水读出来即「未上报」，面板显示「—」。
- **历史数据**：缓存列只统计升级后的新请求（老流水没有缓存字段，无数据可算）。

---

## 🐛 修复清单（对照 issue）

| Issue/PR | 问题 | 状态 |
|---|---|---|
| #62 | 希望把 Cline 合入项目（新增渠道） | ✅ 本版新增（Cline 免费 API Key 池） |
| #96 | 模型列表希望显示模型支持的思考等级 | ✅ 本版新增（思考档位透传） |
| #94 | 明细与刷新路径不同源、面板同屏两个数字对不上 | ✅ 本版修复（积分明细回写快照） |
| #95 | 空壳 assistant 消息致 traecode/SOLO 4001 | ✅ 本版修复（补 content:""） |
| #92 | 单账号渠道流透传 | ✅ 本版合并（PR #92） |
| #93 | macOS 客户端导入路径 | ✅ 本版合并（PR #93） |
| — | Token 用量表缺缓存利用率维度、MonkeyCode 缓存字段被丢弃 | ✅ 本版修复（缓存列 + devdump 取证） |
| — | Cline 非流式请求 502 冷却整条渠道 / 漏 X-CLIENT-TYPE / 地区限制误罚号 | ✅ 本版修复（forceStream + 头强制 + 分类豁免） |

# wild-work v2.7.1 — 思考链死循环检测（防癫拦截）+ trae 系 usage 缓存修复 + 面板改进

> 发布主题：**思考链死循环（发癫）检测**——deepseek v4.1 flash 等模型偶发思维链复读/空转，
> 单块思考几十万字符、烧光额度。本版在抽象层**实时逐帧检测**推理增量，命中即中断 SSE、
> 向客户端返回错误，由下游 AI 客户端（Claude Code / Codex 等）自行重试恢复。
> 同时合入四个社区 PR：**OpenRouter 系思考字段归一化**（修 Cline 免费池思考被静默丢弃，
> 兼修同渠道防癫守卫失效）、**面板四项易用性改进**（费率独立 tab / 用量平均速度列 /
> 区间记忆 / 五卡同行）、**费率完整 ID 双层路径修复**（修 Cline 免费档复制出的 ID 不可用）、
> 以及 **TDZ 回归修复**（修 PR #101 引入的面板空白），分别来自
> [@jianzhangg](https://github.com/jianzhangg) 与 [@bertchiangchiang](https://github.com/bertchiangchiang)
> （含 PR #105 统计重启缓存丢失修复）。
>
> 本版另有**两项自行修复**：**trae 系 usage 缓存字段归一化**（issue #104，
> 修下游命中率恒 0%、费用高估约 50 倍）与**防癫检测误伤修复**（agent 在思维链里
> 正常写代码被误杀）。
>
> ⚠️ 本版为**同版本重发**（v2.7.1），已知旧包不含后续修复。

---

## 🔧 已入库的社区 PR（来自 [@jianzhangg](https://github.com/jianzhangg) 与 [@bertchiangchiang](https://github.com/bertchiangchiang)）

| PR | 内容 |
| --- | --- |
| #100 | **OpenRouter 系思考字段归一化**：Cline 免费池（`cline/*`，上游 Cline→OpenRouter→Meta）实测思考增量的字段名是 `delta.reasoning` / `delta.reasoning_details`，**不是**本仓库历史唯一形态 `delta.reasoning_content`。旧白名单只认后者 ⇒ 思考文本被**静默丢弃**（客户端看不到思考、`usage.reasoning_tokens` 却非零），且**同渠道防癫守卫永远喂不进**（判据只读 `reasoning_content`）——同一根因的两面。新增 `upstream.reasoningDeltaOf` 归一化到 `reasoning_content` 单字段（优先级 `reasoning_content` > `reasoning` > `reasoning_details`，`encrypted` 密文不取），接线流式逐帧 + 非流式聚合两路；`loopguard.ReasoningDeltaOf` 同步兼容三字段 |
| #101 | **面板四项易用性改进**：① 模型列表和费率升为**第四个主 tab**（原独立区块移入）；② Token 用量表新增**平均速度**列（tok/s · TTFB，取自运行统计引擎同源数据）；③ 用量区间（今日/7日）**localStorage 记忆**，刷新不再重置；④ 用量汇总五卡片**同一行**显示 |
| #102 | **费率完整模型 ID 一律补渠道前缀**：上游 ID 自带命名空间（如 Cline 免费档的 `cline-free/*`）时，旧逻辑把「ID 含 `/`」误当「已带渠道前缀」，复制出来的是 `cline-free/mimo-v2.6-flash`——服务端按第一个 `/` 拆渠道 ⇒ `provider "cline-free" is not configured`。改为**一律补前缀**（仅当已以渠道前缀开头时原样保留，防重复），与 `/v1/models` 口径对齐，得到可用双层路径 `cline/cline-free/*` |
| #103 | **修 PR #101 引入的 TDZ 回归**：`usageDays` 用 `let` 声明在 `init()` 之后，而 `bindUsage()` 在 init 顶部引用它 ⇒ 暂时性死区抛 `ReferenceError`、整个 init 中断、账号/费率面板**全空白**。修法：变量声明块移到 `init()` 之前（IIFE 立即执行改普通赋值 + try/catch）；顺带把「今日/7日」高亮收敛到单一入口 `syncUsageRangeActive()`，消除初始化/点击/异步返回三处 toggle 竞态 |

---

## 🧠 思考链死循环检测（防癫拦截）

新增 `internal/loopguard` 模块，算法移植自 [dsh-attention-health](https://github.com/donghangxunlang-cmd/dsh-attention-health)
的 `lib/guard.js`（只取死循环检测，忽略健康评估/工作交接/会话日志）。

- **四条判据**（阈值采用 DSH 在 79+ 真实会话上标定的值，宁可漏报不误杀）：
  - 同行连续重复 ≥8 次（行长 ≥12 字符）；
  - 短行高频交替：最近 60 个 ≤11 字符短行里同一短行 ≥30 次；
  - 滑动窗口空转：最近 40 行不同行占比 ≤25%；
  - 行内周期复读：周期 2–40 字符，重复 ≥16 次且累计 ≥160 字。
- **防误杀护栏**：周期单元须含字母/汉字（纯符号分隔线、纯数字命令参数不算）、
  十六进制字节周期豁免、代码围栏内豁免。
- **全渠道覆盖**：WorkBuddy CN/AI、TraeWork/TraeCode（deepseek-v4.1-flash 所在）、
  Qoder 系、千问办公、MonkeyCode、小浣熊、Loomy、OpenCodeZen、Cline。
- **命中行为**：下发一帧 OpenAI 规范错误（`code=upstream_reasoning_loop`）、不补 `[DONE]`、
  错误透传给客户端；**不罚账号**（死循环是模型级问题，换号必然复现，冷却只会误伤整条渠道）。
- **零误伤兼容**：只在 `reasoning_content` 非空时喂入，无思维链的模型/配置不经过守卫。
- **配置开关**（默认开启）：`config.features.loop_guard` + 面板「设置 → 思考链防癫」开关
  （热切换无需重启）+ 环境变量 `WILDWORK_LOOP_GUARD=0` 可关闭。

> 注：本版未做实盘触发验证（死循环难以稳定复现），先行发版交由社区实测反馈，
> 阈值/判据后续按反馈校准。

---

## 🐛 修复清单（对照 issue）

| Issue/PR | 问题 | 状态 |
|---|---|---|
| — | 模型思维链死循环烧光额度，无拦截手段 | ✅ 本版新增（思考链防癫检测） |
| PR #100 | Cline 免费池（OpenRouter 系）思考文本被静默丢弃，防癫守卫在该渠道完全失效 | ✅ 本版修复（思考字段归一化） |
| PR #101 | 费率区块埋没在页面底部 / 用量表无速度列 / 区间刷新重置 / 五卡片换行 | ✅ 本版改进（独立 tab + 速度列 + 区间记忆 + 五卡同行） |
| PR #102 | Cline 免费档复制出的完整模型 ID 缺渠道前缀，请求报 `provider "cline-free" is not configured` | ✅ 本版修复（完整 ID 一律补前缀） |
| PR #103 | PR #101 引入 TDZ 致 init 中断，账号/费率面板全空白 | ✅ 本版修复（声明前移 + 高亮单一入口） |
| PR #105 | 每次重启后运行统计「模型消耗」缓存列大面积回退成「—」 | ✅ 本版修复（缓存三项随 stats.json 持久化） |
| — | PR #102 只修了费率表、漏了**模型查找面板**（同一 `raw.includes("/")` 误判，从搜索框复制的 cline ID 仍不可用） | ✅ 本版一并修复（两处口径统一 + 静态回归） |
| #104 | trae 系（`traework/*`、`traecode/*`）usage 未归一化：缓存命中信息丢失，下游命中率恒 0%、费用高估约 50 倍 | ✅ 本版修复（内层补齐 OpenAI 形状 + 三接口取数收敛为唯一真源） |
| — | 防癫检测误伤：agent 在思维链里正常写代码（60 行里 30+ 行 `}`）被 short-line-repeat 掐断 | ✅ 本版修复（围栏豁免扩展 + 结构行豁免） |

---

## 🔄 trae 系 usage 缓存字段归一化（issue #104）

**现象**：经网关使用 `traework/*`、`traecode/*` 时，下游客户端**拿不到任何缓存命中信息**——
命中率恒 0%、所有 prompt token 按「未缓存输入」计价（按常见价表 input 0.165 vs cacheRead 0.0033 计，**费用高估约 50 倍**），而 `workbuddy/*` 一切正常。

**根因**：trae 上游的 token_usage 用 **Anthropic 命名**（`cache_read_input_tokens` /
`cache_creation_input_tokens`），内层原样透传；下游按 OpenAI 规范解析
（实测读法 `prompt_tokens_details?.cached_tokens ?? prompt_cache_hit_tokens ?? 0`）两条都命中不了。

**修复**：在**内层补齐 OpenAI 形状**（而非让下游兼容 Anthropic——后者要改所有客户端）：

- 新增 `provider.NormalizeUsageShape`：补 `prompt_tokens_details.cached_tokens` /
  `cache_write_tokens`，**保留原始字段**（不破坏已有依赖）、**只在缺失时写入**、
  **认「字段存在」而非「值 > 0」**（0 是「明确未命中」的有效值）。
- 接线 traework 的**流式**（在 token_usage 捕获点，覆盖回调/返回值/异常收尾三条路径）
  与**非流式**两个出口。
- **取数收敛为唯一真源** `provider.ExtractUsageCache`：此前 `/v1/responses`、`/v1/messages`、
  面板**各写一套优先级且已开始漂移**（转换器先读在 workbuddy 系恒 0 的噪声字段，
  且用 `> 0` 判定会把「明确未命中」丢成「未提供」）。
- **`/v1/messages`（Claude Code 路径）语义换算**：Anthropic 的 `input_tokens` 是
  「未命中且未写入」的部分，客户端自行相加还原总输入；而内层 `prompt_tokens` 已是总输入。
  故 `input_tokens = prompt_tokens − cached − write`——不下发缓存字段（费用高估）与
  下发了却不减（客户端求和近两倍）都是错的。三段相加恒等于原 `prompt_tokens`。

---

## 🛡 防癫检测误伤修复（R50 修订）

**现场**：agent 在思维链里正常生成 Go 代码，60 行里 30+ 行是 `}`，被短行高频判据
误判为「空转」而掐断流。根因是 DSH 原实现为**思维链散文**标定，未预见「思维链里写代码」。

**修复（两层）**：

- **围栏豁免从 ④ 扩展到 ②③**：原实现只豁免行内判据，围栏内代码的 `}` 短行照样进
  短行/唯一率窗口被误杀；现围栏内的行不进 ②③ 窗口（① 同行连续保持不豁免——
  连续 8 行相同长行即使围栏内也几乎必是发癫）。
- **结构行豁免**：兜住无围栏裸代码（DeepSeek 系思维链常不挂围栏、fence 状态机管不到）——
  trim 后无字母/数字的行（`}` `{` `});`）与无空白/标点的连续词行（`break`/`return`）
  不进 ②③ 窗口；发癫短句必含标点或空白（「做。」、`ok then.`）不受影响。

已知残留风险：`else {` 这类带结构符号的关键字组合仍计窗（密度低，遇实锤再收）。

---

## 🙏 致谢

本版含 4 个社区 PR（均已合入 master 并保留作者署名）：

| 贡献者 | 内容 |
|--------|------|
| [@jianzhangg](https://github.com/jianzhangg) | #100 OpenRouter 系思考字段归一化、#101 面板四项易用性改进、#103 TDZ 回归修复、#105 统计缓存持久化修复 |
| [@bertchiangchiang](https://github.com/bertchiangchiang) | #102 费率完整模型 ID 补渠道前缀（修 Cline 免费档路径） |

以及算法来源：[dsh-attention-health](https://github.com/donghangxunlang-cmd/dsh-attention-health)
（思考链死循环检测 `lib/guard.js`）。

---

# wild-work v2.7.2 — 模型降级链 + 备用上游通道 + Cline 模型 ID 简化 + oczen 403 修复与地域改道

> 发布主题：**模型降级链**（主模型被上游按模型限流全池冷却时，按用户配置的有序备胎链自动顶上）、
> **代理配置重构为「全局备用上游通道」**（默认不参与任何请求，仅渠道命中地区拒绝时同请求改道重发）、
> **Cline 模型 ID 直通上游命名空间**（`cline/cline-free/x` → `cline-free/x`）、
> 以及 **oczen 免费档 403 修复**（issue #111）与**其地域受限模型的自动改道**、**workbuddyai 6004 / 402 权益不足纳入降级触发**、
> **traework/traecode 模型元数据补齐 + Kimi 系 tool schema `$ref` 消毒**、
> **流截断原因可辨识**（区分空闲看门狗与上游断连）。
>
> 上次 tag v2.7.1 之后累积 32 个提交 + 8 个合并 PR（#106–#110、#112–#114，
> 来自 [@bertchiangchiang](https://github.com/bertchiangchiang) 与 [@LFDCC](https://github.com/LFDCC)）。
> 本版为**累积发布**：v2.7.1 之后的所有变更都在这里。

---

## 🔗 模型降级链（R54，PR #106 + #112）

主模型被上游按模型限流、全池冷却时，此前只能 fail-fast 503 打断 agent 任务。本版支持预配有序备胎链。

- **配置**：`config.model_fallback[]`，每条 `{from, to[]}`（`from` 全局唯一、不自环、`channel/model` 全路径）。
  设置弹层可视化编辑（备胎以 chip 展示，可逐个删、回车追加、整条删除），保存即热生效
  （`POST /api/config/model_fallback`），无需重启。
- **触发判据**：**仅**「主模型在该渠道全部账号上处于模型级限流冷却」时查链
  （`runtimeCapacity` 一次 `Pool.List()` 同时得出 hasHealthy 与 modelAvail，口径与
  `no_healthy_account` 的「模型全冷」分支一致）。**未配置规则时零探测** —— 正常路由零开销。
- **不粘滞**：每个请求都按原模型重新判定，上游解冻即自动切回（粘性是账号级 R29，降级是模型级且用户显式配置）。
- **agent 可感知**（双保险）：① 响应头 `X-Model-Fallback: 请求模型 -> 备胎`；② chat 面响应体
  `model` 字段写实际备胎全路径。兼容层（`/v1/responses`、`/v1/messages`）**统一转写**该头——
  这两个端点响应体由网关重建且 `model` 回填请求名，不转写就等于对 Codex/Claude Code 静默换模型。
- **运行统计**新增「降级记录」区块：降级/恢复事件流（内存环形 200 条、重启清零），空态常显。
- **记账口径**：token 流水仍按客户端请求名（面向「这一发花多少」）；实际执行模型由日志行 +
  面板「降级记录」承担。
- **修复（PR #112）**：`server.Config.ModelFallbacks` 此前**从未被赋值**，降级链唯一生效通道是
  面板保存的热更新 ⇒ **进程重启后已配置的降级链静默失效**，直到用户重新保存一次。已补启动装配。

## 🔀 402 权益不足纳入降级触发（R60，PR #113）

- **流内嵌 402 无法分类**：qwenwork 等渠道的 402 以 HTTP 200 + 流内 envelope 返回，
  剥壳后以裸 `fmt.Errorf` 抛出 ⇒ handler 的 `errors.As` 全部错过。改为抛 `*sseError`
  并实现 `provider.StreamErrorClassifier`（错误文案保持不变）。
- **单号渠道 402 只透传不冷却**：账号本身健康，死的只是「账号×该模型」组合 ⇒ 不冷却则
  每次请求继续撞 402、降级链永不触发。现改为**模型级冷却**（`CooldownModel`）：账号不动、
  同号其他模型照常可用，下一次请求挑号即「模型全冷」自动走降级链。
- 三处接线：预流式单账号分支、流内单号分支（去掉 `&& !SingleAccount` 限定）、`Aggregate` 出口。

## 🌍 代理配置重构：全局备用上游通道（R57）

旧「每渠道单独配代理」有三处实证缺陷：面板 12 行输入但热更新只覆盖 9 行（`monkeycode`/`raccoon`/`loomy`
改代理不生效、需重启）、`trCodeUp`（TraeCode）**两张表都没进** ⇒ 该渠道代理从未生效；
每次保存都新建 Transport（杀光 idle 连接）；对用户是 12 个概念，实际只需要一个。

- **新语义**：`config.backup_proxy` **单一地址**（http/https/socks5）= **备用链路**，由
  `internal/backuplink` 原子持有。**不影响本工具的任何常规请求**（所有渠道一律直连）；
  仅由渠道自行判定后按需取用（克隆该渠道自己的 Transport、只改出口 ⇒ 除 IP 外与出厂行为同构）。
- **当前唯一取用方 Cline**：直连命中地区拒绝（403/451 + 保守文案表）⇒ **同一次请求内**重放同一 body。
  未配置 / 重放失败 ⇒ **透传直连那份原始错误**（不拿第二次尝试的错误掩盖首因）；重放拿到不同答复则以它为准。
- **学习**：命中一次后 6 小时内该模型直接走备用（免每发白撞一发 403）；改/清代理地址即作废。
- **清理**：删 `config.proxies`、`applyProxies`/`SetTransportProxy`/`proxySyncer`、`POST /api/config/proxies`、
  面板 12 行输入。旧 `proxies` 段在 normalize 一次性迁移（优先 cline 的值，其余渠道地址丢弃并日志点名）。
- 新端点 `POST /api/config/proxy`（同时承载 OpenCodeZen key）。
- **已知限制**：登录编排（`login_*`）、CDP、面板内凭证测试仍不吃备用通道（保持「只服务上游通信」的边界）。

## 🧩 Cline 模型 ID 直通上游命名空间（R59）

Cline 上游免费池的 ID 自带命名空间（`cline-free/*`），此前对外 ID 是双层路径 `cline/cline-free/*`。
本版**直通上游**：`/v1/models` 与面板复制出的都是 `cline-free/step-5-preview` 这类形态。

- **旧写法保留路由兼容**：`cline/cline-free/*` 仍可用（老配置的 `model_map`、降级链规则、
  老客户端都是那种写法），两条路径经同一解析器（`provider.PublicModelID` / `SplitModelID`）。
- **六处接线同源**：`/v1/models`、`runtimeForModel`（也是降级链的解析器）、网关 `Router.Resolve`
  （含 `default_channel` 兜底拼接）、`app.buildFeesChannels` 新增 `id` 字段、`stats.channelOfModel`、
  handler 统计归行（新旧写法必须落同一行）。
- **顺带修既有 bug**：`stats.channelKind` 白名单漏 `cline` ⇒ 该渠道 token 从不进平台统计表。

## 🔐 oczen 免费档 403 修复（issue #111）

**现象**：OpenCodeZen 全部对话恒 403 `FreeTierError`（"free tier can only be used from within OpenCode"），
填不填 key 都一样。

- **根因**：免费档不仅校验会话头**外形** `ses_<12位小写hex><14位Base62>`，还会把 12 位 hex
  时间戳段**解码**成官方客户端 `Identifier.descending()` 的值——`~(unix_ms<<12|counter)` 取低 48 位，
  故末三位是 `0xFFF-counter`；实测只接受 **counter 1–63**（末三位 `0xFC0–0xFFF`），
  超出即判非官方客户端 403。旧实现的时间戳段是 `sha256` 摘要前 6 字节（随机 hex），
  落进被拒窗口 ⇒ **恒 403**。
- **修法**：官方 descending 编码的 ID 原样保留，其余（摘要段 / 升序 ID）**确定性重编码**——
  哈希前 6 字节按 descending 布局渲染、counter 取 1。确定性保证同一会话仍稳定映射同一 session
  （prompt cache / key 亲和不断）。
- 附新增 `-tags live` 实盘测试（`internal/oczen/live_test.go`）。

## 🔁 oczen 地域受限模型自动改走备用通道（R62）

**现象**：2.7.2 修复 oczen 免费档后，部分免费模型（`muse-spark-*-contributor-free` 实测）直连仍回
403 `RegionError`（"This model is not available in your country"）。

- **改道**：与 Cline 同款机制——直连命中地区限制时，在**同一次请求内**经「备用上游通道」换出口重放一次；
  未配置备用代理则原样透传 403（不改成 503，不罚账号）。
- **公共编排**：这条规则语义细（首因不被第二次尝试掩盖 / 备用自身不可用则本轮回落直连 /
  只有拿到可用答复才记学习结果 / 学习结果随代理配置变更作废），故抽到
  `internal/backuplink/route.go` 的 `Replay` + `Routes`，**Cline 与 oczen 共用一份实现**
  （分头实现必然改一处漏一处，这是 R52 的教训）。渠道只提供「怎么发一发」与「什么算地区限制」。
- **oczen 判据宁窄勿宽**（命中会多发一发经第三方出口的请求）：仅 403/451 +
  `RegionError`/region 文案才算；`FreeTierError`、429、5xx 一律不重放。
- **不涉及账号**：备用通道只换出口 IP，`SingleAccount` 豁免语义不变（不变量 22），
  改道发生在渠道内部，成功时上层完全看不到 403。
- **如实披露的限制**：`muse-spark-*-contributor-free` 经该出口被上游判 400
  `ModelProtocolUnsupported`（同一出口下 mimo / big-pickle / nemotron 均 200）。
  本工具的职责是「换出口、把上游的权威答复原样交出」，**某模型在某个出口下能否用由上游决定**，
  本工具不承诺。

---

## 🧠 workbuddyai 6004 归入软限流管线（PR #114）

- 国际版 6004（`usage exceeds frequency limit ... switch to the other models`）此前归 `ErrPassthrough`
  （纯透传）。该设计诞生时只有账号级冷却这一种手段，但 #82 的**模型级冷却**与 #106 的**降级链**
  就位后，纯透传反而成了死角：**什么都不冷却 ⇒「模型全冷」判据永不成立 ⇒ 降级链永不触发**，
  客户端只能对着重置时间干等。
- 现改判 `ErrSoftRate`：handler 走 `CooldownModel`，**只冷却「账号×该模型」到响应体重置点**，
  同号其他模型不受牵连；模型全冷时降级链自动顶替，重置后自动切回。本次请求仍原文透传。
- 同时补 `softRateResetTs` 识别国际版英文形态「will reset at `<ts>` UTC+8,」——
  冷却精确到真实重置点，而非回退固定 60s。

## 🗂 traework / traecode 模型元数据 + tool schema 消毒（R55/R56，PR #108/#109）

- **模型元数据补齐**（R55）：此前只解析 `config_name` + `display_name` ⇒ 两渠道 118 个模型
  在 `/v1/models` 上**全无** `context_length` / `max_output_tokens`、图片能力恒 false。
  现解析上游 `context_window_tokens.{dev,max}`、`display_config.multimodal`、
  `model_detail_list[].max_tokens`（上下文取 max 档、缺省回 dev；0 不填）。
  **容错承接**：容量字段用 `json.RawMessage`，上游形状突变（对象→标量）时只丢该字段，
  不让整张目录解析失败（否则本渠道池整体拉不到）。
- **tool schema `$ref` 全量内联消毒**（R56）：Kimi/Moonshot 系对 `tools.function.parameters`
  里任何保留的 `$ref` 都报 400 `code=4027 detected infinite recursion`（官方客户端不发 `$ref`，
  经 MCP 接入的代理客户端会发）。改为**全量内联**：所有本地 `$ref` 就地替换为目标消毒副本，
  成环处截断为空 schema；支持 `parameters` 预序列化字符串形态。
  **零改动原则**：无 `$ref` 的请求一个字节不动（回写 Marshal 结果会重排键序、改写数字字面量，
  污染上游 prompt 缓存前缀）。

## 🩺 流截断原因可辨识（R58）

**现场**：`Qwen3.8-Flash (QoderCOM)` 输出一部分后客户端报 `upstream stream truncated`。

- **诊断缺口**：qoder/qodercn/qodercom/qwenwork 四个渠道把「截断」布尔当唯一信息，
  收尾写**固定串**并丢弃原始错误 ⇒ 三种完全不同的故障（空闲看门狗掐的 / 上游主动断连 /
  读到半个帧）在日志与客户端**完全同形**。
- **判据不可达**：`internal/idle` 看门狗唯一的动作是 cancel 请求 context，在途 Read 返回
  `context canceled`（真实链路实测），而 `IsIdleTimeout` 只认 `ErrIdleTimeout` ⇒
  看门狗掐断在语义上根本认不出来。
- **修法**：`provider.IsIdleTimeout` 扩展识别看门狗族；新增 `TruncationCause/TruncationError/
  TruncationCode/TruncationFrame/WriteTruncationFrame` 一族——错误文本保留原因、空闲超时统一
  归一成 `upstream stream idle timeout`（不直写 `context canceled`，否则日志检索漏一半）、
  出口帧 `code` 分流为 **`upstream_timeout`** / `upstream_truncated`。
- **日志补强**：`stream relay end` 补 `model` / `elapsed` / `ttfb` ——
  **耗时 ≈ 空闲阈值且 TTFB 远小于它 ⇒ 看门狗掐的；耗时远小于阈值 ⇒ 上游自己断的**。
- **清理空头支票注释**：`config.Upstream` 只有 `timeout_seconds`、`main.go` 零处给 `IdleTimeout`
  赋值 ⇒ cline/loomy/raccoon/monkeycode 注释里承诺的 `stream_idle_seconds` 配置项**从未存在**，
  阈值恒为硬编码。已改成如实说明（不新增用户概念）。
- **已知限制**：`elapsed` 含上游首字节等待时间；qoder 三渠道 Transport 未设 `ResponseHeaderTimeout`，
  「排队」若发生在响应头之前则该阶段无上限，判据需结合 TTFB 一起看。空闲阈值仍不可配。

## 🐛 修复清单

| Issue/PR | 问题 | 状态 |
|---|---|---|
| — | 主模型被上游按模型限流全池冷却时只能 fail-fast 503，无自动顶替 | ✅ 本版新增（模型降级链） |
| PR #112 | 进程重启后降级链静默失效（配置从未在启动时载入） | ✅ 本版修复（启动装配补 `ModelFallbacks`） |
| PR #113 | 402 权益不足不进任何冷却/降级触发（流内 402 无法分类 + 单号渠道只透传） | ✅ 本版修复（分类化 + 模型级冷却） |
| PR #114 | workbuddyai 6004 纯透传 ⇒「模型全冷」判据永不成立 ⇒ 降级链永不触发 | ✅ 本版修复（改判软限流 + 英文重置点解析） |
| #111 | OpenCodeZen 全部对话恒 403 `FreeTierError`（会话头时间戳段非官方 descending 编码） | ✅ 本版修复（确定性重编码） |
| — | 旧「每渠道代理」热更新覆盖不全（3 渠道改代理不生效、TraeCode 从未生效） | ✅ 本版重构（全局备用上游通道） |
| — | Cline 对外模型 ID 是双层路径 `cline/cline-free/*`，反直觉 | ✅ 本版改为直通上游 ID（旧写法兼容） |
| — | Cline 的 token 从不进运行统计平台表（`channelKind` 白名单漏 cline） | ✅ 本版修复 |
| PR #108 | traework/traecode 118 个模型无上下文/输出上限、图片能力恒 false | ✅ 本版修复（解析上游元数据 + RawMessage 容错） |
| PR #109 | Kimi/Moonshot 系 tool schema 含 `$ref` 即 400 `4027 infinite recursion` | ✅ 本版修复（`$ref` 全量内联消毒） |
| — | 流被截断时三种原因在日志与客户端完全同形，无法定位 | ✅ 本版修复（原因自证 + 出口错误码分流 + 日志补 elapsed/ttfb） |
| PR #107 | qoder 系「max_output 推导」与 monkeycode 容量来源两处注释失实 | ✅ 本版修正 |

## 🙏 致谢

本版含 8 个社区 PR（均已合入 master 并保留作者署名）：

| 贡献者 | 内容 |
|--------|------|
| [@bertchiangchiang](https://github.com/bertchiangchiang) | #106 模型降级链、#107 模型元数据注释修正、#108 traework 元数据解析、#109 Kimi 系 `$ref` 消毒、#112 降级链启动载入、#113 402 纳入降级触发、#114 workbuddyai 6004 软限流 |
| [@LFDCC](https://github.com/LFDCC) | #110 OpenCodeZen 免费档会话头 descending 编码（修 issue #111 的 403） |

---

# wild-work v2.7.3 — 自定义渠道接入 + 小浣熊签到 + 思考强度注入

> 发布主题：**自定义 OpenAI 兼容渠道**（在面板直接接入任意第三方 OpenAI 兼容 API，
> 用 `渠道标识/模型名` 统一访问并纳入用量流水与运行统计）、**小浣熊 setting_info 签到**
> （触发每日积分的惰性落账，不用再手动开网页）、**思考强度档位注入**（客户端未传
> `reasoning_effort` 时按面板配置的「模型 → 档位」映射注入，与 R49 互补），
> 以及 **面板「测试」按钮与真实对话同源**（直连被地区拒时同样自动改走备用通道，并如实回传实际出口）。
>
> 本次为 v2.7.2 之后的小版本，含 **4 个社区 PR**（#118 [@ezjbc](https://github.com/ezjbc)；
> #119/#120/#121 [@jianzhangg](https://github.com/jianzhangg)）。三个新渠道/能力 PR 的
> 合并期共修 **9 条缺陷**（含一处会打死进程的并发 map 读写、一处设置弹层打不开的悬空 DOM 引用），
> 详见下文各自章节。

---

## 🎚 思考强度档位注入（R63，PR #118）

客户端**没传** `reasoning_effort` 时，可按面板配置的模型档位自动补上；客户端**显式传了**的
（任意值，含 `null`）一律不覆盖。这是 R49「透出上游声明的档位」的互补能力：前者解决
「告诉客户端有哪些档位」，本条解决「客户端没指定时用哪个」。

- **配置**：`config.reasoning_efforts`（键 = `channel/model` 全路径，或 `*` 全局默认）。
  设置弹层新增「思考强度」区：全局默认行 + 模型档位映射列表（可增删）+ 档位输入框
  （建议 `none/low/medium/high/max`，也可自由输入）。保存走
  `POST /api/config/reasoning_efforts`，**保存即热生效**（无需重启）。
- **注入三态（哑管道）**：
  1. 请求体顶层**已有** `reasoning_effort` 键 → 一律不碰；
  2. 未带且**精确命中**模型 ID → 注入配置档位；
  3. 精确未命中 → 回退 `*` 全局默认；两者皆无 → 不注入（由上游用模型默认档）。
- **零配置零开销**：未配置映射时，注入路径首行即短路 —— 不发日志、不改 body。
- **失败不中断**：注入失败（body 非 JSON 对象等）仅记日志，原 body 照发，绝不因此让请求失败。
- **如实披露**：`none` 档在 WorkBuddy 上游疑似回落默认档（哑管道照实注入、不做纠正）；
  **TraeWork 渠道协议无 reasoning 注入点，对该渠道配置的档位注入无害但无效**（面板提示已写明）。

## 🔌 面板「测试」按钮与真实对话同源（R64，issue #117）

用户反馈：配了备用代理，面板 OC Zen「测试」仍 403，代理日志零访问记录，上游看到的仍是真实地区。

根因不是备用通道失效，而是**测试路径从未经过改道编排**——`TestKey` 原先直接走 `c.HTTP`
直连，配了代理也不会改道；看到 403 时用户无从判断「代理没生效」还是「上游本来就不给这个出口用」。

- **测试改走同一条编排**：`oczen.TestKey` 现与 `ChatStream` 共用 `backuplink.Replay`
  （含地区限制学习路由），直连被地区拒时同请求内自动改走备用通道。
- **如实回传实际出口**：`via_backup` 字段告诉面板答复来自「直连」还是「备用通道」，
  面板提示相应展示；403 且走直连时给出可行动提示。
- **日志不再误导**：单账号分支的错误日志补上上游原文 `body=`，文案改为说明**为何**不冷却
  （无号可轮换），不再对与账号无关的 403（如免费档形态校验）断言「所以这不是账号问题」。

## 📝 文档与决议整合

- AGENTS.md 新增 **R63（思考强度注入）**、**R64（探针与真实请求同源）**，并补全
  **R46（Cline 只认官方 `sk_` Key，不接受 WorkOS refreshToken 号池）** 的裁定说明。

## 🐻 小浣熊签到（R65，PR #119）

小浣熊**没有**独立的签到/领取端点，但每日 300 积分是**惰性**落账的：名义记账在当天 00:00，
实际要等账号当天第一次访问**工作台业务接口**才入账（网页端文案「当天登录后有效期延长至 7 天」）。

- **实质动作** = 调一次 `office/v3/setting_info`（网页端打开工作台首页时本来就会调的接口，
  故 ≈ 替用户打开一次网页，不是跨端伪装）。实测接口矩阵：对话/只读余额/`auth/v1` 均**不触发**。
- **结果判据**用 `point_grant_popups`（本次发放的直接回执）而非余额差分——同期可能有对话扣费抵消。
  幂等已实测：重复调用不再发放，故非空 popups ⇔ 本次刚触发新发放。
- **接线补齐**：`rcSch` 从「空签到时间」改为全局签到时间 + 补 `SetCheckinObserver`/`SetRefreshObserver`
  （此前**从未设置**，不设则面板与运行统计都读不到结果，属 R34 家族的静默失效）；
  手动签到按钮开放（`noExplicitCheckin` 移除 raccoon）、登录与导入两条路径都补首次签到。

## 🔌 自定义 OpenAI 兼容渠道（R66，PR #120）

可在面板直接接入任意第三方 OpenAI 兼容 API（官方 StepFun、本地 CLIProxy、DeepSeek 等），
用 `渠道标识/模型名` 统一访问，并纳入用量流水与运行统计（含 TTFB、请求日志）。

- **配置**：`config.custom_channels[]`（`name`/`base_url`/`api_key`/`models`/`enabled`），
  设置弹层可视化增删改 + 连通性测试，保存即热生效（无需重启）。
- **模型面不做假设**：用户配什么 `/v1/models` 就返回什么，**不向上游拉取、不推测**；
  上下文/思考档位/多模态能力均由用户填写并如实透出。
- 单账号 + `NoCooldownOnServerError`（R16/R24 哲学）；流式与非流式分流（R35）。

**合并期修复的五条缺陷**（均有代码证据，非推断）：

1. **`a.runtimes` 并发读写会在保存时打死进程**——本 PR 引入该表的**首个 post-init 写**，
   而 9 处读点（含 30s 后台统计喂入与 `/api/state`）全程不加锁 ⇒ 与保存重叠即
   `fatal error: concurrent map read and map write`（不可 recover）。已加 `runtimesMu` +
   快照读，回归测试反向验证（去掉锁 5/5 崩溃）。
2. **compat syncer 捕获启动快照会丢新渠道**——「先加自定义渠道、之后单独改模型映射并保存」
   会把路由白名单重置回启动态，`/v1/responses` 与 `/v1/messages` 报 unknown channel。
   已抽出可变共享态 `channelList`，两条 syncer 共读。
3. **自定义渠道名可与内建同名**——会导致 server/app 两层指向不同 Runtime（面板操作打到自定义池、
   对话走内建渠道）。已加 `provider.BuiltinKinds()` 校验，直接拒绝。
4. **「费率表展示自定义渠道」原为假承诺**——费率表只遍历静态渠道表，自定义渠道永不出现。
   已改为把未登记的渠道追加到末尾。
5. **`/api/state` 明文回传上游 `api_key`**——已改为脱敏回显（保留 `…` 供识别「未改动」），
   面板未改动时提交脱敏值 = 不修改（后端按渠道名找回真实密钥，避免把密钥覆盖成废值）。

## 🐛 修复清单

| Issue/PR | 问题 | 状态 |
|---|---|---|
| PR #118 | 客户端未传 `reasoning_effort` 时无法按面板配置注入档位 | ✅ 本版新增（思考强度注入） |
| PR #119 | 小浣熊每日积分要手动开网页才到账 | ✅ 本版新增（setting_info 签到，R65） |
| PR #120 | 无法接入第三方 OpenAI 兼容渠道 | ✅ 本版新增（自定义渠道，R66，合并期修 5 处缺陷） |
| PR #121 | #120 的 UI 打磨未进 master | ✅ 本版新增（k/M 单位、工具能力、紧凑布局；合并期修 3 处缺陷） |
| — | 合并 #120 后设置弹层打不开（悬空引用已废弃节点） | ✅ 本版修复（+ 通用悬空 DOM 引用静态检查） |
| #117 | 配了备用代理，面板「测试」仍 403 且无从判断代理是否生效 | ✅ 本版修复（测试同源 + 出口回传 + 日志澄清） |
| — | 单账号渠道错误日志写死「不罚账号」，掩盖真实原因 | ✅ 本版修复（补上游原文 + 说明为何不冷却） |
| #98 | TraeWork 授权回调停在 `127.0.0.1:18080`，回面板无响应 | ⏸ 暂缓（登录编排层，另作评估） |

## 🙏 致谢

| 贡献者 | 内容 |
|--------|------|
| [@ezjbc](https://github.com/ezjbc) | #118 思考强度档位注入（含面板可视化编辑与热更新） |
| [@jianzhangg](https://github.com/jianzhangg) | #119 小浣熊 setting_info 签到、#120 自定义 OpenAI 兼容渠道、#121 自定义渠道 UI 打磨 |

---

# wild-work v2.8.0 — 新增华为云果办 OfficeAce 渠道 + WorkBuddyAI 渠道校验修复 + 小浣熊迁站适配 + 跨渠道降级 + 面板易用性

> 发布主题：**新增第十四条渠道（华为云果办 OfficeAce）+ 上游改版/风控适配 + 四处「静默失效」修复**。
> 新增 **OfficeAce（`officeace/*`，华为云 AgentArts 模型网关）**——支持本机客户端导入与自助
> OAuth 登录两条添加路径、积分/签到、临时凭据自动续期；**修国际版 workbuddyai 全模型
> `400 code=11-128` 不可用**（上游新增渠道校验，点名缺 `X-Conversation-ID`）；小浣熊登录
> 跟进上游迁站（入口与凭据 cookie 名都变了，旧实现已完全失效）；启动时监听失败不再静默留在后台
> 当僵尸实例；模型降级链在「整渠道全灭」时也能跨渠道顶上；另修一处会让
> **管理面板登录层不可见**的 HTML 结构缺陷；设置面板补上模型查找入口、模型查找结果不再截断、
> 思考强度行的模型 ID 不再被截断。
>
> 本次为 v2.7.3 之后的功能版本（跳过未发布的 v2.7.4 编号，把该轮修复一并归入），含
> **8 个社区 PR**（#124/#126 [@xxhhlk](https://github.com/xxhhlk)；#127/#130/#131/#132 [@ezjbc](https://github.com/ezjbc)；
> #122 [@zkj123](https://github.com/zkj123)；#128 [@fmdxx1991](https://github.com/fmdxx1991)；
> #135 [@jianzhangg](https://github.com/jianzhangg)）
> 与 3 个 issue（#125 [@sept13-yu](https://github.com/sept13-yu)；#117 [@fmdxx1991](https://github.com/fmdxx1991)；
> #133 [@danshui93](https://github.com/danshui93)）。
> **合并期额外修出 7 条跨切面缺陷**（小浣熊 JWT uid 提取漏改两处、降级文案与死代码、
> oczen 匿名车道与伪装头、OfficeAce 的 error 帧误报/非流式吞错/Extra map 竞态/导入丢 refresh_token），详见下文。
>
> **⚠️ 本版于当日重发**：初版 v2.8.0 发布后 1 小时，国际版上游开始按账号灰度拦截
> **缺 `X-Conversation-ID` 的对话请求**（`400 code=11-128`），workbuddyai 全模型不可用；
> 修复（PR #135）合并后本 tag 重指向新提交并重新构建全部产物。已下载初版的用户请重新下载。

---

## 🎉 新渠道：华为云果办 OfficeAce（`officeace/*`，PR #122，[@zkj123](https://github.com/zkj123)）

把**华为云果办**（AgentArts 模型网关，本身 OpenAI 兼容）聚合进本工具，可多账号池化调度、
统一记流水与运行统计。协议参考开源项目 [Q-shuang-dot/officeace2api](https://github.com/Q-shuang-dot/officeace2api)（MIT）。

- **对话**：`officeace/<model>` 路由；流式/非流式（恒以 `stream:true` 打上游，非流式本地聚合）；
  推理模型自适应补足 `max_tokens` 预算（思维链吃光预算会让正文为空），`glm-5.3` 系自动剥离
  其不认的 `thinking` 参数、且**保留 `stream_options.include_usage`**（否则该模型 token 流水恒 0）。
- **账号来源（两路）**：① 面板「从客户端导入」读本机已登录的 OfficeAce 桌面端文件
  （`models.json` + 加密档案，后者经 DPAPI/AES-256-GCM 解密取临时 AK/SK，仅 Windows）；
  ② 面板「浏览器登录」走自助 OAuth（华为云授权码 + PKCE + DPoP，跨平台，无需桌面端）。
- **积分/签到**：控制面 V4/V11 华为签名查询套餐与奖励积分；每日签到走 `bonus/claim`。
- **续期**：`refresh_token` 一次性轮换（单账号单飞 + 死名单防重放），落盘持久化。
- **模型列表**：`/v1/models` 以华为网关动态目录为准；拉取失败回退内置静态表。
- **能力字段口径（R49）**：上游目录只返回模型 id、无任何能力声明，故 `SupportsTools` /
  `SupportsReasoning` 一律**不编造**（保持未知）；思考档位面同样不编造。
- **已知限制（如实披露）**：合并时**无真实华为账号端到端实盘验证**——首次真实登录/续期/签到
  如遇上游字段与预期不符，需按实际响应微调；DPAPI 解密仅 Windows 生效，其它平台若不走自助登录
  则积分/签到降级（对话不受影响，网关 Basic 凭据不过期）。

## 🛡 WorkBuddyAI 对话补会话头族，修上游渠道校验（R71，PR #135，issue #133）

2026-10-16 上午起，**国际版 workbuddyai 全模型**恒返回
`400 code=11-128 "conversation id missing"`，响应体里明写「这次调用没通过渠道校验，
可能不是官方客户端发出的」；**国内版 workbuddy 同时刻完全正常**。上游按账号灰度推进
（生产池实测约 22% 账号先中招），故表现为「同一时刻有人能用、有人全挂」。

- **根因**：上游国际版新增渠道校验，校验对象是**请求头里的会话头族**（实测只在 body 里
  加 `conversationId` **无效**）。本仓 `chatHeaders` 此前已带
  `X-Conversation-Request-ID` / `-Message-ID` / `X-Request-ID` / `X-Root-Request-ID` 四件，
  **唯独缺 `X-Conversation-ID`** —— 恰是上游点名的那个字段。
- **修法**：补 `X-Conversation-ID`（随机 UUIDv4、带连字符，每次请求现生成），
  既有四件头保持原貌不动。**不改 body**（实测无效），**不碰国内版**（上游无此校验）。
- **证据**：三个独立项目交叉验证 —— ardeyouxipianyi/workbuddy2api-hub#380（对官方客户端
  5.8.4 静态解包 + mitmproxy 抓包约 350 条核验会话头族形态）、CangShui/workbuddy-gateway#20
  的 A/B 实验（同凭据同 body 直连，缺会话头族恒 400、带齐恒 200）、ithtelab/workbuddy-manager#193
  （生产池补头后逐号全绿）。
- **本仓独立复现与验证**：三账号直连上游做消元矩阵 —— 被封账号「无头 / 只补
  `X-Conversation-ID` / 只带原四件头」三种形态**全部 400**，**五件齐全才 200**；
  未封账号任何形态均 200。端到端跑 master 与修复版两份二进制：master 对 `deepseek-v4.1-flash`
  / `glm-5.3` 恒 400，修复版对 `deepseek-v4.1-flash` / `glm-5.3` / `kimi-k3` 均正常出流。
- **顺带实证的两条**（PR 描述未覆盖，已记入 R71）：① 上游校验的是**头族齐全**而非单点，
  只补 `X-Conversation-ID` 仍被拒；② `X-Conversation-ID` 的**取值形态不被校验** ——
  UUIDv4、32 位 hex、甚至裸串 `xyz` 都通过，重复使用同一 id 也稳定 200，故「每请求现生成」
  与「稳定派生」在功能上等价。
- **已知限制**：上游为抽样/灰度校验，补头后短期不再出现不等于绝对修好（可能还有其他校验维度）；
  若后续仍偶发 11-128，需抓失败样本重新定位。

## 🦝 小浣熊登录跟进上游迁站（R67，PR #124）

上游把小浣熊登录从 `office.xiaohuanxiong.com` 迁到 `xiaohuanxiong.com`，改用独立 `/login`
页，并**改了凭据 cookie 名**。旧实现两处失效，表现为**面板弹出的登录窗口登录成功后一直跳回
登录页，接口随后报 429**，5 分钟超时后连访客凭据都读不到。

- **入口修正**：旧入口 `…/home?loginModal=true` 在新版前端会陷入
  `/work/home?loginModal=true ↔ /login?redirect_url=…loginModal=true` 的**重定向死循环**
  （cookie 永远写不进来）。改为直接开 `/login?product_code=work&redirect_url=…%2Fwork%2Fhome`，
  **不带 `loginModal`**。
- **凭据 cookie 改名**：登录态凭据从 `raccoon_refresh_token` 变为 `xhx_token`（新版标准 OIDC JWT，
  ≈30 天）。旧名在**访客态仍会下发**且能成功调 refresh —— 监听旧名会一打开就抓到访客凭据、
  落盘成假账号（R28 那个坑，现在必现）。
- **判据放宽**：新版 JWT 无 `owner_type`，原判据会恒判访客 ⇒ 永不捕获。改为
  「`xhx_token` 出现即登录，仅上游**显式**声明 `visitors` 才拒绝」。

## 🚪 启动时监听失败必须终止进程（R68，PR #126）

第二次双击启动没有任何「端口已被占用」提示：托盘图标照常出现、面板也能打开——打开的是
**第一个实例**，用户以为多开成功。根因不在 socket 层，而在启动时只把 `listen` 失败写进日志
就继续往下跑：11 个调度器与托盘全部照常启动，第二个进程在后台**重复签到**，并与第一个实例
**共享同一数据目录**，刷新出的 token 互相覆盖。

- 改用启动层已有的 `fatal()`（写日志 + 系统提示框 + `os.Exit(1)`），与「配置加载失败」
  「对外暴露但缺管理密码」两条 `fatal` 同一口径；位置在托盘创建与调度器启动之前，
  不会留下幽灵托盘图标。
- `--autostart` 同样适用：自启时端口被占也必须让用户知道，静默退出只是把「静默假活」
  换成「静默真死」，问题一样不可见。

## 🔀 整渠道全灭时跨渠道模型降级（R54 扩展，PR #127）

R54 的降级链在生产里**只救了一半**：主模型所在渠道被 pool 的「多模型接连限流升级整号冷却」
打成整渠道全灭后，请求直接 503——链上的跨渠道备胎（raccoon/loomy/workbuddy）**从未被尝试**。

- 删掉 `pickModelFallback` 里「本渠道无健康账号就提前返回」的分支：「模型全冷」与
  「整渠道全灭」两种形态**共用同一条降级链**；链路里落在同渠道的备胎会被
  `modelHasCapacity` 自然跳过，故必然落到真正可用的跨渠道备胎上。
- **不改**：`X-Model-Fallback` 头 / `body.model` 回填 / 恢复探针语义 / 未配置规则时零探测。
  「无备胎可用」时仍回到原 `no_healthy_account` 503 路径。
- 面板文案同步覆盖两种触发形态（不再只说「模型全冷」）。

## 🖥 管理面板登录层不可见（issue #125）

开启 `admin_password` 后，管理面板能打开壳子，却**看不到登录弹层**、点按钮无反应。
根因是 `index.html` 里「思考强度」区块**少了一个闭合 `</div>`**，导致 `settingsOverlay`
提前吞掉其后的 5 个弹层（含 `adminLoginOverlay`）——DOM 上登录层虽已去掉 `hidden` 且
`display:flex`，但父级 `settingsOverlay.hidden` 为 `display:none`，子树布局恒 0×0。

- 补回缺失的 `</div>`，并新增 3 条静态断言防回退（`<div>` 开闭配平 / `settingsOverlay`
  有匹配闭合 / `adminLoginOverlay` 位于 `settingsOverlay` 之外）。

## 🎛 面板易用性打磨（PR #130 / #131 / #132，[@ezjbc](https://github.com/ezjbc)）

三处纯前端改进，都不改接口与既有 DOM 结构，且各补了静态断言防回退：

- **设置面板补上模型查找入口（#131）**：「模型降级链」与「思考强度」两块都要填
  `channel/model` 完整 ID，此前只能关掉设置弹层、跳回首页查、再手工抄回来。现两处标题右侧各加
  「🔍 查询模型」按钮，复用首页同一个查找弹层。**审查跟进修出真实缺陷**：按钮同时带 `.icon-btn`
  （`width:30px` 纯图标尺寸），只覆盖 `height` 不覆盖 `width` 会让 68px 标签挣出 30px 边框盒、
  压到相邻内容；已补 `width:auto` 并加断言（原先只校验选择器存在，对本缺陷会假绿）。
- **模型查找结果不再截断（#130）**：原 `MODEL_SEARCH_MAX = 12` 让结果只渲染前 12 条，
  渠道多或前缀相似时想找的模型看不到。现结果全量渲染，超出对话框高度的部分由 `.ms-list`
  纵向滚动（对话框尺寸不变）；`min-height:0` 是让 flex 子项可滚动的前提，已在注释标明。
- **思考强度行模型 ID 不再被截断（#132）**：`renderEffList` 的 ID 列用了 `.lbl.wide`（定宽
  `90px`），同时给定宽与 `max-width` 时取较小值 ⇒ 内联 `max-width:260px` 从未生效。改为
  `width:auto;max-width:340px`，短 ID 自适应、长 ID 在 340px 处省略。

## 🔓 OpenCodeZen 匿名车道与伪装头跟进上游闸门（R69，issue #117）

issue #117 二次反馈「升级了还是恒 403」，并附**逐项消元矩阵**称上游 2026-10-14 起有三道形状闸门。
经核实，其引用的参考实现注释**确实存在且当前有效**（opencode2api 明确记载该三处变更），
故按参考实现对齐：

- **匿名车道改走 `/inference/v1`**（新增 `AnonymousBase`，`base()` 按凭证分流：匿名 → `/inference`，
  自备 `sk-…` key → `/zen`）。上游把 **keyless 推理**从 `/zen` 迁到了独立入口，两条车道基址已分离。
- **不再发 `x-opencode-request`**：该关联头一旦出现即判非官方客户端 ⇒ 403。
- **`x-opencode-project` 改裸 40 位小写 hex**：`prj_` 前缀的规范 id 会被拒（值本身不校验，只校验形状），
  由 session 经 sha256 派生 ⇒ 同一会话稳定映射同一 project（保住 prompt cache 亲和）。
- 顺带补发参考实现要求、本仓此前**缺失**的 `x-opencode-session-id`（与 `x-opencode-session` 同值）。

**如实披露（不是「已复现」）**：本仓在 CN 直连与 JP 代理出口下，出厂形状与逐项变体共 **56 次**
匿名调用**全部 200**（含 `zen/v1` + `prj_` + `x-opencode-request`），同时**负对照有效**
（去掉 `bash`/`read` 桩工具 ⇒ 403），证明探针确能观测闸门。故判断为**上游按出口/后端金丝雀下发**，
而非闸门不存在。换到 `/inference` 在两种情形下都不会更差（两基址的 `GET /models` 完全相同、均 200），
故跟随参考实现；**若仍有 403，请附日志里的上游原文 body。**

## 🔧 合并期修出的跨切面缺陷

1. **小浣熊 JWT uid 提取漏改两处（R67 补正）**：上游迁站后 JWT 换成标准 OIDC，
  **无 `name` claim**（账号标识在 `sub`）。PR #124 只给「登录落盘」与 `web.go` 补了 `sub` 回落，
  而 `internal/raccoon/clientsession.go`（**客户端会话自愈**）与 `internal/app/import_local.go`
  （**从客户端导入**）仍只读 `name` ⇒ 两路恒空：前者报「uid 不匹配」使自愈**永久失效**
  （不变式 19），后者把账号落成 `raccoon-default.json`。现收敛为**唯一取数点 `raccoon.JWTUID`**
  （旧版取 `name`、非空才认，否则回落 `sub`），四处共用并删掉已死代码的 `jwtClaim`。
2. **降级链的 `runtimeCapacity` 死代码与过期文案**：PR #127 删掉提前返回后，`runtimeCapacity`
  的 `hasHealthy` 返回值已无人使用，折叠为单一 `modelHasCapacity`；面板/统计里写死「主模型全冷」
  的文案同步改为覆盖「整渠道全灭」。
3. **OfficeAce 流内 error 帧误报（合并期实证）**：`errorSniffer` 原用
  `strings.Contains(line, "error")` 判据——正常回答正文里只要出现 `error` 字样（模型写代码/
  解释报错）就被当成错误帧 ⇒ 产出 `StreamError` ⇒ `statusOfPayload` 默认 502 ⇒ 归 `ErrServer`
  （Rotatable）⇒ handler 累计 `errCount` 后**冷却健康账号**。修为「必须能解析成 JSON 且顶层存在
  `error` 键」。
4. **OfficeAce 非流式路径吞掉内嵌错误**：`Aggregate` 原不做 error 嗅探，上游 200 + 流内 error 帧
  被 `upstream.Aggregate` 当成「成功空回答」吞掉（实测 `err=nil`）——客户端看到空内容、账号不冷却
  （与 R60 修 qwenwork 的正是同一形态，本渠道漏了非流式出口）。现与 `Stream` 同款嗅探。
5. **OfficeAce `Extra` map 并发读写致命错误**：`a.Extra` 是 map，`refreshToken` 的 `addDead`
  无锁就地写、`accountFromAuth` 无锁读 ⇒ Go `fatal error: concurrent map read and map write`
  （**不可 recover，直接打死进程**，实测复现）。修为 `accountFromAuth` 持 `RLock`、`addDead` 持 `Lock`。
6. **OfficeAce「从客户端导入」丢 refresh_token**：`Account.Export()` 原缺 `RefreshToken` 字段，
  而导入路径用它落盘 ⇒ 导入的账号只有 ~2h 临时凭据，之后积分/签到**永久无法续期**
  （对话走不过期的网关凭据，故极隐蔽）。现补字段并在导入处落盘 + 对无 refresh_token 的账号给出提示。
7. **OfficeAce 能力字段不编造（R49）**：`fetchModels`/`StaticModels` 原一律置
  `SupportsTools/SupportsReasoning = true`（上游目录只回 id，无任何声明）⇒ 属编造。现保持零值（未知）。
  另：401/403 从 `ErrSessionDead`（会 `pool.Disable` 永久禁用）改归 `ErrAccountFault`（软冷却可自愈），
  `reloadAccounts` 补上 OfficeAce 分支（否则 OAuth/导入落盘后账号要等重启才进池，R34/R41 家族）。

## 📝 文档与决议整合

- AGENTS.md：新增 **R67（小浣熊迁站）**、**R68（监听失败终止）**、**R69（oczen 车道与伪装头）**，
  R54 触发判据同步扩展，R44 注明「入口与 cookie 名已更新，见 R67」；OpenCodeZen 渠道说明改为四道闸门；
  决议索引更新为 R1–R69。
- **PR #128 的 oczen 核心未采纳（本仓实现更优）**：PR #128 与本仓已落地的 R69 同向，但其核心相对本仓
  是退化版——① 把 `DefaultBase` 一刀切改成 `/inference/v1`，**丢掉键控车道**（本仓 `base()` 按凭证
  分流：匿名 → `AnonymousBase`(/inference)，自备 `sk-…` key → `DefaultBase`(/zen)，与参考实现
  `Upstream.Zen` / `Upstream.Anonymous` 双字段一致）；② **漏发 `x-opencode-session-id`**（参考实现要求
  两个 session 头同值）。故 oczen 三文件与 AGENTS.md 保留本仓版本，仅采纳其文档/夹具/重复行等净残差：
  `DEVELOPMENT.md`（端点按车道分流 + 闸门清单补 `-session-id`）、`README.md`（systemone 示例端点）、
  `internal/provider/provider.go` 注释、`internal/devdump/devdump_on_test.go` 夹具、
  `internal/oczen/live_test.go` 注释、`cmd/wild-work/web/app.js`（删 `openSettings` 里重复两遍的
  `editCustomChannels`/`renderCustomChList`）。
- 回归：`internal/login_raccoon`、`internal/raccoon`（`TestJWTUID` / 新版 OIDC 自愈）、
  `internal/app`（uid 单点来源静态锁）、`internal/server`（降级链 13 例）、
  `internal/oczen`（车道分流 / 伪装头形状 / project 派生 + 实盘 live）、
  `cmd/wild-work/listen_fail_test.go`、`scripts/ui-static-check.mjs`。

## 🙏 致谢

| 贡献者 | 内容 |
|--------|------|
| [@zkj123](https://github.com/zkj123) | #122 新增华为云果办 OfficeAce 渠道（对话/积分/签到/OAuth 登录/DPAPI） |
| [@xxhhlk](https://github.com/xxhhlk) | #124 小浣熊登录跟进上游迁站、#126 监听失败终止进程 |
| [@ezjbc](https://github.com/ezjbc) | #127 整渠道全灭时跨渠道模型降级（R54 触发判据扩展）；#130/#131/#132 面板查找与模型 ID 显示打磨 |
| [@sept13-yu](https://github.com/sept13-yu) | issue #125 管理面板登录层不可见（定位到缺失的 `</div>`） |
| [@fmdxx1991](https://github.com/fmdxx1991) | issue #117 与 #128 OpenCodeZen 恒 403（附逐项消元矩阵，定位到端点与两个伪装头闸门） |
| [@jianzhangg](https://github.com/jianzhangg) | #135 WorkBuddyAI 对话补 `X-Conversation-ID`（修上游渠道校验 11-128，附 5.8.4 抓包口径） |
| [@danshui93](https://github.com/danshui93) | issue #133 国际版全模型 400 `11-128`（首个现场报告 + 指出国内版不受影响） |

---

# wild-work v2.8.1 — 自定义渠道积分噪音修复（无积分概念渠道全面退出积分体系）

> 发布主题：**修「刷新积分」把自定义渠道误计为失败**的提示噪音（R66 收尾）。
> 用户实测症状：面板点「刷新积分」恒报「成功 32 / 失败 4」，但全部真实账号都是绿的——
> 失败的 4 条全是自定义渠道（cliproxy / sgproxy / stepfun / zhipu）的占位账号。

## 🐛 修复内容

自定义 OpenAI 兼容渠道（R66）本就无积分概念：凭据本体是**渠道级 api_key**（对话转发用），
占位账号无 access/refresh token，上游也无任何余额端点（`UserResourceDetail` 恒返回空），
面板积分区已正确显示「不适用」。但积分体系的三条路径漏了对齐：

- **`RefreshAll`（批量刷新积分）**：不跳过自定义渠道，每个渠道必记一条
  `no token` 假失败 → 面板 toast 报「失败 N」纯噪音。对齐 oczen 的处理：
  `rt.Custom` 运行时直接 `continue`，不进统计（回归 `TestRefreshAllSkipsCustomChannels`）。
- **`RefreshCredits`（单账号刷新）**：不拒绝会落入 `creditSnapshot`，把 pool 快照**写成 0**
  （`UserResourceDetail` 恒空），让人误读成「额度已用光」。改为明确报
  「<渠道> 为自定义渠道，无积分概念，不适用」（回归 `TestRefreshCreditsCustomChannelNA`）。
- **`ResourceDetail`（积分明细弹层）**：同样恒返回空 items，前端弹出一张空白明细层。
  改为明确报错（同上回归覆盖）。

## 🎨 前端对齐（`web/app.js`）

- `NO_CREDITS` / `NO_EXPLICIT_CHECKIN` 原为**静态内建名单**，自定义渠道名不在其中：
  自定义账号卡片上有可点的「签到 / 刷新积分」按钮（点了后端必拒或写 0），
  显示名也可点改名（后端必报「无凭证文件」）。改为**动态判定**：
  `isCustomGroup()` 以后端 `/api/state` 下发的 `custom_channels` 为准（保存即热更新，
  勿在前端另维护一份）；`hasNoCredits() = 内建 NA 名单 ∪ 自定义渠道`，与后端
  `creditsNAKind` 同口径。
- 无积分账号拆成两类分别处理：**oczen/cline（固定账号）**维持原样——只留静态指示、
  不可停用/删除/改名；**自定义渠道账号**无积分/无签到，但保留停用/删除/改名入口
  （后端本就支持：删除 = 移除整个渠道配置），仅去掉签到/刷新积分两个无意义按钮。
- 「全部签到」的 skip 计数随之动态化（此前自定义账号既不在 results 里也不在 skip 里，
  toast 合计数对不上）。
- 「刷新积分」toast：失败为 0 时显示「全部成功（N 个）」，避免「成功 N / 失败 0」
  的怪异句式；「不适用」tooltip 文案改为「该渠道无积分概念」（不再是匿名通道特指）。
- `promptLogin` 的登录提示文案改回查内建名单（该处只可能收到内建渠道名）。

## ✅ 验证

- `go build ./... && go vet ./... && go test ./...` 全绿（35 包）；
  新增 3 条回归测试（`internal/app/creditrefresh_test.go`）。
- `scripts/ui-static-check.mjs` 通过；`node --check app.js` 语法通过。
- 本地实测（macOS，带 4 个自定义渠道的真实配置）：`POST /api/account/refresh_all`
  返回 `failed=0`，自定义渠道不再出现在 platforms；单账号刷新与明细均明确报「不适用」；
  `/api/state` 中自定义账号 `credits_na=true` 不变。
