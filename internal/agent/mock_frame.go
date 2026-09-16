package agent

import (
	"bytes"
	"hash/fnv"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
)

// mockFrame 生成一帧**一眼能看出是模拟的**控制台画面。
//
// 为什么不画点像真实画面的东西：一张看起来像桌面截图的图，会让人以为
// 控制台通路已经打通了，从而不去验证真实链路；而真实链路（RFB 握手、帧缓冲
// 协商）恰恰是 mock 明确不模拟的那部分（ADR-0007）。图里写着「模拟画面」的
// 含义只能靠颜色和图案来表达，因为标准库里没有字体。
//
// 图案：深色背景 + 对角斜纹 + 一条随目标名变化的色带。
//
//	斜纹是「占位图」的通用视觉语言，几乎不会被误认成真实画面；
//	色带按虚拟机名取色，**同一台机器每次拿到同一颜色**——如果每帧颜色都在
//	变，轮询预览卡时会显得像画面在刷新，而它其实什么都没变。
func mockFrame(target string, width, height int) ConsoleFrame {
	img := image.NewRGBA(image.Rect(0, 0, width, height))

	bg := color.RGBA{R: 0x1b, G: 0x1e, B: 0x24, A: 0xff}
	stripe := color.RGBA{R: 0x25, G: 0x2a, B: 0x33, A: 0xff}

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			// 45° 斜纹：用 (x+y) 的周期性决定条带，得到均匀的对角线。
			if (x+y)/14%2 == 0 {
				img.Set(x, y, bg)
			} else {
				img.Set(x, y, stripe)
			}
		}
	}

	h := fnv.New32a()
	_, _ = h.Write([]byte(target))
	accent := hueToRGB(float64(h.Sum32()%360), 0.55, 0.62)

	// 中央色带：给画面一个明确的视觉锚点，也让不同虚拟机的预览卡一眼可分。
	bandTop := height*2/5 - 6
	bandBottom := height*2/5 + 6
	for y := bandTop; y < bandBottom; y++ {
		if y < 0 || y >= height {
			continue
		}
		for x := width / 5; x < width*4/5; x++ {
			img.Set(x, y, accent)
		}
	}

	// 底部一条细线：暗示这是「占位」而不是画面内容的一部分。
	for x := 0; x < width; x++ {
		img.Set(x, height-1, color.RGBA{R: 0x3a, G: 0x40, B: 0x4a, A: 0xff})
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		// 编码失败不该让整个探测报错：预览卡拿不到画面只是少一个功能，
		// 而把 500 抛给用户会让他以为虚拟机出了问题。
		log.Printf("[agent] mock 控制台画面编码失败: %v", err)
		return ConsoleFrame{MIME: "image/png", Width: width, Height: height}
	}

	return ConsoleFrame{
		MIME:   "image/png",
		Data:   buf.Bytes(),
		Width:  width,
		Height: height,
	}
}

// hueToRGB 把一个色相角（0-360）转成 RGB。
//
// 用 HSL 而不是直接给三通道随机数：随机三通道会得到大量灰扑扑或刺眼的
// 颜色，而色相环上的颜色彼此区分度更好——预览卡上的色带正是要靠它区分。
func hueToRGB(hue, sat, light float64) color.RGBA {
	c := (1 - math.Abs(2*light-1)) * sat
	x := c * (1 - math.Abs(math.Mod(hue/60, 2)-1))
	m := light - c/2

	var r, g, b float64
	switch {
	case hue < 60:
		r, g, b = c, x, 0
	case hue < 120:
		r, g, b = x, c, 0
	case hue < 180:
		r, g, b = 0, c, x
	case hue < 240:
		r, g, b = 0, x, c
	case hue < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}

	to8 := func(v float64) uint8 {
		v = (v + m) * 255
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return uint8(v)
	}
	return color.RGBA{R: to8(r), G: to8(g), B: to8(b), A: 0xff}
}
