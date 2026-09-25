package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeWoWTree builds a minimal WoW directory layout under base.
func makeWoWTree(t *testing.T, base string, accounts []string) {
	t.Helper()
	for _, acc := range accounts {
		dir := filepath.Join(base, "WTF", "Account", acc, "SavedVariables")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(base, "Screenshots"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeConfig(t *testing.T, dir string, body string) string {
	t.Helper()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadHappyPath(t *testing.T) {
	dir := t.TempDir()
	wow := filepath.Join(dir, "wow")
	makeWoWTree(t, wow, []string{"ACCT1"})

	cfg := writeConfig(t, dir, `
api_url = "https://ops.wow.example.com"
bearer_token = "abc"
wow_root = "`+wow+`"
`)
	c, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.PollIntervalMs != 5000 {
		t.Errorf("default poll: %d", c.PollIntervalMs)
	}
	if c.ScreenshotMatchWindowSec != 15 {
		t.Errorf("default window: %d", c.ScreenshotMatchWindowSec)
	}
	acc, err := c.AccountDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(acc, "ACCT1") {
		t.Errorf("auto-pick: %q", acc)
	}
}

func TestLoadMissingFields(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		body, want string
	}{
		{`api_url = "x"`, "bearer_token"},
		{`bearer_token = "x"`, "api_url"},
		{`api_url = "x"
bearer_token = "x"`, "wow_root"},
	}
	for _, c := range cases {
		cfg := writeConfig(t, dir, c.body)
		_, err := Load(cfg)
		if err == nil {
			t.Errorf("body %q: expected error", c.body)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("body %q: error %q does not mention %q", c.body, err, c.want)
		}
	}
}

func TestAccountDirAmbiguous(t *testing.T) {
	dir := t.TempDir()
	wow := filepath.Join(dir, "wow")
	makeWoWTree(t, wow, []string{"ACCT1", "ACCT2"})

	cfg := writeConfig(t, dir, `
api_url = "x"
bearer_token = "x"
wow_root = "`+wow+`"
`)
	c, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AccountDir(); err == nil {
		t.Error("expected ambiguity error")
	} else if !strings.Contains(err.Error(), "set account_name") {
		t.Errorf("error: %v", err)
	}

	// Disambiguate via account_name.
	c.AccountName = "ACCT2"
	got, err := c.AccountDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "ACCT2") {
		t.Errorf("dir: %q", got)
	}
}

func TestSavedVariablesPath(t *testing.T) {
	dir := t.TempDir()
	wow := filepath.Join(dir, "wow")
	makeWoWTree(t, wow, []string{"ACCT1"})

	cfg := writeConfig(t, dir, `
api_url = "x"
bearer_token = "x"
wow_root = "`+wow+`"
`)
	c, err := Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.SavedVariablesPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(wow, "WTF", "Account", "ACCT1", "SavedVariables", "SynthiqBotsUI.lua")
	if got != want {
		t.Errorf("path: %q want %q", got, want)
	}
}
