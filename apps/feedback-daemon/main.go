// feedback-daemon — Windows companion process for the SynthiqBotsUI addon.
// Tails the addon's SavedVariables file, matches Screenshots/ files by
// timestamp, and POSTs both to the ops-api /v1/feedback endpoint as
// multipart/form-data. Dedup state lives in %APPDATA%/synthiq-feedback/.
//
// PR 3 of the feedback-loop project. Design: docs/feedback-loop.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/feedback-daemon/internal/config"
	"github.com/synthiq-ai/synthiqbots/apps/feedback-daemon/internal/observer"
	"github.com/synthiq-ai/synthiqbots/apps/feedback-daemon/internal/recorder"
	"github.com/synthiq-ai/synthiqbots/apps/feedback-daemon/internal/results"
	"github.com/synthiq-ai/synthiqbots/apps/feedback-daemon/internal/saves"
	"github.com/synthiq-ai/synthiqbots/apps/feedback-daemon/internal/screens"
	"github.com/synthiq-ai/synthiqbots/apps/feedback-daemon/internal/state"
	"github.com/synthiq-ai/synthiqbots/apps/feedback-daemon/internal/upload"
)

// resultsCacheCap caps the number of agent responses the daemon keeps
// in the SavedVariables sidecar. The addon's `/sb feedback log` only
// shows the last 10, but we keep a larger window for history.
const resultsCacheCap = 100

func main() {
	configPath := flag.String("config", defaultConfigPath(), "path to TOML config")
	once := flag.Bool("once", false, "scan, upload pending entries, then exit")
	verbose := flag.Bool("v", false, "verbose logging")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("config load failed", "path", *configPath, "err", err)
		os.Exit(1)
	}
	slog.Info("loaded config",
		"apiUrl", cfg.APIURL,
		"wowRoot", cfg.WoWRoot,
		"poll", cfg.PollInterval(),
		"dryRun", cfg.DryRun)

	store, err := state.Open(cfg.EffectiveStatePath())
	if err != nil {
		slog.Error("state open failed", "err", err)
		os.Exit(1)
	}
	slog.Info("state loaded", "path", cfg.EffectiveStatePath(), "alreadyUploaded", store.Stats())

	client := upload.New(cfg.APIURL, cfg.BearerToken)
	resultsClient := results.NewClient(cfg.APIURL, cfg.BearerToken)
	resultsCache := map[string]results.ServerRow{}
	// Observer capture poller (Phases 2 + 3). Polls ops-api for pending
	// real-client capture requests and ships matching screenshots, or records and
	// ships video clips. No-op unless the server has the observer feature
	// configured. A missing ffmpeg only disables clips — screenshots still work,
	// so warn rather than exit.
	var rec recorder.Recorder
	if bin := recorder.Resolve(cfg.FFmpegPath); bin != "" {
		rec = recorder.NewFFmpeg(bin, cfg.EffectiveWindowTitle())
		slog.Info("observer: clip recording enabled", "ffmpeg", bin, "window", cfg.EffectiveWindowTitle())
	} else {
		slog.Warn("observer: ffmpeg not found; clip requests will fail (screenshots unaffected)",
			"hint", "set ffmpeg_path in config.toml, or drop ffmpeg.exe next to the daemon")
	}
	observerPoller := observer.New(cfg.APIURL, cfg.BearerToken, cfg.ScreenshotsDir(), rec)

	tick := func() {
		if err := scanAndUpload(cfg, store, client); err != nil {
			slog.Warn("scan failed", "err", err)
		}
		if err := pollResults(cfg, store, resultsClient, resultsCache); err != nil {
			slog.Warn("results poll failed", "err", err)
		}
		if err := observerPoller.Tick(context.Background()); err != nil {
			slog.Warn("observer poll failed", "err", err)
		}
	}

	if *once {
		tick()
		return
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	t := time.NewTicker(cfg.PollInterval())
	defer t.Stop()

	tick() // first sweep immediately
	for {
		select {
		case <-stop:
			slog.Info("shutting down")
			return
		case <-t.C:
			tick()
		}
	}
}

