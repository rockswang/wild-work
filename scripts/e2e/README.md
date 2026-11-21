# scripts/e2e —— 面板 ⇄ 后端的端到端脚本

本目录放**用户视角**的可复跑 E2E 脚本。定位：补单元测试补不上的缝——
「面板真的把操作送到了后端、后端真的接受、配置真的落盘/热生效」。

单元测试覆盖各自函数，`scripts/ui-static-check.mjs` 覆盖「渲染结果是否
该有的 DOM」，本层再往上一层：**真事件 → 真的 app.js → 真的 HTTP → 真的 Go daemon**。

## 运行

```bash
node scripts/e2e/fallback-primary-overlap.mjs
```

前置：Node ≥ 18（用到内建 `fetch`）、Go 工具链（脚本会 `go build ./cmd/wild-work`）。
**无 npm 依赖**：不装包、不联网、不读 `node_modules`。

输出示例：

```
✅ US-01  主力模型可作为另一条规则的备胎（面板 + 保存）  5/5
...
17 stories, 68 assertions, 0 failed  (6.3s)
```

退出码 0 = 全绿；任一 story 红 = 1（可直接用于下一步的闸门）。

## 什么是真的，什么是替身

| 部件 | 真/替 | 说明 |
|---|---|---|
| 面板脚本 | **真** | `cmd/wild-work/web/app.js` 原文件求值，不 stub、不改写逻辑 |
| 面板静态 DOM | **真** | `index.html` 被解析进沙箱，`getElementById` 拿到的是真节点 |
| 后端 | **真** | 真 `dist` 级 Go daemon（`go build ./cmd/wild-work` → 起进程） |
| `fetch` | **真** | Node 原生 fetch 打到真 daemon，行为与浏览器同构（环回免鉴权） |
| 事件 | **真** | `focus` / `input` / `click` / `keydown(Enter)` 走 app.js 注册的监听器 |
| DOM 实现 | 替 | `lib/dom.mjs` 手写最小 DOM（解析 `innerHTML`、事件冒泡、`closest`/`querySelector`）。项目无 JS 测试设施、无 Node 构建链，故不引 jsdom |
| 布局/绘制/CSS | 无 | 本层不断言样式（那是 `ui-static-check.mjs` 的职责） |

因此这层能证明的：**面板与后端只有一个真相**（面板发出的载荷后端原样接受）、
候选集/清洗/提示文案的用户可见行为、保存后 `config.json` 与运行期状态的落地。
不能证明的见「已知限制」。

## 隔离与「不打上游」

- 每个 story 起一个**独立 daemon**：`WILDWORK_HOME` 指向 `temp/e2e/<story>-<pid>/`
  （`temp/` 已在 `.gitignore`），只用临时 `config.json`，绝不碰开发者的 `auths/`、`data/`。
- 账号池为空：调度器无号可跑，**零上游请求**（遵守 AGENTS §0.1）。
- 模型候选来自临时配置里的两个**自定义渠道**（`demoa` / `demob`，静态模型表，不联网），
  因此用例不依赖任何真实渠道目录的快照。
- 结束后删除该 story 的临时目录；仅保留 `temp/e2e/_bin/` 的编译产物（下一次运行自动清理旧二进制）。
- 中断安全：runner 收到 Ctrl-C / SIGTERM 时先杀掉全部 daemon 子进程再退出（早期版本会把
  daemon 留成孤儿进程）。被杀那一刻正在跑的 story 的临时目录会留下（`temp/` 已忽略，无害）。

## 文件

| 文件 | 职责 |
|---|---|
| `fallback-primary-overlap.mjs` | 具体套件：spec 的 17 条 user story，每条一个 story 块 |
| `modelscope-credits.sh` | **Go 整机层**套件的可复跑入口（`internal/app/e2e_modelscope_*_test.go`，说明见下节） |
| `lib/harness.mjs` | 起/停 daemon + 加载面板的编排、断言收集、彩色汇总 |
| `lib/backend.mjs` | 编译与启停真 daemon（隔离 home、空闲端口、就绪探测、config 读写） |
| `lib/panel.mjs` | 在最小 DOM 里求值真 app.js，并把「用户手势」封装成 `typeInlineAndEnter()` 这类入口 |
| `lib/dom.mjs` | 最小 DOM：HTML 解析、元素查询、事件派发与冒泡 |

