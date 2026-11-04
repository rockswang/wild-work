// Package provider 定义不同上游（workbuddy / traework）共用的最小接口。
// 只抽取 server/scheduler 必需能力，避免为未来平台过度设计。
package provider

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"wild-work/internal/auth"
)

// Kind 平台标识，同时也是模型名前缀。
type Kind string

const (
	WorkBuddy   Kind = "workbuddy"
	WorkBuddyAI Kind = "workbuddyai" // WorkBuddy 国际版（www.workbuddy.ai），与国内版完全独立
	TraeWork    Kind = "traework"
	Qoder       Kind = "qoder"    // QoderWork（qoder.com.cn，移植自 qoderwork2api）
	QoderCN     Kind = "qodercn"  // QoderCN（qoder.com.cn，移植自 qoder2api，独立渠道）
	QoderCOM    Kind = "qodercom" // QoderCOM 国际版（qoder.com / qoder.sh，移植自 qodercn）
	QwenWork    Kind = "qwenwork" // 千问办公（gateway.qwenwork.cn + qwenwork.cn）
	// MonkeyCode 平台托管模型（proxy.monkeycode-ai.com）：凭据由面板从本机官方客户端导入，
	// 一个账号 = oma_ api_key + omas_ signing_secret；上游无目录/额度/刷新接口，
	// 故模型表静态、额度恒 0、RefreshToken 为空实现。
	MonkeyCode Kind = "monkeycode"
	// Raccoon 小浣熊（xiaohuanxiong.com）：凭据由面板从本机官方客户端导入，
	// access_token 约 2h + refresh_token 约 30d，可用 refresh_token 自动续期。
	Raccoon Kind = "raccoon"
	// Loomy 讯飞 Loomy（loomyad.xunfei.cn）：凭据由面板从本机官方客户端导入，
	// session 约 14 天且上游无 refresh 端点，到期需重新导入。
	Loomy    Kind = "loomy"
	TraeCode Kind = "traecode" // Trae 代码版：与 TraeWork 同一上游、共用账号，function=solo_agent
	Oczen    Kind = "oczen"    // OpenCodeZen 匿名免费通道（opencode.ai/inference，无账号、凭证固定 public）
	GLM      Kind = "glm"      // 智谱清言（chatglm.cn 网页版私有接口，凭据为 chatglm_refresh_token）
	// Cline 免费池（api.cline.bot）：凭据是**官方文档化的 sk_ API Key**（非 OAuth），
	// 一个账号一把 Key；Key 不轮换 ⇒ 无 RefreshToken、无保活、无签到；
	// 只暴露 Cline 官方免费池，定位是多账号免费额度调度器 + 反代（见决议 R46）。
	Cline Kind = "cline"
	// OfficeAce 华为云果办（hw AgentArts 模型网关，OpenAI 兼容）：凭据来自本机
	// OfficeAce 桌面端文件（models.json 里的 Basic AK/SK）或自助 OAuth 登录；
	// 积分/签到走 V11 签名，续期走 DPoP + refresh_token。
	OfficeAce Kind = "officeace"
	// Kilo Kilo AI 免费池（api.kilo.ai/api/gateway）：**免密**、无账号、无签到、
	// 无积分；免费集合由上游清单的 isFree 字段声明（不按模型名猜）。
	// 与 oczen 同属「无账号渠道」形态（R16）：不建 auth 加载器、不进 reloadAccounts，
	// 虚拟账号由 main 装配时注入（kilo.AnonymousAuth）。
	Kilo Kind = "kilo"
	// Minimax MiniMax Code 中国版（account.minimax.cn + agent.minimax.cn）：
	// 登录走 OAuth 设备码 + PKCE；凭据是 access/refresh token（**非 JWT**，
	// 过期时间必须由 expires_in 自算，不能解 exp）；推理是 Anthropic Messages 形状，
	// 签到与积分走 minimax-cloud 端点。多账号渠道 ⇒ **不**置 SingleAccount。
	Minimax Kind = "minimax"
	// LobsterAI LobsterAI（有道龙虾）：API 在 lobsterai-server.youdao.com，对话是
	// **OpenAI 兼容**面（不是 Anthropic 形态）；登录走本机 127.0.0.1 随机端口回调 + state
	// （无 PKCE）；凭据必须持久化 keyfrom 四元组（uuid/first_keyfrom/latest_keyfrom/user_id）
	// —— 它们是 refresh 与模型列表的必需载荷。多账号渠道 ⇒ **不**置 SingleAccount。
	LobsterAI Kind = "lobsterai"
	// Gemini Google Code Assist（cloudcode-pa.googleapis.com）：推理走自有 `v1internal` 信封，
	// 鉴权是 Bearer + **逐字身份五头**，请求体必须**逐层字母序序列化**；配额是窗口百分比、无签到。
	Gemini Kind = "gemini"
	// CodeArts 华为云 CodeArts Agent：对话走华为 snap-access（**SDK-HMAC-SHA256 签名**），
	// 登录是 PKCE OAuth；头名是否参与签名必须逐字节对齐（`maas_type` 要参与、`Agent-Type` 不参与）。
	CodeArts Kind = "codearts"
	// ZCode 智谱 ZCode：Anthropic Messages 形态；请求体 `system` 必须内嵌官方身份块（否则 405/3012）；
	// 登录是服务端中介设备授权流。⚠️ **积分领取依赖阿里云验证码 + headful 浏览器**，
	// 本仓不实现领取路径（渠道包注释里注明）。
	ZCode Kind = "zcode"
	// Comate WPS Comate（comate.wps.cn 网页版模型代理）：粘贴型渠道，凭据是 WPS 账号的
	// **wps_sid Cookie**（浏览器登录 www.wps.cn 后复制），由面板「粘贴 wps_sid」写入 auths/。
	// Cookie 不轮换 ⇒ 无 RefreshToken、无保活、无签到；上游无积分查询端点（积分只能从
	// 逐请求 usage.credits 记账）。模型面绑死上游 /models 的 items 数组原样透传。
	Comate Kind = "comate"
	// Lingxi WPS 灵犀（lingxi.kdocs.cn 网页版助手）：粘贴型渠道，凭据与 comate 同为
	// **wps_sid Cookie**（同一 WPS 账号体系但**门禁独立**——灵犀侧开通即可用）。
	// 有每日签到（领智点）与余额端点；对话是 sessions+completions 两段式（每请求新建
	// session）+ 自有 SSE 帧型（非 OpenAI 形）。模型面来自 plans 端点 data.models 原样透传。
	Lingxi Kind = "lingxi"
)

