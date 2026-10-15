package traework

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/loopguard"
	"wild-work/internal/provider"
)

// 本文件锁定「流内业务错误不得伪装成正常收尾」这一行为（2026-10-02 修复）。
//
// 背景：上游 SOLO 在流中途回一帧 event:error（如 3004 限流 / 1005 权益不足）。
// 旧实现把它写成 delta.content 文本 + finish_reason:"stop" + 补 [DONE]，
// 客户端看到的是「正常结束、内容莫名其妙」，agent 会拿着半截回答继续跑；
// 且函数返回 nil，调用方无从得知失败 ⇒ 账号不会被冷却，粘性路由还把请求
// 钉死在同一个限流号上（用户实测困惑："明明积分很多却报错，重试还是它"）。
//
// 现行为（对齐上游 issue #42 的同类修复）：
//   - 发一帧 OpenAI 规范 error（code=upstream_rate_limited / upstream_error）；
//   - **不补 [DONE]**（终止信号交给 error 帧，避免被当成正常收尾）；
//   - 把 *SOLOStreamError 返回给调用方，供 handler 冷却账号。

// soloFrame 构造一帧 SOLO SSE（event + data + 空行结尾）。
func soloFrame(event, data string) string {
	return "event:" + event + "\ndata:" + data + "\n\n"
}

// TestStreamErrorDoesNotFakeDone 上游回 3004 → 必须发 error 帧且不发 [DONE]。
func TestStreamErrorDoesNotFakeDone(t *testing.T) {
	body := soloFrame("output", `{"response":"部分回答"}`) +
		soloFrame("error", `{"code":3004,"message":"We're sorry, your requests have exceeded the rate limit."}`)

	w := httptest.NewRecorder()
	var gotErr *SOLOStreamError
	usage, err := streamOpts(w, strings.NewReader(body), func(e *SOLOStreamError) { gotErr = e }, "m", nil)
	_ = usage
	out := w.Body.String()

	// ① 必须返回错误给调用方（旧实现返回 nil，这正是"账号罚不到"的根因）
	var se *SOLOStreamError
	if !asSOLOError(err, &se) {
		t.Fatalf("流内错误必须返回给调用方，got err=%v", err)
	}
	if se.Code != 3004 {
		t.Fatalf("错误码=%d want 3004", se.Code)
	}
	// ② onErr 回调也要被触发（保留给需要即时反应的调用方）
	if gotErr == nil || gotErr.Code != 3004 {
		t.Fatalf("onErr 未被正确回调，got %+v", gotErr)
	}
	// ③ 不得补 [DONE]（否则客户端误判为正常收尾）
	if strings.Contains(out, "[DONE]") {
		t.Errorf("流内错误不得补 [DONE]（会把失败伪装成正常收尾）\nbody=%s", out)
	}
	// ④ 必须发出 OpenAI 规范 error 帧，且能区分限流
	if !strings.Contains(out, `"error"`) {
		t.Errorf("必须发 error 帧\nbody=%s", out)
	}
	if !strings.Contains(out, "upstream_rate_limited") {
		t.Errorf("3004 应标记为限流的错误码\nbody=%s", out)
	}
	// ⑤ 已透传的部分内容仍应在（不因报错而丢弃）
	if !strings.Contains(out, "部分回答") {
		t.Errorf("已透传的部分内容不应丢失\nbody=%s", out)
	}
}

// TestStreamErrorKindClassification 错误码 → 惩罚档位的映射。
// 3004/9074 是**账号级**限流（实测：同秒 8 号仅 1 个中招）⇒ 软冷却后换号即可成功。
func TestStreamErrorKindClassification(t *testing.T) {
	cases := []struct {
		code int64
		want provider.ErrKind
		why  string
	}{
		{1005, provider.ErrHardCredit, "权益/余额不足 → 长冷却"},
		{3004, provider.ErrSoftRate, "按账号限流 → 短冷却，换号可恢复"},
		{9074, provider.ErrSoftRate, "同族限流码（客户端签名校验不过时的拒绝）"},
		{1001, provider.ErrClient, "模型不可用等其余业务错误不罚账号"},
	}
	for _, c := range cases {
		got := (&SOLOStreamError{Code: c.code}).Kind()
		if got != c.want {
			t.Errorf("code=%d Kind()=%v want %v（%s）", c.code, got, c.want, c.why)
		}
	}
}

