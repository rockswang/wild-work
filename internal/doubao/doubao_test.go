package doubao

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// ---------- SSE 转换 ----------

// TestStreamChunkDelta 正文增量走 CHUNK_DELTA。
func TestStreamChunkDelta(t *testing.T) {
	src := "event: CHUNK_DELTA\ndata: {\"text\":\"你好\"}\n\n" +
		"event: CHUNK_DELTA\ndata: {\"text\":\"，世界\"}\n\n" +
		"event: SSE_REPLY_END\ndata: {\"end_type\":0}\n\n"
	out, err := io.ReadAll(newDoubaoStream(strings.NewReader(src), "doubao/deepseek-v4-flash"))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	got := string(out)
	for _, want := range []string{"你好", "，世界", "chat.completion.chunk", "[DONE]", "doubao/deepseek-v4-flash"} {
		if !strings.Contains(got, want) {
			t.Fatalf("输出缺少 %q：\n%s", want, got)
		}
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("[DONE] 应恰好一次，实际 %d", strings.Count(got, "data: [DONE]"))
	}
}

// TestStreamReasoningGoesToReasoningContent 思考标题必须走 reasoning_content，
// 不得混进正文（与其它渠道同口径）。
func TestStreamReasoningGoesToReasoningContent(t *testing.T) {
	src := "event: STREAM_CHUNK\n" +
		"data: {\"patch_op\":[{\"patch_value\":{\"content_block\":[{\"block_type\":10040,\"content\":{\"thinking_block\":{\"streaming_title\":\"正在思考\"}}}]}}]}\n\n" +
		"event: STREAM_CHUNK\n" +
		"data: {\"patch_op\":[{\"patch_value\":{\"content_block\":[{\"block_type\":10000,\"content\":{\"text_block\":{\"text\":\"答案\"}}}]}}]}\n\n" +
		"event: SSE_REPLY_END\ndata: {\"end_type\":0}\n\n"
	out, err := io.ReadAll(newDoubaoStream(strings.NewReader(src), "doubao/auto"))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `"reasoning_content":"正在思考"`) {
		t.Fatalf("思考标题未走 reasoning_content：\n%s", got)
	}
	if !strings.Contains(got, `"content":"答案"`) {
		t.Fatalf("正文缺失：\n%s", got)
	}
	// 思考内容不得出现在正文 delta 里
	if strings.Contains(got, `"content":"正在思考"`) {
		t.Fatalf("思考内容混进了正文：\n%s", got)
	}
}

// TestStreamEmptyIsError 一个内容块都没有必须上抛，且**不补 [DONE]**（R36）。
func TestStreamEmptyIsError(t *testing.T) {
	src := "event: SSE_REPLY_END\ndata: {\"end_type\":0}\n\n"
	out, err := io.ReadAll(newDoubaoStream(strings.NewReader(src), "doubao/auto"))
	if err == nil {
		t.Fatalf("空响应必须报错，实际输出：%s", out)
	}
	if strings.Contains(string(out), "[DONE]") {
		t.Fatal("空响应不得补 [DONE] 伪装正常收尾（R36）")
	}
}

// TestStreamReplyEndTypeInformational end_type 非 0 不等于错误（工作模式正常收尾即 1，
// 2026-11-15 实测：正文完整 + end_type=1）；有正文就正常收尾、补 [DONE]。
func TestStreamReplyEndTypeInformational(t *testing.T) {
	src := "event: CHUNK_DELTA\ndata: {\"text\":\"半截\"}\n\n" +
		"event: SSE_REPLY_END\ndata: {\"end_type\":1}\n\n"
	out, err := io.ReadAll(newDoubaoStream(strings.NewReader(src), "doubao/deepseek-v4-flash"))
	if err != nil {
		t.Fatalf("end_type=1 + 有正文不应报错：%v", err)
	}
	if !strings.Contains(string(out), "半截") || !strings.Contains(string(out), "[DONE]") {
		t.Fatalf("应正常收尾：\n%s", out)
	}
}

