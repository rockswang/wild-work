// vision_test.go 「CodeArts 渠道支持图片输入」与模态能力声明的回归测试（R169）。
//
// 修复要点（2026-11-18 实测结论）：
//  1. 出站 content 不再一刀切丢弃非 text 块：text + image_url 保留，
//     Responses 的 input_image 归一成 Chat 的 image_url，audio/file 仍丢弃；
//  2. 图片块 url 为空必须丢弃（上游会 400），不得原样透传；
//  3. 纯文本请求的形状**零变化**（历史会话不能因这次改动重新计 token）；
//  4. 模态能力靠实测表标定：上游目录不下发该字段，静态表 + measured 表取并集，
//     实测否定的模型不得被静态表误标。
package codearts

import (
	"encoding/json"
	"testing"
)

// decodeContent 把 buildChatBody 的输出解回 []any，便于逐块断言。
func decodeContent(t *testing.T, content any) []any {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":    "kimi-k3.1-pro",
		"stream":   false,
		"messages": []any{map[string]any{"role": "user", "content": content}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out, err := buildChatBody(body, "kimi-k3.1-pro", "sid")
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	var decoded struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(decoded.Messages))
	}
	blocks, ok := decoded.Messages[0].Content.([]any)
	if !ok {
		t.Fatalf("content is %T, want []any", decoded.Messages[0].Content)
	}
	return blocks
}

// TestNormalizeKeepsTextAndImage 图片块必须活到出站请求里。
func TestNormalizeKeepsTextAndImage(t *testing.T) {
	blocks := decodeContent(t, []any{
		map[string]any{"type": "text", "text": "describe"},
		map[string]any{"type": "image_url", "image_url": map[string]any{
			"url": "data:image/png;base64,iVBORw0KGgo=",
		}},
	})
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 (text + image_url)", len(blocks))
	}
	if got := blocks[0].(map[string]any)["type"]; got != "text" {
		t.Errorf("blocks[0].type = %v, want text", got)
	}
	img, ok := blocks[1].(map[string]any)
	if !ok {
		t.Fatalf("blocks[1] is %T", blocks[1])
	}
	if got := img["type"]; got != "image_url" {
		t.Errorf("blocks[1].type = %v, want image_url", got)
	}
	inner, ok := img["image_url"].(map[string]any)
	if !ok {
		t.Fatalf("image_url is %T", img["image_url"])
	}
	if inner["url"] != "data:image/png;base64,iVBORw0KGgo=" {
		t.Errorf("url = %v", inner["url"])
	}
}

// TestNormalizeDropsNonVisonBlocks audio/file 之类仍要丢弃（上游不认）。
func TestNormalizeDropsNonVisionBlocks(t *testing.T) {
	blocks := decodeContent(t, []any{
		map[string]any{"type": "text", "text": "hi"},
		map[string]any{"type": "audio", "audio": map[string]any{"data": "xxx"}},
		map[string]any{"type": "file", "file": map[string]any{"file_id": "f1"}},
	})
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1 (audio/file 必须被丢弃)", len(blocks))
	}
	if got := blocks[0].(map[string]any)["type"]; got != "text" {
		t.Errorf("blocks[0].type = %v", got)
	}
}

// TestNormalizeInputImageConverts Responses 形状必须归一成 Chat 的 image_url。
func TestNormalizeInputImageConverts(t *testing.T) {
	blocks := decodeContent(t, []any{
		map[string]any{"type": "input_image", "image_url": "data:image/jpeg;base64,AAA", "detail": "high"},
	})
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	img := blocks[0].(map[string]any)
	if got := img["type"]; got != "image_url" {
		t.Fatalf("type = %v, want image_url", got)
	}
	inner := img["image_url"].(map[string]any)
	if inner["url"] != "data:image/jpeg;base64,AAA" {
		t.Errorf("url = %v", inner["url"])
	}
	if inner["detail"] != "high" {
		t.Errorf("detail = %v, want high（必须透传）", inner["detail"])
	}
}

