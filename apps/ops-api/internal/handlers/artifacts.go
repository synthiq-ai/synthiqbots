package handlers

// Static serving for hosted capture artifacts (tactical maps, observer
// screenshots, video clips). The route is intentionally OUTSIDE the bearer
// group so a returned URL is clickable in a browser; the security model is the
// unguessable 32-hex-char token baked into each filename plus the host-level
// Traefik IP allowlist (same gate as the rest of ops.wow.example.com).
//
// Storage/layout lives in internal/artifact. See docs/capture.md.

import (
	"io"
	"mime"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
)

// ArtifactServe streams a stored artifact by its storage-relative path
// (everything after /v1/artifacts/). 404 for missing/again-traversal paths,
// 503 when the store is unconfigured.
func (s *Server) ArtifactServe(w http.ResponseWriter, r *http.Request) {
	if s.Artifacts == nil || !s.Artifacts.Enabled() {
		writeError(w, http.StatusServiceUnavailable, "artifact store not configured")
		return
	}
	rel := chi.URLParam(r, "*")
	if rel == "" {
		writeError(w, http.StatusNotFound, "no artifact path")
		return
	}
	f, err := s.Artifacts.Open(rel)
	if err != nil {
		// Don't leak whether it was traversal vs missing — both are 404 to a caller.
		writeError(w, http.StatusNotFound, "artifact not found")
		return
	}
	defer f.Close()

	if ct := contentTypeForPath(rel); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// Artifacts are immutable (token-named) — let clients cache aggressively.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// contentTypeForPath maps the file extension to a MIME type, covering the
// formats the capture pipeline produces (PNG maps, JPG/TGA screenshots, MP4/GIF
// clips) before falling back to the stdlib table.
func contentTypeForPath(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".tga":
		return "image/x-tga"
	case ".gif":
		return "image/gif"
	case ".mp4":
		return "video/mp4"
	case ".webp":
		return "image/webp"
	default:
		return mime.TypeByExtension(path.Ext(p))
	}
}
