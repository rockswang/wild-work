# 上游渠道逆向分析备忘

本文档记录 `wild-work` 支持的三个上游渠道的 API 接口逆向分析结果，包括请求/响应结构、错误码含义、鉴权方式等。

---

## 1. WorkBuddy (CodeBuddy)

### 1.1 基础信息

- **OAuth 域名**: `https://oauth.codebuddy.ai`
- **Billing 域名**: `https://billing.codebuddy.ai`
- **模型前缀**: `wb/`
- **认证方式**: OAuth2 + JWT token (`accessToken`)

### 1.2 账号登录 (`POST /oauth/token`)

**请求**:
```json
{
  "grant_type": "authorization_code",
  "code": "<授权码>",
  "redirect_uri": "http://127.0.0.1:<随机端口>"
}
```

**响应**:
```json
{
  "access_token": "<JWT>",
  "token_type": "Bearer",
  "expires_in": 3600,
  "refresh_token": "<刷新令牌>",
  "scope": "user:email"
}
```

**字段说明**:
- `expires_in`: 秒数，从当前时间起算
- `refresh_token`: 用于刷新 accessToken，轮换后旧 token 失效

---

### 1.3 获取用户资源 (`POST /v2/billing/meter/get-user-resource`)

**请求头**:
```
Authorization: Bearer <accessToken>
Content-Type: application/json
User-Agent: CodeBuddy/1.0
```

**请求体**:
```json
{
  "PageNumber": 1,
  "PageSize": 100,
  "ProductCode": "p_tcaca",
  "Status": [0, 3],
  "PackageEndTimeRangeBegin": "2024-01-01 00:00:00",
  "PackageEndTimeRangeEnd": "2124-01-01 00:00:00"
}
```

**响应**:
```json
{
  "Response": {
    "Data": {
      "Accounts": [
        {
          "PackageName": "月度套餐",
          "CapacitySize": 10000,
          "CapacityRemain": 5000,
          "CapacityUsed": 5000,
          "CycleCapacitySize": 2000,
          "CycleCapacityRemain": 1500,
          "CycleCapacityUsed": 500
        }
      ]
    }
  }
}
```

**余额计算逻辑**:
```go
switch {
case acct.CycleCapacitySize > 0:
    r = acct.CycleCapacityRemain
case acct.CycleCapacityRemain > 0 || acct.CycleCapacityUsed > 0:
    r = acct.CycleCapacityRemain
default:
    r = acct.CapacityRemain
}
```

**状态码含义**:
- `Status[0, 3]`: 0=有效，3=已过期（查询时包含两种状态）

---

### 1.4 错误码分类

| HTTP 状态 | 错误类型 | 特征 |
|----------|---------|------|
| 401 | ErrSessionDead | `message contains "unauthorized"` |
| 429 | ErrSoftRate | 频率限制 |
| 400 | ErrClient | 客户端参数错误 |
| 500+ | ErrServer | 服务端错误 |

---

## 2. TraeWork (Tae Solo)

### 2.1 基础信息

- **OAuth 域名**: `https://oauth.trae.cn`
- **UG 域名**: `https://api.trae.cn`
- **Agent 域名**: `https://agent.trae.cn`
- **模型前缀**: `traework/`
- **认证方式**: JWT token (`Cloud-IDE-JWT <accessToken>`)

### 2.2 账号登录 (`POST /oauth/token/exchange`)

**请求头**:
```
Content-Type: application/json
Accept: application/json
User-Agent: Trae/0.1.52
```

**请求体**:
```json
{
  "ClientID": "solo_agent_web",
  "RefreshToken": "<refresh_token>",
  "ClientSecret": "-",
  "UserID": ""
}
```

**响应**:
```json
{
  "Result": {
    "Token": "<新 accessToken>",
    "TokenExpireAt": 1788745782,
    "TokenExpireDuration": 3600000,
    "RefreshToken": "<新 refresh_token>"
  }
}
```

**字段说明**:
- `TokenExpireAt`: Unix 时间戳（秒或毫秒，需归一化）
- `TokenExpireDuration`: 有效期（毫秒），若 `TokenExpireAt` 为空则用此计算
- `RefreshToken`: 可能轮换，保留旧 token 作为 fallback

---

### 2.3 积分明细 (`POST /trae/api/v2/pay/web_user_ent_usage`)

**请求头**:
```
Content-Type: application/json
Accept: application/json
User-Agent: Trae/0.1.52
Authorization: Cloud-IDE-JWT <accessToken>
X-User-Region: CN
x-device-brand: 20Y5A002XX
x-device-type: windows
x-os-version: Windows 10 Pro
x-app-version: 0.1.52
x-device-id: <设备 ID>
x-machine-id: <机器 ID>
```

**请求体**:
```json
{"require_usage": true}
```

