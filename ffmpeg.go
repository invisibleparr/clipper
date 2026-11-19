package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

func probeDuration(path string) (float64, error) {
	cmd := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=nw=1:nk=1", path)
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe failed: %w", err)
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("no usable duration in ffprobe output %q for %s", strings.TrimSpace(string(out)), path)
	}
	return d, nil
}

// probeSize returns the display width and height of the first video stream.
// A rotation display matrix (common in phone recordings) swaps them, because
// ffmpeg's autorotate hands decoded frames to us already turned; scaling to
// the stored size would squash portrait video into a landscape box.
func probeSize(path string) (int, int) {
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_streams", "-of", "json", path).Output()
	if err != nil {
		return 0, 0
	}
	return parseStreamSize(out)
}

type probeStreams struct {
	Streams []struct {
		Width        int `json:"width"`
		Height       int `json:"height"`
		SideDataList []struct {
			SideDataType string  `json:"side_data_type"`
			Rotation     float64 `json:"rotation"`
		} `json:"side_data_list"`
	} `json:"streams"`
}

func parseStreamSize(out []byte) (int, int) {
	var s probeStreams
	if err := json.Unmarshal(out, &s); err != nil || len(s.Streams) == 0 {
		return 0, 0
	}
	st := s.Streams[0]
	w, h := st.Width, st.Height
	for _, sd := range st.SideDataList {
		if sd.SideDataType == "Display Matrix" && int(math.Round(sd.Rotation))%180 != 0 {
			w, h = h, w // a quarter turn swaps the display dimensions
		}
	}
	return w, h
}

