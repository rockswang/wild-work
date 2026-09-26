// Package glm 封装智谱清言（chatglm.cn）上游协议，实现 provider.Upstream 接口。
//
// 协议要点（2026-09-26 实测，见 docs/智谱清言渠道接入备忘.md）：
//   - 网页版私有接口，**非**开放平台 API（open.bigmodel.cn）。凭据是浏览器 Cookie 里的
//     chatglm_refresh_token，用 Authorization: Bearer <refresh_token> 换 access_token。
//   - 所有私有接口都要签名头 X-Sign = md5(timestamp-nonce-SIGN_SECRET)，见 sign.go。
//   - 对话走 /chatglm/backend-api/assistant/stream（SSE）。**帧语义取决于 part.status**：
//     `init` 帧是**增量片段**（delta），`finish` 帧是该段落**完整全文**。
//     即 init 帧拼接 == finish 帧全文（见 sse.go 与 R25；曾误按「全量快照」实现而丢字）。
//   - 模型选择 = assistant_id（24 位 hex，即官方「智能体」ID）+ chat_mode
//     （""=普通 / "zero"=推理 / "deep_research"=沉思）。清言无公开模型列表接口，
//     故模型表为静态表（见 staticAssistants）。
//   - 签到在独立的 activity-api 族：/chatglm/activity-api/activity/daily/*。
//     实测与对话联动（界面文案「对话后记得回来领取打卡进度」），
//     故保活与签到串成「先对话后签到」一个流程（见 checkin.go）。
//
// 单账号渠道：一个 refresh_token 一个号，且不可人工恢复，故 Runtime 置 SingleAccount
// （任何账号级惩罚都等于整条渠道下线，见 server.Runtime.SingleAccount）。
package glm

import (
	"strings"
	"time"

	"wild-work/internal/provider"
)

// Kind 渠道标识，同时是模型名前缀（glm/xxx）。
const Kind = provider.GLM

// 上游地址。清言网页版全部接口都在 chatglm.cn 同域下，按 API 族分前缀。
const (
	// Base 上游站点根。
	Base = "https://chatglm.cn"
	// Origin chatglm.cn 对私有接口校验 Origin/Referer。
	Origin = "https://chatglm.cn"

	// EpRefresh 用 refresh_token 换 access_token（同时轮换 refresh_token）。
	EpRefresh = "/chatglm/user-api/user/refresh"
	// EpUserInfo 用户信息（昵称/手机/邮箱/是否访客）。
	EpUserInfo = "/chatglm/user-api/user/info"
	// EpMemberInfo 会员与积分信息（left_score = 积分余额）。
	//
	// 这是清言**唯一**能拿到积分余额的接口（2026-09-26 实测）。
	// 返回值要点：
	//   left_score      积分余额（单位「分」，1 积分 = 100 分；App 里显示的 3000 积分 = 300000 分）
	//   true_left_score 实测恒为 0（疑似未启用字段，不作展示依据）
	//   left_token      剩余 token 额度
	//   score_rule      积分规则文案，实测「免费用户，登录赠送200积分/天」
	//
	// 结论：清言积分是**登录即送、服务端被动发放**，没有「领取」接口。
	// 每日任务（与伙伴对话等）发一条消息即算完成——保活对话正好满足。
	EpMemberInfo = "/chatglm/member-api/member/member_info"
	// EpChat 对话（SSE）。
	EpChat = "/chatglm/backend-api/assistant/stream"
	// EpDeleteConv 删除会话（避免在用户会话列表里留痕）。
	EpDeleteConv = "/chatglm/backend-api/assistant/conversation/delete"

	// EpCheckinInfo 签到状态（打卡天数 / 今日是否已签 / 抽奖次数）。
	EpCheckinInfo = "/chatglm/activity-api/activity/daily/check_in_info"
	// EpCheckin 执行签到。
	EpCheckin = "/chatglm/activity-api/activity/daily/check_in"
	// EpPrizeList 奖品列表。
	EpPrizeList = "/chatglm/activity-api/activity/daily/prize_list"
	// EpDrawPrize 抽奖。
	EpDrawPrize = "/chatglm/activity-api/activity/daily/draw_prize"

	// EpDailyLoginScore 每日登录积分领取（**在线、真实**的每日积分来源）。
	//
	// 2026-09-26 从 Web 版主包挖出：网页版页面加载时自己就会调它
	// （带 errorMessageShow:false，静默失败，说明重复调用不骚扰用户）。
	//
	// 响应语义：status=0 领取成功；status=10001 "今日已领取"（幂等，非错误）。
	//
	// ⚠️ 与上面 activity-api 的旧签到区分：那套已下线（返回「活动已结束」），
	// **这一套是在线的**，是「登录赠送200积分/天」的实际载体。
	EpDailyLoginScore = "/chatglm/member-api/member/daily_login_score"
)

