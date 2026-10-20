// Package imgutil 提供「多参考图拼版 + 图床上传」能力，供 glm 渠道生图使用。
//
// 背景（glm 生图协议 [实证 chatglm-cli docs/生图协议与实操.md]）：
//   - 参考图改图端点 image/reference 是**单图接口**（多字段并列被忽略、数组 400、
//     拼接 10031），且只收**上游服务器可达的公网 URL**（data URI / 境外直连 500）；
//   - 多参考图的唯一可行方案 = 客户端拼成一张图 + 图床托管拿公网直链；
//   - 上游像素上限 ~500w，超限静默异常（拼版按 480w 预算留余量）。
//
// 拼版规则（chatglm-cli 实测有效，照搬）：
//   - 单行左右布局：所有参考图等高缩放、从左到右排列，每张的标注条 referenceN
//     在其图像正下方、且在 outline 框内 ⇒ 标注归属无歧义；
//   - 框与框之间留足空白（UnitGap），标注不会认错邻居；
//   - 单元图高二分搜索最大化：总面积 ≤ 预算，且单图放大 ≤ UpscaleCap；
//   - 方形外框画在单元外侧，不遮任何有效像素。
package imgutil

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/gif" // 注册 gif 解码
	"image/jpeg"
	_ "image/png" // 注册 png 解码
	"math"

	_ "golang.org/x/image/bmp"      // 注册 bmp 解码
	xdraw "golang.org/x/image/draw" // 高质量缩放（CatmullRom）
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
	_ "golang.org/x/image/tiff" // 注册 tiff 解码
	_ "golang.org/x/image/webp" // 注册 webp 解码
)

const (
	// PixelBudget 是拼版结果的像素面积上限（上游 >500w 会静默异常，留余量）。
	PixelBudget = 4_800_000
	// BudgetTarget 实际瞄准的填充比例。
	BudgetTarget = 0.98
	// BarH 是图像下方、框内的标注条高度（含 3x 文字）。
	BarH = 40
	// UnitGap 是两个带框单元之间的留白（标注归属要清晰）。
	UnitGap = 40
	// Margin 是画布外边距。
	Margin = 24
	// FramePad 是 outline 框与单元内容的间距；OutlineW 是线宽。
	FramePad = 6
	OutlineW = 4
	// UpscaleCap 单图最大放大倍数（避免小图被拉大成纯模糊）。
	UpscaleCap = 4.0
	// MinUnitH 单元图高的下限（极小图也不至于缩没）。
	MinUnitH = 64
)

// Placement 是一张参考图的图像区域位置（不含框与标注条）。
type Placement struct {
	Index int // 原始顺序（0-based），标注为 reference{Index+1}
	X, Y  int
	W, H  int
}

type layout struct {
	W, H       int
	Placements []Placement
	UsedPixels int
}

