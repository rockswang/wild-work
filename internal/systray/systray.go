// Package systray 系统托盘封装：固定菜单（打开主界面 / 查看日志 / 退出）。
// 基于 energye/systray（跨平台 Windows/macOS/Linux），单文件实现，无平台差异代码。
//
// 设计约定（见项目 AGENTS.md R1-R3）：
//   - 菜单固定，不做动态内容，不用定时/事件刷新
//   - 单击/双击托盘 = 打开主界面
package systray

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"log"

	"github.com/energye/systray"
)

// Actions 托盘动作回调（由 daemon 注入）。
type Actions struct {
	// OpenUI 打开主界面（系统浏览器）。
	OpenUI func()
	// OpenLog 打开日志文件。
	OpenLog func()
	// Quit 退出程序。
	Quit func()
}

// miniPNG 生成 16x16 纯色 PNG（RGBA）。
func miniPNG(r, g, b uint8) []byte {
	const w, h = 16, 16
	var buf bytes.Buffer

	// PNG signature
	buf.Write([]byte{137, 80, 78, 71, 13, 10, 26, 10})

	// IHDR
	writeChunk(&buf, "IHDR", func() {
		binary.Write(&buf, binary.BigEndian, int32(w))
		binary.Write(&buf, binary.BigEndian, int32(h))
		buf.WriteByte(8) // bit depth
		buf.WriteByte(6) // color type RGBA
		buf.WriteByte(0) // compression
		buf.WriteByte(0) // filter
		buf.WriteByte(0) // interlace
	})

	// IDAT
	writeChunk(&buf, "IDAT", func() {
		// zlib header
		buf.Write([]byte{0x78, 0x01})
		adler := adler32Start()
		for y := 0; y < h; y++ {
			buf.WriteByte(0) // filter none
			row := []byte{r, g, b, 255}
			for x := 0; x < w; x++ {
				buf.Write(row)
			}
			adler = adler32Update(adler, []byte{0}) // filter byte
			for x := 0; x < w; x++ {
				adler = adler32Update(adler, row)
			}
		}
		// adler32
		binary.Write(&buf, binary.BigEndian, adler)
	})

	// IEND
	writeChunk(&buf, "IEND", nil)

	return buf.Bytes()
}

func miniPNGWithBorder(r, g, b uint8) []byte {
	const w, h = 16, 16
	var buf bytes.Buffer
	buf.Write([]byte{137, 80, 78, 71, 13, 10, 26, 10})

	writeChunk(&buf, "IHDR", func() {
		binary.Write(&buf, binary.BigEndian, int32(w))
		binary.Write(&buf, binary.BigEndian, int32(h))
		buf.WriteByte(8)
		buf.WriteByte(6)
		buf.WriteByte(0)
		buf.WriteByte(0)
		buf.WriteByte(0)
	})

	writeChunk(&buf, "IDAT", func() {
		buf.Write([]byte{0x78, 0x01})
		adler := adler32Start()
		for y := 0; y < h; y++ {
			buf.WriteByte(0)
			for x := 0; x < w; x++ {
				// 浅色边框（四个角留白）
				isBorder := y == 0 || y == h-1 || x == 0 || x == w-1
				if isBorder {
					// 白色边
					row := []byte{255, 255, 255, 255}
					buf.Write(row)
					adler = adler32Update(adler, row)
				} else {
					row := []byte{r, g, b, 255}
					buf.Write(row)
					adler = adler32Update(adler, row)
				}
			}
		}
		binary.Write(&buf, binary.BigEndian, adler)
	})

	writeChunk(&buf, "IEND", nil)
	return buf.Bytes()
}

func writeChunk(buf *bytes.Buffer, name string, writeData func()) {
	var data bytes.Buffer
	if writeData != nil {
		writeData()
	}
	binary.Write(buf, binary.BigEndian, uint32(data.Len()))
	start := buf.Len()
	buf.Write([]byte(name))
	buf.Write(data.Bytes())
	crc := crc32.ChecksumIEEE(buf.Bytes()[start:])
	binary.Write(buf, binary.BigEndian, crc)
}

func adler32Start() uint32 { return 1 }
func adler32Update(a uint32, data []byte) uint32 {
	s1, s2 := a&0xffff, a>>16
	for _, b := range data {
		s1 = (s1 + uint32(b)) % 65521
		s2 = (s2 + s1) % 65521
	}
	return s2<<16 | s1
}

// Run 启动托盘（阻塞，直到 Quit）。icon 为 ico/png 字节。
func Run(icon []byte, tooltip string, act Actions) {
	if act.OpenUI == nil {
		act.OpenUI = func() {}
	}
	if act.OpenLog == nil {
		act.OpenLog = func() {}
	}
	if act.Quit == nil {
		act.Quit = func() {}
	}

	// 各菜单项图标（16x16 纯色小方块）
	iconOpen := miniPNGWithBorder(37, 99, 235)   // 蓝色 - 打开
	iconLog  := miniPNGWithBorder(140, 145, 159)  // 灰色 - 日志
	iconQuit := miniPNGWithBorder(220, 38, 38)    // 红色 - 退出

	systray.Run(func() {
		systray.SetIcon(icon)
		systray.SetTooltip(tooltip)

		mOpen := systray.AddMenuItem("打开主界面", "在系统浏览器中打开管理界面")
		mOpen.SetIcon(iconOpen)

		mLog := systray.AddMenuItem("查看日志", "用系统默认编辑器打开日志文件")
		mLog.SetIcon(iconLog)

		systray.AddSeparator()

		mQuit := systray.AddMenuItem("退出", "退出程序")
		mQuit.SetIcon(iconQuit)

		// 单击 / 双击 = 打开主界面（与旧版行为一致）
		systray.SetOnClick(func(systray.IMenu) { go act.OpenUI() })
		systray.SetOnDClick(func(systray.IMenu) { go act.OpenUI() })

		// 菜单回调：全部 goroutine 化（托盘消息循环线程只做投递，绝不阻塞）
		mOpen.Click(func() { go act.OpenUI() })
		mLog.Click(func() { go act.OpenLog() })
		mQuit.Click(func() { go act.Quit() })
	}, func() {
		log.Printf("托盘已退出")
	})
}