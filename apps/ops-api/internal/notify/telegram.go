package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"strings"
)

// telegramClient pushes an artifact to a single Telegram chat via the Bot API.
// One bot token + one destination chat id. The Bot API takes multipart uploads
// at https://api.telegram.org/bot<token>/<method>; sendPhoto for stills,
// sendVideo for clips.
type telegramClient struct {
	token   string
	chatID  string
	http    *http.Client
	apiBase string // overridable in tests; defaults to https://api.telegram.org
}

func (c *telegramClient) base() string {
	if c.apiBase != "" {
		return strings.TrimRight(c.apiBase, "/")
	}
	return "https://api.telegram.org"
}

// push uploads the artifact. Telegram limits bot photo uploads to ~10 MB and
// video to ~50 MB; our artifacts are well under that, so we send the bytes
// directly rather than passing a URL (the hosted URL is bearer/IP-gated and
// Telegram's fetcher couldn't reach it anyway).
func (c *telegramClient) push(ctx context.Context, a Artifact) error {
	method, field := "sendPhoto", "photo"
	if a.Kind == KindVideo {
		method, field = "sendVideo", "video"
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("chat_id", c.chatID)
	if cap := captionWithURL(a); cap != "" {
		_ = mw.WriteField("caption", cap)
	}
	fw, err := mw.CreateFormFile(field, a.Filename)
	if err != nil {
		return fmt.Errorf("telegram form file: %w", err)
	}
	if _, err := fw.Write(a.Bytes); err != nil {
		return fmt.Errorf("telegram form write: %w", err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("telegram form close: %w", err)
	}

	url := fmt.Sprintf("%s/bot%s/%s", c.base(), c.token, method)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()

	// The Bot API returns 200 with {"ok":false,"description":...} on logical
	// errors (bad chat_id, file too big), so we must read the body, not just
	// the status.
	var out struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("telegram %s: status %d, undecodable body: %w", method, resp.StatusCode, err)
	}
	if !out.OK {
		return fmt.Errorf("telegram %s rejected: %s", method, out.Description)
	}
	return nil
}

// captionWithURL appends the hosted URL on its own line so the user can also
// open the full-resolution artifact in a browser.
func captionWithURL(a Artifact) string {
	switch {
	case a.Caption != "" && a.URL != "":
		return a.Caption + "\n" + a.URL
	case a.URL != "":
		return a.URL
	default:
		return a.Caption
	}
}
