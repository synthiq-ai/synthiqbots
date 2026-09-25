// Package observer implements the daemon side of the on-demand real-client
// capture loop. It polls ops-api for pending capture requests and ships back
// either a screenshot (Phase 2) or a recorded video clip (Phase 3), tagged with
// the request id.
//
// Why this exists separately from the SavedVariables feedback path: WoW only
// flushes SavedVariables on /reload, so the observer addon can't hand the
// reqId↔file mapping to the daemon in real time. The screenshot FILE, though, is
// written instantly — so we correlate by time: a new image whose mtime is at or
// after a request's requestedAt is that request's shot. Because the MCP tool
// blocks until resolved, at most one capture is in flight at a time, which makes
// newest-after-request matching unambiguous.
//
// Clips reuse that same signal. The addon can't record video, and the daemon
// can't know when the observer's teleport has landed — so the addon's
// Screenshot() doubles as a "the camera is in position" marker. The daemon sees
// that file appear and starts ffmpeg. See docs/capture.md.
package observer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/feedback-daemon/internal/recorder"
)

// matchSlack lets a screenshot whose mtime is a hair before the recorded
// requestedAt (clock skew between ops-api and the gaming PC) still match.
const matchSlack = 5 * time.Second

// Capture kinds, mirroring ops-api's observer.Kind* constants.
const (
	kindShot = "shot"
	kindClip = "clip"
)

// Marker-wait tuning. The daemon's outer poll is every 5s, which is far too
// coarse to start a recording on: by the time we noticed, the interesting moment
// could be half over. So once a clip request is picked up we watch the
// Screenshots/ folder tightly for the addon's ready-marker.
const (
	markerPoll    = 250 * time.Millisecond
	markerTimeout = 20 * time.Second
)

// Pending mirrors one entry of GET /v1/observer/pending.
type Pending struct {
	ReqID       string `json:"reqId"`
	Target      string `json:"target"`
	Kind        string `json:"kind"`    // "shot" | "clip" ("" from an older server => shot)
	Seconds     int    `json:"seconds"` // clip length
	RequestedAt int64  `json:"requestedAt"`
}

type pendingResponse struct {
	Pending []Pending `json:"pending"`
}

// Poller owns the HTTP client and the dedup state for the observer loop.
type Poller struct {
	apiURL         string
	bearer         string
	screenshotsDir string
	http           *http.Client
	rec            recorder.Recorder // nil => clip requests are refused

	mu         sync.Mutex
	shippedReq map[string]bool // reqIds we've already POSTed (within this process)
	usedFiles  map[string]bool // absolute screenshot paths already consumed
	inFlight   map[string]bool // clip reqIds currently recording in a goroutine
}

// New builds a Poller. screenshotsDir is the WoW client's Screenshots/ folder.
// rec may be nil (no ffmpeg installed): screenshots keep working, clips fail
// with a clear message.
func New(apiURL, bearer, screenshotsDir string, rec recorder.Recorder) *Poller {
	return &Poller{
		apiURL:         strings.TrimRight(apiURL, "/"),
		bearer:         bearer,
		screenshotsDir: screenshotsDir,
		http:           &http.Client{Timeout: 5 * time.Minute}, // a clip upload can be tens of MB
		rec:            rec,
		shippedReq:     map[string]bool{},
		usedFiles:      map[string]bool{},
		inFlight:       map[string]bool{},
	}
}

// Tick runs one poll: fetch pending captures and, for each, ship a screenshot or
// kick off a clip recording. Errors are logged per-request, not fatal.
//
// Tick must return promptly: it shares the daemon's ticker with the feedback
// scan and the results poll, so a 30s recording is spawned into a goroutine
// rather than blocking those.
func (p *Poller) Tick(ctx context.Context) error {
	pending, err := p.fetchPending(ctx)
	if err != nil {
		return err
	}
	for _, pc := range pending {
		if p.claimed(pc.ReqID) {
			continue
		}
		if pc.Kind == kindClip {
			p.startClip(ctx, pc)
			continue
		}
		p.handleShot(ctx, pc)
	}
	return nil
}

// claimed reports whether this reqId is already shipped or being recorded.
func (p *Poller) claimed(reqID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.shippedReq[reqID] || p.inFlight[reqID]
}

// handleShot is the Phase 2 path: the newest fresh screenshot IS the artifact.
func (p *Poller) handleShot(ctx context.Context, pc Pending) {
	shot, mime := p.matchAfter(p.after(pc))
	if shot == "" {
		slog.Debug("observer: no screenshot yet", "reqId", pc.ReqID, "target", pc.Target)
		return
	}
	if err := p.ship(ctx, pc.ReqID, shot, mime, "/v1/observer/screenshot", "screenshot"); err != nil {
		slog.Error("observer: ship failed", "reqId", pc.ReqID, "shot", shot, "err", err)
		return
	}
	p.mu.Lock()
	p.shippedReq[pc.ReqID] = true
	p.usedFiles[shot] = true
	p.mu.Unlock()
	slog.Info("observer: shipped screenshot", "reqId", pc.ReqID, "target", pc.Target, "shot", filepath.Base(shot))
}

