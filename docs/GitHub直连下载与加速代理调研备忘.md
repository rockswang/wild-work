# GitHub 直连下载与加速代理调研备忘

> 调研目的：为 wild-work 的**自动更新**功能确定「中国大陆如何可靠获取 GitHub Release」的技术路线。
> 调研时间：2026-10-15。调研环境：Windows 10 + git-bash，本机 DNS `DNS1.bjev.com.cn`，**系统代理已关闭**（`ProxyEnable=0`，全程直连）。
> 调研产物（未入库，仅本地）：`temp/ghdns/`（DoH 验证、IP 探活、对照实验、代理测试、`ghdl.sh`）。

---

## 0. 结论速览

| 问题 | 结论 |
|------|------|
| 是不是 DNS 污染？ | **不是**。系统 DNS 与国内四家 DoH 返回**完全一致**，均为真实 GitHub IP |
| DoH 有用吗？ | **无用**。照原方案实现收益为零，长缓存反而比系统 DNS 更差 |
| 真实故障是什么？ | **IP 级 TCP 443 阻断，按秒级轮换**（同一时刻部分候选通、部分不通） |
| 应用级纯解析能解决多少？ | 69% → 83%（多 IP 故障转移），**剩余 17% 是"全候选黑洞窗口"，解析层无解** |
| 最终选型 | **公共加速代理为主 + GitHub 主站兜底**；版本号**多源查询取最高**；优先用 `/releases/latest/download/` 恒定 URL（§4.7）；代理侧必须校验 SHA256 |

---

## 1. 方向性证伪：本场景不是 DNS 污染

最初假设「DNS 被污染 → 用 DoH 拿洁净 IP → 直连下载」。实测**不成立**。

### 1.1 多源 DNS 应答完全一致

对 `github.com` 用系统 DNS + 4 家国内 DoH + 8.8.8.8 分别查询，20 轮采样：

```
系统 DNS (DNS1.bjev.com.cn) -> 20.205.243.166
alidns   https://dns.alidns.com/resolve        -> 20.205.243.166   (20/20)
doh.pub  https://doh.pub/dns-query             -> 20.205.243.166   (20/20)
dnspod   https://1.12.12.12/dns-query          -> 20.205.243.166   (20/20)
360      https://doh.360.cn/resolve            -> 20.205.243.166   (20/20)
8.8.8.8  (UDP 53)                              -> 20.205.243.166
```

- **没有任何解析器返回污染特征值**（`127.0.0.1` / `0.0.0.0` / 野 IP）
- `20.205.243.166` 是真实 GitHub 地址（AS8075 Microsoft，新加坡前置节点）
- 结论：**DoH 拿到的 IP 与系统 DNS 完全相同 ⇒ 仅做 DoH 替换，收益为零**

> ⚠️ 方法论提醒（对齐 R23b）：调研首轮 `doh.pub` 曾单独返回 `140.82.114.3`，若就此下结论会得出
> 「DoH 能纠正污染」的**错误结论**。后续 20 轮采样证明那是一次性抖动，四家解析器稳定一致。
> **观察性数据只能提假设，定因果必须做重复采样/对照实验。**

### 1.2 原方案实测被打脸

按「DoH 解析 + 5 分钟缓存固定 IP」实现第一版，与系统 DNS 朴素路径对照（同一时间窗交替执行）：

```
round  NAIVE(系统DNS单IP)      SMART(多DoH+固定IP缓存)
11     OK    7992ms            FAIL 10000ms   <== 系统DNS成功，洁净解析失败
12     OK     234ms            FAIL 10001ms   <== 同上
13     OK     277ms            FAIL 10000ms   <== 同上
```

**长缓存把已失效的 IP 钉死，比系统 DNS 更糟**。这是本次调研最重要的方向性纠正。

---

## 2. 真实故障机制：IP 级阻断 + 秒级轮换

### 2.1 可用率实测（长时 soak，2 秒采样）

```
IP                        归属                可用率
20.205.243.166  (github.com)              52%
185.199.108.133 (release-assets)          48%
185.199.109.133 (release-assets)          44%
185.199.110.133 (release-assets)          62%
185.199.111.133 (release-assets)          84%
20.205.243.168  (api.github.com)          98%
20.205.243.165  (codeload)               100%
140.82.112.x / 113.x / 114.x              100%
```

