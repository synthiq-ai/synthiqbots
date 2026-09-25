package observer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// writeShot creates a screenshot file with a specific mtime.
func writeShot(t *testing.T, dir, name string, mod time.Time) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("PIXELS"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mod, mod); err != nil {
		t.Fatal(err)
	}
	return p
}

// stubRecorder stands in for ffmpeg: it just writes plausible bytes.
type stubRecorder struct {
	mu      sync.Mutex
	calls   int
	seconds int
	fail    error
}

func (s *stubRecorder) Record(_ context.Context, outPath string, seconds int) error {
	s.mu.Lock()
	s.calls++
	s.seconds = seconds
	fail := s.fail
	s.mu.Unlock()
	if fail != nil {
		return fail
	}
	return os.WriteFile(outPath, []byte("MP4BYTES"), 0o644)
}

func (s *stubRecorder) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// pendingServer serves one pending capture and records what got shipped back.
type shipped struct {
	mu       sync.Mutex
	endpoint string
	reqID    string
	field    string
	done     chan struct{}
}

func newTestServer(t *testing.T, pending []Pending) (*httptest.Server, *shipped) {
	t.Helper()
	sh := &shipped{done: make(chan struct{}, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/observer/pending", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(pendingResponse{Pending: pending})
	})
	record := func(field string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			sh.mu.Lock()
			sh.endpoint = r.URL.Path
			sh.reqID = r.FormValue("reqId")
			if len(r.MultipartForm.File[field]) > 0 {
				sh.field = field
			}
			sh.mu.Unlock()
			fmt.Fprint(w, `{"ok":true}`)
			select {
			case sh.done <- struct{}{}:
			default:
			}
		}
	}
	mux.HandleFunc("/v1/observer/screenshot", record("screenshot"))
	mux.HandleFunc("/v1/observer/clip", record("clip"))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, sh
}

func TestMatchAfterPicksNewestUnusedFile(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	writeShot(t, dir, "old.jpg", base.Add(-time.Hour))
	writeShot(t, dir, "mid.jpg", base.Add(-time.Second))
	newest := writeShot(t, dir, "new.jpg", base)
	writeShot(t, dir, "notes.txt", base) // unrecognized extension: ignored

	p := New("http://x", "tok", dir, nil)

	got, mime := p.matchAfter(base.Add(-2 * time.Second))
	if got != newest || mime != "image/jpeg" {
		t.Fatalf("matchAfter = %q (%s), want %q", got, mime, newest)
	}

	// Once consumed, the next match must fall back to the older candidate,
	// never re-ship the same file.
	p.usedFiles[newest] = true
	got2, _ := p.matchAfter(base.Add(-2 * time.Second))
	if got2 == newest {
		t.Fatal("matchAfter returned an already-used file")
	}
	if filepath.Base(got2) != "mid.jpg" {
		t.Fatalf("matchAfter = %q, want mid.jpg", got2)
	}
}

func TestMatchAfterIgnoresOlderThanCutoff(t *testing.T) {
	dir := t.TempDir()
	base := time.Now()
	writeShot(t, dir, "stale.jpg", base.Add(-time.Hour))

	p := New("http://x", "tok", dir, nil)
	if got, _ := p.matchAfter(base); got != "" {
		t.Fatalf("matchAfter = %q, want no match for a pre-request file", got)
	}
}

func TestTickShipsScreenshot(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeShot(t, dir, "shot.jpg", now)

	srv, sh := newTestServer(t, []Pending{
		{ReqID: "r1", Target: "Claude", Kind: kindShot, RequestedAt: now.Unix()},
	})
	p := New(srv.URL, "tok", dir, nil)
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.endpoint != "/v1/observer/screenshot" || sh.reqID != "r1" || sh.field != "screenshot" {
		t.Fatalf("shipped %+v", sh)
	}
}

