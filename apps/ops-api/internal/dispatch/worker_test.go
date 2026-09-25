package dispatch

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// End-to-end test: claim a row, fake Synthiq returns a config_tweak
// decision, worker writes it back to the DB. Stops after one drain.
func TestWorkerEndToEnd(t *testing.T) {
	// 1. DB setup
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"id", "addon_id", "addon_ts", "char_name", "char_realm", "char_class",
		"char_lvl", "zone", "note", "chat_history", "ctx_json", "image_path", "image_mime",
	}).AddRow(int64(1), "a", int64(0), "", "", "", 0, "", "", "", "", "", "")

	// First drain: claim + flip dispatched + commit, then mark resolved.
	mock.ExpectExec(regexp.QuoteMeta("WHERE status='dispatched'")).
		WillReturnResult(sqlmock.NewResult(0, 0)) // ResetStale
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM mod_ollama_chat_feedback")).WillReturnRows(rows)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE mod_ollama_chat_feedback SET status='dispatched'")).
		WithArgs(int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectExec(regexp.QuoteMeta("status='resolved'")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Second drain: empty queue, returns ErrNoRows.
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM mod_ollama_chat_feedback")).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	// 2. Fake Synthiq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"action\":\"no_action\",\"summary\":\"all good\"}"}}]}`))
	}))
	defer srv.Close()

	// 3. Worker — once-mode so it stops after the queue is drained.
	w := NewWorker(Options{
		Store:          &Store{DB: db},
		Synthiq:        New(srv.URL, "x", "m"),
		StorageDir:     t.TempDir(),
		PollInterval:   10 * time.Millisecond,
		IdleSleepShort: 10 * time.Millisecond,
		StopOnEmpty:    true,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	w.Run(ctx)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}

// Image read + base64 — verifies the worker actually touches disk and
// the readImage cap is enforced.
func TestWorkerImageCapEnforced(t *testing.T) {
	dir := t.TempDir()
	imgRel := "huge.tga"
	if err := os.WriteFile(filepath.Join(dir, imgRel), make([]byte, 512), 0o644); err != nil {
		t.Fatal(err)
	}
	w := NewWorker(Options{StorageDir: dir, MaxImageBytes: 100})
	_, err := w.readImage(&PendingRow{ImagePath: imgRel})
	if err == nil {
		t.Fatal("expected cap error")
	}
}

func TestWorkerImageMissingPathOK(t *testing.T) {
	w := NewWorker(Options{StorageDir: t.TempDir()})
	bytes, err := w.readImage(&PendingRow{ImagePath: ""})
	if err != nil {
		t.Errorf("empty image_path should be fine: %v", err)
	}
	if len(bytes) != 0 {
		t.Errorf("expected nil bytes")
	}
}
