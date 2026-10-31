// raccoon 出站请求头：对齐官方客户端**发 LLM 请求的那一层**（R148，取代原先按渲染层的实现）。
//
// 取证来源（2026-10-31 反编译）：商汤小浣熊-1.0.38-arm64.deb →
//   resources/box-agent-runtime/bin/box-agent-acp
//   （PyInstaller 打包的 Python 进程；manifest.json 声明 box-agent 0.9.8，内嵌 CPython 3.12.13。
//     必须用 py3.12 解 marshal —— 3.13 会报 "bad marshal data"）
//   ├─ box_agent/llm/openai_client.py  ← 构造 AsyncOpenAI(api_key, base_url, timeout)，对话从这里发出
//   ├─ box_agent/llm/base.py           ← _auth_headers / _agent_headers / _request_headers
//   ├─ box_agent/client_info.py        ← ClientInfo.headers_for_url（x-client-* 的唯一来源）
//   └─ openai/_base_client.py          ← SDK 的 user_agent / platform_headers（X-Stainless-*）
//
// ⚠️ 此前那套为什么错（别再按旧结论实现）：旧实现只解了 app.asar 里
// build/desktop-renderer/chunks/75741-*.js 的 createHeaders()，据「UA 里没有 Raccoon-Platform= 标记」
// 断定「官方永不发 X-Client-*」。但**对话请求根本不走渲染层**，走的是上面那个 Python 子进程：
//   · 官方那组头叫 x-client-{name,platform,version,os-version,channel,device-id}，与渲染层的
//     X-Client-{Platform,Version,Device-ID} 不是同一批（多出 os-version/channel、name 恒 raccoon-ai）；
//   · 触发条件只按 host（xiaohuanxiong.com 及子域）+ scheme，与 UA 内容毫无关系 ⇒ 恒发；
//   · UA 是 openai SDK 的 `AsyncOpenAI/Python 2.8.0`，不是 Electron 形态。
//
// 照抄官方的判定规则（不自作主张）：
//   · headers_for_url 对每个值都过 _clean_header_value（strip、必须全 ASCII、长度 ≤256），空值跳过该头；
//   · version 先 re.fullmatch(`v?x.y.z`)，命中才补 `v` 前缀，不命中置空（⇒ 不发送）；
//   · x-client-name 空则回落 `raccoon`、x-client-platform 空则回落 `unknown`（只有这两个有兜底）；
//   · device_id 官方**不给它赋值**（宿主 buildBoxAgentClientInfo 只传 5 个键，无 device_id/deviceId）
//     ⇒ from_meta 读到空串 ⇒ 被 _clean_header_value 过滤 ⇒ 官方实际不发该头，本渠道也不发。
//     注意别误读成「官方没有这个头」：headers_for_url 确实构造了它（BUILD_TUPLE 6），只是值恒空。
//   · LLM 路径**不发** X-Raccoon-Language（box_agent 内 grep 零命中；那是渲染层专有）；
//   · X-Org-Code 在 LLM 路径**有值才发**（read_auth_org_code 非空才写），渲染层才是无条件设置。
package raccoon

import (
	"net/http"
	"strings"
)

const (
	// OfficialUA 官方 LLM 请求的 User-Agent（openai SDK 的 BaseClient.user_agent 原文
	// f"{__class__.__name__}/Python {_version}" ⇒ AsyncOpenAI + 2.8.0，见 openai/_version.py）。
	OfficialUA = "AsyncOpenAI/Python 2.8.0"

	// sdkVersion / pyVersion 与官方包同源：openai 2.8.0、内嵌 CPython 3.12.13。
	sdkVersion = "2.8.0"
	pyVersion  = "3.12.13"

	// clientName 取官方 package.json 的 name（raccoon-ai）；官方为空时回落 `raccoon`。
	clientName = "raccoon-ai"
	// clientPlatform 官方 normalizeDesktopPlatform(platform, arch) 的输出形态
	// `desktop-<macos|windows|linux>-<x86|arm|arm64|unknown>`；口径与 UA 的 aarch64 一致。
	clientPlatform = "desktop-linux-arm64"
	// clientVersion 官方 app.getVersion() 经 normalizeClientVersion 后**带 v 前缀**。
	clientVersion = "v1.0.38"
	// clientChannel 官方常量 BOX_AGENT_CLIENT_CHANNEL = 'official'。
	clientChannel = "official"
)

// ApplyHeaders 把官方 LLM 请求的请求头一次设好。
//
// orgCode：企业版才有；个人账号传空串。**只在非空时发送**（对齐 LLM 路径
// `if org_code: headers['X-Org-Code'] = org_code`；渲染层才是无条件设置）。
//
// osVersion：网关发行版串。官方在 Linux 上取 os.release() 的内核串，本渠道刻意不逐字对齐，
// 理由见本决议「后果」已知限制 2。
//
// sessID / turnID：官方 `_agent_headers` 的关联头，非空才发。**不伪造 Title / Call-Kind**：
// 官方由宿主按会话标题与调用类型填，网关无对应语义，硬填会把主对话误标成内部辅助调用。
func ApplyHeaders(req *http.Request, accessToken, orgCode, osVersion, sessID, turnID string) {
	req.Header.Set("User-Agent", OfficialUA)
	req.Header.Set("Content-Type", "application/json")
	// SDK 的 platform_headers（openai/_base_client.py 里那 6 个固定头）。
	req.Header.Set("X-Stainless-Lang", "python")
	req.Header.Set("X-Stainless-Package-Version", sdkVersion)
	req.Header.Set("X-Stainless-OS", "Linux")
	req.Header.Set("X-Stainless-Arch", "arm64")
	req.Header.Set("X-Stainless-Runtime", "CPython")
	req.Header.Set("X-Stainless-Runtime-Version", pyVersion)
	if accessToken != "" {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	if v := cleanHeaderValue(orgCode); v != "" {
		req.Header.Set("X-Org-Code", v)
	}
	// x-client-*：官方 ClientInfo.headers_for_url，逐项 cleanHeaderValue 后非空才写。
	// device_id 官方从不赋值 ⇒ 官方这个头恒空、恒不发出 ⇒ 本渠道同「不发」。
	// （官方 headers_for_url 里确实有这个头，别读成「官方没有」；见文件头说明。）
	for _, h := range []struct{ name, value string }{
		{"X-Client-Name", clientName},
		{"X-Client-Platform", clientPlatform},
		{"X-Client-Version", clientVersion},
		{"X-Client-OS-Version", osVersion},
		{"X-Client-Channel", clientChannel},
	} {
		if v := cleanHeaderValue(h.value); v != "" {
			req.Header.Set(h.name, v)
		}
	}
	// X-RACCOON-*：官方 _agent_headers，非空才发。
	if v := cleanHeaderValue(sessID); v != "" {
		req.Header.Set("X-RACCOON-Session-ID", v)
	}
	if v := cleanHeaderValue(turnID); v != "" {
		req.Header.Set("X-RACCOON-Turn-ID", v)
	}
}

// cleanHeaderValue 对齐官方 _clean_header_value（_MAX_HEADER_VALUE_LENGTH = 256）：
// strip 后必须全 ASCII 且长度 ≤256，否则视为空、不发送。
func cleanHeaderValue(value string) string {
	s := strings.TrimSpace(value)
	if s == "" || len(s) > 256 {
		return ""
	}
	for _, r := range s {
		if r > 127 {
			return ""
		}
	}
	return s
}
