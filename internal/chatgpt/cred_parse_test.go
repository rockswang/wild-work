package chatgpt

import (
	"strings"
	"testing"
)

// cred_parse_test.go 锁「面板粘贴」两段输入的解析口径（票 05）。
//
// 为什么要单测这一层：粘贴是**唯一**能让用户把凭据弄进来的入口，而它容忍的
// 是「浏览器 DevTools 里能复制的各种形态」。解析放宽一点就会把「只贴了一段」的
// 半截凭据当成合法输入放进去，产出的账号在校验时也许能过、每次对话都 403 ——
// 这正是本渠道最难排障的失败形态（spec 改动锚点）。所以边界既要有容忍，也要有拒绝。

func TestParseCookieHeaderAcceptsPastedForms(t *testing.T) {
	cases := []struct {
		name, raw string
		want      int
	}{
		{"裸值", "a=1; b=2", 2},
		{"带 Cookie: 前缀（复制整行请求头）", "Cookie: a=1; b=2", 2},
		{"前缀大小写不敏感", "cookie: a=1; b=2", 2},
		{"带引号", `"a=1; b=2"`, 2},
		{"多余空白与空段", "  a=1 ;;  b = 2 ; ", 2},
		{"值含等号（base64 尾）", "s=abc==; t=1", 2},
	}
	for _, c := range cases {
		got, err := ParseCookieHeader(c.raw)
		if err != nil {
			t.Errorf("%s: ParseCookieHeader(%q) err=%v", c.name, c.raw, err)
			continue
		}
		if len(got) != c.want {
			t.Errorf("%s: 解析出 %d 条（%v），期望 %d", c.name, len(got), got, c.want)
		}
	}
}

func TestParseCookieHeaderRejectsEmpty(t *testing.T) {
	for _, raw := range []string{"", "   ", "Cookie:", "nosuchpair"} {
		if got, err := ParseCookieHeader(raw); err == nil {
			t.Errorf("ParseCookieHeader(%q) 应报错（否则会落盘一个空 Cookie 账号），得到 %v", raw, got)
		}
	}
	// 错误文案必须点名「整条 Cookie 头」，否则用户不知道贴什么
	if _, err := ParseCookieHeader(""); err == nil || !strings.Contains(err.Error(), "Cookie") || !strings.Contains(err.Error(), "整条") {
		t.Errorf("错误文案要说明需要整条 Cookie 头，得到 %v", err)
	}
}

func TestParseAuthorizationHeaderAcceptsPastedForms(t *testing.T) {
	const tok = "eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjF9.sig"
	cases := []string{
		tok,
		"Bearer " + tok,
		"bearer " + tok,
		"Authorization: Bearer " + tok,
		"authorization:" + tok,
		`"` + tok + `"`,
		"  Bearer   " + tok + "  ",
	}
	for _, raw := range cases {
		got, err := ParseAuthorizationHeader(raw)
		if err != nil {
			t.Errorf("ParseAuthorizationHeader(%q) err=%v", raw, err)
			continue
		}
		if got != tok {
			t.Errorf("ParseAuthorizationHeader(%q) = %q，期望裸 token", raw, got)
		}
	}
}

func TestParseAuthorizationHeaderRejectsEmptyAndSpaces(t *testing.T) {
	for _, raw := range []string{"", "   ", "Bearer ", "Authorization:"} {
		if _, err := ParseAuthorizationHeader(raw); err == nil {
			t.Errorf("ParseAuthorizationHeader(%q) 应报错", raw)
		}
	}
	// token 中间混入空白（常见于复制整段 JSON 后带换行）必须拒绝：上游会把它当成坏 token，
	// 而我们的错误文案比上游 401 更能提示用户「只贴 Bearer 后面那串」。
	if _, err := ParseAuthorizationHeader("eyJabc eyJdef"); err == nil {
		t.Error("含空白的 token 应报错")
	}
}

func TestNewDeviceIDShape(t *testing.T) {
	id := NewDeviceID()
	if len(id) != 36 {
		t.Fatalf("设备号应为 uuid4 形态（36 字符），得到 %q", id)
	}
	if id == NewDeviceID() {
		t.Error("两次生成必须不同（复用设备号会把两边的风控记录串在一起）")
	}
}
