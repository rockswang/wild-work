# GLOSSARY

本仓库的**领域词汇表**（single context，按 `docs/agents/domain.md`（本地约定，仓库不收录） 的约定）。
写代码、提工单、写测试名时**使用这里的词**；不确定某个概念该叫什么时先查这里，缺了再加。

只收录「含义不止字面」「容易叫错」「一处改动会牵连多列跨模块」的术语；通用的编程词不收录。

---

## 魔粒（magicubes）

ModelScope / ModelScopeAI（魔搭）站内的**积分单位**，官网计费与账单里就叫「魔粒」。

- **接口**：主站（`modelscope.cn` / `modelscope.ai`，**不是**对话基址 `api-inference.*`）的
  `GET /openapi/v1/magicubes/balance`（`Authorization: Bearer <ms- token>`）返回
  `total_balance` / `available_balance` / `frozen_amount`。
- **映射**：本工具的「积分余额」取 `available_balance`（可消耗）；
  `frozen_amount` 是冻结部分，进「不可用」小计（`Usable=false`），**不**并入可用余额。
- **面板文案**：渠道级显示术语走后端 `AccountView.credits_term = "魔粒"`，
  前端渲染成「可用魔粒 / 魔粒明细」。**这只是文案**：
  `credits_unit` 保持空串 ⇒ 魔粒**仍是积分**，并入积分合计与积分折线图，
  与 `credits_unit="token"`（ZCode / MonkeyCode 的 Token 计数）是**两条不同口径**。
- **不要叫**：「魔方」「积分次数」「Token 额度」。「剩余次数」是我们**从不**展示的东西
  （上游不给次数口径，编造等于说谎）。
- **逐笔到期**：`GET /openapi/v1/magicubes/transactions` 的 `EARN.expire_at` 是
  `YYYY-MM-DD HH:MM:SS`（**UTC+8 墙钟，非 RFC3339**）⇒ 按到期升序 FIFO 扣 `SPEND_CONFIRM`、
  余量挂最后一笔；拆分条目共用同一 `Name` 且不设 `Key`（R107 同款），所以 `ledger` 不会产生假流水，
  且到期清零不会被记成「消耗」。
- **每日领取**：上游**没有独立签到端点**，领取动作 = 幂等 `GET /api/v1/users/login/info`
  （`Cookie: m_session_id=<ms- token>`）；「已领取 / 领到 / 今日无活动」三态借道 `transactions` 的当日
  `EARN` 判定。金额只在 `CheckinReport.Msg` 里展示，**不重复入账**（账本按差分记 earn）。
- **两区不通用**：同一枚 token 在 `modelscope.cn` 与 `modelscope.ai` 是两个账号（实测：CN token 打 AI 站回
  401）⇒ 两个渠道、两个池、互不兜底；AI 站是否有真实魔粒**未实证**。
- 相关：今日用量（`GET /api/v1/inference/rate-limit` 的 `currentUsagePerDay`）单位按 token 计，
  走 `InfoOnly` 明细条**只作展示**，不进任何积分算术。完整决议见 R161（更正 R159 的额度口径）。

## 积分（credits）

本工具所有渠道的**默认计量口径**（`credits_unit = ""`）：参与账号卡片主数字、汇总条合计、
积分折线图、`ledger` 差分记账（earn / spend / expire）。

- 各渠道的上游单位不同（魔粒 / 智点 / 每日额度…），面板用 `credits_term` 改**文案**，
  但只有 `credits_unit` 能改**口径**——两者不可混用（把魔粒写成 `credits_unit="token"`
  会把它踢出积分合计，这是 R17「双流水」纪律最容易踩的一脚）。
- 与积分并列的另一套口径是 **Token 用量**：按请求实测计量，只进 stats 的 tokens 列，
  **不**与积分相加、不互相折算。

## 可用 / 不可用额度

`provider.ResourceItem.Usable` 的两种取值，决定条目进哪一个小计：

- **可用**（`Usable=true`）：本工具路由与合计使用的额度；
- **不可用**（`Usable=false`）：账号名下但本工具动不到的额度（TraeWork 的官方客户端专用池、
  ModelScope 的冻结魔粒），只在面板「不可用」角标里展示，**绝不**进 `Pick()` 排序。
- 第三种是 **只作展示**（`InfoOnly=true`，量纲与积分不同，如今日 Token 用量）：
  既不进「可用」也不进「不可用」，只出现在明细浮层。

## 临期

可消耗额度中**在展示窗口内到期**的部分（`config.schedule.expiring_days` = 1/2/7 个自然日，
今天算第 1 天）。**只用于展示**（面板三口径 + 汇总条角标）。

- **选号不看临期体量**：`pool.Pick()` 的第一排序键是 `provider.EarliestExpire`
  （最早到期**时刻**升序，同刻按 uid 的 ASCII 序），不是「临期额度大小」（R112）。
- 与「今日作废」（`credit_expired`，往回看：今天已经到期消失的额度）是**相反方向**的
  两个口径，不可混为一列（R102）。

## 冷却（冻结）

账号在池子里**临时不可选**的状态。用户口语里的「冻结」就是它。两种程度：

- **长冷却**：上游说这个账号**钱/权益不够**（余额、免费额度、积分耗尽）⇒ 整个号移出候选；
- **短冷却**：限流、账号级故障等可期望自行消退的原因。

两条铁律：

- **与「停用」不是一回事**：停用是**持久**状态，需人工（重登/重新启用）才能恢复；
  冷却**到期自解**，不靠人工。把「积分耗尽」做成停用就变成真卡死。
- **与「模型级冷却」不是一回事**：后者只封「账号 × 该模型」，同号其他模型照常参与选号。

## 不适用（credits_na）

该渠道**没有积分概念**（上游确无余额端点，或凭据本体不是账号）：面板显示「不适用」而不是 0，
并隐藏「刷新积分」与明细入口（0 会被用户读成「额度已用光」）。

- 判据的唯一来源是后端 `App.creditsNAKind`（内建 oczen / kilo / cline + 自定义渠道）。
- **与「虚拟账号」「默认收起」正交**：`credits_na` 不决定账号是否默认收起
  （R131 踩过：粘完凭据的 comate/cline 账号被当噪声默认隐藏，表现为「账号没刷出来」）。

## 虚拟账号

没有账号概念的渠道（oczen 匿名通道 / kilo 免费池）注入的占位条目：不可删除、不可停用、
不可改名，默认收起。

- 判据是 `VIRTUAL_ACCOUNTS = {oczen, kilo}`，**不是** `credits_na`（两者正交，见上）。
- 自定义渠道与 ModelScope 两区都是**真多账号池**（一枚凭据 = 一个账号），不是虚拟账号。
