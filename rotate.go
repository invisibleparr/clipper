package main

import (
	"fmt"
	"image"
	"image/color"
)

func (u *ui) rotate(delta int) {
	u.rot = ((u.rot+delta)%360 + 360) % 360
	u.rotLbl.SetText(fmt.Sprintf("%d°", u.rot))
	if u.lastImg != nil && !u.playing {
		u.preview.Image = rotateCW(u.lastImg, u.rot/90)
		u.preview.Refresh()
	}
	u.estimate() // rotation swaps width/height under an active crop rect
}

// rotateCW rotates src clockwise by n*90 degrees.
func rotateCW(src image.Image, n int) image.Image {
	n %= 4
	if n == 0 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	var dst *image.NRGBA
	if n%2 == 1 {
		dst = image.NewNRGBA(image.Rect(0, 0, h, w))
	} else {
		dst = image.NewNRGBA(image.Rect(0, 0, w, h))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, a := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
			var dx, dy int
			switch n {
			case 1: // CW
				dx, dy = h-1-y, x
			case 2:
				dx, dy = w-1-x, h-1-y
			default: // CCW
				dx, dy = y, w-1-x
			}
			dst.SetNRGBA(dx, dy, color.NRGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), uint8(a >> 8)})
		}
	}
	return dst
}
