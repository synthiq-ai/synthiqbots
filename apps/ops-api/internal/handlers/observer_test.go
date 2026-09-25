package handlers

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/artifact"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/config"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/observer"
)

func newObserverServer(t *testing.T) (*Server, *observer.Queue) {
	t.Helper()
	q := observer.NewQueue(time.Minute)
	srv := NewServer(&Deps{
		Cfg:       &config.Config{ArtifactMaxMB: 25},
		Artifacts: &artifact.Store{Dir: t.TempDir(), BaseURL: "https://ops.example"},
		Observer:  q,
		// Notify nil — handler must be nil-safe.
	})
	return srv, q
}

func TestObserverPending(t *testing.T) {
	srv, q := newObserverServer(t)
	q.Add("r1", "Claude", observer.KindShot, 0, 0)

	rec := httptest.NewRecorder()
	srv.ObserverPending(rec, httptest.NewRequest(http.MethodGet, "/v1/observer/pending", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var out struct {
		Pending []observer.PendingView `json:"pending"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Pending) != 1 || out.Pending[0].ReqID != "r1" {
		t.Fatalf("pending = %+v", out.Pending)
	}
}

func TestObserverScreenshotStoresAndResolves(t *testing.T) {
	srv, q := newObserverServer(t)
	capt := q.Add("r1", "Claude", observer.KindShot, 0, 0)

	// Build multipart body: reqId + a fake screenshot.
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("reqId", "r1")
	fw, _ := mw.CreateFormFile("screenshot", "shot.jpg")
	_, _ = fw.Write([]byte("\xff\xd8\xff\xe0JPEGISH"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/observer/screenshot", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.ObserverScreenshot(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK          bool   `json:"ok"`
		ArtifactURL string `json:"artifact_url"`
		Resolved    bool   `json:"resolved"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.ArtifactURL == "" || !out.Resolved {
		t.Fatalf("out = %+v", out)
	}

	// The waiting MCP tool (simulated by Wait) should now unblock with the URL.
	res, err := q.Wait(req.Context(), capt)
	if err != nil {
		t.Fatalf("wait after resolve: %v", err)
	}
	if res.URL != out.ArtifactURL {
		t.Errorf("resolved URL %q != response URL %q", res.URL, out.ArtifactURL)
	}
}

func TestObserverScreenshotRequiresReqID(t *testing.T) {
	srv, _ := newObserverServer(t)
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	fw, _ := mw.CreateFormFile("screenshot", "shot.jpg")
	_, _ = fw.Write([]byte("x"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/observer/screenshot", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.ObserverScreenshot(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
}

func TestObserverClipStoresAsMP4AndResolves(t *testing.T) {
	srv, q := newObserverServer(t)
	q.Add("c1", "Claude", observer.KindClip, 10, time.Minute)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("reqId", "c1")
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="clip"; filename="clip.mp4"`)
	h.Set("Content-Type", "video/mp4")
	fw, _ := mw.CreatePart(h)
	_, _ = fw.Write([]byte("\x00\x00\x00\x18ftypmp42MP4ISH"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/observer/clip", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.ObserverClip(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK          bool   `json:"ok"`
		ArtifactURL string `json:"artifact_url"`
		Resolved    bool   `json:"resolved"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || !out.Resolved {
		t.Fatalf("out = %+v", out)
	}
	// Slack types uploads by extension — a .bin here means no video preview.
	if !strings.HasSuffix(out.ArtifactURL, ".mp4") {
		t.Errorf("artifact_url = %q, want a .mp4 suffix", out.ArtifactURL)
	}
	// The async tool reads the result via Lookup, not Wait.
	if r, ok := q.Lookup("c1"); !ok || r.URL != out.ArtifactURL {
		t.Errorf("Lookup = %+v, %v; want the hosted URL retained", r, ok)
	}
}

// A daemon predating the clip path would ship a clip's marker screenshot to
// /v1/observer/screenshot, resolving the clip with a still image. Reject it
// loudly rather than answering wrong.
func TestObserverCrossKindUploadRejected(t *testing.T) {
	srv, q := newObserverServer(t)
	q.Add("c1", "Claude", observer.KindClip, 10, time.Minute)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("reqId", "c1")
	fw, _ := mw.CreateFormFile("screenshot", "shot.jpg")
	_, _ = fw.Write([]byte("\xff\xd8\xff\xe0JPEGISH"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/observer/screenshot", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.ObserverScreenshot(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409 (clip reqId posted to the screenshot endpoint)", rec.Code)
	}
	if q.Len() != 1 {
		t.Errorf("rejected upload must leave the capture pending, len = %d", q.Len())
	}
}

// An expired reqId is not a kind mismatch: the media is still worth hosting.
func TestObserverLateDeliveryStillHosted(t *testing.T) {
	srv, _ := newObserverServer(t)

	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("reqId", "gone")
	fw, _ := mw.CreateFormFile("screenshot", "shot.jpg")
	_, _ = fw.Write([]byte("\xff\xd8\xff\xe0JPEGISH"))
	_ = mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/observer/screenshot", body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.ObserverScreenshot(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var out struct {
		OK       bool `json:"ok"`
		Resolved bool `json:"resolved"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.OK || out.Resolved {
		t.Fatalf("out = %+v, want ok with resolved=false", out)
	}
}

func TestObserverDisabledWhenNoQueue(t *testing.T) {
	srv := NewServer(&Deps{Cfg: &config.Config{}}) // no Observer
	rec := httptest.NewRecorder()
	srv.ObserverPending(rec, httptest.NewRequest(http.MethodGet, "/v1/observer/pending", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
}