func (k Kind) String() string { return string(k) }

// BuiltinKinds 本仓内建渠道集合（R66）。
//
// 用于拒绝自定义渠道占用内建渠道名：两者同名时两层会指向不同 Runtime
// （server 侧 getRuntime 内建优先，而 app 侧 UpdateCustomRuntimes 无条件覆盖），
// 表现为「面板/账号操作打到自定义池、对话请求走内建渠道」——不报错但静默出错。
// 同时排除 ModelNamespaces 里的上游命名空间（如 cline-free），以免路由歧义。
func BuiltinKinds() map[string]bool {
	m := make(map[string]bool, len(builtinKindList))
	for _, k := range builtinKindList {
		m[k.String()] = true
	}
	for ns := range ModelNamespaces {
		m[ns] = true
	}
	return m
}

// builtinKindList 内建渠道全集（唯一真源）：BuiltinKinds / DisplayNames / ChannelDisplayOrder 都由此派生。
// 新增渠道只改这一处 + 下方 DisplayNames，漏登记由 provider_test.go 与 ui-static-check 双向兜住。
var builtinKindList = []Kind{
	WorkBuddy, WorkBuddyAI, TraeWork, Qoder, QoderCN, QoderCOM,
	QwenWork, MonkeyCode, Raccoon, Loomy, TraeCode, Oczen, GLM, Cline, OfficeAce, Kilo, Minimax, LobsterAI,
	Gemini, CodeArts, ZCode, Comate, Lingxi,
}

// DisplayNames 渠道**显示名**的 Go 侧真源（面板文案、`/v1/models` 之外的展示处同源）。
//
// 前端 `cmd/wild-work/web/app.js` 的 CH_LABEL 是同一份数据的 JS 侧副本 ——
// 两份必须逐项一致，`scripts/ui-static-check.mjs` 有跨语言一致性断言（Go 源它直接读）。
// 顺序无关（展示序由 ChannelDisplayOrder 按值排序派生）。
var DisplayNames = map[Kind]string{
	WorkBuddy:   "WorkBuddyCN",
	WorkBuddyAI: "WorkBuddyAI",
	TraeWork:    "TraeWork",
	TraeCode:    "TraeCode",
	Qoder:       "Qoder",
	QoderCN:     "QoderCN",
	QoderCOM:    "QoderCOM",
	QwenWork:    "千问办公",
	GLM:         "智谱清言",
	MonkeyCode:  "MonkeyCode",
	Raccoon:     "小浣熊",
	Loomy:       "Loomy",
	Oczen:       "OpenCodeZen",
	Kilo:        "Kilo",
	Minimax:     "MiniMax",
	LobsterAI:   "LobsterAI",
	ZCode:       "ZCode",
	Gemini:      "Gemini",
	CodeArts:    "CodeArts",
	Cline:       "Cline",
	OfficeAce:   "OfficeAce",
	Comate:      "WPS Comate",
	Lingxi:      "WPS 灵犀",
}

// DisplayName 渠道显示名；未登记时退回渠道 kind 本身（自定义渠道就走这条）。
func DisplayName(k Kind) string {
	if n, ok := DisplayNames[k]; ok {
		return n
	}
	return k.String()
}

// ChannelDisplayOrder 面板渠道展示顺序的**唯一真源**（R142）。
//
// 规则（用户 2026-10-30 拍板）：内建渠道**按显示名的 UTF-8 字节序升序** ——
// 纯 ASCII 名字自然排在前（Cline → CodeArts → … → ZCode），中文显示名（千问办公 / 小浣熊 /
// 智谱清言）因码位高自然垫底，`WPS Comate` / `WPS 灵犀` 落在 `ZCode` 之前。
// 刻意不引入拼音表：那是额外依赖 + 另一份需要人工维护的真源，而 Unicode 码位序自带稳定定序。
//
// 自定义渠道（R66）**不入本表**：使用方统一把它们钉在最末（见 app 的 feeChannelOrderWithDynamic）。
var ChannelDisplayOrder = sortedKindsByDisplayName(builtinKindList)

// sortedKindsByDisplayName 按显示名字节序升序排序（副本，不修改入参）。
func sortedKindsByDisplayName(ks []Kind) []Kind {
	out := append([]Kind{}, ks...)
	sort.SliceStable(out, func(i, j int) bool { return DisplayName(out[i]) < DisplayName(out[j]) })
	return out
}

// ModelNamespaces 声明「上游 ID 自带命名空间」的渠道：命名空间前缀 → 渠道 kind。
//
// 这类渠道对客户端**直接透传上游 ID**，不再拼 `<kind>/` 前缀（决议 R59，首例为 cline 免费档：
// 上游 free[] 的 ID 就是 `cline-free/*`）。路由时该前缀映射回渠道，且**整串**即上游模型名。
// 同时旧的 `<kind>/<上游ID>` 双层写法（如 `cline/cline-free/*`）继续有效 —— 老配置/老客户端
// 里的映射表与降级链都是那种写法，不能失效。
var ModelNamespaces = map[string]Kind{
	"cline-free": Cline,
}

