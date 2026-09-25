package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// slackClient uploads an artifact to one Slack channel using the modern
// external-upload flow (the legacy files.upload was deprecated/retired):
//
//  1. files.getUploadURLExternal  -> { upload_url, file_id }
//  2. POST the bytes to upload_url
//  3. files.completeUploadExternal -> shares the file into the channel
//
// Needs a bot token (xoxb-…) with files:write, and the bot must be a member of
// the target channel.
type slackClient struct {
	token   string
	channel string
	http    *http.Client
	apiBase string // overridable in tests; defaults to https://slack.com/api
}

func (c *slackClient) base() string {
	if c.apiBase != "" {
		return strings.TrimRight(c.apiBase, "/")
	}
	return "https://slack.com/api"
}

func (c *slackClient) push(ctx context.Context, a Artifact) error {
	uploadURL, fileID, err := c.getUploadURL(ctx, a.Filename, len(a.Bytes))
	if err != nil {
		return err
	}
	if err := c.putBytes(ctx, uploadURL, a); err != nil {
		return err
	}
	return c.complete(ctx, fileID, a)
}

// getUploadURL is step 1: reserve an upload slot. Returns the one-time upload
// URL and the file id used to complete the share.
func (c *slackClient) getUploadURL(ctx context.Context, filename string, length int) (string, string, error) {
	form := url.Values{}
	form.Set("filename", filename)
	form.Set("length", strconv.Itoa(length))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base()+"/files.getUploadURLExternal", strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("slack getUploadURLExternal: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		OK        bool   `json:"ok"`
		Error     string `json:"error"`
		UploadURL string `json:"upload_url"`
		FileID    string `json:"file_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", fmt.Errorf("slack getUploadURLExternal decode: %w", err)
	}
	if !out.OK {
		return "", "", fmt.Errorf("slack getUploadURLExternal: %s", out.Error)
	}
	return out.UploadURL, out.FileID, nil
}

// putBytes is step 2: upload the raw artifact to the one-time URL as a
// multipart "file" part (the form the Slack SDKs use).
func (c *slackClient) putBytes(ctx context.Context, uploadURL string, a Artifact) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", a.Filename)
	if err != nil {
		return fmt.Errorf("slack upload form: %w", err)
	}
	if _, err := fw.Write(a.Bytes); err != nil {
		return fmt.Errorf("slack upload write: %w", err)
	}
	if err := mw.Close(); err != nil {
		return fmt.Errorf("slack upload close: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("slack upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("slack upload: status %d", resp.StatusCode)
	}
	return nil
}

// complete is step 3: share the uploaded file into the channel with a caption.
func (c *slackClient) complete(ctx context.Context, fileID string, a Artifact) error {
	body := map[string]any{
		"files":      []map[string]string{{"id": fileID, "title": a.Filename}},
		"channel_id": c.channel,
	}
	if cap := captionWithURL(a); cap != "" {
		body["initial_comment"] = cap
	}
	payload, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base()+"/files.completeUploadExternal", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("slack completeUploadExternal: %w", err)
	}
	defer resp.Body.Close()

	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("slack completeUploadExternal decode: %w", err)
	}
	if !out.OK {
		return fmt.Errorf("slack completeUploadExternal: %s", out.Error)
	}
	return nil
}
