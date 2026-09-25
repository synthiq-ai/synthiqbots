package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- Telegram ---

func TestTelegramPushPhotoSuccess(t *testing.T) {
	var gotMethod, gotChatID, gotCaption, gotField string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// URL is /bot<token>/sendPhoto
		if strings.HasSuffix(r.URL.Path, "/sendPhoto") {
			gotMethod = "sendPhoto"
		} else if strings.HasSuffix(r.URL.Path, "/sendVideo") {
			gotMethod = "sendVideo"
		}
		_ = r.ParseMultipartForm(1 << 20)
		gotChatID = r.FormValue("chat_id")
		gotCaption = r.FormValue("caption")
		if _, ok := r.MultipartForm.File["photo"]; ok {
			gotField = "photo"
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c := &telegramClient{token: "T", chatID: "123", http: srv.Client(), apiBase: srv.URL}
	err := c.push(context.Background(), Artifact{
		Kind: KindImage, Bytes: []byte("PNGDATA"), Filename: "m.png",
		MIME: "image/png", Caption: "Durotar", URL: "https://x/y.png",
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if gotMethod != "sendPhoto" {
		t.Errorf("method = %q, want sendPhoto", gotMethod)
	}
	if gotChatID != "123" {
		t.Errorf("chat_id = %q", gotChatID)
	}
	if gotField != "photo" {
		t.Errorf("file field = %q, want photo", gotField)
	}
	if !strings.Contains(gotCaption, "Durotar") || !strings.Contains(gotCaption, "https://x/y.png") {
		t.Errorf("caption = %q, want caption+url", gotCaption)
	}
}

func TestTelegramPushVideoUsesSendVideo(t *testing.T) {
	var gotField string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		if _, ok := r.MultipartForm.File["video"]; ok {
			gotField = "video"
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c := &telegramClient{token: "T", chatID: "1", http: srv.Client(), apiBase: srv.URL}
	if err := c.push(context.Background(), Artifact{Kind: KindVideo, Bytes: []byte("MP4"), Filename: "c.mp4"}); err != nil {
		t.Fatalf("push: %v", err)
	}
	if gotField != "video" {
		t.Errorf("file field = %q, want video", gotField)
	}
}

func TestTelegramPushLogicalError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bot API returns HTTP 200 with ok:false on logical errors.
		_, _ = io.WriteString(w, `{"ok":false,"description":"chat not found"}`)
	}))
	defer srv.Close()

	c := &telegramClient{token: "T", chatID: "bad", http: srv.Client(), apiBase: srv.URL}
	err := c.push(context.Background(), Artifact{Kind: KindImage, Bytes: []byte("x"), Filename: "m.png"})
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("err = %v, want chat-not-found", err)
	}
}

// --- Slack (3-step external upload) ---

func TestSlackPushSuccess(t *testing.T) {
	var sawGetURL, sawUpload, sawComplete bool
	var completeBody map[string]any
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/files.getUploadURLExternal", func(w http.ResponseWriter, r *http.Request) {
		sawGetURL = true
		_ = r.ParseForm()
		if r.FormValue("filename") == "" || r.FormValue("length") == "" {
			t.Errorf("missing filename/length: %v", r.Form)
		}
		_, _ = io.WriteString(w, `{"ok":true,"upload_url":"`+srv.URL+`/upload","file_id":"F1"}`)
	})
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		sawUpload = true
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/files.completeUploadExternal", func(w http.ResponseWriter, r *http.Request) {
		sawComplete = true
		_ = json.NewDecoder(r.Body).Decode(&completeBody)
		_, _ = io.WriteString(w, `{"ok":true}`)
	})

	c := &slackClient{token: "xoxb", channel: "C1", http: srv.Client(), apiBase: srv.URL}
	err := c.push(context.Background(), Artifact{
		Kind: KindImage, Bytes: []byte("PNG"), Filename: "m.png", Caption: "cap", URL: "https://u",
	})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if !sawGetURL || !sawUpload || !sawComplete {
		t.Fatalf("steps: getURL=%v upload=%v complete=%v", sawGetURL, sawUpload, sawComplete)
	}
	if completeBody["channel_id"] != "C1" {
		t.Errorf("channel_id = %v", completeBody["channel_id"])
	}
	if ic, _ := completeBody["initial_comment"].(string); !strings.Contains(ic, "https://u") {
		t.Errorf("initial_comment = %v, want url", completeBody["initial_comment"])
	}
}

func TestSlackPushGetURLError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":false,"error":"not_authed"}`)
	}))
	defer srv.Close()

	c := &slackClient{token: "x", channel: "C1", http: srv.Client(), apiBase: srv.URL}
	err := c.push(context.Background(), Artifact{Kind: KindImage, Bytes: []byte("x"), Filename: "m.png"})
	if err == nil || !strings.Contains(err.Error(), "not_authed") {
		t.Fatalf("err = %v, want not_authed", err)
	}
}

// --- Notifier fan-out ---

func TestNewDisabledWhenNoCreds(t *testing.T) {
	n := New(Options{})
	if n.Enabled() {
		t.Fatal("expected disabled")
	}
	res := n.Push(context.Background(), Artifact{Kind: KindImage, Bytes: []byte("x"), Filename: "m.png"})
	if len(res.Pushed) != 0 || len(res.Errors) != 0 {
		t.Fatalf("expected no-op push, got %+v", res)
	}
}

func TestNotifierFanOutMixedOutcome(t *testing.T) {
	// Telegram succeeds, Slack fails at step 1 — Push records one of each.
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer tg.Close()
	sl := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":false,"error":"channel_not_found"}`)
	}))
	defer sl.Close()

	n := &Notifier{
		telegram: &telegramClient{token: "T", chatID: "1", http: tg.Client(), apiBase: tg.URL},
		slack:    &slackClient{token: "x", channel: "C", http: sl.Client(), apiBase: sl.URL},
	}
	if !n.Enabled() {
		t.Fatal("expected enabled")
	}
	res := n.Push(context.Background(), Artifact{Kind: KindImage, Bytes: []byte("x"), Filename: "m.png"})
	if len(res.Pushed) != 1 || res.Pushed[0] != "telegram" {
		t.Errorf("pushed = %v, want [telegram]", res.Pushed)
	}
	if _, ok := res.Errors["slack"]; !ok {
		t.Errorf("errors = %v, want slack failure", res.Errors)
	}
}
