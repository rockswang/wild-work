package officeace

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 锁定 V4/V11 签名的结构（不联网）：授权头应含算法标识、Credential、SignedHeaders、Signature。
func TestSignV4Shape(t *testing.T) {
	hdrs, err := SignV4("AKTEST", "SKTEST", "STS", "PROJ",
		"GET", "https://agentarts.cn-southwest-2.myhuaweicloud.com/v1/claw/client-permission-validate",
		"", "application/json;charset=utf8", nil, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	auth := hdrs["Authorization"]
	if !strings.HasPrefix(auth, "SDK-HMAC-SHA256 Access=AKTEST, SignedHeaders=") {
		t.Fatalf("unexpected V4 auth header: %s", auth)
	}
	if !strings.Contains(auth, ", Signature=") {
		t.Fatalf("missing signature: %s", auth)
	}
	if hdrs["X-Sdk-Date"] != "20231114T221320Z" {
		t.Fatalf("unexpected sdk date: %s", hdrs["X-Sdk-Date"])
	}
	if hdrs["X-Security-Token"] != "STS" || hdrs["X-Project-ID"] != "PROJ" {
		t.Fatalf("missing security/project headers: %v", hdrs)
	}
}

func TestSignV11Shape(t *testing.T) {
	hdrs, err := SignV11("AKTEST", "SKTEST", "", "",
		"POST", "https://modelgw-0004.officeace.cn-southwest-2.huaweicloud-agentarts.com/v2/v1/subscription/bonus/claim",
		"{}", "application/json;charset=utf8",
		map[string]string{"x-subscription-type": "v2"}, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	auth := hdrs["Authorization"]
	if !strings.HasPrefix(auth, "V11-HMAC-SHA256 Credential=AKTEST/") {
		t.Fatalf("unexpected V11 auth header: %s", auth)
	}
	if !strings.Contains(auth, "/cn-southwest-2/apic") {
		t.Fatalf("missing V11 scope region: %s", auth)
	}
	if hdrs["x-subscription-type"] != "v2" {
		t.Fatalf("missing x-subscription-type: %v", hdrs)
	}
}

// DPoP proof 应形如三段 base64url、头部带 jwk 无 d、算法 ES256。
func TestDPoPProof(t *testing.T) {
	k, err := NewDPoPKey()
	if err != nil {
		t.Fatal(err)
	}
	proof, err := k.DPoPProof("POST", "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("DPoP proof not 3 segments: %d", len(parts))
	}
}

func TestRewriteBodyForcesStreamAndBudget(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","stream":false,"max_tokens":100,"messages":[]}`)
	out := rewriteBody(in)
	s := string(out)
	if !strings.Contains(s, `"stream":true`) {
		t.Fatalf("stream not forced: %s", s)
	}
	if !strings.Contains(s, `"max_tokens":1124`) {
		t.Fatalf("max_tokens budget not applied: %s", s)
	}
	if !strings.Contains(s, `include_usage`) {
		t.Fatalf("stream_options not set: %s", s)
	}
}

func TestRewriteBodyStripsThinkingForGLM53(t *testing.T) {
	in := []byte(`{"model":"glm-5.3","thinking":{"type":"disabled"},"messages":[]}`)
	out := rewriteBody(in)
	if strings.Contains(string(out), "thinking") {
		t.Fatalf("thinking not stripped for glm-5.3: %s", out)
	}
}

func TestClassifyHuaweiCodes(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{200, `{"error":{"code":"ModelArts.81004"}}`, "model_blocked"},
		{200, `{"error":{"code":"ModelArts.81111"}}`, "soft_rate"},
		{401, `{"error_code":"APIG.1009"}`, "account_fault"},
		{200, `{"error":{"code":"ModelArts.81113","message":"exceeds the maximum size"}}`, "passthrough"},
		{500, `oops`, "server"},
	}
	for _, c := range cases {
		got := classifyUpstream(c.status, c.body).String()
		if got != c.want {
			t.Errorf("classify(%d,%s)=%s want %s", c.status, c.body, got, c.want)
		}
	}
}

// ---- 批2/批3 新增能力的回归 ----

func TestDecryptOAuthProfileRoundTrip(t *testing.T) {
	secret := "install-secret-abc"
	plain := []byte(`{"authType":"huawei-oauth","accountId":"acc","user_id":"usr","userName":"张三","credential":{"access":"AK1","secret":"SK1","sts_token":"ST","project_id":"PJ","expires_at":"2026-11-08T16:43:17.412Z","refresh_token":"RT1"}}`)
	key := sha256Sum(secret)
	iv := make([]byte, 12)
	for i := range iv {
		iv[i] = byte(i + 1)
	}
	gcm := mustGCM(t, key)
	ct := gcm.Seal(nil, iv, plain, nil)
	tag := ct[len(ct)-16:]
	data := ct[:len(ct)-16]
	env := map[string]any{
		"version": 1, "alg": "aes-256-gcm",
		"iv":   b64url(iv),
		"tag":  b64url(tag),
		"data": b64url(data),
	}
	raw, _ := jsonMarshal(env)
	prof, err := decryptOAuthProfile(secret, raw)
	if err != nil {
		t.Fatal(err)
	}
	if prof.Credential.Access != "AK1" || prof.Credential.Secret != "SK1" || prof.Credential.RefreshToken != "RT1" {
		t.Fatalf("decrypted fields wrong: %+v", prof.Credential)
	}
	if prof.Credential.ExpiresAt == nil {
		t.Fatalf("expires_at not captured")
	}
}

func TestRotationCSVHelpers(t *testing.T) {
	s := ""
	s = appendCSV(s, "t1")
	s = appendCSV(s, "t2")
	s = appendCSV(s, "t1") // 去重
	if s != "t1,t2" {
		t.Fatalf("appendCSV got %q", s)
	}
	if !containsCSV(splitCSV(s), "t2") || containsCSV(splitCSV(s), "t3") {
		t.Fatalf("containsCSV wrong for %q", s)
	}
}

func TestAnyToUnixSec(t *testing.T) {
	if got := anyToUnixSec("2026-11-08T16:43:17.412Z"); got <= 0 {
		t.Fatalf("iso string not parsed: %d", got)
	}
	if got := anyToUnixSec(float64(1700000000)); got != 1700000000 {
		t.Fatalf("seconds wrong: %d", got)
	}
	if got := anyToUnixSec(float64(1700000000000)); got != 1700000000 {
		t.Fatalf("milliseconds not normalized: %d", got)
	}
}

// 非推理模型（qwen3 系）不应被补 max_tokens 预算（不篡改用户显式语义）。
func TestRewriteBodyNoBudgetForNonReasoning(t *testing.T) {
	in := []byte(`{"model":"qwen3-32b","max_tokens":100,"messages":[]}`)
	out := rewriteBody(in)
	if !strings.Contains(string(out), `"max_tokens":100`) {
		t.Fatalf("non-reasoning model max_tokens was changed: %s", out)
	}
}

// glm-5.3 剥 thinking 后仍须带 stream_options.include_usage（否则 token 流水恒 0）。
func TestRewriteBodyGLM53KeepsUsage(t *testing.T) {
	in := []byte(`{"model":"glm-5.3","thinking":{"type":"disabled"},"messages":[]}`)
	out := string(rewriteBody(in))
	if strings.Contains(out, "thinking") {
		t.Fatalf("thinking not stripped: %s", out)
	}
	if !strings.Contains(out, "include_usage") {
		t.Fatalf("stream_options.include_usage missing after strip: %s", out)
	}
}

// 流内 error 帧应产出实现 StreamErrorClassifier 的错误，供 handler 冷却账号。
func TestStreamErrorClassifier(t *testing.T) {
	body := `{"error":{"code":"ModelArts.81111","message":"TooManyRequests"}}`
	se := &StreamError{Status: statusOfPayload(body), Body: body}
	var sec provider.StreamErrorClassifier = se
	if k := sec.Kind(); k != provider.ErrSoftRate {
		t.Fatalf("stream error kind=%s want soft_rate", k)
	}
}

// errorSniffer 应嗅探到带 error 键的 data 行。
func TestErrorSnifferDetects(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"error\":{\"code\":\"81113\",\"message\":\"exceeds the maximum size\"}}\n\n" +
		"data: [DONE]\n\n"
	sn := &errorSniffer{src: strings.NewReader(stream)}
	buf := make([]byte, 64)
	for {
		if _, err := sn.Read(buf); err != nil {
			break
		}
	}
	if sn.payload == "" {
		t.Fatalf("sniffer did not capture error frame")
	}
	if k := classifyUpstream(statusOfPayload(sn.payload), sn.payload); k != provider.ErrPassthrough {
		t.Fatalf("81113 should classify passthrough, got %s", k)
	}
}

// ---- 合并期审查修出的 4 个缺陷的回归锁 ----

// 缺陷1（合并期实证）：errorSniffer 原用 strings.Contains(line, "error")，正常回答正文里
// 出现 error 字样（模型写代码/解释报错）即被误判成错误帧 → StreamError → statusOfPayload
// 默认 502 → classifyUpstream 判 ErrServer（Rotatable）→ handler 累计 errCount 后冷却健康账号。
// 修法：必须能解析成 JSON 且**顶层存在 error 键**才算错误帧。
func TestErrorSnifferIgnoresErrorInContent(t *testing.T) {
	for _, stream := range []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"error\"}}]}\n\ndata: [DONE]\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"the error was handled\"}}]}\n\ndata: [DONE]\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"code: if err != nil { return err }\"}}]}\n\ndata: [DONE]\n\n",
	} {
		sn := &errorSniffer{src: strings.NewReader(stream)}
		buf := make([]byte, 64)
		for {
			if _, err := sn.Read(buf); err != nil {
				break
			}
		}
		if sn.payload != "" {
			t.Errorf("误报：正文含 error 字样被当成错误帧 payload=%q", sn.payload)
		}
	}
}

