package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

const helpMD = `# clipper

trim, crop and shrink video clips — telegram style.

## getting a video in
- press **open** or drag a file onto the window
- mp4, mov, mkv, avi, webm, m4v, 3gp — anything ffmpeg can read that has a video stream

## timeline
- drag the two **handles** to set clip start and end
- audio files show a loudness waveform behind the handles so you can aim at peaks; silent files keep the plain bar
- tap anywhere on the bar to move the playhead
- **loop** replays the clip while it plays
- the **start / end** fields accept ` + "`9.3`" + `, ` + "`1:02.5`" + ` or ` + "`1:02:03`" + ` — press enter to apply

## picture
- **rotate** buttons turn the output in quarter turns; what the preview shows is what you get
- **crop** enables cropping on export; while checked, drag a rectangle on the preview to choose the region. dragging across the whole frame clears it, unchecking keeps the rect but exports full frame

## sound
- the speaker button opens a vertical volume slider above it — tap outside or press it again to close
- slide down to zero for silence; there is no separate mute button
- grayed-out speaker: this file has no audio track
- red speaker: no audio device found on this machine

## size
- the resolution select is a cap, never an upscale: ` + "`720p`" + ` means "at most 720p"
- **hq** trades roughly double the bytes for visually-lossless quality
- **≈ ...** estimates export size by encoding a short sample with your exact settings; it updates whenever you touch a knob

## export
- press **export**, pick where to save, watch the progress bar — cancel works any time
- **mute** exports without any audio track
- audio is copied when the container accepts it, otherwise re-encoded to aac 192k`

// showHelp renders the built-in manual with fyne's markdown rich text.
func (u *ui) showHelp() {
	rt := widget.NewRichTextFromMarkdown(helpMD)
	rt.Wrapping = fyne.TextWrapWord
	dlg := dialog.NewCustom("help", "ok", container.NewScroll(rt), u.win) // second arg is the dismiss label, not a subtitle
	dlg.Resize(fyne.NewSize(560, 620))                                    // before Show, like the export dialog
	dlg.Show()
}
