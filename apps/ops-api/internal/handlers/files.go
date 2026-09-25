package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/files"
)

func (s *Server) FilesList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	root := q.Get("root")
	path := q.Get("path")
	glob := q.Get("glob")
	if root == "" {
		writeError(w, http.StatusBadRequest, "root is required")
		return
	}
	entries, err := s.Files.List(root, path, glob)
	if err != nil {
		writeFilesError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"root": root, "path": path, "entries": entries})
}

func (s *Server) FilesRead(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	root := q.Get("root")
	path := q.Get("path")
	if root == "" || path == "" {
		writeError(w, http.StatusBadRequest, "root and path are required")
		return
	}
	start := atoi(q.Get("start"), 0)
	end := atoi(q.Get("end"), 0)
	maxBytes := atoi(q.Get("maxBytes"), files.DefaultMaxRead)

	res, err := s.Files.Read(root, path, start, end, maxBytes)
	if err != nil {
		writeFilesError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) FilesSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	root := q.Get("root")
	queryStr := q.Get("q")
	path := q.Get("path")
	if root == "" || queryStr == "" {
		writeError(w, http.StatusBadRequest, "root and q are required")
		return
	}
	useRegex := q.Get("regex") == "1"
	max := atoi(q.Get("max"), files.MaxSearchHits)

	hits, timedOut, err := s.Files.Search(r.Context(), root, path, queryStr, useRegex, max)
	if err != nil {
		writeFilesError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": hits, "timedOut": timedOut})
}

func writeFilesError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, files.ErrUnknownRoot):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, files.ErrPathOutsideRoot):
		writeError(w, http.StatusBadRequest, "path outside allowlist")
	case errors.Is(err, files.ErrPathNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, files.ErrIsDir):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func atoi(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
