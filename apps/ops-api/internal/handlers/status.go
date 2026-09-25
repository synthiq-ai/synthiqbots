package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/gitsha"
)

type StatusResponse struct {
	Schema      int             `json:"schema"`
	GeneratedAt time.Time       `json:"generatedAt"`
	GitSHA      string          `json:"gitSHA,omitempty"`
	Worldserver WorldserverInfo `json:"worldserver"`
	MCPHealth   string          `json:"mcpHealth"`
	Module      json.RawMessage `json:"module,omitempty"`     // nested ops_module_status output
	ModuleError string          `json:"moduleError,omitempty"`
	DBOK        bool            `json:"dbOk"`
	DBError     string          `json:"dbError,omitempty"`
}

type WorldserverInfo struct {
	State        string    `json:"state"`
	Running      bool      `json:"running"`
	StartedAt    string    `json:"startedAt,omitempty"`
	Image        string    `json:"image,omitempty"`
	RestartCount int       `json:"restartCount"`
	ExitCode     int       `json:"exitCode,omitempty"`
	InspectedAt  time.Time `json:"inspectedAt"`
}

// StatusJSON is the pure-result function backing both the REST and MCP status calls.
func (s *Server) StatusJSON(ctx context.Context) StatusResponse {
	resp := StatusResponse{Schema: 1, GeneratedAt: time.Now().UTC()}

	if insp, err := s.Docker.Inspect(ctx, s.Cfg.WorldserverContainer); err == nil {
		resp.Worldserver = WorldserverInfo{
			State:        insp.State.Status,
			Running:      insp.State.Running,
			StartedAt:    insp.State.StartedAt,
			Image:        insp.Image,
			RestartCount: insp.RestartCount,
			ExitCode:     insp.State.ExitCode,
			InspectedAt:  time.Now().UTC(),
		}
	} else {
		resp.Worldserver = WorldserverInfo{State: "inspect_error: " + err.Error(), InspectedAt: time.Now().UTC()}
	}

	if err := s.MCP.Ping(ctx); err == nil {
		resp.MCPHealth = "ok"
	} else {
		resp.MCPHealth = "error: " + err.Error()
	}

	if raw, err := s.MCP.CallTool(ctx, "ops_module_status", nil); err == nil {
		resp.Module = raw
	} else {
		resp.ModuleError = err.Error()
	}

	// Resolve git SHA from the first available repo root. Best-effort.
	for _, root := range s.Files.Roots() {
		if sha, err := gitsha.Resolve(root); err == nil {
			resp.GitSHA = sha
			break
		}
	}

	if s.DB != nil {
		if err := s.DB.PingContext(ctx); err == nil {
			resp.DBOK = true
		} else {
			resp.DBError = err.Error()
		}
	} else {
		resp.DBError = "DB_DSN not configured"
	}

	return resp
}

func (s *Server) Status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.Cfg.RequestTimeout)
	defer cancel()
	writeJSON(w, http.StatusOK, s.StatusJSON(ctx))
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
