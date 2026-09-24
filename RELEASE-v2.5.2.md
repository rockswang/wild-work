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