**完整响应示例**:
```json
{
  "is_credits_billing": true,
  "is_dollar_usage_billing": false,
  "is_pay_freshman": true,
  "trial_status": {
    "is_eligible_for_trial": false,
    "is_in_trial": false
  },
  "usage_summary": {
    "consumed_amount": 982.72,
    "consumption_ratio": 0.17240701754385965,
    "total_amount": 5700
  },
  "user_entitlement_pack_list": [
    {
      "display_desc": "老用户福利",
      "entitlement_base_info": {
        "available_endpoint": 0,
        "charge_amount": 0,
        "currency": 1,
        "end_time": 1789623315,
        "ent_status": 0,
        "entitlement_id": "340864129538",
        "product_extra": {
          "package_extra": {
            "duration": 0,
            "package_duration_type": 0,
            "package_name": "福利积分",
            "package_source_type": 10,
            "quota": {
              "credits_limit": 2000,
              "no_bonus_quota": true
            }
          }
        },
        "product_id": 208,
        "product_type": 2,
        "quota": {
          "credits_limit": 2000,
          "no_bonus_quota": true
        }
      },
      "expire_time": 1789623315,
      "group_name": "用户福利",
      "group_type": 4,
      "is_hide": false,
      "is_last_period": false,
      "next_billing_time": 0,
      "source_id": "340864129538",
      "status": 0,
      "usage": {
        "credits_amount": 482.7212
      },
      "yearly_expire_time": 0
    },
    {
      "display_desc": "每月登录赠送",
      "entitlement_base_info": {
        "available_endpoint": 0,
        "charge_amount": 0,
        "currency": 1,
        "end_time": 1788191999,
        "ent_status": 0,
        "entitlement_id": "monthly_bonus_20268_1096660468371514",
        "product_extra": {
          "package_extra": {
            "duration": 1,
            "package_duration_type": 1,
            "package_source_type": 0,
            "quota": {
              "credits_limit": 500,
              "enable_solo_agent": true,
              "enable_solo_builder": true,
              "enable_solo_coder": true,
              "enable_solo_lite": true,
              "enable_solo_web": true,
              "no_bonus_quota": true,
              "solo_agent_parallel_limit": 2
            }
          }
        },
        "product_id": 221,
        "product_type": 2,
        "quota": {
          "credits_limit": 500,
          "enable_solo_agent": true,
          "enable_solo_builder": true,
          "enable_solo_coder": true,
          "enable_solo_lite": true,
          "enable_solo_web": true,
          "no_bonus_quota": true,
          "solo_agent_parallel_limit": 2
        }
      },
      "expire_time": 1788191999,
      "group_name": "每月登录积分",
      "group_type": 3,
      "is_hide": false,
      "is_last_period": false,
      "next_billing_time": 0,
      "source_id": "",
      "status": 1,
      "usage": {
        "credits_amount": 500
      },
      "yearly_expire_time": 0
    },
    {
      "display_desc": "签到奖励",
      "entitlement_base_info": {
        "available_endpoint": 1,
        "charge_amount": 0,
        "currency": 1,
        "end_time": 1789624063,
        "ent_status": 0,
        "entitlement_id": "checkin_20260817_1096660468371514",
        "product_extra": {
          "package_extra": {
            "duration": 31,
            "package_duration_type": 0,
            "package_name": "签到奖励",
            "package_source_type": 9,
            "quota": {
              "credits_limit": 200
            }
          }
        },
        "product_id": 209,
        "product_type": 2,
        "quota": {
          "credits_limit": 200
        }
      },
      "expire_time": 1789624063,
      "group_name": "每日签到",
      "group_type": 1,
      "is_hide": false,
      "is_last_period": false,
      "next_billing_time": 0,
      "source_id": "",
      "status": 1,
      "usage": {},
      "yearly_expire_time": 0
    }
  ]
}
```

**额度包分组特征**:

| group_type | group_name | 说明 |
|-----------|-----------|------|
| 4 | 用户福利 | 老用户福利积分（2000+） |
| 3 | 每月登录积分 | 登录赠送（500） |
| 1 | 每日签到 | 签到奖励（200×N） |
| 0 | 免费 | 免费订阅能力（enable_solo_*） |

**product_type**:
- `0`: 免费订阅（含 enable_solo_agent 等能力）
- `2`: 付费套餐/活动赠送

**关键结论**:
- **所有额度包都有 `no_bonus_quota: true`** → 无法区分"通用额度"和"Work 专属积分"
- 上游未暴露 `package_type` 字段区分用途
- 建议：按 `display_desc` 或 `group_name` 过滤特定类型（如只保留"签到奖励"）

---

### 2.4 签到状态 (`POST /trae/api/v2/checkin/status`)

**请求头**: 同积分明细

**请求体**: `{}`

**响应**:
```json
{
  "checked_in": true,
  "credits": 200,
  "enable": true,
  "code": 0,
  "message": "",
  "msg": "",
  "success": true
}
```

