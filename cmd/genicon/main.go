// genicon 从仓库根目录 icon.png 生成构建资产：
//   - build/windows/icon.ico  ：exe 图标（多分辨率 16/32/48/64/128/256，含透明）
//   - build/trayicon.ico      ：托盘图标（同上，主体放大更清晰）
//   - build/logo64.png        ：前端标题栏 logo 源（再内联进 index.html）
// 用法：go run ./cmd/genicon （在项目根目录执行）
//
// 图标源要求：正方形 PNG（建议 >= 512x512，带透明背景），放在仓库根目录 icon.png。
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
)

// 生成 ICO 时包含的档位（从小到大，保证文件管理器/任务栏/托盘各 DPI 均清晰）
var icoSizes = []int{16, 32, 48, 64, 128, 256}

// cropSquare 去掉透明留白，居中裁成正方形（保留 4% 内边距，避免主体贴边）。
func cropSquare(src *image.RGBA) *image.RGBA {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	minX, minY, maxX, maxY := w, h, 0, 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if src.RGBAAt(x, y).A > 60 {
				if x < minX {
					minX = x
				}
				if x > maxX {
					maxX = x
				}
				if y < minY {
					minY = y
				}
				if y > maxY {
					maxY = y
				}
			}
		}
	}
	if maxX < minX {
		return src
	}
	pad := int(float64(maxX-minX) * 0.04)
	minX, minY = maxI(0, minX-pad), maxI(0, minY-pad)
	maxX, maxY = minI(w-1, maxX+pad), minI(h-1, maxY+pad)
	side := maxI(maxX-minX, maxY-minY)
	c := (minX + maxX) / 2
	cx0 := maxI(0, c-side/2)
	return src.SubImage(image.Rect(cx0, cx0, cx0+side, cx0+side)).(*image.RGBA)
}

func maxI(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minI(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// scaleImage 把 src 等比缩放（LANCZOS 近似：最近邻上采样+双线性，这里用 Draw 的默认高质量）填到 dst 正方形。
func scaleImage(src image.Image, dst *image.RGBA) {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	for y := 0; y < dst.Bounds().Dy(); y++ {
		for x := 0; x < dst.Bounds().Dx(); x++ {
			sx := x * w / dst.Bounds().Dx()
			sy := y * h / dst.Bounds().Dy()
			dst.Set(x, y, src.At(sx, sy))
		}
	}
}

// writeICO 把一组 PNG 帧（按尺寸为键）打包成多条目 ICO（PNG 压缩条目，Vista+ 可用）。
func writeICO(path string, frames map[int]*bytes.Buffer) error {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0))         // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))         // type = icon
	_ = binary.Write(&buf, binary.LittleEndian, uint16(len(frames))) // count
	offset := 6 + 16*len(frames)
	for _, sz := range icoSizes {
		fb := frames[sz]
		if fb == nil {
			continue
		}
		data := fb.Bytes()
		if sz == 256 {
			_ = buf.WriteByte(0) // 256 在 ICO 里宽度/高度均记 0
			_ = buf.WriteByte(0)
		} else {
			_ = buf.WriteByte(byte(sz)) // 宽度
			_ = buf.WriteByte(byte(sz)) // 高度
		}
		_ = buf.WriteByte(0)            // palette
		_ = buf.WriteByte(0)            // reserved
		_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // planes
		_ = binary.Write(&buf, binary.LittleEndian, uint16(32)) // bitcount
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(data)))
		_ = binary.Write(&buf, binary.LittleEndian, uint32(offset))
		offset += len(data)
	}
	for _, sz := range icoSizes {
		if fb := frames[sz]; fb != nil {
			buf.Write(fb.Bytes())
		}
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func main() {
	root, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	srcPath := filepath.Join(root, "icon.png")
	srcFile, err := os.Open(srcPath)
	if err != nil {
		panic("找不到 icon.png（需放在仓库根目录）：" + err.Error())
	}
	defer srcFile.Close()
	srcDec, err := png.Decode(srcFile)
	if err != nil {
		panic("icon.png 解码失败：" + err.Error())
	}
	src := image.NewRGBA(srcDec.Bounds())
	for y := 0; y < src.Bounds().Dy(); y++ {
		for x := 0; x < src.Bounds().Dx(); x++ {
			src.Set(x, y, srcDec.At(x, y))
		}
	}

	trayDir := filepath.Join(root, "build")
	icoDir := filepath.Join(root, "build", "windows")
	_ = os.MkdirAll(trayDir, 0o755)
	_ = os.MkdirAll(icoDir, 0o755)

	// 裁掉透明留白，主体居中正方形
	cropped := cropSquare(src)

	// exe 图标：用满幅原图缩放（保留原留白，视觉更自然）
	exeFrames := make(map[int]*bytes.Buffer)
	for _, sz := range icoSizes {
		tmp := image.NewRGBA(image.Rect(0, 0, sz, sz))
		scaleImage(src, tmp)
		var b bytes.Buffer
		_ = png.Encode(&b, tmp)
		exeFrames[sz] = &b
	}
	if err := writeICO(filepath.Join(icoDir, "icon.ico"), exeFrames); err != nil {
		panic(err)
	}

	// 托盘图标：用裁切后的主体放大，小尺寸更清晰
	trayFrames := make(map[int]*bytes.Buffer)
	for _, sz := range icoSizes {
		tmp := image.NewRGBA(image.Rect(0, 0, sz, sz))
		scaleImage(cropped, tmp)
		var b bytes.Buffer
		_ = png.Encode(&b, tmp)
		trayFrames[sz] = &b
	}
	if err := writeICO(filepath.Join(trayDir, "trayicon.ico"), trayFrames); err != nil {
		panic(err)
	}

	// 前端 logo 源（64x64，裁切版）
	logo64 := image.NewRGBA(image.Rect(0, 0, 64, 64))
	scaleImage(cropped, logo64)
	logoFP := filepath.Join(trayDir, "logo64.png")
	lf, _ := os.Create(logoFP)
	_ = png.Encode(lf, logo64)
	_ = lf.Close()

	fmt.Printf("generated: %s, %s, %s\n",
		filepath.Join(icoDir, "icon.ico"),
		filepath.Join(trayDir, "trayicon.ico"),
		logoFP)
}