// PublicModelID 返回客户端应填写的模型 ID（面板复制 / `/v1/models` / 报错提示同源）。
// 上游 ID 自带命名空间时直接透传上游 ID；其余渠道拼 `<kind>/<上游ID>`。
func PublicModelID(k Kind, upstreamID string) string {
	up := strings.TrimSpace(upstreamID)
	if i := strings.IndexByte(up, '/'); i > 0 {
		if kk, ok := ModelNamespaces[up[:i]]; ok && kk == k {
			return up
		}
	}
	return k.String() + "/" + up
}

// SplitModelID 把客户端模型 ID 解析为 (渠道, 上游模型名)。
// 两种形态均接受，第二个返回值恒为「应该发给上游的模型名」：
//
//	① `cline-free/mimo-v2.6-flash`      → (cline, 整串)            ← 上游命名空间直通（R59）
//	② `cline/cline-free/mimo-v2.6-flash` → (cline, 去掉 cline/ 后的串) ← 旧写法兼容
//	③ `workbuddy/glm-5.2`               → (workbuddy, glm-5.2)
//
// 无 `/`、或首段空、或尾段空的串一律返回 ok=false（由调用方报错）。
func SplitModelID(model string) (Kind, string, bool) {
	m := strings.TrimSpace(model)
	i := strings.IndexByte(m, '/')
	if i <= 0 || i == len(m)-1 {
		return "", "", false
	}
	if k, ok := ModelNamespaces[m[:i]]; ok {
		return k, m, true // 上游命名空间：整串就是上游模型名
	}
	return Kind(m[:i]), m[i+1:], true
}

// UpstreamModelOf 把客户端模型 ID 归一为**发给上游的模型名**（账务/统计用：新旧写法必须落同一行）。
// 无前缀的裸名原样返回（compat 层按 default_channel/model_map 映射，此处不做猜测）。
func UpstreamModelOf(model string) string {
	if _, up, ok := SplitModelID(model); ok {
		return up
	}
	return strings.TrimSpace(model)
}

// CanonicalModelID 把客户端模型 ID 归一为**规范对外 ID**：同一模型的新旧两种写法归一到同一串。
// 例：`cline-free/x` 与 `cline/cline-free/x` 均 → `cline-free/x`；`workbuddy/glm-5.2` 不变。
// 用于运行统计/日志归行（同一个模型不能因客户端写法不同而在榜单上裂成两行）；裸名原样返回。
func CanonicalModelID(model string) string {
	if k, up, ok := SplitModelID(model); ok {
		return PublicModelID(k, up)
	}
	return strings.TrimSpace(model)
}

// ModelIDPrefix 返回该渠道对外模型 ID 的**首选前缀**（compat 层的 default_channel 兜底拼接用）：
// 上游命名空间渠道（cline）返回命名空间（`cline-free`），其余返回 kind 本身。
func ModelIDPrefix(k Kind) string {
	if ns, ok := kindNamespace[k]; ok {
		return ns
	}
	return k.String()
}

// kindNamespace 是 ModelNamespaces 的反查表（包初始化时构建，避免地图遍历序不确定）。
var kindNamespace = func() map[Kind]string {
	m := make(map[Kind]string, len(ModelNamespaces))
	for ns, k := range ModelNamespaces {
		if _, dup := m[k]; !dup {
			m[k] = ns
		}
	}
	return m
}()

// ErrKind 错误分类，驱动 pool 冷却状态机。
type ErrKind int

const (
	ErrNone           ErrKind = iota // 成功
	ErrHardCredit                    // 余额/权益不足 → 长冷却
	ErrSoftRate                      // 429 软限流 → 短冷却
	ErrSessionDead                   // 登录态失效 → 禁用
	ErrNotFound                      // 404 上游偶发 → 短冷却不累计 errCount
	ErrServer                        // 5xx 上游故障
	ErrClient                        // 其他 4xx / 业务错误
	ErrContentBlocked                // 内容策略拦截（400 + 审核文案）→ 不罚账号，透传原文
	ErrPromptTooLong                 // 11115 上下文超限 → 请求级错误，不罚号不轮转，透传原文
	ErrImageInvalid                  // 图片格式/数据无效（11135 等）→ 请求级错误，不罚号不轮转，透传原文
	ErrBadParams                     // 出站 body / 请求参数无法被上游接受 → 请求级错误，不罚号不轮转，透传原文
	ErrWafBlock                      // 403 + 非业务信封（WAF 拦截页/空体）→ 账号软冷却
	ErrAccountFault                  // 账号级授权/配额故障（11140/14017）→ 冷却轮换
	ErrModelBlocked                  // 11102 该后端无此模型 → (账号,模型) 负缓存避让
	ErrPassthrough                   // 请求级拒绝（形态/地域/身体不被接受）→ 原文透传，不罚账号不轮转
	ErrQueued                        // 上游「模型排队中」提示（qodercn 10605 isQueued）→ 换号可能命中不同队列
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	case ErrContentBlocked:
		return "content_blocked"
	case ErrPromptTooLong:
		return "prompt_too_long"
	case ErrImageInvalid:
		return "image_invalid"
	case ErrBadParams:
		return "bad_params"
	case ErrWafBlock:
		return "waf_block"
	case ErrAccountFault:
		return "account_fault"
	case ErrModelBlocked:
		return "model_blocked"
	case ErrPassthrough:
		return "passthrough"
	case ErrQueued:
		return "queued"
	default:
		return "none"
	}
}

