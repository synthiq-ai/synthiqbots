// Package artifact is the on-disk store for hosted capture artifacts (tactical
// maps, observer screenshots, video clips). It mirrors the date-sharding of the
// feedback image store but adds an unguessable per-file token used as a
// capability: the public serve route (GET /v1/artifacts/<relpath>) is reachable
// without a bearer, so the 32-hex-char token in the filename — combined with
// the host-level Traefik IP allowlist — is what keeps an artifact private.
package artifact

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Store writes artifacts under Dir and builds public URLs against BaseURL.
// A zero Dir means the feature is unconfigured (Enabled reports false and the
// serve/save paths 503 / refuse).
type Store struct {
	Dir     string // base directory, e.g. /artifacts (a compose bind mount)
	BaseURL string // public origin, e.g. https://ops.wow.example.com (no trailing /)
}

// Enabled reports whether a storage dir is configured.
func (s *Store) Enabled() bool { return s != nil && s.Dir != "" }

// token returns 16 random bytes hex-encoded — the per-artifact capability.
func token() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// sanitizeExt normalises a caller-supplied extension to a safe, leading-dot
// form (".png"). Unknown/empty extensions fall back to ".bin".
func sanitizeExt(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	ext = strings.TrimPrefix(ext, ".")
	for _, r := range ext {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') {
			return ".bin"
		}
	}
	if ext == "" || len(ext) > 5 {
		return ".bin"
	}
	return "." + ext
}

// Save writes data and returns the storage-relative path plus the public URL.
// kind is a short label folded into the filename for human legibility
// (e.g. "tacmap", "shot", "clip"); ext is the file extension (with or without
// the dot). Layout on disk: <Dir>/YYYY/MM/DD/<kind>_<token><ext>.
func (s *Store) Save(kind, ext string, data []byte) (relPath, publicURL string, err error) {
	if !s.Enabled() {
		return "", "", fmt.Errorf("artifact store not configured")
	}
	tok, err := token()
	if err != nil {
		return "", "", err
	}
	now := time.Now().UTC()
	dayDir := filepath.Join(now.Format("2006"), now.Format("01"), now.Format("02"))
	baseDir := filepath.Join(s.Dir, dayDir)
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir: %w", err)
	}
	name := fmt.Sprintf("%s_%s%s", safeKind(kind), tok, sanitizeExt(ext))
	full := filepath.Join(baseDir, name)
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return "", "", fmt.Errorf("write: %w", err)
	}
	relPath = filepath.ToSlash(filepath.Join(dayDir, name))
	return relPath, s.BaseURL + "/v1/artifacts/" + relPath, nil
}

// Open resolves a storage-relative path and opens the file for reading,
// refusing anything that escapes Dir (path traversal) or isn't a regular file.
func (s *Store) Open(relPath string) (*os.File, error) {
	if !s.Enabled() {
		return nil, fmt.Errorf("artifact store not configured")
	}
	clean := filepath.Clean("/" + filepath.FromSlash(relPath)) // force absolute, collapse ..
	full := filepath.Join(s.Dir, clean)
	// Defence in depth: the joined path must stay within Dir.
	rel, err := filepath.Rel(s.Dir, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("path escapes artifact dir")
	}
	info, err := os.Stat(full)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	return os.Open(full)
}

// safeKind keeps only [a-z0-9-] in the human label, capped short.
func safeKind(s string) string {
	s = strings.ToLower(s)
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return "artifact"
	}
	if len(out) > 24 {
		out = out[:24]
	}
	return string(out)
}
