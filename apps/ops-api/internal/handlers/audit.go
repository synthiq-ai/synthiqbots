package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type GatewayAuditRow struct {
	ID               uint64    `json:"id"`
	BotGuid          uint64    `json:"botGuid"`
	PlayerGuid       uint64    `json:"playerGuid"`
	AccountID        uint32    `json:"accountId"`
	TS               time.Time `json:"ts"`
	RequestChars     uint32    `json:"requestChars"`
	ResponseChars    uint32    `json:"responseChars"`
	PromptTokens     uint32    `json:"promptTokens"`
	CompletionTokens uint32    `json:"completionTokens"`
	LatencyMs        uint32    `json:"latencyMs"`
	SourceChannel    string    `json:"sourceChannel"`
	Error            bool      `json:"error"`
}

type TacticalAuditRow struct {
	ID               uint64    `json:"id"`
	BotGuid          uint64    `json:"botGuid"`
	BotName          string    `json:"botName"`
	TS               time.Time `json:"ts"`
	Action           string    `json:"action"`
	Result           string    `json:"result"`
	Error            string    `json:"error,omitempty"`
	Escalated        bool      `json:"escalated"`
	EscalationReason string    `json:"escalationReason,omitempty"`
	LatencyMs        uint32    `json:"latencyMs"`
	InCombat         bool      `json:"inCombat"`
}

// AdminAuditRow mirrors one mod_ollama_chat_admin_audit entry — the ops-api's
// own per-tools/call audit row. Args are surfaced raw (already truncated to 4
// KiB by WriteAdminAudit on insert).
type AdminAuditRow struct {
	ID         uint64    `json:"id"`
	TS         time.Time `json:"ts"`
	ClientIP   string    `json:"clientIp"`
	Tool       string    `json:"tool"`
	ArgsJSON   string    `json:"argsJson,omitempty"`
	Result     string    `json:"result"` // "ok" or "error"
	Error      string    `json:"error,omitempty"`
	DurationMs uint32    `json:"durationMs"`
}

// AuditFilters carries the optional WHERE filters supported by the audit queries.
// Empty / zero fields are treated as "no filter".
type AuditFilters struct {
	BotGuid    string // string so MCP tools can pass either a number or omit
	AccountID  string
	Action     string
	Result     string
	Tool       string // admin audit: filter by tool name (exact match)
	ClientIP   string // admin audit: filter by client IP (exact match)
	From       string
	To         string
	ErrorsOnly bool
	Escalated  bool
	Limit      int
	Offset     int
}

