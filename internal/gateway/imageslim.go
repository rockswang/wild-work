// imageslim.go 转发前把过大的内联图片（data URL）降采样 + 重编码。
//
// 为什么需要（issue #176）：
//
//	R113 修好了「工具结果里的图片被 stringify 成 base64 文本」的 token 爆炸，
//	但**字节**这一维没解决。Codex 是无状态客户端，每轮把整段历史原样重放，
//	截图以 `data:image/png;base64,...` 原样躺在历史里。实测一个会话：
//	12 张 2048px 游戏截图 = 33.01MiB base64，文本只占 0.43MiB，
//	请求体 33.44MiB → 撞上 32MiB 硬限回 413 Payload Too Large，会话直接报废。
//
//	同一张图按 vision 计价只有约 1K token，但 base64 字节数完全取决于编码格式：
//	2048×1151 的 PNG 有 4.55MB，缩到 1568 长边 + JPEG q85 只要 0.38MB（约 1/12），
//	而截图文字仍然清晰可读（实测校对过底部小字）。
//
// 设计取舍：
//   - **只动 data URL**：http(s) 直链不下载、不改写（那是客户端/上游的事）。
//   - **只在超阈值时才解码**：小于 512KB 且长边不超限的图原样放行，避免无谓 CPU。
//   - **按内容哈希缓存**：历史重放会让同一张图每轮再进一次，缓存后每轮只多一次
//     sha256（毫秒级），不必每轮重新解码 4MB PNG。
//   - **失败一律原样返回**：解码不了/体积反而变大/像素数离谱，都退回原串，
//     宁可让上游去处理，也不在这里制造新故障。
package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"sync"

	_ "golang.org/x/image/bmp"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

const (
	// imageMaxLongEdge 转发前把图片长边缩到这个像素以内。
	// 1568 是主流 vision 模型（Claude / OpenAI）推荐的长边上限，再大只是白烧带宽与内存。
	imageMaxLongEdge = 1568
	// imageReencodeQuality JPEG 重编码质量。85 在截图文字可读性与体积之间取平衡
	// （实测 2048×1151 游戏截图 4.55MB → 0.38MB，底部小字仍清晰）。
	imageReencodeQuality = 85
	// imageSlimMinBytes 原图小于此值且长边不超限时原样放行，不做解码重编码。
	imageSlimMinBytes = 512 << 10
	// imageMaxPixels 解码前的像素数上限（防解压炸弹：一张几 KB 的 PNG 可以解出
	// 上亿像素）。超限直接原样返回，不在本层承担 OOM 风险。
	imageMaxPixels = 50_000_000
	// slimCacheMaxEntries 缓存条目上限（按内容哈希）。命中率极高（历史重放），
	// 溢出时整体清空重建，逻辑简单且不会无限吃内存。
	slimCacheMaxEntries = 256
)

// slimCache 缓存「原始 base64 → 瘦身后的 data URL」。
// key 用原始 payload 的 sha256（不含前缀），value 是完整 data URL。
var slimCache = struct {
	sync.Mutex
	m map[string]string
}{m: make(map[string]string)}

// imageURLBlock 构造 Chat 的 image_url 内容块；data URL 图片先按需瘦身。
// detail 为空时不写该字段（与旧行为一致）。
func imageURLBlock(url, detail string) map[string]any {
	img := map[string]any{"url": slimDataURLImage(url)}
	if detail != "" {
		img["detail"] = detail
	}
	return map[string]any{"type": "image_url", "image_url": img}
}

// slimDataURLImage 把 `data:image/...;base64,...` 的图片按需降采样并重编码，
// 返回新的 data URL。非 data URL、非 base64、解析失败、或本就不大且尺寸合规时**原样返回**。
func slimDataURLImage(rawURL string) string {
	if !strings.HasPrefix(rawURL, "data:image/") {
		return rawURL
	}
	comma := strings.IndexByte(rawURL, ',')
	if comma < 0 {
		return rawURL
	}
	meta := rawURL[len("data:"):comma]
	if !strings.Contains(meta, ";base64") {
		return rawURL // 非 base64 的 data URL（如 utf8 编码的 SVG），不动
	}
	payload := rawURL[comma+1:]

	// 缓存：历史重放时同一张图每轮都会进来，命中即免解码。
	sum := sha256.Sum256([]byte(payload))
	key := hex.EncodeToString(sum[:])
	slimCache.Lock()
	if v, ok := slimCache.m[key]; ok {
		slimCache.Unlock()
		return v
	}
	slimCache.Unlock()

	out, ok := slimPayload(payload)
	if !ok {
		return rawURL // 失败/无收益：原样返回，不缓存
	}
	slimCache.Lock()
	if len(slimCache.m) >= slimCacheMaxEntries {
		slimCache.m = make(map[string]string)
	}
	slimCache.m[key] = out
	slimCache.Unlock()
	return out
}

// slimPayload 解码 base64 → 按需缩放 → 重编码，返回新的 data URL。
// 第二个返回值 false 表示「不该替换」（解码失败 / 像素过大 / 无需处理 / 体积没变小）。
//
// 只做一次 base64 解码：先读文件头拿尺寸（DecodeConfig），据此决定是否值得解码整图。
func slimPayload(payload string) (string, bool) {
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", false
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return "", false
	}
	if int64(cfg.Width)*int64(cfg.Height) > imageMaxPixels {
		return "", false // 防解压炸弹
	}
	long := cfg.Width
	if cfg.Height > long {
		long = cfg.Height
	}
	// 快路径：体积不大且长边已合规 ⇒ 无需处理（省掉整图解码与重编码的 CPU）。
	if len(raw) <= imageSlimMinBytes && long <= imageMaxLongEdge {
		return "", false
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", false
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	var dstImg image.Image = img
	if long > imageMaxLongEdge {
		scale := float64(imageMaxLongEdge) / float64(long)
		nw := int(float64(w)*scale + 0.5)
		nh := int(float64(h)*scale + 0.5)
		if nw < 1 {
			nw = 1
		}
		if nh < 1 {
			nh = 1
		}
		rgba := image.NewRGBA(image.Rect(0, 0, nw, nh))
		xdraw.CatmullRom.Scale(rgba, rgba.Bounds(), img, b, xdraw.Over, nil)
		dstImg = rgba
	}

	var buf bytes.Buffer
	var mime string
	if isOpaque(dstImg) {
		// 不透明（绝大多数截图）：JPEG 体积远小于 PNG。
		mime = "image/jpeg"
		if err := jpeg.Encode(&buf, dstImg, &jpeg.Options{Quality: imageReencodeQuality}); err != nil {
			return "", false
		}
	} else {
		// 带透明通道：只能 PNG（转 JPEG 会把透明区涂黑）。用最高压缩。
		mime = "image/png"
		enc := png.Encoder{CompressionLevel: png.BestCompression}
		if err := enc.Encode(&buf, dstImg); err != nil {
			return "", false
		}
	}
	if buf.Len() == 0 || buf.Len() >= len(raw) {
		return "", false // 没变小就别换（避免无收益的格式抖动）
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), true
}

// isOpaque 判断图片是否完全不透明。*image.RGBA / *image.NRGBA / *image.Gray 等
// 标准类型都实现了 Opaque() bool；未实现者按「可能带透明」处理，走 PNG 分支更安全。
func isOpaque(im image.Image) bool {
	if o, ok := im.(interface{ Opaque() bool }); ok {
		return o.Opaque()
	}
	return false
}