// Rotatable 报告该错误是否应换下一个账号重试本请求（账号级错误）。
// 账号有问题的（限流/欠费/登录态死/偶发 404/账号故障/排队）：换号有价值；
// 请求内容有问题的（内容拦截/上下文超限/模型不存在）：换号必然复现，透传原文结束；
// ErrClient（其他 4xx 业务错误）可换号——实测 workbuddyai 11158「model is not allowed for
// user type [personal_free_user]」就是账号等级差异，换到 pro 号即放行。
//
// ⚠️ ErrServer（5xx）**不在**此列（2026-10-20 除名）：实测 workbuddyai 11134
// 「the model provider is temporarily unavailable」在同一波里 4 个号全 500，且每个号
// 内部客户端已自行重试 3 次 ⇒ 换号必撞同一面墙，只是把 1 次失败放大成 9 次。
func (k ErrKind) Rotatable() bool {
	switch k {
	case ErrSoftRate, ErrHardCredit, ErrSessionDead, ErrNotFound, ErrAccountFault, ErrClient, ErrQueued:
		return true
	default:
		return false
	}
}

// Queued 上游「模型排队中」提示：HTTP 200 里的一帧业务错误，不是拒绝。
//
// 首例 qodercn deepseek-flash（2026-10-20 生产实录）——外层 headers 带 Retry-After，
// body 内三层嵌套：{code:403} → {code:10605} → {isQueued:true, modelKey:"dfmodel",
// queueCount:8407, retryAfterSeconds:30, waitTime:509}。
//
// 账号本身健康（同号其他模型照用），拥塞发生在网关的**模型维度**上；换号可能命中不同队列。
// 该帧必须在**写出任何字节之前**识别：把这种没有 choices 的帧当普通 chunk 透传，
// 客户端只会显示「正在等待模型…」，直到网关 180s 后掐断连接（实测三次 180.3/180.7/180.8s）。
type Queued struct {
	ModelKey    string
	QueueCount  int
	RetryAfter  time.Duration // 上游建议的重试间隔（外层 Retry-After 或 retryAfterSeconds）
	WaitSeconds int           // 上游估计的排队时长（waitTime）
}

func (e *Queued) Error() string {
	msg := "model queued upstream"
	if e.ModelKey != "" {
		msg += " (" + e.ModelKey + ")"
	}
	if e.QueueCount > 0 {
		msg += fmt.Sprintf(" queue=%d", e.QueueCount)
	}
	if e.WaitSeconds > 0 {
		msg += fmt.Sprintf(" wait=%ds", e.WaitSeconds)
	}
	if e.RetryAfter > 0 {
		msg += fmt.Sprintf(" retry_after=%s", e.RetryAfter)
	}
	return msg
}

// Kind 实现 StreamErrorClassifier：流在写出字节之前命中排队时，handler 据此走换号路径。
func (e *Queued) Kind() ErrKind { return ErrQueued }

// Cooldown 本次「账号×模型」应冷却多久：优先用上游 retryAfter，缺失时退 30s。
func (e *Queued) Cooldown() time.Duration {
	if e.RetryAfter > 0 {
		return e.RetryAfter
	}
	return 30 * time.Second
}

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// Classified 把 *Error 包装成实现了 StreamErrorClassifier 的错误，供**流式路径**使用。
//
// 为什么需要包装而不是直接给 Error 加 Kind() 方法：字段名就叫 Kind，与方法同名会冲突。
// 为什么流式路径需要它：渠道在流首帧发现「上游 200 里包着业务错误」时（如 qodercom 的
// envelope 403 → code 110/112 额度耗尽），一字节未写就上抛错误——此刻还来得及换号，
// 但 handler 必须能问出错误类别（ErrHardCredit ⇒ 整号硬冷却 + 换号）。
// 不包装则只能走「已写流」分支（只罚不换），5 个号全耗尽时客户端只能干等下次请求。
type Classified struct{ Err *Error }

// Kind 实现 StreamErrorClassifier。
func (c *Classified) Kind() ErrKind { return c.Err.Kind }

func (c *Classified) Error() string { return c.Err.Error() }

// Unwrap 让 errors.As(err, &pe) 仍能取回原始 *provider.Error（保留 Status/Msg）。
func (c *Classified) Unwrap() error { return c.Err }

// AsClassified 把任何带 Kind 字段的 provider.Error 转成 StreamErrorClassifier 可识别的错误；
// 不是 *provider.Error 时原样返回（如 *Queued 自己就实现了 Kind()）。
func AsClassified(err error) error {
	var pe *Error
	if errors.As(err, &pe) {
		return &Classified{Err: pe}
	}
	return err
}

// ModelInfo 动态/静态模型信息。
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64
	MaxTokens     int64
	// ContextFromAPI 标记 ContextWindow/MaxTokens 是否来自上游接口。
	// false 表示是硬编码估算/占位（如渠道不返回该字段），
	// 消费方（面板/API）不应把估值当作真实容量展示。
	ContextFromAPI bool
	// HandMeasured 标记 ContextWindow/能力字段是**本工具手工实测**得出的（非上游下发、
	// 也非拍脑袋估算）。首例：cline 的 `measuredCaps` —— 上游 recommended-models
	// 完全不返回上下文/能力字段，只能逐个实测（三暗号回忆验证排除静默截断）。
	//
	// 与 ContextFromAPI 的关系：两者互斥，同置时以 ContextFromAPI 为准。
	// 为什么单独一个标志而不是直接置 ContextFromAPI=true：口径不同——
	// 实测值不是「上游声明」，面板展示时必须能如实标注来源（否则就是把实测
	// 冒充成官方值，违反 R46/issue #40 的「不把缺失信息当成确定结论」原则）。
	// 消费方约定：has_context = ContextFromAPI || HandMeasured || ContextFromUser。
	HandMeasured bool
	// ContextFromUser 标记上下文/能力字段是**用户在面板手填**的（自定义渠道专用）：
	// 既不是上游声明，也不是本工具实测，但同样是「有人对它负责」的确定值，
	// 所以也要能展示（否则用户填了上下文却永远显示「未知」，等于白填）。
	// 三者互斥，来源不同只为面板如实标注，不参与任何路由/容量计算。
	ContextFromUser bool
	// 以下能力字段均为「已确认为 true」才置 true；数据缺失时保持 false（未知）。
	// 未知与「明确不支持」在语义上不同，但对消费方（/v1/models 声明、UI 图标）
	// 的处理一致：不声明该能力。需要区分时由各渠道自行记录来源。
	SupportsImages    bool // 支持图像输入（多模态视觉）
	SupportsReasoning bool // 支持思考/推理模式
	SupportsTools     bool // 支持函数/工具调用
	// 思考档位（issue #96）：上游目录显式声明的可选档位表。nil = 上游未声明，
	// 消费方不得自行编造（对齐 ContextFromAPI 哲学；参考 Buddy2api v2.1.11 的
	// 「never invent effort lists」原则）。典型值 low/medium/high/xhigh/max。
	ReasoningEfforts []string
	// DefaultReasoningEffort 上游声明的默认档位；空 = 未声明。
	DefaultReasoningEffort string
	// CanDisableThinking 该模型思考是否可关闭；nil = 上游未声明。
	// 上游只对显式 true 语义不同（false 可能是「不可关闭」也可能是「未声明」，
	// 由各渠道解析时保留原始三态）。
	CanDisableThinking *bool
}

