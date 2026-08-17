// genicon 生成构建资产：托盘图标 PNG 与 Windows 可执行文件图标 ICO。
// 用法：go run ./cmd/genicon （在项目根目录执行，输出到 build/ 与 build/windows/）
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

const size = 256

// drawIcon 画一个深蓝圆角方块 + 白色 "W"，返回 RGBA 图像。
// W 用四笔粗线段绘制（左竖、中左斜、中右斜、右竖），笔画粗壮、间隙大，
// 缩到 16/32px 托盘尺寸后仍能看出 W（避免缩小时中缝糊成 V）。
func drawIcon() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{0x1f, 0x6f, 0xeb, 0xff} // 品牌蓝
	white := color.RGBA{0xff, 0xff, 0xff, 0xff}

	// 圆角矩形背景（角落透明）
	radius := size / 5
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if rounded(x, y, radius) {
				img.SetRGBA(x, y, bg)
			}
		}
	}

	// 白色 "W"：四笔粗线段（宽 30），笔画分布均匀、中缝清晰
	thick := 30.0
	segments := [][4]float64{
		{62, 70, 62, 190},   // 左竖
		{62, 78, 128, 190},  // 中左斜（左 V 的右斜笔）
		{128, 190, 194, 78}, // 中右斜（右 V 的左斜笔）
		{194, 70, 194, 190}, // 右竖
	}
	for _, s := range segments {
		drawSegment(img, s[0], s[1], s[2], s[3], thick, white)
	}
	return img
}

func rounded(x, y, r int) bool {
	if x >= r && x < size-r || y >= r && y < size-r {
		return true
	}
	// 判断是否落在圆角内：四个角的圆心圆外剔除
	cx, cy := 0, 0
	switch {
	case x < r && y < r:
		cx, cy = r, r
	case x >= size-r && y < r:
		cx, cy = size-r-1, r
	case x < r && y >= size-r:
		cx, cy = r, size-r-1
	case x >= size-r && y >= size-r:
		cx, cy = size-r-1, size-r-1
	default:
		return true
	}
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}

func drawPolyline(img *image.RGBA, pts []float64, w float64, c color.RGBA) {
	for i := 0; i+3 < len(pts); i += 2 {
		x1, y1, x2, y2 := pts[i], pts[i+1], pts[i+2], pts[i+3]
		drawSegment(img, x1, y1, x2, y2, w, c)
	}
}

// drawSegment 粗线段（逐步采样画圆）。
func drawSegment(img *image.RGBA, x1, y1, x2, y2, w float64, c color.RGBA) {
	steps := int(maxi(abs(x2-x1), abs(y2-y1))) * 2
	dx, dy := (x2-x1)/float64(steps), (y2-y1)/float64(steps)
	radius := w / 2
	for i := 0; i <= steps; i++ {
		cx, cy := x1+dx*float64(i), y1+dy*float64(i)
		for oy := -int(radius); oy <= int(radius); oy++ {
			for ox := -int(radius); ox <= int(radius); ox++ {
				px, py := int(cx)+ox, int(cy)+oy
				if px < 0 || py < 0 || px >= size || py >= size {
					continue
				}
				if float64(ox*ox+oy*oy) <= radius*radius {
					img.SetRGBA(px, py, c)
				}
			}
		}
	}
}

func maxi(vals ...float64) float64 {
	m := vals[0]
	for _, v := range vals[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// writeICO 把 PNG 打包成 ICO（PNG 压缩条目，Vista+ 可用）。
func writeICO(path string, pngData []byte) error {
	// ICO 头：reserved=0, type=1, count=1
	// 目录项：w/h(0=256), colorCount=0, reserved=0, planes=1, bitCount=32, size, offset=22
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint8(0))  // 256px
	_ = binary.Write(&buf, binary.LittleEndian, uint8(0))  // 256px
	_ = binary.Write(&buf, binary.LittleEndian, uint8(0))  // palette
	_ = binary.Write(&buf, binary.LittleEndian, uint8(0))  // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // planes
	_ = binary.Write(&buf, binary.LittleEndian, uint16(32))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(pngData)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(22))
	_, _ = buf.Write(pngData)
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func main() {
	root, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	trayDir := filepath.Join(root, "build")
	icoDir := filepath.Join(root, "build", "windows")
	_ = os.MkdirAll(trayDir, 0o755)
	_ = os.MkdirAll(icoDir, 0o755)

	img := drawIcon()

	// 托盘图标 32x32 PNG + ICO（Windows 托盘需要 .ico 内容）
	tray32 := image.NewRGBA(image.Rect(0, 0, 32, 32))
	scale := float64(size) / 32
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			sx, sy := int(float64(x)*scale), int(float64(y)*scale)
			tray32.SetRGBA(x, y, img.RGBAAt(sx, sy))
		}
	}
	trayFP := filepath.Join(trayDir, "trayicon.png")
	f, _ := os.Create(trayFP)
	_ = png.Encode(f, tray32)
	_ = f.Close()

	var trayPngBuf bytes.Buffer
	_ = png.Encode(&trayPngBuf, tray32)
	trayIcoFP := filepath.Join(trayDir, "trayicon.ico")
	if err := writeICO(trayIcoFP, trayPngBuf.Bytes()); err != nil {
		panic(err)
	}

	// exe 图标 256x256 ICO
	var pngBuf bytes.Buffer
	_ = png.Encode(&pngBuf, img)
	icoFP := filepath.Join(icoDir, "icon.ico")
	if err := writeICO(icoFP, pngBuf.Bytes()); err != nil {
		panic(err)
	}
	fmt.Printf("generated: %s, %s, %s\n", trayFP, trayIcoFP, icoFP)
}
