package main

import (
	"image"
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

const (
	handleW    = float32(10) // grab zone half-width for hit tests
	handleBody = float32(14) // drawn tab width
	handleR    = float32(6)  // outer corner radius
	pillW      = float32(4)  // grip line width
)

// trimBar is a timeline with two draggable handles marking clip start/end,
// plus a playhead line. Tapping the bar seeks.
type trimBar struct {
	widget.BaseWidget
	duration float64
	start    float64
	end      float64
	pos      float64

	onSeek    func(float64) // tap or playhead scrub
	onChange  func(start, end float64, which int)
	onDragEnd func()

	wave  *canvas.Image // audio envelope behind the handles; hidden when peaks is nil
	peaks []float32     // normalized loudness buckets; nil = plain fallback line

	// bake cache: re-render only when size or cut points actually moved,
	// so playhead refreshes during playback cost nothing
	waveW, waveH   int
	waveX0, waveX1 float32
	waveImg        *image.RGBA

	dragging int // 0 none, 1 start handle, 2 end handle, 3 playhead scrub
}

func newTrimBar() *trimBar {
	t := &trimBar{wave: canvas.NewImageFromImage(nil)}
	t.wave.FillMode = canvas.ImageFillStretch
	t.wave.Hide()
	t.ExtendBaseWidget(t)
	return t
}

// setWave swaps the loudness peaks; nil falls back to the plain bar.
func (t *trimBar) setWave(peaks []float32) {
	t.peaks = peaks
	t.waveImg = nil // invalidate the bake cache
	t.Refresh()
}

func (t *trimBar) xFor(v float64) float32 {
	if t.duration <= 0 {
		return 0
	}
	return float32(v / t.duration * float64(t.Size().Width))
}

func (t *trimBar) valFor(x float32) float64 {
	w := t.Size().Width
	if w <= 0 || t.duration <= 0 {
		return 0
	}
	return clampF(float64(x)/float64(w)*t.duration, 0, t.duration)
}

func (t *trimBar) pickHandle(x float32) int {
	const grab = handleW * 2
	sx, ex := t.xFor(t.start), t.xFor(t.end)
	switch {
	case abs32(x-sx) <= grab && abs32(x-sx) <= abs32(x-ex):
		return 1
	case abs32(x-ex) <= grab:
		return 2
	}
	return 0
}

func (t *trimBar) DragStart(e *fyne.DragEvent) {
	t.dragging = t.pickHandle(e.Position.X)
}

func (t *trimBar) Dragged(e *fyne.DragEvent) {
	if t.duration <= 0 {
		return
	}
	// the glfw driver never calls DragStart, so pick the handle here too
	if t.dragging == 0 {
		t.dragging = t.pickHandle(e.Position.X)
	}
	v := t.valFor(e.Position.X)
	switch t.dragging {
	case 0:
		t.dragging = 3 // dragging anywhere else scrubs the playhead
		fallthrough
	case 3:
		t.pos = v
		if t.onSeek != nil {
			t.onSeek(t.pos)
		}
	default:
		if t.dragging == 1 {
			if v > t.end-minClipLen {
				v = t.end - minClipLen
			}
			t.start = v
		} else {
			if v < t.start+minClipLen {
				v = t.start + minClipLen
			}
			t.end = v
		}
		if t.onChange != nil {
			t.onChange(t.start, t.end, t.dragging)
		}
	}
	t.Refresh()
}

func (t *trimBar) DragEnd() {
	was := t.dragging
	t.dragging = 0
	if (was == 1 || was == 2) && t.onDragEnd != nil {
		t.onDragEnd()
	}
}

func (t *trimBar) Tapped(e *fyne.PointEvent) {
	if t.duration <= 0 || t.onSeek == nil {
		return
	}
	x := e.Position.X
	if abs32(x-t.xFor(t.start)) <= handleW*1.5 || abs32(x-t.xFor(t.end)) <= handleW*1.5 {
		return
	}
	t.pos = t.valFor(e.Position.X)
	t.onSeek(t.pos)
	t.Refresh()
}

func abs32(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}

type trimRenderer struct {
	w            *trimBar
	bg           *canvas.Rectangle
	sel          *canvas.Rectangle
	tabS         *canvas.Rectangle // start handle tab (rounded on its left corners)
	tabE         *canvas.Rectangle
	pillS, pillE *canvas.Rectangle // blue grip lines
	head         *canvas.Line
}

func (r *trimRenderer) Layout(size fyne.Size) {
	w := r.w
	if w.duration <= 0 {
		r.sel.Hide()
		w.wave.Hide()
		r.tabS.Hide()
		r.tabE.Hide()
		r.pillS.Hide()
		r.pillE.Hide()
		return
	}
	sx, ex, px := w.xFor(w.start), w.xFor(w.end), w.xFor(w.pos)
	mid := size.Height / 2
	ph := size.Height * 0.55 // grip line length

	if w.peaks != nil {
		wpx, hpx := int(size.Width), int(size.Height)
		if w.waveImg == nil || wpx != w.waveW || hpx != w.waveH || sx != w.waveX0 || ex != w.waveX1 {
			w.waveImg = waveImage(w.peaks, wpx, hpx, sx, ex) // trimmed-away columns stay transparent
			w.waveW, w.waveH, w.waveX0, w.waveX1 = wpx, hpx, sx, ex
		}
		w.wave.Image = w.waveImg
		w.wave.Move(fyne.Position{})
		w.wave.Resize(size)
		w.wave.Show()
		r.sel.Hide() // waveform replaces the plain selection bar
	} else {
		w.wave.Hide()
		r.sel.Show()
	}
	r.tabS.Show()
	r.tabE.Show()
	r.pillS.Show()
	r.pillE.Show()

	r.bg.Resize(size)
	r.sel.Move(fyne.NewPos(sx, mid-8))
	r.sel.Resize(fyne.NewSize(ex-sx, 16))
	for _, h := range []struct {
		tab, pill *canvas.Rectangle
		x         float32
	}{{r.tabS, r.pillS, sx}, {r.tabE, r.pillE, ex}} {
		h.tab.Move(fyne.NewPos(h.x-handleBody/2, 2)) // a little shorter than the bar
		h.tab.Resize(fyne.NewSize(handleBody, size.Height-4))
		h.pill.Move(fyne.NewPos(h.x-pillW/2, mid-ph/2))
		h.pill.Resize(fyne.NewSize(pillW, ph))
	}

	r.head.Position1 = fyne.NewPos(px, 0)
	r.head.Position2 = fyne.NewPos(px, size.Height)
	r.head.StrokeWidth = 2
	r.head.Refresh()
}

func (r *trimRenderer) MinSize() fyne.Size { return fyne.NewSize(200, 48) }

func (r *trimRenderer) Refresh() {
	tab := theme.Color(theme.ColorNameBackground) // themes with light/dark mode
	pill := theme.Color(theme.ColorNamePrimary)
	for _, c := range []*canvas.Rectangle{r.tabS, r.tabE} {
		c.FillColor = tab
	}
	for _, c := range []*canvas.Rectangle{r.pillS, r.pillE} {
		c.FillColor = pill
	}
	r.Layout(r.w.Size())
	for _, o := range r.Objects() {
		o.Refresh()
	}
}

func (t *trimBar) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(color.NRGBA{128, 128, 128, 60})
	sel := canvas.NewRectangle(theme.Color(theme.ColorNamePrimary))
	tabS := &canvas.Rectangle{TopLeftCornerRadius: handleR, BottomLeftCornerRadius: handleR} // round left, square right
	tabE := &canvas.Rectangle{TopRightCornerRadius: handleR, BottomRightCornerRadius: handleR}
	pillS := &canvas.Rectangle{CornerRadius: pillW / 2}
	pillE := &canvas.Rectangle{CornerRadius: pillW / 2}
	head := canvas.NewLine(color.NRGBA{255, 80, 80, 255})
	return &trimRenderer{w: t, bg: bg, sel: sel, tabS: tabS, tabE: tabE, pillS: pillS, pillE: pillE, head: head}
}

func (r *trimRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.bg, r.w.wave, r.sel, r.tabS, r.pillS, r.tabE, r.pillE, r.head}
}

func (r *trimRenderer) Destroy() {}
