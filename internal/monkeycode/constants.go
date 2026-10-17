// constants.go MonkeyCode 平台托管模型渠道的静态常量。
//
// 取证来源：官方客户端 ohmyagent（Go 编写）经 `--model-config` 重定向抓包，
// 见 本地 docs/MonkeyCode渠道接入评估.md §3。本文件只放**非敏感**常量：
// 端点、协议版本、UA 与静态模型表。凭据（oma_ key / omas_ secret）一律来自
// auth 文件，**不得**出现在这里。
package monkeycode

import (
	"strings"
	"time"
)

const (
	// DefaultBase 上游 Anthropic 兼容端点前缀（请求打到 {base}/messages）。
	// 客户端 settings.json 里的 base_url 与之同形；导入时以客户端值为准，
	// 该常量只作缺省与静态兜底。
	DefaultBase = "https://proxy.monkeycode-ai.com/v1"

	// consoleBase 控制台站点。**与 agent 的 proxy 域是两个不同的域**：
	// 钱包等控制台接口只在 console 域上，且只认登录 Cookie（agent 的 oma_ key 401）。
	consoleBase = "https://monkeycode-ai.com"

	// baizhiBase 百智云站点。MonkeyCode 的**身份提供方**：控制台会话不是自己登录出来的，
	// 而是用百智云会话经 OAuth 派生（见 deriveConsoleSession）。
	baizhiBase = "https://baizhi.cloud"

	// epWallet 控制台钱包接口（GET，Cookie 认证）：
	// `{code,message,data:{balance, daily_token_balance, daily_token_limit}}`。
	// balance 单位是**毫积分**（console 前端 `balance/1e3`），daily_token_* 是 token 数。
	epWallet = "/api/v1/users/wallet"

	// epWalletCheckin 签到领积分（2026-10-17 取证，见 checkin.go 文件头）：
	// 路径取自官方桌面端二进制 `monkeycode-desktop.exe` 的明文串，与 epWallet 同域同 Cookie。
	// ⚠️ 方法（取 POST）与回包字段名未确认，是签名之外本渠道仅剩的推测项。
	epWalletCheckin = "/api/v1/users/wallet/checkin"

	// epOAuthAuthorize 百智云 OAuth 授权端点（GET，需 baizhi 会话）。
	// 带上它即等于用户在前端点了一次「同意授权」：302 到 redirect_uri 并附 `code`。
	// 参数取值与官方前端跳转的 URL 完全一致（client_id / scope / response_type）。
	epOAuthAuthorize = "/api/v1/oauth/authorize"
	oauthClientID    = "monkeycode-ai"
	oauthScope       = "user phone"
	// oauthCallbackPath 挂在**控制台域**上（即 {consoleBase} + 它），负责把 code 换成会话。
	oauthCallbackPath = "/api/v1/users/baizhi/callback"

	// anthropicVersion 与官方客户端一致（抓包实测）。
	anthropicVersion = "2023-06-01"

	// userAgent 官方客户端形态：`ohmyagent <版本>`。
	// 版本号取自抓包实测（客户端构建标识，非凭据）。
	userAgent = "ohmyagent f6b21ad"

	// SignatureHeader 签名头名。HTTP 头名大小写不敏感，官方下发形态为
	// `X-Ohmyagent-Signature`，值形如 `v1=<hex>`。
	SignatureHeader = "X-Ohmyagent-Signature"

	// requestTimeout 单次请求上限。上游是流式长响应，给足 10 分钟。
	requestTimeout = 10 * time.Minute
)

// 两侧会话 Cookie 的名字。导出是因为导入器（internal/app）要按名字从客户端
// cookie 文件里取值，客户端与服务端两处写法必须一致。
const (
	// CookieNameConsole 控制台会话（由百智云会话派生而来、短寿）
	CookieNameConsole = "monkeycode_ai_session"
	// CookieNameBaizhi 百智云会话（上游身份凭据、长寿）
	CookieNameBaizhi = "baizhi_session"
)

// signatureSystemPrompt 是**参与签名的第一条 system**（system[0].text）。
//
// 为什么用固定短句而不是复刻官方那段 5170 字提示词：
// 服务端只校验「签名与所发内容互相匹配」，**不校验内容是否为官方原文**
// （2026-09-23 端到端实测：28 字符自定义 system[0] + 自算签名 → HTTP 200）。
// 因此这里固定一条常量即可，好处是同一账号的签名**恒定**，可缓存复用。
//
// ⚠️ 该字符串**不得包含 \r\n**，也不得在运行时被改写——一旦内容变了签名必须
// 重算（见 sign.go 的注释与回归测试）。
const signatureSystemPrompt = "You are a helpful assistant."

// maxTokensDefault 客户端未指定 max_tokens 时的兜底值。
// 上游托管模型 max_output 为 32000，取 16384 与官方客户端主请求一致。
const maxTokensDefault = 16384

