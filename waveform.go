package main

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
	"os/exec"

	"fyne.io/fyne/v2/theme"
)

const waveBuckets = 1600 // horizontal resolution; stretched to bar width at paint time

// waveform decodes a file's audio to mono peaks (max abs per bucket, 0..1).
// One low-rate ffmpeg pass does it; nil means "nothing to draw" and the trim
// bar keeps its plain fallback line.
func waveform(path string, dur float64) []float32 {
	const rate = 8000
	total := int(dur * rate)
	if total <= 0 || dur > 12*3600 { // sanity guard against absurd durations
		return nil
	}
	per := math.Max(1, float64(total)/waveBuckets)

	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
		"-i", path, "-vn", "-sn", "-ac", "1", "-ar", fmt.Sprint(rate), "-f", "s16le", "pipe:1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil
	}
	if err := cmd.Start(); err != nil {
		return nil
	}
	defer func() { _, _ = io.Copy(io.Discard, stdout); _ = cmd.Wait() }() // drain + reap, no zombie

	buckets := make([]float32, waveBuckets)
	buf := make([]byte, 8192)
	var carry []byte // lone low byte between reads (odd chunk boundary)
	i, saw := 0, false
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			saw = true
			chunk := buf[:n]
			if len(carry) == 1 && len(chunk) > 0 {
				accumulate(buckets, int32(int16(binary.LittleEndian.Uint16([]byte{carry[0], chunk[0]}))), i, per)
				i++
				chunk = chunk[1:]
			}
			for j := 0; j+1 < len(chunk); j += 2 {
				accumulate(buckets, int32(int16(binary.LittleEndian.Uint16(chunk[j:]))), i, per)
				i++
			}
			if len(chunk)%2 == 1 {
				carry = append(carry[:0], chunk[len(chunk)-1])
			} else {
				carry = carry[:0]
			}
		}
		if err != nil { // EOF is the happy end; any error still leaves usable peaks
			break
		}
	}
	if !saw {
		return nil
	}
	var max float32             // fit to view: real voices live far below full scale, so
	for _, p := range buckets { // rescale by the file's own peak or all dynamics hide mid-bar
		if p > max {
			max = p
		}
	}
	if max > 0 {
		for i := range buckets {
			buckets[i] /= max
		}
	}
	return buckets
}

func accumulate(buckets []float32, v int32, i int, per float64) {
	if v < 0 {
		v = -v // int16 min negates safely in int32
	}
	b := int(float64(i) / per)
	if b >= len(buckets) {
		b = len(buckets) - 1
	}
	if a := float32(v) / 32768; a > buckets[b] {
		buckets[b] = a
	}
}

// waveImage bakes the full peaks envelope at bar resolution but paints only
// columns inside [x0,x1) — trimmed-away time stays transparent, so the
// waveform keeps its exact time alignment and simply vanishes outside the cut.
// A silent stretch still draws the thin center line.
func waveImage(peaks []float32, w, h int, x0, x1 float32) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if w <= 0 || h <= 0 || len(peaks) == 0 {
		return img
	}
	r, g, b, _ := theme.Color(theme.ColorNamePrimary).RGBA() // 16-bit channels
	rgba := color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8), 255}
	mid := float64(h) / 2
	for x := int(math.Max(0, math.Floor(float64(x0)))); x < w && float32(x) < x1; x++ {
		bi := int(float64(x) / float64(w) * float64(len(peaks)))
		if bi >= len(peaks) {
			bi = len(peaks) - 1
		}
		a := math.Sqrt(float64(peaks[bi])) // lift quiet parts so speech stays visible
		top := mid - a*(mid-1)
		bot := mid + a*(mid-1)
		for y := int(math.Round(top)); y <= int(math.Round(bot)); y++ {
			img.SetRGBA(x, y, rgba)
		}
	}
	return img
}
