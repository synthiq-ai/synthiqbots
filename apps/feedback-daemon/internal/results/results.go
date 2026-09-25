// Package results pulls dispatcher-resolved feedback rows from ops-api's
// `GET /v1/feedback/results?since=<id>` endpoint and writes them to a
// SavedVariables sidecar file the addon reads.
//
// The file lives at:
//   <wow>/WTF/Account/<acc>/SavedVariables/SynthiqBotsUIResults.lua
//
// The addon declares `SynthiqBotsUIResults` as a SavedVariable in its
// .toc and reads it on `ADDON_LOADED`. The addon never mutates the
// global, so when the user `/reload`s, WoW writes the same value back
// to disk — no race with the daemon's last write.
//
// Race window: if the daemon writes WHILE WoW is mid-`/reload` the
// daemon's content can be lost. In practice the daemon polls every
// poll_interval_ms with the addon idle, so this is rare and harmless
// (next tick re-fetches the same rows).
package results

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ServerRow mirrors the JSON shape ops-api returns under `rows[]`.
type ServerRow struct {
	ID            int64  `json:"id"`
	AddonID       string `json:"addonId"`
	AddonTs       int64  `json:"addonTs"`
	Status        string `json:"status"`
	AgentResponse string `json:"agentResponse"`
	AgentActions  string `json:"agentActions"`
	AgentPRURL    string `json:"agentPrUrl"`
	ResolvedAt    string `json:"resolvedAt"`
}

type Page struct {
	Rows      []ServerRow `json:"rows"`
	NextSince int64       `json:"nextSince"`
	Count     int         `json:"count"`
}

// Client is the HTTP wrapper for the results endpoint. Reused per tick.
type Client struct {
	APIURL string
	Bearer string
	HTTP   *http.Client
}

func NewClient(apiURL, bearer string) *Client {
	return &Client{
		APIURL: strings.TrimRight(apiURL, "/"),
		Bearer: bearer,
		HTTP:   &http.Client{Timeout: 30 * time.Second},
	}
}

// Fetch pulls the next page of results since cursor. Returns the parsed
// page or a transport / decode error.
func (c *Client) Fetch(ctx context.Context, since int64, limit int) (*Page, error) {
	url := fmt.Sprintf("%s/v1/feedback/results?since=%d&limit=%d", c.APIURL, since, limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Bearer)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("server %d: %s", resp.StatusCode, snippet(body))
	}
	var p Page
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("decode: %w (body: %s)", err, snippet(body))
	}
	return &p, nil
}

// WriteSavedVariables serializes results to the addon's SavedVariables
// format and writes them atomically (tmp + rename) to the path.
//
// Format produced:
//
//   SynthiqBotsUIResults = {
//       ["1745673600-4823"] = {
//           id = "1745673600-4823",
//           db_id = 42,
//           status = "resolved",
//           summary = "raise tactical heartbeat",
//           pr_url = "",
//           resolved_at = "2026-04-26T14:00:03Z",
//           actions = [[ {"action":"config_tweak",...} ]],
//       },
//       -- next entry
//   }
//
// The actions JSON is wrapped in a Lua long-bracket string so embedded
// quotes don't need escaping; we pick a safe equals-count by scanning
// for the longest `]==]`-style sequence in the JSON.
func WriteSavedVariables(path string, rows map[string]ServerRow) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	var b strings.Builder
	b.WriteString("-- SynthiqBotsUIResults — written by feedback-daemon. DO NOT EDIT.\n")
	b.WriteString("-- This file is loaded by the SynthiqBotsUI addon on ADDON_LOADED.\n")
	b.WriteString("SynthiqBotsUIResults = {\n")

	// Iterate in addon-id order for deterministic file content.
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sortKeys(keys)

	for _, k := range keys {
		r := rows[k]
		fmt.Fprintf(&b, "    [%s] = {\n", luaQuote(k))
		fmt.Fprintf(&b, "        id = %s,\n", luaQuote(r.AddonID))
		fmt.Fprintf(&b, "        db_id = %d,\n", r.ID)
		fmt.Fprintf(&b, "        addon_ts = %d,\n", r.AddonTs)
		fmt.Fprintf(&b, "        status = %s,\n", luaQuote(r.Status))
		fmt.Fprintf(&b, "        summary = %s,\n", luaQuote(r.AgentResponse))
		fmt.Fprintf(&b, "        pr_url = %s,\n", luaQuote(r.AgentPRURL))
		fmt.Fprintf(&b, "        resolved_at = %s,\n", luaQuote(r.ResolvedAt))
		if r.AgentActions != "" {
			fmt.Fprintf(&b, "        actions = %s,\n", luaLongBracket(r.AgentActions))
		}
		b.WriteString("    },\n")
	}
	b.WriteString("}\n")

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// luaQuote escapes a Go string for Lua double-quoted form. Backslashes,
// double-quotes, newlines, and control bytes are escaped numerically.
func luaQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString("\\\\")
		case r == '"':
			b.WriteString("\\\"")
		case r == '\n':
			b.WriteString("\\n")
		case r == '\r':
			b.WriteString("\\r")
		case r == '\t':
			b.WriteString("\\t")
		case r < 0x20:
			fmt.Fprintf(&b, "\\%03d", r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// luaLongBracket wraps a string in Lua's long-bracket form, picking an
// equals-count that doesn't appear inside the string. Lets us embed
// arbitrary JSON without escaping every quote.
func luaLongBracket(s string) string {
	for n := 0; n < 16; n++ {
		eq := strings.Repeat("=", n)
		closer := "]" + eq + "]"
		if !strings.Contains(s, closer) {
			return "[" + eq + "[" + s + closer
		}
	}
	// Pathological — fall back to escaped double-quote form.
	return luaQuote(s)
}

func sortKeys(s []string) {
	// stdlib sort.Strings without dragging in the import; tiny payloads.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

func snippet(b []byte) string {
	const max = 200
	s := string(b)
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}