状态在 O/X 之间**反复翻转，几十秒一轮**。不是「某 IP 被封」，而是**随机式阻断/抖动**。

### 2.2 故障高度集中在 `github.com`

25 轮「系统 DNS 首选 IP vs 候选池」对照：

| host | 默认 IP 可用 | **多 IP 救回** | 全挂 |
|------|------|------|------|
| **github.com** | 5/25 (20%) | **20/25 (80%)** | 0 |
| api.github.com | 25/25 | 0 | 0 |
| codeload.github.com | 25/25 | 0 | 0 |
| objects.githubusercontent.com | 25/25 | 0 | 0 |
| release-assets.githubusercontent.com | 25/25 | 0 | 0 |

`github.com` 恰是下载链的**第一跳**，故障集中于此。

### 2.3 分层定位：不是 TLS/SNI 问题

TLS 探测（SNI = 真实域名，证书校验开启）全部 `TCP_OK + TLS_OK (TLS1.3)`；
ICMP 可达；TCP 80 可达。**纯 TCP 443 层问题**，非 SNI 阻断、非证书问题。

### 2.4 「全候选黑洞窗口」确实存在

14:30–14:32 连续 5 轮，**所有候选 IP 全部不可达**：

```
14:30:37  smart FAIL  all candidate IPs unreachable for github.com
14:31:09  smart FAIL  同上
14:31:41  smart FAIL  同上
14:32:14  smart FAIL  同上
14:32:46  smart FAIL  同上
```

⇒ **应用级 DNS 不可能 100% 解决**，必须配重试/退避/备用下载源。

### 2.5 修正后的对照实验（42 样本）

```
naive(系统 DNS 单 IP)   29/42 (69%)
smart(多 IP 故障转移)   35/42 (83%)
  ├─ 智能救回 : 7 次
  ├─ 智能更差 : 1 次
  └─ 两者皆挂 : 6 次
```

---

## 3. 公共加速代理实测

### 3.1 关键提醒：`ghproxy.com` 已失效

需求中提到的 `https://ghproxy.com/` **域名已易主，HTTP 000 无响应**。
可用的是**带连字符**的 `https://gh-proxy.com/`。

### 3.2 20 个候选代理首轮筛选（Range 请求 64KB）

```
PROXY                              OK     CODE   BYTES     MS
gh-proxy.com/                      YES    206    65536     1386
ghfast.top/                        YES    206    65536     1627
ghproxy.link/                      YES    200    2106      3086   ← 仅 2KB，可疑
ghproxy.net/                       YES    206    65536     3260
ghps.cc/                           YES    200    373       5503   ← 373B，可疑
gitproxy.click/                    YES    200    27376     5661   ← 跳转到随机子域
ghproxy.com/  ghproxy.cc/  hub.gitmirror.com/  gh.llkk.cc/
gh.api.99988866.xyz/  mirror.ghproxy.com/  gh.zwy.one/  …    全部 ERR

可用 6/20
```

> ⚠️ **只看状态码会踩坑**：`ghproxy.link` 返回 200 但只有 2106 字节，`ghps.cc` 只有 373 字节
> （HTML 错误页），`gitproxy.click` 跳转到 `ww19.gitproxy.click` 这类随机子域。

### 3.3 完整下载 + SHA256 校验（决定性判据）

以 v2.7.2 windows 产物为样本（真值 `666d467a8e281a739fcdaab651347f27b53940fb2a5c81c834d2d852e4a51311`，size 14799872）：

| 代理 | 完整下载 | SHA256 | 速度 |
|------|---------|--------|------|
| **gh-proxy.com** | 3/3 成功 | ✅ 一致 | 430–840 KB/s |
| **ghfast.top** | 3/3 成功 | ✅ 一致 | 620–850 KB/s |
| ghproxy.net | 0/3 | ❌ **内容截断** | ~19 KB/s 卡死 |
| ghproxy.cc | — | HTTP 000 | — |
| ghproxy.com | — | HTTP 000 | — |