// TestNormalizeImageURLStringForm 宽松形态（image_url 直接是字符串）也要认。
func TestNormalizeImageURLStringForm(t *testing.T) {
	blocks := decodeContent(t, []any{
		map[string]any{"type": "image_url", "image_url": "https://example.com/a.png"},
	})
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1", len(blocks))
	}
	inner := blocks[0].(map[string]any)["image_url"].(map[string]any)
	if inner["url"] != "https://example.com/a.png" {
		t.Errorf("url = %v", inner["url"])
	}
}

// TestNormalizeDropsEmptyImageURL 空 url 的图片块必须丢弃（透传会让上游 400）。
func TestNormalizeDropsEmptyImageURL(t *testing.T) {
	cases := []map[string]any{
		{"type": "image_url", "image_url": map[string]any{"url": ""}},
		{"type": "image_url", "image_url": map[string]any{}},
		{"type": "image_url", "image_url": "   "},
		{"type": "input_image", "image_url": ""},
		{"type": "input_image"},
	}
	for i, c := range cases {
		blocks := decodeContent(t, []any{
			map[string]any{"type": "text", "text": "keep"},
			c,
		})
		if len(blocks) != 1 {
			t.Errorf("case %d: blocks = %d, want 1（空 url 必须丢弃）", i, len(blocks))
		}
	}
}

// TestNormalizePlainTextUnchanged 纯文本形状零变化（历史会话不得重新计 token）。
func TestNormalizePlainTextUnchanged(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"model":  "kimi-k3.1-pro",
		"stream": false,
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := buildChatBody(body, "kimi-k3.1-pro", "sid")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if s, ok := got.Messages[0].Content.(string); !ok || s != "hello" {
		t.Fatalf("content = %#v, want 裸字符串 hello（必须保持 string 形态）", got.Messages[0].Content)
	}
}

// TestMeasuredVisionCapabilities 锁定实测能力表（R169）。
//
// 上游目录不下发模态字段，这组断言就是「谁能识图」的唯一真源；
// 任何一侧被误改都必须当场变红。
func TestMeasuredVisionCapabilities(t *testing.T) {
	supported := []string{"kimi-k3.1-pro", "kimi-k3", "glm-5.3-flash", "deepseek-v4.1-flash"}
	for _, id := range supported {
		if !measuredVisionModels[id] {
			t.Errorf("%s 应标记为可识图（实测 ptok 上升且答对图内内容）", id)
		}
		if !modelSupportsImages(id, false) {
			t.Errorf("modelSupportsImages(%q) = false, want true（实测表必须压过静态缺省）", id)
		}
	}

	// 实测明确拒绝图片的模型：即使静态表误标也必须为 false。
	rejected := []string{"glm-5.3", "GLM-5.2", "openpangu-2.0-flash", "openpangu-2.1-pro"}
	for _, id := range rejected {
		if measuredNoVisionModels[id] != true {
			t.Errorf("%s 应标记为不可识图（上游回 406 not multimodal）", id)
		}
		if modelSupportsImages(id, true) {
			t.Errorf("modelSupportsImages(%q, true) = true, want false（否定表必须压过静态表误标）", id)
		}
	}

	// 未测过的模型回落到静态标记，不得凭空判定。
	if modelSupportsImages("some-new-model", false) {
		t.Error("未实测模型不得默认为可识图")
	}
	if !modelSupportsImages("some-static-vision", true) {
		t.Error("静态表标了 SupportsImages 时应透传")
	}
}

// TestStaticCatalogContextWindows staticCatalog 的容量必须与上游目录一致。
//
// 2026-11-18 取自上游模型目录：三档 300k/600k/1M 的取默认档 300000；
// 单档 200k 的取 200000。写死这组值是为了防止有人拍脑袋拔高容量。
func TestStaticCatalogContextWindows(t *testing.T) {
	want := map[string]int64{
		"kimi-k3.1-pro":       300000,
		"kimi-k3":             300000,
		"glm-5.3-flash":       300000,
		"glm-5.3":             300000,
		"GLM-5.2":             300000,
		"deepseek-v4.1-flash": 300000,
		"deepseek-v4-pro":     300000,
		"GLM-5.1":             200000,
		"GLM-5":               200000,
		"openpangu-2.1-pro":   200000,
	}
	got := map[string]int64{}
	for _, e := range staticCatalog {
		got[e.ID] = e.ContextWindow
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s ContextWindow = %d, want %d", id, got[id], w)
		}
	}
}
