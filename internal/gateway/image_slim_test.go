// image_slim_test.go 回归 issue #176：转发前把过大的内联图片降采样 + 重编码。
//
// 背景：R113 修好了「图片被 stringify 成 base64 文本」的 token 爆炸，但**字节**没解决。
// 一个 Codex 会话累积 12 张 2048px 截图 = 33.01MiB base64（文本仅 0.43MiB），
// 请求体 33.44MiB 撞 32MiB 硬限回 413。这里锁住四条不变量：
//  1. 超大 PNG 被缩到长边 ≤1568 且重编码为 JPEG，体积显著下降；
//  2. 小图原样返回（不做无谓解码，零行为变化）；
//  3. http(s) 直链与非 base64 data URL 一律不动；
//  4. 解码失败/像素过大时原样返回（绝不制造新故障）。
package gateway

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

// makeNoisyPNG 生成一张 w×h 的「类截图」PNG：带噪点，压缩率接近真实截图，
// 保证体积足够大以触发瘦身路径（纯色图 PNG 压缩后会小到不触发）。
func makeNoisyPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	seed := uint32(2463534242)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			seed ^= seed << 13
			seed ^= seed >> 17
			seed ^= seed << 5
			img.Set(x, y, color.RGBA{R: uint8(seed), G: uint8(seed >> 8), B: uint8(seed >> 16), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func dataURLOf(mime string, raw []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

// TestSlimLargeImageDownscaled 大图必须被缩小并重编码，体积显著下降。
func TestSlimLargeImageDownscaled(t *testing.T) {
	raw := makeNoisyPNG(t, 2048, 1151)
	in := dataURLOf("image/png", raw)
	if len(in) <= imageSlimMinBytes*4/3 {
		t.Fatalf("测试图太小（%d 字节），无法覆盖瘦身路径", len(in))
	}
	out := slimDataURLImage(in)
	if out == in {
		t.Fatal("大图应被瘦身，实际原样返回")
	}
	if !strings.HasPrefix(out, "data:image/jpeg;base64,") {
		t.Fatalf("不透明大图应重编码为 JPEG，实际前缀 %q", out[:40])
	}
	payload, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(out, "data:image/jpeg;base64,"))
	if err != nil {
		t.Fatalf("输出不是合法 base64: %v", err)
	}
	if len(payload) >= len(raw) {
		t.Fatalf("瘦身后应更小：%d >= %d", len(payload), len(raw))
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("输出不是可解码图片: %v", err)
	}
	if cfg.Width > imageMaxLongEdge || cfg.Height > imageMaxLongEdge {
		t.Fatalf("长边应 <= %d，实际 %dx%d", imageMaxLongEdge, cfg.Width, cfg.Height)
	}
	// 体积应显著下降（真实截图实测约 1/12；这里给宽松门槛 1/3）
	if len(payload)*3 > len(raw) {
		t.Fatalf("瘦身幅度不足：%d 字节（原 %d）", len(payload), len(raw))
	}
}

// TestSlimSmallImageUntouched 小图原样返回。
func TestSlimSmallImageUntouched(t *testing.T) {
	in := dataURLOf("image/png", makeNoisyPNG(t, 64, 64))
	if got := slimDataURLImage(in); got != in {
		t.Fatal("小图不应被改动")
	}
}

// TestSlimNonDataURLUntouched http(s) 直链与普通字符串一律不动。
func TestSlimNonDataURLUntouched(t *testing.T) {
	for _, s := range []string{
		"https://example.com/a.png",
		"data:image/svg+xml;utf8,<svg/>",
		"not a url",
		"",
	} {
		if got := slimDataURLImage(s); got != s {
			t.Fatalf("%q 不应被改动，实际 %q", s, got)
		}
	}
}

// TestSlimCorruptBase64Untouched 解码失败时原样返回，绝不 panic。
func TestSlimCorruptBase64Untouched(t *testing.T) {
	in := "data:image/png;base64," + strings.Repeat("!!!!", 300000)
	if got := slimDataURLImage(in); got != in {
		t.Fatal("非法 base64 应原样返回")
	}
}

// TestImageURLBlockKeepsDetail 块形态与 detail 透传保持不变（R113 行为不回归）。
func TestImageURLBlockKeepsDetail(t *testing.T) {
	blk := imageURLBlock("https://example.com/a.png", "high")
	if blk["type"] != "image_url" {
		t.Fatalf("type=%v", blk["type"])
	}
	img, _ := blk["image_url"].(map[string]any)
	if img["url"] != "https://example.com/a.png" || img["detail"] != "high" {
		t.Fatalf("image_url=%v", img)
	}
	blk2 := imageURLBlock("https://example.com/a.png", "")
	if _, has := blk2["image_url"].(map[string]any)["detail"]; has {
		t.Fatal("detail 为空时不应写入该字段")
	}
}

// TestSlimCacheReuse 同一张图第二次调用走缓存，结果一致。
func TestSlimCacheReuse(t *testing.T) {
	in := dataURLOf("image/png", makeNoisyPNG(t, 1600, 1000))
	a := slimDataURLImage(in)
	b := slimDataURLImage(in)
	if a != b {
		t.Fatal("缓存前后结果应一致")
	}
}
