// Package provider 定义不同上游（workbuddy / traework）共用的最小接口。
// 只抽取 server/scheduler 必需能力，避免为未来平台过度设计。
package provider

import (
	"fmt"
	"io"
	"net/http"
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
)

func (k Kind) String() string { return string(k) }

// BuiltinKinds 本仓内建渠道集合（R66）。
//
// 用于拒绝自定义渠道占用内建渠道名：两者同名时两层会指向不同 Runtime
// （server 侧 getRuntime 内建优先，而 app 侧 UpdateCustomRuntimes 无条件覆盖），
// 表现为「面板/账号操作打到自定义池、对话请求走内建渠道」——不报错但静默出错。
// 同时排除 ModelNamespaces 里的上游命名空间（如 cline-free），以免路由歧义。
func BuiltinKinds() map[string]bool {
	m := map[string]bool{}
	for _, k := range []Kind{WorkBuddy, WorkBuddyAI, TraeWork, Qoder, QoderCN, QoderCOM,
		QwenWork, MonkeyCode, Raccoon, Loomy, TraeCode, Oczen, GLM, Cline} {
		m[k.String()] = true
	}
	for ns := range ModelNamespaces {
		m[ns] = true
	}
	return m
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
	default:
		return "none"
	}
}

// Rotatable 报告该错误是否应换下一个账号重试本请求（账号级错误）。
// 账号有问题的（限流/欠费/登录态死/偶发 404/上游 5xx/账号故障）：换号有价值；
// 请求内容有问题的（内容拦截/上下文超限/模型不存在）：换号必然复现，透传原文结束；
// ErrClient（其他 4xx 业务错误）默认可换一次——部分渠道业务错误挂在账号权益上。
func (k ErrKind) Rotatable() bool {
	switch k {
	case ErrSoftRate, ErrHardCredit, ErrSessionDead, ErrNotFound, ErrServer, ErrAccountFault, ErrClient:
		return true
	default:
		return false
	}
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
	// 消费方约定：has_context = ContextFromAPI || HandMeasured。
	HandMeasured bool
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

	// ExpireAt 该条目到期时刻（RFC3339，UTC+8 墙钟）。空串表示上游未下发到期时间，
	// 前端据此隐藏「有效期」列——不得用零值时间冒充「永不过期」。
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

// ExpiringWithin 统计 horizon 时长内到期的**可消耗**积分小计（临期额度）。
// 仅累计 Usable=true 且 ExpireAt 非空的条目——不可用池本工具消耗不到，
// 临期与否不影响路由决策，混入会虚高临期值。
// ExpireAt 是 YYYY-MM-DD 日期粒度（UTC+8 墙钟），精确到小时的 24h 判定无意义，
// 实际口径为「到期日 ≤ 明天」：今天到期/明天到期都算临期，后天起不算。
// ExpireAt 为空串（上游未下发，如 Qoder）或不可解析时不计入，调用方无需特判。
func ExpiringWithin(items []ResourceItem, horizon time.Duration) int64 {
	deadline := time.Now().In(expireLoc).Add(horizon)
	var expiring int64
	for _, it := range items {
		if !it.Usable || it.InfoOnly || it.ExpireAt == "" {
			continue
		}
		// 日期解析到当天零点（UTC+8），零点落在 deadline 之前即视为临期
		if t, err := time.ParseInLocation("2006-01-02", it.ExpireAt, expireLoc); err == nil && t.Before(deadline) {
			expiring += it.Remain
		}
	}
	return expiring
}
