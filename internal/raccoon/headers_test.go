package raccoon

// raccoon 出站头对齐测试（R148，取代原先按渲染层的实现）。
//
// 取证：商汤小浣熊 1.0.38 arm64 官方客户端 → resources/box-agent-runtime/bin/box-agent-acp
// （PyInstaller 打包的 Python 进程，box-agent 0.9.8，内嵌 CPython 3.12.13）。
// **对话请求由它发出，不经过 Electron 渲染层**，因此头形态以它为准：
//   box_agent/llm/openai_client.py  → AsyncOpenAI(api_key, base_url, timeout)
//   openai/_base_client.py          → user_agent = f"{cls}/Python {version}"、platform_headers 6 个
//   box_agent/llm/base.py           → _auth_headers（X-Org-Code 有值才发）、_agent_headers（X-RACCOON-*）
//   box_agent/client_info.py        → ClientInfo.headers_for_url（x-client-{name,platform,version,os-version,channel,device-id}）
//
// 判据层级：**对齐官方形态**，不是「绕过拦截」。本轮零上游请求（AGENTS.md §0.1）；
// 「补头即解限」属推断，未实测（见本决议「后果」）。

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// TestUserAgentMatchesOfficial UA 必须是 openai SDK 形态，不是 Electron 渲染层形态。
//
// openai SDK 的 BaseClient.user_agent 原文 f"{self.__class__.__name__}/Python {self._version}"
// ⇒ `AsyncOpenAI/Python 2.8.0`（openai/_version.py 的 __version__ = '2.8.0'）。
func TestUserAgentMatchesOfficial(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://xiaohuanxiong.com/x", nil)
	ApplyHeaders(req, "tok", "", "13", "sess-abc", "turn-abc")

	if got, want := req.Header.Get("User-Agent"), OfficialUA; got != want {
		t.Fatalf("User-Agent = %q, want %q", got, want)
	}
	// 反向断言：旧那套 Electron 形态不得再出现在 LLM 端点。
	for _, forbidden := range []string{"Mozilla/5.0", "Electron/", "Chrome/", "raccoon-ai/1.0.38"} {
		if strings.Contains(req.Header.Get("User-Agent"), forbidden) {
			t.Errorf("UA 不该再含 %q（那是渲染层形态，LLM 端点用的是 Python SDK）", forbidden)
		}
	}
}

// TestStainlessHeadersPresent SDK 的 6 个 X-Stainless-* 必须齐备（openai/_base_client.py 的
// platform_headers 常量组，键名取自 (('X-Stainless-Lang','X-Stainless-Package-Version',
// 'X-Stainless-OS','X-Stainless-Arch','X-Stainless-Runtime','X-Stainless-Runtime-Version'))）。
func TestStainlessHeadersPresent(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://xiaohuanxiong.com/x", nil)
	ApplyHeaders(req, "tok", "", "13", "sess-abc", "turn-abc")

	want := map[string]string{
		"X-Stainless-Lang":            "python",
		"X-Stainless-Package-Version": sdkVersion,
		"X-Stainless-OS":              "Linux",
		"X-Stainless-Arch":            "arm64",
		"X-Stainless-Runtime":         "CPython",
		"X-Stainless-Runtime-Version": pyVersion,
	}
	for h, v := range want {
		if got := req.Header.Get(h); got != v {
			t.Errorf("%s = %q, want %q", h, got, v)
		}
	}
}

// TestClientHeadersPresent x-client-* 必须发 5 个，且 x-client-device-id **必须不发**。
//
// 旧测试曾断言「官方永不发 X-Client-*」，该结论只覆盖渲染层、已被证伪（R148）。
// 官方的 headers_for_url 里**确实构造了 x-client-device-id**（ClientInfo 有 device_id 字段），
// 但宿主 buildBoxAgentClientInfo 只传 5 个键、从不赋值 ⇒ 值恒空 ⇒ _clean_header_value 过滤掉 ⇒ 不发出。
// 所以「不发」不是因为没有这个头，而是因为值永远为空；补一个等于伪造「已绑定设备」。
func TestClientHeadersPresent(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://xiaohuanxiong.com/x", nil)
	ApplyHeaders(req, "tok", "", "13", "sess-abc", "turn-abc")

	want := map[string]string{
		"X-Client-Name":       clientName,
		"X-Client-Platform":   clientPlatform,
		"X-Client-Version":    clientVersion,
		"X-Client-OS-Version": "13",
		"X-Client-Channel":    clientChannel,
	}
	for h, v := range want {
		if got := req.Header.Get(h); got != v {
			t.Errorf("%s = %q, want %q（官方 LLM 路径必发）", h, got, v)
		}
	}
	if got := req.Header.Get("X-Client-Device-ID"); got != "" {
		t.Errorf("X-Client-Device-ID = %q，不得发送：官方该头值恒空（宿主从不赋值 device_id）", got)
	}
}

