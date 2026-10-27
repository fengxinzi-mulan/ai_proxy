// Command gen-icon 生成 Windows 应用图标（多尺寸 ICO）。
//
// 图标是纯代码绘制的，不依赖外部素材：一个深色圆角方块上叠蓝色的
// 「流量/代理」折线标记，与界面里的 brand-mark 视觉一致。
// 用法：go run ./scripts/gen-icon
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
)

// sizes 是 ICO 里要打包的尺寸。Windows 会按 DPI 选最合适的一张。
var sizes = []int{16, 20, 24, 32, 40, 48, 64, 128, 256}

func main() {
	out := filepath.Join("build", "windows", "app.ico")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		log.Fatal(err)
	}

	var pngs [][]byte
	for _, s := range sizes {
		img := render(s)
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			log.Fatal(err)
		}
		pngs = append(pngs, buf.Bytes())
	}

	ico, err := buildICO(sizes, pngs)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(out, ico, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s (%d bytes, %d frames)\n", out, len(ico), len(sizes))
}

// buildICO 组装 ICO 容器。每帧存 PNG 数据（Vista 以后支持）。
func buildICO(sizes []int, pngs [][]byte) ([]byte, error) {
	var buf bytes.Buffer
	// ICONDIR
	binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // type = icon
	binary.Write(&buf, binary.LittleEndian, uint16(len(sizes)))

	offset := 6 + 16*len(sizes)
	for i, s := range sizes {
		b := byte(s)
		if s >= 256 {
			b = 0 // 256 在 ICO 目录里用 0 表示
		}
		buf.WriteByte(b) // width
		buf.WriteByte(b) // height
		buf.WriteByte(0) // palette
		buf.WriteByte(0) // reserved
		binary.Write(&buf, binary.LittleEndian, uint16(1))  // planes
		binary.Write(&buf, binary.LittleEndian, uint16(32)) // bpp
		binary.Write(&buf, binary.LittleEndian, uint32(len(pngs[i])))
		binary.Write(&buf, binary.LittleEndian, uint32(offset))
		offset += len(pngs[i])
	}
	for _, p := range pngs {
		buf.Write(p)
	}
	return buf.Bytes(), nil
}

// render 画一张 s×s 的图标。所有坐标按比例缩放，保证各尺寸观感一致。
func render(s int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, s, s))
	f := float64(s)

	// 圆角方块底：深蓝到靛蓝的垂直线性渐变。
	radius := f * 0.22
	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			if !insideRoundRect(float64(x)+0.5, float64(y)+0.5, 0, 0, f, f, radius) {
				continue
			}
			t := float64(y) / f
			img.Set(x, y, color.NRGBA{
				R: uint8(0x1d + t*0x10),
				G: uint8(0x4e + t*0x1a),
				B: uint8(0xd8 + t*0x10),
				A: 255,
			})
		}
	}

	// 折线：三个点，表示请求在代理两侧的流转。
	pts := [][2]float64{
		{f * 0.24, f * 0.66},
		{f * 0.42, f * 0.38},
		{f * 0.60, f * 0.58},
		{f * 0.76, f * 0.30},
	}
	width := math.Max(1.2, f*0.085)
	for i := 0; i+1 < len(pts); i++ {
		drawLine(img, pts[i][0], pts[i][1], pts[i+1][0], pts[i+1][1], width)
	}
	// 末端圆点强调「出口」。
	drawDisc(img, pts[len(pts)-1][0], pts[len(pts)-1][1], math.Max(1.6, f*0.075))
	return img
}

func insideRoundRect(x, y, rx, ry, w, h, r float64) bool {
	if x < rx || y < ry || x > rx+w || y > ry+h {
		return false
	}
	cx := math.Min(math.Max(x, rx+r), rx+w-r)
	cy := math.Min(math.Max(y, ry+r), ry+h-r)
	dx, dy := x-cx, y-cy
	return dx*dx+dy*dy <= r*r
}

func drawLine(img *image.NRGBA, x0, y0, x1, y1, width float64) {
	steps := int(math.Ceil(math.Hypot(x1-x0, y1-y0) * 3))
	if steps < 1 {
		steps = 1
	}
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		drawDisc(img, x0+(x1-x0)*t, y0+(y1-y0)*t, width/2)
	}
}

func drawDisc(img *image.NRGBA, cx, cy, r float64) {
	b := img.Bounds()
	for y := int(cy - r - 1); y <= int(cy+r+1); y++ {
		for x := int(cx - r - 1); x <= int(cx+r+1); x++ {
			if x < b.Min.X || y < b.Min.Y || x >= b.Max.X || y >= b.Max.Y {
				continue
			}
			dx := float64(x) + 0.5 - cx
			dy := float64(y) + 0.5 - cy
			d := math.Hypot(dx, dy)
			if d > r+0.5 {
				continue
			}
			// 边缘一像素做简单抗锯齿。
			alpha := 1.0
			if d > r-0.5 {
				alpha = math.Max(0, math.Min(1, r+0.5-d))
			}
			blend(img, x, y, color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: uint8(255 * alpha)})
		}
	}
}

func blend(img *image.NRGBA, x, y int, c color.NRGBA) {
	old := img.NRGBAAt(x, y)
	a := float64(c.A) / 255
	img.SetNRGBA(x, y, color.NRGBA{
		R: uint8(float64(c.R)*a + float64(old.R)*(1-a)),
		G: uint8(float64(c.G)*a + float64(old.G)*(1-a)),
		B: uint8(float64(c.B)*a + float64(old.B)*(1-a)),
		A: 255,
	})
}
