# internal/chatgpt —— ChatGPT Web（`chatgpt/*`）渠道包

chatgpt.com **网页逆向**通道的内核：浏览器 TLS 指纹传输层、凭据、sentinel 对抗算法链路、
对话（多轮续接）、生图/改图、错误分类。对外经 `provider.Upstream` 接入网关，模型前缀 `chatgpt/`。

与官方 OpenAI API 无关（不用 `api.openai.com`、不用 `sk-` key），故面板显示名带 `Web`。

## 1. 上游出处与许可

| 项 | 值 |
|---|---|
| 上游仓 | <https://github.com/wild-work3/ww3-chatgpt-cli> |
| commit | `8c9bf2d5998403306c81779278f09acb5d0b06a6`（v2.0.0，2026-10-26 的抓包口径） |
| 许可 | **MIT**（`Copyright (c) 2026 rockswang`），全文见同目录 [`NOTICE`](NOTICE) |

本包有相当一部分文件是**移植**（不是重新实现）：协议形状、常量、sentinel 算法链、上传四步、
SSE 解析顺序都逐函数对照上游写成。因此：

- 上游改协议时，**先 diff 本清单**再去动代码，不要凭抓包重写一遍；
- 本包**不引入**上游的 CLI/IPC 外壳（见 §2.3），所以 diff 上游时忽略那些目录即可。

## 2. 同源清单（三列）

### 2.1 同源（逐文件对应，仅做「库化」适配）

| 上游（`internal/…`） | 本仓（`internal/chatgpt/…`） | 说明 |
|---|---|---|
| `cgpt/transport.go` | `transport.go` | TLS 指纹客户端 + 头族 + cookie jar + SSE 读取；代理隧道拆到 `proxy.go` |
| `cgpt/cred.go` | `cred.go` | 凭据嵌套形（ww3 `auths/chatgpt-*.json` 逐字段相同，零转换） |
| `cgpt/account.go` | `account.go` | 只搬 `Me` / `RefreshSession` / `Models`（额度与 OAuth 见 §2.3） |
| `cgpt/chat.go` | `chat.go` | 五步主链路（首页 → prepare → finalize → conduit → conversation） |
| `cgpt/sentinel.go` | `sentinel.go` | L1→L3 对抗算法链 + 漂移阻断 |
| `cgpt/sse.go` | `sse.go` | 首帧完整消息 + JSON Patch 增量（含裸 `{"v":[…]}` 变体） |
| `cgpt/citation.go` | `citation.go` | `<Cite refs={…}/>` 增量剥离（标记可能跨帧） |
| `cgpt/homecache.go` | `homecache.go` | 首页缓存（唯一内容在数小时内不变的步骤） |
| `cgpt/tokencache.go` | `tokencache.go` | sentinel token 预取缓存（账号级、落盘） |
| `cgpt/fingerprint.go` | `fingerprint.go` | 升级预警：上游对抗算法变更的可判定门禁 |
| `cgpt/upload.go` | `upload.go` | 上传四步（含对象存储那一步**不带** `Authorization`） |
| `cgpt/client.go` | `transport.go` 的 `NewWithConfig` + `provider.go` | 只搬组装（客户端/凭据/cookie jar）；凭据选择顺序见 §2.3 |
| `sentinel/pow/pow.go` | `sentinel/pow/pow.go` | requirements token 与 PoW（Go 重实现） |
| `sentinel/vm/{vm.go,hostenv.go}` | `sentinel/vm/{vm.go,hostenv.go}` | 挑战程序 VM + 宿主环境模拟 |
| `cgpt/util.go` | 无同名文件 | 小工具（`trunc` / `itoa` / `b64URL`…）就地内联，未留公共 util |

### 2.2 本仓适配（上游无对应物，或形状被改写成网关所需）

| 文件 | 做了什么 | 为什么 |
|---|---|---|
| `provider.go` | `provider.Upstream` 的账号面（`RefreshToken` / `Me` / `Models` / `Cookies`） | 网关按账号池取用凭据，上游是 CLI 单进程单凭据 |
| `upstream.go` | 编译期断言「本包实现全量 `provider.Upstream`」 | 少一个方法在装配期才炸，改到编译期 |
| `chat.go` | 入站形状换成 OpenAI Chat（`stream`/`messages`/`reasoning_effort`），只取最后一条 user 文本 | 上游入参是 CLI 的 `ChatRequest` |
| `sse.go` / `stream.go` | 出站 OpenAI 流形状（`delta` / `finish_reason` / `usage`）+ 流式与聚合两条路径 | 上游直接发 CLI 事件 |
| `convchain.go` | 多轮**前缀指纹链**：用「除最后一句外的全部历史」派生的键复用上游续接点，落 `data/chatgpt-convs.json`（上限 + LRU、独立于账号池状态文件） | 上游是 CLI，能自带会话 id；网关是无状态旁路（见 §2.3 的 `resolveParent`） |
| `image.go` | OpenAI `images/generations` + `images/edits` 形状（默认 `b64_json`，指针→真地址两步转换在包内完成） | 上游是 `images` 子命令 |
| `proxy.go` | 自写 CONNECT 隧道（不用 tls-client 内置拨号） | 后者拿 `config.timeout` 当 CONNECT deadline，流式渠道必须把该值设 0 ⇒ 隧道立刻 timeout |
| `import.go` | 从 ww3 客户端数据目录**定位**候选凭据文件（只定位，解析复用 `cred.go`） | 上游就是那个客户端本身，不需要「导入别人的文件」 |
| `classify.go` | 错误分类表（429 最先、403 二分、413、伪装 404）+ `provider.ErrKind` | 上游用自家 `Code` 体系（见 §2.3） |
| `constants.go` | 端点/常量真源（含「同源 commit」注记） | 上游常量分散在用到的地方 |
| `*_test.go` | 逐票补齐的单测（含 httptest 假上游） | 上游只有 `citation_test` / `conv_test` / `thoughts_test` |