// SIGN_SECRET 是清言客户端内硬编码的签名密钥。
// 2026-09-26 实测仍有效：带签名 → 401 unauthorized user（认证层拒绝）；
// 不带签名 → 400 bad request（签名层拒绝）。两者响应不同即证明签名通过。
const SIGN_SECRET = "8a1317a7468aa3ad86e997d08f3f31cb"

// userAgent 伪装 Edge 143（与清言网页版实测一致）。
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0"

// DefaultAssistantID 清言主对话（ChatGLM）的 assistant_id。
// 来源：网页版主包常量，与 GLM-Free-API / Chat2API 的 DEFAULT_ASSISTANT_ID 一致。
const DefaultAssistantID = "65940acff94777010aa6b796"

// 超时。
const (
	// requestTimeout 单次对话请求超时上限（沉思/推理模型可能跑很久）。
	requestTimeout = 10 * time.Minute
	// jsonTimeout 轻量 JSON 接口（刷新/信息/签到）超时。
	jsonTimeout = 30 * time.Second
)

// utc8 清言所有日期口径都是 UTC+8 墙钟（event_date、签到日界）。
var utc8 = time.FixedZone("UTC+8", 8*60*60)

// staticAssistants 静态模型表：客户端模型名 → assistant_id。
//
// 清言**没有公开的模型列表接口**（官方 App 是客户端硬编码的），故只能静态表。
// 这里的 ID 全部从网页版主包常量表里挖出，非猜测。
//
// staticAssistants 客户端模型名 → assistant_id（仅收录**实测可用**的）。
//
// 2026-09-26 对全部候选逐个实测（真实账号 + 纯文本输入），结果：
//
//	✅ chatglm   主对话        model=moe_53f
//	✅ video     视频助手      model=all-tools-glms-glms-v2
//	✅ search    AI搜索        model=ai-search
//	✅ ppt       清言PPT       model=all-tools-glms-glms-v2
//	❌ aidraw    AI画图        需 cogview 参数（aspect_ratio/style/scene），纯文本触发不了出图
//	❌ doc       AI阅读        需 file_list/img_list（上传文件），纯文本无内容可读
//	❌ study     学习搭子       上游返回 error_code 10025 internal server error（**该智能体已失效**）
//
// 故**只收录可用的 4 个**。缺失的三个不是"忘了加"，而是加了只会让用户选中后拿到空回答。
// 若将来需要画图/读文档，应单独实现（补 cogview / 文件上传参数），而不是塞进对话路径。
//
// # 上游模型代号后缀（`:<model>`）
//
// 清言**客户端无法选择模型版本**（实测：三种 chat_mode 服务端都上报同一个 `moe_53f`），
// 故把**服务端实际使用的模型代号**写进模型名，让用户一眼看出对应关系：
//
//	chatglm:moe_53f              主对话（普通）
//	chatglm-think:moe_53f        推理模式（chat_mode=zero）
//	chatglm-deepresearch:moe_53f 沉思模式（chat_mode=deep_research）
//
// `moe_53f` 是服务端内部代号（`moe` = MoE 架构，`53` 很可能指 5.3，`f` = 变体）。
// 三者是**同一个底层模型的不同推理等级**，不是三个不同版本的模型。
//
// ⚠️ **旧名保留为别名**（`chatglm` / `chatglm-think` / `chatglm-deepresearch`）：
// 已配置旧名的客户端不会因改名而断掉。`/v1/models` 只列出带后缀的新名。
//
// 另外：任何 24 位 hex 的模型名都会被直接当作 assistant_id 透传
// （清言原生支持该语义，等于免费获得「接任意智能体」的能力，见 resolveAssistant）——
// 想用未收录的智能体，直接填它的 ID 即可。
var staticAssistants = map[string]string{
	// ---- 带上游模型代号的新名（面板/客户端可见）----
	"chatglm:moe_53f":              DefaultAssistantID, // 主对话（普通）
	"chatglm-think:moe_53f":        DefaultAssistantID, // 推理模式
	"chatglm-deepresearch:moe_53f": DefaultAssistantID, // 沉思模式

	// ---- 旧名别名（向后兼容，不再出现在 /v1/models）----
	"chatglm": DefaultAssistantID,
	"glm":     DefaultAssistantID,
	"default": DefaultAssistantID,

	// ---- 其它智能体（服务端 model 见上表）----
	"video":  "668d03b2e99d661ed3c32516", // 视频助手
	"search": "659e54b1b8006379b4b2abd6", // AI搜索
	"ppt":    "670e3c3e119b48fe5a851149", // 清言PPT
}

