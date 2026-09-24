package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/widget"
)

func (u *ui) openFile() {
	fd := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
		if rc == nil {
			return
		}
		path := rc.URI().Path()
		rc.Close()
		u.load(path)
	}, u.win)
	fd.SetFilter(&storage.ExtensionFileFilter{Extensions: []string{
		".mp4", ".mov", ".mkv", ".avi", ".webm", ".m4v", ".3gp"}})
	fd.Show()
	fd.Resize(u.win.Canvas().Size()) // fill window; resizing before Show panics
}

func (u *ui) load(path string) {
	dur, err := probeDuration(path)
	if err != nil {
		dialog.ShowError(err, u.win)
		return
	}
	if !probeHasVideo(path) { // audio-only containers leave nothing to preview or trim
		dialog.ShowInformation("no video", "this file has no video stream.\nclipper needs a picture to trim.", u.win)
		return
	}
	u.path = path
	u.duration = dur
	u.start, u.end, u.pos = 0, dur, 0
	u.rot = 0
	u.crop, u.cropOn = cropRect{}, false
	if u.cropChk != nil && u.cropChk.Checked {
		u.cropChk.SetChecked(false) // hides the overlay via OnChanged
	}
	u.lastImg = nil // do not let rotation act on the previous video's frame
	u.rotLbl.SetText("0°")
	u.trim.duration, u.trim.start, u.trim.end, u.trim.pos = dur, 0, dur, 0
	u.trim.Refresh()

	u.win.SetTitle("Clipper - " + truncName(filepath.Base(path)))
	u.placeholder.Hide()
	u.status.SetText("") // do not carry "saved …" into the new file
	if !u.exporting {    // opening a file mid-export must not re-enable its button
		u.setControlsEnabled(true)
	}
	u.estLbl.SetText("")
	u.updateTimeLabel()
	u.updateCuts()
	u.stopPlaying()
	u.hasAudio = probeHasAudio(path) // cheap ffprobe; decides the volume button's fate
	u.updateVolBtn()

	u.trim.setWave(nil) // old envelope must not haunt the new file
	if u.hasAudio {
		u.waveGen++
		gen := u.waveGen
		go func() {
			peaks := waveform(path, dur) // decode off the UI thread
			fyne.Do(func() {             // paint only if this load still owns the bar
				if gen == u.waveGen && path == u.path {
					u.trim.setWave(peaks)
				}
			})
		}()
	}

	u.player.setPath(path, u.hasAudio)
	u.player.seekFrame(0)
	u.estimate()
}

// truncName shortens long file names for the window title, keeping the
// extension and counting runes so a multibyte name is never cut mid-rune
// (or sliced past its end into spare backing-array capacity).
func truncName(name string) string {
	r := []rune(name)
	if len(r) <= 34 {
		return name
	}
	ext := filepath.Ext(name)
	return string(r[:16]) + "…" + ext
}

func (u *ui) saveFile() {
	base := strings.TrimSuffix(filepath.Base(u.path), filepath.Ext(u.path))
	sd := dialog.NewFileSave(func(wc fyne.URIWriteCloser, err error) {
		if wc == nil {
			return
		}
		out := wc.URI().Path()
		wc.Close()
		u.export(out)
	}, u.win)
	sd.SetFileName(base + "_clipper.mp4")
	sd.SetFilter(&storage.ExtensionFileFilter{Extensions: []string{".mp4"}}) // steer away from cryptic muxer failures
	if dir, err := storage.ListerForURI(storage.NewFileURI(filepath.Dir(u.path))); err == nil {
		sd.SetLocation(dir) // start in the source file's folder
	}
	sd.Show()
	sd.Resize(u.win.Canvas().Size()) // fill window; resizing before Show panics
}

// job snapshots the UI state into an encode job; out may be empty for estimates.
func (u *ui) job(out string) clipJob {
	return clipJob{in: u.path, out: out, start: u.start, end: u.end, rot: u.rot, preset: u.preset, crop: u.effCrop(), hq: u.hq, muted: u.muted}
}

func (u *ui) export(out string) {
	if u.duration <= 0 {
		return
	}
	u.player.pause()
	u.stopPlaying()
	if u.estCancel != nil { // export owns the ffmpeg slot now; stop any estimate
		u.estCancel()
		u.estCancel = nil
	}
	u.exporting = true
	u.setControlsEnabled(false)
	j := u.job(out) // snapshot; UI may change meanwhile
	ctx, cancel := context.WithCancel(context.Background())

	bar := widget.NewProgressBar() // determinate: ffmpeg reports out_time_us
	bar.Min, bar.Max = 0, 1
	var cancelBtn *widget.Button
	cancelBtn = widget.NewButton("cancel", func() {
		cancel()
		cancelBtn.Disable()
	})
	dlg := dialog.NewCustomWithoutButtons("exporting", container.NewPadded(container.NewVBox(
		bar, cancelBtn)), u.win)
	dlg.Resize(fyne.NewSize(320, 140))
	dlg.Show()

	go func() {
		err := export(ctx, j, func(p float64) {
			fyne.Do(func() {
				bar.SetValue(p)
				u.status.SetText(fmt.Sprintf("exporting… %.0f%%", p*100))
			})
		})
		fyne.Do(func() {
			cancel()
			dlg.Dismiss()
			u.exporting = false
			u.setControlsEnabled(true)
			if errors.Is(err, errExportCancelled) {
				u.status.SetText("export cancelled")
				return
			}
			if err != nil {
				u.status.SetText("export failed")
				dialog.ShowError(fmt.Errorf("ffmpeg export failed: %w", err), u.win)
				return
			}
			u.status.SetText("saved " + filepath.Base(out))
			dialog.ShowInformation("done", "clip saved to\n"+out, u.win)
		})
	}()
}

// presets are the resolution choices; original keeps the source size.
var presets = []struct {
	name string
	px   int
}{
	{"original", 0}, {"240p", 240}, {"360p", 360}, {"480p", 480}, {"720p", 720}, {"1080p", 1080},
}

func (u *ui) setPreset(name string) {
	for _, p := range presets {
		if p.name == name {
			u.preset = p.px
			break
		}
	}
	u.estimate()
}

// estimate kicks off a background sample encode; each trigger supersedes the last.
func (u *ui) estimate() {
	if u.path == "" || u.exporting {
		return
	}
	if u.estCancel != nil {
		u.estCancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	u.estCancel = cancel
	j := u.job("") // snapshot; UI may change meanwhile
	go func() {
		size, err := estimateSize(ctx, j)
		fyne.Do(func() {
			if ctx.Err() != nil || u.exporting { // superseded or export took over
				return
			}
			u.estCancel = nil
			if err != nil {
				return // keep last good number; never nag about a failed guess
			}
			u.estLbl.SetText("≈ " + fmtSize(size))
		})
	}()
}
