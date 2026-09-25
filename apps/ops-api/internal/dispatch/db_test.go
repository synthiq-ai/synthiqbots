package dispatch

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newStore(t *testing.T) (*Store, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Store{DB: db}, mock
}

func TestClaimNextEmptyQueue(t *testing.T) {
	s, mock := newStore(t)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM mod_ollama_chat_feedback")).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	_, err := s.ClaimNext(context.Background())
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err: %v (want sql.ErrNoRows)", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}

func TestClaimNextHappyPath(t *testing.T) {
	s, mock := newStore(t)

	rows := sqlmock.NewRows([]string{
		"id", "addon_id", "addon_ts", "char_name", "char_realm", "char_class",
		"char_lvl", "zone", "note", "chat_history", "ctx_json", "image_path", "image_mime",
	}).AddRow(
		int64(42), "abc-1", int64(1700000000), "the operator", "SynthiqEU", "PRIEST",
		47, "Stranglethorn Vale", "boss pulled", `[{"text":"go"}]`,
		`{"zone":"STV"}`, "2026/04/26/x.tga", "image/x-tga",
	)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM mod_ollama_chat_feedback")).WillReturnRows(rows)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE mod_ollama_chat_feedback SET status='dispatched'")).
		WithArgs(int64(42)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	row, err := s.ClaimNext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if row.ID != 42 || row.AddonID != "abc-1" || row.CharClass != "PRIEST" {
		t.Errorf("row: %+v", row)
	}
	if row.Note != "boss pulled" {
		t.Errorf("note: %q", row.Note)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}

func TestMarkResolved(t *testing.T) {
	s, mock := newStore(t)
	mock.ExpectExec(regexp.QuoteMeta("status='resolved'")).
		WithArgs("summary", `{"action":"x"}`, "https://github.com/.../pull/1", int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.MarkResolved(context.Background(), 7, "summary", `{"action":"x"}`, "https://github.com/.../pull/1"); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}

func TestMarkError(t *testing.T) {
	s, mock := newStore(t)
	mock.ExpectExec(regexp.QuoteMeta("status='error'")).
		WithArgs("synthiq 503", int64(8)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := s.MarkError(context.Background(), 8, "synthiq 503"); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}

func TestResetStale(t *testing.T) {
	s, mock := newStore(t)
	mock.ExpectExec(regexp.QuoteMeta("WHERE status='dispatched'")).
		WithArgs(int(600 * time.Second / time.Second)).
		WillReturnResult(sqlmock.NewResult(0, 3))

	n, err := s.ResetStale(context.Background(), 600*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("rows: %d", n)
	}
}