**`ghproxy.net` 是典型陷阱**：HTTP 200、`Content-Length` 正常，但**内容被静默截断**
（每次卡在 2–3MB），校验和不匹配。**只看状态码会静默装上一个坏包。**

### 3.4 与直连的同期对比（关键发现）

同一时间窗口内：

```
直连 GitHub：  3/3 全部卡死在 1.5–2.2MB / 120s 超时，速度 ~15 KB/s   ❌
gh-proxy.com： 3/3 完整下载 14.8MB，430–840 KB/s                    ✅
ghfast.top：   3/3 完整下载 14.8MB，620–850 KB/s                    ✅
```

⇒ **在故障窗口内，代理不是"备用"，而是当下唯一可用的主力通道。**

---

## 4. 选定方案：主站 + 镜像站双源自动更新

### 4.1 需求（本工具自动更新的产品行为）

> 使用 **GitHub 主站 + 上述加速代理** 实现自动更新。
> **检测阶段**：分别尝试从主站和镜像站拉取最新版版本号，**任一可用**即提示用户更新。
> **下载阶段**：用户点击更新后，**同时测试主站和镜像站**，择优下载。

### 4.2 推荐实现（按上述实测结论落地）

> ⚠️ **不要把 `api.github.com` 当成可信锚点或唯一来源**。它同样可能从大陆不可达
> （本机调研期间恰好稳定，但 98% 不等于 100%，且故障窗口不可预期）。
> **版本号必须多源查询、取最大值**，原因见 §4.3 约束 1。

```
┌─ 检测阶段（拉版本号，多源并行，取最高版本）───────────┐
│ 源1: api.github.com 直连 → .tag_name           ← 主，非唯一      │
│ 源2: api.github.com 经 gh-proxy.com             ← 实测 200，字节一致 │
│ 源3: ghfast.top 代理 releases/latest 页面 → 正则取 tag  ← 互补路径 │
│ 源4: raw 文件经 gh-proxy                       ← 兜底（注意缓存滞后） │
│ 全部失败 → 静默放弃本次检测（不打扰用户）                        │
│ 取所有成功源中的**最高版本** → 比对 local Version → 提示更新      │
└─────────────────────────────────────────────────────┘
                       ↓ 用户点击「更新」
┌─ 下载阶段（并发测速择优 + SHA256 校验）────────────────┐
│ 并发发起: 主站直连 + gh-proxy.com + ghfast.top           │
│ 取「首个成功建立连接且吞吐达标」的源，其余取消              │
│ 下载中若速度长期低于阈值/中断 → 切下一个源（断点续传）       │
│ 下载完成 → 校验 digest（来自 API）→ 不匹配则换源重下      │
│ 全部失败 → 保留当前版本，提示手动下载（给出原始 URL）        │
└─────────────────────────────────────────────────────┘
```

> 下载 URL 有两种写法：显式 tag（`/releases/download/vX.Y.Z/<asset>`，需先知道版本号）
> 或恒定 `/releases/latest/download/<asset>`（无需知道版本号，**两个镜像站都支持**）。

### 4.3 必须遵守的约束

| # | 约束 | 依据 |
|---|------|------|
| 1 | **版本号必须多源查询并取最高值，不得依赖单一来源** | ① `api.github.com` 并非必然可直连（本次仅是本机该时段恰好稳定）；② 镜像站回源有延迟，可能主站已发新版而镜像仍返回旧版；③ 实测 `api.github.com` 响应带 `Cache-Control: public, max-age=60, s-maxage=60`，CDN 层可缓存 60s，经代理后实测 `Age: 19~25`（确在复用缓存）。**任一单源都可能滞后或不可达** |
| 2 | **SHA256 校验值必须从与下载不同的源获取，且需交叉验证** | 若校验值与被下载文件来自同一代理，代理被篡改时校验形同虚设。理想做法：从两个独立源各取一次校验值，一致才采信；否则至少保证「取校验值的源 ≠ 下载的源」 |
| 3 | **下载完成必须校验 SHA256，不匹配即换源重下** | `ghproxy.net` 实测 HTTP 200 + 内容截断（2–3MB 处卡死），仅看状态码会静默装坏包 |
| 4 | **代理列表必须可配置/可热更新，不得硬编码写死** | `ghproxy.com` 曾是最知名代理，现已完全失效（HTTP 000）；公共代理是打地鼠 |
| 5 | **必须保留直连兜底与「手动下载」出口** | 存在「全候选黑洞窗口」（实测连续 5 轮所有 IP 不可达），且代理可能集体失效 |
| 6 | **检测阶段失败必须静默**，不得弹错误框 | 网络抖动是常态；更新检测是锦上添花，失败不能干扰主功能 |
| 7 | **并发测速要有上限与超时**，避免多路同时拉满带宽 | 三个源同时下载 14.8MB 会互相挤占 |
| 8 | 更新器**不得使用 `InsecureSkipVerify`** | 证书校验/SNI 必须保留 |

