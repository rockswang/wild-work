package officeace

import (
	"encoding/json"
	"testing"
)

// 本文件锁死「导入路径必须复用桌面端档案里的 DPoP 私钥」这一契约（R-6，2026-10-17 定位）。
//
// 为什么关键：refresh.go 的 dpopKey() 是按 `map[string]string` 反序列化落盘 JWK 的。
// 桌面端档案里的 `dpop.privateJwk` 除 kty/crv/x/y/d 之外还带 `ext`（bool）与 `key_ops`（数组）
// —— 直接把整个对象 Marshal 落盘，反序列化会**失败**，dpopKey() 随即静默回落到
// NewDPoPKey() 新建一把密钥；而 DPoP 要求续期与签发使用同一把密钥，
// 结果就是「导入当天可用、临时凭据（~2h）到期后积分/签到又全坏」且没有任何日志。
// 因此 dpopJWKJSON 必须做窄化，且窄化结果必须能被 dpopKeyFromJWK 还原。

// TestDpopJWKJSONFiltersNonStringKeys 真实形状（含 ext/key_ops）必须窄化成可用的 5 键 JWK。
// 反向验证（已做）：把 dpopJWKJSON 改成直接 `json.Marshal(in)` 返回 → 本用例在
// `json.Unmarshal(..., &map[string]string)` 处失败，报 "cannot unmarshal bool into ... string"。
func TestDpopJWKJSONFiltersNonStringKeys(t *testing.T) {
	k, err := NewDPoPKey()
	if err != nil {
		t.Fatal(err)
	}
	jwk := k.privateJWK() // map[string]string{kty,crv,x,y,d}
	in := map[string]any{
		"kty": jwk["kty"], "crv": jwk["crv"],
		"x": jwk["x"], "y": jwk["y"], "d": jwk["d"],
		// 桌面端档案实测存在的两个非字符串字段
		"ext":     true,
		"key_ops": []any{"sign"},
	}

	got := dpopJWKJSON(in)
	if got == "" {
		t.Fatal("dpopJWKJSON 返回空串；期望得到可用的 JWK")
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("落盘 JWK 无法按 map[string]string 解析（真实后果是 dpopKey 静默新建密钥、续期被拒）：%v", err)
	}
	if len(m) != 5 {
		t.Errorf("期望恰好 5 个键（kty/crv/x/y/d），实际 %d 个：%v", len(m), m)
	}
	for _, key := range []string{"ext", "key_ops"} {
		if _, exists := m[key]; exists {
			t.Errorf("非字符串字段 %q 未被过滤掉", key)
		}
	}
	back, err := dpopKeyFromJWK(m)
	if err != nil {
		t.Fatalf("dpopKeyFromJWK 无法还原窄化后的 JWK：%v", err)
	}
	if back.Private.D.Cmp(k.Private.D) != 0 || back.Private.X.Cmp(k.Private.X) != 0 || back.Private.Y.Cmp(k.Private.Y) != 0 {
		t.Error("还原出的私钥与档案里的原始密钥不一致（续期会被 STS 拒绝）")
	}
}

// TestDpopJWKJSONRequiresPrivateComponents 缺 x/y/d 任一项时返回空串（宁可不填）。
func TestDpopJWKJSONRequiresPrivateComponents(t *testing.T) {
	full := map[string]any{"kty": "EC", "crv": "P-256", "x": "xx", "y": "yy", "d": "dd"}
	if dpopJWKJSON(full) == "" {
		t.Fatal("完整 JWK 不应返回空串")
	}
	for _, missing := range []string{"x", "y", "d"} {
		in := map[string]any{}
		for k, v := range full {
			if k != missing {
				in[k] = v
			}
		}
		if got := dpopJWKJSON(in); got != "" {
			t.Errorf("缺 %q 时应返回空串，实际得到 %q", missing, got)
		}
	}
	if got := dpopJWKJSON(nil); got != "" {
		t.Errorf("nil 输入应返回空串，实际得到 %q", got)
	}
	// 非字符串分量（上游形状突变）同样不得落成坏 JWK
	if got := dpopJWKJSON(map[string]any{"kty": "EC", "x": 1, "y": "yy", "d": "dd"}); got != "" {
		t.Errorf("非字符串 x 应返回空串，实际得到 %q", got)
	}
}

// TestDpopJWKJSONRoundTrip 正常形状（无多余字段）必须原样可用。
func TestDpopJWKJSONRoundTrip(t *testing.T) {
	k, err := NewDPoPKey()
	if err != nil {
		t.Fatal(err)
	}
	in := map[string]any{}
	for kk, vv := range k.privateJWK() {
		in[kk] = vv
	}
	got := dpopJWKJSON(in)
	var m map[string]string
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("标准 JWK 解析失败：%v", err)
	}
	back, err := dpopKeyFromJWK(m)
	if err != nil {
		t.Fatalf("标准 JWK 还原失败：%v", err)
	}
	if back.Private.D.Cmp(k.Private.D) != 0 {
		t.Error("往返后私钥不一致")
	}
}
