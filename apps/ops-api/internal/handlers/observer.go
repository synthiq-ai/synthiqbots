package handlers

// Observer capture REST surface (Phases 2 and 3 of the capture feature). The
// feedback-daemon (running on the gaming PC next to a real WoW client) drives
// these endpoints:
//
//	GET  /v1/observer/pending      → outstanding capture requests to fulfil
//	POST /v1/observer/screenshot   → ship the captured image back, tagged with reqId
//	POST /v1/observer/clip         → ship the recorded MP4 back, tagged with reqId
//
// ops-api stores the media as a hosted artifact, pushes it out-of-band
// (Telegram/Slack), and resolves the in-memory queue so the waiting
// request_observer_screenshot MCP tool returns the URL — or, for a clip, so the
// URL is retrievable via get_observer_clip. See docs/capture.md.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/notify"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/observer"
)

// ingestSpec describes the media-specific bits of an observer upload. Both
// endpoints are the same pipeline — parse, store, push, resolve — differing only
// in the multipart field, artifact kind, and how long the push may take.
type ingestSpec struct {
	kind         string // observer.KindShot | observer.KindClip
	fileField    string // multipart file field name
	artifactKind string // artifact filename prefix
	filePrefix   string // pushed filename prefix
	caption      string
	notifyKind   notify.Kind
	maxMB        int
	pushTimeout  time.Duration
}

// ObserverPending returns the list of captures awaiting media. The daemon polls
// this; for each entry it watches Screenshots/ for a new file with mtime >=
// requestedAt. A "shot" is POSTed straight back; for a "clip" that file is only
// the ready-marker and the daemon records video before POSTing.
func (s *Server) ObserverPending(w http.ResponseWriter, r *http.Request) {
	if s.Observer == nil {
		writeError(w, http.StatusServiceUnavailable, "observer capture not configured (OPS_OBSERVER_CHAR empty)")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pending": s.Observer.Pending()})
}

// ObserverScreenshot ingests a captured screenshot for a pending reqId:
// multipart `reqId` (text) + `screenshot` (file).
func (s *Server) ObserverScreenshot(w http.ResponseWriter, r *http.Request) {
	s.observerIngest(w, r, ingestSpec{
		kind:         observer.KindShot,
		fileField:    "screenshot",
		artifactKind: "shot",
		filePrefix:   "observer_",
		caption:      "Observer screenshot",
		notifyKind:   notify.KindImage,
		maxMB:        s.maxArtifactMB(),
		pushTimeout:  30 * time.Second,
	})
}

// ObserverClip ingests a recorded video clip for a pending reqId:
// multipart `reqId` (text) + `clip` (file). Clips are an order of magnitude
// larger than stills, so they get their own size cap and a longer push window.
func (s *Server) ObserverClip(w http.ResponseWriter, r *http.Request) {
	s.observerIngest(w, r, ingestSpec{
		kind:         observer.KindClip,
		fileField:    "clip",
		artifactKind: "clip",
		filePrefix:   "clip_",
		caption:      "Observer clip",
		notifyKind:   notify.KindVideo,
		maxMB:        s.clipMaxMB(),
		pushTimeout:  120 * time.Second,
	})
}

// observerIngest hosts the uploaded media, pushes it out-of-band, and resolves
// the queue. Returns {ok, artifact_url, pushed, resolved}.
func (s *Server) observerIngest(w http.ResponseWriter, r *http.Request, spec ingestSpec) {
	if s.Observer == nil {
		writeError(w, http.StatusServiceUnavailable, "observer capture not configured")
		return
	}
	if s.Artifacts == nil || !s.Artifacts.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "artifact store not configured (OPS_ARTIFACT_DIR empty)")
		return
	}

	maxBytes := int64(spec.maxMB)*1024*1024 + 16*1024
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := r.ParseMultipartForm(1024 * 1024); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("multipart parse: %v", err))
		return
	}
	reqID := strings.TrimSpace(r.FormValue("reqId"))
	if reqID == "" {
		writeError(w, http.StatusBadRequest, "reqId required")
		return
	}

	// Reject an upload aimed at the wrong endpoint. A daemon that predates the
	// clip path would ship a clip request's marker screenshot here and resolve
	// the clip with a still image — a silent, confusing wrong answer. An unknown
	// reqId is NOT a mismatch: the capture may simply have expired, and a late
	// delivery is still worth hosting.
	if got, ok := s.Observer.KindOf(reqID); ok && got != spec.kind {
		writeError(w, http.StatusConflict, fmt.Sprintf(
			"reqId %s is a %q capture, not a %q — is the feedback-daemon up to date?", reqID, got, spec.kind))
		return
	}

	files := r.MultipartForm.File[spec.fileField]
	if len(files) == 0 {
		writeError(w, http.StatusBadRequest, spec.fileField+" file required")
		return
	}
	fh := files[0]
	f, err := fh.Open()
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%s open: %v", spec.fileField, err))
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("read: %v", err))
		return
	}

	mime := fh.Header.Get("Content-Type")
	ext := extByMime(mime)
	rel, url, err := s.Artifacts.Save(spec.artifactKind, ext, data)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("store: %v", err))
		return
	}

	pushed := []string{}
	if s.Notify != nil && s.Notify.Enabled() {
		pctx, cancel := context.WithTimeout(r.Context(), spec.pushTimeout)
		defer cancel()
		pr := s.Notify.Push(pctx, notify.Artifact{
			Kind:     spec.notifyKind,
			Bytes:    data,
			Filename: spec.filePrefix + safeID(reqID) + ext,
			MIME:     mime,
			Caption:  spec.caption,
			URL:      url,
		})
		pushed = pr.Pushed
	}

	// Resolve the queue so a waiting MCP tool unblocks and get_observer_clip can
	// find the URL. A false return means the request already expired (observer
	// was slow) — we still hosted+pushed the media, so report success but flag
	// the late delivery.
	resolved := s.Observer.Resolve(reqID, observer.Result{URL: url, Rel: rel, Pushed: pushed})

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"artifact_url": url,
		"pushed":       pushed,
		"resolved":     resolved,
	})
}

// maxArtifactMB returns the configured artifact size cap, defaulting to 25.
func (s *Server) maxArtifactMB() int {
	if s.Cfg != nil && s.Cfg.ArtifactMaxMB > 0 {
		return s.Cfg.ArtifactMaxMB
	}
	return 25
}

// clipMaxMB returns the video size cap, defaulting to 100. Stills fit in 25MB;
// a 30s 1080p clip does not.
func (s *Server) clipMaxMB() int {
	if s.Cfg != nil && s.Cfg.ClipMaxMB > 0 {
		return s.Cfg.ClipMaxMB
	}
	return 100
}