### 4.4 各源能力矩阵（实测，2026-10-15 复测）

代理**不是全路径透传**，不同代理支持的上游路径差别很大。

> ⚠️ **必须区分两条容易混淆的路径**（初版曾把它们搞混，得出相反结论）：
> - `/releases/latest` —— 是 **HTML 页面**（重定向到 `/releases/tag/vX.Y.Z`）
> - `/releases/latest/download/<asset>` —— 是 **资产下载**（重定向到具体 tag 的下载路径）

| 上游路径 | gh-proxy.com | ghfast.top | 直连 |
|---------|:---:|:---:|:---:|
| `api.github.com/…/releases/latest` | ✅ 200（字节一致） | ❌ 403 `Invalid input.` | ✅ 200 |
| `raw.githubusercontent.com/…` | ✅ 200 | ❌ 000 | ⚠️ 不稳 |
| `codeload.github.com/…/zip/…` | ✅ 200 | ❌ 403 | ✅ 200 |
| `github.com/…/releases.atom` | ❌ 404 | ❌ 403 | ✅ 200 |
| `github.com/…/releases/latest`（**HTML 页面**） | ❌ 403 | ✅ 200（含 `releases/tag/v2.7.3`） | ⚠️ 不稳 |
| **`github.com/…/releases/latest/download/<asset>`** | ✅ 206 | ✅ 206 | ⚠️ 不稳 |
| `github.com/…/releases/download/<tag>/<asset>` | ✅ 200/206 | ✅ 200/206 | ⚠️ 故障窗口内近乎不可用 |

**要点**：
- **两个代理都支持 `/releases/latest/download/<asset>`**（§4.7 实测 6/6，均为真 PE 二进制）
- **`ghfast.top` 能代理 `/releases/latest` HTML 页面**（可从中正则 `releases/tag/vX.Y.Z` 取版本号），
  而 `gh-proxy.com` 对该页面返回 403 —— 与「ghfast 只支持资产」的初版结论**恰好相反**
- `ghfast.top` **不能**代理 `api.github.com` / `raw` / `codeload`（403/000）⇒ 无法用它们做版本检测
- `gh-proxy.com` **能**代理 API/raw/codeload，但**不能**代理 `releases/latest` 页面

> 📌 **两者能力互补**，不存在“谁更全”。实际部署应同时配置两个代理。

### 4.5 版本号来源候选（按可靠性排序）