// TestStreamBriefFallback 无流式正文但有 brief 时，用 brief 兜底（参考实现同款）。
func TestStreamBriefFallback(t *testing.T) {
	src := "event: SSE_REPLY_END\ndata: {\"end_type\":0,\"msg_finish_attr\":{\"brief\":\"兜底摘要\"}}\n\n"
	out, err := io.ReadAll(newDoubaoStream(strings.NewReader(src), "doubao/deepseek-v4-flash"))
	if err != nil {
		t.Fatalf("有 brief 不应报错：%v", err)
	}
	if !strings.Contains(string(out), "兜底摘要") || !strings.Contains(string(out), "[DONE]") {
		t.Fatalf("brief 应作为正文兜底并正常收尾：\n%s", out)
	}
}

// TestStreamEOFWithoutBlankLine 上游不发收尾空行时，最后一个事件仍要被处理
// （参考实现的真实缺陷：末帧被静默丢弃）。
func TestStreamEOFWithoutBlankLine(t *testing.T) {
	src := "event: CHUNK_DELTA\ndata: {\"text\":\"尾巴\"}"
	out, err := io.ReadAll(newDoubaoStream(strings.NewReader(src), "doubao/auto"))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !strings.Contains(string(out), "尾巴") {
		t.Fatalf("EOF 前最后一个事件被丢弃：\n%s", out)
	}
}

// TestStreamStreamError 流内 STREAM_ERROR 必须解析并上抛（R40）。
func TestStreamStreamError(t *testing.T) {
	src := "id: 0\nevent: SSE_HEARTBEAT\ndata: {}\n\n" +
		"id: 0\nevent: STREAM_ERROR\ndata: {\"code\":710022004,\"message\":\"触发人机验证\"}\n\n"
	_, err := io.ReadAll(newDoubaoStream(strings.NewReader(src), "doubao/auto"))
	if err == nil {
		t.Fatal("STREAM_ERROR 必须报错")
	}
	if !strings.Contains(err.Error(), "710022004") || !strings.Contains(err.Error(), "人机验证") {
		t.Fatalf("错误应带上游码与文案，实际：%v", err)
	}
}

// TestStreamSamanthaRealFrames 用**真实抓到的帧**验证整条链（2026-11-15 实盘）。
//
// 最保值的回归：上游真实形态是 event_data 为 **JSON 字符串**、首帧是
// seed_intention、ACK 是 2002、正文是 (event_type=2001, content_type=2001)、
// 推荐问是 (2001, 2002)、收尾是 2003。少认一帧就静默丢内容。
//
// 帧用 json.Marshal 构造（不手写转义）：event_data / message / content 三层
// 都是「JSON 序列化成字符串」的形态，手写转义极易错。
func TestStreamSamanthaRealFrames(t *testing.T) {
	frame := func(eventType int, eventData any) string {
		inner, err := json.Marshal(eventData)
		if err != nil {
			t.Fatal(err)
		}
		outer, err := json.Marshal(map[string]any{
			"event_data": string(inner), // 关键：是**字符串**，不是对象
			"event_id":   "0",
			"event_type": eventType,
		})
		if err != nil {
			t.Fatal(err)
		}
		return "data: " + string(outer) + "\n\n"
	}
	contentFrame := func(contentType int, content any) string {
		c, err := json.Marshal(content)
		if err != nil {
			t.Fatal(err)
		}
		return frame(2001, map[string]any{
			"message": map[string]any{
				"content_type": contentType,
				"content":      string(c), // 同样：content 也是 JSON 字符串
			},
			"message_id": "1",
		})
	}

	src := frame(2002, map[string]any{"type": "seed_intention", "seed_intention": map[string]any{"intention": "seed_main"}}) +
		frame(2002, map[string]any{"message_id": "61933800861933570", "conversation_id": "0", "section_id": "0"}) +
		contentFrame(2001, map[string]any{"text": "Hello! How can I help you today?"}) +
		contentFrame(2002, map[string]any{"suggest": []any{}}) +
		frame(2003, map[string]any{})

	out, err := io.ReadAll(newDoubaoStream(strings.NewReader(src), "doubao/doubao"))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "Hello! How can I help you today?") {
		t.Fatalf("真实帧的正文没解出来：\n%s", got)
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("应收尾：\n%s", got)
	}
}