// A clip's marker screenshot must trigger a recording, and the MP4 — not the
// marker — is what gets shipped, to /v1/observer/clip.
func TestTickRecordsAndShipsClip(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	marker := writeShot(t, dir, "marker.jpg", now)

	srv, sh := newTestServer(t, []Pending{
		{ReqID: "c1", Target: "Claude", Kind: kindClip, Seconds: 7, RequestedAt: now.Unix()},
	})
	rec := &stubRecorder{}
	p := New(srv.URL, "tok", dir, rec)

	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Tick spawns the recording; wait for the ship to land.
	select {
	case <-sh.done:
	case <-time.After(5 * time.Second):
		t.Fatal("clip never shipped")
	}

	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.endpoint != "/v1/observer/clip" || sh.reqID != "c1" || sh.field != "clip" {
		t.Fatalf("shipped %+v, want the clip endpoint with a `clip` file field", sh)
	}
	if rec.count() != 1 || rec.seconds != 7 {
		t.Errorf("recorder calls=%d seconds=%d, want 1 call of 7s", rec.count(), rec.seconds)
	}
	// The marker is consumed, so the screenshot path can never ship it.
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.usedFiles[marker] {
		t.Error("marker screenshot was not consumed; it could be shipped as a stray artifact")
	}
}

// Tick must not block for the recording's duration — it shares the daemon's
// ticker with the feedback scan and results poll.
func TestTickDoesNotBlockOnRecording(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeShot(t, dir, "marker.jpg", now)

	srv, _ := newTestServer(t, []Pending{
		{ReqID: "c1", Target: "Claude", Kind: kindClip, Seconds: 30, RequestedAt: now.Unix()},
	})
	p := New(srv.URL, "tok", dir, &stubRecorder{})

	start := time.Now()
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Tick blocked for %s; the clip must record in a goroutine", elapsed)
	}
}

// A second Tick while a clip is recording must not start a duplicate recording.
func TestInFlightClipNotRestarted(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeShot(t, dir, "marker.jpg", now)

	srv, _ := newTestServer(t, []Pending{
		{ReqID: "c1", Target: "Claude", Kind: kindClip, Seconds: 5, RequestedAt: now.Unix()},
	})
	p := New(srv.URL, "tok", dir, &stubRecorder{})
	p.mu.Lock()
	p.inFlight["c1"] = true
	p.mu.Unlock()

	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.rec.(*stubRecorder).count() != 0 {
		t.Fatal("an in-flight clip was recorded a second time")
	}
}

// Without ffmpeg the daemon must not silently retry every 5s forever; it marks
// the request handled and logs. Screenshots keep working.
func TestClipWithoutRecorderIsNotRetried(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeShot(t, dir, "marker.jpg", now)

	srv, _ := newTestServer(t, []Pending{
		{ReqID: "c1", Target: "Claude", Kind: kindClip, Seconds: 5, RequestedAt: now.Unix()},
	})
	p := New(srv.URL, "tok", dir, nil) // no recorder

	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.shippedReq["c1"] {
		t.Error("clip request should be marked handled so it isn't retried each tick")
	}
}

// An empty Kind comes from a server predating Phase 3: treat it as a screenshot.
func TestEmptyKindTreatedAsScreenshot(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	writeShot(t, dir, "shot.jpg", now)

	srv, sh := newTestServer(t, []Pending{
		{ReqID: "r1", Target: "Claude", Kind: "", RequestedAt: now.Unix()},
	})
	p := New(srv.URL, "tok", dir, nil)
	if err := p.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.endpoint != "/v1/observer/screenshot" {
		t.Fatalf("endpoint = %q, want the screenshot path", sh.endpoint)
	}
}

func TestAwaitMarkerTimesOut(t *testing.T) {
	p := New("http://x", "tok", t.TempDir(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.awaitMarker(ctx, time.Now()); err == nil {
		t.Fatal("expected an error when no marker ever appears")
	}
}

// A 503 means the observer feature is off server-side: a no-op, not an error.
func TestFetchPendingTolerates503(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	p := New(srv.URL, "tok", t.TempDir(), nil)
	got, err := p.fetchPending(context.Background())
	if err != nil || got != nil {
		t.Fatalf("fetchPending = %v, %v; want nil, nil", got, err)
	}
}
