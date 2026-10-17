package app

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// 本测试锁死 OfficeAce「新增账号」**前后端状态契约**（同 R34/R41 家族的「声明了却没接线」）。
//
// 背景：这条链路跨三个文件，任一处写错都**不报错**，只表现为「点了没反应 / 永远转圈 / 永远失败」：
//
//	cmd/wild-work/web/app.js   POST /api/login/officeace_auto          → 拿 auth_url
//	                           GET  /api/login/officeace_auto_status   → 读 status/nickname/error
//	internal/app/app.go        officeAceLoginStatus（字段名的唯一来源）
//	internal/officeace/login.go LoginSession.status 取值（pending…canceled）
//
// 所以用静态断言把「前端读的字段名」与「后端写的字段名」焊在一起。

// 前端轮询真正用到的字段（app.js startOfficeAcePoll）。
var oaFrontendStatusKeys = []string{"status", "nickname", "error"}

// 前端会当成终态并停止轮询的值（其余值一律视为「进行中」）。
var oaFrontendTerminalStates = []string{"done", "failed", "expired", "canceled", "idle"}

func readAppJS(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../wild-work/web/app.js")
	if err == nil {
		return string(raw)
	}
	// 包目录相对路径在 CI 的 cwd 下可能不同，直接按仓库布局再试一次。
	raw, err = os.ReadFile("../../cmd/wild-work/web/app.js")
	if err != nil {
		t.Fatalf("读前端 app.js 失败（签名变化会让本测试静默失效）：%v", err)
	}
	return string(raw)
}

func TestOfficeAceStatusKeysMatchFrontend(t *testing.T) {
	// ① 后端结构体的 JSON 标签必须覆盖前端读取的每一个字段。
	raw, err := json.Marshal(officeAceLoginStatus{
		Status: "done", Error: "e", UID: "u", Nickname: "n", AuthURL: "a", Warning: "w",
	})
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	for _, k := range oaFrontendStatusKeys {
		if _, ok := back[k]; !ok {
			t.Errorf("后端 status 响应缺字段 %q —— 前端读 st.%s 会恒为 undefined（表现为一直转圈/失败无原因）", k, k)
		}
	}

	// ② 前端源码里必须真的在读这些字段（防止前端被改成 st.state 之类而后端没跟上）。
	js := readAppJS(t)
	for _, k := range oaFrontendStatusKeys {
		if !strings.Contains(js, "st."+k) {
			t.Errorf("app.js 里找不到 st.%s —— 前端轮询契约已变化，请同步 officeAceLoginStatus", k)
		}
	}
	if !strings.Contains(js, "/api/login/officeace_auto_status") {
		t.Error("app.js 未轮询 /api/login/officeace_auto_status")
	}
	if !strings.Contains(js, "/api/login/officeace_auto\"") {
		t.Error("app.js 未调用 POST /api/login/officeace_auto")
	}
}

func TestOfficeAceTerminalStatesMatchFrontend(t *testing.T) {
	js := readAppJS(t)
	for _, st := range oaFrontendTerminalStates {
		if !strings.Contains(js, `st.status === "`+st+`"`) {
			t.Errorf("app.js 不认识终态 %q，后端进入该态后前端会一直轮询（或永不停止）", st)
		}
	}
	// 前端的终态分支必须带上后端 error 文案（否则用户只看到兜底文案，
	// 上游拒绝 DPoP/未开通服务等原因全部丢失）。
	js = readAppJS(t)
	if !strings.Contains(js, "st.error") {
		t.Error("app.js 未读取 st.error —— 失败路径会退化成无原因的「OfficeAce 登录失败」")
	}
}

// TestOfficeAceStatusStructIsSingleSource 锁死「状态字段名只有一处定义」：
// 若有人改回 map[string]any，本测试会在编译期就失败（officeAceLoginStatus 不存在）。
func TestOfficeAceStatusStructIsSingleSource(t *testing.T) {
	var st officeAceLoginStatus
	st.Status = "pending"
	if !oaLoginActive(st.Status) {
		t.Fatal("pending 必须被视为进行中")
	}
	for _, s := range []string{"done", "failed", "expired", "canceled", "idle"} {
		if oaLoginActive(s) {
			t.Errorf("%q 不应被视为进行中（否则再点新增会被 409/开新流程顶掉）", s)
		}
	}
}

// TestOfficeAcePollingIntervalSane 轮询间隔必须 ≥ 2s：state 端点由云签发，过密轮询
// 既是无谓请求也有风控暴露风险（AGENTS.md §0.1）。
func TestOfficeAcePollingIntervalSane(t *testing.T) {
	raw, err := os.ReadFile("../officeace/login.go")
	if err != nil {
		t.Fatalf("读 officeace/login.go 失败: %v", err)
	}
	m := regexp.MustCompile(`pollInterval\s*=\s*([0-9]+)\s*\*\s*time\.Millisecond`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("找不到 pollInterval 常量定义（本测试需要同步更新）")
	}
	if m[1] < "2000" {
		t.Fatalf("pollInterval=%sms 过密，至少 2s", m[1])
	}
}