// TestStreamHeartbeatSkipped 心跳帧不得当成内容、不得让流失败。
func TestStreamHeartbeatSkipped(t *testing.T) {
	src := "id: 0\nevent: SSE_HEARTBEAT\ndata: {}\n\n" +
		"event: CHUNK_DELTA\ndata: {\"text\":\"正文\"}\n\n" +
		"id: 0\nevent: SSE_HEARTBEAT\ndata: {}\n\n" +
		"event: SSE_REPLY_END\ndata: {\"end_type\":0}\n\n"
	out, err := io.ReadAll(newDoubaoStream(strings.NewReader(src), "doubao/auto"))
	if err != nil {
		t.Fatalf("心跳帧不应导致失败：%v", err)
	}
	if !strings.Contains(string(out), "正文") {
		t.Fatalf("正文缺失：\n%s", out)
	}
}

// ---------- 请求体 ----------

const diagDev = "7558368080707955"

// TestOutboundBodyChatShape 请求体必须是 /chat/completion 的工作模式形状。
func TestOutboundBodyChatShape(t *testing.T) {
	body := []byte(`{"model":"doubao/deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	out, err := outboundBody(body, diagDev)
	if err != nil {
		t.Fatalf("outboundBody: %v", err)
	}
	got := string(out)
	// /chat/completion 工作模式关键字段
	for _, want := range []string{
		`"model_item_key":"person-deepseek-v4-1-flash-261017"`,
		`"agent_mode":1`,
		`"client_meta"`,
		`"content_block"`,
		`"block_type":10000`,
		`"conversation_init_ext"`,
		`"general_task_param"`,
		`"local_device_id":"` + diagDev + `"`, // device_id 必须透传进 agent_task_param
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("请求体缺少 %q：\n%s", want, got)
		}
	}
	// 已废弃的 samantha 形状字段不得出现
	for _, bad := range []string{"content_type", "completion_option", "evaluate_option"} {
		if strings.Contains(got, bad) {
			t.Fatalf("不应出现旧 samantha 字段 %q：\n%s", bad, got)
		}
	}
}

// TestOutboundBodyDefaultModel 空 model 回退默认模型。
func TestOutboundBodyDefaultModel(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out, err := outboundBody(body, diagDev)
	if err != nil {
		t.Fatalf("outboundBody: %v", err)
	}
	spec := staticModels[DefaultModel]
	if !strings.Contains(string(out), `"model_item_key":"`+spec.ItemKey+`"`) {
		t.Fatalf("空 model 应回退默认 %s（item_key=%s）：\n%s", DefaultModel, spec.ItemKey, out)
	}
}

// TestOutboundBodyUnknownModel 未知模型本地拒绝（请求级错误，不罚账号不轮换）。
func TestOutboundBodyUnknownModel(t *testing.T) {
	body := []byte(`{"model":"doubao/nope","messages":[{"role":"user","content":"hi"}]}`)
	_, err := outboundBody(body, diagDev)
	if err == nil {
		t.Fatal("未知模型必须拒绝")
	}
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrBadParams {
		t.Fatalf("未知模型应归 ErrBadParams，实际 %T %v", err, err)
	}
}

// TestOutboundBodyFlattensHistory 首轮把历史压成一段（上游只认一条 user 文本）。
func TestOutboundBodyFlattensHistory(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"第一句"},{"role":"assistant","content":"第一次回"},{"role":"user","content":"第二句"}]}`)
	out, err := outboundBody(body, diagDev)
	if err != nil {
		t.Fatalf("outboundBody: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "第一句") || !strings.Contains(got, "第一次回") || !strings.Contains(got, "第二句") {
		t.Fatalf("历史未被压进请求体：\n%s", got)
	}
}

// ---------- 凭据 ----------

// TestNormalizeCookie 三种粘贴形态都要吃得下。
func TestNormalizeCookie(t *testing.T) {
	cases := []string{
		"sessionid=abc; ttwid=def",
		"Cookie: sessionid=abc; ttwid=def",
		"\"sessionid=abc; ttwid=def\"",
		"sessionid=abc\n\tttwid=def",
	}
	for _, in := range cases {
		got := NormalizeCookie(in)
		if !strings.HasPrefix(got, "sessionid=abc") || !strings.Contains(got, "ttwid=def") {
			t.Fatalf("NormalizeCookie(%q) = %q", in, got)
		}
	}
}

// TestUIDStable 同一 sessionid → 同一 uid；不同 sessionid → 不同 uid。
func TestUIDStable(t *testing.T) {
	a := UIDOfCookie("sessionid=aaa; ttwid=x")
	b := UIDOfCookie("sessionid=aaa; ttwid=y")
	c := UIDOfCookie("sessionid=bbb")
	if a != b {
		t.Fatalf("同一 sessionid 应得同一 uid：%s vs %s", a, b)
	}
	if a == c {
		t.Fatal("不同 sessionid 应得不同 uid")
	}
	if !strings.HasPrefix(a, "doubao-") {
		t.Fatalf("uid 前缀应为 doubao-，实际 %s", a)
	}
}

// TestDeviceIDStable 设备号必须稳定（每次请求都变会让上游看到无限新设备）。
func TestDeviceIDStable(t *testing.T) {
	c := "sessionid=aaa; ttwid=x"
	if DeviceIDOf(c, "") != DeviceIDOf(c, "") {
		t.Fatal("device_id 必须由 cookie 稳定派生")
	}
	if DeviceIDOf(c, "fixed") != "fixed" {
		t.Fatal("已存储的 device_id 应优先")
	}
}

// ---------- 模型面 ----------

func TestKnownModel(t *testing.T) {
	for _, id := range []string{"doubao-2.1-lite", "doubao-2.1-pro", "deepseek-v4-flash", "deepseek-v4-pro", "kimi-k3", "kimi-k3.1-pro", "kimi-k3.1-pro-fast", "glm-5.3", "glm-5.3-flash"} {
		if !KnownModel(id) {
			t.Fatalf("%s 应已知", id)
		}
	}
	if KnownModel("nope") {
		t.Fatal("未知模型不得报已知")
	}
	// 已废弃的 samantha 思考档位不再暴露（R49：不列已下线的能力）
	if KnownModel("doubao-think") {
		t.Fatal("samantha 思考档位已废弃，不应再报已知")
	}
}

// TestStreamChunkDeltaDedup 正文经 CHUNK_DELTA 给过后，STREAM_CHUNK 的 block_type 10000
// 不得重复吐（参考实现同款去重：/chat/completion 会两路下发同一段正文）。
func TestStreamChunkDeltaDedup(t *testing.T) {
	src := "event: CHUNK_DELTA\ndata: {\"text\":\"答案\"}\n\n" +
		"event: STREAM_CHUNK\n" +
		"data: {\"patch_op\":[{\"patch_value\":{\"content_block\":[{\"block_type\":10000,\"content\":{\"text_block\":{\"text\":\"答案\"}}}]}}]}\n\n" +
		"event: SSE_REPLY_END\ndata: {\"end_type\":0}\n\n"
	out, err := io.ReadAll(newDoubaoStream(strings.NewReader(src), "doubao/deepseek-v4-flash"))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if n := strings.Count(string(out), `"content":"答案"`); n != 1 {
		t.Fatalf("正文应只出现一次（CHUNK_DELTA + STREAM_CHUNK 去重），实际 %d 次：\n%s", n, out)
	}
}

// ---------- 设备指纹格式 ----------

// TestDeviceParamsShape 设备参数必须是上游认的 shape（形状不对回 710020202）。
func TestDeviceParamsShape(t *testing.T) {
	dev, web, fp := GenerateDeviceParams()
	if !allDigits(dev) || len(dev) != 16 || dev[0] != '7' {
		t.Fatalf("device_id 应是 16 位数字且 7 开头，实际 %q", dev)
	}
	if !allDigits(web) || len(web) != 19 || !strings.HasPrefix(web, "76") {
		t.Fatalf("web_id 应是 19 位数字且 76 开头，实际 %q", web)
	}
	if !strings.HasPrefix(fp, "verify_") {
		t.Fatalf("fp 应以 verify_ 开头，实际 %q", fp)
	}
	parts := strings.Split(strings.TrimPrefix(fp, "verify_"), "_")
	if len(parts) != 5 {
		t.Fatalf("fp 应五段，实际 %q", fp)
	}
	for i, want := range []int{8, 4, 4, 4, 12} {
		if len(parts[i]) != want {
			t.Fatalf("fp 第 %d 段应 %d 位，实际 %q", i+1, want, parts[i])
		}
	}
}

// TestDeviceParamsStableForLegacy 老账号（无三个字段）按 Cookie 稳定派生，且形状合法。
func TestDeviceParamsStableForLegacy(t *testing.T) {
	cookie := "sessionid=abc123; ttwid=x"
	d1, w1, f1 := DeviceParamsOf(cookie, "", "", "")
	d2, w2, f2 := DeviceParamsOf(cookie, "", "", "")
	if d1 != d2 || w1 != w2 || f1 != f2 {
		t.Fatal("同一 Cookie 必须派生同一套设备参数")
	}
	if !validDeviceID(d1) || !validWebID(w1) || !validFP(f1) {
		t.Fatalf("派生值形状不合法：%q %q %q", d1, w1, f1)
	}
	// 存量的合法值应优先（不重复派生）
	d3, w3, f3 := DeviceParamsOf(cookie, d1, w1, f1)
	if d3 != d1 || w3 != w1 || f3 != f1 {
		t.Fatal("已存储的合法值应优先")
	}
	// 坏形状（首版用过的 16 位十六进制）必须被替换掉
	bad := "abcdef0123456789"
	d4, _, _ := DeviceParamsOf(cookie, bad, "", "")
	if d4 == bad {
		t.Fatal("十六进制 device_id 必须被判为不合法并替换")
	}
}

// ---------- 错误分类 ----------

func TestClassifyOrder(t *testing.T) {
	c := New()
	// 429 优先于业务码（不变量 15）：限流响应体高频带风控文案。
	if got := c.Classify(http.StatusTooManyRequests, `{"code":710022004,"message":"触发人机验证"}`); got != provider.ErrSoftRate {
		t.Fatalf("429 必须优先归 ErrSoftRate，实际 %v", got)
	}
	if got := c.Classify(http.StatusOK, `{"code":710022004}`); got != provider.ErrSoftRate {
		t.Fatalf("710022004 应归 ErrSoftRate，实际 %v", got)
	}
	if got := c.Classify(http.StatusOK, `{"code":710022002}`); got != provider.ErrSoftRate {
		t.Fatalf("710022002 应归 ErrSoftRate，实际 %v", got)
	}
	if got := c.Classify(http.StatusUnauthorized, ""); got != provider.ErrSessionDead {
		t.Fatalf("401 应归 ErrSessionDead，实际 %v", got)
	}
	if got := c.Classify(http.StatusInternalServerError, ""); got != provider.ErrServer {
		t.Fatalf("5xx 应归 ErrServer，实际 %v", got)
	}
}

// ---------- 签到 / 额度（httptest） ----------

func newProbeClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New()
	c.HTTP = srv.Client()
	c.baseURL = srv.URL
	return c
}

