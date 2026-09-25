package handlers

// GET /v1/feedback/results — pull rows the dispatcher has finished
// processing (status in 'resolved' | 'error') so the daemon can write
// them back to a SavedVariables sidecar file the addon reads.
//
// The daemon advances a `since` cursor (client-side state) so each call
// is incremental. The endpoint returns up to `limit` rows ordered by id
// ascending — newest at the bottom keeps the cursor advancing
// monotonically as the daemon walks through the response.

import (
	"context"
	"net/http"
	"strconv"
)

type FeedbackResultRow struct {
	ID            int64  `json:"id"`
	AddonID       string `json:"addonId"`
	AddonTs       int64  `json:"addonTs"`
	Status        string `json:"status"`
	AgentResponse string `json:"agentResponse"`
	AgentActions  string `json:"agentActions"` // JSON string as-stored
	AgentPRURL    string `json:"agentPrUrl,omitempty"`
	ResolvedAt    string `json:"resolvedAt,omitempty"` // RFC3339
}

func (s *Server) FeedbackResults(w http.ResponseWriter, r *http.Request) {
	if s.DB == nil {
		writeError(w, http.StatusServiceUnavailable, "database not configured")
		return
	}
	q := r.URL.Query()
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	limit := atoi(q.Get("limit"), 100)
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.Cfg.RequestTimeout)
	defer cancel()

	rows, err := s.DB.QueryContext(ctx,
		"SELECT id, addon_id, addon_ts, status, "+
			"COALESCE(agent_response,''), COALESCE(agent_actions,''), "+
			"COALESCE(agent_pr_url,''), "+
			"COALESCE(DATE_FORMAT(resolved_at, '%Y-%m-%dT%H:%i:%sZ'),'') "+
			"FROM mod_ollama_chat_feedback "+
			"WHERE id > ? AND status IN ('resolved','error') "+
			"ORDER BY id ASC LIMIT ?", since, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query: "+err.Error())
		return
	}
	defer rows.Close()

	out := []FeedbackResultRow{}
	var maxID int64 = since
	for rows.Next() {
		var r FeedbackResultRow
		if err := rows.Scan(&r.ID, &r.AddonID, &r.AddonTs, &r.Status,
			&r.AgentResponse, &r.AgentActions, &r.AgentPRURL, &r.ResolvedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "scan: "+err.Error())
			return
		}
		out = append(out, r)
		if r.ID > maxID {
			maxID = r.ID
		}
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, "rows: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":      out,
		"nextSince": maxID,
		"count":     len(out),
	})
}