// probeHasAudio reports whether the file carries at least one audio stream.
func probeHasAudio(path string) bool {
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=codec_type", "-of", "csv=p=0", path).Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// probeHasVideo reports whether the file carries at least one video stream;
// audio-only containers (some webm, mp3 renamed...) have nothing to preview.
func probeHasVideo(path string) bool {
	out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_type", "-of", "csv=p=0", path).Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// rotateFilter returns the -vf filter chain for rot degrees clockwise (0/90/180/270).
func rotateFilter(rot int) string {
	switch ((rot % 360) + 360) % 360 {
	case 90:
		return "transpose=1"
	case 180:
		return "hflip,vflip"
	case 270:
		return "transpose=2"
	}
	return ""
}

var errExportCancelled = errors.New("export cancelled")

// clipJob is one ffmpeg encode: cut [start,end], rotate, scale to preset (0 = original).
type clipJob struct {
	in, out    string
	start, end float64
	rot        int      // 0/90/180/270 clockwise
	preset     int      // short-side target pixels; 0 keeps original size
	crop       cropRect // region of the displayed frame; zero = full frame
	hq         bool     // visually-lossless encode instead of share-fast default
	muted      bool     // drop the audio track entirely
}

// encodeArgs is the video encoder settings; HQ trades ~2x bytes for x264's
// "visually lossless" crf 18 and a slower, more efficient preset.
func encodeArgs(hq bool) []string {
	if hq {
		return []string{"-c:v", "libx264", "-preset", "medium", "-crf", "18"}
	}
	return []string{"-c:v", "libx264", "-preset", "veryfast", "-crf", "23"}
}

var (
	copyAudio = []string{"-c:a", "copy"}
	aacAudio  = []string{"-c:a", "aac", "-b:a", "192k"} // fallback when the container rejects the source codec
	dropAudio = []string{"-an"}                         // mute: no audio stream at all
)

// scaleFilter shrinks the shorter side to preset pixels, never upscaling.
// The expressions run on ffmpeg's real input size (after autorotate), so
// portrait and rotation-metadata videos come out right without Go math.
func scaleFilter(preset int) string {
	if preset <= 0 {
		return ""
	}
	return fmt.Sprintf("scale='if(lte(iw,ih),min(%d,iw),-2)':'if(gt(iw,ih),min(%d,ih),-2)'", preset, preset)
}

// cropFilter cuts a normalized region from the frame it receives. The iw/ih
// expressions resolve against that filter's real input, so the same rect
// matches what the preview shows even after transpose or autorotate.
func cropFilter(c cropRect) string {
	if c.full() {
		return ""
	}
	return fmt.Sprintf("crop='trunc(iw*%.4f/2)*2':'trunc(ih*%.4f/2)*2':'trunc(iw*%.4f)':'trunc(ih*%.4f)'",
		c.w, c.h, c.x, c.y) // even w/h keep yuv420p encoders happy
}

// running tracks the active ffmpeg encode so app exit can kill it
// synchronously; CommandContext's async killer goroutine never runs when
// main returns. Exports are bosses: they may displace an estimate (subordinate),
// estimates may displace each other, but never a boss.
var running struct {
	sync.Mutex
	cmd        *exec.Cmd
	out        string
	killed     bool // set by killRunning to suppress the audio fallback retry
	estimating bool // current cmd is only a size estimate
}

func killRunning() {
	running.Lock()
	defer running.Unlock()
	running.killed = true
	if running.cmd != nil && running.cmd.Process != nil {
		_ = running.cmd.Process.Kill()
		os.Remove(running.out) // synchronous: main returns before any goroutine could clean up
	}
}

func stopped(ctx context.Context) bool {
	running.Lock()
	defer running.Unlock()
	return ctx.Err() != nil || running.killed
}

// export cuts [start,end], applies rotate/crop/scale and encodes, calling
// onProgress with 0..1. It keeps the original audio format; if the output
// container rejects it (e.g. vorbis into .mp4), ffmpeg fails fast and we
// retry re-encoding to AAC — unless muted, which has no audio to save.
// Cancelling ctx kills ffmpeg and removes the partial file.
func export(ctx context.Context, j clipJob, onProgress func(float64)) error {
	running.Lock()
	running.killed = false
	running.Unlock()
	audio := copyAudio
	if j.muted {
		audio = dropAudio
	}
	err := runExport(ctx, j, audio, false, onProgress)
	if err == nil {
		if onProgress != nil {
			onProgress(1) // ensure the bar reaches 100% before the dialog closes
		}
		return nil
	}
	if stopped(ctx) {
		os.Remove(j.out)
		return errExportCancelled
	}
	if !j.muted { // aac retry only makes sense when there is an audio track to rescue
		if err2 := runExport(ctx, j, aacAudio, false, onProgress); err2 != nil {
			if stopped(ctx) { // stopped during fallback: do not report codec error
				os.Remove(j.out)
				return errExportCancelled
			}
			return err // first error is the real cause
		}
		if onProgress != nil {
			onProgress(1)
		}
		return nil
	}
	return err
}

const estSample = 8 // seconds of clip to encode when extrapolating file size

// estimateSize encodes a short sample with the exact export settings and
// scales the byte count up; clips no longer than the sample are encoded
// whole, so their estimate is the real thing.
func estimateSize(ctx context.Context, j clipJob) (int64, error) {
	tmp, err := os.CreateTemp("", "clipper-est-*.mp4")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	clip := j.end - j.start
	sample := clip
	if sample > estSample {
		sample = estSample
	}
	j.out = tmp.Name()
	j.end = j.start + sample
	audio := copyAudio
	if j.muted {
		audio = dropAudio
	}
	err = runExport(ctx, j, audio, true, nil)
	if err != nil && !j.muted && !stopped(ctx) { // same audio fallback as export
		err = runExport(ctx, j, aacAudio, true, nil)
	}
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(tmp.Name())
	if err != nil {
		return 0, err
	}
	return int64(float64(st.Size()) * clip / sample), nil
}

func runExport(ctx context.Context, j clipJob, audioArgs []string, estimating bool, onProgress func(float64)) error {
	dur := j.end - j.start
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y",
		"-ss", fmt.Sprintf("%.3f", j.start),
		"-i", j.in,
		"-t", fmt.Sprintf("%.3f", dur),
	}
	var vf []string
	if f := rotateFilter(j.rot); f != "" {
		vf = append(vf, f)
	}
	if f := cropFilter(j.crop); f != "" {
		vf = append(vf, f) // crop after transpose: rect is defined on the displayed frame
	}
	if f := scaleFilter(j.preset); f != "" {
		vf = append(vf, f) // scale last so presets shrink whatever survived rotate+crop
	}
	if len(vf) > 0 {
		args = append(args, "-vf", strings.Join(vf, ","))
	}
	args = append(args, encodeArgs(j.hq)...)
	args = append(args,
		"-pix_fmt", "yuv420p", "-movflags", "+faststart",
		"-progress", "pipe:2", "-stats_period", "0.2", // machine-readable progress on stderr
	)
	args = append(args, audioArgs...)
	args = append(args, j.out)

	cmd := exec.CommandContext(ctx, "ffmpeg", args...) // ctx cancel kills ffmpeg
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	running.Lock()
	if running.cmd != nil {
		if running.estimating { // newer job displaces the old estimate
			_ = running.cmd.Process.Kill()
			os.Remove(running.out)
		} else if estimating { // export in flight: subordinate bows out
			running.Unlock()
			return errors.New("export in progress")
		}
	}
	err = cmd.Start()
	if err == nil {
		running.cmd, running.out, running.estimating = cmd, j.out, estimating
	}
	running.Unlock()
	if err != nil {
		return err
	}
	defer func() {
		running.Lock()
		if running.cmd == cmd {
			running.cmd, running.out = nil, ""
		}
		running.Unlock()
	}()
	sc := bufio.NewScanner(stderr) // -progress shares fd 2; with loglevel error, everything else is real errors
	var tail []string              // last few stderr lines: the reason behind a cryptic exit status
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "out_time_us=") {
			if len(tail) == 4 {
				tail = tail[1:]
			}
			tail = append(tail, line)
			continue
		}
		us, cerr := strconv.ParseInt(strings.TrimSpace(line[len("out_time_us="):]), 10, 64)
		if cerr != nil || dur <= 0 || onProgress == nil {
			continue
		}
		p := float64(us) / 1e6 / dur
		if p > 1 {
			p = 1
		}
		onProgress(p)
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if len(tail) > 0 {
			return fmt.Errorf("%w: %s", err, strings.Join(tail, "; "))
		}
		return err
	}
	return nil
}
