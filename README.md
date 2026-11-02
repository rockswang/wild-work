# wild-work

> 多渠道账号聚合桌面工具——把 WorkBuddy(CodeBuddy) 国内版/国际版、TraeWork、QoderCN、QoderCOM（国际版）、千问办公、MonkeyCode 平台托管模型、小浣熊、Loomy（讯飞）、智谱清言、Cline（官方 API Key 免费池）、WPS Comate、华为云果办 OfficeAce 的多个账号聚合成一个 OpenAI 兼容 API，并额外提供**无需账号**的 OpenCodeZen 匿名免费通道，双击启动，浏览器管理。

[![GitHub](https://img.shields.io/badge/GitHub-rockswang%2Fworkbuddy--wild-blue)](https://github.com/rockswang/workbuddy-wild)

## 功能

- **三接口协议兼容**：`/v1/chat/completions`（OpenAI Chat）+ `/v1/responses`（OpenAI Responses）+ `/v1/messages`（Anthropic Messages，含 `count_tokens`）+ **`/v1/systemone`（Jev 结构化决策）**，可直接接入 `codex` CLI、`claude` CLI 与需要语义决策的自动化流程
- **OpenAI Images 生图**（glm 渠道）：`/v1/images/generations`（文生图）+ `/v1/images/edits`（参考图改图），OpenAI Images API 兼容，外部工具零改造接入（详见下文「生图 API」）
- **请求体指纹脱敏**：自动清除 Claude Code / Codex CLI 注入的模板句，防止上游 11128 内容拦截
- **思考链死循环检测（防癫拦截）**：流式生成中逐帧检测推理复读/空转，命中即中断 SSE 并向客户端返回错误（`code=upstream_reasoning_loop`），由下游 AI 客户端自行重试恢复；不罚账号（模型级问题），默认开启，面板「设置」可关闭
- **OpenAI 兼容代理**：`/v1/chat/completions`、`/v1/models`，支持流式/非流式，模型前缀路由
- **错误分类精细化**：区分「请求问题」与「账号问题」——内容拦截/上下文超限不罚号，限流/风控/账号故障分级冷却，429 不再误判余额耗尽；**单账号渠道（OpenCodeZen）例外：任何错误均不冷却**（无号可轮换，惩罚即等于整渠道下线）
- **二十二渠道聚合**：23 条内建渠道统一按「模型前缀路由」接入（`渠道前缀/<model>`），粘性路由优先复用账号以提升会话缓存利用率，选号按**最早到期优先**（快过期的积分先烧）；其中旧 Qoder（`qoder/*`）仅保留路由、界面已下线，可添加的是 22 条。渠道明细见下文[二十二渠道聚合](#二十二渠道聚合)
- **Cline 免费池**（`cline/*`）：粘贴官方 `sk_` API Key 即用（`app.cline.bot` 自行注册并创建，Key 不过期、不轮换、不与浏览器/客户端互踢）；本渠道**只调度免费模型**，多把 Key 须来自**多个独立账号**（官方额度按账号计，同账号多 Key 共享一份额度，面板会自动识别并提示）
- **匿名免费通道**（OpenCodeZen）：内置 `public` 凭证即可调用 Zen 上的免费模型（含 `big-pickle`），无需注册/登录；面板固定一个 `[OpenCodeZen] 匿名` 条目（不可增删停用、无签到、积分显示「不适用」）
- **自动签到**：每日定时签到领额度（Qoder 双区含 10:00–12:00 窗口重试，避免上游活动延迟导致漏领；小浣熊走工作台接口触发**惰性落账**的每日积分，见下），token 保活，冷却状态机。**没有签到活动的渠道**（千问办公/Cline/Comate/OpenCodeZen/Kilo/ZCode/Gemini）不参与签到调度，面板显示灰色「无签到」而不是红色「签到失败」
- **自动领日活奖励**（WorkBuddy 国际版）：定时自动用免费模型对话保活，自动领取每日活跃奖励，无需手动签到
- **用量与流水统计**：token 流水（渠道×模型）与积分流水（入项/消耗/过期）双口径记账，面板折线图 + 模型用量榜（含**缓存利用率**与**当前速度**列，速度是最近 60 秒的滑动窗口速率，取自运行统计引擎同源数据）+ 积分流水对账，选号按最早到期优先；区间（今日/7日）选择**自动记忆**
- **运行统计面板**（第三 tab）：实时视图——今日签到收入/花费/账号数里程碑、平台概览、使用中接棒（粘性路由）、最近临期与预计作废、模型消耗、请求日志（含 TTFB/吞吐）、异常账号、运行日志，30s 自动刷新；与上面「用量与流水」是**两套并列口径**（各自标注，不做数字对齐）
- **Web 管理面板**：账号管理（添加/签到/刷新/停用/删除）、积分明细、模型列表和费率、API 配置、统一设置弹层（含**备用上游通道**与**自定义渠道**）；监听非本机地址时强制管理密码登录（HttpOnly cookie 会话）
- **自定义渠道接入**（`custom_channels`）：在面板设置中接入任意第三方 OpenAI 兼容 API（如官方 StepFun、本地 CLIProxy、DeepSeek 等），通过 `渠道标识/模型名` 统一路由并集中查看用量流水（ledger）与实时运行统计（TTFB、吞吐、请求日志）
- **系统托盘**：常驻右下角，双击打开面板，右键菜单操作
- **跨平台**：Windows（完整支持）、macOS（代码已就绪，CI 构建）、Linux（无头模式）
- **developer→system 角色转换**：自动将下游 Agent 发送的 `<developer>` 角色改写为 `<system>`，避免上游触发内容过滤
- **上游协议头仿真**：出站自动注入官方客户端指纹头（UA、`X-IDE-Version`、`X-Product`、会话头族等，按官方桌面客户端 5.8.3 对齐），降低风控误判风险

## 二十二渠道聚合

`provider.BuiltinKinds()` 共声明 **23 条内建渠道**，统一按「模型前缀路由」接入（`渠道前缀/<model>`），粘性路由优先复用账号以提升会话缓存利用率，选号按**最早到期优先**（快过期的积分先烧）；其中旧 Qoder（`qoder/*`）仅保留路由、界面已下线 ⇒ 面板上可添加的是 **22 条**。另有自定义渠道（`custom_channels`，见「自定义渠道接入」）可接入任意 OpenAI 兼容 API。

| 渠道 | 模型前缀 | 接入方式 | 说明 |
|------|----------|----------|------|
| WorkBuddyCN（CodeBuddy） | `workbuddy/*` | 浏览器登录 | WorkBuddy 国内版 |
| WorkBuddyAI | `workbuddyai/*` | 浏览器登录 | WorkBuddy 国际版，与国内版完全独立 |
| TraeWork | `traework/*` | 浏览器登录 | |
| TraeCode | `traecode/*` | 复用 TraeWork 账号 | Trae 代码版（`function=solo_agent`），与 TraeWork 同一上游、共用账号 |
| QoderCN | `qodercn/*` | 浏览器登录 | |
| QoderCOM | `qodercom/*` | 浏览器登录 | Qoder 国际版 |
| 千问办公 | `qwenwork/*` | 浏览器登录 | |
| MonkeyCode | `monkeycode/*` | 从客户端导入 | 平台托管模型 |
| 小浣熊 | `raccoon/*` | 网页版自动登录（CDP）／从客户端导入 | 两条添加路径：主按钮拉起独立 profile 浏览器登录后经 CDP 捕获凭据（跨平台），次按钮复用本机客户端凭据 |
| Loomy（讯飞） | `loomy/*` | 从客户端导入 | 导入型渠道：凭据从**本机已登录的官方客户端**读取；上游无 refresh 端点，session 约 14 天，到期需重新导入 |
| 智谱清言 | `glm/*` | CDP 自动捕获 | **网页版私有接口**（非开放平台），凭据是浏览器 Cookie 里的 `chatglm_refresh_token`；新账号须在智谱清言 App 登录一次才发放初始额度 |
| Cline | `cline-free/*`（兼容旧写法 `cline/*`） | 粘贴官方 API Key | 只调度免费模型；Key 不过期、不轮换；官方额度按账号计，多把 Key 须来自**多个独立账号**（同账号多 Key 共享一份额度，面板会自动识别并提示） |
| WPS Comate | `comate/*` | 扫码登录 / 本机浏览器接管 / 手工粘贴 | WPS 网页版模型代理（`comate.wps.cn`）：**扫码登录**为默认路径（服务端向 `qr.wps.cn` 申请二维码，面板就地显示，手机 WPS 扫码确认后由服务端换出 Cookie —— 无需桌面、无需浏览器，NAS/docker 可用；**Cookie 一出现 ≠ 登录完成**，判据是上游 `/models` 校验通过）；有桌面的本机部署可配置 `login.comate_browser_cdp` 接管**已在跑**的浏览器（`--remote-debugging-port`）直接读登录态；任何环境都能展开「手工方式」粘贴 `wps_sid`（整段 Cookie 头也直接吃）；模型列表由上游 `/models` 动态下发（GLM/DeepSeek/MiniMax/MiMo 等，上下文窗口为上游官方声明）；**额度查得到**（主积分 = `/llmproxy/v1/user/token-usage` 的每日额度剩余；该池为空的账号回退 `/api/coserve/v1/billing/v3/credits/trial` 的「免费额度」，见 R136/R138。明细里每日/每月/免费额度各自成条，倍率取每款模型的 `consume_ratio`），并支持「刷新积分」；积分按 WPS 账号计，多份 sid 须来自**多个独立账号**（面板会自动识别并提示） |
| WPS 灵犀 | `lingxi/*` | 扫码 / 粘贴 `wps_sid`（CDP 自动捕获） | WPS 灵犀（`lingxi.kdocs.cn`）网页版助手：两段式对话（先建 session 再 completions SSE），模型目录取上游 `plans` 动态下发，`reasoning_effort` 三档映射；每日签到 + 余额查询（智点口径）。凭据就是 WPS 网页版 Cookie 里的 `wps_sid`，与 comate 共用同一份 Cookie 但账号 uid 带 `lxsid-` 前缀，两渠道互不串号 |
| 华为云果办 OfficeAce | `officeace/*` | 自助 OAuth／从客户端导入 | 华为云 AgentArts 模型网关；两条添加路径：主按钮自助 OAuth（华为云授权码 + PKCE + DPoP，跨平台），次按钮读本机 OfficeAce 桌面端凭据（临时 AK/SK 经 DPAPI 解密，仅 Windows）；对话走不过期的网关 Basic 凭据，每日签到的临时凭据约 2 小时有效并自动续期 |
| OpenCodeZen | `oczen/*` | 无需账号 | 匿名免费通道，内置 `public` 凭证即可调用 Zen 免费模型 |
| Kilo | `kilo/*` | 无需账号 | 免密免费池 |
| MiniMax Code | `minimax/*` | 设备码授权登录 | |
| LobsterAI（有道龙虾） | `lobsterai/*` | 本机回调授权登录 | |
| ZCode（智谱） | `zcode/*` | 设备授权登录 | |
| Gemini | `gemini/*` | 本机回调授权登录 | Google Code Assist |
| CodeArts（华为云） | `codearts/*` | PKCE 授权登录 | |
| 旧 Qoder | `qoder/*` | — | 路由保留，已从界面下线 |

## 项目渊源

本项目最初算法来源于 [Sliverkiss/](https://github.com/Sliverkiss/) 大佬的 xxx2api 系列项目，本项目针对多渠道进行了聚合，针对 Windows 环境进行了适配，降低了使用门槛，并提供跨平台 Web 管理界面。

### 声明

**本项目只是各上游渠道的集成与聚合，本身不涉及任何逆向分析和破解工作。**

即：本仓库不产出、不包含、不分发任何协议逆向研究成果。各渠道的接口参数、签名算法、风控指纹等底层知识，全部来自社区其他开源项目的公开成果；本项目所做的是**在既有公开成果之上做多渠道路由、账号池调度、协议适配与界面封装**，并提供跨平台、低门槛的使用形态。所有逆向相关的功劳与风险归于下述各上游项目作者。

### 致谢

感谢以下开源项目及其作者。它们分别完成了各渠道的接口逆向与协议封装，是本项目各渠道能够落地的前提：

| 项目 | 贡献 |
|------|------|
| [Sliverkiss/workbuddy2api](https://github.com/Sliverkiss/workbuddy2api) | WorkBuddy(CodeBuddy) 国内版接口逆向，本项目最初的算法来源 |
| [Sliverkiss/qoderwork2api](https://github.com/Sliverkiss/qoderwork2api) | QoderWork 接口逆向，Qoder 渠道参考 |
| [wicm84266964/Buddy2api](https://github.com/wicm84266964/Buddy2api) | 千问办公渠道逆向参考 |
| [wpy030414/xrl-router-plugin-qwenwork](https://github.com/wpy030414/xrl-router-plugin-qwenwork) | 千问办公渠道逆向参考 |
| [iceloon/dsh-workbuddyai-connect](https://github.com/iceloon/dsh-workbuddyai-connect) | WorkBuddy 国际版接口逆向，国际版渠道主要参考 |
| [corrinehu/dsh-workbuddy-connect](https://github.com/corrinehu/dsh-workbuddy-connect) | WorkBuddy 渠道架构参考（MIT） |
| [Zhengyuuuui/qoder2api](https://github.com/Zhengyuuuui/qoder2api) | Qoder 渠道端点与逻辑比对参考 |
| [icebears111/qoderwork2api](https://github.com/icebears111/qoderwork2api) | Qoder 渠道端点与逻辑比对参考 |
| [zhangdailin/Orchids-2api](https://github.com/zhangdailin/Orchids-2api) | Qoder 渠道端点与逻辑比对参考 |
| [jasonxu114514/opencode2api](https://github.com/jasonxu114514/opencode2api) | OpenCodeZen（oczen）匿名渠道 endpoint 特殊要求分析 |
| [FishBottle7/opencode2dsh](https://github.com/FishBottle7/opencode2dsh) | 同上，OpenCode 相关补充参考 |
| [ttales430/glm2api](https://github.com/ttales430/glm2api) | 智谱清言（chatglm.cn）网页版私有接口协议逆向，glm 渠道协议依据 |
| [codkeep/MonkeyCodeReverseEngineer](https://github.com/codkeep/MonkeyCodeReverseEngineer) | MonkeyCode 渠道协议对照参考（Apache-2.0） |
| [xxhhlk/raccoon2api](https://github.com/xxhhlk/raccoon2api) | 小浣熊渠道协议逆向（MIT），raccoon 渠道依据 |
| [xxhhlk/loomy2api](https://github.com/xxhhlk/loomy2api) | 讯飞 Loomy 渠道协议逆向（MIT），loomy 渠道依据 |
| [Q-shuang-dot/officeace2api](https://github.com/Q-shuang-dot/officeace2api) | 华为云果办 OfficeAce 渠道协议依据（MIT）——V4/V11 华为云签名（`src/quota.mjs`）、网关错误码分类（`src/server.mjs`）、`max_tokens` 推理预算常量（`src/config.mjs`）、桌面端凭据发现与 DPAPI/AES-256-GCM 档案解密（`src/discover*.mjs` / `src/dpapi.mjs`）、DPoP + PKCE 自助 OAuth（`src/oauth.mjs`）、refresh_token 一次性轮换（`src/refresh.mjs`）；本项目只取协议与凭据链路，其**批量注册/接码**、Web 控制台、Windows 计划任务看门狗未采用 |
| [luawei1/cline2api](https://github.com/luawei1/cline2api) | Cline 渠道协议逆向参考（MIT；虽未采用其 WorkOS OAuth 方案，模型/对话协议知识有参考，`recommended-models` free 池机制亦借鉴自其分析） |
| [DiEr-TouShou/dsh-connect-comate](https://github.com/DiEr-TouShou/dsh-connect-comate) | WPS Comate 渠道协议逆向参考（MIT；虽未采用其 DSH 插件封装与凭据密封存储，comate 网关的 Cookie-only 鉴权形态、思考档位线值、错误文案分类与请求体规范化的知识有参考） |
| [KevinFX-code/magpie-wps-lingxi](https://github.com/KevinFX-code/magpie-wps-lingxi) | WPS 灵犀（`lingxi/*`）渠道协议依据（MIT）——两段式对话（先建 session 再 completions）、三专用头、SSE 9 帧序列、签到与智点余额端点均由此逆向而来；本仓只取协议知识、未复用其代码 |
| [donghangxunlang-cmd/dsh-attention-health](https://github.com/donghangxunlang-cmd/dsh-attention-health) | 思考链死循环（发癫）检测算法来源——`lib/guard.js` 的四条复读/空转判据与防误杀护栏（本项目只取死循环检测，健康评估/工作交接/会话日志未采用） |
| [ttales430/wild-work PR #46](https://github.com/rockswang/wild-work/pull/46) | 智谱清言渠道实现（含 CDP 登录编排） |

> 若上述项目作者认为本项目的引用方式不当，请提 issue 联系，我们会立即调整或移除相关内容。

同时感谢给本项目 PR 的各位贡献者！

## 使用方式

### Windows

1. 从 [Releases](https://github.com/rockswang/workbuddy-wild/releases) 下载 `wild-work.exe`
2. 放到任意目录，双击启动
3. 右下角出现 W 图标，**双击托盘图标** → 浏览器打开 Web 管理面板
4. 在面板中点击「+ WorkBuddyCN」/「+ WorkBuddyAI」/「+ TraeWork」/「+ QoderCN」/「+ QoderCOM」/「+ 千问办公」/「+ 智谱清言」/「+ OfficeAce」添加账号
   （OpenCodeZen 无需添加账号，启动即已就绪；Cline 无需登录流程，直接粘贴 API Key；WPS Comate 默认走「扫码登录」（手机 WPS 扫码确认即可，无桌面环境也能用），也可粘贴整段 Cookie 兜底）
5. 根据下方配置说明接入你的 AI 客户端

### 托盘菜单

- **打开主界面**：在浏览器中打开管理面板
- **查看日志**：用记事本打开运行日志
- **退出**：退出程序

### 无头模式（Linux 服务器）

```bash
./wild-work --no-tray
```

启动后打印 API 地址、Key 等信息，阻塞运行，Ctrl+C 退出。

## 配置 AI 客户端

### 1. 获取模型列表

wild-work 的 `/v1/models` 端点返回当前所有可用模型。在终端中执行：

```bash
curl -s -H "Authorization: Bearer WildWorkAPI" http://127.0.0.1:7863/v1/models
```

### 2. 配置 Pi（models.json）

Pi 不支持自动拉取模型列表，需要手动编辑 `~/.pi/agent/models.json`（Windows 路径 `C:\Users\<用户名>\.pi\agent\models.json`），在 `providers` 中加入 wild-work 配置：

```json
{
  "providers": {
    "wild-work": {
      "name": "wild-work",
      "api": "openai-completions",
      "baseUrl": "http://127.0.0.1:7863/v1",
      "apiKey": "WildWorkAPI",
      "models": [
        {
          "id": "workbuddy/auto",
          "name": "自动路由 (WorkBuddy)",
          "reasoning": false,
          "input": ["text"],
          "contextWindow": 168000,
          "maxTokens": 32000,
          "compat": { "maxTokensField": "max_tokens" }
        },
        {
          "id": "workbuddy/deepseek-v4-pro",
          "name": "DeepSeek V4 Pro (WorkBuddy)",
          "reasoning": true,
          "input": ["text"],
          "contextWindow": 1000000,
          "maxTokens": 50000,
          "compat": {
            "thinkingFormat": "deepseek",
            "supportsReasoningEffort": true,
            "maxTokensField": "max_tokens"
          }
        },
        {
          "id": "workbuddy/glm-5.3",
          "name": "GLM-5.3 (WorkBuddy)",
          "reasoning": true,
          "input": ["text"],
          "contextWindow": 1000000,
          "maxTokens": 48000,
          "compat": { "maxTokensField": "max_tokens" }
        },
        {
          "id": "traework/DeepSeek-V4-Pro",
          "name": "DeepSeek V4 Pro (TraeWork)",
          "reasoning": true,
          "input": ["text"],
          "contextWindow": 1000000,
          "maxTokens": 50000,
          "compat": {
            "thinkingFormat": "deepseek",
            "supportsReasoningEffort": true,
            "maxTokensField": "max_tokens"
          }
        },
        {
          "id": "traework/glm-5.2",
          "name": "GLM-5.2 (TraeWork)",
          "reasoning": true,
          "input": ["text"],
          "contextWindow": 1000000,
          "maxTokens": 48000,
          "compat": { "maxTokensField": "max_tokens" }
        },
        {
          "id": "workbuddyai/deepseek-v4.1-flash",
          "name": "DeepSeek V4.1 Flash (WorkBuddy 国际版)",
          "reasoning": true,
          "input": ["text", "image"],
          "contextWindow": 1000000,
          "maxTokens": 128000,
          "compat": {
            "thinkingFormat": "deepseek",
            "supportsReasoningEffort": true,
            "maxTokensField": "max_tokens"
          }
        }
      ]
    }
  }
}
```

> 上面只列出了部分常用模型，完整列表请通过 `/v1/models` 端点获取后自行添加。

### 3. 其他客户端

支持 OpenAI 兼容 API 的客户端均可接入：

```
Base URL: http://127.0.0.1:7863/v1
API Key:  WildWorkAPI
```

模型 ID 需带渠道前缀：`workbuddy/<model>`、`workbuddyai/<model>`、`traework/<model>`、`traecode/<model>`、`qodercn/<model>`、`qodercom/<model>`、`qwenwork/<model>`、`monkeycode/<model>`、`raccoon/<model>`、`loomy/<model>`、`glm/<model>`、`oczen/<model>`、`cline/<model>`（旧 `qoder/<model>` 仍可用，已从界面下线）。

#### Cline 免费池（`cline/*`）

粘贴官方 **API Key** 即用，无需在本机安装任何 Cline 客户端：

1. 打开 <https://app.cline.bot/> 并注册 Cline 账号；
2. `Account / API Keys` → `Create API Key`；
   Key 形如 `sk_` + 64 位十六进制，**只在创建时完整显示一次**；
3. 面板「账号管理」→「＋ Cline」→ 粘贴 Key → 添加。

```bash
curl http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer WildWorkAPI" -H "Content-Type: application/json" \
  -d '{"model":"cline-free/step-5-preview","messages":[{"role":"user","content":"你好"}]}'
```

- 可用模型：完全跟随 Cline 官方免费池动态下发（`recommended-models` 的 free 组，
  原样透传，不做任何本地筛选）。**模型 ID 就是上游 ID**（自带 `cline-free/` 命名空间），
  典型如 `cline-free/step-5-preview`、`cline-free/mimo-v2.6-flash`；
  完整清单见 `/v1/models` 中 `cline-free/` 开头的条目。
  旧版双层写法 `cline/cline-free/*` 仍可路由（兼容老配置），但对外只广告前者。
- **必须使用多个独立 Cline 账号各自的 Key**：官方免费额度按**账号**计
  （其权益字段名为 `…UsageCostUSDPerUser`），**同一账号建多把 Key 共享一份额度，不会翻倍**。
  面板在检测到新 Key 与已有账号同属一个 Cline 账号时会给出提示。
- Key **不过期、不轮换**，也不会与浏览器 / 官方客户端互相挤下线（这是选 API Key 而不是
  逆向 OAuth 登录的主要原因）。
- 本渠道**只暴露官方免费池**（`cline-free` 命名空间）：订阅池 `cline-pass` 与云池 `cline-cloud`
  不在使用范围内（请求它们会得到 `ENTITLEMENT_ERROR`）。上游无余额/额度查询接口，
  因此积分显示「不适用」，实际花费见「用量与流水」（来自逐请求 `usage.cost`）。
- 不做价格承诺：官方「free」标记与实际计价并不一致（实测同前缀有扣费、
  非 free 后缀反倒 `cost=0`，且同一模型相邻两次请求计价可能漂移），
  一切以实测 `usage.cost` 为准。
- 免费池的上游限流（OpenRouter 侧 `limit_rpd` 日限 / 临时限流）属**共享出口**，
  与你的账号数无关：本渠道对此只做模型级冷却，不会因此惩罚或轮换账号。

#### OpenCodeZen 匿名免费模型（`oczen/*`）

无需任何账号或登录，直接可用（免费清单由上游动态下发，随时增删）：

```bash
curl http://127.0.0.1:7863/v1/chat/completions \
  -H "Authorization: Bearer WildWorkAPI" -H "Content-Type: application/json" \
  -d '{"model":"oczen/mimo-v2.6-flash-free","messages":[{"role":"user","content":"你好"}]}'
```

- 常见可用：`oczen/big-pickle`、`oczen/mimo-v2.6-flash-free`、`oczen/mimo-v2.5-free`、
  `oczen/nemotron-3-ultra-free`、`oczen/nemotron-3.5-lightning-free`、`oczen/ling-3.0-flash-fin-free`；
  完整清单见 `/v1/models` 中 `oczen/` 开头的条目。
- 部分免费模型有**地域限制**（如 `muse-spark-*-contributor-free` 在国内直连返回 403 RegionError）：
  列表仍会列出。本工具的**备用上游通道**就是为这类场景准备的（见下文设置章节），
  但该渠道上游对抗策略变更、当前暂不可用，待恢复后再接入自动改道。
- Cline 的 `cline-free/muse-spark-1.3-contributor` 已接入自动改道：国内直连回 403
  `not available in your region` 时，会在同一次请求内经备用通道重发（未配则原样透传）。
- 免费额度/限流由上游 OpenCode Zen 控制，本工具不做额外限制；上游未见请求数配额，
  但高并发时延会上升。

### 4. 三种接口协议

除 OpenAI Chat Completions 外，同一端口还兼容 **OpenAI Responses** 与 **Anthropic Messages**，
可用官方 CLI 直接接入（无需改代码）：

| 端点 | 协议 | 适用客户端 |
|------|------|-----------|
| `POST /v1/chat/completions` | OpenAI Chat | 大多数第三方客户端、`openai` SDK |
| `POST /v1/responses` | OpenAI Responses | `codex` CLI、`openai` SDK 的 Responses API |
| `POST /v1/messages` | Anthropic Messages | `claude` CLI、`anthropic` SDK |
| `POST /v1/messages/count_tokens` | Anthropic count_tokens | Claude Code 上下文预算用 |

鉴权：OpenAI 侧用 `Authorization: Bearer <API Key>`；Anthropic 侧 `x-api-key: <API Key>`
或 `Authorization: Bearer` 均可。

**模型名可省略渠道前缀**，在 `config.json` 的 `compat` 段配置映射：

```json
"compat": {
  "default_channel": "workbuddy",
  "max_tokens_cap": 32000,
  "model_map": {
    "claude-*": "workbuddy/glm-5.2",
    "gpt-5*": "workbuddy/deepseek-v4-pro"
  }
}
```

- 带 `channel/` 前缀的模型名始终优先（如 `workbuddy/hy3`），行为与旧版一致
- 无前缀时依次尝试：**降级链别名** → `model_map` 精确匹配 → 通配匹配（key 以 `*` 结尾）→ `default_channel`
- `max_tokens_cap` 用于封顶客户端的 `max_tokens`（Anthropic 客户端常发 64000，
  超出部分上游会直接 400）；设 `0` 表示不限制

#### 给降级链起个短名（别名，R129）

降级链的主力模型 ID 是 `channel/model` 全路径，每次在客户端里抄一遍很容易抄错。
可以在降级链规则上配一个 `name`，客户端直接请求这个名字：

```json
"model_fallback": [
  { "from": "workbuddyai/deepseek-v4.1-flash", "to": ["workbuddy/glm-5.2"], "name": "fast" }
]
```

- **别名是主力模型的另一个名字**：请求 `fast` 等同于请求 `workbuddyai/deepseek-v4.1-flash`——
  同样的粘性路由、容量判定、降级链与冷却；主力没容量时照样按 `to` 降级。
- 优先级：**别名 > `model_map` > `default_channel`**（别名是结构化配置，最具体）；
  三个接口（`/v1/chat/completions`、`/v1/responses`、`/v1/messages`）行为一致，保存即热生效。
- 别名**不参与备胎链**（`to` 里只能写 `channel/model` 全路径）；也**不会**在用量榜/请求日志里
  单独成行——消耗一律记在主力模型名下，面板数字不会裂成两截。
- 别名会出现在 `/v1/models` 里（排在真实模型之前，带 `alias` 字段指向主力），能力字段
  与主力模型逐字段相同。
- 保存时的硬限制：不能含 `/`、空白、逗号、`*`；不能与另一条规则的别名重名、
  不能与 `model_map` 的键或渠道名重名（大小写敏感：`Fast` ≠ `fast`）。与某个真实模型的裸名重合时
  只给一条**提示**（允许保存，运行时按别名走）。

**Codex CLI 配置示例**（`~/.codex/config.toml`）：

```toml
model = "traework/glm-5.2"
model_provider = "wildwork"

[model_providers.wildwork]
name = "wildwork"
base_url = "http://127.0.0.1:7863/v1"
wire_api = "responses"
env_key = "WILDWORK_KEY"
```

**Claude Code 配置示例**（环境变量）：

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:7863"
export ANTHROPIC_AUTH_TOKEN="WildWorkAPI"
export ANTHROPIC_MODEL="traework/glm-5.2"
```

> ⚠️ **已知限制**：Responses 接口是无状态实现，不支持 `previous_response_id`
> （会返回 400 并提示）。客户端需在 `input` 中携带完整历史（Codex CLI 默认如此）。
> 另外 WorkBuddy 国内版/国际版上游对某些 agent 系统提示词（如 Codex CLI 自带的那份）
> 会触发内容策略拦截（上游返回 `code=11128 Illegal API invocation from an unapproved channel`），
> 网关已对已知模板句（Codex CLI、DeepSeek Harness 身份句等）做等价改写绕过，
> 见 `internal/sanitize`；如仍遇拦截请更换渠道模型。
>
> 📸 **图片相关（已自动处理，无需配置）**：
> - 工具结果里的截图（Codex `view_image`）走 vision 通道，**不会**变成 base64 文本；
> - 内联 base64 图片在转发前降采样到长边 ≤1568（JPEG q85，透明图走 PNG），
>   典型 12 张 2048px 截图的会话请求体由 33MiB 降到 4MiB；
> - 入站请求体上限默认 **128MiB**，可用 `config.json` 的 `server.max_request_body_mb` 调整
>   （钳 8~1024）；超限返回 413 并提示实际限额。

### 5. 生图 API（OpenAI Images 兼容，glm 渠道）

同一端口提供 OpenAI Images API 兼容端点，由 glm（智谱清言）渠道承载，**不依赖其它渠道**：

| 端点 | 用途 | 说明 |
|------|------|------|
| `POST /v1/images/generations` | 文生图 | 一次上游固定产 4 图，`n` ≤ 4 取前 N 张；12~15s |
| `POST /v1/images/edits` | 参考图改图 | 多参考图自动拼版托管；输出比例**跟随输入图** |

鉴权与 Chat 相同（`Authorization: Bearer <API Key>`）。
`model` 字段可省略或任意填写（服务端忽略，固定走 glm 画图通道；不换模型、不做请求内换号）。

#### 文生图

```bash
curl -X POST "http://127.0.0.1:7863/v1/images/generations" \
  -H "Authorization: Bearer WildWorkAPI" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "glm/image",
    "prompt": "鲸系女仆立绘，蓝白渐变长发，手持一碗米饭",
    "n": 1,
    "size": "1024x1024"
  }'
```

- `size` 支持 OpenAI 尺寸与裸比例两种写法：`1024x1024` / `864x1152` / `1152x864` /
  `720x1280` / `1280x720`，或 `1:1` / `3:4` / `4:3` / `9:16` / `16:9`（也可 `auto`）。
  **其它值一律 400**——上游对非法比例会静默降级为 1:1，本层直接拦截；
- `response_format` 默认 `b64_json`（产物 CDN 直链会过期，服务端下载后内联返回）；
  传 `"url"` 则返回直链（过期失效，需自取自存）；
- 响应结构与 OpenAI 一致：`{"created": …, "data": [{"b64_json": …, "revised_prompt": …}]}`。

#### 参考图改图

双形态入参：

- **multipart**（OpenAI 官方 SDK 形状）：文件字段 `image`（可重复，也接受 `image[]`），
  另有 `prompt`、`output_format`（`png` 时产物真转 PNG——上游原生产 JPEG）；
- **JSON**：`{"prompt": "…", "image": ["https://… | data:image/png;base64,…"]}`。

```bash
curl -X POST "http://127.0.0.1:7863/v1/images/edits" \
  -H "Authorization: Bearer WildWorkAPI" \
  -F "prompt=给这个角色换上红色围巾，保持立绘风格" \
  -F "image=@./character.webp" \
  -F "output_format=png"
```

- 单张 `http(s)` URL 参考图直接透传上游（不过图床，链路最短）；
  本地文件 / base64 / 多图则自动**拼版**（referenceN 标注）+ 图床托管，对调用方透明；
- 产物默认下载转 `b64_json`（下载失败时回退 `url` 直链）；`output_format=png` 可让
  产物真转 PNG（上游原生产 JPEG）；请求的 `size` 被忽略——
  **输出比例跟随输入图**（`aspect_ratio` 对 edits 无效，实证）。

#### 限制与注意

- 生图依赖 glm 渠道账号池；402/限流等按 glm 模型级冷却处理，下一次请求自动换号；
- **产物不自动删除**，走图床的参考图会上传到第三方图床（含隐私提示，敏感图勿用）；
- edits 的多图拼版出图效果以双人设定集定性验证过，任意 N 张未全量验证。

### 6. Jev 结构化决策端点（`/v1/systemone`）

OpenCode Zen 上还有一个免费的结构化决策模型 **Jev（`jev-1.13-free`）**。
它不是聊天模型——不能调 `/v1/chat/completions`（会 500），只能通过专用端点调用。
本网关在 `POST /v1/systemone` 直接透传，用法：

```bash
curl -X POST "http://127.0.0.1:7863/v1/systemone" \
  -H "Authorization: Bearer WildWorkAPI" \
  -H "Content-Type: application/json" \
  -d '{
    "state": "用户反馈：我被重复扣费了，订单号 A-104，要求今天退款。",
    "questions": {
      "refund":  {"type": "noul",   "criteria": {"true": "要求退款", "false": "未提及退款"}},
      "team":    {"type": "choice", "criteria": {"billing": "支付/发票/退款", "technical": "Bug/故障", "other": "以上都不是"}},
      "urgency": {"type": "score",  "criteria": ["低（不紧急）", "中（今天内）", "高（立即处理）"]}
    }
  }'
```

#### 三种问题类型

| 类型 | 问什么 | 返回 | 类比代码 |
|------|--------|------|----------|
| **noul** | 这是不是 X？ | 0~1 的概率 | `if (isX(state))` |
| **choice** | 这是哪个？ | 每个选项的概率 + 最佳选择 | `switch (classify(state))` |
| **score** | 这几分？ | 0~(N-1) 等级分 + 概率分布 | `scoring(state)` |

#### 响应示例

```json
{
  "model": "jev-1.13-free",
  "answers": {
    "refund":  {"type": "noul",   "noul": 0.92},
    "team":    {"type": "choice", "choice": "billing", "confidence": 1, "probabilities": {"billing":1,"technical":0,"other":0}},
    "urgency": {"type": "score",  "score": 1.83, "confidence": 0.74,
                "legend": {"0":"低","1":"中","2":"高"}, "probabilities": {"0":0,"1":0.17,"2":0.83}}
  },
  "usage": {"input_tokens": 444, "output_tokens": 69},
  "cost": "0"
}
```

#### 关键限制

- **必须带 `criteria` 字段**——`noul` 用 dict `{"true":"…","false":"…"}`，`choice` 用 dict（每个选项的判断标准），`score` 用 list（与级别对齐的描述）
- `state` 文本约 32k token 预算（state + questions 共享），英文最强，数学/日期/十六进制请交给代码
- `choice` 最多 255 个选项，`score` 2~10 级
- **免费**：`cost:"0"`，实测 0.5~1.3s 完成决策
- 也可直连上游：`POST https://opencode.ai/inference/v1/systemone` + `Authorization: Bearer public` + OpenCode 伪装头，但本网关已帮你处理了鉴权与伪装

> **别拿它当 GPT 用**——把它当成"语义判断 API"：给它状态和选择题，它回概率和置信度，你的代码根据 typed 值做后续决策。

## 面板操作指南

所有操作在 Web 管理面板中完成，按直觉操作即可：

- **添加账号**：点击渠道按钮 → 确认对话框 → 浏览器窗口登录 → 自动完成
  （「＋ MonkeyCode」是**导入**按钮：读取本机已登录客户端的凭据，不需要浏览器登录）
  （「＋ 小浣熊」主按钮走**网页版自动登录**：拉起一个独立浏览器窗口，登录后自动捕获凭据；
  次按钮「从客户端导入」则读取本机已登录客户端的凭据，不需要浏览器登录）
  （「＋ Loomy」是**导入**按钮：读取本机已登录客户端的凭据，不需要浏览器登录）
  （「＋ OfficeAce」主按钮走**自助 OAuth**：拉起华为云授权页，登录后自动捕获凭据；
  次按钮「从客户端导入」则读取本机已登录 OfficeAce 桌面端的凭据（临时 AK/SK 经 DPAPI 解密，仅 Windows），
  不需要浏览器登录）
  - ⚠️ **OfficeAce 关于登录态互踢**：上游刷新时会轮换 refresh_token，若同一账号的凭据被本工具与 OfficeAce 桌面端**共用**，
  两边会争抢同一个 refresh_token。要两边同时用，请用「浏览器登录」在本工具**单独登录一次**（新建独立链路），而不是从桌面端导入。
  - ⚠️ **OfficeAce 关于平台限制**：**浏览器登录（自助 OAuth）跨平台**（纯 HTTP + 系统浏览器）；
  而「从客户端导入」的加密档案解密依赖 **Windows DPAPI**，仅 Windows 可用。
  两条路径的对话都不受影响（网关 Basic 凭据不过期），但非 Windows 下若不走自助登录，积分/签到会降级。
  - ⚠️ **关于小浣熊登录态互踢**：上游刷新时会轮换 refresh_token，若同一账号的凭据被本工具与浏览器 / 官方客户端**共用**，
  两边会争抢同一个 refresh_token，可能出现其中一侧被挤下线。要两边同时用，请用「网页版登录」在独立浏览器窗口里**单独登录一次**
  （本流程新建独立会话），而不是从浏览器复制凭据粘进来。
  - ⚠️ **平台限制**：MonkeyCode / 小浣熊 / Loomy 的「从客户端导入」路径探测**跨平台**（用标准用户目录 / 环境变量定位客户端凭据，macOS / Linux 上客户端把配置写在标准位置即可命中，找不到则返回明确报错）。小浣熊的**网页版自动登录同样跨平台**（纯 HTTP + 系统 Edge/Chrome，不依赖注册表）；小浣熊 / Loomy 官方另有 macOS / Linux 客户端，MonkeyCode 客户端则仅发布过 Windows 版（其导入在非 Windows 仍会按上述口径尽力而为）。其余渠道（WorkBuddy 系 / Trae 系 / Qoder 系 / 千问办公 / 智谱清言 / Cline）跨平台可用（Cline 是纯 HTTP + 官方 API Key，不依赖本机客户端，天然跨平台）。
- **账号管理**：卡片显示积分、签到状态（WorkBuddy 国际版显示「自动领日活奖励」，无签到活动的渠道显示灰色「无签到」）；图标按钮操作（签到 ✓ / 刷新 ↻ / 停用 ⏸ / 删除 ✕）
  - **修改显示名**：点击卡片上的账号名即可修改，用于给账号起好认的别名
  - 账号按**渠道显示名的字母序**展示（Unicode 码位序，中文名因码位高自然排在最后）：Cline → CodeArts → Gemini → Kilo → LobsterAI → Loomy → MiniMax → MonkeyCode → OfficeAce → OpenCodeZen → Qoder → QoderCN → QoderCOM → TraeCode → TraeWork → WPS Comate → WPS 灵犀 → WorkBuddyAI → WorkBuddyCN → ZCode → 千问办公 → 小浣熊 → 智谱清言；**自定义渠道硬编码排在最后**（同组内按渠道标识排）。账号卡片、积分汇总条、费率 tab 三处同一顺序，规则单一来源（见决议 R142）
  - 积分显示为「可用积分」；若该账号还有本工具用不了的额度（如 TraeWork 官方客户端专用池），会追加显示 `/ N 不可用`
- **积分明细**：鼠标悬停积分数字显示套餐明细（含有效期与可用/不可用小计），条目多时用底部 `‹ ›` 翻页
- **刷新积分**：面板顶部按钮，批量刷新全部账号余额
- **模型列表和费率**（第四 tab）：点击「刷新」从上游拉取最新模型定价；上游未返回定价的模型显示 `unknown`，免费模型高亮为 `Free`
- **自定义渠道（任意 OpenAI 兼容 API）**：主面板「＋ 渠道」或渠道行的「编辑」弹窗里填 *渠道标识 / 接入点 / API Key* 即可接入：
  - 接入点**可以随便粘**——带 `/chat/completions`、`/completions`、`/models` 尾巴，或漏了 `http(s)://`，保存时都会被规范化成 API 根
    （内网地址默认 `http`，公网默认 `https`）；`🔍 识别端点` 会在候选根里做**有界嗅探**（最多 3 个根，只 GET 不 POST）确认真实端点，
    并把结果回填输入框；定位失败会**逐条列出试过哪几个地址、各回什么**，不是一句「连接失败」。
  - 「＋ 模型」是一个两页签面板：*上游列表*（拉一次 `/models`，逐行点行尾 `＋`/`－` 加入或移除，已添加的行可「更新能力」把旧值洗成上游真值）
    与 *手动添加 / 编辑*（不打上游）。上游没声明的能力标为**未探测**，本工具**不编造默认值**，需要就手填。
  - 渠道标识**可以改**（改完旧前缀模型立即失效、历史用量流水仍记旧名，弹窗会先列出代价）。
  - API Key 常态显示**脱敏串**（`长头****短尾`）；点行上的 key 或编辑框旁的 👁 才向后端**单条**取原文（复制进剪贴板 / 短暂显示，关窗即清）。
  - 费率页每个自定义渠道的接入点行后有 **`显示全部模型`** 开关：打开才现拉一次上游目录（未添加的标「未添加」），**只看不写盘**，要哪个仍回「＋ 模型」里点 `＋`。
  - 「关闭」按钮**带保存语义**：有改动会先落盘并热生效，保存失败则不关窗。
- **设置（统一配置入口）**：点击顶栏右侧 ⚙ 齿轮按钮，包含：
  - *接口与鉴权*：API 监听地址（`127.0.0.1` / `0.0.0.0` / 自定义 + 端口）、API-Key（给 AI 客户端）、
    **管理密码**（给面板登录；与 API-Key 是两套凭据）
  - *自动签到与开机自启*：签到时间多组增删、开机自启开关
  - *模型路由*：默认渠道、封顶 tokens、CC/Codex 客户端模型名映射表（`claude-* = workbuddy/glm-5.2` 形式，支持通配）
  - *模型降级链*：每条链可填一个**别名**（可选，如 `fast`）——客户端直接请求这个名字等同于请求主力模型；
    面板会即时校验（非法字符/重名当场报错，与真实裸名重合只黄字提示），保存即热生效，
    并在链卡片上显示可一键复制的别名 chip（R129）
  - *备用上游通道*：全局只配**一个**代理地址（`http://` / `https://` / `socks5://`），留空 = 关闭。
    它**不影响任何常规请求**（所有渠道一律直连）；只有上游回「地区限制」时，本工具才在
    同一次请求内自动改走该通道重发一次，重发也失败则透传原始错误（不拿第二次尝试的错误掩盖首因）。
    目前取用方：Cline 与 OpenCodeZen（两渠道的部分模型直连会被上游按地域拒，如 cline-free/muse-spark 系）。
    命中的模型会被「学会」，后续 6 小时内不再白撞直连；改/清代理地址则学习结果立即作废。
    同处还可为 OpenCodeZen 填入自定义 API key（`sk-…`，留空用匿名凭证 `public`；
    自定义 key 有独立配额不受匿名限流影响，且可调用付费模型——需账户余额）。
    > 旧版「每个渠道单独配代理」已整体下线：除 cline 外的渠道不再支持代理，一律直连。

> 顶栏的 API 地址 / API-Key 文本框点击复制；所有配置修改统一走 ⚙ 设置弹层。

### 面板登录（监听非 127.0.0.1 时）

- 监听 `127.0.0.1` 时面板默认不鉴权（只有本机能访问，无需打扰）。
- 监听 `0.0.0.0` 或具体网卡 IP 时**必须设置管理密码**（`config.json` 的 `admin_password`，或面板设置里的「管理密码」）：
  未设置时程序拒绝启动（日志/消息框提示），面板保存监听地址时也会拒绝并提示。
- 设置弹层中选 `127.0.0.1` 时不显示警告；改成其它地址时会实时出现黄色警示条，
  且「管理密码」label 后带红色 `*` 表示必填。
- 登录后下发 HttpOnly cookie（有效期 7 天，使用中自动续期，跨站请求受 SameSite 保护）；
  密码连续错误 5 次锁定该来源 IP 5 分钟；改密码后所有已登录会话立即失效。
- OpenAI/兼容接口的鉴权不受影响，仍用 `api_key`（两套凭据相互独立）。
- 也可用环境变量 `WILDWORK_ADMIN_PASSWORD` 配置（优先级高于 `config.json`）。

## 常见问题

### 1. Windows 提示“未知发布者”或报毒？

本工具没有数字签名，因此可能会被 Windows 安全中心或杀毒软件拦截。如果报毒，请将本工具添加到排除项，方法如下：

>设置 → 更新和安全 → Windows 安全中心 → 打开 Windows 安全中心 → 病毒和威胁防护 → “病毒和威胁防护”设置/管理设置 → 排除项/添加或删除排除项 → 添加排除项（文件或文件夹），把 exe 或所在目录加进去。

如果对本仓库发布包不放心，可以克隆到本地让 Agent 帮你审查一遍，然后自行基于源码构建（见 [DEVELOPMENT.md](DEVELOPMENT.md)）。

### 2. 小浣熊在 macOS / Linux 上能用吗？

可以。小浣熊官方有 macOS / Linux / 银河麒麟客户端（不止 Windows），本工具的「从客户端导入」路径探测
**本身跨平台**——用标准用户目录 / 环境变量定位客户端凭据（`~/.box-agent/config/auth.json`，或
`BOX_AGENT_CONFIG_DIR` 覆盖），客户端把配置写在标准位置即可命中；同时从客户端读到的活凭证也用于
运行时自愈（客户端续期会轮换 token，本工具会从同一个会话文件自动取新 token，无需重新导入）。

> 小浣熊的**网页版自动登录跨平台**：它拉起系统已装的 Edge/Chrome（独立 profile）完成网页登录，
> 再经 CDP 捕获凭据，不依赖 Windows 注册表。若机器上没有 Edge/Chrome，可改用「从客户端导入」路径。
>
> 注：MonkeyCode 客户端仅发布过 Windows 版；Loomy 客户端配置目录为 Windows 专用路径，其导入在
> 非 Windows 上按尽力而为处理，找不到凭证时返回明确报错。

### 3. 如何绑定多个 WorkBuddy 国际版账号？

添加新账号时，因为默认使用当前已登录的 github/Google 等账号自动登录，因此无法快速添加新的账号。
这里给出我的方案，在浏览器右上角菜单选择“新建无痕窗口”，在窗口地址栏输入`http://127.0.0.1:7863`打开 WildWork 主界面，即可在干净环境内新增账号。
也可以打开开发者控制台，单独清除 github.com 和 workbuddy.ai 的 Cookies。

### 4. WorkBuddy 国际版登录后积分显示为0，刷新积分报错
注册 WorkBuddy 国际版新账号时，需要选择地区后奖励积分才发放。

### 5. 小浣熊的「签到」是怎么回事？

小浣熊**没有**独立的签到/领取端点，它的每日 300 积分是**惰性落账**的：

- 名义记账时间是当天 00:00（积分流水里显示 `grant_at`）；
- **实际入账**要等账号当天第一次访问「工作台」接口才发生（流水里的 `created_at`）——
  也就是网页端文案说的「当天**登录**后有效期延长至 7 天」。

本工具的签到就是替你做这次访问（调网页端打开首页时自己也会调的 `setting_info`），
所以：

- **定时签到**：按面板「签到时间」自动执行，当天积分即时到账；
- **手动签到**：账号卡片上的 ✓ 按钮同样有效（当天已领过时返回「今日已领取」，不会重复发放）；
- 如果你平时会打开小浣熊网页/客户端，积分本来也会到账；本工具的签到保证**即使一天都不打开**也不漏领。

> 注：纯对话、查余额、刷新 token 都**不会**触发这个落账，只有工作台接口算「当天登录」。

### 6. 账号切换太频繁 / 太黏着不动，能调吗？

可以。本工具对每个渠道使用**粘性路由**：选定一个账号后尽量复用它（提高上游会话缓存命中率），
直到「连续成功 N 次」或该账号遇冷却/禁用/登录态失效才换号。这个 N 就是
`config.json` 里的 `server.sticky_max_requests`，**默认 50**：

```json
{ "server": { "sticky_max_requests": 20 } }
```

- 调**小**（如 10~20）：更均匀地分摊消耗、遇到单个账号变慢时更快轮到别人，代价是缓存命中率下降；
- 调**大**（如 200）：缓存亲和更好，但单个账号会被长时间钉住；
- 填 `0` 或负数视为非法，会在日志里记一条 `WARN config: server.sticky_max_requests=…` 并**回退默认 50**（不会改你的配置文件）。

注意它只决定「什么时候换下一个号」，**不影响选哪个号**——选号永远是「最早到期优先」（快过期的积分先烧）。
改完需重启本工具生效；已在跑的粘性链路仍按旧阈值跑到耗尽（不会中途被打断）。

### 7. WPS Comate 怎么登录？没有桌面的 NAS 能用吗？

能用。三条路，**同一份凭据纪律**，以「上游 `/models` 校验通过」为完成判据
（Cookie 出现 ≠ 登录完成：未登录态也会下发预登录 Cookie，上游会回 401 `not_login`）：

1. **扫码登录（默认，NAS/docker 可用）**：点「＋ WPS Comate」→ 面板里直接出二维码 →
   用手机 WPS 扫码确认。二维码由**服务端**向 `qr.wps.cn` 申请、Cookie 也在服务端换取，
   全程不需要桌面、不需要浏览器。二维码 110 秒自动换新，整体 5 分钟超时。
2. **本机浏览器接管（有桌面的本机部署）**：在 `config.json` 里填
   `"login": { "comate_browser_cdp": "http://127.0.0.1:9222" }`，并用
   `--remote-debugging-port=9222` 启动浏览器（Chrome 136+ 默认 profile 拒开调试口，需换 profile）。
   之后面板会显示该入口：它会**在你正在用的浏览器里**开一个 WPS 登录页，登录完自动读 `wps_sid`；
   结束后只断开连接，**不会关掉你的浏览器**。
3. **手工粘贴（兜底）**：展开「手工方式」粘贴。裸值、`wps_sid=...`、整段 Cookie 请求头、
   DevTools 表格整行都能直接吃（`comate.NormalizeSID` 会自己剥噪声）。

注意 `wps_sid` 没有 refresh 机制，过期后需重新走一遍上面任一条路（账号卡片上没有 ⟳ 重登按钮）。

## ⚠️ 风险警告与免责声明

**在使用本工具前，请务必阅读并理解以下内容。下载、运行或继续使用本工具即表示你已阅读、理解并同意承担全部风险与责任。**

### 风险警告

1. **逆向工程风险**：本工具基于对上游平台（WorkBuddy/CodeBuddy、TraeWork、Qoder、千问办公、OpenCode 等）**未公开接口的逆向工程**实现，非官方支持、无任何授权。上游随时可能调整接口、加密方式或风控策略，导致本工具**部分或全部功能立即失效**，作者不承诺修复时效。
2. **封号风险**：使用本工具的请求特征与官方客户端存在差异，**可能违反上游平台的服务条款**，存在账号被**限制功能、降低额度、暂时或永久封禁**的风险。请自行评估后果，**强烈建议使用小号或可承受损失的账号接入，切勿将主力账号用于本工具**。
3. **网络暴露风险**：面板监听 `0.0.0.0`（或任意非 `127.0.0.1` 地址）时**必须配置管理密码**（`config.json` 的 `admin_password`，
   或面板设置中「管理密码」），否则程序拒绝启动；未配置密码时只能监听本机。即便如此，仍请确认网络环境可信。
4. **数据安全风险**：账号凭证（access/refresh token）以明文 JSON 保存在本地 `auths/` 目录，请妥善保管该目录，避免泄露。

### 免责声明

1. 本工具仅供**个人学习、技术研究与测试**用途，严禁用于商业用途、大规模滥用或任何违反法律法规及上游平台服务条款的场景。
2. 本软件按"**现状**"（AS IS）提供，**不提供任何明示或默示的保证**。作者不对使用本软件造成的任何直接或间接损失承担责任，包括但不限于：账号被封禁、积分/额度损失、数据丢失、商业纠纷、法律纠纷等。
3. 使用者应**自行承担**因使用本工具产生的一切风险与后果；若不同意上述条款，请立即停止使用并删除本软件。
4. 本工具与上游各平台及其运营方**无任何关联与合作**；相关商标与品牌权利归其各自所有者所有。
5. 如本工具的使用对上游平台造成影响或平台方提出异议，作者将配合处理；本项目的存在不代表对任何规避行为的价值判断。

## License

[MIT](LICENSE) © rockswang。

欢迎 fork、修改与二次分发（含以独立仓库形式发布的衍生作品），只需按 MIT 条款保留版权声明与许可声明。
仅供个人学习研究使用，请遵守各上游平台服务条款。
