package glm

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
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

// TestChatStreamRejectsImageModel `/v1/models` 会把 `glm/image` 透出（带
// supports_image_generation 标记），客户端若拿它来聊天必须本地 400 并给出可行动文案——
// 不得静默跑到上游拿回一段普通聊天文本（与 modelscope 的 image_model_not_chat 同口径）。
func TestChatStreamRejectsImageModel(t *testing.T) {
	c := New()
	a := &auth.Auth{AccessToken: "fake-token-for-guard"} // 守卫在出站之前，不会用到它
	for _, model := range []string{"glm/image", "image", "glm/image:moe_53f"} {
		body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"画一只猫"}]}`)
		rc, status, respBody, err := c.ChatStream(a, body)
		if err != nil || rc != nil {
			t.Fatalf("model=%q 应在本地拒（不返回流/error），实际 rc=%v err=%v", model, rc, err)
		}
		if status != http.StatusBadRequest {
			t.Errorf("model=%q status = %d，want 400", model, status)
		}
		var out map[string]any
		if jsonErr := json.Unmarshal(respBody, &out); jsonErr != nil {
			t.Fatalf("model=%q 的拒收体不是 JSON: %v", model, jsonErr)
		}
		e, _ := out["error"].(map[string]any)
		if code, _ := e["code"].(string); code != "image_model_not_chat" {
			t.Errorf("model=%q error.code = %q，want image_model_not_chat", model, code)
		}
		if msg, _ := e["message"].(string); !strings.Contains(msg, "/v1/images/generations") {
			t.Errorf("model=%q 的文案应指向生图端点（用户要知道改走哪里）：%q", model, msg)
		}
	}
	// 反向护栏：普通对话模型不受影响（不存在按名字滥杀）。
	if isImageModelName("chatglm:moe_53f") || isImageModelName("video") {
		t.Error("普通模型被误判为生图模型（会让对话路径静默 400）")
	}
}