// InputModalities 按 OpenAI 生态惯例给出输入模态列表（OpenRouter/llama.cpp 的
// architecture.input_modalities 语义）。不支持图像时只返回 ["text"]。
func (m ModelInfo) InputModalities() []string {
	if m.SupportsImages {
		return []string{"text", "image"}
	}
	return []string{"text"}
}

// Modality 返回 OpenRouter 风格的 modality 描述串（如 "text+image->text"）。
func (m ModelInfo) Modality() string {
	if m.SupportsImages {
		return "text+image->text"
	}
	return "text->text"
}

// CleanEfforts 规范化上游声明档位表：去空白、去重、丢空串；全部无效时返回 nil
// （「上游没给合法列表就不编档」，Buddy2api model_reasoning.py 同款语义）。
func CleanEfforts(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := make(map[string]bool, len(raw))
	for _, s := range raw {
		if s = strings.TrimSpace(s); s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ModelPricing 模型积分定价（从上游 API 拉取）。
type ModelPricing struct {
	Model   string  `json:"model"`
	Channel string  `json:"channel"`
	Rate    float64 `json:"rate"`
	Note    string  `json:"note,omitempty"` // 促销/标签文案（已剔除颜色）
	// Color 是上游 badge 附带的颜色（形如 "#FF0000"），无则空。
	Color string `json:"color,omitempty"`
	// Explicit 标记 Rate 是否来自上游显式倍率字段。
	// 例如 CN 的 auto 模型根本没有 credits 字段，其 Rate 值无意义，
	// Explicit=false 时不应当作「免费」展示（应为未知）。
	// 零值兼容：老缓存/老实现未设置时视为 true（保持原有行为）。
	Explicit *bool `json:"explicit,omitempty"`
}

// IsExplicit 报告该定价是否来自上游显式倍率字段（未设置时按 true 处理）。
func (p ModelPricing) IsExplicit() bool {
	return p.Explicit == nil || *p.Explicit
}

// Upstream 是 server/scheduler 依赖的最小上游能力集合。
type Upstream interface {
	RefreshToken(a *auth.Auth) error
	ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error)
	FetchModels(a *auth.Auth) ([]ModelInfo, error)
	FetchModelPricing(a *auth.Auth) ([]ModelPricing, error)
	UserResource(a *auth.Auth) (int64, error)
	// UserResourceDetail 返回可消耗余额 + 明细条目（含到期时间与可用性标记）。
	// 返回的 remain 口径与 UserResource 一致，均为「本工具可消耗」的余额，
	// 不得包含不可用池——否则 pool 会按虚高余额选号。
	UserResourceDetail(a *auth.Auth) (int64, []ResourceItem, error)
	DailyCheckin(a *auth.Auth) error
	Classify(status int, body string) ErrKind

	// Stream/Aggregate 的 model 参数是「客户端请求的原始模型名」（含 channel/ 前缀），
	// 由调用方显式传入而非渠道内部记忆状态——后者在多账号并发下会串号。
	// 实现方应在输出的 model 字段回填该值（上游常返回 "auto" 或裸名）。
	// Stream 返回值为末帧捕获的 usage（OpenAI 形状，pt/ct/total），供 handler 记 token 流水；
	// 上游未返回 usage 时为 nil（调用方记 0 token + 请求数）。
	Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error)
	Aggregate(r io.Reader, model string) (map[string]any, error)
}

// StreamErrorClassifier 由「流内业务错误可分类」的渠道实现（当前 traework 的
// SOLOStreamError）。handler 用 errors.As 取值，据此决定账号惩罚——
// 使 server 层不必 import 具体渠道包。
//
// 为什么需要它：流式请求的 HTTP 200 与响应头在读到上游错误帧**之前**就已发出，
// 协议上无法在同一请求内换号重试。唯一可行的补救是「把中招账号冷却掉」，
// 让客户端**下一次**重试时由挑号逻辑换到别的账号（3004 是账号级限流，换号有效）。
type StreamErrorClassifier interface {
	Kind() ErrKind
}

// CheckinReporter 由「签到结果可结构化上报」的渠道实现（当前 QoderCN / QoderCOM）。
// 调度器优先使用它；未实现的渠道回退 DailyCheckin + isAlready 文本判定。
// 之所以需要独立接口：error 通道无法区分 no_campaign 与 error——两者都不是
// 「已签到」，却都需要在当日窗口内继续重试，而 error 文本判定做不到。
type CheckinReporter interface {
	DailyCheckinReport(a *auth.Auth) (CheckinReport, error)
}

