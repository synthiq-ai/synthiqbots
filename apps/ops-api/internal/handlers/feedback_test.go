package handlers

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/config"
)

// makeServer wires a minimal *Server pointing at the supplied storage dir +
// optional DB. Mirrors the dependency bundle main.go builds.
func makeServer(t *testing.T, storageDir string) (*Server, sqlmock.Sqlmock) {
	t.Helper()
	cfg := &config.Config{
		FeedbackStorageDir: storageDir,
		FeedbackMaxImageMB: 12,
		FeedbackMaxMetaKB:  256,
		RequestTimeout:     5 * time.Second,
	}
	if storageDir == "" {
		return NewServer(&Deps{Cfg: cfg}), nil
	}
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewServer(&Deps{Cfg: cfg, DBExec: db}), mock
}

// buildMultipart returns a multipart body + content-type header for a
// {meta JSON, optional screenshot bytes} pair.
func buildMultipart(t *testing.T, meta map[string]any, image []byte, mime string) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)

	metaBytes, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	if err := mw.WriteField("meta", string(metaBytes)); err != nil {
		t.Fatalf("write meta: %v", err)
	}

	if image != nil {
		h := make(map[string][]string)
		h["Content-Disposition"] = []string{`form-data; name="screenshot"; filename="ss.tga"`}
		h["Content-Type"] = []string{mime}
		fw, err := mw.CreatePart(textprotoHeader(h))
		if err != nil {
			t.Fatalf("create part: %v", err)
		}
		if _, err := io.Copy(fw, bytes.NewReader(image)); err != nil {
			t.Fatalf("copy image: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return body, mw.FormDataContentType()
}

// textprotoHeader is a tiny shim so we don't have to pull in net/textproto for
// the one MIMEHeader instantiation.
func textprotoHeader(h map[string][]string) map[string][]string { return h }

func TestFeedback503WhenStorageMissing(t *testing.T) {
	s, _ := makeServer(t, "")
	body, ct := buildMultipart(t, map[string]any{"id": "x"}, nil, "")
	req := httptest.NewRequest(http.MethodPost, "/v1/feedback", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	s.FeedbackIngest(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d want 503; body=%s", rr.Code, rr.Body.String())
	}
}

func TestFeedback503WhenDBMissing(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{FeedbackStorageDir: dir, FeedbackMaxImageMB: 12, FeedbackMaxMetaKB: 256, RequestTimeout: 5 * time.Second}
	s := NewServer(&Deps{Cfg: cfg}) // no DBExec
	body, ct := buildMultipart(t, map[string]any{"id": "x"}, nil, "")
	req := httptest.NewRequest(http.MethodPost, "/v1/feedback", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	s.FeedbackIngest(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d want 503; body=%s", rr.Code, rr.Body.String())
	}
}

func TestFeedback400WhenMetaMissing(t *testing.T) {
	dir := t.TempDir()
	s, _ := makeServer(t, dir)
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/feedback", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	s.FeedbackIngest(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestFeedback400WhenMetaJSONInvalid(t *testing.T) {
	dir := t.TempDir()
	s, _ := makeServer(t, dir)
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("meta", "{not json")
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/v1/feedback", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	s.FeedbackIngest(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestFeedback400WhenIDMissing(t *testing.T) {
	dir := t.TempDir()
	s, _ := makeServer(t, dir)
	body, ct := buildMultipart(t, map[string]any{"addonTs": 1}, nil, "")
	req := httptest.NewRequest(http.MethodPost, "/v1/feedback", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	s.FeedbackIngest(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestFeedbackHappyPath(t *testing.T) {
	dir := t.TempDir()
	s, mock := makeServer(t, dir)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO mod_ollama_chat_feedback")).
		WithArgs(
			"abc-123",                  // addon_id
			int64(1745673600),          // addon_ts
			"the operator",                    // char_name
			"SynthiqEU",                // char_realm
			"PRIEST",                   // char_class
			47,                         // char_lvl
			"Stranglethorn Vale",       // zone
			"bot pulled twice",         // note
			sqlmock.AnyArg(),           // chat_history
			sqlmock.AnyArg(),           // ctx_json
			sqlmock.AnyArg(),           // image_path
			sqlmock.AnyArg(),           // image_bytes
			"image/x-tga",              // image_mime
		).
		WillReturnResult(sqlmock.NewResult(42, 1))

	imgBytes := []byte("\x00\x00\x02") // tiny "TGA"
	meta := map[string]any{
		"id":          "abc-123",
		"addonTs":     1745673600,
		"note":        "bot pulled twice",
		"charName":    "the operator",
		"charRealm":   "SynthiqEU",
		"charClass":   "PRIEST",
		"charLvl":     47,
		"zone":        "Stranglethorn Vale",
		"chatHistory": []map[string]any{{"ts": 1, "channel": "PARTY", "sender": "Geek", "text": "pulling"}},
		"ctx":         map[string]any{"target": "Bloodscalp Hunter|Hunter|46|82"},
	}
	body, ct := buildMultipart(t, meta, imgBytes, "image/x-tga")
	req := httptest.NewRequest(http.MethodPost, "/v1/feedback", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	s.FeedbackIngest(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rr.Code, rr.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["status"] != "pending" {
		t.Errorf("status: %v", resp)
	}
	if resp["dbId"].(float64) != 42 {
		t.Errorf("dbId: %v", resp)
	}
	imagePath, _ := resp["imagePath"].(string)
	if imagePath == "" || !strings.HasSuffix(imagePath, ".tga") {
		t.Errorf("imagePath: %q", imagePath)
	}

	// Image written to disk where promised
	full := filepath.Join(dir, imagePath)
	if _, err := os.Stat(full); err != nil {
		t.Fatalf("image not on disk: %v", err)
	}
	got, _ := os.ReadFile(full)
	if !bytes.Equal(got, imgBytes) {
		t.Errorf("image bytes mismatch")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}

func TestFeedbackMetaOnlyAccepted(t *testing.T) {
	dir := t.TempDir()
	s, mock := makeServer(t, dir)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO mod_ollama_chat_feedback")).
		WillReturnResult(sqlmock.NewResult(7, 1))

	meta := map[string]any{"id": "no-image", "addonTs": 1}
	body, ct := buildMultipart(t, meta, nil, "")
	req := httptest.NewRequest(http.MethodPost, "/v1/feedback", body)
	req.Header.Set("Content-Type", ct)
	rr := httptest.NewRecorder()
	s.FeedbackIngest(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp["imagePath"] != "" {
		t.Errorf("imagePath: %v (should be empty for meta-only)", resp["imagePath"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock: %v", err)
	}
}