**错误码**:
- `code=0`: 成功
- `code=9074`: 限流（重试延迟 8s）
- `code=1001`: 认证失败（session_dead）

---

### 2.5 签到领取 (`POST /trae/api/v2/checkin/claim`)

**请求头**: 同签到状态

**请求体**: `{}`

**响应**:
```json
{
  "code": 0,
  "message": "",
  "msg": "签到成功",
  "success": true
}
```

**业务逻辑**:
1. 先查状态 (`CheckinStatus`)
2. 若未签到且 enable=true → 领取 (`CheckinClaim`)
3. 再查状态验证是否成功
4. `code=9074` 时重试一次（延迟 8s）

---

### 2.6 错误码分类

| HTTP 状态 | 错误类型 | 特征 |
|----------|---------|------|
| 401 | ErrSessionDead | `login`, `token 失效`, `token invalid`, `session`, `unauthorized` |
| 429 | ErrSoftRate | 频率限制 |
| 404 | ErrNotFound | 接口不存在 |
| 500+ | ErrServer | 服务端错误 |
| code=1005 | ErrHardCredit | `"code":1005` + `"plan"` |

---

## 3. Qoder

### 3.1 基础信息

- **API 域名**: `https://qodo.ai`
- **模型前缀**: `qoder/`
- **认证方式**: JWT token (`dt-` 前缀)

### 3.2 账号登录 (`GET /api/user/login`)

**请求头**:
```
Authorization: Bearer <auth_code>
```

**响应**:
```json
{
  "code": 0,
  "data": {
    "token": "<dt-xxx>",
    "userId": "01a00f81-2f0d-775e-8e67-2790396ca3a0",
    "nickname": "user123"
  }
}
```

---

### 3.3 配额使用 (`GET /api/quota/usage`)

**请求头**:
```
Authorization: Bearer dt-<token>
```

**响应**:
```json
{
  "userQuota": {
    "remaining": 296.5
  },
  "addOnQuota": {
    "remaining": 0
  },
  "isQuotaExceeded": false
}
```

**总余额**: `userQuota.remaining + addOnQuota.remaining`

---

### 3.4 积分明细 (`GET /api/quota/detail`)

**请求头**: 同上

**响应**:
```json
{
  "userQuota": {
    "total": 1000,
    "used": 703.5,
    "remaining": 296.5
  },
  "addOnQuota": {
    "total": 0,
    "used": 0,
    "remaining": 0
  }
}
```

**返回条目**:
- `userQuota`: 用户主套餐
- `addOnQuota`: 附加套餐（通常为 0）

---

### 3.5 错误处理

Qoder 上游 ≥400 直接透传原始响应，不包装：

```go
if resp.StatusCode >= 400 {
    return 0, c.classifyError(resp.StatusCode, string(raw))
}
```

---

## 4. 通用模式总结

### 4.1 鉴权头规范

| 渠道 | Authorization | 其他关键头 |
|------|--------------|-----------|
| WorkBuddy | `Bearer <accessToken>` | `User-Agent: CodeBuddy/1.0` |
| TraeWork | `Cloud-IDE-JWT <accessToken>` | `X-User-Region: CN`, `x-device-*` 系列 |
| Qoder | `Bearer dt-<token>` | 无特殊头 |

### 4.2 Token 刷新策略

- **WorkBuddy**: `refresh_token` 轮换后立即失效，需保存新 token
- **TraeWork**: `refresh_token` 轮换但可能保留旧 token 作为 fallback
- **Qoder**: 无刷新机制，token 过期需重新登录

### 4.3 余额计算统一逻辑

```go
remain = Σ(credits_limit - credits_amount) for all packs
```

**注意**:
- TraeWork 每个 pack 有 `usage.credits_amount`（实际用量）
- WorkBuddy 有 `CycleCapacityRemain`（周期剩余）优先取
- Qoder 直接返回 `remaining`

### 4.4 错误透传原则

上游 HTTP ≥400 直接返回原始 body，不在本地包装：
- 保持错误码清晰
- 便于调试和用户理解
- 避免重复翻译错误信息

---

## 5. 待调研问题

1. **TraeWork Work 专属积分**: 上游未暴露 `package_type` 字段，无法区分通用/专属额度
   - 建议：按 `display_desc` 或 `group_name` 过滤特定类型
   - 或联系上游获取更详细的额度分类

2. **Qoder 推理开关**: 需确认上游是否支持 `reasoning_effort` / `thinking` 参数
   - 当前实现：透传 `reasoning_effort` → `thinking`
   - 需验证上游兼容性

3. **TraeWork 设备指纹**: `x-device-id` 必须与账号注册设备一致，否则 401
   - 首次登录自动绑定设备
   - 换设备需重新登录

---

*文档版本：v1.0*  
*最后更新：2026-08-26*
