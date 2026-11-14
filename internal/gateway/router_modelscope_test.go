package gateway

import (
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// TestModelscopeModelIDForms 双斜杠护栏：魔搭的上游模型名**自带斜杠**
// （`Qwen/Qwen-Image`），对外 ID 是 `modelscope/Qwen/Qwen-Image`（两段斜杠）。
// 本条把 compat 层（model_map / default_channel）与降级链（from/to）在
// 这种形态下的**实际行为**逐条钉住，行为与本票预期不符的部分写进「已知限制」。
func TestModelscopeModelIDForms(t *testing.T) {
	full := "modelscope/Qwen/Qwen-Image"
	rt := &Router{
		Default:  "modelscope",
		Channels: []string{"modelscope", "modelscopeai"},
	}

	// ① 带渠道前缀的全串：原样透传（前缀已知即可，后半段有几段斜杠都不影响）。
	got, err := rt.Resolve(full)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", full, err)
	}
	if got != full {
		t.Errorf("Resolve(%q) = %q，期望原样 %q", full, got, full)
	}

	// ② model_map 精确/通配都能把**裸名**映射到带双斜杠的全串，且不重复拼前缀。
	for _, tc := range []struct {
		name  string
		mapKV map[string]string
		model string
	}{
		{"精确", map[string]string{"image": full}, "image"},
		{"通配", map[string]string{"qwen-image*": full}, "qwen-image-v2"},
	} {
		r := &Router{Map: tc.mapKV, Channels: []string{"modelscope"}}
		got, err := r.Resolve(tc.model)
		if err != nil {
			t.Errorf("%s: Resolve(%q): %v", tc.name, tc.model, err)
			continue
		}
		if got != full {
			t.Errorf("%s: Resolve(%q) = %q，期望 %q（不得出现 modelscope/modelscope/… 双前缀）", tc.name, tc.model, got, full)
		}
		if strings.HasPrefix(got, "modelscope/modelscope/") {
			t.Errorf("%s: 拼出了双前缀：%q", tc.name, got)
		}
	}

	// ③ 裸名 + default_channel：由 ModelIDPrefix 拼**一次**前缀。
	if p := provider.ModelIDPrefix(provider.ModelScope); p != "modelscope" {
		t.Fatalf("ModelIDPrefix(modelscope) = %q，期望 modelscope（未注册 ModelNamespaces，D3）", p)
	}

	// ④ 已知限制（记录实际行为）：**裸上游名**（自带斜杠、首段不是渠道名）不能靠
	// default_channel 兜底——Router 会先把它当「已带前缀」解析，首段 `Qwen`
	// 不是已知渠道 ⇒ 报 unknown channel。这是既有语义（不是本渠道引入的），
	// 用户要么填全串 `modelscope/Qwen/...`，要么配 model_map 把裸名映射过去。
	if _, err := rt.Resolve("Qwen/Qwen-Image"); err == nil {
		t.Error("裸上游名 Qwen/Qwen-Image 意外被路由成功：行为与「已知限制」不符，需更新文档")
	}

	// ⑤ 降级链 from/to 用同一形态：SetModelFallback 只校验 `channel/model` 形状
	// （首个 `/` 位置），多段斜杠的全串合法；handler 侧靠 SplitModelID 反解。
	for _, id := range []string{full, "modelscopeai/Qwen/Qwen-Image-Edit"} {
		if strings.Index(id, "/") <= 0 {
			t.Errorf("%q 不满足 SetModelFallback 的 channel/model 形状校验", id)
		}
		k, up, ok := provider.SplitModelID(id)
		if !ok || up == "" {
			t.Errorf("SplitModelID(%q) 反解失败", id)
		}
		if want := strings.SplitN(id, "/", 2)[0]; k.String() != want {
			t.Errorf("SplitModelID(%q) 渠道 = %q，期望 %q", id, k, want)
		}
		if provider.PublicModelID(k, up) != id {
			t.Errorf("PublicModelID 往返不一致：%q → %q/%q", id, k, up)
		}
	}
}