package provider

import "testing"

// TestModelscopePublicModelIDKeepsUpstreamSlash D3：魔搭模型名**自带斜杠**
// （`Qwen/Qwen-Image`），且两个渠道**不注册** ModelNamespaces ⇒ 对外 ID 一律
// `modelscope/Qwen/Qwen-Image`、`modelscopeai/Qwen/Qwen-Image`，上游斜杠原样保留。
//
// 若哪天有人把 modelscope 塞进 ModelNamespaces，对外 ID 会变成裸上游名
// （`Qwen/Qwen-Image`）——那既丢了渠道前缀也反解不回渠道，这条断言会立刻变红。
func TestModelscopePublicModelIDKeepsUpstreamSlash(t *testing.T) {
	for _, k := range []Kind{ModelScope, ModelScopeAI} {
		const up = "Qwen/Qwen-Image"
		if got, want := PublicModelID(k, up), k.String()+"/"+up; got != want {
			t.Errorf("PublicModelID(%s, %q) = %q，期望 %q", k, up, got, want)
		}
		if _, ok := ModelNamespaces[k.String()]; ok {
			t.Errorf("%s 不得注册进 ModelNamespaces（对外 ID 必须带渠道前缀，D3）", k)
		}
	}
	// 与既有命名空间渠道（cline）互不干扰。
	if got := PublicModelID(ModelScope, "Qwen/Qwen-Image"); got != "modelscope/Qwen/Qwen-Image" {
		t.Errorf("相邻命名空间误伤：%q", got)
	}
}

// TestSplitModelIDModelscope 反解必须回到「上游模型名」（第二返回值即发给上游的串，
// 渠道内不得自己剥前缀）。
func TestSplitModelIDModelscope(t *testing.T) {
	cases := []struct {
		in   string
		kind Kind
		up   string
	}{
		{"modelscope/Qwen/Qwen-Image", ModelScope, "Qwen/Qwen-Image"},
		{"modelscopeai/Qwen/Qwen-Image-Edit", ModelScopeAI, "Qwen/Qwen-Image-Edit"},
		{"modelscope/deepseek-ai/DeepSeek-V3.2", ModelScope, "deepseek-ai/DeepSeek-V3.2"},
	}
	for _, c := range cases {
		k, up, ok := SplitModelID(c.in)
		if !ok || k != c.kind || up != c.up {
			t.Errorf("SplitModelID(%q) = (%q, %q, %v)，期望 (%q, %q, true)", c.in, k, up, ok, c.kind, c.up)
		}
		// 规范 ID / 上游名与拼接规则必须自洽（同一模型不得在统计里裂成两行）。
		if got := CanonicalModelID(c.in); got != c.in {
			t.Errorf("CanonicalModelID(%q) = %q，期望原样（幂等）", c.in, got)
		}
		if got := UpstreamModelOf(c.in); got != c.up {
			t.Errorf("UpstreamModelOf(%q) = %q，期望 %q", c.in, got, c.up)
		}
	}
}

// TestModelscopeBuiltinKindsAndDisplayOrder 两个 kind 必须进唯一真源
// （builtinKindList ⇒ BuiltinKinds / DisplayNames / ChannelDisplayOrder 全由它派生），
// 展示序**不手写**：ModelScope 是 ModelScopeAI 的前缀，字节序下必定相邻且在前。
func TestModelscopeBuiltinKindsAndDisplayOrder(t *testing.T) {
	builtin := BuiltinKinds()
	for k, name := range map[Kind]string{ModelScope: "ModelScope", ModelScopeAI: "ModelScopeAI"} {
		if !builtin[k.String()] {
			t.Errorf("%s 未登记进 builtinKindList（自定义渠道会占用该名，两层指向不同 Runtime）", k)
		}
		if got := DisplayName(k); got != name {
			t.Errorf("DisplayName(%s) = %q，期望 %q", k, got, name)
		}
	}

	idx := func(k Kind) int {
		for i, kk := range ChannelDisplayOrder {
			if kk == k {
				return i
			}
		}
		return -1
	}
	iCN, iAI := idx(ModelScope), idx(ModelScopeAI)
	if iCN < 0 || iAI < 0 {
		t.Fatalf("ChannelDisplayOrder 缺项：modelscope=%d modelscopeai=%d", iCN, iAI)
	}
	if iAI != iCN+1 {
		t.Errorf("展示序应按显示名字节序派生：ModelScope 在下标 %d、ModelScopeAI 在下标 %d（期望相邻且后者在后）", iCN, iAI)
	}
}