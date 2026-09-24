package main

import (
	"fmt"
	"os/exec"
	"sync"

	"github.com/ebitengine/oto/v3"
)

// audioEngine plays preview sound through the system audio device via oto.
// A nil or unavailable engine is silent but harmless: every method tolerates
// it, so headless runs and machines without a sound card just keep working.
type audioEngine struct {
	mu     sync.Mutex
	ok     bool // an audio device was opened at startup
	ctx    *oto.Context
	volume float64 // 0..1, applied to every session
	cmd    *exec.Cmd
	player *oto.Player
}

func newAudioEngine() *audioEngine {
	e := &audioEngine{volume: 1}
	// oto dlopens ALSA/PulseAudio at runtime; failure here means no device,
	// not a broken app. The ready channel is desktop-immediate; drain it.
	ctx, ready, err := oto.NewContext(&oto.NewContextOptions{
		SampleRate:      48000,
		ChannelCount:    2,
		Format:          oto.FormatSignedInt16LE,
		ApplicationName: "Clipper",
	})
	if err != nil {
		return e // preview stays silent; volume button will show danger red
	}
	go func() { <-ready }()
	e.ctx, e.ok = ctx, true
	return e
}

func (e *audioEngine) available() bool { return e != nil && e.ok }

// audioArgs streams decoded audio as raw s16le PCM; -ss before -i seeks fast.
func audioArgs(path string, t, stop float64) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin",
		"-ss", fmt.Sprintf("%.3f", t), "-i", path, "-vn", "-sn"}
	if stop > t { // let ffmpeg cut at the trim end, like the video stream
		args = append(args, "-t", fmt.Sprintf("%.3f", stop-t))
	}
	return append(args, "-f", "s16le", "-ar", "48000", "-ac", "2", "pipe:1")
}

// play starts preview audio from t until stop (-1 = end of file), replacing
// any previous session. Called after the video stream started successfully.
func (e *audioEngine) play(path string, t, stop float64) {
	if !e.available() {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopLocked()
	cmd := exec.Command("ffmpeg", audioArgs(path, t, stop)...)
	r, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		r.Close()
		return
	}
	pl := e.ctx.NewPlayer(r) // pulls the pipe at realtime pace
	pl.SetVolume(e.volume)
	e.cmd, e.player = cmd, pl
	pl.Play()
}

// stop ends the current session; safe to call anytime.
func (e *audioEngine) stop() {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopLocked()
}

func (e *audioEngine) stopLocked() {
	if e.cmd != nil && e.cmd.Process != nil { // dead writer: blocked pipe reads hit EOF fast
		_ = e.cmd.Process.Kill()
	}
	if e.player != nil { // Close is deprecated since v3.4: dropping the reference releases it
		e.player.PauseAndStopReading() // returns once no read is in flight; then Wait is safe
	}
	if e.cmd != nil {
		cmd := e.cmd
		go func() { _ = cmd.Wait() }() // reap, like player.reapLocked
	}
	e.cmd, e.player = nil, nil
}

// getVolume returns the current volume (0..1); nil or deviceless engines
// still report their stored value so UI can seed itself from truth.
func (e *audioEngine) getVolume() float64 {
	if e == nil {
		return 1
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.volume
}

// setVolume adjusts live volume (0..1) and the default for future sessions.
func (e *audioEngine) setVolume(v float64) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.volume = v
	if e.player != nil {
		e.player.SetVolume(v)
	}
}