// TestCheckinClaimedAndAlready 签到三态。
func TestCheckinClaimedAndAlready(t *testing.T) {
	c := newProbeClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/checkin") {
			t.Errorf("路径不符：%s", r.URL.Path)
		}
		if got := r.Header.Get("User-Agent"); !strings.Contains(got, "SamanthaDoubao") {
			t.Errorf("UA 必须是桌面客户端形态，实际 %q", got)
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"checked_in":true,"continuous_days":3,"reward_amount":100}}`))
	})
	rep, err := c.DailyCheckinReport(&auth.Auth{Extra: map[string]string{ExtraCookie: "sessionid=x"}})
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	if rep.Status != provider.CheckinClaimed || rep.Amount != 100 {
		t.Fatalf("应判 claimed +100，实际 %v %d", rep.Status, rep.Amount)
	}

	c2 := newProbeClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"checked_in":true,"idempotent":true,"reward_amount":100}}`))
	})
	rep2, err := c2.DailyCheckinReport(&auth.Auth{Extra: map[string]string{ExtraCookie: "sessionid=x"}})
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	if rep2.Status != provider.CheckinAlready {
		t.Fatalf("幂等命中应判 already，实际 %v", rep2.Status)
	}
	if rep2.Status.Retryable() {
		t.Fatal("already 不得可重试（否则整天重敲）")
	}
}

