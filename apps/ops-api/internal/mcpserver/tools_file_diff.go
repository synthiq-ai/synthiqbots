package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	fileDiffMaxFileBytes    = 2 * 1024 * 1024 // 2 MiB per side
	fileDiffMaxFileLines    = 3000            // LCS table is O(n*m) ints; 3000² ≈ 72 MiB worst case
	fileDiffDefaultMaxLines = 500
	fileDiffHardCapLines    = 5000
	fileDiffContextLines    = 3
	fileDiffMaxBackupList   = 10
)

// FileDiffDeps wires file_diff to the same allowlist + backup dir as
// file_set_key. We deliberately reuse AllowedFiles (basename allowlist) so the
// agent can only diff files it could have edited in the first place — there is
// no separate diff allowlist to keep in sync.
type FileDiffDeps struct {
	ConfigRoot   string
	BackupDir    string
	AllowedFiles []string
}

// RegisterFileDiffTool registers `file_diff` — a read-only tool that returns a
// unified diff between an allowlisted .conf file (under OPS_ADMIN_CONFIG_ROOT)
// and one of its timestamped backups under OPS_ADMIN_BACKUP_DIR. Closes the
// post-edit verification gap: after file_set_key/file_write runs, the operator
// previously had no way to inspect the resulting `<basename>.<ts>.bak` files
// via MCP because the backup dir is not exposed under OPS_FILE_ROOTS.
func RegisterFileDiffTool(reg *Registry, deps FileDiffDeps) {
	reg.Register(Tool{
		Name: "file_diff",
		Description: "Show a unified diff between an allowlisted .conf file (under " +
			"OPS_ADMIN_CONFIG_ROOT) and one of its timestamped backups under " +
			"OPS_ADMIN_BACKUP_DIR. Defaults to the most recent backup; pass `backupTs` " +
			"to pick a specific one. Use this to verify what file_set_key / file_write " +
			"actually changed — the backup directory is not exposed under OPS_FILE_ROOTS, " +
			"so this is the only way to inspect *.bak via MCP. `file` must be in " +
			"OPS_ADMIN_ALLOWED_FILES (same allowlist as file_set_key — there is no " +
			"separate diff allowlist). Returns {addedLines, removedLines, diff (unified " +
			"format), backupTs, backupAgeSeconds, availableBackups[]}.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"file":{"type":"string","description":"Basename of the file (must be in OPS_ADMIN_ALLOWED_FILES)"},
			"backupTs":{"type":"integer","description":"Specific backup unix timestamp (default: latest backup)"},
			"reverse":{"type":"boolean","description":"Reverse direction — diff backup→current instead of current→backup (default false)"},
			"maxLines":{"type":"integer","description":"Cap on diff output lines (default 500, max 5000)"}
		},"required":["file"]}`),
		Annotations: AnnRead(),
		Handler: func(_ context.Context, raw json.RawMessage, _ string) any {
			return handleFileDiff(deps, raw)
		},
	})
}

func handleFileDiff(deps FileDiffDeps, raw json.RawMessage) any {
	var a struct {
		File     string `json:"file"`
		BackupTs any    `json:"backupTs"`
		Reverse  bool   `json:"reverse"`
		MaxLines int    `json:"maxLines"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return map[string]any{"error": "decode args: " + err.Error()}
	}
	if a.File == "" {
		return map[string]any{"error": "file is required"}
	}
	if !isSafeBasename(a.File) {
		return map[string]any{"error": "file must be a plain basename"}
	}
	if !contains(deps.AllowedFiles, a.File) {
		return map[string]any{"error": "file not in OPS_ADMIN_ALLOWED_FILES"}
	}
	if deps.ConfigRoot == "" {
		return map[string]any{"error": "OPS_ADMIN_CONFIG_ROOT not configured"}
	}
	if deps.BackupDir == "" {
		return map[string]any{"error": "OPS_ADMIN_BACKUP_DIR not configured"}
	}

	currentPath := filepath.Join(deps.ConfigRoot, a.File)
	cleanRoot := filepath.Clean(deps.ConfigRoot) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(currentPath), cleanRoot) {
		return map[string]any{"error": "resolved current path escapes config root"}
	}

	backups, err := listBackups(deps.BackupDir, a.File)
	if err != nil {
		return map[string]any{"error": "listing backups: " + err.Error()}
	}
	if len(backups) == 0 {
		return map[string]any{
			"error":            "no backups found for this file",
			"backupDir":        deps.BackupDir,
			"availableBackups": []map[string]any{},
		}
	}

	pickTsStr := numToString(a.BackupTs)
	var picked backupInfo
	if pickTsStr == "" || pickTsStr == "0" {
		picked = backups[0] // sorted desc — most recent first
	} else {
		wantTs, err := strconv.ParseInt(pickTsStr, 10, 64)
		if err != nil {
			return map[string]any{"error": "backupTs must be a unix-seconds integer"}
		}
		idx := -1
		for i, b := range backups {
			if b.Ts == wantTs {
				idx = i
				break
			}
		}
		if idx < 0 {
			return map[string]any{
				"error":            fmt.Sprintf("no backup with ts=%d", wantTs),
				"availableBackups": briefBackups(backups),
			}
		}
		picked = backups[idx]
	}

	currentLines, currentErr := readLinesCapped(currentPath, fileDiffMaxFileBytes, fileDiffMaxFileLines)
	if currentErr != nil {
		return map[string]any{"error": "reading current: " + currentErr.Error()}
	}
	backupLines, backupErr := readLinesCapped(picked.Path, fileDiffMaxFileBytes, fileDiffMaxFileLines)
	if backupErr != nil {
		return map[string]any{"error": "reading backup: " + backupErr.Error()}
	}

	fromLabel := picked.Path
	toLabel := currentPath
	from := backupLines
	to := currentLines
	if a.Reverse {
		fromLabel, toLabel = toLabel, fromLabel
		from, to = to, from
	}

	outCap := a.MaxLines
	if outCap <= 0 {
		outCap = fileDiffDefaultMaxLines
	}
	if outCap > fileDiffHardCapLines {
		outCap = fileDiffHardCapLines
	}

	added, removed, diffLines, truncated := unifiedDiff(from, to, fileDiffContextLines, outCap)

	out := map[string]any{
		"file":             a.File,
		"currentPath":      currentPath,
		"backupPath":       picked.Path,
		"backupTs":         picked.Ts,
		"backupAgeSeconds": int64(time.Since(time.Unix(picked.Ts, 0)).Seconds()),
		"fromLabel":        fromLabel,
		"toLabel":          toLabel,
		"addedLines":       added,
		"removedLines":     removed,
		"diff":             strings.Join(diffLines, "\n"),
		"truncated":        truncated,
		"availableBackups": briefBackups(backups),
	}
	if added == 0 && removed == 0 {
		out["identical"] = true
	}
	return out
}