// TestStreamNormalEndStillEmitsDone 正常收尾行为不得被改动：
// 上游发 done → 恰好一个 [DONE]，且不返回错误。
func TestStreamNormalEndStillEmitsDone(t *testing.T) {
	body := soloFrame("output", `{"response":"你好"}`) +
		soloFrame("done", `{"finish_reason":"stop"}`)

	w := httptest.NewRecorder()
	_, err := streamOpts(w, strings.NewReader(body), nil, "m", nil)
	out := w.Body.String()

	if err != nil {
		t.Fatalf("正常收尾不得返回错误，got %v", err)
	}
	if n := strings.Count(out, "[DONE]"); n != 1 {
		t.Errorf("[DONE] 出现 %d 次，want 恰好 1 次\nbody=%s", n, out)
	}
	if strings.Contains(out, `"error"`) {
		t.Errorf("正常收尾不得出现 error 帧\nbody=%s", out)
	}
}

// TestStreamErrorStopsConsumingFurtherFrames 错误帧之后的上游帧不再消费，
// 且不补 [DONE]（终止信号交给 error 帧）。
//
// 注意本用例是「error 在前」，**不是** done→error 双终止场景——
// 后者见 TestStreamErrorAfterDoneEmitsSingleTerminator。
func TestStreamErrorStopsConsumingFurtherFrames(t *testing.T) {
	body := soloFrame("error", `{"code":3004,"message":"rate limited"}`) +
		soloFrame("output", `{"response":"错误之后的内容"}`)

	w := httptest.NewRecorder()
	_, err := streamOpts(w, strings.NewReader(body), nil, "m", nil)
	out := w.Body.String()

	if err == nil {
		t.Fatal("错误帧后应立即返回错误（不再继续消费后续帧）")
	}
	if strings.Contains(out, "[DONE]") {
		t.Errorf("错误路径不得补 [DONE]\nbody=%s", out)
	}
	if strings.Contains(out, "错误之后的内容") {
		t.Errorf("错误帧之后的内容不应再透传\nbody=%s", out)
	}
}

// TestStreamErrorAfterDoneEmitsSingleTerminator 防御场景：done → error。
//
// done 已写出 [DONE]，流对客户端而言已正常终止；此时若再发 error 帧，
// 客户端会收到**两个终止信号**（严格客户端行为未定义）。
// 故该路径只记日志 + 把错误返回给调用方（账号照常冷却），不再写第二帧。
func TestStreamErrorAfterDoneEmitsSingleTerminator(t *testing.T) {
	body := soloFrame("output", `{"response":"完整回答"}`) +
		soloFrame("done", `{"finish_reason":"stop"}`) +
		soloFrame("error", `{"code":3004,"message":"late rate limit"}`)

	w := httptest.NewRecorder()
	_, err := streamOpts(w, strings.NewReader(body), nil, "m", nil)
	out := w.Body.String()

	if got := strings.Count(out, "[DONE]"); got != 1 {
		t.Errorf("[DONE] 出现 %d 次，want 恰好 1 次（两个终止信号会让客户端行为未定义）\nbody=%s", got, out)
	}
	if strings.Contains(out, `"error"`) {
		t.Errorf("done 之后不得再发 error 帧（会形成双终止信号）\nbody=%s", out)
	}
	// 但仍须把业务错误返回给调用方：账号冷却依赖它
	var se *SOLOStreamError
	if !asSOLOError(err, &se) {
		t.Fatalf("done 后的业务错误仍须返回给调用方（否则账号不冷却），got %v", err)
	}
}

// failingWriter 首次 Write 即失败，用于模拟「客户端在 error 帧写出前已断开」。
type failingWriter struct {
	hdr http.Header
}