// TestCheckinNoCampaign 710010702 → no_campaign（窗口内可重试）。
func TestCheckinNoCampaign(t *testing.T) {
	c := newProbeClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":710010702,"message":"no campaign"}`))
	})
	rep, err := c.DailyCheckinReport(&auth.Auth{Extra: map[string]string{ExtraCookie: "sessionid=x"}})
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	if rep.Status != provider.CheckinNoCampaign {
		t.Fatalf("应判 no_campaign，实际 %v", rep.Status)
	}
}

// TestCheckin401Typed 401 必须类型化 ErrSessionDead（不变量 19）。
func TestCheckin401Typed(t *testing.T) {
	c := newProbeClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":200003,"message":"authorization_verify_error"}`))
	})
	_, err := c.DailyCheckinReport(&auth.Auth{Extra: map[string]string{ExtraCookie: "sessionid=x"}})
	if err == nil {
		t.Fatal("401 必须返回 error")
	}
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrSessionDead {
		t.Fatalf("401 应类型化为 ErrSessionDead，实际 %T %v", err, err)
	}
}

// TestQuotaAggregation 额度按 window_limits 累加。
func TestQuotaAggregation(t *testing.T) {
	c := newProbeClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"feature_group_name":"活动赠送","window_limits":[
			{"total_amount":800,"used_amount":23,"end_time":1797105419},
			{"total_amount":0,"used_amount":0}
		]}}`))
	})
	remain, err := c.UserResource(&auth.Auth{Extra: map[string]string{ExtraCookie: "sessionid=x"}})
	if err != nil {
		t.Fatalf("UserResource: %v", err)
	}
	if remain != 777 {
		t.Fatalf("remain 应累加为 777，实际 %d", remain)
	}
	_, items, err := c.UserResourceDetail(&auth.Auth{Extra: map[string]string{ExtraCookie: "sessionid=x"}})
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("应产出 2 条明细（1 窗口 + 1 汇总），实际 %d", len(items))
	}
	for _, it := range items {
		if it.ExpireAt != "" {
			t.Fatalf("条目不得伪造到期时间（不变量 18）：%+v", it)
		}
	}
}

// TestNoCookieIsSessionDead 没有 Cookie 时必须类型化会话失效。
func TestNoCookieIsSessionDead(t *testing.T) {
	c := New()
	_, err := c.UserResource(&auth.Auth{})
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrSessionDead {
		t.Fatalf("无 Cookie 应归 ErrSessionDead，实际 %v", err)
	}
}

// TestRefreshTokenNoEndpoint 无 refresh 端点 → ErrSessionDead（转面板重登，不 Disable）。
func TestRefreshTokenNoEndpoint(t *testing.T) {
	c := New()
	err := c.RefreshToken(&auth.Auth{RefreshToken: "x"})
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrSessionDead {
		t.Fatalf("应归 ErrSessionDead，实际 %v", err)
	}
}
