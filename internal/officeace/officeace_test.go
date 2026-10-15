package officeace

import (
	"strings"
	"testing"
	"time"
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
	out := rewriteBody(in, "")
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
	out := rewriteBody(in, "")
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
		{401, `{"error_code":"APIG.1009"}`, "session_dead"},
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