// startClip is the Phase 3 path: the fresh screenshot is only a ready-marker;
// the artifact is a recording made after it appears. Runs detached so the
// daemon's other pollers keep ticking.
func (p *Poller) startClip(ctx context.Context, pc Pending) {
	if p.rec == nil {
		slog.Error("observer: clip requested but ffmpeg is not available",
			"reqId", pc.ReqID, "hint", "set ffmpeg_path in config.toml or drop ffmpeg.exe next to the daemon")
		p.mu.Lock()
		p.shippedReq[pc.ReqID] = true // don't retry every 5s until the server TTL reaps it
		p.mu.Unlock()
		return
	}
	p.mu.Lock()
	p.inFlight[pc.ReqID] = true
	p.mu.Unlock()

	go func() {
		defer func() {
			p.mu.Lock()
			delete(p.inFlight, pc.ReqID)
			p.mu.Unlock()
		}()
		if err := p.recordAndShip(ctx, pc); err != nil {
			slog.Error("observer: clip failed", "reqId", pc.ReqID, "target", pc.Target, "err", err)
			return
		}
		p.mu.Lock()
		p.shippedReq[pc.ReqID] = true
		p.mu.Unlock()
	}()
}

func (p *Poller) recordAndShip(ctx context.Context, pc Pending) error {
	marker, err := p.awaitMarker(ctx, p.after(pc))
	if err != nil {
		return err
	}
	// Consume the marker so the screenshot path never ships it as an artifact.
	p.mu.Lock()
	p.usedFiles[marker] = true
	p.mu.Unlock()
	slog.Info("observer: in position, recording", "reqId", pc.ReqID, "target", pc.Target, "seconds", pc.Seconds)

	tmp, err := os.CreateTemp("", "sbclip_*.mp4")
	if err != nil {
		return fmt.Errorf("temp file: %w", err)
	}
	out := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(out)

	if err := p.rec.Record(ctx, out, pc.Seconds); err != nil {
		return fmt.Errorf("record: %w", err)
	}
	size := int64(0)
	if st, serr := os.Stat(out); serr == nil {
		size = st.Size()
	}
	if err := p.ship(ctx, pc.ReqID, out, "video/mp4", "/v1/observer/clip", "clip"); err != nil {
		return fmt.Errorf("ship: %w", err)
	}
	slog.Info("observer: shipped clip", "reqId", pc.ReqID, "target", pc.Target, "seconds", pc.Seconds, "bytes", size)
	return nil
}

// awaitMarker blocks until the addon's ready-marker screenshot lands, or the
// timeout elapses (observer offline, addon stale, teleport failed).
func (p *Poller) awaitMarker(ctx context.Context, after time.Time) (string, error) {
	deadline := time.Now().Add(markerTimeout)
	for {
		if shot, _ := p.matchAfter(after); shot != "" {
			return shot, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no ready-marker screenshot within %s — is the observer in-world with the addon armed?", markerTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(markerPoll):
		}
	}
}

// after is the earliest mtime that can belong to this request.
func (p *Poller) after(pc Pending) time.Time {
	return time.Unix(pc.RequestedAt, 0).Add(-matchSlack)
}

func (p *Poller) fetchPending(ctx context.Context) ([]Pending, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.apiURL+"/v1/observer/pending", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.bearer)
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusServiceUnavailable {
		// Observer feature not configured server-side — nothing to do.
		return nil, nil
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("pending %d: %s", resp.StatusCode, snippet(body))
	}
	var pr pendingResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("decode pending: %w", err)
	}
	return pr.Pending, nil
}

// matchAfter returns the newest unused image file with mtime >= after.
func (p *Poller) matchAfter(after time.Time) (string, string) {
	entries, err := os.ReadDir(p.screenshotsDir)
	if err != nil {
		return "", ""
	}
	var (
		best     string
		bestMime string
		bestMod  time.Time
	)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		mime := mimeForExt(strings.ToLower(filepath.Ext(e.Name())))
		if mime == "" {
			continue
		}
		full := filepath.Join(p.screenshotsDir, e.Name())
		p.mu.Lock()
		used := p.usedFiles[full]
		p.mu.Unlock()
		if used {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(after) {
			continue
		}
		if best == "" || info.ModTime().After(bestMod) {
			best, bestMime, bestMod = full, mime, info.ModTime()
		}
	}
	return best, bestMime
}

// ship POSTs a captured file back to ops-api as multipart {reqId, <field>}.
func (p *Poller) ship(ctx context.Context, reqID, path, mime, endpoint, field string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	if err := mw.WriteField("reqId", reqID); err != nil {
		return err
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, field, filepath.Base(path)))
	if mime != "" {
		h.Set("Content-Type", mime)
	}
	fw, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	if _, err := io.Copy(fw, f); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiURL+endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.bearer)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("server %d: %s", resp.StatusCode, snippet(rb))
	}
	return nil
}

func mimeForExt(ext string) string {
	switch ext {
	case ".tga":
		return "image/x-tga"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".bmp":
		return "image/bmp"
	}
	return ""
}

func snippet(b []byte) string {
	const max = 200
	s := string(b)
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