// pollResults pulls dispatcher-resolved rows since the last cursor and
// writes the addon-readable SavedVariables sidecar file. Idempotent: a
// row that was already in the cache from a prior tick stays there.
func pollResults(cfg *config.Config, store *state.Store, client *results.Client, cache map[string]results.ServerRow) error {
	since := store.LastResultsID()
	page, err := client.Fetch(context.Background(), since, 50)
	if err != nil {
		return err
	}
	if page.Count == 0 {
		return nil
	}
	for _, row := range page.Rows {
		cache[row.AddonID] = row
	}
	// Cap the cache at resultsCacheCap by dropping the lowest db_id rows
	// (oldest dispatched). Walk once to find the cutoff.
	if len(cache) > resultsCacheCap {
		ids := make([]int64, 0, len(cache))
		for _, r := range cache {
			ids = append(ids, r.ID)
		}
		// Sort ascending then take cutoff = ids[len-cap].
		for i := 1; i < len(ids); i++ {
			for j := i; j > 0 && ids[j-1] > ids[j]; j-- {
				ids[j-1], ids[j] = ids[j], ids[j-1]
			}
		}
		cutoff := ids[len(ids)-resultsCacheCap]
		for k, r := range cache {
			if r.ID < cutoff {
				delete(cache, k)
			}
		}
	}
	resultsPath, err := cfg.ResultsSavedVariablesPath()
	if err != nil {
		return fmt.Errorf("resolve results path: %w", err)
	}
	if err := results.WriteSavedVariables(resultsPath, cache); err != nil {
		return fmt.Errorf("write savedvariables: %w", err)
	}
	if err := store.SetLastResultsID(page.NextSince); err != nil {
		slog.Warn("results cursor flush failed (will re-fetch on next start)", "err", err)
	}
	slog.Info("results synced", "newRows", page.Count, "cacheSize", len(cache), "nextSince", page.NextSince)
	return nil
}

// scanAndUpload performs one poll: parse SavedVariables, match screenshots,
// upload anything not already in the dedup state. Errors per-entry are
// logged and skipped — one bad row shouldn't block the rest.
func scanAndUpload(cfg *config.Config, store *state.Store, client *upload.Client) error {
	savesPath, err := cfg.SavedVariablesPath()
	if err != nil {
		return fmt.Errorf("resolve saved variables: %w", err)
	}
	entries, err := saves.Parse(savesPath)
	if err != nil {
		return fmt.Errorf("parse saves: %w", err)
	}
	if len(entries) == 0 {
		slog.Debug("no entries", "path", savesPath)
		return nil
	}

	pending := 0
	for _, e := range entries {
		if store.HasUploaded(e.ID) {
			continue
		}
		pending++
		shotPath, mime, err := screens.Match(cfg.ScreenshotsDir(), e.AddonTimestamp(), cfg.ScreenshotWindow())
		if err != nil {
			slog.Warn("screenshot match", "id", e.ID, "err", err)
		}
		if cfg.DryRun {
			slog.Info("[dry-run] would upload",
				"id", e.ID, "addonTs", e.AddonTs, "screenshot", shotPath, "note", truncate(e.Note, 60))
			continue
		}
		res, err := client.Send(e, shotPath, mime)
		if err != nil {
			slog.Error("upload failed", "id", e.ID, "err", err)
			continue
		}
		if err := store.MarkUploaded(e.ID); err != nil {
			slog.Warn("state flush failed (will re-attempt on next start)", "id", e.ID, "err", err)
		}
		slog.Info("uploaded",
			"id", e.ID, "dbId", res.DBId, "imagePath", res.ImagePath, "imageBytes", res.ImageBytes)
	}
	if pending == 0 {
		slog.Debug("nothing to upload", "queued", len(entries), "alreadyUploaded", store.Stats())
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func defaultConfigPath() string {
	if v, err := os.UserConfigDir(); err == nil {
		return v + string(os.PathSeparator) + "synthiq-feedback" + string(os.PathSeparator) + "config.toml"
	}
	return "config.toml"
}