type backupInfo struct {
	Path string
	Ts   int64
}

// listBackups scans backupDir for `<basename>.<unix_ts>.bak` files and returns
// them sorted newest first. Files whose middle segment is not a valid integer
// are skipped silently — the dir may legitimately contain other operator
// artefacts.
func listBackups(backupDir, basename string) ([]backupInfo, error) {
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	prefix := basename + "."
	var out []backupInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".bak") {
			continue
		}
		mid := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".bak")
		ts, err := strconv.ParseInt(mid, 10, 64)
		if err != nil {
			continue
		}
		out = append(out, backupInfo{
			Path: filepath.Join(backupDir, name),
			Ts:   ts,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ts > out[j].Ts })
	return out, nil
}

func briefBackups(backups []backupInfo) []map[string]any {
	limit := len(backups)
	if limit > fileDiffMaxBackupList {
		limit = fileDiffMaxBackupList
	}
	now := time.Now()
	out := make([]map[string]any, 0, limit)
	for _, b := range backups[:limit] {
		out = append(out, map[string]any{
			"ts":         b.Ts,
			"path":       b.Path,
			"ageSeconds": int64(now.Sub(time.Unix(b.Ts, 0)).Seconds()),
		})
	}
	return out
}

func readLinesCapped(path string, maxBytes int64, maxLines int) ([]string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Size() > maxBytes {
		return nil, fmt.Errorf("file too large: %d bytes (cap %d)", st.Size(), maxBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := string(data)
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil, nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) > maxLines {
		return nil, fmt.Errorf("file too many lines: %d (cap %d)", len(lines), maxLines)
	}
	return lines, nil
}

type diffOp struct {
	kind byte // ' ' equal, '+' insert (in `to`), '-' delete (from `from`)
	text string
}

// editScript computes a Myers/LCS-based edit script. O(n*m) time and space —
// fine for the .conf-sized files this tool targets (capped at 3000 lines per
// side; LCS table is ~72 MiB worst case at int sizing).
func editScript(from, to []string) []diffOp {
	n, m := len(from), len(to)
	if n == 0 && m == 0 {
		return nil
	}
	if n == 0 {
		out := make([]diffOp, m)
		for i, l := range to {
			out[i] = diffOp{'+', l}
		}
		return out
	}
	if m == 0 {
		out := make([]diffOp, n)
		for i, l := range from {
			out[i] = diffOp{'-', l}
		}
		return out
	}

	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= m; j++ {
			if from[i-1] == to[j-1] {
				lcs[i][j] = lcs[i-1][j-1] + 1
			} else if lcs[i-1][j] >= lcs[i][j-1] {
				lcs[i][j] = lcs[i-1][j]
			} else {
				lcs[i][j] = lcs[i][j-1]
			}
		}
	}

	// Walk back, then reverse.
	var rev []diffOp
	i, j := n, m
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && from[i-1] == to[j-1]:
			rev = append(rev, diffOp{' ', from[i-1]})
			i--
			j--
		case j > 0 && (i == 0 || lcs[i][j-1] >= lcs[i-1][j]):
			rev = append(rev, diffOp{'+', to[j-1]})
			j--
		default:
			rev = append(rev, diffOp{'-', from[i-1]})
			i--
		}
	}
	for k, l := 0, len(rev)-1; k < l; k, l = k+1, l-1 {
		rev[k], rev[l] = rev[l], rev[k]
	}
	return rev
}

