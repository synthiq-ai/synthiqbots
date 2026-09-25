package upload

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/synthiq-ai/synthiqbots/apps/feedback-daemon/internal/saves"
)

// fakeServer mirrors the contract from apps/ops-api/internal/handlers/feedback.go.
// Records the parsed multipart for assertions.
type captured struct {
	bearer  string
	meta    map[string]any
	gotImg  bool
	imgMime string
	imgBody []byte
}

func newServer(t *testing.T, status int, resp Result, cap *captured) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.bearer = r.Header.Get("Authorization")
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = json.Unmarshal([]byte(r.FormValue("meta")), &cap.meta)
		if files := r.MultipartForm.File["screenshot"]; len(files) > 0 {
			cap.gotImg = true
			cap.imgMime = files[0].Header.Get("Content-Type")
			f, _ := files[0].Open()
			cap.imgBody, _ = io.ReadAll(f)
			_ = f.Close()
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestSendWithImage(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "WoWScrnShot.tga")
	imgBody := []byte("\x00\x00\x02fake-tga-bytes")
	if err := os.WriteFile(imgPath, imgBody, 0o644); err != nil {
		t.Fatal(err)
	}

	cap := &captured{}
	srv := newServer(t, http.StatusOK,
		Result{ID: "id-1", DBId: 99, Status: "pending", ImagePath: "2026/04/26/foo.tga", ImageBytes: int64(len(imgBody))}, cap)
	defer srv.Close()

	c := New(srv.URL, "secret-token")
	res, err := c.Send(saves.Entry{
		ID:       "id-1",
		AddonTs:  1700000000,
		Note:     "boss pulled adds",
		CharName: "the operator",
		Zone:     "Stranglethorn Vale",
	}, imgPath, "image/x-tga")
	if err != nil {
		t.Fatal(err)
	}
	if res.DBId != 99 {
		t.Errorf("dbId: %d", res.DBId)
	}
	if cap.bearer != "Bearer secret-token" {
		t.Errorf("bearer: %q", cap.bearer)
	}
	if !cap.gotImg {
		t.Error("server did not see screenshot part")
	}
	if cap.imgMime != "image/x-tga" {
		t.Errorf("mime: %q", cap.imgMime)
	}
	if string(cap.imgBody) != string(imgBody) {
		t.Errorf("image bytes mismatch: got %d, want %d", len(cap.imgBody), len(imgBody))
	}
	if cap.meta["id"] != "id-1" {
		t.Errorf("meta.id: %v", cap.meta["id"])
	}
	// Empty optional fields should not appear in meta.
	if _, ok := cap.meta["charLvl"]; ok {
		t.Error("zero charLvl leaked into meta")
	}
}

func TestSendMetaOnly(t *testing.T) {
	cap := &captured{}
	srv := newServer(t, http.StatusOK,
		Result{ID: "id-2", DBId: 5, Status: "pending"}, cap)
	defer srv.Close()

	c := New(srv.URL, "x")
	_, err := c.Send(saves.Entry{ID: "id-2", AddonTs: 1700000001}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if cap.gotImg {
		t.Error("did not expect a screenshot part")
	}
}

func TestSendBubblesServerError(t *testing.T) {
	cap := &captured{}
	srv := newServer(t, http.StatusInternalServerError, Result{}, cap)
	defer srv.Close()

	c := New(srv.URL, "x")
	_, err := c.Send(saves.Entry{ID: "id-3", AddonTs: 1700000002}, "", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error: %v", err)
	}
}

// Sanity: the multipart writer we build is parseable by Go's std lib,
// matching what ops-api handler does on the server side.
func TestMultipartShapeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "ss.png")
	if err := os.WriteFile(imgPath, []byte("\x89PNG\x0d\x0a"), 0o644); err != nil {
		t.Fatal(err)
	}

	cap := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Custom inspection: parse manually so we test the boundary bits too.
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			switch p.FormName() {
			case "meta":
				body, _ := io.ReadAll(p)
				_ = json.Unmarshal(body, &cap.meta)
			case "screenshot":
				cap.gotImg = true
				cap.imgMime = p.Header.Get("Content-Type")
			}
		}
		_, _ = w.Write([]byte(`{"id":"x","status":"pending"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok")
	if _, err := c.Send(saves.Entry{ID: "x", AddonTs: 1, Note: "hi"}, imgPath, "image/png"); err != nil {
		t.Fatal(err)
	}
	if !cap.gotImg {
		t.Error("manual parser missed screenshot part")
	}
	if cap.imgMime != "image/png" {
		t.Errorf("mime: %q", cap.imgMime)
	}
}

