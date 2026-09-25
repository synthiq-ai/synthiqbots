package handlers

// In-game feedback ingest. The SynthiqBotsUI addon captures a screenshot +
// chat ring buffer + game context into SavedVariables; a Windows companion
// daemon (PR 3) tails that file, matches the screenshot in Screenshots/, and
// POSTs both here as multipart/form-data. We persist the image to disk and
// insert one row into mod_ollama_chat_feedback (status='pending') for the
// PR 4 dispatch worker to pick up.
//
// Design: docs/feedback-loop.md.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// feedbackMeta is the JSON payload the daemon sends as the `meta` text part.
// All fields are optional except `id` and `addonTs` so the row stays useful
// even when the addon failed to populate ctx (e.g. captured at the login
// screen).
type feedbackMeta struct {
	ID          string          `json:"id"`
	AddonTs     int64           `json:"addonTs"`
	Note        string          `json:"note"`
	CharName    string          `json:"charName"`
	CharRealm   string          `json:"charRealm"`
	CharClass   string          `json:"charClass"`
	CharLvl     int             `json:"charLvl"`
	Zone        string          `json:"zone"`
	ChatHistory json.RawMessage `json:"chatHistory"`
	Ctx         json.RawMessage `json:"ctx"`
}

// extByMime picks a sensible filename suffix for a screenshot. WoW's classic
// client writes either .tga (lossless, default) or .jpg if the user toggled
// it on; either way the daemon forwards the actual content-type.
func extByMime(mime string) string {
	switch strings.ToLower(mime) {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "video/mp4":
		// Slack types uploads by filename extension, so a clip that lands as
		// .bin uploads fine and then refuses to preview.
		return ".mp4"
	case "image/x-tga", "image/tga", "application/octet-stream":
		// Old WoW clients write raw TGAs the browser won't volunteer a MIME for;
		// the daemon should explicitly set image/x-tga. Fall back to .tga.
		return ".tga"
	default:
		return ".bin"
	}
}

// randSuffix returns 4 hex bytes for de-conflicting filenames in the rare
// case two captures land in the same second with the same addon id.
func randSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// safeID strips path-traversal characters from the addon-supplied id before
// it gets baked into a filename.
func safeID(s string) string {
	if s == "" {
		return "noid"
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return string(out)
}

// FeedbackIngest accepts a multipart POST: `meta` (text, JSON) + optional
// `screenshot` (file). On success returns {id, dbId, status:"pending",
// imagePath}. The dbId lets the daemon update its local sidecar state.
func (s *Server) FeedbackIngest(w http.ResponseWriter, r *http.Request) {
	if s.Cfg.FeedbackStorageDir == "" {
		writeError(w, http.StatusServiceUnavailable, "feedback storage not configured")
		return
	}
	if s.DBExec == nil {
		writeError(w, http.StatusServiceUnavailable, "feedback ingest requires DB_EXEC_DSN")
		return
	}

	// Multipart parser cap: meta JSON + image. Add a small slack for boundary
	// overhead. The image is the dominant term.
	maxBytes := int64(s.Cfg.FeedbackMaxImageMB)*1024*1024 +
		int64(s.Cfg.FeedbackMaxMetaKB)*1024 + 16*1024
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := r.ParseMultipartForm(int64(s.Cfg.FeedbackMaxMetaKB) * 1024); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("multipart parse: %v", err))
		return
	}

	metaStr := r.FormValue("meta")
	if metaStr == "" {
		writeError(w, http.StatusBadRequest, "meta field required")
		return
	}
	if len(metaStr) > s.Cfg.FeedbackMaxMetaKB*1024 {
		writeError(w, http.StatusBadRequest, "meta exceeds OPS_FEEDBACK_MAX_META_KB")
		return
	}
	var meta feedbackMeta
	if err := json.Unmarshal([]byte(metaStr), &meta); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("meta json: %v", err))
		return
	}
	if meta.ID == "" {
		writeError(w, http.StatusBadRequest, "meta.id required")
		return
	}

	// Optional image. Daemon may upload meta-only if the screenshot file went
	// missing (player deleted Screenshots/ between capture and upload).
	var imageRel string
	var imageBytes int64
	var imageMime string
	if files := r.MultipartForm.File["screenshot"]; len(files) > 0 {
		fh := files[0]
		f, err := fh.Open()
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("screenshot open: %v", err))
			return
		}
		defer f.Close()

		imageMime = fh.Header.Get("Content-Type")
		now := time.Now().UTC()
		dayDir := filepath.Join(now.Format("2006"), now.Format("01"), now.Format("02"))
		baseDir := filepath.Join(s.Cfg.FeedbackStorageDir, dayDir)
		if err := os.MkdirAll(baseDir, 0o755); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("mkdir: %v", err))
			return
		}

		fname := fmt.Sprintf("%d_%s_%s%s",
			meta.AddonTs, safeID(meta.ID), randSuffix(), extByMime(imageMime))
		fullPath := filepath.Join(baseDir, fname)
		out, err := os.OpenFile(fullPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("create: %v", err))
			return
		}
		n, err := io.Copy(out, f)
		_ = out.Close()
		if err != nil {
			_ = os.Remove(fullPath)
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("write: %v", err))
			return
		}
		imageBytes = n
		imageRel = filepath.Join(dayDir, fname)
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.Cfg.RequestTimeout)
	defer cancel()

	chatHistoryStr := ""
	if len(meta.ChatHistory) > 0 {
		chatHistoryStr = string(meta.ChatHistory)
	}
	ctxStr := ""
	if len(meta.Ctx) > 0 {
		ctxStr = string(meta.Ctx)
	}

	res, err := s.DBExec.ExecContext(ctx,
		"INSERT INTO mod_ollama_chat_feedback "+
			"(addon_id, addon_ts, char_name, char_realm, char_class, char_lvl, zone, "+
			"note, chat_history, ctx_json, image_path, image_bytes, image_mime, status) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')",
		meta.ID, meta.AddonTs,
		meta.CharName, meta.CharRealm, meta.CharClass, meta.CharLvl, meta.Zone,
		meta.Note, nullableJSON(chatHistoryStr), nullableJSON(ctxStr),
		imageRel, imageBytes, imageMime)
	if err != nil {
		// Best-effort cleanup of the screenshot we just wrote — without the DB
		// row the file is unreachable from the dispatch worker.
		if imageRel != "" {
			_ = os.Remove(filepath.Join(s.Cfg.FeedbackStorageDir, imageRel))
		}
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("db insert: %v", err))
		return
	}
	dbID, _ := res.LastInsertId()

	writeJSON(w, http.StatusOK, map[string]any{
		"id":         meta.ID,
		"dbId":       dbID,
		"status":     "pending",
		"imagePath":  imageRel,
		"imageBytes": imageBytes,
	})
}

// nullableJSON returns the JSON string for a non-empty input or nil so the
// driver writes SQL NULL for empty fields. Keeps audit queries cleaner.
func nullableJSON(s string) any {
	if s == "" {
		return nil
	}
	return s
}
