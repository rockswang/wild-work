# TraeWork 自动签到与积分获取的正确方式

本文档说明本工具对 **TraeWork（Trae SOLO CN）自动签到** 与 **积分查询** 的正确实现方式，
以及它与上游原版（`Sliverkiss/traework2api` 系）的区别。内容基于对官方客户端
（TRAE SOLO 桌面端）真实请求的抓包逆向分析，2026-08 实测有效。

---

## 一、登录流程（前置依赖）

签到与积分接口都依赖 **Cloud-IDE-JWT**（下文简称 JWT），登录是获取 JWT 的前置步骤。
新版官方客户端（`0.1.52`，插件 `2.3.73734`）已改为 **PKCE + AuthCode** 流程：

1. 客户端生成 PKCE 对：`code_verifier`（base64url，48 随机字节）与
   `code_challenge = base64url(sha256(code_verifier))`，method `S256`。
2. 构造授权 URL 打开浏览器（参数见下文「登录 URL」），其中携带 `code_challenge`。
3. 用户在 `www.trae.cn` 完成登录后，**网页端**调用
   `POST api.trae.cn/cloudide/api/v3/trae/oauth/GetPCAuthCode`
   换取 `AuthCode`（请求体：`{ClientID, CodeChallenge, CodeChallengeMethod, DeviceID, PlatformCode:"SOLO_PC"}`），
   并把 `authCodeInfo={AuthCode,...}` 通过回调 URL 回传给本地监听端口
   （`http://127.0.0.1:PORT/authorize?...`）。
4. 本地服务收到回调，提取 `AuthCode`，用**登录时保存的 code_verifier** 换取 JWT。

### 登录 URL 关键参数（必须对齐官方客户端）

| 参数 | 值 | 说明 |
|------|-----|------|
| `auth_from` | `solo` | SOLO 身份 |
| `login_channel` | `native_ide` | |
| `plugin_version` | `2.3.73734` | 随客户端版本更新 |
| `client_id` | `en1oxy7wnw8j9n` | SOLO CN 固定值 |
| `device_id` | 15 位数字 | **服务端绑定账号**，首次随机生成后持久化复用 |
| `machine_id` | 64 位 hex（32 字节） | 同上 |
| `x_device_brand` | 真实机型（如 `20Y5A00XXX`） | 设备指纹 |
| `x_device_type` | `windows` | |
| `x_os_version` | `Windows 10 Pro` | 真实系统版本 |
| `x_app_version` | `0.1.52` | 客户端版本 |
| `code_challenge` | base64url(S256(verifier)) | PKCE |
| `code_challenge_method` | `S256` | |
| `hide_saas_login` | `true` | |

> 注意：`device_id` 必须是**数字**且**持久化**（存在 auth 文件里），不能每次随机生成。
> 旧实现用随机 hex 会被服务端以 `9074` 拒绝。

### AuthCode 换 JWT（无签名）

```
POST https://api.trae.cn/trae/api/v3/oauth/ExchangeToken
{
  "ClientID": "en1oxy7wnw8j9n",
  "AuthCode": "<authCode>",
  "CodeVerifier": "<code_verifier>",
  "DeviceInfo": { ...12 字段... },
  "IDEVersion": "0.1.52"
}
```

**首次 AuthCode 换 token 不需要签名**（不带 `DeviceProof` / `ClientSecret` / `RefreshToken`）。
`DeviceInfo` 12 字段为 PascalCase：

```json
{
  "DeviceID": "<15 位数字，首次登录自动生成并持久化>",
  "MachineID": "<64 位 hex，首次登录自动生成并持久化>",
  "PlatformCode": "SOLO_PC",
  "DeviceType": "PC",
  "DeviceName": "<主机名>",
  "DeviceModel": "20Y5A00XXX",
  "ClientVersion": "0.1.52",
  "DevicePublicKey": "<ECDSA P-256 公钥 SPKI PEM>",
  "DeviceBrand": "20Y5A00XXX",
  "DeviceCPU": "",
  "OSInfo": "windows",
  "OSVersion": "Windows 10 Pro"
}
```

其中 `DevicePublicKey` 是**一次性生成的 ECDSA P-256 公私钥对**的公钥部分（SPKI PEM）。
私钥仅在后续 refresh 续期时用于 `DeviceProof` 签名（本工具目前未做自动 refresh，
token 过期后由调度器触发重新登录）。

> 下方 `DeviceInfo` 与登录参数为**脱敏示例**（`<...>` 为占位符）。
> `device_id` / `machine_id` 由本工具首次登录时自动生成并持久化在 `auths/` 目录，
> **切勿在代码中硬编码或提交到仓库**——它们绑定你的账号，泄露等同于泄露账号身份。

响应结构：`{Result:{Token, RefreshToken, TokenExpireAt, RefreshExpireAt}}`，
`Token` 即 Cloud-IDE-JWT。

---

## 二、自动签到

### 查询签到状态

```
POST https://api.trae.cn/trae/api/v2/ug/checkin_credits/status
body: {}
```

响应关键字段：`checked_in`（今日是否已签）、`credits`、`enable`。

### 执行签到

```
POST https://api.trae.cn/trae/api/v2/ug/checkin_credits/claim
body: {}
```

- `code=0` → 签到成功。
- `code=9095` → 当前设备今日已签到（幂等，可视为成功）。
- `code=9074` → **设备指纹校验失败**（见下文）。

### 设备指纹头（9074 根因）

签到（`checkin_credits/*`）与积分接口都必须携带官方客户端的设备指纹头，
**缺任一环节都会以 `9074 当前参与用户太多` 为由拒绝**：

