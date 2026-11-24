package desktop

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

type iconKind int

const (
	iconIdle iconKind = iota
	iconPlaying
	iconPaused
)

var (
	accent = color.NRGBA{0xFF, 0xCC, 0x00, 0xFF}
	grey   = color.NRGBA{0x8A, 0x8A, 0x8A, 0xFF}
	dark   = color.NRGBA{0x1A, 0x1A, 0x1A, 0xFF}
	white  = color.NRGBA{0xFF, 0xFF, 0xFF, 0xFF}
)

// drawIcon рисует круглую иконку 64×64 со значком «▶» или «❚❚» (сглаживание суперсэмплингом 4×4).
func drawIcon(kind iconKind) []byte {
	const size, ss = 64, 4
	bg, fg := accent, dark
	if kind == iconIdle {
		bg, fg = grey, white
	}
	c := float64(size) / 2
	r := c - 1.5
	inGlyph := func(x, y float64) bool {
		if kind == iconPaused {
			return y > 20 && y < 44 && ((x > 22 && x < 29) || (x > 35 && x < 42))
		}
		// Треугольник ▶ с вершинами (24,18), (24,46), (47,32).
		ax, ay, bx, by, px, py := 24.0, 18.0, 24.0, 46.0, 47.0, 32.0
		sign := func(x1, y1, x2, y2, x3, y3 float64) float64 { return (x1-x3)*(y2-y3) - (x2-x3)*(y1-y3) }
		d1, d2, d3 := sign(x, y, ax, ay, bx, by), sign(x, y, bx, by, px, py), sign(x, y, px, py, ax, ay)
		neg := d1 < 0 || d2 < 0 || d3 < 0
		pos := d1 > 0 || d2 > 0 || d3 > 0
		return !(neg && pos)
	}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var inCircle, glyph int
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					fx := float64(x) + (float64(sx)+0.5)/ss
					fy := float64(y) + (float64(sy)+0.5)/ss
					if math.Hypot(fx-c, fy-c) > r {
						continue
					}
					inCircle++
					if inGlyph(fx, fy) {
						glyph++
					}
				}
			}
			if inCircle == 0 {
				continue
			}
			t := float64(glyph) / float64(inCircle)
			mix := func(a, b uint8) uint8 { return uint8(math.Round(float64(a)*(1-t) + float64(b)*t)) }
			img.SetNRGBA(x, y, color.NRGBA{
				R: mix(bg.R, fg.R), G: mix(bg.G, fg.G), B: mix(bg.B, fg.B),
				A: uint8(math.Round(255 * float64(inCircle) / (ss * ss))),
			})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