| 途径 | 直连 | 经 gh-proxy.com | 缓存滞后 | 备注 |
|------|:---:|:---:|:---:|------|
| `api.github.com/…/releases/latest` → `.tag_name` | ✅ | ✅ | **60s** | **首选**，JSON 可解析，字段最全（含 assets/size/**digest**） |
| `github.com/…/releases/latest/download/<asset>` 的 302 | ✅ | ✅ | 无 | **次选，见 §4.8**——恒定 URL 永远指向最新版，代理可直接跟随 |
| `raw.githubusercontent.com/…/<file>` | ⚠️ 直连不稳 | ✅ | **300s** | 内容由仓库控制，但需发版时同步写文件 |
| `github.com/…/releases.atom` | ✅ | ❌ | 无 | 极轻量 XML，代理不支持 |

> 建议：**前两条同时查，取较高版本**；后两条作为兜底。

### 4.6 「发版时写 `.version` 文件」方案评估（用户提议，已实测）

**提议**：改 workflow，每次发版在主仓更新一个 `.version` 文件，客户端不经 API，
直接通过代理读 raw 内容取版本号。

**结论：思路对（脱离 API 依赖确实提升通用性），但用 raw 文件做版本发现有两个坑，
而且有一个更优的替代（§4.8）。**

#### 坑 1：raw 的 CDN 缓存比 API 更久

| 来源 | `Cache-Control` | 版本检测最大滞后 |
|------|----------------|----------------|
| `api.github.com/…/releases/latest` | `max-age=60, s-maxage=60` | **60 秒** |
| `raw.githubusercontent.com` | `max-age=300` | **5 分钟** |
| jsDelivr `@master` | `max-age=604800, s-maxage=43200` | **7 天 / 12 小时**（❗） |

即：**用 raw 反而比用 API 滞后更久**（5min vs 60s）。若再经 jsDelivr 这类 CDN，
滞后可达 **12 小时～7 天**，对自动更新是不可接受的。

> 实测证据：jsDelivr 的版本列表 API（`data.jsdelivr.com/v1/packages/gh/...`）
> 在本仓已发布 **v2.7.3** 时仍只报到 **2.6.0**（连续 3 次重试一致），滞后跨 5 个版本。
> 这直接证明：**CDN 对仓库内容的缓存是不可控的**，不能用它做版本发现。

#### 坑 2：`@master` 引用本身不是“最新版”的可靠信号

`.version` 文件写在 master 上，但**发版 tag 与 master 的推送时机未必一致**；
若发版流程是“先打 tag → 后补 .version”（或反之），会出现
「`.version` 说新版、但 release 资产还没上传完」或「资产已传、`.version` 未更新」的窗口。
需要 workflow 内严格串行（见下方建议）。

#### 若仍要用此方案，必须做到

1. **`.version` 写入必须与 release 创建在同一 job 内串行**，且 **release 先成功、再提交 `.version`**
   （宁滞勿错：版本号偏旧只会晚提醒，偏新会让用户下载不到）
2. **加缓存击穿参数**（`?t=<unix_ts>`）并**实测是否真能绕过**——本次未验证成功
   （网络故障窗口内两个域名的击穿请求均未返回），需在可用窗口下补测
3. **仍必须多源交叉**（API + raw + 302），不能把 raw 当唯一来源

### 4.7 更优替代：`/releases/latest/download/<asset>`（实测可用，推荐）

GitHub 提供一条**恒定 URL**，永远指向最新版的指定资产，无需 API、无需改 workflow：

```
https://github.com/<owner>/<repo>/releases/latest/download/<asset-name>
```

**实测（v2.7.3，2026-10-15 复测）**：

| 路径 | 直连 | 经 gh-proxy.com | 经 ghfast.top |
|------|------|:---:|:---:|
| `/releases/latest/download/<asset>` | ✅ 302→302→CDN | ✅ **200**（代理内部跟随） | ✅ **200**（先返回 302 再跟随） |
| `/releases/download/<tag>/<asset>`（显式 tag） | ✅ | ✅ | ✅ |

> ✅ **两个镜像站都支持**（初版曾误记为「仅 gh-proxy.com」，已实测纠正）。
> 完整下载 + SHA256 校验：`gh-proxy.com` 3/3 一致，`ghfast.top` 3/3 一致。

**两者的行为差异**（对实现有影响，需分别处理）：

| | gh-proxy.com | ghfast.top |
|---|---|---|
| 对 `latest/download` 的响应 | **直接 `200` + 全量 body**（内部跟随了 302） | 先返回 `302`，`Location: /https://github.com/…/releases/download/v2.7.3/…` |
| 能否从响应头拿到版本号 | ❌ 无 `Location`（仅 `Content-Disposition: filename=…`，无版本号） | ✅ **`Location` 中含 `v2.7.3`**，可直接正则提取 |
| 稳定性 | 5/5 成功 | **2/5 超时**（抖动量明显高于 gh-proxy） |

**为何比 `.version` 方案好**：

1. **零 workflow 改动**——不新增文件、不增加发版步骤、不会“忘了更新”
2. **无额外缓存滞后**——302 由 GitHub 实时计算（与 API 同源），不存在 CDN 缓存旧版本号的问题
3. **一次请求同时完成“版本发现 + 下载”**——代理场景下就是一次 GET
4. **天然抗遗忘**——版本号内嵌在 `Location` 头里，不可能与资产不一致

**版本号怎么拿**（三条途径，推荐前两条）：

| 途径 | 说明 |
|------|------|
| `api.github.com` → `.tag_name` | 直连或经 gh-proxy，字段最全（含 `digest`） |
| **`ghfast.top` 的 302 `Location`** | 正则 `…/download/(v[\d.]+)/…` 即可，**一次 HEAD 同时拿到版本号与资产存在性** |
| 直连 GitHub 的 302 `Location` | 同上，但受 §2 的 IP 阻断影响（故障窗口内不可用） |

> `gh-proxy.com` 因内部跟随重定向，**拿不到版本号**，只能用于下载、不能用于版本检测。

### 4.8 当前仓库的缺口与已确认的解法

- **release 未发布 `.sha256` 资产**（v2.7.2 仅有 7 个平台产物 + 2 个 dmg）。
- **但 `api.github.com` 已为每个资产提供 `digest` 字段，且实测可信**（§4.9），
  ⇒ **无需额外发布 `.sha256` 资产**，直接用 `assets[].digest` 即可。

### 4.9 `digest` 字段可用性验证（已实测）

`GET /repos/{owner}/{repo}/releases/latest` 的每个 asset 都带 `digest`：

```json
{
  "name": "wild-work-windows-amd64.exe",
  "size": 14938112,
  "digest": "sha256:44868f69ad1e25adc9c61756d3eda2a16930d10897299c6e908b215634e7475a"
}
```

**交叉验证**（v2.7.3）：经 `gh-proxy.com` 下载的产物实测 SHA256
`44868f69ad1e25adc9c61756d3eda2a16930d10897299c6e908b215634e7475a`
**与 `digest` 逐字节一致** ⇒ 该字段可信，可直接作为校验值来源。

> 价值：解决了「校验值与被下载文件来自同一代理」的信任问题——
> **校验值来自 GitHub API（不同域名、不同路径），文件来自代理**，两者天然异源。
> 若再配合「同时从 API 直连与经代理各取一次 digest 并比对」，
> 就同时满足了约束 2 的「异源」与「交叉验证」。

---

## 5. 被否决/不采纳的方案

| 方案 | 否决理由 |
|------|---------|
| **DoH 纠正解析** | 实测与系统 DNS 结果完全一致，收益为零；长缓存固定 IP 反而更差 |
| **固定 hosts / 写死 IP** | IP 可用性秒级轮换，写死必然踩雷；且需改系统文件（用户明确不要） |
| **单一公共代理** | `ghproxy.com` 前车之鉴，单点必然失效 |
| **纯应用级解析（不做代理）** | 上限 83%，剩余 17% 黑洞窗口无解 |
| 国际 DoH（Google/Cloudflare/Quad9/OpenDNS/NextDNS） | 实测**全部 000 不可达**，不能作为候选 |

---

## 6. 附：DoH 端点实测清单（供将来复用）

| 端点 | 可用 | 备注 |
|------|------|------|
| `https://dns.alidns.com/resolve` | ✅ 200 | **必须用 `/resolve`**；`/dns-query` 返回 400（要求 RFC8484 `?dns=<base64url>`） |
| `https://doh.pub/dns-query` | ✅ 200 | 支持 `?name=&type=` 形式 |
| `https://1.12.12.12/dns-query` | ✅ 200 | dnspod，同 doh.pub |
| `https://doh.360.cn/resolve` | ✅ 200 | 同上，`/dns-query` 返回 400 |
| `https://dns.google/*` | ❌ 000 | 不可达 |
| `https://cloudflare-dns.com/dns-query` | ❌ 000 | 不可达 |
| `https://dns.quad9.net/dns-query` | ❌ 000 | 不可达 |
| `https://doh.opendns.com/dns-query` | ❌ 000 | 不可达 |
| `https://dns.nextdns.io/dns-query` | ❌ 000 | 不可达 |

> 本表虽不用于本次更新方案，但对将来任何需要 DoH 的场景（如 CDP 登录、上游接口）有参考价值。

---

## 7. 调研脚本索引（`temp/ghdns/`，未入库）

| 路径 | 用途 |
|------|------|
| `doh.go` | 国内 DoH 客户端（含 bootstrap IP 引导、CNAME 跟随） |
| `resolver.go` | 应用级解析器：多源候选 + TCP 探活 + 短缓存 + 失败即失效 |
| `main.go` / `ghdns.exe` | 全链路验证：拉 release → 跟随重定向 → 下载 → 校验 |
| `compare/` | **朴素单 IP vs 洁净解析** 对照实验（输出 `compare.csv`） |
| `multip/` | 多 IP 故障转移收益量化（`multip.log`） |
| `probe/` | 连通性时序探针（`soak.log`） |
| `tlsprobe/` | TLS/SNI 分层定位 |
| `proxy/` | 20 个公共代理首轮筛选 |
| `proxyverify/` | 代理内容完整性（SHA256）校验 |
| `ghdl.sh` | **最终选型的最小实现**：主站/镜像双源 + SHA256 校验 |
| `latestcheck.sh` | `/releases/latest/download/` 在两个镜像站的严格对比（多轮 + PE 头校验） |

> 复现方式：`bash ghdl.sh v2.7.2 out.exe`（可选 `GH_SHA256=<hash>`）。

---

## 8. 方案演进记录（四轮修正）

本方案的结论经过四次基于实测的修正，记录于此以避免后人重蹈：

| 轮次 | 原结论 | 修正后 | 修正依据 |
|------|--------|--------|---------|
| 1 | 「DNS 被污染 → 用 DoH 拿洁净 IP 即可」 | 「**不是 DNS 污染**，是 IP 级秒级轮换；DoH 无用」 | 四家 DoH 与系统 DNS 应答完全一致（§1.1）；原方案实测比系统 DNS 更差（§1.2） |
| 2 | 「版本号与 SHA256 一律取自 `api.github.com`」 | 「**必须多源查询取最高值**，不得假定 API 可直连」 | `api.github.com` 不保证可直连；镜像回源有延迟；API 带 `max-age=60` CDN 缓存（§4.3 约束 1） |
| 3 | （用户提议）「发版写 `.version`，客户端读 raw 取版本号」 | 「思路对，但 **raw 缓存 300s 比 API 的 60s 更滞后**；jsDelivr 类 CDN 可达 12h～7 天；改用 `/releases/latest/download/` 恒定 URL 更优」 | raw 实测 `max-age=300`；jsDelivr 版本列表实测滞后 5 个版本（§4.6/§4.7） |
| 4 | 「`/releases/latest/download/` 只有 gh-proxy.com 支持，ghfast.top 不行」 | 「**两个镜像站都支持**；且 `ghfast.top` 还能代理 `releases/latest` HTML 页面而 gh-proxy 不能——初版结论恰好写反了」 | 6/6 轮 Range GET 均为 206 + 真 PE 二进制；HTML 页面 3/3 含 `releases/tag/v2.7.3`（§4.4/§4.7） |

**共同教训**（四条同族，值得合并记一笔）：

1. **不要假定某个域名/端点“总是可用”**——`api.github.com` 恰好稳定，但写成硬依赖就是重复「假定可用」的老错。任何网络来源都必须有备用路径。
2. **不要假定“换个来源”就一定更好**——raw 文件看似更简单，实则 CDN 缓存比 API 更久（300s vs 60s）。
3. **不要假定 CDN 会及时回源**——jsDelivr 对仓库内容的缓存实测滞后 5 个版本。**缓存时长必须实测，不能拍脑袋。**
4. **不要凭单次结果下结论**——初版「ghfast 不支持 latest/download」来自单次 000/403，复测后完全相反。
   本调研中**网络抖动极大**（同一 URL 相邻两次可能一次 200 一次 000），**单次探测无意义**，
   必须重复多轮 + 校验内容类型（PE 头）才能定性。

> 尤其第 4 条：本次调研的多次误判都源于「一次请求失败 → 认定该路径不支持」。
> 在抖动的网络下，**失败结果比成功结果更不可信**——成功至少证明“确实能通”，
> 而失败可能只是撞上了一个随机阻断窗口。
