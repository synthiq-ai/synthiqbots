// Package recorder captures a short video of the running WoW client for the
// observer clip loop (Phase 3 of the capture feature).
//
// Why forward-recording and not an OBS replay buffer: a replay buffer is
// retrospective — it holds the last N seconds so you can save what already
// happened. The clip trigger is prospective: the MCP tool fires, THEN the
// observer teleports, THEN there is something worth filming. Flushing a buffer
// at trigger time would capture the observer's old parking spot and a loading
// screen. So we record forward, starting the moment the addon signals it is in
// position. ffmpeg is also a single subprocess rather than a long-running peer
// with its own websocket auth and scene config to drift out of sync.
//
// Recorder is an interface so an OBS-websocket backend can replace ffmpeg later
// without touching the queue, the artifact store, or the addon.
package recorder

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// minPlausibleBytes guards against gdigrab's silent failure mode: capturing a
// hardware-accelerated D3D9 window by title yields all-black (or zero) frames on
// some driver combinations, and ffmpeg still exits 0. A real clip of even a few
// seconds is far larger than this.
const minPlausibleBytes = 32 * 1024

// Recorder captures `seconds` of video to outPath.
type Recorder interface {
	Record(ctx context.Context, outPath string, seconds int) error
}

// FFmpeg records the WoW window via the Windows GDI grabber.
type FFmpeg struct {
	Bin         string // path to ffmpeg (resolved by Resolve)
	WindowTitle string // gdigrab -i title=<...>; empty => capture the whole desktop
}

// NewFFmpeg builds a recorder. bin must already be resolved to something
// runnable (see Resolve).
func NewFFmpeg(bin, windowTitle string) *FFmpeg {
	return &FFmpeg{Bin: bin, WindowTitle: windowTitle}
}

// Resolve locates an ffmpeg binary: an explicit configured path first, then
// PATH, then next to our own executable. Returns "" when none is usable — the
// caller should warn rather than die, since the screenshot and feedback paths
// work fine without it.
func Resolve(configured string) string {
	if configured != "" {
		if p, err := exec.LookPath(configured); err == nil {
			return p
		}
		// An explicit path that isn't on PATH may still be a real file.
		if st, err := os.Stat(configured); err == nil && !st.IsDir() {
			return configured
		}
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	if self, err := os.Executable(); err == nil {
		sidecar := filepath.Join(filepath.Dir(self), "ffmpeg.exe")
		if st, err := os.Stat(sidecar); err == nil && !st.IsDir() {
			return sidecar
		}
	}
	return ""
}

// ffmpegArgs builds the command line. Pure, so it can be tested without ffmpeg
// installed.
//
//   - -pix_fmt yuv420p is mandatory: gdigrab emits BGRA, and without the
//     conversion QuickTime and Slack's player show nothing at all.
//   - -movflags +faststart moves the moov atom to the front so Slack can preview
//     the clip without downloading all of it.
//   - -an because gdigrab captures no audio; wiring dshow would be a whole other
//     dependency for a silent flyover.
func ffmpegArgs(input string, seconds int, outPath string) []string {
	return []string{
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "gdigrab", "-framerate", "30", "-i", input,
		"-t", strconv.Itoa(seconds),
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "28",
		"-pix_fmt", "yuv420p",
		"-movflags", "+faststart",
		"-an",
		outPath,
	}
}

// inputs returns the gdigrab inputs to try, in order. Capturing by window title
// is preferred (it excludes everything else on screen); `desktop` is the
// fallback for when the title grab comes back black.
func (f *FFmpeg) inputs() []string {
	if f.WindowTitle == "" {
		return []string{"desktop"}
	}
	return []string{"title=" + f.WindowTitle, "desktop"}
}

// Record captures `seconds` of video to outPath, retrying with the desktop
// grabber if the window grab fails or produces an implausibly small file.
func (f *FFmpeg) Record(ctx context.Context, outPath string, seconds int) error {
	if f.Bin == "" {
		return fmt.Errorf("ffmpeg not found (set ffmpeg_path in config.toml, or drop ffmpeg.exe next to the daemon)")
	}
	if seconds <= 0 {
		return fmt.Errorf("seconds must be positive, got %d", seconds)
	}

	var lastErr error
	for _, input := range f.inputs() {
		err := f.runOnce(ctx, input, outPath, seconds)
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("gdigrab %s: %w", input, err)
		_ = os.Remove(outPath) // don't let a black/partial file poison the retry
	}
	return lastErr
}

func (f *FFmpeg) runOnce(ctx context.Context, input, outPath string, seconds int) error {
	cmd := exec.CommandContext(ctx, f.Bin, ffmpegArgs(input, seconds, outPath)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, snippet(out))
	}
	st, err := os.Stat(outPath)
	if err != nil {
		return fmt.Errorf("no output file: %w", err)
	}
	if st.Size() < minPlausibleBytes {
		return fmt.Errorf("output implausibly small (%d bytes) — likely black frames", st.Size())
	}
	return nil
}

func snippet(b []byte) string {
	const max = 200
	s := string(b)
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
