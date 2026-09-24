package main

import (
	"fmt"
	"image"
	"io"
	"os/exec"
	"sync"
	"time"
)

const previewMaxWidth = 960 // preview width cap; height keeps aspect

// player streams low-res raw RGBA frames from ffmpeg for preview playback;
// no image codec is involved, the bytes go straight into an image.RGBA.
type player struct {
	mu      sync.Mutex
	path    string
	fps     int
	vw, vh  int // preview frame size in pixels
	cmd     *exec.Cmd
	gen     int
	playing bool

	onFrame func(gen int, img image.Image, t float64) // frame + timestamp (called from goroutine)
	onEnd   func(gen int)                             // reached end of file (called from goroutine)

	audio    *audioEngine // preview sound; nil-safe to leave unset
	hasAudio bool         // current path carries an audio stream
}

// current reports whether gen still names the live stream. Callbacks that
// hop onto the UI thread must re-check it at execution time: a kill bumps
// gen after stale closures are already queued, and only they can drop
// themselves then — checking before enqueue is too early to help.
func (p *player) current(gen int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gen == gen
}

func newPlayer(fps int) *player {
	return &player{fps: fps}
}

func (p *player) setPath(path string, hasAudio bool) {
	w, h := probeSize(path)
	if w > previewMaxWidth {
		h = int(float64(h) * (previewMaxWidth / float64(w)))
		w = previewMaxWidth
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.path = path
	p.vw, p.vh = w, h
	p.hasAudio = hasAudio
	p.playing = false
	p.killLocked()
}

// play starts streaming frames from time t, stopping at stop (-1 = end of file).
func (p *player) play(t, stop float64) { p.start(t, false, stop) }

// seekFrame shows a single frame at time t and stops.
func (p *player) seekFrame(t float64) { p.start(t, true, -1) }

// pause stops the stream; caller keeps track of the position to show.
func (p *player) pause() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.playing = false
	p.killLocked()
}

func (p *player) killLocked() {
	p.gen++
	p.reapLocked()
}

// reapLocked stops and reaps the current ffmpeg; caller holds p.mu.
// Without Wait the exited process lingers as a zombie child. Audio rides
// the same lifecycle: every stop path (pause, seek, end, new start) ends
// here, so one engine.stop covers them all.
func (p *player) reapLocked() {
	p.audio.stop()
	if p.cmd != nil && p.cmd.Process != nil {
		cmd := p.cmd
		_ = cmd.Process.Kill() // no-op if already exited on its own
		go func() { _ = cmd.Wait() }()
	}
	p.cmd = nil
}

func (p *player) start(t float64, oneShot bool, stop float64) {
	p.mu.Lock()
	gen := p.gen + 1
	p.gen = gen
	p.reapLocked() // stop previous stream before starting the next
	if p.vw <= 0 || p.vh <= 0 {
		return // unknown frame size, nothing to stream
	}
	w, h := p.vw, p.vh // capture under lock; readFrames must not reread (setPath may change them)
	hasAudio := p.hasAudio
	p.playing = false // a failed start below must not leave us claiming playback
	stdout, err := func() (io.ReadCloser, error) {
		vf := fmt.Sprintf("fps=%d,scale=%d:%d", p.fps, w, h)
		args := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
			"-ss", fmt.Sprintf("%.3f", t), "-i", p.path,
			"-vf", vf, "-an", "-sn"}
		if !oneShot && stop > t { // let ffmpeg cut at the trim end
			args = append(args, "-t", fmt.Sprintf("%.3f", stop-t))
		}
		args = append(args, "-f", "rawvideo", "-pix_fmt", "rgba", "pipe:1")
		cmd := exec.Command("ffmpeg", args...)
		r, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		p.cmd = cmd
		return r, nil
	}()
	if err == nil {
		p.playing = !oneShot // only claim playing once stream is really running
	}
	p.mu.Unlock()

	if err != nil {
		return
	}
	go p.readFrames(gen, stdout, t, oneShot, w, h)
	if !oneShot && hasAudio { // preview sound rides along with real playback only
		p.audio.play(p.path, t, stop)
	}
}

func (p *player) readFrames(gen int, r io.Reader, t0 float64, oneShot bool, w, h int) {
	frameDur := time.Second / time.Duration(p.fps)
	start := time.Now() // wall clock used to pace frames at realtime
	idx := 0

	for {
		buf := make([]byte, w*h*4)
		if _, err := io.ReadFull(r, buf); err != nil {
			break
		}
		p.mu.Lock()
		cur := p.gen == gen
		p.mu.Unlock()
		if !cur {
			return
		}
		img := &image.RGBA{Pix: buf, Stride: 4 * w, Rect: image.Rect(0, 0, w, h)}
		ts := t0 + float64(idx)/float64(p.fps)
		if !oneShot { // pace display at realtime; ffmpeg drains unpaced
			if d := time.Until(start.Add(time.Duration(idx) * frameDur)); d > 0 {
				time.Sleep(d)
			}
		}
		p.onFrame(gen, img, ts)
		idx++
		if oneShot { // frame delivered, stop and reap our ffmpeg
			p.mu.Lock()
			if p.gen == gen {
				p.reapLocked()
			}
			p.mu.Unlock()
			return
		}
	}

	p.mu.Lock()
	cur := p.gen == gen && p.playing
	if cur {
		p.playing = false
		p.reapLocked() // stream ended on its own; do not leave zombie ffmpeg behind
	}
	p.mu.Unlock()
	if cur && p.onEnd != nil {
		p.onEnd(gen)
	}
}
