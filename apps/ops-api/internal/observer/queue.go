// Package observer coordinates on-demand real-client captures — still
// screenshots (Phase 2) and video clips (Phase 3). The flow:
//
//  1. The request_observer_screenshot / request_observer_clip MCP tool calls
//     Add() to register a pending capture, then whispers a structured command to
//     the in-world observer character (via the gameplay MCP whisper_player tool).
//  2. The observer addon catches the whisper, frames the shot, and calls the WoW
//     client's Screenshot() — a file lands in Screenshots/ immediately.
//  3. The feedback-daemon polls Pending(). For a "shot" it ships that screenshot
//     straight back; for a "clip" the screenshot is only a ready-marker — the
//     daemon rolls ffmpeg for Seconds and ships the resulting MP4. Either way it
//     POSTs back tagged with the reqId, which calls Resolve().
//  4. A "shot" tool's Wait() unblocks with the hosted URL. A "clip" tool returns
//     immediately (recording outlives the MCP transport timeout), so its result
//     is parked in the retained-results cache for get_observer_clip to Lookup().
//
// Why a queue at all: WoW SavedVariables only flush on /reload, so the addon
// can't hand the reqId↔file mapping to the daemon in real time. The screenshot
// FILE, however, is written instantly — so correlation happens here, server-side,
// keyed by a reqId the daemon learns from Pending(). State is in-memory and
// ephemeral (captures are on-demand); a TTL reaps abandoned requests.
package observer

import (
	"context"
	"sync"
	"time"
)

// Capture kinds. The daemon branches on these: KindShot ships the marker
// screenshot itself, KindClip treats it as a "recorder is in position" signal.
const (
	KindShot = "shot"
	KindClip = "clip"
)

// resultTTL is how long a resolved capture's Result stays retrievable by
// Lookup(). Long enough that an agent polling get_observer_clip after a long
// recording still finds the URL; short enough that the map can't grow unbounded.
const resultTTL = 15 * time.Minute

// Result is what a resolved capture yields back to the waiting MCP tool.
type Result struct {
	URL    string   `json:"url"`
	Rel    string   `json:"rel"`
	Pushed []string `json:"pushed"`
	Err    string   `json:"err,omitempty"`
}

// Capture is one in-flight capture request.
type Capture struct {
	ReqID       string
	Target      string
	Kind        string // KindShot | KindClip
	Seconds     int    // clip length; 0 for a still
	RequestedAt time.Time
	ttl         time.Duration // 0 => queue default
	done        chan Result
}

// PendingView is the daemon-facing projection of a Capture (no channel).
type PendingView struct {
	ReqID       string `json:"reqId"`
	Target      string `json:"target"`
	Kind        string `json:"kind"`
	Seconds     int    `json:"seconds,omitempty"`
	RequestedAt int64  `json:"requestedAt"` // unix seconds
}

// retained is a resolved Result parked for later Lookup().
type retained struct {
	res Result
	at  time.Time
}

// Queue is a mutex-guarded map of pending captures with a TTL reaper, plus a
// short-lived cache of resolved results for the async (clip) path.
type Queue struct {
	mu      sync.Mutex
	pending map[string]*Capture
	results map[string]retained
	ttl     time.Duration
	now     func() time.Time // injectable clock for tests
}

// NewQueue builds a queue whose entries expire after ttl (a request the daemon
// never fulfils — observer offline — is reaped so Wait returns instead of
// hanging). ttl<=0 falls back to 90s. Individual captures may override it.
func NewQueue(ttl time.Duration) *Queue {
	if ttl <= 0 {
		ttl = 90 * time.Second
	}
	return &Queue{
		pending: map[string]*Capture{},
		results: map[string]retained{},
		ttl:     ttl,
		now:     time.Now,
	}
}

