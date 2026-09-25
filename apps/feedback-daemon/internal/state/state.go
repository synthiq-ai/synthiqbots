// Package state persists which feedback entries the daemon has already
// uploaded so duplicate POSTs don't pile up server-side. The state file is
// a single JSON object on disk, written atomically.
//
// We don't trust the SavedVariables file to be cleared by the addon —
// users who never `/reload` have entries lingering for hours. Without
// dedup the daemon would re-upload them every poll tick.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type State struct {
	// Uploaded maps addon-supplied id → upload time. Acts as a set + audit log.
	Uploaded map[string]time.Time `json:"uploaded"`

	// LastResultsID is the cursor for the GET /v1/feedback/results poll.
	// We hand it back to the server as `since=<id>` so each tick is
	// incremental.
	LastResultsID int64 `json:"lastResultsId"`
}

// Store wraps an on-disk State with a mutex and atomic-write semantics.
type Store struct {
	path string
	mu   sync.Mutex
	st   State
}

// Open loads state from path or returns an empty store if it doesn't exist.
// MkdirAll creates the parent directory if missing.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir state dir: %w", err)
	}
	s := &Store{path: path, st: State{Uploaded: map[string]time.Time{}}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &s.st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.st.Uploaded == nil {
		s.st.Uploaded = map[string]time.Time{}
	}
	return s, nil
}

// HasUploaded reports whether the entry with this addon-side id has
// already been pushed to the server.
func (s *Store) HasUploaded(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.st.Uploaded[id]
	return ok
}

// MarkUploaded records the id and persists the state file. Returns an
// error only if the file write fails — the in-memory map is updated
// regardless so a failed flush doesn't cause double-uploads on the same
// process run (the next start might re-upload, but that's the lesser evil).
func (s *Store) MarkUploaded(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Uploaded[id] = time.Now().UTC()
	return s.flushLocked()
}

// flushLocked writes the state atomically: tmp file + rename. Caller holds s.mu.
func (s *Store) flushLocked() error {
	b, err := json.MarshalIndent(s.st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// Stats returns the count of recorded uploads — handy for status logs.
func (s *Store) Stats() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.st.Uploaded)
}

// LastResultsID returns the persisted GET /v1/feedback/results cursor.
func (s *Store) LastResultsID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.LastResultsID
}

// SetLastResultsID advances + persists the results-poll cursor.
func (s *Store) SetLastResultsID(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.LastResultsID = id
	return s.flushLocked()
}