// CheckinGranter 由「签到回执自带发放额」的渠道实现（当前 WorkBuddy，issue #67）。
// 上游 daily-checkin 回执的 credit/today_credit 是本次发放的权威值；调度器在签到
// 成功后用它直接记 earn（ledger.RecordCheckinEarn），不再完全依赖快照差分——
// WorkBuddy 签到包落在「明日到期」key 上且提前一天以 r=0 建档，「发放即消耗」
// 场景下差分会把当日 earn 抵消为 0（面板今日收入显示 0 的根因）。
// granted<=0 时调度器静默降级回差分口径；未实现本接口的渠道行为不变。
type CheckinGranter interface {
	DailyCheckinGrant(a *auth.Auth) (granted int64, err error)
}

// CheckinStatus 是签到结果状态（语义对齐上游 qoder2api 的 checkinStatus*）。
// 决定调度器的「当日是否已完成」判定与窗口内重试策略。
type CheckinStatus string

const (
	// CheckinClaimed 本次真正领取成功（金额可能为 0：上游未回填 benefit）。
	CheckinClaimed CheckinStatus = "claimed"
	// CheckinAlready 今日已领取（幂等：409 / CLAIMED / replayed）。
	CheckinAlready CheckinStatus = "already_claimed"
	// CheckinNoCampaign 无可用签到活动（含 legacy DISABLED）：可能是活动尚未创建，
	// 属于「可重试」状态——上游 10:00 整点存在延迟。
	CheckinNoCampaign CheckinStatus = "no_campaign"
	// CheckinNoToken 本地无可用凭据（可重试，refresh 后可能恢复）。
	CheckinNoToken CheckinStatus = "no_token"
	// CheckinError 其它失败（可重试）。
	CheckinError CheckinStatus = "error"
)

// Retryable 报告该状态是否应在当日窗口内继续重试。
// 只有「已领取」和「本次领取成功」算完成——上游同款判定。
func (s CheckinStatus) Retryable() bool {
	switch s {
	case CheckinClaimed, CheckinAlready:
		return false
	default:
		return true
	}
}

// CheckinReport 单次签到的结构化结果（Upstream.DailyCheckinReport 返回值）。
type CheckinReport struct {
	Status CheckinStatus // 结果状态
	Msg    string        // 展示文案（含金额），可为空
	Amount int64         // 本次领取金额（仅 Status==CheckinClaimed 时有意义）
}

// ResourceItem 积分明细条目。
type ResourceItem struct {
	Name   string `json:"name"`
	Total  int64  `json:"total"`
	Used   int64  `json:"used"`
	Remain int64  `json:"remain"`

	// ExpireAt 该条目的到期时间。**两种写法并存，消费方须同时支持**：
	//   - "YYYY-MM-DD"（UTC+8 墙钟）：该日**全天可用**，次日 00:00 起失效。全仓主口径，
	//     当日有效类额度（每日发放池等）统一走 ExpireDateToday()；
	//   - RFC3339（如 "2026-10-18T15:00:00+08:00"）：**精确时刻**，到点失效。需要时分
	//     语义时才用（gemini 的 5 小时/周配额窗口重置时间就是这一类）。
	// 空串表示上游未下发到期时间，前端据此隐藏「有效期」列——不得用零值时间冒充「永不过期」。
	// 判定统一走 ExpireInstant（是否已失效）/ ExpiringWithin（是否临期），**不要各自 Parse**：
	// 旧实现各处自解析且吞掉错误，导致 RFC3339 的条目被判成「永不过期」（2026-10-18 核对）。
	ExpireAt string `json:"expire_at,omitempty"`
	// Key 条目稳定标识（上游提供的 ID，如 TraeWork entitlement_id），
	// 供 ledger 差分对账用；渠道无 ID 时留空，差分退回 Name 作伪键。
	Key string `json:"key,omitempty"`
	// Usable 标记该条目是否属于本工具可消耗的额度池。
	// TraeWork 存在按 available_endpoint 划分的专用池（ep=1，官方客户端专用），
	// 本工具走的是 ep=0；这类额度对用户是「看得见用不了」，需在界面上分开统计。
	// 注意：零值为 false，故各渠道构造时须显式置位；渠道无此概念时统一填 true。
	Usable bool `json:"usable"`
	// InfoOnly 标记该条目**只作展示**，不参与任何积分算术（Summarize 小计、
	// ExpiringWithin 临期、ledger 差分）。用于「同一账号下计量单位不同的另一套额度」——
	// 如 MonkeyCode 的每日 Token 额度（单位是 token，而积分是 credits）：
	// 两者都该显示，但相加无意义。
	// 注意：InfoOnly 条目应同时置 Usable=true（它在界面上既非「不可用」，也不该被小计）。
	InfoOnly bool `json:"info_only,omitempty"`
	// Unit 该条目的计量单位：空串 = 积分（credits，默认），`"token"` = Token 计数。
	//
	// 为什么需要它：有些渠道的额度**不以积分计量**（ZCode 的 billing/balance 是 token 桶，
	// MonkeyCode 的每日额度也是 token）。这类数字按「积分」展示会严重误导（上亿 token 看起来
	// 像巨额积分），但完全隐藏又让用户看不到用量。故用 Unit 明确标注计量单位：前端据此把
	// 该渠道渲染成「Token 计数」面板，并与积分**分开统计**（不并入积分合计）。
	// 零值（空串）保持既有语义 = 积分，向后兼容。
	Unit string `json:"unit,omitempty"`
}

// Summarize 按 Usable 标记汇总条目：返回 (可消耗剩余, 不可消耗剩余)。
// 供 app 层统一填充 ResourceDetail 接口的两个小计字段，避免多处各写一份循环。
func Summarize(items []ResourceItem) (usable, unusable int64) {
	for _, it := range items {
		if it.InfoOnly {
			continue
		}
		if it.Usable {
			usable += it.Remain
		} else {
			unusable += it.Remain
		}
	}
	return usable, unusable
}

// expireLoc 各渠道 ExpireAt 的统一墙钟口径（见各渠道 softRateResetLoc，固定 UTC+8）。
var expireLoc = time.FixedZone("UTC+8", 8*60*60)

