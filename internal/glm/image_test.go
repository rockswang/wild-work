package glm

import (
	"errors"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// TestRatioOfSize glm 侧的 size→aspect_ratio 换算。取值域刻意与 server 层
// ratioBySize（校验表）逐项一致——两处是真源分离的（handler 只判「能不能生图」，
// 渠道自己换算），漏一格会让该尺寸静默降级 1:1 而客户端无从察觉。
func TestRatioOfSize(t *testing.T) {
	cases := map[string]string{
		"1024x1024": "1:1",
		"864x1152":  "3:4",
		"1152x864":  "4:3",
		"720x1280":  "9:16",
		"1280x720":  "16:9",
		"auto":      "1:1",
		"1:1":       "1:1",
		"3:4":       "3:4",
		"4:3":       "4:3",
		"9:16":      "9:16",
		"16:9":      "16:9",
		"":          "1:1",
		" 1280x720": "16:9",
		"2048x2048": "1:1", // 非五档：handler 已 400，这里只兜底
	}
	for size, want := range cases {
		if got := ratioOfSize(size); got != want {
			t.Errorf("ratioOfSize(%q) = %q, want %q", size, got, want)
		}
	}
}

// TestEditImageRequiresReference EditImage 无参考图时本地即拒，且必须是
// *provider.Error（handler 靠 errors.As 取 Kind 决定惩罚），不得打上游。
func TestEditImageRequiresReference(t *testing.T) {
	c := NewWithBase("http://127.0.0.1:1") // 不可达：本用例不应发出任何请求
	_, err := c.EditImage(&auth.Auth{UID: "u1"}, provider.ImageRequest{Prompt: "x"})
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v, want *provider.Error", err)
	}
	if pe.Kind != provider.ErrBadParams {
		t.Errorf("Kind = %v, want ErrBadParams（请求级错误，不罚号）", pe.Kind)
	}
}
