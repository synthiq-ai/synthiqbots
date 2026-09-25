package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/config"
)

func makeResultsServer(t *testing.T) (*Server, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := &config.Config{RequestTimeout: 5 * time.Second}
	return NewServer(&Deps{Cfg: cfg, DB: db}), mock
}

func TestFeedbackResultsHappyPath(t *testing.T) {
	s, mock := makeResultsServer(t)

	rows := sqlmock.NewRows([]string{
		"id", "addon_id", "addon_ts", "status", "agent_response", "agent_actions", "agent_pr_url", "resolved_at",
	}).AddRow(int64(11), "abc-11", int64(1700000011), "resolved", "summary 1", `{"action":"x"}`, "", "2026-04-26T14:00:00Z").
		AddRow(int64(13), "abc-13", int64(1700000013), "error", "synthiq 503", "", "", "2026-04-26T14:01:00Z")
	mock.ExpectQuery(regexp.QuoteMeta("FROM mod_ollama_chat_feedback")).
		WithArgs(int64(10), 100).
		WillReturnRows(rows)

	req := httptest.NewRequest(http.MethodGet, "/v1/feedback/results?since=10&limit=100", nil)
	rr := httptest.NewRecorder()
	s.FeedbackResults(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d; body=%s", rr.Code, rr.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	rowsAny, _ := resp["rows"].([]any)
	if len(rowsAny) != 2 {
		t.Errorf("rows: %d", len(rowsAny))
	}
	if resp["nextSince"].(float64) != 13 {
		t.Errorf("nextSince: %v", resp["nextSince"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}

func TestFeedbackResults503WhenNoDB(t *testing.T) {
	cfg := &config.Config{RequestTimeout: 5 * time.Second}
	s := NewServer(&Deps{Cfg: cfg}) // no DB
	req := httptest.NewRequest(http.MethodGet, "/v1/feedback/results", nil)
	rr := httptest.NewRecorder()
	s.FeedbackResults(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status: %d", rr.Code)
	}
}

func TestFeedbackResultsLimitClamped(t *testing.T) {
	s, mock := makeResultsServer(t)
	// limit=9999 should clamp to 500
	mock.ExpectQuery(regexp.QuoteMeta("FROM mod_ollama_chat_feedback")).
		WithArgs(int64(0), 500).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "addon_id", "addon_ts", "status", "agent_response", "agent_actions", "agent_pr_url", "resolved_at",
		}))
	req := httptest.NewRequest(http.MethodGet, "/v1/feedback/results?limit=9999", nil)
	rr := httptest.NewRecorder()
	s.FeedbackResults(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status: %d", rr.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock: %v", err)
	}
}