// ExpireDateToday 返回 UTC+8 墙钟下的今天日期（YYYY-MM-DD）。
//
// 供渠道为「当日有效」类额度（如每日发放池）填 ExpireAt —— 这类额度上游只下发数值、
// 不下发到期时间，但语义上当天结束即作废。统一走本函数可保证与 ExpiringWithin、
// 前端「有效期」列、ledger 差分共用同一日期粒度与时区，避免各渠道各自算。
func ExpireDateToday() string {
	return time.Now().In(expireLoc).Format("2006-01-02")
}

// ExpireInstant 把 ExpireAt 解析为**失效时刻**（UTC+8 墙钟）。
//
// 两种写法的语义差别（见 ResourceItem.ExpireAt）：
//   - "YYYY-MM-DD"：该日全天可用 ⇒ 失效时刻 = **次日 00:00**；
//   - RFC3339：精确时刻，到点失效。
//
// ok=false 表示空串或不可解析 —— 调用方按「无到期信息」处理：既不当「永不过期」，
// 也不当「已过期」。
func ExpireInstant(expireAt string) (time.Time, bool) {
	s := strings.TrimSpace(expireAt)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.ParseInLocation("2006-01-02", s, expireLoc); err == nil {
		return t.AddDate(0, 0, 1), true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(expireLoc), true
	}
	return time.Time{}, false
}

// ExpireDate 返回 ExpireAt 的**日期部分**（UTC+8 墙钟，"YYYY-MM-DD"）。
//
// 用途：**生成 ledger 伪键**。伪键必须只含日期、不能含时刻——伪键是账本差分的
// 身份标识，一改就会让旧键全部「消失」、新键全部「出现」，升级瞬间凭空产出一批
// expire + earn 流水（净额守恒但会把「今日作废」刷成虚高）。
// 典型误用（2026-10-19 方案评审中发现）：把 ExpireAt 从 "2026-10-25" 改成
// "2026-10-25T23:59:59+08:00" 后，workbuddy 的 "包名|到期日" 伪键跟着变，
// 单个账号 28 个包 ⇒ 56 条假流水。
//
// 因此渠道侧必须**分开两个函数**：值用精确时刻（供剩余小时计算），
// 键用本函数取日期前缀（保证账本稳定）。
func ExpireDate(expireAt string) string {
	t, ok := ExpireInstant(expireAt)
	if !ok {
		return ""
	}
	return t.In(expireLoc).Format("2006-01-02")
}

// ExpireIn 返回距失效还剩多久（负数 = 已过期）。ok=false 表示无到期信息。
// 这是面板「N 小时内到期」分桶与「最近临期」列表的唯一判据：
// 直接用 ExpireInstant 减当前时刻，**不做日期取整**。
//
// 为何不能用日期差（2026-10-19 用户实测纠正）：上游给的是精确时刻，且两种形态
// 都真实存在——qwenwork 每日奖励 valid_to = "...T00:00:00+08:00"（次日零点失效），
// traework 每月登录积分 expire_time = "...23:59:59"（当日末失效）。按日期桶
// 展示会把「今晚 5.7 小时后就没」误报成「明日到期，还有一整天」。
func ExpireIn(expireAt string, now time.Time) (time.Duration, bool) {
	t, ok := ExpireInstant(expireAt)
	if !ok {
		return 0, false
	}
	return t.Sub(now), true
}

// expireAnchor 临期判定的**锚点**：日期粒度取该日 00:00（不是次日），RFC3339 取精确时刻。
//
// 与 ExpireInstant 只差日期粒度这一档，是为了保持 ExpiringWithin 的既有口径不变：
// 「到期日 ≤ 明天」——今天到期/明天到期都算临期，后天起不算（24h 阈值下宽一档，
// 给用户提前一天准备）。若改成失效时刻，明天到期的条目会从临期里消失，属于行为变更，
// 不在本次（补 RFC3339 支持）范围内。
func expireAnchor(expireAt string) (time.Time, bool) {
	s := strings.TrimSpace(expireAt)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.ParseInLocation("2006-01-02", s, expireLoc); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.In(expireLoc), true
	}
	return time.Time{}, false
}

// ExpireBucket 自然日分档：把「到期时刻」归入今日 / 明日 / 7 日内三个桶。
//
// 口径（2026-10-19 用户明确定义）：**按精确时刻算，不按日期字符串比**，
// 桶的边界是**自然日**，且**次日 00:00:00 算今日到期**。
//
// 为什么次日零点要归今日：上游很多包的到期时刻就是次日零点——
// qwenwork 每日奖励 valid_to = "...T00:00:00+08:00"、traework 每日签到
// expire_time = "次日 00:00:03~07"（几秒是上游时钟抖动）。从用户视角
// 「今晚 24:00 就没了」是今天必须处理的事；归到「明日」会让人以为还有一整天
// （2026-10-19 实测：415 分被误报成「明日到期」，实际只剩 5.7 小时）。
//
// 实现：先给失效时刻减去 grace 宽限，再取**日期**分档。等价于
// 「凌晨 grace 内失效的包算前一天」。grace 取 5 分钟：实测上游抖动最大 7 秒，
// 5 分钟足够覆盖时钟偏差，又不会把「凌晨 0:10 失效」这种真正的次日包误归今日。
const expireMidnightGrace = 5 * time.Minute
const (
	BucketNone     = iota // 无到期信息，或已失效
	BucketToday           // 今日到期（含次日零点）
	BucketTomorrow        // 明日到期
	Bucket7d              // 7 日内到期
)

// expireDayOf 返回失效时刻所属的**自然日**（已扣除零点宽限）。
// 次日 00:00:03 这类会归到前一天；返回 ok=false 表示无到期信息。
func expireDayOf(expireAt string, now time.Time) (time.Time, bool) {
	t, ok := ExpireInstant(expireAt)
	if !ok {
		return time.Time{}, false
	}
	t = t.In(expireLoc).Add(-expireMidnightGrace)
	if !t.After(now.In(expireLoc)) { // 已失效
		return time.Time{}, false
	}
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, expireLoc), true
}