写新套件时只加 `scripts/e2e/<feature>.mjs`，复用 `lib/`；不要复制 DOM 或后端编排。

### 唯一的例外：`modelscope-credits.sh`（Go 整机层）

它不满足本目录「真 daemon + 真 app.js」的范式，因为那套范式要求驱动**面板**；而 ModelScope
魔粒额度面的失败面在**双 host 装配**上（额度端点挂在主站 `modelscope.cn`，对话挂在
`api-inference.modelscope.*`），面板反而只是消费 `/api/state`。于是那层用例写在 Go 里
（`internal/app/e2e_modelscope_{harness,site_fake,credits}_test.go`）：两台上游都是进程内
`httptest`，App/server.Handler 起**真 TCP 端口**，断言用真 HTTP 打到 `/api/*`，就地验证
「余额请求落在哪台 host、带哪种鉴权」这类只有装配层才看得见的证据。

```bash
bash scripts/e2e/modelscope-credits.sh          # 全部魔粒用例（-v）
bash scripts/e2e/modelscope-credits.sh -run FIFO   # 附加参数原样透传给 go test
```

覆盖票 02（余额/今日用量/面板口径/失败保快照）、票 03（逐笔到期 FIFO 与 ΣRemain 对齐）、
票 04（`claimed` / `already_claimed` / `no_campaign` 三态与文案）。**不联网、不打真上游**：
凭据是假 token，两个 host 都是 httptest。

## 变异验证（本层是否真的有牙）

逐个把产品代码**临时改回旧行为**、跑本套件、确认变红，然后还原（当前 tree 干净，
`git status` 只有本目录）：

| 变异 | 变红的 story |
|---|---|
| 行内备胎候选排除集改回「全部主力集合」 | US-01 / US-02 / US-05 / US-10 |
| 渲染前清洗改回「删除一切等于某主力的备胎」 | US-01 / US-04 / US-10 / US-11 / US-13 / US-14 |
| 提示文案改回「与主力模型互斥」 | US-07 / US-12 |
| `?` 帮助文案改回「主力不能作为备胎」 | US-08 / US-14 |
| `fbSelfLoop` 恒 false（自环守卫失效） | US-06 / US-07 / US-12 / US-14 |

US-14 是跨脚本护栏：它跑仓库既有的 `scripts/ui-static-check.mjs`，一旦有人把
「主力/备胎互斥」加回去，两层同时变红。

Go 整机层（`modelscope-credits.sh`）同样逐条验过：

| 变异 | 变红的用例 |
|---|---|
| 领取前的流水复读失效（`already_claimed` 分支永不命中） | `DailyClaimThreeStates/already_claimed`（`login/info` 被调 1 次） |
| `Remain` 取 `total_balance`（而非 `available_balance`） | `BalanceUsageAndPanel/明细`、`PerGrantExpiryFIFO`（ΣRemain ≠ available） |
| 临期窗口未接线（`ExpiringDays=0` ⇒ 回退默认 1 天） | `BalanceUsageAndPanel/面板口径`、`PerGrantExpiryFIFO`（`expiring_credits` = 0） |
| AI 区凭据加载改读 CN 目录（`LoadModelscopeAIDir` 串区） | `TwoZonesIndependent`（CN 账号数 2） |


## 已知限制

1. **不是真浏览器**：DOM 是自写最小实现，`innerHTML` 解析只覆盖面板自产的那点标记；
   布局、滚动、`sticky`、拖拽排序（`dragstart/drop`）未驱动。
2. **热生效只验证到「运行期状态 + 落盘配置」**：真正的「请求失败后切到备胎」需要打上游，
   按 AGENTS §0.1 不在本轮做，由 `internal/server/model_fallback_14018_test.go` 覆盖。
3. **每个 story 一个新 daemon**：约 +50ms/story，换来用例之间零污染；套件总耗时 ~6s。
4. **面板轮询被停用**：`setInterval` 在沙箱里替换为空实现，story 内的刷新一律显式驱动；
   否则 15s/30s 的轮询会在断言中途改状态。
5. 需要 Go 工具链：没有 `go` 时脚本在编译阶段直接失败（不静默跳过）。