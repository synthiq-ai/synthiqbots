// Package config loads the daemon's TOML config file from the user's
// AppData (Windows) or XDG_CONFIG_HOME (Linux/Mac for cross-platform tests).
//
// The daemon ships as a single .exe alongside this config; users edit the
// file once with their WoW install path, ops-api URL, and bearer token.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	// APIURL is the ops-api base, e.g. https://ops.wow.example.com.
	// The daemon POSTs to APIURL + "/v1/feedback".
	APIURL string `toml:"api_url"`

	// BearerToken matches OPS_BEARER_TOKEN on the server.
	BearerToken string `toml:"bearer_token"`

	// WoWRoot is the path to the WoW client install (the directory that
	// contains WoW.exe, WTF/, Screenshots/, Interface/). Forward or back
	// slashes both work.
	WoWRoot string `toml:"wow_root"`

	// AccountName scopes which SavedVariables file we tail. WoW stores
	// per-character UI state at:
	//   <WoWRoot>/WTF/Account/<account>/SavedVariables/SynthiqBotsUI.lua
	// If empty and exactly one account directory exists, the daemon
	// auto-picks it.
	AccountName string `toml:"account_name"`

	// PollIntervalMs is how often the daemon checks SavedVariables +
	// Screenshots/ for new captures (default 5000).
	PollIntervalMs int `toml:"poll_interval_ms"`

	// ScreenshotMatchWindowSec is the ± mtime window the daemon allows
	// when associating a Screenshots/ file with a feedback entry by
	// timestamp (default 15).
	ScreenshotMatchWindowSec int `toml:"screenshot_match_window_sec"`

	// StateFilePath overrides the default state location
	// (%APPDATA%/synthiq-feedback/state.json). Used by tests.
	StateFilePath string `toml:"state_file_path"`

	// DryRun when true logs what would upload without making HTTP calls.
	DryRun bool `toml:"dry_run"`

	// FFmpegPath points at the ffmpeg binary used to record observer video
	// clips. Empty resolves via PATH, then ffmpeg.exe beside the daemon. When
	// nothing resolves, screenshots still work and clips fail with a hint.
	FFmpegPath string `toml:"ffmpeg_path"`

	// CaptureWindowTitle is the WoW window title passed to ffmpeg's gdigrab
	// (`-i title=...`). Default "World of Warcraft". The client must run
	// windowed or borderless — exclusive fullscreen cannot be captured.
	CaptureWindowTitle string `toml:"capture_window_title"`
}

// EffectiveWindowTitle returns the configured gdigrab window title.
func (c *Config) EffectiveWindowTitle() string {
	if c.CaptureWindowTitle != "" {
		return c.CaptureWindowTitle
	}
	return "World of Warcraft"
}

// Load reads the TOML file at path. Missing path is an error so the user
// knows to seed the config before starting the daemon.
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, errors.New("config path is required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	c := &Config{}
	if err := toml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) applyDefaults() {
	if c.PollIntervalMs <= 0 {
		c.PollIntervalMs = 5000
	}
	if c.ScreenshotMatchWindowSec <= 0 {
		c.ScreenshotMatchWindowSec = 15
	}
	c.WoWRoot = strings.ReplaceAll(c.WoWRoot, "\\", "/")
}

func (c *Config) validate() error {
	if c.APIURL == "" {
		return errors.New("api_url is required")
	}
	if c.BearerToken == "" {
		return errors.New("bearer_token is required")
	}
	if c.WoWRoot == "" {
		return errors.New("wow_root is required")
	}
	if _, err := os.Stat(c.WoWRoot); err != nil {
		return fmt.Errorf("wow_root not accessible: %w", err)
	}
	return nil
}

// PollInterval returns the configured poll interval as a time.Duration.
func (c *Config) PollInterval() time.Duration {
	return time.Duration(c.PollIntervalMs) * time.Millisecond
}

// ScreenshotWindow returns the configured screenshot match window.
func (c *Config) ScreenshotWindow() time.Duration {
	return time.Duration(c.ScreenshotMatchWindowSec) * time.Second
}

// AccountDir returns the resolved per-account WTF directory (or an error if
// AccountName is empty AND there isn't exactly one account on disk).
func (c *Config) AccountDir() (string, error) {
	wtfAccount := filepath.Join(c.WoWRoot, "WTF", "Account")
	if c.AccountName != "" {
		dir := filepath.Join(wtfAccount, c.AccountName)
		if _, err := os.Stat(dir); err != nil {
			return "", fmt.Errorf("account dir %s not found: %w", dir, err)
		}
		return dir, nil
	}
	entries, err := os.ReadDir(wtfAccount)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", wtfAccount, err)
	}
	var accounts []string
	for _, e := range entries {
		// WoW account dirs are uppercase; skip "SavedVariables" or stray files.
		if e.IsDir() && e.Name() != "SavedVariables" && !strings.HasPrefix(e.Name(), ".") {
			accounts = append(accounts, e.Name())
		}
	}
	if len(accounts) == 0 {
		return "", fmt.Errorf("no account dirs in %s; set account_name", wtfAccount)
	}
	if len(accounts) > 1 {
		return "", fmt.Errorf("%d accounts in %s (%s); set account_name to disambiguate",
			len(accounts), wtfAccount, strings.Join(accounts, ", "))
	}
	return filepath.Join(wtfAccount, accounts[0]), nil
}

// SavedVariablesPath returns the path to SynthiqBotsUI.lua. WoW stores it
// under <account>/SavedVariables/. AzerothCore single-realm setups generally
// only have one realm, but the addon's PerCharacter saves are keyed by
// character — for our purposes the global save (one file per account) is
// what carries `feedback_queue`.
func (c *Config) SavedVariablesPath() (string, error) {
	acc, err := c.AccountDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(acc, "SavedVariables", "SynthiqBotsUI.lua"), nil
}

// ResultsSavedVariablesPath returns the path to the daemon-written
// sidecar file the addon reads on ADDON_LOADED to surface agent
// responses. Lives in the same SavedVariables/ dir as the addon's
// own save file but is owned by the daemon (the addon never mutates it).
func (c *Config) ResultsSavedVariablesPath() (string, error) {
	acc, err := c.AccountDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(acc, "SavedVariables", "SynthiqBotsUIResults.lua"), nil
}

// ScreenshotsDir returns the WoW Screenshots/ folder.
func (c *Config) ScreenshotsDir() string {
	return filepath.Join(c.WoWRoot, "Screenshots")
}

// DefaultStatePath returns the platform-default state file location.
// Windows: %APPDATA%/synthiq-feedback/state.json
// Other:   $XDG_CONFIG_HOME/synthiq-feedback/state.json (or ~/.config/...)
func DefaultStatePath() string {
	if v, err := os.UserConfigDir(); err == nil {
		return filepath.Join(v, "synthiq-feedback", "state.json")
	}
	return filepath.Join(".", "synthiq-feedback-state.json")
}

// EffectiveStatePath returns the configured override or the default.
func (c *Config) EffectiveStatePath() string {
	if c.StateFilePath != "" {
		return c.StateFilePath
	}
	return DefaultStatePath()
}