// Add registers a pending capture and returns it. Caller then triggers the
// whisper and — for a still — calls Wait on the returned Capture. A clip caller
// returns immediately and lets Resolve park the result for Lookup.
//
// ttl<=0 uses the queue default; clips pass a longer one (recording + encode +
// upload easily outruns the 90s a screenshot needs).
func (q *Queue) Add(reqID, target, kind string, seconds int, ttl time.Duration) *Capture {
	q.cloud()
	c := &Capture{
		ReqID:       reqID,
		Target:      target,
		Kind:        kind,
		Seconds:     seconds,
		RequestedAt: q.now(),
		ttl:         ttl,
		done:        make(chan Result, 1),
	}
	q.mu.Lock()
	q.pending[reqID] = c
	q.mu.Unlock()
	return c
}

// Pending lists the outstanding captures for the daemon to fulfil.
func (q *Queue) Pending() []PendingView {
	q.cloud()
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]PendingView, 0, len(q.pending))
	for _, c := range q.pending {
		out = append(out, PendingView{
			ReqID:       c.ReqID,
			Target:      c.Target,
			Kind:        c.Kind,
			Seconds:     c.Seconds,
			RequestedAt: c.RequestedAt.Unix(),
		})
	}
	return out
}

// KindOf reports the kind of a pending capture. Handlers use it to reject an
// upload aimed at the wrong endpoint — e.g. a stale daemon that predates the
// clip path POSTing a clip's marker screenshot to /v1/observer/screenshot.
func (q *Queue) KindOf(reqID string) (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	c, ok := q.pending[reqID]
	if !ok {
		return "", false
	}
	return c.Kind, true
}

// Resolve delivers a result to a waiting capture, retains it for later Lookup,
// and removes it from pending. Returns false if the reqId is unknown (already
// resolved / expired / never existed) — the caller still hosted the artifact,
// so that is a late delivery, not a failure.
func (q *Queue) Resolve(reqID string, r Result) bool {
	q.mu.Lock()
	c, ok := q.pending[reqID]
	if ok {
		delete(q.pending, reqID)
	}
	q.results[reqID] = retained{res: r, at: q.now()}
	q.mu.Unlock()
	if !ok {
		return false
	}
	c.done <- r // buffered(1) — never blocks
	return true
}

// Retain parks a Result for later Lookup without touching the pending map.
func (q *Queue) Retain(reqID string, r Result) {
	q.mu.Lock()
	q.results[reqID] = retained{res: r, at: q.now()}
	q.mu.Unlock()
}

// Lookup returns a previously resolved Result. This is how get_observer_clip
// answers after the async request_observer_clip has long since returned.
func (q *Queue) Lookup(reqID string) (Result, bool) {
	q.cloud()
	q.mu.Lock()
	defer q.mu.Unlock()
	r, ok := q.results[reqID]
	if !ok {
		return Result{}, false
	}
	return r.res, true
}

// Wait blocks until the capture is resolved, the context is cancelled, or the
// capture's TTL elapses. On timeout it removes the capture so it stops appearing
// in Pending(). Only the synchronous (screenshot) path calls this.
func (q *Queue) Wait(ctx context.Context, c *Capture) (Result, error) {
	timer := time.NewTimer(q.ttlOf(c))
	defer timer.Stop()
	select {
	case r := <-c.done:
		return r, nil
	case <-timer.C:
		q.remove(c.ReqID)
		return Result{}, context.DeadlineExceeded
	case <-ctx.Done():
		q.remove(c.ReqID)
		return Result{}, ctx.Err()
	}
}

// ttlOf is the capture's own TTL, falling back to the queue default.
func (q *Queue) ttlOf(c *Capture) time.Duration {
	if c.ttl > 0 {
		return c.ttl
	}
	return q.ttl
}

func (q *Queue) remove(reqID string) {
	q.mu.Lock()
	delete(q.pending, reqID)
	q.mu.Unlock()
}

// cloud drops captures past their TTL (observer never fulfilled them) and expires
// retained results.
func (q *Queue) cloud() {
	now := q.now()
	q.mu.Lock()
	defer q.mu.Unlock()
	for id, c := range q.pending {
		if c.RequestedAt.Before(now.Add(-q.ttlOf(c))) {
			delete(q.pending, id)
		}
	}
	for id, r := range q.results {
		if r.at.Before(now.Add(-resultTTL)) {
			delete(q.results, id)
		}
	}
}

// Len reports the number of outstanding captures (for tests / metrics).
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}