// ExpireBucketOf 返回 expireAt 属于哪个自然日桶（见 BucketToday 等常量）。
// now 由调用方传入，保证一次快照内所有条目用同一时刻判定（避免跨界抖动）。
func ExpireBucketOf(expireAt string, now time.Time) int {
	day, ok := expireDayOf(expireAt, now)
	if !ok {
		return BucketNone
	}
	y, m, d := now.In(expireLoc).Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, expireLoc)
	switch {
	case day.Equal(today):
		return BucketToday
	case day.Equal(today.AddDate(0, 0, 1)):
		return BucketTomorrow
	case !day.After(today.AddDate(0, 0, 6)):
		return Bucket7d
	}
	return BucketNone
}

// ExpireWithinDays 报告 expireAt 是否在 days 个自然日内到期。
//
// 「N 个自然日」= 从今天起算的 N 天，今天算第 1 天：
//   - days=1 → 只含今天（今晚 24:00 失效的都在内，见零点宽限）
//   - days=2 → 今天 + 明天
//   - days=3 → 今天 + 明天 + 后天
//
// 这是调度器「临期优先消耗」与设置项「临期阈值」的判据。与旧 ExpiringWithin
// （时长口径）的差别：旧实现用「到期锚点 < now + days×24h」，days=1 时会把
// 「明天 23:59 失效」也算进来（不足 48h），阈值随当天时刻漂移（早上 8 点设的
// 24h 窗口和晚上 8 点设的覆盖不同日期）。自然日口径回答的才是调度器真正要问的
// 问题：「今天该优先消耗谁」。
func ExpireWithinDays(expireAt string, now time.Time, days int) bool {
	day, ok := expireDayOf(expireAt, now)
	if !ok {
		return false
	}
	if days < 1 {
		days = 1
	}
	y, m, d := now.In(expireLoc).Date()
	limit := time.Date(y, m, d, 0, 0, 0, 0, expireLoc).AddDate(0, 0, days-1)
	return !day.After(limit)
}

// ExpiringDays 面板「临期额度」的**默认**展示窗口（自然日，1 = 今日到期）。
//
// 自 2026-10-20（R112）起**不是路由参数**：选号改用「最早到期时刻升序」
// （EarliestExpire），窗口大小只决定面板上那一列/汇总条怎么加总。
// 该值自 2026-11-04 起可由 `config.schedule.expiring_days` 覆盖（1/2/7，
// 纯展示，仍不参与选号，见 R112b）——运行时请读 App 的配置值，
// 本常量仅作默认值与单测基准。
const ExpiringDays = 1

// ExpiringWithinDays 统计 days 个自然日内到期的**可消耗**积分小计。
// 自 R112 起仅用于**展示**（面板三口径的「临期额度」/汇总条）；路由判据是
// EarliestExpire（最早到期时刻），不再看这个量。
// days 由 `config.schedule.expiring_days` 决定（1/2/7），与运行统计三桶同源：
// days=1 → 今日到期桶；days=7 → 三桶累加（= 运行统计「7 日内到期」列）。
func ExpiringWithinDays(items []ResourceItem, days int, now time.Time) int64 {
	var expiring int64
	for _, it := range items {
		if !it.Usable || it.InfoOnly || it.ExpireAt == "" {
			continue
		}
		if ExpireWithinDays(it.ExpireAt, now, days) {
			expiring += it.Remain
		}
	}
	return expiring
}

// EarliestExpire 返回「可消耗 + 有剩余 + 尚未失效」条目中**最早的到期时刻**。
// ok=false 表示该账号没有任何可用的到期信息（渠道不下发 expire_at，如 Qoder；
// 或所有包都已失效）——调用方应把它当作「到期未知」，排在已知到期时刻的账号之后。
//
// 这是选号的**唯一**路由判据（R112）：快过期的先烧，减少到期作废。
// 与 ExpiringWithinDays 的区别：那个是**体量**（窗口内到期积分之和），两个账号
// 体量相同时它无法分辨谁更急；这里是**紧迫度**（绝对时刻），天然可比。
func EarliestExpire(items []ResourceItem, now time.Time) (time.Time, bool) {
	var earliest time.Time
	ok := false
	for _, it := range items {
		if !it.Usable || it.InfoOnly || it.Remain <= 0 || it.ExpireAt == "" {
			continue
		}
		t, valid := ExpireInstant(it.ExpireAt)
		if !valid || !t.After(now) {
			continue // 已失效的包不参与「最快过期」判定
		}
		if !ok || t.Before(earliest) {
			earliest, ok = t, true
		}
	}
	return earliest, ok
}

// ExpiringWithin 统计 horizon 时长内到期的**可消耗**积分小计（临期额度）。
// 仅累计 Usable=true 且 ExpireAt 非空的条目——不可用池本工具消耗不到，
// 临期与否不影响路由决策，混入会虚高临期值。
// 锚点落在 deadline 之前即视为临期（日期粒度取当天零点，见 expireAnchor）：
// 实际口径为「到期日 ≤ 明天」——今天到期/明天到期都算临期，后天起不算。
// ExpireAt 为空串（上游未下发，如 Qoder）或不可解析时不计入，调用方无需特判。
func ExpiringWithin(items []ResourceItem, horizon time.Duration) int64 {
	deadline := time.Now().In(expireLoc).Add(horizon)
	var expiring int64
	for _, it := range items {
		if !it.Usable || it.InfoOnly || it.ExpireAt == "" {
			continue
		}
		if t, ok := expireAnchor(it.ExpireAt); ok && t.Before(deadline) {
			expiring += it.Remain
		}
	}
	return expiring
}