func (s *Server) AuditGatewayQuery(ctx context.Context, f AuditFilters) ([]GatewayAuditRow, error) {
	if s.DB == nil {
		return nil, fmt.Errorf("database not configured")
	}
	limit := clampInt(f.Limit, 1, 1000)
	if f.Limit == 0 {
		limit = 200
	}
	offset := clampInt(f.Offset, 0, 1<<20)

	var conds []string
	var args []any
	if f.BotGuid != "" {
		if n, err := strconv.ParseUint(f.BotGuid, 10, 64); err == nil {
			conds = append(conds, "bot_guid = ?")
			args = append(args, n)
		}
	}
	if f.AccountID != "" {
		if n, err := strconv.ParseUint(f.AccountID, 10, 32); err == nil {
			conds = append(conds, "account_id = ?")
			args = append(args, n)
		}
	}
	if f.From != "" {
		conds = append(conds, "ts >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		conds = append(conds, "ts <= ?")
		args = append(args, f.To)
	}
	if f.ErrorsOnly {
		conds = append(conds, "error = 1")
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit, offset)

	rows, err := s.DB.QueryContext(ctx,
		"SELECT id, bot_guid, player_guid, account_id, ts, request_chars, response_chars, "+
			"prompt_tokens, completion_tokens, latency_ms, source_channel, error "+
			"FROM mod_ollama_chat_gateway_audit"+where+
			" ORDER BY id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	out := []GatewayAuditRow{}
	for rows.Next() {
		var r GatewayAuditRow
		var errFlag uint8
		if err := rows.Scan(&r.ID, &r.BotGuid, &r.PlayerGuid, &r.AccountID, &r.TS,
			&r.RequestChars, &r.ResponseChars, &r.PromptTokens, &r.CompletionTokens,
			&r.LatencyMs, &r.SourceChannel, &errFlag); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		r.Error = errFlag != 0
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return out, nil
}

func (s *Server) AuditTacticalQuery(ctx context.Context, f AuditFilters) ([]TacticalAuditRow, error) {
	if s.DB == nil {
		return nil, fmt.Errorf("database not configured")
	}
	limit := clampInt(f.Limit, 1, 1000)
	if f.Limit == 0 {
		limit = 200
	}
	offset := clampInt(f.Offset, 0, 1<<20)

	var conds []string
	var args []any
	if f.BotGuid != "" {
		if n, err := strconv.ParseUint(f.BotGuid, 10, 64); err == nil {
			conds = append(conds, "bot_guid = ?")
			args = append(args, n)
		}
	}
	if f.Action != "" {
		conds = append(conds, "action = ?")
		args = append(args, f.Action)
	}
	if f.Result != "" {
		conds = append(conds, "result = ?")
		args = append(args, f.Result)
	}
	if f.Escalated {
		conds = append(conds, "escalated = 1")
	}
	if f.From != "" {
		conds = append(conds, "ts >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		conds = append(conds, "ts <= ?")
		args = append(args, f.To)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit, offset)

	rows, err := s.DB.QueryContext(ctx,
		"SELECT id, bot_guid, bot_name, ts, action, result, COALESCE(error,''), "+
			"escalated, COALESCE(escalation_reason,''), latency_ms, in_combat "+
			"FROM mod_ollama_chat_tactical_audit"+where+
			" ORDER BY id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	out := []TacticalAuditRow{}
	for rows.Next() {
		var r TacticalAuditRow
		var escFlag, combatFlag uint8
		if err := rows.Scan(&r.ID, &r.BotGuid, &r.BotName, &r.TS, &r.Action, &r.Result,
			&r.Error, &escFlag, &r.EscalationReason, &r.LatencyMs, &combatFlag); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		r.Escalated = escFlag != 0
		r.InCombat = combatFlag != 0
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return out, nil
}

// AuditAdminQuery reads mod_ollama_chat_admin_audit — the per-tools/call audit
// the ops-api writes for itself via WriteAdminAudit. Filters: tool (exact),
// clientIp (exact), errorsOnly (result='error'), and the standard from/to
// time window. Mirrors the AuditGatewayQuery / AuditTacticalQuery shape so
// the MCP tool layer can lean on the same AuditFilters struct.
//
// Note: the table lives in `acore_characters` (same as the gateway/tactical
// audits). The ops_ro user already covers it via the
// `acore_characters.mod_ollama_chat_%` wildcard grant — no extra GRANT
// required on top of what `ops_audit_gateway` / `ops_audit_tactical`
// already need.
func (s *Server) AuditAdminQuery(ctx context.Context, f AuditFilters) ([]AdminAuditRow, error) {
	if s.DB == nil {
		return nil, fmt.Errorf("database not configured")
	}
	limit := clampInt(f.Limit, 1, 1000)
	if f.Limit == 0 {
		limit = 200
	}
	offset := clampInt(f.Offset, 0, 1<<20)

	var conds []string
	var args []any
	if f.Tool != "" {
		conds = append(conds, "tool = ?")
		args = append(args, f.Tool)
	}
	if f.ClientIP != "" {
		conds = append(conds, "client_ip = ?")
		args = append(args, f.ClientIP)
	}
	if f.From != "" {
		conds = append(conds, "ts >= ?")
		args = append(args, f.From)
	}
	if f.To != "" {
		conds = append(conds, "ts <= ?")
		args = append(args, f.To)
	}
	if f.ErrorsOnly {
		conds = append(conds, "result = 'error'")
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit, offset)

	rows, err := s.DB.QueryContext(ctx,
		"SELECT id, ts, client_ip, tool, COALESCE(args_json,''), result, "+
			"COALESCE(error,''), duration_ms "+
			"FROM mod_ollama_chat_admin_audit"+where+
			" ORDER BY id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	out := []AdminAuditRow{}
	for rows.Next() {
		var r AdminAuditRow
		if err := rows.Scan(&r.ID, &r.TS, &r.ClientIP, &r.Tool, &r.ArgsJSON,
			&r.Result, &r.Error, &r.DurationMs); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return out, nil
}

type GatewayBucket struct {
	BotGuid       uint64 `json:"botGuid"`
	Calls         uint64 `json:"calls"`
	Errors        uint64 `json:"errors"`
	AvgLatencyMs  uint64 `json:"avgLatencyMs"`
	PromptTokens  uint64 `json:"promptTokens"`
	CompletionTok uint64 `json:"completionTokens"`
}

type TacticalBucket struct {
	BotGuid   uint64 `json:"botGuid"`
	BotName   string `json:"botName"`
	Action    string `json:"action"`
	Result    string `json:"result"`
	Count     uint64 `json:"count"`
	Escalated uint64 `json:"escalated"`
}

type SummaryResult struct {
	Window   string           `json:"window"`
	Since    time.Time        `json:"since"`
	Gateway  []GatewayBucket  `json:"gateway"`
	Tactical []TacticalBucket `json:"tactical"`
}

// AdminAuditBucket is the per-tool aggregate row for AuditAdminSummaryQuery.
// Counts and durations are pre-rolled-up server-side so callers (the daily
// gap-finding cron, an operator looking for slow / erroring tools) don't have
// to pull up to 1000 raw rows and aggregate manually.
type AdminAuditBucket struct {
	Tool          string  `json:"tool"`
	Calls         uint64  `json:"calls"`
	Errors        uint64  `json:"errors"`
	ErrorRatePct  float64 `json:"errorRatePct"`
	MinDurationMs uint64  `json:"minDurationMs"`
	AvgDurationMs uint64  `json:"avgDurationMs"`
	MaxDurationMs uint64  `json:"maxDurationMs"`
}

type AdminSummaryResult struct {
	Window      string             `json:"window"`
	Since       time.Time          `json:"since"`
	TotalCalls  uint64             `json:"totalCalls"`
	TotalErrors uint64             `json:"totalErrors"`
	Tools       []AdminAuditBucket `json:"tools"`
}

func (s *Server) AuditSummaryQuery(ctx context.Context, window string) (*SummaryResult, error) {
	if s.DB == nil {
		return nil, fmt.Errorf("database not configured")
	}
	if window == "" {
		window = "24h"
	}
	d, err := time.ParseDuration(window)
	if err != nil || d <= 0 || d > 30*24*time.Hour {
		return nil, fmt.Errorf("window must be a positive duration up to 720h")
	}
	since := time.Now().UTC().Add(-d)
	out := &SummaryResult{Window: window, Since: since, Gateway: []GatewayBucket{}, Tactical: []TacticalBucket{}}

	rows, err := s.DB.QueryContext(ctx,
		"SELECT bot_guid, COUNT(*), SUM(error), COALESCE(AVG(latency_ms),0), "+
			"COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0) "+
			"FROM mod_ollama_chat_gateway_audit WHERE ts >= ? GROUP BY bot_guid ORDER BY COUNT(*) DESC", since)
	if err != nil {
		return nil, fmt.Errorf("gateway agg: %w", err)
	}
	for rows.Next() {
		var b GatewayBucket
		if err := rows.Scan(&b.BotGuid, &b.Calls, &b.Errors, &b.AvgLatencyMs, &b.PromptTokens, &b.CompletionTok); err != nil {
			rows.Close()
			return nil, fmt.Errorf("gateway scan: %w", err)
		}
		out.Gateway = append(out.Gateway, b)
	}
	rows.Close()

	rows, err = s.DB.QueryContext(ctx,
		"SELECT bot_guid, bot_name, action, result, COUNT(*), SUM(escalated) "+
			"FROM mod_ollama_chat_tactical_audit WHERE ts >= ? "+
			"GROUP BY bot_guid, bot_name, action, result ORDER BY COUNT(*) DESC LIMIT 200", since)
	if err != nil {
		return nil, fmt.Errorf("tactical agg: %w", err)
	}
	for rows.Next() {
		var b TacticalBucket
		if err := rows.Scan(&b.BotGuid, &b.BotName, &b.Action, &b.Result, &b.Count, &b.Escalated); err != nil {
			rows.Close()
			return nil, fmt.Errorf("tactical scan: %w", err)
		}
		out.Tactical = append(out.Tactical, b)
	}
	rows.Close()
	return out, nil
}

// AuditAdminSummaryQuery aggregates mod_ollama_chat_admin_audit rows over a
// time window into per-tool buckets (calls, errors, error-rate %, duration
// min/avg/max). Parallels AuditSummaryQuery for the gateway/tactical tables.
//
// The wow-ops-api-improve cron's gap-finding step ("which tools come up most,
// which error, which are slow?") previously had to call ops_audit_admin and
// fold rows in-memory — this collapses that into one round trip with the
// aggregation pushed down to the DB. Operators investigating ops-api
// behavior get the same shortcut.
func (s *Server) AuditAdminSummaryQuery(ctx context.Context, window string) (*AdminSummaryResult, error) {
	if s.DB == nil {
		return nil, fmt.Errorf("database not configured")
	}
	if window == "" {
		window = "24h"
	}
	d, err := time.ParseDuration(window)
	if err != nil || d <= 0 || d > 30*24*time.Hour {
		return nil, fmt.Errorf("window must be a positive duration up to 720h")
	}
	since := time.Now().UTC().Add(-d)
	out := &AdminSummaryResult{Window: window, Since: since, Tools: []AdminAuditBucket{}}

	// CAST AVG to UNSIGNED so MySQL returns it as an integer instead of a
	// DECIMAL — the Go driver delivers DECIMAL as []byte ("7.0000"), which
	// blows up rows.Scan into uint64 ("driver.Value type []uint8 ... invalid
	// syntax"). MIN/MAX are already integer-typed via the INT UNSIGNED column.
	rows, err := s.DB.QueryContext(ctx,
		"SELECT tool, COUNT(*), "+
			"SUM(CASE WHEN result='error' THEN 1 ELSE 0 END), "+
			"COALESCE(MIN(duration_ms),0), "+
			"CAST(COALESCE(AVG(duration_ms),0) AS UNSIGNED), "+
			"COALESCE(MAX(duration_ms),0) "+
			"FROM mod_ollama_chat_admin_audit WHERE ts >= ? "+
			"GROUP BY tool ORDER BY COUNT(*) DESC LIMIT 200", since)
	if err != nil {
		return nil, fmt.Errorf("admin agg: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var b AdminAuditBucket
		if err := rows.Scan(&b.Tool, &b.Calls, &b.Errors,
			&b.MinDurationMs, &b.AvgDurationMs, &b.MaxDurationMs); err != nil {
			return nil, fmt.Errorf("admin scan: %w", err)
		}
		if b.Calls > 0 {
			// Round to 1 decimal — matches the granularity operators
			// actually reason about ("10.5% error rate" vs "10.4823%").
			b.ErrorRatePct = float64(int(float64(b.Errors)/float64(b.Calls)*1000+0.5)) / 10
		}
		out.TotalCalls += b.Calls
		out.TotalErrors += b.Errors
		out.Tools = append(out.Tools, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("admin rows: %w", err)
	}
	return out, nil
}

func (s *Server) AuditGateway(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ctx, cancel := context.WithTimeout(r.Context(), s.Cfg.RequestTimeout)
	defer cancel()
	f := AuditFilters{
		BotGuid:    q.Get("botGuid"),
		AccountID:  q.Get("accountId"),
		From:       q.Get("from"),
		To:         q.Get("to"),
		ErrorsOnly: q.Get("errors") == "1",
		Limit:      atoi(q.Get("limit"), 200),
		Offset:     atoi(q.Get("offset"), 0),
	}
	rows, err := s.AuditGatewayQuery(ctx, f)
	if err != nil {
		if err.Error() == "database not configured" {
			writeError(w, http.StatusServiceUnavailable, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	limit := clampInt(f.Limit, 1, 1000)
	if f.Limit == 0 {
		limit = 200
	}
	offset := clampInt(f.Offset, 0, 1<<20)
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "limit": limit, "offset": offset})
}

func (s *Server) AuditTactical(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ctx, cancel := context.WithTimeout(r.Context(), s.Cfg.RequestTimeout)
	defer cancel()
	f := AuditFilters{
		BotGuid:   q.Get("botGuid"),
		Action:    q.Get("action"),
		Result:    q.Get("result"),
		Escalated: q.Get("escalated") == "1",
		From:      q.Get("from"),
		To:        q.Get("to"),
		Limit:     atoi(q.Get("limit"), 200),
		Offset:    atoi(q.Get("offset"), 0),
	}
	rows, err := s.AuditTacticalQuery(ctx, f)
	if err != nil {
		if err.Error() == "database not configured" {
			writeError(w, http.StatusServiceUnavailable, err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	limit := clampInt(f.Limit, 1, 1000)
	if f.Limit == 0 {
		limit = 200
	}
	offset := clampInt(f.Offset, 0, 1<<20)
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows, "limit": limit, "offset": offset})
}

func (s *Server) AuditSummary(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.Cfg.RequestTimeout)
	defer cancel()
	res, err := s.AuditSummaryQuery(ctx, r.URL.Query().Get("window"))
	if err != nil {
		switch err.Error() {
		case "database not configured":
			writeError(w, http.StatusServiceUnavailable, err.Error())
		case "window must be a positive duration up to 720h":
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