// unifiedDiff renders an edit script as a unified-format diff. ctx is the
// number of context lines around each change. maxLines caps the total output
// (header + body); when the cap is reached the function returns truncated=true
// and stops emitting further hunks/lines.
func unifiedDiff(from, to []string, ctx, maxLines int) (added, removed int, lines []string, truncated bool) {
	script := editScript(from, to)
	for _, o := range script {
		switch o.kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	if added == 0 && removed == 0 {
		return
	}

	// Map each script index to its starting 1-based (from-line, to-line). At
	// index n (one past the end) we record the file lengths +1, useful for
	// hunk-header arithmetic.
	n := len(script)
	aAt := make([]int, n+1)
	bAt := make([]int, n+1)
	a, b := 1, 1
	for k, o := range script {
		aAt[k] = a
		bAt[k] = b
		switch o.kind {
		case ' ':
			a++
			b++
		case '-':
			a++
		case '+':
			b++
		}
	}
	aAt[n] = a
	bAt[n] = b

	var changes []int
	for k, o := range script {
		if o.kind != ' ' {
			changes = append(changes, k)
		}
	}

	// Group changes into hunks: each change pulls in `ctx` surrounding equal
	// lines as context; adjacent changes whose contexts overlap (or touch) get
	// merged into one hunk. This matches `diff -u` behaviour.
	type hunk struct{ start, end int } // both inclusive script indices
	var hunks []hunk
	hStart := changes[0] - ctx
	if hStart < 0 {
		hStart = 0
	}
	hEnd := changes[0] + ctx
	for _, c := range changes[1:] {
		if c-ctx <= hEnd+1 {
			hEnd = c + ctx
		} else {
			if hEnd >= n {
				hEnd = n - 1
			}
			hunks = append(hunks, hunk{hStart, hEnd})
			hStart = c - ctx
			if hStart < 0 {
				hStart = 0
			}
			hEnd = c + ctx
		}
	}
	if hEnd >= n {
		hEnd = n - 1
	}
	hunks = append(hunks, hunk{hStart, hEnd})

	for _, h := range hunks {
		aLen, bLen := 0, 0
		for k := h.start; k <= h.end; k++ {
			switch script[k].kind {
			case ' ':
				aLen++
				bLen++
			case '-':
				aLen++
			case '+':
				bLen++
			}
		}
		header := fmt.Sprintf("@@ -%d,%d +%d,%d @@", aAt[h.start], aLen, bAt[h.start], bLen)
		if len(lines)+1 > maxLines {
			truncated = true
			return
		}
		lines = append(lines, header)
		for k := h.start; k <= h.end; k++ {
			if len(lines) >= maxLines {
				truncated = true
				return
			}
			lines = append(lines, string(script[k].kind)+script[k].text)
		}
	}
	return
}
