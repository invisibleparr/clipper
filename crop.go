package main

import (
	"image/color"
	"math"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/widget"
)

// cropRect is a normalized region of the displayed frame, 0..1 on both axes.
type cropRect struct{ x, y, w, h float64 } // zero = full frame

func (c cropRect) full() bool { return c.w <= 0 || c.h <= 0 }

// effCrop returns the crop only while the checkbox enables it.
func (u *ui) effCrop() cropRect {
	if u.cropOn {
		return u.crop
	}
	return cropRect{}
}

// displayRect maps an iw×ih video into a bw×bh box the way ImageFillContain
// paints it: centered with letterbox bars. Returns x, y, w, h of the image.
func displayRect(iw, ih int, bw, bh float32) (float32, float32, float32, float32) {
	if iw <= 0 || ih <= 0 || bw <= 0 || bh <= 0 {
		return 0, 0, 0, 0
	}
	s := math.Min(float64(bw)/float64(iw), float64(bh)/float64(ih))
	w, h := float32(float64(iw)*s), float32(float64(ih)*s)
	return (bw - w) / 2, (bh - h) / 2, w, h
}

// rectFrom builds a normalized rect between two dragged corners.
func rectFrom(x1, y1, x2, y2 float64) cropRect {
	return cropRect{
		x: math.Min(x1, x2), y: math.Min(y1, y2),
		w: math.Abs(x2 - x1), h: math.Abs(y2 - y1),
	}
}

// cropOverlay sits above the preview in crop mode; dragging rubber-bands
// the export region. Dragging across (nearly) the whole frame clears it.
type cropOverlay struct {
	widget.BaseWidget
	u *ui

	dragging bool
	start    fyne.Position // drag origin, normalized coords
	cur      cropRect      // rect currently being dragged
}

func newCropOverlay(u *ui) *cropOverlay {
	c := &cropOverlay{u: u}
	c.ExtendBaseWidget(c)
	return c
}

// norm converts a widget-local point to normalized displayed-frame coords.
func (c *cropOverlay) norm(p fyne.Position) (float64, float64, bool) {
	img := c.u.preview.Image
	if img == nil {
		return 0, 0, false
	}
	b := img.Bounds()
	x, y, w, h := displayRect(b.Dx(), b.Dy(), c.Size().Width, c.Size().Height)
	if w <= 0 || h <= 0 {
		return 0, 0, false
	}
	nx := clampF((float64(p.X)-float64(x))/float64(w), 0, 1)
	ny := clampF((float64(p.Y)-float64(y))/float64(h), 0, 1)
	return nx, ny, true
}

func (c *cropOverlay) begin(p fyne.Position) {
	if nx, ny, ok := c.norm(p); ok {
		c.dragging = true
		c.start = fyne.NewPos(float32(nx), float32(ny))
		c.cur = cropRect{x: nx, y: ny}
		c.Refresh()
	}
}

func (c *cropOverlay) DragStart(e *fyne.DragEvent) { c.begin(e.Position) }

func (c *cropOverlay) Dragged(e *fyne.DragEvent) {
	if !c.dragging {
		c.begin(e.Position) // glfw driver may skip DragStart, as in trimBar
		if !c.dragging {
			return
		}
	}
	nx, ny, ok := c.norm(e.Position)
	if !ok {
		return
	}
	c.cur = rectFrom(float64(c.start.X), float64(c.start.Y), nx, ny)
	c.Refresh()
}

func (c *cropOverlay) DragEnd() {
	if !c.dragging {
		return
	}
	c.dragging = false
	old := c.u.crop
	switch r := c.cur; {
	case r.w < 0.02 || r.h < 0.02: // tap or sliver: accidental, keep old crop
	case r.w > 0.98 && r.h > 0.98: // drag across the whole frame clears it
		c.u.crop = cropRect{}
	default:
		c.u.crop = r
	}
	if c.u.crop != old {
		c.u.estimate() // fewer pixels usually means a smaller file; taps cost nothing
	}
	c.Refresh()
}

type cropRenderer struct {
	w      *cropOverlay
	mask   [4]*canvas.Rectangle // shade above, below, left and right of the hole
	border *canvas.Rectangle
}

func (r *cropRenderer) visibleRect() (cropRect, bool) {
	if r.w.dragging && !r.w.cur.full() {
		return r.w.cur, true
	}
	return r.w.u.crop, !r.w.u.crop.full()
}

func (r *cropRenderer) Layout(size fyne.Size) {
	rect, ok := r.visibleRect()
	img := r.w.u.preview.Image
	dx, dy, dw, dh := float32(0), float32(0), size.Width, size.Height
	if img != nil {
		b := img.Bounds()
		dx, dy, dw, dh = displayRect(b.Dx(), b.Dy(), size.Width, size.Height)
	}
	if !ok || img == nil {
		for _, m := range r.mask {
			m.Resize(fyne.NewSize(0, 0))
		}
		r.border.Resize(fyne.NewSize(0, 0))
		return
	}
	hx, hy := dx+float32(rect.x)*dw, dy+float32(rect.y)*dh
	hw, hh := float32(rect.w)*dw, float32(rect.h)*dh
	shade := func(i int, x, y, w, h float32) {
		r.mask[i].Move(fyne.NewPos(x, y))
		r.mask[i].Resize(fyne.NewSize(w, h))
	}
	shade(0, 0, 0, size.Width, hy)                    // above
	shade(1, 0, hy+hh, size.Width, size.Height-hy-hh) // below
	shade(2, 0, hy, hx, hh)                           // left
	shade(3, hx+hw, hy, size.Width-hx-hw, hh)         // right
	r.border.Move(fyne.NewPos(hx, hy))
	r.border.Resize(fyne.NewSize(hw, hh))
}

func (r *cropRenderer) MinSize() fyne.Size { return fyne.NewSize(0, 0) }

func (r *cropRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.mask[0], r.mask[1], r.mask[2], r.mask[3], r.border}
}

func (r *cropRenderer) Refresh() {
	r.Layout(r.w.Size())
	for _, o := range r.Objects() {
		o.Refresh()
	}
}

func (c *cropOverlay) CreateRenderer() fyne.WidgetRenderer {
	shade := color.NRGBA{0, 0, 0, 140}
	var mask [4]*canvas.Rectangle
	for i := range mask {
		mask[i] = canvas.NewRectangle(shade)
	}
	border := &canvas.Rectangle{StrokeColor: color.NRGBA{255, 255, 255, 230}, StrokeWidth: 2}
	return &cropRenderer{w: c, mask: mask, border: border}
}

func (r *cropRenderer) Destroy() {}
