package main

import (
	"fmt"
	"image"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// setPos moves the playhead everywhere it is shown.
func (u *ui) setPos(t float64) {
	u.pos = t
	u.trim.pos = t
	u.trim.Refresh()
	u.updateTimeLabel()
}

// stopPlaying resets the play button; caller stops the player itself.
func (u *ui) stopPlaying() {
	u.playing = false
	u.playBtn.SetIcon(theme.MediaPlayIcon())
}

// toggleVolume opens or closes an OS-style vertical volume popup above the button.
func (u *ui) toggleVolume() {
	if u.volPop == nil { // built once, reused; slider keeps its value between openings
		v := u.audio.getVolume() * 100 // seed from the engine; a fyne slider defaults to its min (silence)
		pct := widget.NewLabel(fmt.Sprintf("%.0f%%", v))
		pct.Alignment = fyne.TextAlignCenter
		sl := widget.NewSlider(0, 100)
		sl.Value = v
		sl.Orientation = widget.Vertical
		sl.OnChanged = func(v float64) {
			u.audio.setVolume(v / 100)
			pct.SetText(fmt.Sprintf("%.0f%%", v))
		}
		u.volSlider = sl
		cell := container.NewGridWrap(fyne.NewSize(36, 150), sl) // fixed cell sizes the thin popup
		content := container.NewVBox(pct, container.NewCenter(cell))
		u.volPop = widget.NewPopUp(content, u.win.Canvas())
		u.volPop.Hide() // widgets start visible; hide once so Visible() tracks real state (outside-tap dismiss also hides)
	}
	if u.volPop.Visible() {
		u.volPop.Hide()
		return
	}
	ms := u.volPop.MinSize() // float it above the button, centered over it; tap outside dismisses too
	rel := fyne.NewPos((u.volBtn.Size().Width-ms.Width)/2, -(ms.Height + theme.Padding()))
	u.volPop.ShowAtRelativePosition(rel, u.volBtn)
}

// updateVolBtn decides the volume button's look: calm and disabled when no
// file is loaded or it has no audio track; danger red when this machine has
// no audio device to play anything with.
func (u *ui) updateVolBtn() {
	switch {
	case u.path == "": // nothing loaded yet: stay quiet, not scary-red
		u.volBtn.Disable()
		u.volBtn.Importance = widget.MediumImportance
	case !u.audio.available(): // the file may have sound; this machine cannot play it
		u.volBtn.Disable()
		u.volBtn.Importance = widget.DangerImportance
	case !u.hasAudio:
		u.volBtn.Disable()
		u.volBtn.Importance = widget.MediumImportance
	default:
		u.volBtn.Enable()
	}
	if u.volBtn.Disabled() && u.volPop != nil && u.volPop.Visible() { // do not leave the popup out on a dead button
		u.volPop.Hide()
	}
	u.volBtn.Refresh() // repaint with the new importance even when already disabled
}

func (u *ui) togglePlay() {
	if u.path == "" {
		return
	}
	if u.playing {
		u.player.pause()
		u.stopPlaying()
		return
	}
	t := u.pos
	if t < u.start || t >= u.end-minClipLen { // replay the clip from its start
		t = u.start
	}
	u.playing = true
	u.playBtn.SetIcon(theme.MediaPauseIcon())
	u.player.play(t, u.end)
}

func (u *ui) scrub(t float64, force bool) {
	if u.path == "" {
		return
	}
	now := time.Now()
	if !force && now.Sub(u.lastScrub) < scrubThrottle {
		return // throttle ffmpeg seeks during drags
	}
	u.lastScrub = now
	if u.playing { // interacting pauses playback, telegram-style
		u.stopPlaying()
		u.player.pause()
	}
	u.setPos(t)
	u.player.seekFrame(t)
}

func (u *ui) frame(gen int, img image.Image, t float64) {
	fyne.Do(func() {
		if !u.player.current(gen) { // a newer seek/play won the race; this frame is history
			return
		}
		if t > u.duration {
			t = u.duration
		}
		u.lastImg = img
		u.preview.Image = rotateCW(img, u.rot/90)
		u.preview.Refresh()
		u.setPos(t)
	})
}

func (u *ui) endOfVideo(gen int) {
	fyne.Do(func() {
		if !u.player.current(gen) { // seeked or restarted since the stream ended
			return
		}
		if u.loop && u.duration > 0 { // rewind and keep rolling
			u.setPos(u.start)
			u.player.play(u.start, u.end)
			return
		}
		u.stopPlaying()
		u.setPos(u.end) // last preview frame is quantized to 1/fps; land on the cut point
	})
}
