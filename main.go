package main

import (
	"context"
	"image"
	"image/color"
	"os"
	"os/exec"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

type ui struct {
	app fyne.App
	win fyne.Window

	path      string
	duration  float64
	start     float64
	end       float64
	pos       float64
	rot       int      // 0/90/180/270 clockwise
	crop      cropRect // rubber-banded region of the displayed frame
	cropOn    bool     // checkbox state: apply u.crop on export when true
	preset    int      // short-side target pixels for export; 0 = original
	hq        bool     // checkbox state: visually-lossless encode on export
	muted     bool     // checkbox state: drop the audio track on export
	exporting bool     // export in flight; keep controls disabled across loads
	playing   bool
	loop      bool
	lastImg   image.Image
	lastScrub time.Time

	player                *player
	audio                 *audioEngine // preview sound device; never nil after main() starts
	hasAudio              bool         // current file carries an audio stream
	preview               *canvas.Image
	placeholder           *widget.Label
	timeLbl               *widget.Label
	clipLbl               *widget.Label
	status                *widget.Label
	rotLbl                *widget.Label
	startEntry, endEntry  *widget.Entry
	playBtn               *widget.Button
	volBtn                *widget.Button
	volPop                *widget.PopUp  // vertical volume slider popup, built on first tap
	volSlider             *widget.Slider // inside volPop; kept for tests and future knobs
	trim                  *trimBar
	overlay               *cropOverlay
	muteChk               *widget.Check
	cropChk               *widget.Check
	hqChk                 *widget.Check
	themeBtn              *widget.Button
	exportBtn, rotL, rotR *widget.Button
	presetSel             *widget.Select
	estLbl                *widget.Label
	estCancel             context.CancelFunc // active size estimate, if any
	waveGen               int                // generation token for async waveform loads
}

const (
	previewFPS    = 30                     // preview frame rate; export uses the source's own fps
	minClipLen    = 0.1                    // shortest clip allowed between handles, seconds
	scrubThrottle = 100 * time.Millisecond // ffmpeg seek limit while dragging
)

func main() {
	a := app.NewWithID("dev.local.clipper")
	w := a.NewWindow("Clipper")
	u := &ui{app: a, win: w, player: newPlayer(previewFPS)}
	u.player.onFrame = u.frame
	u.player.onEnd = u.endOfVideo
	u.audio = newAudioEngine() // silent no-op on machines without an audio device
	u.player.audio = u.audio

	w.SetContent(u.build())
	w.Resize(fyne.NewSize(900, 640))
	if len(os.Args) > 1 { // ./clipper video.mp4
		go func() { fyne.Do(func() { u.load(os.Args[1]) }) }()
	}
	w.SetOnDropped(func(_ fyne.Position, uris []fyne.URI) { // drag a file in
		if len(uris) > 0 {
			u.load(uris[0].Path())
		}
	})
	for _, tool := range []string{"ffmpeg", "ffprobe"} { // everything shells out; say so up front
		if _, err := exec.LookPath(tool); err != nil {
			dialog.ShowInformation("missing "+tool, "clipper needs "+tool+" on your PATH.\ninstall the ffmpeg package and start again.", w)
			break // one nag is enough
		}
	}
	// system light/dark preference resolves asynchronously after startup; both
	// hooks cover the race, and refresh is idempotent so double-firing is fine
	a.Settings().AddListener(func(fyne.Settings) { u.refreshThemeLabel() })
	go func() { fyne.Do(func() { u.refreshThemeLabel() }) }()
	w.ShowAndRun()
	killRunning()    // closed mid-export: synchronous kill, no goroutine race
	u.player.pause() // don't leave ffmpeg children behind
}

func (u *ui) build() fyne.CanvasObject {
	// preview area
	u.preview = canvas.NewImageFromImage(nil)
	u.preview.FillMode = canvas.ImageFillContain
	u.placeholder = widget.NewLabel("open a video file to start")
	u.placeholder.Alignment = fyne.TextAlignCenter
	u.overlay = newCropOverlay(u)
	u.overlay.Hide() // crop mode only; a hidden widget swallows no events
	preview := container.NewStack(u.preview, container.NewCenter(u.placeholder), u.overlay)

	// trim bar
	u.trim = newTrimBar()
	u.trim.onSeek = func(v float64) { u.scrub(v, false) }
	u.trim.onChange = func(s, e float64, which int) {
		u.start, u.end = s, e
		u.updateCuts()
		b := u.start // preview the frame at the cut point being dragged
		if which == 2 {
			b = u.end
		}
		u.scrub(b, false)
	}
	u.trim.onDragEnd = func() {
		t := u.pos
		if t < u.start {
			t = u.start
		} else if t > u.end {
			t = u.end
		}
		u.scrub(t, true) // ensure final cut point is shown even if throttled away
		u.estimate()
	}

	// controls row
	u.playBtn = widget.NewButtonWithIcon("", theme.MediaPlayIcon(), u.togglePlay)
	u.volBtn = widget.NewButtonWithIcon("", theme.VolumeUpIcon(), u.toggleVolume)
	loopChk := widget.NewCheck("loop", func(on bool) { u.loop = on })
	u.startEntry, u.endEntry = widget.NewEntry(), widget.NewEntry()
	u.startEntry.SetPlaceHolder("start")
	u.endEntry.SetPlaceHolder("end")
	u.startEntry.OnSubmitted = func(s string) { u.setCut(1, s) }
	u.endEntry.OnSubmitted = func(s string) { u.setCut(2, s) }
	tsSizer := widget.NewLabel("10:00:00.0") // two-digit hour timestamp; measured, not hardcoded
	fixed := func(e *widget.Entry) fyne.CanvasObject {
		w := tsSizer.MinSize().Width + 2*theme.Padding() // text plus the entry's own padding
		// extra padding above: fyne scaling bug makes exact-height cells spawn a scrollbar
		return container.NewGridWrap(fyne.NewSize(w, 34+theme.Padding()), e) // entries don't size themselves
	}
	u.timeLbl = widget.NewLabel("0:00.0 / 0:00.0")
	u.clipLbl = widget.NewLabel("")
	u.status = widget.NewLabel("")
	u.status.Truncation = fyne.TextTruncateEllipsis
	controls := container.NewBorder(nil, nil, container.NewHBox(u.playBtn, u.volBtn, loopChk),
		container.NewHBox(u.clipLbl, widget.NewSeparator(),
			widget.NewLabel("start"), fixed(u.startEntry),
			widget.NewLabel("end"), fixed(u.endEntry),
			widget.NewSeparator(), u.timeLbl), u.status)

	bottom := container.NewVBox(u.trim, controls)

	u.updateVolBtn() // calm disabled until a file says otherwise
	return container.NewBorder(u.topBar(), bottom, nil, nil, preview)
}

// pinnedTheme forces light or dark colors whatever the system prefers;
// SetTheme(theme.LightTheme()) is deprecated and dies in fyne v3.
type pinnedTheme struct {
	fyne.Theme
	v fyne.ThemeVariant
}

func (p pinnedTheme) Color(name fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	return p.Theme.Color(name, p.v)
}

// darkNow asks the actually painted background whether we are in dark mode;
// pixels cannot lie. Caveat: system preference resolves only after the run
// loop starts, so build-time answers can be stale - refresh once running.
func darkNow() bool {
	r, g, b, _ := theme.Color(theme.ColorNameBackground).RGBA() // 16-bit channels
	return (299*r+587*g+114*b)/1000 < 32768                     // Rec.601 luma vs mid-gray
}

// themeTarget is the label for the toggle: the mode a click would bring.
func themeTarget() string {
	if darkNow() {
		return "L"
	}
	return "D"
}

// refreshThemeLabel re-reads the pixels; needed once after ShowAndRun starts
// because system light/dark preference is not resolved during build().
func (u *ui) refreshThemeLabel() { u.themeBtn.SetText(themeTarget()) }

func (u *ui) topBar() fyne.CanvasObject {
	open := widget.NewButtonWithIcon("open", theme.FolderOpenIcon(), u.openFile)
	u.themeBtn = widget.NewButton(themeTarget(), func() { // label offers the opposite of what shows
		v := theme.VariantDark
		if darkNow() {
			v = theme.VariantLight
		}
		u.app.Settings().SetTheme(pinnedTheme{Theme: theme.DefaultTheme(), v: v})
		u.refreshThemeLabel() // re-measured after the flip; pixels do not lie
	})

	u.rotL = widget.NewButton("↺ 90°", func() { u.rotate(-90) })
	u.rotR = widget.NewButton("90° ↻", func() { u.rotate(90) })
	u.rotLbl = widget.NewLabel("0°")
	u.rotLbl.Alignment = fyne.TextAlignCenter
	u.muteChk = widget.NewCheck("mute", func(on bool) { // drop the audio track on export
		u.muted = on
		u.estimate() // one less stream is fewer bytes; show them right away
	})
	u.cropChk = widget.NewCheck("crop", func(on bool) { // enable/disable cropping on export
		u.cropOn = on
		if on {
			u.overlay.Refresh() // paint the existing rect before the first drag
			u.overlay.Show()
		} else {
			u.overlay.Hide() // rect is kept; re-checking brings it back
		}
		u.estimate() // cropping on or off changes the number
	})
	u.hqChk = widget.NewCheck("hq", func(on bool) { // crf 18 + medium preset instead of crf 23 + veryfast
		u.hq = on
		u.estimate() // quality costs bytes; show them right away
	})

	names := make([]string, len(presets))
	for i, p := range presets {
		names[i] = p.name
	}
	u.presetSel = widget.NewSelect(names, u.setPreset)
	u.presetSel.SetSelected("original") // fires setPreset; no path yet so estimate() is a no-op
	u.estLbl = widget.NewLabel("")      // no truncation: it must report its full text width so the bar gives room

	rotSizer := widget.NewLabel("180°") // widest value; measured, not hardcoded
	fixedCell := func(o fyne.CanvasObject, w float32) fyne.CanvasObject {
		return container.NewGridWrap(fyne.NewSize(w, 34), o) // stop text changes from jittering the bar
	}
	rotBox := container.NewHBox(widget.NewSeparator(), u.muteChk, u.cropChk, u.hqChk,
		fixedCell(u.presetSel, 92), u.estLbl,
		widget.NewSeparator(), u.rotL,
		fixedCell(u.rotLbl, rotSizer.MinSize().Width), u.rotR)

	u.exportBtn = widget.NewButtonWithIcon("export", theme.MediaRecordIcon(), u.saveFile)
	helpBtn := widget.NewButtonWithIcon("", theme.HelpIcon(), u.showHelp) // always available, even mid-export
	u.setControlsEnabled(false)

	return container.NewBorder(nil, nil, container.NewHBox(open, u.exportBtn, u.themeBtn, helpBtn), rotBox)
}

func (u *ui) setControlsEnabled(on bool) {
	for _, c := range []fyne.Disableable{u.playBtn, u.rotL, u.rotR, u.exportBtn, u.presetSel, u.muteChk, u.cropChk, u.hqChk, u.startEntry, u.endEntry} {
		if on {
			c.Enable()
		} else {
			c.Disable()
		}
	}
}