```
Authorization: Cloud-IDE-JWT <jwt>
x-device-id: <15 位数字，与账号绑定的真实设备 ID>
x-device-brand: <机型>
x-device-type: windows
x-os-version: <系统版本>
x-app-version: <客户端版本>
X-User-Region: CN
```

注意：
- `x-device-id` 必须**小写** key（官方客户端如此发送）。
- `x-device-id` 的值必须与登录时绑定账号的 `device_id` 一致，且**持久化**（存 auth 文件）。
- `x-device-brand` / `x-device-type` / `x-os-version` / `x-app-version`
  是官方客户端 `bb()` 注入的指纹头，一个都不能少。

> **多账号提示**：服务端按「设备」判重，同一真实 `device_id` 下多账号当日可能互斥
> （后签的返回 `9095`）。若需多账号全签，建议每个账号使用各自独立的 `device_id`
> （如分别在不同设备登录后导出），否则当日只能签到其中一个。

---

## 三、积分获取

### 正确接口（网页版）

```
POST https://api.trae.cn/trae/api/v2/pay/web_user_ent_usage
body: {"require_usage": true}
```

响应关键结构：

```json
{
  "is_credits_billing": true,
  "user_entitlement_pack_list": [
    {
      "display_desc": "老用户福利",
      "entitlement_base_info": {
        "quota": { "credits_limit": 2000 }
      },
      "usage": {}
    },
    {
      "display_desc": "每月登录赠送",
      "entitlement_base_info": {
        "quota": { "credits_limit": 500 }
      },
      "usage": { "credits_amount": 32.8464 }
    }
  ]
}
```

### 正确算法

**剩余积分 = Σ(credits_limit - usage.credits_amount)**，遍历 `user_entitlement_pack_list`
全部条目：

- 只累加带 `credits_limit` 的包（免费的 `enable_*` 包没有 `credits_limit`，自动跳过）。
- `usage.credits_amount` 是该包已消耗的积分，必须扣除。
- 最终结果向下取整（`int64`）。

示例（实测）：`Σlimit=5100`，`Σusage=32.85`，剩余 **5067**。

---

## 四、与上游原版（Sliverkiss/traework2api 系）的区别

| 方面 | 上游原版 | 本工具（正确实现） |
|------|---------|-------------------|
| **登录流程** | 旧版回调直接带 `refreshToken`，本地监听只解析 GET query | 新版 **PKCE + AuthCode**：保存 `code_verifier`，解析 `authCodeInfo`，用 AuthCode 换 JWT |
| **AuthCode 换 token 端点** | 无（不涉及） | `POST api.trae.cn/trae/api/v3/oauth/ExchangeToken`（首次交换无签名） |
| **设备指纹头** | `X-Device-Id`（大写）单独发送 | `x-device-id`（小写）+ `x-device-brand/type/os-version/app-version` 全套指纹头 |
| **device_id 格式** | 随机 hex | 15 位数字 + 持久化，绑定账号 |
| **签到端点** | `checkin_credits/status`、`checkin_credits/claim`（相同） | 相同端点 + 完整指纹头，规避 `9074` |
| **积分端点** | `ide_user_ent_usage`（`{}` body），仅 `Σ credits_limit` | `web_user_ent_usage`（`{"require_usage":true}`），`Σ(credits_limit - usage.credits_amount)` |
| **积分准确性** | 显示总额度，**不含已消耗** | 显示**真实剩余**（扣减用量） |

### 上游原版积分为何不准

上游的 `UserEntUsage` 调用 `ide_user_ent_usage`（body `{}`）并仅对
`entitlement_base_info.quota.credits_limit` 求和。问题有二：

1. 端点返回的包不含（或不带）`usage` 明细，无法体现已消耗积分 → 显示虚高。
2. 与网页版 `web_user_ent_usage`（`require_usage:true`）返回的
   `usage.credits_amount` 不一致。

切换到 `web_user_ent_usage` 并按 `credits_limit - credits_amount` 计算后，
显示值与官方网页版完全一致。

---

## 五、抓包分析过程（简述）

分析基于 Fiddler 抓取的真实客户端请求（`docs/traework_auth_*.saz`、
`docs/traework_cradit_*.saz`），关键步骤：

1. **登录**：对比授权页 JS（`www.trae.cn/authorization` 页面代码）中
   `GetPCAuthCode` 的调用与回调构造，确认新流程为
   `授权 URL（带 code_challenge）→ GetPCAuthCode 换 AuthCode → 回调 authCodeInfo → ExchangeToken 换 JWT`。
2. **签到 9074**：对照实验确认随机 `device_id` + 缺指纹头必被拒；
   补全 `x-device-*` 指纹头 + 持久化数字 `device_id` 后稳定通过。
3. **积分**：对比网页版 `web_user_ent_usage` 与旧版 `ide_user_ent_usage` 响应，
   确认剩余 = `Σ(credits_limit - usage.credits_amount)`。

---

## 六、相关源码位置

| 模块 | 文件 |
|------|------|
| 登录（PKCE/AuthCode/回调） | `internal/login_trae/login.go` |
| AuthCode 交换 + 设备公钥 | `internal/traework/authcode.go` |
| 签到 / 积分客户端 | `internal/traework/client.go` |
| 请求头（指纹头） | `internal/traework/headers.go` |
| 常量（端点 / 版本 / 指纹） | `internal/traework/constants.go` |

> 本文档基于 2026-08 的线上行为整理，若官方流程再次变更，需重新抓包核对。
