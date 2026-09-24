package main

import (
	"bytes"
	"context"
	"errors"
	"image"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

func mustFFmpeg(t testing.TB, args ...string) {
	t.Helper()
	cmd := exec.Command("ffmpeg", append([]string{"-hide_banner", "-loglevel", "error"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg unavailable: %v %s", err, out)
	}
}

func testVideo(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "in.mp4")
	mustFFmpeg(t, "-y", "-f", "lavfi", "-i", "testsrc=size=640x360:rate=30:duration=8",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", path)
	return path
}

func TestProbeDuration(t *testing.T) {
	d, err := probeDuration(testVideo(t))
	if err != nil {
		t.Fatal(err)
	}
	if d < 7.5 || d > 8.5 {
		t.Fatalf("duration = %v, want ~8", d)
	}
}

func TestPlayerFrames(t *testing.T) {
	path := testVideo(t)
	got := make(chan float64, 100)
	p := newPlayer(12)
	p.setPath(path, false)
	p.onFrame = func(_ int, _ image.Image, ts float64) { got <- ts }

	p.seekFrame(3.0)
	select {
	case ts := <-got:
		if ts < 2.9 || ts > 3.2 {
			t.Fatalf("seek frame at %v, want ~3.0", ts)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no seek frame received")
	}

	p.play(4.0, -1)
	n := 0
	for n < 5 {
		select {
		case <-got:
			n++
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d play frames", n)
		}
	}
	p.pause()
}

func TestPlayerStopsAtTrim(t *testing.T) {
	path := testVideo(t)
	got := make(chan float64, 200)
	ended := make(chan struct{}, 1)
	p := newPlayer(12)
	p.setPath(path, false)
	p.onFrame = func(_ int, _ image.Image, ts float64) { got <- ts }
	p.onEnd = func(int) { ended <- struct{}{} }

	p.play(0, 2.0) // 8s video, clip ends at 2s
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("playback did not stop at trim end")
	}
	last := 0.0
	for {
		select {
		case ts := <-got:
			if ts > last {
				last = ts
			}
		default:
			if last < 1.5 || last > 2.1 {
				t.Fatalf("last frame at %v, want ~2.0", last)
			}
			return
		}
	}
}

func assertNoZombies(t *testing.T, stage string) {
	t.Helper()
	out, err := exec.Command("ps", "-o", "stat=", "--ppid", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	for _, st := range strings.Fields(string(out)) {
		if strings.HasPrefix(st, "Z") {
			t.Fatalf("%s left a zombie child process", stage)
		}
	}
}

func TestNoZombieFFmpeg(t *testing.T) {
	path := testVideo(t)
	p := newPlayer(12)
	p.onFrame = func(int, image.Image, float64) {}
	ended := make(chan struct{}, 1)
	p.onEnd = func(int) { ended <- struct{}{} }
	p.setPath(path, false)

	p.seekFrame(1.0) // oneShot path kills ffmpeg after the frame
	time.Sleep(500 * time.Millisecond)
	assertNoZombies(t, "seekFrame")

	p.play(0, 0.3) // short clip: stream ends on its own
	select {
	case <-ended:
	case <-time.After(10 * time.Second):
		t.Fatal("playback did not end")
	}
	time.Sleep(500 * time.Millisecond)
	assertNoZombies(t, "natural end of stream")
	p.pause()
}

func TestParseTS(t *testing.T) {
	for _, c := range []struct {
		in   string
		want float64
		ok   bool
	}{
		{"9.3", 9.3, true}, {"0:09.3", 9.3, true}, {"1:02.5", 62.5, true},
		{"1:30", 90, true}, {"9,3", 9.3, true}, {" 4 ", 4, true},
		{"", 0, false}, {"abc", 0, false}, {"-1", 0, false}, {":5", 0, false},
		{"1:2:3", 3723, true}, {"1:02:03.5", 3723.5, true}, {"2.5:1", 0, false}, {"1:2:3:4", 0, false},
	} {
		got, ok := parseTS(c.in)
		if ok != c.ok || (ok && math.Abs(got-c.want) > 1e-9) {
			t.Errorf("parseTS(%q) = %v,%v want %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestExport(t *testing.T) {
	// big enough that encoding takes >1s and progress updates must appear
	in := filepath.Join(t.TempDir(), "in.mp4")
	mustFFmpeg(t, "-y", "-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration=20",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=20",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "libmp3lame", in) // distinctive audio codec to prove it is copied, not re-encoded
	out := filepath.Join(t.TempDir(), "out.mp4")
	var (
		last  float64
		calls int
		mid   bool
	)
	if err := export(context.Background(), clipJob{in: in, out: out, start: 2.0, end: 17.0, rot: 90}, func(p float64) {
		calls++
		last = p
		if p > 0.1 && p < 0.9 {
			mid = true
		}
	}); err != nil {
		t.Fatal(err)
	}
	if last < 0.9 {
		t.Errorf("progress ended at %v", last)
	}
	if !mid || calls < 2 { // fast machines may finish within a tick or two
		t.Errorf("expected mid-progress updates, got %d calls (mid=%v)", calls, mid)
	}
	d, err := probeDuration(out)
	if err != nil {
		t.Fatal(err)
	}
	if d < 14.5 || d > 15.5 {
		t.Errorf("exported duration = %v, want ~15", d)
	}
	pr := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height", "-of", "csv=p=0:s=x", out)
	o, _ := pr.Output()
	if got := string(o[:len(o)-1]); got != "720x1280" {
		t.Errorf("rotated size = %q, want 720x1280", got)
	}
	pr = exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=codec_name", "-of", "csv=p=0", out)
	o, _ = pr.Output()
	if got := strings.TrimSpace(string(o)); got != "mp3" {
		t.Errorf("audio codec = %q, want mp3 (original format preserved)", got)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Error("output file missing/empty")
	}
}

func TestExportAudioFallback(t *testing.T) {
	// wma audio cannot be muxed into .mp4; export must retry with aac
	in := filepath.Join(t.TempDir(), "in.mkv")
	mustFFmpeg(t, "-y", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=15:duration=6",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=6",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "wmav2", "-b:a", "64k", in)
	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := export(context.Background(), clipJob{in: in, out: out, start: 1.0, end: 4.0}, nil); err != nil {
		t.Fatalf("fallback export failed: %v", err)
	}
	pr := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "stream=codec_name", "-of", "csv=p=0", out)
	o, _ := pr.Output()
	if got := strings.TrimSpace(string(o)); got != "aac" {
		t.Errorf("audio codec = %q, want aac fallback", got)
	}
}

func TestExportCancelled(t *testing.T) {
	in := testVideo(t)
	out := filepath.Join(t.TempDir(), "out.mp4")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: must not even start encoding
	if err := export(ctx, clipJob{in: in, out: out, start: 0.5, end: 2.0}, nil); !errors.Is(err, errExportCancelled) {
		t.Fatalf("err = %v, want errExportCancelled", err)
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("partial output file left behind after cancel")
	}
}

func TestExportKillRunning(t *testing.T) {
	// simulates app exit: killRunning must stop ffmpeg synchronously, no fallback retry
	in := filepath.Join(t.TempDir(), "in.mp4")
	mustFFmpeg(t, "-y", "-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=30:duration=60",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", in)
	out := filepath.Join(t.TempDir(), "out.mp4")
	done := make(chan error, 1)
	go func() { done <- export(context.Background(), clipJob{in: in, out: out, end: 60}, nil) }()

	waited := false
	for i := 0; i < 600 && !waited; i++ {
		running.Lock()
		waited = running.cmd != nil
		running.Unlock()
		if !waited {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !waited {
		t.Fatal("export never started")
	}
	killRunning()
	if _, err := os.Stat(out); err == nil {
		t.Error("stale output file left behind by killRunning")
	}
	select {
	case err := <-done:
		if !errors.Is(err, errExportCancelled) {
			t.Errorf("err = %v, want errExportCancelled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("export did not stop after killRunning")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("partial output file left behind after kill")
	}
}

// Benchmarks compare the preview streaming pipelines (source decode + scale +
// output encode) on a 30s 1080p clip; run with: go test -bench Preview -benchtime 3x

func benchSource(b *testing.B) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), "bench.mp4")
	mustFFmpeg(b, "-y", "-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=30:duration=30",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", path)
	return path
}

func benchPreview(b *testing.B, vf string, outArgs ...string) {
	src := benchSource(b)
	for i := 0; i < b.N; i++ {
		args := append([]string{"-hide_banner", "-loglevel", "error", "-i", src, "-vf", vf}, outArgs...)
		cmd := exec.Command("ffmpeg", args...)
		var stderr bytes.Buffer
		cmd.Stdout = io.Discard // drain pipe sink, like the app reading frames
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			b.Fatalf("ffmpeg: %v %s", err, stderr.String())
		}
	}
}

func BenchmarkPreviewRaw960(b *testing.B) { // current pipeline
	benchPreview(b, "fps=30,scale=960:540", "-f", "rawvideo", "-pix_fmt", "rgba", "-")
}

func BenchmarkPreviewRaw480(b *testing.B) {
	benchPreview(b, "fps=30,scale=480:270", "-f", "rawvideo", "-pix_fmt", "rgba", "-")
}

func BenchmarkPreviewJPEG12(b *testing.B) { // pre-rawvideo pipeline, for reference
	benchPreview(b, "fps=12,scale=w='min(480,iw)':h=-2", "-f", "image2pipe", "-vcodec", "mjpeg", "-q:v", "6", "-")
}

func TestFmtTParseRoundTrip(t *testing.T) { // what fmtT writes, parseTS must read back
	for _, v := range []float64{0, 9.3, 62.5, 3599.9, 3600, 3723.5, 7325.2} {
		s := fmtT(v)
		got, ok := parseTS(s)
		if !ok || math.Abs(got-v) > 1e-9 {
			t.Errorf("round trip %v -> %q -> %v,%v", v, s, got, ok)
		}
	}
}

func TestTruncName(t *testing.T) {
	if got := truncName("short.mp4"); got != "short.mp4" {
		t.Errorf("short name changed: %q", got)
	}
	long := strings.Repeat("a", 50) + ".mp4"
	got := truncName(long)
	if !strings.HasSuffix(got, ".mp4") || !strings.Contains(got, "…") {
		t.Errorf("long name mangled: %q", got)
	}
	// multibyte names must not be cut mid-rune (old byte-slice bug)
	utf := strings.Repeat("é", 50) + ".mp4"
	if !utf8.ValidString(truncName(utf)) {
		t.Errorf("invalid utf-8 in title: %q", truncName(utf))
	}
	// >34 bytes but <17 runes: old code sliced [:16] past len into spare
	// capacity and injected a stray rune; must come back untouched
	cjk := strings.Repeat("日", 11) + ".mp4" // 37 bytes, 15 runes
	if got := truncName(cjk); got != cjk {
		t.Errorf("short-rune multibyte name mangled: %q", got)
	}
}

func TestRotateFilter(t *testing.T) {
	cases := map[int]string{0: "", 90: "transpose=1", 180: "hflip,vflip", 270: "transpose=2", -90: "transpose=2"}
	for rot, want := range cases {
		if got := rotateFilter(rot); got != want {
			t.Errorf("rotateFilter(%d) = %q, want %q", rot, got, want)
		}
	}
}

func TestTrimDragNoDragStart(t *testing.T) {
	// the glfw driver never calls DragStart; Dragged alone must grab a handle
	tb := newTrimBar()
	tb.duration, tb.start, tb.end = 10, 0, 10
	tb.Resize(fyne.NewSize(400, 48))

	tb.Dragged(&fyne.DragEvent{PointEvent: fyne.PointEvent{Position: fyne.NewPos(3, 24)}, Dragged: fyne.NewDelta(77, 0)})
	tb.Dragged(&fyne.DragEvent{PointEvent: fyne.PointEvent{Position: fyne.NewPos(80, 24)}, Dragged: fyne.NewDelta(77, 0)})
	if tb.start < 1.5 || tb.start > 2.5 {
		t.Errorf("start = %v, want ~2", tb.start)
	}
	tb.DragEnd()

	// drag far from both handles scrubs the playhead without moving them
	scubbed := -1.0
	tb.onSeek = func(v float64) { scubbed = v }
	tb.Dragged(&fyne.DragEvent{PointEvent: fyne.PointEvent{Position: fyne.NewPos(200, 24)}, Dragged: fyne.NewDelta(30, 0)})
	if tb.start != 2 || tb.end != 10 {
		t.Errorf("scrub drag moved handles: start=%v end=%v", tb.start, tb.end)
	}
	if scubbed < 4.5 || scubbed > 5.5 {
		t.Errorf("scrub seek = %v, want ~5", scubbed)
	}
}

func TestDialogsNoPanic(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	u := &ui{win: test.NewWindow(nil), path: "/tmp/x.mp4"}
	u.openFile()
	u.saveFile()
	u.showHelp() // markdown must parse and the dialog must not bite
}

func TestScaleFilter(t *testing.T) {
	if got := scaleFilter(0); got != "" {
		t.Fatalf("original must add no filter, got %q", got)
	}
	got := scaleFilter(480)
	if !strings.Contains(got, "min(480,iw)") || !strings.Contains(got, "min(480,ih)") {
		t.Fatalf("scaleFilter = %q, want min() on both sides", got)
	}
}

func TestFmtSize(t *testing.T) {
	for _, c := range []struct {
		in   int64
		want string
	}{
		{999, "999 B"}, {1500, "2 KB"}, {3_500_000, "3.5 MB"}, {2_700_000_000, "2.70 GB"},
	} {
		if got := fmtSize(c.in); got != c.want {
			t.Errorf("fmtSize(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExportScaled(t *testing.T) {
	in := testVideo(t) // 640x360 landscape
	out := filepath.Join(t.TempDir(), "small.mp4")
	if err := export(context.Background(), clipJob{in: in, out: out, start: 1, end: 3, preset: 240}, nil); err != nil {
		t.Fatal(err)
	}
	w, h := probeSize(out)
	if w != 426 || h != 240 { // short side hits the preset, aspect kept, even dims
		t.Fatalf("scaled to %dx%d, want 426x240", w, h)
	}
}

func TestEstimateSize(t *testing.T) {
	in := testVideo(t) // 8s; a 4s clip fits inside the sample and is encoded whole
	const start, end = 1.0, 5.0
	out := filepath.Join(t.TempDir(), "real.mp4")
	if err := export(context.Background(), clipJob{in: in, out: out, start: start, end: end}, nil); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	got, err := estimateSize(context.Background(), clipJob{in: in, start: start, end: end})
	if err != nil {
		t.Fatal(err)
	}
	// whole-clip sample: same settings, so the guess must sit within a few percent
	if diff := math.Abs(float64(got-st.Size())) / float64(st.Size()); diff > 0.05 {
		t.Fatalf("estimate %d vs real %d: off by %.1f%%", got, st.Size(), diff*100)
	}
}

func TestDisplayRect(t *testing.T) {
	for _, c := range []struct {
		iw, ih     int
		bw, bh     float32
		x, y, w, h float32
	}{
		{640, 360, 800, 600, 0, 75, 800, 450},       // landscape letterboxed vertically
		{360, 640, 800, 600, 231.25, 0, 337.5, 600}, // portrait pillarboxed horizontally
		{640, 360, 640, 360, 0, 0, 640, 360},        // exact fit
		{0, 360, 800, 600, 0, 0, 0, 0},              // guards
	} {
		x, y, w, h := displayRect(c.iw, c.ih, c.bw, c.bh)
		if x != c.x || y != c.y || w != c.w || h != c.h {
			t.Errorf("displayRect(%dx%d in %vx%v) = %v,%v,%v,%v want %v,%v,%v,%v",
				c.iw, c.ih, c.bw, c.bh, x, y, w, h, c.x, c.y, c.w, c.h)
		}
	}
}

func TestCropFilter(t *testing.T) {
	if got := cropFilter(cropRect{}); got != "" {
		t.Fatalf("full frame must add no filter, got %q", got)
	}
	got := cropFilter(cropRect{x: 0.25, y: 0.25, w: 0.5, h: 0.5})
	for _, want := range []string{"crop=", "trunc(iw*0.5000/2)*2", "trunc(ih*0.5000/2)*2", "trunc(iw*0.2500)", "trunc(ih*0.2500)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("cropFilter = %q, missing %q", got, want)
		}
	}
}

func TestExportCropped(t *testing.T) {
	in := testVideo(t) // 640x360
	out := filepath.Join(t.TempDir(), "crop.mp4")
	job := clipJob{in: in, out: out, start: 1, end: 3, crop: cropRect{x: 0.25, y: 0.25, w: 0.5, h: 0.5}}
	if err := export(context.Background(), job, nil); err != nil {
		t.Fatal(err)
	}
	if w, h := probeSize(out); w != 320 || h != 180 { // center quarter of 640x360
		t.Fatalf("cropped to %dx%d, want 320x180", w, h)
	}
}

func TestRectFrom(t *testing.T) {
	r := rectFrom(0.8, 0.9, 0.3, 0.4) // drag up-left
	if r.x != 0.3 || r.y != 0.4 || math.Abs(r.w-0.5) > 1e-9 || math.Abs(r.h-0.5) > 1e-9 {
		t.Fatalf("rectFrom = %+v", r)
	}
}

func TestStaleGenDropped(t *testing.T) {
	path := testVideo(t)
	got := make(chan int, 100)
	p := newPlayer(12)
	p.setPath(path, false)
	p.onFrame = func(gen int, _ image.Image, _ float64) { got <- gen }

	p.play(0, -1)
	var oldGen int
	select {
	case oldGen = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("no frame received")
	}
	if !p.current(oldGen) {
		t.Fatal("live stream gen must be current")
	}

	p.seekFrame(5.0) // new generation supersedes the play stream
	if p.current(oldGen) {
		t.Fatal("old gen still current after new start; stale UI callbacks would paint over the seek")
	}
	p.pause()
}

func TestFmtTRounding(t *testing.T) {
	// %04.1f alone rounds 59.96s up to the impossible "0:60.0"; rounding
	// the timestamp first must carry into minutes/hours instead.
	for _, c := range []struct {
		in   float64
		want string
	}{
		{59.96, "1:00.0"},
		{3599.97, "1:00:00.0"},
		{59.94, "0:59.9"},
		{0, "0:00.0"},
	} {
		if got := fmtT(c.in); got != c.want {
			t.Errorf("fmtT(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExportFailureMentionsReason(t *testing.T) {
	in := testVideo(t)
	err := export(context.Background(), clipJob{in: in, out: "/no-such-dir-clipper/out.mp4", start: 0, end: 1}, nil)
	if err == nil {
		t.Fatal("export into nonexistent dir should fail")
	}
	if !strings.Contains(err.Error(), "No such file") { // ffmpeg's real reason, not bare exit status
		t.Errorf("error hides the reason: %v", err)
	}
}

func TestEncodeArgs(t *testing.T) {
	fast := strings.Join(encodeArgs(false), " ")
	if !strings.Contains(fast, "-crf 23") || !strings.Contains(fast, "veryfast") {
		t.Errorf("default encode = %q, want crf 23 veryfast", fast)
	}
	hq := strings.Join(encodeArgs(true), " ")
	if !strings.Contains(hq, "-crf 18") || !strings.Contains(hq, "medium") {
		t.Errorf("hq encode = %q, want crf 18 medium", hq)
	}
}

func TestProbeHasAudio(t *testing.T) {
	with := filepath.Join(t.TempDir(), "a.mp4")
	mustFFmpeg(t, "-y", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=15:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", with)
	if !probeHasAudio(with) {
		t.Error("file with sine track must report audio")
	}
	if probeHasAudio(testVideo(t)) {
		t.Error("video-only file must not report audio")
	}
}

func TestExportMuted(t *testing.T) {
	in := filepath.Join(t.TempDir(), "in.mp4")
	mustFFmpeg(t, "-y", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=15:duration=6",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=6",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", in)
	if !probeHasAudio(in) {
		t.Fatal("fixture must carry audio")
	}
	out := filepath.Join(t.TempDir(), "out.mp4")
	if err := export(context.Background(), clipJob{in: in, out: out, start: 1, end: 3, muted: true}, nil); err != nil {
		t.Fatal(err)
	}
	if probeHasAudio(out) {
		t.Error("muted export still carries an audio stream")
	}
	d, err := probeDuration(out)
	if err != nil || d < 1.5 || d > 2.5 {
		t.Errorf("duration = %v, want ~2", d)
	}
}

func TestWaveform(t *testing.T) {
	in := filepath.Join(t.TempDir(), "a.mp4")
	mustFFmpeg(t, "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=6", "-c:a", "aac", in)
	peaks := waveform(in, 6)
	if peaks == nil {
		t.Fatal("sine file must produce peaks")
	}
	if len(peaks) != waveBuckets {
		t.Fatalf("got %d buckets, want %d", len(peaks), waveBuckets)
	}
	var max float32
	for _, p := range peaks {
		if p > max {
			max = p
		}
	}
	if max < 0.01 { // the lavfi fixture's level is not guaranteed loud, only non-silent
		t.Fatalf("loudest bucket %v, want audible", max)
	}
	uniform := 0
	for _, p := range peaks { // a constant tone must fill nearly every bucket comparably
		if p > max/2 {
			uniform++
		}
	}
	if uniform < waveBuckets*9/10 {
		t.Errorf("only %d of %d buckets near the peak level", uniform, waveBuckets)
	}
	if w := waveform(testVideo(t), 8); w != nil {
		t.Error("video-only file must produce no envelope")
	}
}

func TestWaveImage(t *testing.T) {
	img := waveImage([]float32{0, 1}, 2, 96, -1, 3)
	if b := img.Bounds(); b.Dx() != 2 || b.Dy() != 96 {
		t.Fatalf("bounds %v", b)
	}
	if _, _, _, a := img.At(0, 48).RGBA(); a == 0 { // silent columns still draw the thin center line
		t.Error("silent column lost its center line")
	}
	if _, _, _, a := img.At(0, 10).RGBA(); a != 0 {
		t.Error("silent column painted outside the envelope")
	}
	if _, _, _, a := img.At(1, 10).RGBA(); a == 0 {
		t.Error("full-scale column too short")
	}

	clipped := waveImage([]float32{1, 1, 1, 1}, 4, 20, 2, 4) // only the kept half paints
	if _, _, _, a := clipped.At(0, 10).RGBA(); a != 0 {
		t.Error("trimmed-away column was painted")
	}
	if _, _, _, a := clipped.At(3, 10).RGBA(); a == 0 {
		t.Error("kept column missing")
	}
}

func TestTrimBarWaveNoPanic(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	tb := newTrimBar()
	tb.duration, tb.start, tb.end, tb.pos = 10, 2, 8, 5
	w := test.NewWindow(tb)
	defer w.Close()
	tb.setWave([]float32{0.2, 0.9, 0}) // waveform replaces the bar
	tb.setWave(nil)                    // and falls back cleanly
}

func TestProbeHasVideo(t *testing.T) {
	if !probeHasVideo(testVideo(t)) {
		t.Error("test video must report a video stream")
	}
	audioOnly := filepath.Join(t.TempDir(), "a.m4a")
	mustFFmpeg(t, "-y", "-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-c:a", "aac", audioOnly)
	if probeHasVideo(audioOnly) {
		t.Error("audio-only file must not report a video stream")
	}
}

func TestVolBtnStates(t *testing.T) {
	u := &ui{volBtn: widget.NewButtonWithIcon("", theme.VolumeUpIcon(), func() {})}
	u.updateVolBtn() // nothing loaded: calm and disabled, not scary red
	if !u.volBtn.Disabled() || u.volBtn.Importance != widget.MediumImportance {
		t.Error("startup state must be disabled and normal")
	}

	u.path = "x.mp4"
	u.audio = &audioEngine{} // device failed to open
	u.hasAudio = true
	u.updateVolBtn()
	if !u.volBtn.Disabled() || u.volBtn.Importance != widget.DangerImportance {
		t.Error("missing audio device must show disabled danger red")
	}

	u.audio.ok = true
	u.hasAudio = false
	u.updateVolBtn()
	if !u.volBtn.Disabled() || u.volBtn.Importance != widget.MediumImportance {
		t.Error("silent file must be disabled and calm")
	}

	u.hasAudio = true
	u.updateVolBtn()
	if u.volBtn.Disabled() {
		t.Error("file with audio on a working device must enable the button")
	}
}

func TestVolumePopup(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	u := &ui{volBtn: widget.NewButtonWithIcon("", theme.VolumeUpIcon(), func() {}), audio: &audioEngine{volume: 1}} // volume 1 like newAudioEngine gives
	w := test.NewWindow(u.volBtn)                                                                                   // on-canvas so relative placement has real geometry
	defer w.Close()
	u.win = w

	u.toggleVolume()
	if !u.volPop.Visible() {
		t.Error("first tap must open the volume popup")
	}
	if u.volSlider.Value != 100 { // fyne sliders default to their min; must seed from engine volume instead
		t.Errorf("slider opened at %v, want 100 like the label says", u.volSlider.Value)
	}
	u.toggleVolume()
	if u.volPop.Visible() {
		t.Error("second tap must close it again")
	}

	// a popup left open when its button dies must get tucked away
	u.path = "x.mp4" // device unavailable -> danger disabled
	u.hasAudio = true
	u.toggleVolume()
	u.updateVolBtn()
	if u.volPop.Visible() {
		t.Error("popup must hide when its button gets disabled")
	}
}

func TestAudioArgs(t *testing.T) {
	got := strings.Join(audioArgs("v.mp4", 3.5, 9.25), " ")
	for _, want := range []string{"-ss 3.500", "-t 5.750", "-vn", "-f s16le", "-ar 48000", "-ac 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("audioArgs = %q, missing %q", got, want)
		}
	}
	got = strings.Join(audioArgs("v.mp4", 1, -1), " ")
	if strings.Contains(got, "-t ") {
		t.Errorf("open-ended session must not cut: %q", got)
	}
}

func TestAudioEngineNilSafe(t *testing.T) {
	var e *audioEngine // headless runs and tests never build an engine
	e.play("x.mp4", 0, -1)
	e.stop()
	e.setVolume(0.5)
	if e.getVolume() != 1 { // nil engine must still seed UI at full volume
		t.Errorf("nil engine getVolume = %v, want 1", e.getVolume())
	}
	if e.available() {
		t.Error("nil engine must report unavailable")
	}
	e2 := &audioEngine{} // non-nil but no device opened
	if e2.available() {
		t.Error("engine without a device must report unavailable")
	}
	e2.play("x.mp4", 0, -1)
	e2.stop()
	e2.setVolume(1)
}
