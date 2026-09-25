package dispatch

// The actual loop that drains the feedback queue. One worker per process
// is enough — the queue arrival rate is human-paced (a few captures per
// session at most) and the bottleneck is the upstream agent latency
// (multi-second per call). Concurrency would just give the agent more
// chances to step on its own commits.
//
// State machine + DB queries: see db.go.
// Prompt assembly: see prompt.go.
// Synthiq HTTP: see synthiq.go.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Options bundle the deps the Worker needs.
type Options struct {
	Store          *Store
	Synthiq        *Client
	StorageDir     string        // base for image_path joins; FeedbackStorageDir from config
	PollInterval   time.Duration // default 30s
	StaleAfter     time.Duration // dispatched-row reset threshold; default 10m
	StopOnEmpty    bool          // if true, return after first empty queue scan (used by --once)
	MaxImageBytes  int64         // cap; rows with bigger images are marked error
	IdleSleepShort time.Duration // sleep after a successful drain, default 5s
}

// Worker is the dispatch loop. Construct via NewWorker, run via Run.
type Worker struct{ Options }

func NewWorker(o Options) *Worker {
	if o.PollInterval <= 0 {
		o.PollInterval = 30 * time.Second
	}
	if o.StaleAfter <= 0 {
		o.StaleAfter = 10 * time.Minute
	}
	if o.IdleSleepShort <= 0 {
		o.IdleSleepShort = 5 * time.Second
	}
	if o.MaxImageBytes <= 0 {
		o.MaxImageBytes = 16 * 1024 * 1024 // 16 MiB ceiling
	}
	return &Worker{Options: o}
}

// Run blocks until ctx is cancelled. Logs each tick.
func (w *Worker) Run(ctx context.Context) {
	if w.Store == nil || w.Synthiq == nil {
		slog.Error("dispatch: worker missing deps; refusing to run")
		return
	}

	if rows, err := w.Store.ResetStale(ctx, w.StaleAfter); err != nil {
		slog.Warn("dispatch: reset stale failed", "err", err)
	} else if rows > 0 {
		slog.Info("dispatch: rewound stuck-in-dispatched rows", "n", rows)
	}

	tick := time.NewTimer(0) // fire immediately
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			slog.Info("dispatch: stopping")
			return
		case <-tick.C:
			drained := w.drain(ctx)
			if w.StopOnEmpty && drained == 0 {
				return
			}
			next := w.PollInterval
			if drained > 0 {
				next = w.IdleSleepShort // there might be more — check again soon
			}
			tick.Reset(next)
		}
	}
}

// drain handles up to one row per tick. Returns 1 on success, 0 when the
// queue is empty or the tick errored. Errors are logged, not returned —
// the worker is supposed to keep going.
func (w *Worker) drain(ctx context.Context) int {
	row, err := w.Store.ClaimNext(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	if err != nil {
		slog.Warn("dispatch: claim failed", "err", err)
		return 0
	}

	slog.Info("dispatch: claimed",
		"id", row.ID, "addonId", row.AddonID, "char", row.CharName, "zone", row.Zone)

	imgBytes, err := w.readImage(row)
	if err != nil {
		_ = w.Store.MarkError(ctx, row.ID, fmt.Sprintf("read image: %v", err))
		slog.Warn("dispatch: image read failed", "id", row.ID, "err", err)
		return 1
	}

	resp, err := w.Synthiq.Call(ctx, BuildUserContent(row, imgBytes))
	if err != nil {
		// Transport / upstream error — record but don't lose the row.
		// We rewind to 'pending' so a future tick can retry. The
		// dispatched_at < now-stale check in ResetStale wouldn't hit
		// for fresh dispatched rows, so do it inline here.
		_ = w.Store.MarkError(ctx, row.ID, fmt.Sprintf("synthiq call: %v", err))
		slog.Warn("dispatch: agent call failed", "id", row.ID, "err", err)
		return 1
	}

	actionsJSON := encodeActions(resp)
	display := resp.Summary
	if display == "" {
		display = resp.RawContent
	}
	if err := w.Store.MarkResolved(ctx, row.ID, display, actionsJSON, resp.PRURL); err != nil {
		slog.Warn("dispatch: mark resolved failed", "id", row.ID, "err", err)
		return 1
	}

	slog.Info("dispatch: resolved",
		"id", row.ID, "action", resp.Action, "prUrl", resp.PRURL,
		"summary", truncate(resp.Summary, 80))
	return 1
}

func (w *Worker) readImage(p *PendingRow) ([]byte, error) {
	if p.ImagePath == "" {
		return nil, nil // meta-only entry, that's fine
	}
	full := filepath.Join(w.StorageDir, p.ImagePath)
	info, err := os.Stat(full)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", full, err)
	}
	if info.Size() > w.MaxImageBytes {
		return nil, fmt.Errorf("image %d bytes exceeds cap %d", info.Size(), w.MaxImageBytes)
	}
	return os.ReadFile(full)
}

func encodeActions(r *AgentResponse) string {
	// Capture only the structured action data, not the entire response —
	// the raw text is already in agent_response.
	payload := map[string]any{
		"action":             r.Action,
		"reasoning":          r.Reasoning,
		"config_changes":     r.ConfigChanges,
		"pr_url":             r.PRURL,
		"follow_up_question": r.FollowUpQuestion,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
