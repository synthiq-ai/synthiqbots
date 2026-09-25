// Package dispatch contains the worker that drains pending feedback rows
// and routes each one through a vision-capable Synthiq agent. Sits inside
// wow-ops-api so it shares the Postgres connection and the screenshot
// volume mount with the ingest endpoint — no second process needed.
//
// State machine in mod_ollama_chat_feedback:
//
//   pending  ──worker picks up──►  dispatched (dispatched_at = NOW)
//                                   │
//                          ┌────────┴────────┐
//                       success             error
//                          │                 │
//                          ▼                 ▼
//                       resolved          (back to pending if
//                       agent_response    attempts < max,
//                       agent_actions     else error)
//                       agent_pr_url
//                       resolved_at
package dispatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PendingRow carries the fields the worker needs to build a prompt + read
// the screenshot off disk.
type PendingRow struct {
	ID          int64
	AddonID     string
	AddonTs     int64
	CharName    string
	CharRealm   string
	CharClass   string
	CharLvl     int
	Zone        string
	Note        string
	ChatHistory string // JSON
	CtxJSON     string // JSON
	ImagePath   string // relative to FeedbackStorageDir (may be empty)
	ImageMime   string
}

// Store wraps a *sql.DB with the queries the worker uses. Kept narrow so
// the worker can be unit-tested with sqlmock.
type Store struct{ DB *sql.DB }

// ClaimNext atomically picks one pending row and flips it to 'dispatched'.
// The flip is the lock — concurrent workers won't double-claim the same
// row because UPDATE ... LIMIT 1 takes a row lock. Returns sql.ErrNoRows
// when the queue is empty.
func (s *Store) ClaimNext(ctx context.Context) (*PendingRow, error) {
	if s.DB == nil {
		return nil, errors.New("dispatch: db not configured")
	}

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx,
		"SELECT id, addon_id, addon_ts, char_name, char_realm, char_class, "+
			"char_lvl, zone, COALESCE(note,''), COALESCE(chat_history,''), "+
			"COALESCE(ctx_json,''), image_path, image_mime "+
			"FROM mod_ollama_chat_feedback "+
			"WHERE status='pending' "+
			"ORDER BY id ASC LIMIT 1 FOR UPDATE")

	var p PendingRow
	if err := row.Scan(&p.ID, &p.AddonID, &p.AddonTs, &p.CharName, &p.CharRealm,
		&p.CharClass, &p.CharLvl, &p.Zone, &p.Note, &p.ChatHistory, &p.CtxJSON,
		&p.ImagePath, &p.ImageMime); err != nil {
		return nil, err // includes sql.ErrNoRows when queue is empty
	}

	if _, err := tx.ExecContext(ctx,
		"UPDATE mod_ollama_chat_feedback SET status='dispatched', dispatched_at=NOW() WHERE id=?",
		p.ID); err != nil {
		return nil, fmt.Errorf("flip dispatched: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &p, nil
}

// MarkResolved records a successful agent run. agentActions is a JSON
// string (caller-built) describing what the agent did — config tweaks,
// PR URL, etc. — so we can audit later without re-querying Synthiq.
func (s *Store) MarkResolved(ctx context.Context, id int64, response, actions, prURL string) error {
	_, err := s.DB.ExecContext(ctx,
		"UPDATE mod_ollama_chat_feedback SET status='resolved', resolved_at=NOW(), "+
			"agent_response=?, agent_actions=?, agent_pr_url=NULLIF(?,'') WHERE id=?",
		response, actions, prURL, id)
	if err != nil {
		return fmt.Errorf("mark resolved: %w", err)
	}
	return nil
}

// MarkError flips the row to terminal 'error' state. Used when the agent
// call fails or the worker can't make progress (image missing, etc.).
// The error string is stored in agent_response so the operator can see
// what went wrong without re-running.
func (s *Store) MarkError(ctx context.Context, id int64, errMsg string) error {
	_, err := s.DB.ExecContext(ctx,
		"UPDATE mod_ollama_chat_feedback SET status='error', resolved_at=NOW(), "+
			"agent_response=? WHERE id=?",
		errMsg, id)
	if err != nil {
		return fmt.Errorf("mark error: %w", err)
	}
	return nil
}

// Reset rewinds a stuck-in-dispatched row back to pending. The worker
// runs this at startup so a previous-process crash doesn't strand rows
// forever in the dispatched state. We bound the rewind to rows older
// than `staleAfter` so an actively-in-flight call from a peer doesn't
// get yanked.
func (s *Store) ResetStale(ctx context.Context, staleAfter time.Duration) (int64, error) {
	res, err := s.DB.ExecContext(ctx,
		"UPDATE mod_ollama_chat_feedback SET status='pending', dispatched_at=NULL "+
			"WHERE status='dispatched' AND dispatched_at < (NOW() - INTERVAL ? SECOND)",
		int(staleAfter.Seconds()))
	if err != nil {
		return 0, fmt.Errorf("reset stale: %w", err)
	}
	return res.RowsAffected()
}