// UpstreamModel 返回某客户端模型名对应的**上游服务端模型代号**（仅文档/展示用途）。
// 未知返回空串。这些值是 2026-09-26 实测得到的。
func UpstreamModel(clientModel string) string {
	m := strings.ToLower(strings.TrimSpace(clientModel))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if i := strings.LastIndex(m, ":"); i >= 0 {
		return m[i+1:]
	}
	// 无后缀的旧名：按已知映射回填
	switch {
	case m == "chatglm" || m == "glm" || m == "default",
		m == "chatglm-think" || m == "chatglm-deepresearch":
		return "moe_53f"
	case m == "search":
		return "ai-search"
	case m == "ppt" || m == "video":
		return "all-tools-glms-glms-v2"
	}
	return ""
}

// staticModelNames 面板展示用的静态模型清单（顺序即展示顺序）。
//
// 只列实测可用的；失效/需额外参数的智能体不列（见 staticAssistants 的实测结论）。
// 三个 chatglm 变体带 `:moe_53f` 后缀，标明服务端实际使用的模型代号。
var staticModelNames = []string{
	"glm/chatglm:moe_53f",
	"glm/chatglm-think:moe_53f",
	"glm/chatglm-deepresearch:moe_53f",
	"glm/search",
	"glm/ppt",
	"glm/video",
}

// StaticModels 上游不可达时的静态模型清单。
// 清言无模型列表接口，故这**就是**权威清单（不是兜底）。
func StaticModels() []provider.ModelInfo {
	out := make([]provider.ModelInfo, 0, len(staticModelNames))
	for _, name := range staticModelNames {
		// 模型名去掉 channel 前缀后即客户端可见的裸名。
		bare := name
		if i := len("glm/"); len(name) > i && name[:i] == "glm/" {
			bare = name[i:]
		}
		// ID 即裸名（含 `:moe_53f` 后缀），客户端用它请求；
		// Name 同样带后缀，让面板/客户端一眼看出上游模型代号。
		info := provider.ModelInfo{ID: bare, Name: bare}
		// 能力标注：推理/沉思支持思考；清言主对话支持联网检索但不支持图像输入
		// （图像走 AI画图/图像识别等独立智能体，主对话的 content 只收文本）。
		switch {
		case strings.HasPrefix(bare, "chatglm-think"), strings.HasPrefix(bare, "chatglm-deepresearch"):
			info.SupportsReasoning = true
		}
		out = append(out, info)
	}
	return out
}

// knownAssistantIDs 已知的 assistant_id 反查表：ID → 友好名。
// 用于面板把「用户直填的智能体 ID」显示成可读名字。
var knownAssistantIDs = map[string]string{
	DefaultAssistantID:         "ChatGLM",
	"65a232c082ff90a2ad2f15e2": "AI画图",
	"658a7988b8a9a98d38725745": "AI阅读",
	"668d03b2e99d661ed3c32516": "视频助手",
	"659e54b1b8006379b4b2abd6": "AI搜索",
	"670e3c3e119b48fe5a851149": "清言PPT",
	"68f0b8c110eea3e78b0e0e5e": "学习搭子",
	"676411c38945bbc58a905d31": "智能体",
	"67ac6fcb143f8d2fd00d3ce9": "智能体",
	"67e13a4b1673c8ab92ba1bfc": "智能体",
	"67cabed6fce0ae01cd721733": "智能体",
}