// maxTokensCap 单次请求 max_tokens 上限（上游模型 max_output 32000）。
const maxTokensCap = 32000

// 三档托管模型前缀（对外模型 ID 的形态；发给上游时前面补 `monkeycode-`）。
const (
	tierBasic = "basic/"
	tierPro   = "pro/"
	tierUltra = "ultra/"
)

// staticModels 静态兜底模型表（上游无 GET /models，返回 405）。
//
// 条目 ID 采用「去 monkeycode- 前缀」的形态（如 `basic/deepseek-flash`），
// 对外模型名为 `monkeycode/basic/deepseek-flash`；发给上游时再补回
// `monkeycode-` 前缀。这样既避免 `monkeycode/monkeycode-basic/...` 的重复，
// 又保留档位语义。
//
// 清单来自客户端 settings.json 的托管条目（2026-09-23 抓取，见评估文档 §3.2）。
var staticModels = []string{
	// basic
	"basic/deepseek-flash",
	"basic/glm-5.3-flash",
	"basic/kimi-k2.5",
	"basic/minimax-m2.5",
	"basic/qwen3.5-plus",
	"basic/qwen3.8-flash",
	// pro
	"pro/deepseek-flash",
	"pro/glm-5",
	"pro/hy3",
	"pro/kimi-k2.6",
	"pro/minimax-m3",
	"pro/qwen3.6-plus",
	"pro/gpt-5.6-terra",
	// ultra
	"ultra/glm-5.1",
	"ultra/qwen3.7-max",
	"ultra/gpt-5.6-sol",
	"ultra/gpt-6-astra",
	// 无档位前缀组（客户端里以裸名出现）
	"deepseek-flash",
	"glm-5.1",
	"glm-5",
	"kimi-k2.6",
	"minimax-m2.5",
	"minimax-m3",
	"qwen3.6-plus",
	"qwen3.7-max",
}

// upstreamModelName 把对外模型 ID 还原成上游模型名。
// 带档位前缀的条目补 `monkeycode-`；裸名条目原样返回；
// 已经是上游形态（`monkeycode-` 开头）的原样透传。
func upstreamModelName(id string) string {
	if hasPrefix(id, "monkeycode-") {
		return id
	}
	switch {
	case hasPrefix(id, tierBasic), hasPrefix(id, tierPro), hasPrefix(id, tierUltra):
		return "monkeycode-" + id
	default:
		return id
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// channelPrefix 是对外模型 ID 的渠道前缀（`monkeycode/basic/x`）。
const channelPrefix = "monkeycode/"

// bareModelID 归一化成静态表里的条目 ID：剥掉渠道前缀与上游 `monkeycode-` 前缀。
//
// 三个来源都要能吃：对外 ID（`monkeycode/basic/x`，Stream/Aggregate 的入参）、
// 渠道内 ID（`basic/x`，handler 剥过前缀的 body 字段）、
// 客户端原样 ID（`monkeycode-basic/x`，手填 auth 时可能出现的形态）。
func bareModelID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, channelPrefix)
	id = strings.TrimPrefix(id, "monkeycode-")
	return id
}

// staticSet 是 staticModels 的集合视图（本地校验用）。
var staticSet = func() map[string]bool {
	m := make(map[string]bool, len(staticModels))
	for _, id := range staticModels {
		m[id] = true
	}
	return m
}()

// responsesModels 是客户端 settings.json 里 `type: "openai-responses"` 的托管模型。
//
// 2026-09-24 实测：这类模型在 `{base}/messages` 上返回 404「路由不存在」，
// 必须走 `{base}/responses`，且**签名对象是 input[0]（role=system）的 content**——
// 把签名串放进 `instructions` 或顶层 `system` 一律 403（见评估文档 §3.9）。
var responsesModels = map[string]bool{
	"basic/qwen3.8-flash": true,
	"pro/gpt-5.6-terra":   true,
	"ultra/gpt-5.6-sol":   true,
	"ultra/gpt-6-astra":   true,
}

// modelDisplayNames 给出「上游 API 名 ≠ 客户端展示名」那几个模型的中文展示名。
//
// 客户端 settings.json 里这几条以中文作 key（`专业模型-5.6-terra`），但真正要
// 发给上游的是条目里 `model` 字段的 ASCII 名（`gpt-5.6-terra`）。对外 ID 必须用
// ASCII 名——用中文展示名请求会被上游回 **纯文本 `Forbidden`**（2026-09-24 实测；
// 与签名错的 `{"error":"invalid ohmyagent request"}` 是两种不同的 403）。
var modelDisplayNames = map[string]string{
	"pro/gpt-5.6-terra": "专业模型 5.6 terra",
	"ultra/gpt-5.6-sol": "极致模型 5.6 sol",
	"ultra/gpt-6-astra": "极致模型 6 astra",
}

// usesResponsesAPI 报告该模型是否走 OpenAI Responses 面。
func usesResponsesAPI(id string) bool {
	return responsesModels[bareModelID(id)]
}
