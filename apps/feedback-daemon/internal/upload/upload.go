// Package upload posts a feedback entry to the ops-api ingest endpoint as
// multipart/form-data: a `meta` text part (JSON) plus an optional
// `screenshot` file part. Mirrors the contract documented in
// docs/feedback-loop.md PR 2 section.
package upload

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/feedback-daemon/internal/saves"
)

// Result is the parsed JSON body the ingest endpoint returns on 2xx.
type Result struct {
	ID         string `json:"id"`
	DBId       int64  `json:"dbId"`
	Status     string `json:"status"`
	ImagePath  string `json:"imagePath"`
	ImageBytes int64  `json:"imageBytes"`
}

// Client is the daemon's HTTP client wrapper.
type Client struct {
	APIURL string // e.g. https://ops.wow.example.com
	Bearer string
	HTTP   *http.Client
}

// New constructs a Client with sensible HTTP timeouts.
func New(apiURL, bearer string) *Client {
	return &Client{
		APIURL: strings.TrimRight(apiURL, "/"),
		Bearer: bearer,
		HTTP:   &http.Client{Timeout: 60 * time.Second},
	}
}

// Send uploads a single feedback entry. screenshotPath may be empty
// (meta-only upload). Returns the server's parsed response on success.
func (c *Client) Send(entry saves.Entry, screenshotPath, screenshotMime string) (*Result, error) {
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)

	meta := buildMeta(entry)
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return nil, fmt.Errorf("meta marshal: %w", err)
	}
	if err := mw.WriteField("meta", string(metaJSON)); err != nil {
		return nil, fmt.Errorf("write meta field: %w", err)
	}

	if screenshotPath != "" {
		f, err := os.Open(screenshotPath)
		if err != nil {
			return nil, fmt.Errorf("open screenshot: %w", err)
		}
		defer f.Close()

		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition",
			fmt.Sprintf(`form-data; name="screenshot"; filename=%q`,
				filepath.Base(screenshotPath)))
		if screenshotMime != "" {
			h.Set("Content-Type", screenshotMime)
		}
		fw, err := mw.CreatePart(h)
		if err != nil {
			return nil, fmt.Errorf("create part: %w", err)
		}
		if _, err := io.Copy(fw, f); err != nil {
			return nil, fmt.Errorf("copy screenshot: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("close multipart: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.APIURL+"/v1/feedback", body)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Bearer)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("server %d: %s", resp.StatusCode, snippet(respBody))
	}
	var out Result
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w (body: %s)", err, snippet(respBody))
	}
	return &out, nil
}

// buildMeta assembles the JSON `meta` part the server expects (matches
// the feedbackMeta struct in apps/ops-api/internal/handlers/feedback.go).
func buildMeta(e saves.Entry) map[string]any {
	m := map[string]any{
		"id":      e.ID,
		"addonTs": e.AddonTs,
	}
	if e.Note != "" {
		m["note"] = e.Note
	}
	if e.CharName != "" {
		m["charName"] = e.CharName
	}
	if e.CharRealm != "" {
		m["charRealm"] = e.CharRealm
	}
	if e.CharClass != "" {
		m["charClass"] = e.CharClass
	}
	if e.CharLvl > 0 {
		m["charLvl"] = e.CharLvl
	}
	if e.Zone != "" {
		m["zone"] = e.Zone
	}
	if len(e.ChatHistory) > 0 {
		m["chatHistory"] = e.ChatHistory
	}
	if e.Ctx != nil {
		m["ctx"] = e.Ctx
	}
	return m
}

// snippet returns a bounded prefix of a body for error messages.
func snippet(b []byte) string {
	const max = 200
	s := string(b)
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
