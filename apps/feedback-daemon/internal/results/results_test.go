package results

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify bearer + cursor are passed through.
		if r.Header.Get("Authorization") != "Bearer t" {
			t.Errorf("bearer: %q", r.Header.Get("Authorization"))
		}
		u, _ := url.Parse(r.URL.String())
		if u.Query().Get("since") != "10" {
			t.Errorf("since: %q", u.Query().Get("since"))
		}
		_ = json.NewEncoder(w).Encode(Page{
			Rows:      []ServerRow{{ID: 11, AddonID: "a", Status: "resolved", AgentResponse: "ok"}},
			NextSince: 11,
			Count:     1,
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "t")
	p, err := c.Fetch(context.Background(), 10, 100)
	if err != nil {
		t.Fatal(err)
	}
	if p.Count != 1 || len(p.Rows) != 1 || p.Rows[0].AddonID != "a" {
		t.Errorf("page: %+v", p)
	}
	if p.NextSince != 11 {
		t.Errorf("nextSince: %d", p.NextSince)
	}
}

func TestFetchBubblesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad bearer"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "t")
	_, err := c.Fetch(context.Background(), 0, 100)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("err: %v", err)
	}
}

func TestWriteSavedVariablesFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SavedVariables", "SynthiqBotsUIResults.lua")
	rows := map[string]ServerRow{
		"abc-1": {
			ID: 1, AddonID: "abc-1", AddonTs: 1700000000, Status: "resolved",
			AgentResponse: `simple "quote" with newline\nhere`,
			AgentActions:  `{"action":"config_tweak","reasoning":"because"}`,
			AgentPRURL:    "",
			ResolvedAt:    "2026-04-26T14:00:00Z",
		},
	}
	if err := WriteSavedVariables(path, rows); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.Contains(got, `SynthiqBotsUIResults = {`) {
		t.Errorf("missing global declaration:\n%s", got)
	}
	if !strings.Contains(got, `["abc-1"]`) {
		t.Errorf("missing entry key:\n%s", got)
	}
	// The summary contains a quote that must be backslash-escaped.
	if !strings.Contains(got, `\"quote\"`) {
		t.Errorf("quote not escaped:\n%s", got)
	}
	// The actions JSON must be wrapped in long brackets so the inner
	// double-quotes don't need escaping.
	if !strings.Contains(got, `actions = [[`) {
		t.Errorf("actions long-bracket:\n%s", got)
	}
	if !strings.Contains(got, `"action":"config_tweak"`) {
		t.Errorf("actions content:\n%s", got)
	}
}

func TestWriteSavedVariablesAtomicRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "SynthiqBotsUIResults.lua")
	if err := WriteSavedVariables(path, map[string]ServerRow{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); err == nil {
		t.Errorf(".tmp leaked")
	}
}

func TestLuaLongBracketEscapesNestedCloser(t *testing.T) {
	// JSON containing the literal `]]` should still be safely wrappable.
	got := luaLongBracket("foo ]] bar")
	if !strings.Contains(got, "[=[") || !strings.Contains(got, "]=]") {
		t.Errorf("expected =-bracketed: %q", got)
	}
}

func TestLuaQuoteControlChars(t *testing.T) {
	got := luaQuote("\x01\x02")
	if got != `"\001\002"` {
		t.Errorf("quote: %q", got)
	}
}