// BestLayout 计算单行左右布局：二分搜索单元图高（max：面积 ≤ 预算、放大 ≤ 上限）。
func BestLayout(imgs []image.Image, budget int) layout {
	hi := math.Inf(1)
	for _, im := range imgs { // 放大上限：任一图超上限即不可行
		hi = math.Min(hi, UpscaleCap*float64(origH(im)))
	}
	lo := 16.0
	if hi < lo {
		hi = lo // 极小图兜底：至少给最小高度
	}
	var best layout
	ok := false
	for lo <= hi {
		mid := (lo + hi) / 2
		if l, feasible := tryLayout(imgs, mid, budget); feasible {
			best, ok = l, true
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if !ok { // 极小图兜底：用下限高度
		best, _ = tryLayout(imgs, math.Max(MinUnitH, hi), budget)
	}
	return best
}

// tryLayout 以单元图高 h 排一行，校验画布面积 ≤ 预算。
func tryLayout(imgs []image.Image, h float64, budget int) (layout, bool) {
	if len(imgs) == 0 || h < 1 {
		return layout{}, false
	}
	uh := int(h)
	bar := BarH
	frameOff := FramePad + OutlineW
	canvasW := 2*Margin + 2*frameOff + UnitGap*(len(imgs)-1)
	canvasH := 2*Margin + 2*frameOff + uh + bar
	for _, im := range imgs {
		canvasW += int(float64(uh) * aspect(im))
	}
	if float64(canvasW)*float64(canvasH) > float64(budget)*BudgetTarget {
		return layout{}, false
	}
	placements := make([]Placement, 0, len(imgs))
	x := Margin + frameOff
	y := Margin + frameOff
	used := 0
	for i, im := range imgs {
		w := int(float64(uh) * aspect(im))
		if w < 1 {
			w = 1
		}
		placements = append(placements, Placement{Index: i, X: x, Y: y, W: w, H: uh})
		used += w * uh
		x += w + UnitGap
	}
	return layout{W: canvasW, H: canvasH, Placements: placements, UsedPixels: used}, true
}

// BuildRefSheet 把多张参考图拼成一张 JPEG：单行左右布局，每张图像正下方（框内）
// 标注 reference1..N，方形外框画在单元外侧。返回 JPEG 字节与最终尺寸。
// 输入是原始图片字节（png/jpeg/gif/webp/bmp/tiff），自动探测格式。
func BuildRefSheet(images [][]byte) ([]byte, int, int, error) {
	if len(images) == 0 {
		return nil, 0, 0, fmt.Errorf("没有参考图")
	}
	imgs := make([]image.Image, 0, len(images))
	for i, raw := range images {
		im, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			return nil, 0, 0, fmt.Errorf("参考图 %d: %w", i+1, err)
		}
		imgs = append(imgs, im)
	}
	lay := BestLayout(imgs, PixelBudget)

	sheet := image.NewRGBA(image.Rect(0, 0, lay.W, lay.H))
	draw.Draw(sheet, sheet.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)

	red := color.RGBA{R: 220, A: 255}
	for _, p := range lay.Placements {
		scaled := scaleTo(imgs[p.Index], p.W, p.H)
		imgRect := image.Rect(p.X, p.Y, p.X+p.W, p.Y+p.H)
		draw.Draw(sheet, imgRect, scaled, scaled.Bounds().Min, draw.Over)
		// 标注条：图像正下方、框内（深灰底白字）
		barRect := image.Rect(p.X, p.Y+p.H, p.X+p.W, p.Y+p.H+BarH)
		draw.Draw(sheet, barRect, image.NewUniform(color.RGBA{R: 40, G: 40, B: 40, A: 255}),
			image.Point{}, draw.Src)
		drawLabel(sheet, fmt.Sprintf("reference%d", p.Index+1),
			(barRect.Min.X+barRect.Max.X)/2, (barRect.Min.Y+barRect.Max.Y)/2)
		// 方形外框：围绕「图像+标注条」单元，画在外侧不遮像素
		unit := image.Rect(p.X, p.Y, p.X+p.W, p.Y+p.H+BarH)
		drawRectOutline(sheet, unit.Inset(-FramePad), OutlineW, red)
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, sheet, &jpeg.Options{Quality: 92}); err != nil {
		return nil, 0, 0, err
	}
	return buf.Bytes(), lay.W, lay.H, nil
}

func aspect(im image.Image) float64 {
	b := im.Bounds()
	if b.Dy() == 0 {
		return 1
	}
	return float64(b.Dx()) / float64(b.Dy())
}

func origH(im image.Image) int { return im.Bounds().Dy() }

// scaleTo 精确缩放到 w×h（布局阶段已保证等比例；CatmullRom 画质优先）。
func scaleTo(im image.Image, w, h int) image.Image {
	b := im.Bounds()
	if b.Dx() == w && b.Dy() == h {
		return im
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), im, b, xdraw.Over, nil)
	return dst
}

// drawRectOutline 方形描边：四条边环带，调用方保证 rect 在内容外侧。
func drawRectOutline(dst *image.RGBA, r image.Rectangle, thickness int, c color.RGBA) {
	for i := 0; i < thickness; i++ {
		top := image.Rect(r.Min.X-i, r.Min.Y-i, r.Max.X+i, r.Min.Y-i+1)
		bot := image.Rect(r.Min.X-i, r.Max.Y+i-1, r.Max.X+i, r.Max.Y+i)
		left := image.Rect(r.Min.X-i, r.Min.Y-i, r.Min.X-i+1, r.Max.Y+i)
		right := image.Rect(r.Max.X+i-1, r.Min.Y-i, r.Max.X+i, r.Max.Y+i)
		for _, rr := range []image.Rectangle{top, bot, left, right} {
			draw.Draw(dst, rr, image.NewUniform(c), image.Point{}, draw.Over)
		}
	}
}

// drawLabel 用 basicfont 渲染文字（3x 放大）后居中贴到 (cx,cy)。
func drawLabel(dst *image.RGBA, text string, cx, cy int) {
	f := basicfont.Face7x13
	d := &font.Drawer{Src: image.NewUniform(color.White), Face: f}
	w := d.MeasureString(text).Ceil()
	tmp := image.NewRGBA(image.Rect(0, 0, w+2, 15))
	d.Dst = tmp
	d.Dot = fixed.P(1, 13)
	d.DrawString(text)

	const k = 3
	scaled := image.NewRGBA(image.Rect(0, 0, tmp.Bounds().Dx()*k, tmp.Bounds().Dy()*k))
	xdraw.NearestNeighbor.Scale(scaled, scaled.Bounds(), tmp, tmp.Bounds(), xdraw.Over, nil)

	dx, dy := cx-scaled.Bounds().Dx()/2, cy-scaled.Bounds().Dy()/2
	draw.Draw(dst, image.Rect(dx, dy, dx+scaled.Bounds().Dx(), dy+scaled.Bounds().Dy()),
		scaled, scaled.Bounds().Min, draw.Over)
}

// EnsureJPEG 把任意已注册格式的图片字节转成 JPEG（已是 JPEG 的原样返回）。
// 用途：参考图托管前的统一转码（图床按 .jpg 语义存储，上游对 webp 支持未实证）。
func EnsureJPEG(raw []byte) ([]byte, error) {
	if len(raw) > 3 && raw[0] == 0xFF && raw[1] == 0xD8 && raw[2] == 0xFF {
		return raw, nil // 已是 JPEG
	}
	im, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, im, &jpeg.Options{Quality: 92}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
