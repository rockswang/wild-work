package app

import (
	"os"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// TestGLMDisplayURLDoesNotFakeSuccess 锁定 H9：GLM 属于「只给链接、不跑流程」的渠道，
// 前端**不得**为它启轮询，否则会出现「提示登录完成、实际什么都没发生」的假成功。
//
// 机理（2026-10-18 深档排障）：
//   - GLM 无编程式登录，凭据靠用户从浏览器 Cookie 取出后粘贴（SubmitGLMToken）。
//   - 所以 StartLoginFor 对它是提前 return（不置 loginBusy、不起 pollLogin）。
//   - 但改造前它靠「返回非空 auth_url」伪装成正常登录，前端据此无条件 startLoginPoll()；
//     轮询读到 login_busy=false 即提示「登录完成，正在同步账号…」—— 假成功。
//
// 修法：后端在 /api/login/start 响应里带 needs_poll，前端据此决定是否轮询。
func TestGLMDisplayURLDoesNotFakeSuccess(t *testing.T) {
	// ① 后端：GLM 必须以 DisplayURL 表达（而不是伪装成有 Start）。
	f, ok := loginFlows[provider.GLM]
	if !ok {
		t.Fatal("❌ loginFlows 未登记 provider.GLM")
	}
	if f.Start != nil {
		t.Error("❌ provider.GLM 登记了 Start —— 它没有编程式登录，只应给链接（DisplayURL）")
	}
	if f.DisplayURL == "" {
		t.Error("❌ provider.GLM 的 DisplayURL 为空 —— 用户拿不到登录页地址")
	}
	// 只有 DisplayURL 形态的渠道才不需要轮询；其余渠道必须有 Start。
	if f.Poll != nil {
		t.Error("❌ provider.GLM 登记了 Poll —— 它不跑流程，无从轮询")
	}

	// ② 后端接口必须下发 needs_poll（否则前端又只能靠「URL 非空」猜）。
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("读 app.go 失败: %v", err)
	}
	if !strings.Contains(string(src), `"needs_poll"`) {
		t.Error("❌ /api/login/start 未下发 needs_poll —— 前端无法区分「真登录」与「只给链接」")
	}

	// ②b 行为层断言（关键）：直接验证后端判定逻辑对 GLM 给出 false、对真登录渠道给出 true。
	//     只断言「字段存在」是不够的 —— 把 needsPoll 写死成 true 也能让那种断言通过
	//     （2026-10-18 变异实验实测踩到，故补上这一层）。
	if (&App{}).needsPollFor("glm") {
		t.Error("❌ needsPollFor(\"glm\") == true —— GLM 不跑流程却要求轮询，前端会假成功（H9）")
	}
	for _, ch := range []string{"raccoon", "loomy", "codearts", "traework", "workbuddyai"} {
		if !(&App{}).needsPollFor(ch) {
			t.Errorf("❌ needsPollFor(%q) == false —— 这些渠道真的跑登录流程，必须轮询", ch)
		}
	}

	// ③ 前端必须真的读了它，且在不轮询的分支里 return（不能落回 startLoginPoll）。
	js, err := os.ReadFile("../../cmd/wild-work/web/app.js")
	if err != nil {
		t.Fatalf("读 app.js 失败: %v", err)
	}
	jsSrc := string(js)
	if !strings.Contains(jsSrc, "r.needs_poll") {
		t.Error("❌ app.js 未读取 needs_poll —— 仍会为 GLM 启轮询（H9 假成功）")
	}
	// 定位 startLogin 里 needs_poll 判定之后，必须紧跟 return（在 startLoginPoll 之前）。
	idx := strings.Index(jsSrc, "r.needs_poll === false")
	if idx < 0 {
		t.Fatal("❌ app.js 未按 needs_poll===false 分流")
	}
	tail := jsSrc[idx:]
	pollIdx := strings.Index(tail, "startLoginPoll()")
	retIdx := strings.Index(tail, "return;")
	if retIdx < 0 || (pollIdx >= 0 && retIdx > pollIdx) {
		t.Error("❌ app.js 的 needs_poll===false 分支没有在 startLoginPoll() 之前 return —— " +
			"GLM 仍会被启轮询（H9 假成功）")
	}
}
