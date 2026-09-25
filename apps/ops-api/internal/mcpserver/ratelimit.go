package mcpserver

import (
	"sync"
	"time"
)

// rateLimiter tracks a sliding 60-second window of timestamps per (tool, ip)
// pair. Mirrors the C++ TryClaimActionRateSlot ring-buffer in
// src/mod-ollama-chat_tools.h:57.
type rateLimiter struct {
	mu      sync.Mutex
	windows map[string][]time.Time
	limits  map[string]int // tool name → calls/min/IP
	def     int            // default limit when tool not in `limits`
}

func newRateLimiter(limits map[string]int, def int) *rateLimiter {
	if limits == nil {
		limits = map[string]int{}
	}
	if def <= 0 {
		def = 30
	}
	return &rateLimiter{
		windows: map[string][]time.Time{},
		limits:  limits,
		def:     def,
	}
}

// allow returns true if the (tool, ip) pair is under its per-minute cap and
// records the call. Returns false when the cap has been hit.
func (r *rateLimiter) allow(tool, ip string) bool {
	cap := r.def
	if v, ok := r.limits[tool]; ok && v > 0 {
		cap = v
	}
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	key := tool + "|" + ip
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.windows[key]
	// Trim entries older than 60s.
	keep := w[:0]
	for _, ts := range w {
		if ts.After(cutoff) {
			keep = append(keep, ts)
		}
	}
	if len(keep) >= cap {
		r.windows[key] = keep
		return false
	}
	r.windows[key] = append(keep, now)
	return true
}