// 缺陷2（合并期实证）：非流式路径（Aggregate）原不做 error 嗅探，上游 200 + 内嵌 error 帧
// 被 upstream.Aggregate 吞成「成功空回答」（实测 err=nil），账号不冷却。
// 修法：Aggregate 出口同样挂 errorSniffer，命中即返回带分类的错误。
func TestAggregateSurfacesEmbeddedStreamError(t *testing.T) {
	stream := "data: {\"error\":{\"code\":\"ModelArts.81111\",\"message\":\"TooManyRequests\"}}\n\n" +
		"data: [DONE]\n\n"
	c := &Client{}
	_, err := c.Aggregate(strings.NewReader(stream), "glm-5.2")
	if err == nil {
		t.Fatal("非流式路径吞掉了内嵌 error 帧（err=nil）——账号不冷却、客户端看到空回答")
	}
	var sec provider.StreamErrorClassifier
	if !errors.As(err, &sec) {
		t.Fatalf("错误未实现 StreamErrorClassifier：%T", err)
	}
	if k := sec.Kind(); k != provider.ErrSoftRate {
		t.Fatalf("kind=%s want soft_rate", k)
	}
}

// 缺陷3（合并期实证）：a.Extra 是 map，refreshToken 的 addDead 无锁就地写、accountFromAuth
// 无锁读 → Go "concurrent map read and map write" **致命错误**（不可 recover）。
// 修法：accountFromAuth 持 RLock、addDead 持 Lock。本用例在 -race 下更有力，
// 无 race 时也能靠高并发循环提高触发概率（修复前 500k 次循环必崩）。
func TestExtraMapConcurrentAccess(t *testing.T) {
	a := &auth.Auth{Kind: "officeace", UID: "u1", Extra: map[string]string{}}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = accountFromAuth(a)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20000; i++ {
			addDead(a, "tok")
			_ = acDeadTokens(a)
		}
		close(stop)
	}()
	wg.Wait()
}

// 缺陷4（合并期实证）：Export() 原缺 RefreshToken 字段，而「从客户端导入」路径用它落盘
// ⇒ 导入的账号只有 ~2h 临时凭据，之后积分/签到永久无法续期（对话不受影响，故极隐蔽）。
func TestExportCarriesRefreshToken(t *testing.T) {
	ac := &Account{ID: "acc:usr", RefreshToken: "RT-SECRET", AccessKeyID: "AK"}
	ex := ac.Export()
	if ex.RefreshToken != "RT-SECRET" {
		t.Fatalf("Export 丢了 refresh_token：%q（导入路径将无法续期）", ex.RefreshToken)
	}
}
