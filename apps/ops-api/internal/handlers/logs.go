package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
)

type LogLine struct {
	TS     time.Time `json:"ts"`
	Stream string    `json:"stream"`
	Msg    string    `json:"msg"`
}

type LogsResult struct {
	Lines     []LogLine `json:"lines"`
	Truncated bool      `json:"truncated"`
}

// LogsTail is the pure-result function backing both REST and MCP log-tail calls.
// container=="" defaults to the worldserver container.
func (s *Server) LogsTail(ctx context.Context, container, tail, since, grep string, useRegex bool) (LogsResult, error) {
	if container == "" {
		container = s.Cfg.WorldserverContainer
	}
	if tail == "" {
		tail = "200"
	}
	if n, err := strconv.Atoi(tail); err != nil || n < 1 || n > 5000 {
		return LogsResult{}, fmt.Errorf("lines must be 1..5000")
	}
	matcher, err := buildMatcher(grep, useRegex)
	if err != nil {
		return LogsResult{}, err
	}
	ch, _, err := s.Docker.Stream(ctx, container, dockerlog.LogOpts{Tail: tail, Since: since})
	if err != nil {
		return LogsResult{}, fmt.Errorf("docker logs: %w", err)
	}
	out := LogsResult{Lines: []LogLine{}}
	const cap = 5000
	for ln := range ch {
		if matcher != nil && !matcher(ln.Msg) {
			continue
		}
		if len(out.Lines) >= cap {
			out.Truncated = true
			break
		}
		out.Lines = append(out.Lines, LogLine{TS: ln.TS, Stream: ln.Stream, Msg: ln.Msg})
	}
	return out, nil
}

func (s *Server) Logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ctx, cancel := context.WithTimeout(r.Context(), s.Cfg.RequestTimeout)
	defer cancel()

	res, err := s.LogsTail(ctx, "", q.Get("lines"), q.Get("since"), q.Get("grep"), q.Get("regex") == "1")
	if err != nil {
		// Map domain errors to status codes; bad input → 400, docker → 502.
		if strings.HasPrefix(err.Error(), "docker logs: ") {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) LogsStream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since := q.Get("since")
	if since == "" {
		since = "0s"
	}
	grep := q.Get("grep")
	useRegex := q.Get("regex") == "1"

	matcher, err := buildMatcher(grep, useRegex)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ctx, cancel := context.WithTimeout(r.Context(), s.Cfg.StreamTimeout)
	defer cancel()

	ch, _, err := s.Docker.Stream(ctx, s.Cfg.WorldserverContainer, dockerlog.LogOpts{Tail: "0", Since: since, Follow: true})
	if err != nil {
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", jsonString(err.Error()))
		flusher.Flush()
		return
	}

	// Heartbeat every 25s to keep proxies/clients awake even when the worldserver is idle.
	hb := time.NewTicker(25 * time.Second)
	defer hb.Stop()

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)

	for {
		select {
		case <-ctx.Done():
			return
		case <-hb.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case ln, ok := <-ch:
			if !ok {
				return
			}
			if matcher != nil && !matcher(ln.Msg) {
				continue
			}
			fmt.Fprint(w, "data: ")
			_ = enc.Encode(LogLine{TS: ln.TS, Stream: ln.Stream, Msg: ln.Msg})
			fmt.Fprint(w, "\n")
			flusher.Flush()
		}
	}
}

func buildMatcher(grep string, useRegex bool) (func(string) bool, error) {
	if grep == "" {
		return nil, nil
	}
	if useRegex {
		re, err := regexp.Compile(grep)
		if err != nil {
			return nil, fmt.Errorf("invalid regex: %w", err)
		}
		return re.MatchString, nil
	}
	return func(s string) bool { return strings.Contains(s, grep) }, nil
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