### 2.3 已裁剪（明确**不搬**，附理由）

| 上游内容 | 为什么不要 |
|---|---|
| **匿名分支**：`/backend-anon/*`（对话与模型）、`Config.Anon`、`sendAnon`、`prefetchAsync(anon)`、`client.go` 的「都没有 → 匿名」兜底 | spec §8 明确不做匿名：匿名链路的上游阈值与风控口径未实证，且它不是本工具能「授权并负责」的账号形态 |
| **`ConvGet` + `resolveParent` 的「按会话 id 取延续点」兜底**（`GET /backend-api/conversations/<id>` 取 `current_node`） | 本仓入站没有会话 id（客户端只给 `messages`）⇒ 该兜底永远拿不到 id；改用 `convchain.go` 的前缀指纹链，认不出就静默新建。**注意**：`ConvGet` 是纯零配额读，但仍搬进来只会成为死代码 |
| `event.go`（`Event` / `Code` / `Retryable` / `KindAndScope`） | ww3 的 CLI 事件与退出码体系；本仓错误面是 `provider.ErrKind`（`classify.go`） |
| `cmd/chatgpt`、`internal/cli`、`internal/protocol`、`internal/channel`、仓根 `conformance.json` | `ww3-plugin-cli/1` IPC 契约与命令外壳；本包是网关进程内的渠道，由 `internal/server` 负责协议面 |
| `conv.go` 的会话管理 CRUD（`ConvList` / `ConvListAll` / `ConvRename` / `ConvDelete` / `ConvStop` / `ConvInit` / `ConvHistory`） | 本仓不提供「会话管理」功能，会话由上游维护、本仓只记续接点 |
| `account.go` 的 `WhamUsage` / `Limits` / `RefreshOAuth` / `AccessExpiry` | 网页套餐额度**没有可读接口** ⇒ 面板「不适用积分」；OAuth 刷新路径（`RefreshOAuth`）在网页 cookie 形态下未实证，走 `SessionDead` 可自愈标记更安全 |
| `client.go` 的凭据选择顺序（`--auth` / `channel_config.auth` / 内联 / `auths/` 第一条 / 匿名） | 本仓凭据由账号池（`auths/chatgpt-*.json`）下发，选择权不在渠道包 |

## 3. 上游升级时怎么做

1. 看一眼 `constants.go` 头部的 commit 注记，与本文 §1 的 commit 对齐；
2. 把上游 checkout 到新版本，**只 diff §2.1 的文件**：

   ```bash
   git clone https://github.com/wild-work3/ww3-chatgpt-cli /tmp/ww3-cgpt
   git -C /tmp/ww3-cgpt checkout <新 commit>
   diff -u /tmp/ww3-cgpt/internal/cgpt/chat.go internal/chatgpt/chat.go
   ```

3. 判据分层（沿用仓库纪律：实测 > 抓包 > 参考实现源码 > 推断）：
   - **协议形状/常量**变了 ⇒ 照抄即可（§2.1 就是为此存在）；
   - **对抗算法**（`sentinel/`、`fingerprint.go`）变了 ⇒ 先跑本包单测，再看 `fingerprint.go` 的漂移门禁是否 fail-closed（宁可「不发业务请求」也不要「带旧基线硬发」）；
   - 拿不准时**不要**用生产账号试跑（§0.1：零费用端点 + 离线取证优先，冒烟只在小号上单发）。
4. 改完同步：`constants.go` 的 commit 注记、本文 §1 的 commit、`NOTICE` 的 commit。

## 4. 已知限制

- **必须走代理**：`chatgpt.com` 境内不可达，客户端只认 `HTTPS_PROXY`/`HTTP_PROXY`（含 `socks5://`）；
  未配代理表现为 502/403，属**预期**而非缺陷。
- **无工具调用**：`tools` / `tool_choice` 入站照收但不使用（不 400、不编造 `tool_calls`）。
- **只发最后一条用户消息**：上游自己维护历史，多轮靠 `convchain.go` 的前缀指纹链接续；认不出就安静新开一轮。
- **对抗算法会漂移**：sentinel 的五步链路与证明算法随上游发版变化，靠 `fingerprint.go` 在业务请求前识别并 fail-closed。
- **输入上限**：约 24K token / 122.9K 字符，超限上游回 413（分类为 `ErrPromptTooLong`，不罚号不换号）。
- **无 token 用量**：上游 `usage` 为空 ⇒ 流水记 `src="none"`、token 记 0、只计请求数。