// TestClientVersionFormat 官方对 version 做 re.fullmatch(`v?x.y.z`)，命中才补 `v` 前缀。
// 这里锁死常量形态，防止后人改成不带 v 或带四位段。
func TestClientVersionFormat(t *testing.T) {
	if !regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`).MatchString(clientVersion) {
		t.Errorf("clientVersion = %q 不满足官方 normalizeClientVersion 的 `v x.y.z` 形态", clientVersion)
	}
}

// TestCleanHeaderValue 对齐官方 _clean_header_value：strip、全 ASCII、长度 ≤256，否则视为空。
func TestCleanHeaderValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{"  org-1  ", "org-1"},
		{"", ""},
		{"   ", ""},
		{"组织码", ""},          // 非 ASCII ⇒ 官方返回空
		{"中文 title", ""},       // 同上（官方对非 ASCII 的 title 是 encode 成 bytes，这里是 head 值，一律拒）
		{strings.Repeat("a", 256), strings.Repeat("a", 256)},
		{strings.Repeat("a", 257), ""},
	}
	for _, c := range cases {
		if got := cleanHeaderValue(c.in); got != c.want {
			t.Errorf("cleanHeaderValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestOrgCodeOnlyWhenPresent LLM 路径的判据是 `if org_code: headers['X-Org-Code'] = org_code`
// （渲染层才是无条件设置）。空值必须**不出现**该 key。
func TestOrgCodeOnlyWhenPresent(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://xiaohuanxiong.com/x", nil)
	ApplyHeaders(req, "tok", "   ", "13", "sess-abc", "turn-abc")
	if _, ok := req.Header["X-Org-Code"]; ok {
		t.Errorf("orgCode 为空白时不应发送 X-Org-Code（LLM 路径有值才发）")
	}

	req2, _ := http.NewRequest(http.MethodPost, "https://xiaohuanxiong.com/x", nil)
	ApplyHeaders(req2, "tok", "org-abc", "13", "sess-abc", "turn-abc")
	if got := req2.Header.Get("X-Org-Code"); got != "org-abc" {
		t.Errorf("X-Org-Code = %q, want org-abc", got)
	}
}

// TestNoRaccoonLanguage LLM 路径不发 X-Raccoon-Language（box_agent 内 grep 零命中，
// 那是渲染层 createHeaders() 专有的头）。
func TestNoRaccoonLanguage(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://xiaohuanxiong.com/x", nil)
	ApplyHeaders(req, "tok", "", "13", "sess-abc", "turn-abc")
	if _, ok := req.Header["X-Raccoon-Language"]; ok {
		t.Error("不应发送 X-Raccoon-Language：官方 LLM 路径没有该头")
	}
}

// TestAgentHeadersPresent 官方的 _agent_headers 四个头里，网关只发 Session-ID / Turn-ID。
// Title / Call-Kind 由宿主按会话标题与调用类型填，网关无对应语义，硬填会把主对话误标成辅助调用。
func TestAgentHeadersPresent(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://xiaohuanxiong.com/x", nil)
	ApplyHeaders(req, "tok", "", "13", "sess-abc", "turn-abc")

	if got := req.Header.Get("X-RACCOON-Session-ID"); got != "sess-abc" {
		t.Errorf("X-RACCOON-Session-ID = %q", got)
	}
	if got := req.Header.Get("X-RACCOON-Turn-ID"); got != "turn-abc" {
		t.Errorf("X-RACCOON-Turn-ID = %q", got)
	}
	for _, h := range []string{"X-RACCOON-Title", "X-RACCOON-Call-Kind"} {
		if _, ok := req.Header[h]; ok {
			t.Errorf("不应发送 %s（官方由宿主填，网关无对应语义）", h)
		}
	}
}

// TestAgentHeadersSkippedWhenEmpty 官方 _agent_headers 对空值跳过该头。
func TestAgentHeadersSkippedWhenEmpty(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://xiaohuanxiong.com/x", nil)
	ApplyHeaders(req, "tok", "", "13", "", "")
	for _, h := range []string{"X-RACCOON-Session-ID", "X-RACCOON-Turn-ID"} {
		if _, ok := req.Header[h]; ok {
			t.Errorf("空值时不应发送 %s", h)
		}
	}
}

// TestAuthorizationRequired 有 token 才发 Authorization。
func TestAuthorizationRequired(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://xiaohuanxiong.com/x", nil)
	ApplyHeaders(req, "tok-abc", "", "13", "s", "t")
	if got := req.Header.Get("Authorization"); got != "Bearer tok-abc" {
		t.Errorf("Authorization = %q", got)
	}
}
