package recorder

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// argValue returns the token following flag, or "" if absent.
func argValue(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func TestFFmpegArgs(t *testing.T) {
	args := ffmpegArgs("title=World of Warcraft", 12, "/tmp/out.mp4")

	if got := argValue(args, "-t"); got != "12" {
		t.Errorf("-t = %q, want 12", got)
	}
	if got := argValue(args, "-i"); got != "title=World of Warcraft" {
		t.Errorf("-i = %q", got)
	}
	// gdigrab emits BGRA; without this conversion the MP4 plays as nothing at
	// all in QuickTime and Slack. Non-negotiable.
	if got := argValue(args, "-pix_fmt"); got != "yuv420p" {
		t.Errorf("-pix_fmt = %q, want yuv420p (else the clip won't play)", got)
	}
	// Slack previews without a full download only when moov is up front.
	if got := argValue(args, "-movflags"); got != "+faststart" {
		t.Errorf("-movflags = %q, want +faststart", got)
	}
	if !slices.Contains(args, "-an") {
		t.Error("expected -an; gdigrab captures no audio")
	}
	if got := argValue(args, "-f"); got != "gdigrab" {
		t.Errorf("-f = %q, want gdigrab", got)
	}
	if args[len(args)-1] != "/tmp/out.mp4" {
		t.Errorf("output path must come last, got %q", args[len(args)-1])
	}
}

// The desktop fallback must differ from the window grab in exactly one place:
// the input. Anything else diverging means the two paths can produce different
// files, which would make a fallback hard to reason about.
func TestDesktopFallbackDiffersOnlyByInput(t *testing.T) {
	win := ffmpegArgs("title=World of Warcraft", 10, "/tmp/o.mp4")
	desk := ffmpegArgs("desktop", 10, "/tmp/o.mp4")

	if len(win) != len(desk) {
		t.Fatalf("arg counts differ: %d vs %d", len(win), len(desk))
	}
	diffs := 0
	for i := range win {
		if win[i] != desk[i] {
			diffs++
		}
	}
	if diffs != 1 {
		t.Errorf("expected exactly 1 differing arg (the input), got %d\nwin:  %v\ndesk: %v", diffs, win, desk)
	}
}

func TestInputsPrefersWindowThenDesktop(t *testing.T) {
	f := NewFFmpeg("ffmpeg", "World of Warcraft")
	if got := f.inputs(); len(got) != 2 || got[0] != "title=World of Warcraft" || got[1] != "desktop" {
		t.Errorf("inputs = %v", got)
	}
	// No title configured: don't try a bogus `title=` grab first.
	bare := NewFFmpeg("ffmpeg", "")
	if got := bare.inputs(); len(got) != 1 || got[0] != "desktop" {
		t.Errorf("inputs with no title = %v, want [desktop]", got)
	}
}

func TestRecordWithoutBinaryIsAClearError(t *testing.T) {
	f := NewFFmpeg("", "World of Warcraft")
	err := f.Record(context.Background(), "/tmp/nope.mp4", 5)
	if err == nil || !strings.Contains(err.Error(), "ffmpeg not found") {
		t.Fatalf("err = %v, want a clear 'ffmpeg not found' message", err)
	}
}

func TestRecordRejectsNonPositiveSeconds(t *testing.T) {
	f := NewFFmpeg("/bin/true", "W")
	if err := f.Record(context.Background(), "/tmp/o.mp4", 0); err == nil {
		t.Fatal("expected an error for seconds=0")
	}
}

func TestResolvePrefersExplicitPath(t *testing.T) {
	// A real, runnable binary that exists on every unix box we build on.
	if got := Resolve("/bin/sh"); got != "/bin/sh" {
		t.Errorf("Resolve(/bin/sh) = %q", got)
	}
}