func (f *failingWriter) Header() http.Header {
	if f.hdr == nil {
		f.hdr = http.Header{}
	}
	return f.hdr
}
func (f *failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (f *failingWriter) WriteHeader(int)           {}

// TestStreamErrorWriteFailureStillReturnsBusinessError 回归（复核发现）：
// 写入 error 帧失败时**不得吞掉业务错误**——调用方要据此冷却账号；
// 若返回 werr，errors.As 取不到 *SOLOStreamError，账号就不冷却，修复目标链断掉。
func TestStreamErrorWriteFailureStillReturnsBusinessError(t *testing.T) {
	body := soloFrame("error", `{"code":3004,"message":"rate limited"}`)

	w := &failingWriter{}
	_, err := streamOpts(w, strings.NewReader(body), nil, "m", nil)

	var se *SOLOStreamError
	if !asSOLOError(err, &se) {
		t.Fatalf("写入失败时仍须返回业务错误（否则账号不冷却），got %v", err)
	}
	if se.Code != 3004 {
		t.Fatalf("错误码=%d want 3004", se.Code)
	}
}

// asSOLOError 小工具：等价于 errors.As，单独抽出来避免测试里 import errors 的噪音。
func asSOLOError(err error, target **SOLOStreamError) bool {
	if err == nil {
		return false
	}
	se, ok := err.(*SOLOStreamError)
	if ok {
		*target = se
	}
	return ok
}

// TestStreamReasoningLoopIntercepted 思考链死循环（发癫）命中：发 error 帧
// （code=upstream_reasoning_loop）、不补 [DONE]、返回 *loopguard.Error，
// 且死循环之后的正文不再透传。SOLO 的 reasoning 走 output 事件的 reasoning_content。
func TestStreamReasoningLoopIntercepted(t *testing.T) {
	line := "这里是一个足够长的、会被模型机械重复的思考行内容"
	var body strings.Builder
	for i := 0; i < 10; i++ {
		body.WriteString(soloFrame("output", `{"response":"","reasoning_content":"`+line+`\n"}`))
	}
	body.WriteString(soloFrame("output", `{"response":"正文","reasoning_content":""}`))
	body.WriteString(soloFrame("done", `{"finish_reason":"stop"}`))

	w := httptest.NewRecorder()
	_, err := streamOpts(w, strings.NewReader(body.String()), nil, "m", nil)
	if err == nil {
		t.Fatal("期望守卫命中返回错误，实际 nil")
	}
	var lge *loopguard.Error
	if !asLoopGuardError(err, &lge) {
		t.Fatalf("期望 *loopguard.Error，得 %T: %v", err, err)
	}
	out := w.Body.String()
	if !strings.Contains(out, `"code":"upstream_reasoning_loop"`) {
		t.Errorf("缺少 upstream_reasoning_loop 错误帧: %q", out)
	}
	if strings.Contains(out, "data: [DONE]") {
		t.Errorf("守卫命中不得补 [DONE]: %q", out)
	}
	if strings.Contains(out, "正文") {
		t.Errorf("死循环之后的正文不应透传: %q", out)
	}
}

// TestStreamNormalReasoningPasses 正常各不相同的思考行不应触发（零误伤），正常收尾。
func TestStreamNormalReasoningPasses(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 30; i++ {
		body.WriteString(soloFrame("output", `{"response":"","reasoning_content":"第 `+itoa(i)+` 步思考内容各不相同\n"}`))
	}
	body.WriteString(soloFrame("done", `{"finish_reason":"stop"}`))

	w := httptest.NewRecorder()
	_, err := streamOpts(w, strings.NewReader(body.String()), nil, "m", nil)
	if err != nil {
		t.Fatalf("正常推理不应报错：%v", err)
	}
	if !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Errorf("正常推理应正常收尾: %q", w.Body.String())
	}
}

// asLoopGuardError errors.As 小工具。
func asLoopGuardError(err error, target **loopguard.Error) bool {
	if err == nil {
		return false
	}
	le, ok := err.(*loopguard.Error)
	if ok {
		*target = le
	}
	return ok
}

// itoa 简易整数转字符串（避免为测试引 strconv）。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var d []byte
	for i > 0 {
		d = append([]byte{byte('0' + i%10)}, d...)
		i /= 10
	}
	return string(d)
}
