// Command mkicon renders the 1024px app icon into Icon.png in the current
// directory. Run from the repo root: go run ./tools/mkicon
package main

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

const size = 1024
const ss = 4 // samples per axis for antialiasing

type col struct{ r, g, b, a float64 } // straight alpha, 0..1

func rgb(r, g, b float64) col { return col{r / 255, g / 255, b / 255, 1} }

func over(dst, src col) col { // src over dst
	a := src.a + dst.a*(1-src.a)
	if a == 0 {
		return col{}
	}
	mix := func(d, s float64) float64 { return (s*src.a + dst.a*(1-src.a)*d) / a }
	return col{mix(dst.r, src.r), mix(dst.g, src.g), mix(dst.b, src.b), a}
}

// signed distance to rounded rect centered at c with half-size h and radius r
func sdRR(px, py, cx, cy, hw, hh, r float64) float64 {
	dx := math.Abs(px-cx) - hw + r
	dy := math.Abs(py-cy) - hh + r
	if dx < 0 {
		dx = 0
	}
	if dy < 0 {
		dy = 0
	}
	return math.Min(math.Max(dx, dy), 0) - r + math.Hypot(math.Max(dx, 0), math.Max(dy, 0))
}

func shade(px, py float64) col {
	const margin, rad = 0.03, 0.21
	d := sdRR(px, py, .5, .5, .5-margin, .5-margin, rad)
	if d > 0 {
		return col{}
	}

	top, bot := rgb(40, 44, 54), rgb(21, 23, 29) // vertical gradient
	t := py
	c := col{top.r + (bot.r-top.r)*t, top.g + (bot.g-top.g)*t, top.b + (bot.b-top.b)*t, 1}
	dxc, dyc := px-.5, py-.5 // subtle vignette
	vig := 1 - .30*(dxc*dxc+dyc*dyc)
	c.r *= vig
	c.g *= vig
	c.b *= vig

	if py > .37 && py < .63 { // filmstrip band
		c = over(c, rgb(45, 50, 61))
		for _, yc := range [2]float64{.4175, .5825} { // sprocket holes
			for i := 0; i < 13; i++ {
				cx := .0375 + float64(i)*.075
				if sdRR(px, py, cx, yc, .021, .015, .009) <= 0 {
					c = over(c, rgb(14, 16, 20))
				}
			}
		}
		if px < .28 || px > .72 { // dim the trimmed-away parts
			c = over(c, col{0, 0, 0, .45})
		}
	}

	for _, x := range [2]float64{.28, .72} { // amber cut markers
		if math.Abs(px-x) <= .010 && py > .335 && py < .665 {
			c = over(c, rgb(255, 179, 64))
		}
		if math.Hypot(px-x, py-.5) <= .030 {
			c = over(c, rgb(255, 179, 64))
		}
		if math.Hypot(px-x, py-.5) <= .0115 {
			c = over(c, rgb(28, 30, 37))
		}
	}

	if d > -.010 { // faint edge light so it reads on dark taskbars too
		c = over(c, col{1, 1, 1, .09})
	}
	return c
}

func main() {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					c := shade((float64(x)+(float64(sx)+.5)/ss)/size, (float64(y)+(float64(sy)+.5)/ss)/size)
					r += c.r * c.a
					g += c.g * c.a
					b += c.b * c.a
					a += c.a
				}
			}
			n := float64(ss * ss)
			img.Set(x, y, color.NRGBA{uint8(r / n * 255), uint8(g / n * 255), uint8(b / n * 255), uint8(a / n * 255)})
		}
	}
	f, err := os.Create("Icon.png")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	png.Encode(f, img)
}
