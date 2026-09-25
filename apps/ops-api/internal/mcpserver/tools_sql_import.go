package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type SqlImportDeps struct {
	WowRoot  string  // /wow-root — all import paths must resolve under this prefix
	ExecDB   *sql.DB // ops_rw pool; DSN must include multiStatements=true
	BaseRoot string  // override for tests; defaults to WowRoot
}

const (
	sqlImportFileTimeout = 60 * time.Second
	sqlImportDirTimeout  = 10 * time.Minute // worst case: full mod-playerbots tree
	sqlImportMaxFileMB   = 25               // single .sql > 25 MB → refuse, almost certainly wrong file
)

// sqlImportFileResult is the per-file shape returned by both tools.
type sqlImportFileResult struct {
	File          string `json:"file"`
	Database      string `json:"database"`
	Ok            bool   `json:"ok"`
	RowsAffected  int64  `json:"rows_affected,omitempty"` // sum across statements
	BytesImported int    `json:"bytes_imported,omitempty"`
	DurationMs    int    `json:"duration_ms,omitempty"`
	Error         string `json:"error,omitempty"`
	Skipped       bool   `json:"skipped,omitempty"` // true if filtered by exclude_*
	SkipReason    string `json:"skip_reason,omitempty"`
}

// RegisterSqlImportTools registers sql_import_file + sql_import_dir.
//
// Both tools require ExecDB to be opened with multiStatements=true (otherwise
// any .sql with more than one statement will fail at the driver). They share
// the same path-safety model: caller-supplied paths must resolve under
// SqlImportDeps.WowRoot after symlink eval — this is the same defense
// tools_files.go uses for read access.
//
// The tools share execution semantics with db_exec (USE <db> on a fresh conn,
// then Exec the contents) but are batched at file granularity, not statement
// granularity. Per-file errors do not abort sql_import_dir unless
// continue_on_error=false (the default — fail closed so a single bad file
// halts the migration before it spreads).
func RegisterSqlImportTools(reg *Registry, deps SqlImportDeps) {
	if deps.BaseRoot == "" {
		deps.BaseRoot = deps.WowRoot
	}

	reg.Register(Tool{
		Name: "sql_import_file",
		Description: "Import a single .sql file into one of acore_world / acore_characters / " +
			"acore_auth via the ops_rw user. Path must resolve under $OPS_WOW_ROOT (default " +
			"/wow-root). REQUIRES `confirm:true`. The pool's DSN must include " +
			"`multiStatements=true` — files with multiple statements will otherwise fail at " +
			"the driver. Returns {file, database, ok, rows_affected, bytes_imported, " +
			"duration_ms, error}. 25 MB per-file cap.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"path":{"type":"string","description":"Absolute path or path relative to $OPS_WOW_ROOT"},
			"database":{"type":"string","enum":["acore_world","acore_characters","acore_auth"]},
			"confirm":{"type":"boolean","description":"MUST be true to actually execute"}
		},"required":["path","database","confirm"]}`),
		Annotations: AnnAction(false),
		Destructive: true,
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Path     string `json:"path"`
				Database string `json:"database"`
				Confirm  bool   `json:"confirm"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if !a.Confirm {
				return map[string]any{"error": "confirm:true required for sql_import_file"}
			}
			if !allowedDatabases[a.Database] {
				return map[string]any{"error": "database must be one of acore_world/acore_characters/acore_auth"}
			}
			if deps.ExecDB == nil {
				return map[string]any{"error": "ops_rw pool not configured (DB_EXEC_DSN/OPS_DB_IMPORT_DSN empty?)"}
			}
			abs, err := resolveUnderRoot(deps.BaseRoot, a.Path)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			c, cancel := context.WithTimeout(ctx, sqlImportFileTimeout)
			defer cancel()
			res := importOneFile(c, deps.ExecDB, abs, a.Database)
			return res
		},
	})

	reg.Register(Tool{
		Name: "sql_import_dir",
		Description: "Import every .sql file in a directory (NOT recursive) into one of " +
			"acore_world / acore_characters / acore_auth, alphabetically sorted by basename. " +
			"Use exclude_files (basenames, exact match) and/or exclude_glob (filepath.Match " +
			"against basename) to skip optional SQL — many modules ship optional schemas the " +
			"script otherwise imports blindly. continue_on_error=false (default) halts on the " +
			"first failure; =true reports all failures and keeps going. REQUIRES `confirm:true`. " +
			"Returns {dir, database, files:[{file, ok, rows_affected, bytes_imported, " +
			"duration_ms, error, skipped, skip_reason}, ...], total_files, imported, skipped, " +
			"failed}. Directory must resolve under $OPS_WOW_ROOT.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"dir":{"type":"string","description":"Directory under $OPS_WOW_ROOT containing .sql files"},
			"database":{"type":"string","enum":["acore_world","acore_characters","acore_auth"]},
			"exclude_files":{"type":"array","items":{"type":"string"},"description":"Exact basenames to skip"},
			"exclude_glob":{"type":"string","description":"filepath.Match glob (e.g. *_optional.sql) tested against basename"},
			"continue_on_error":{"type":"boolean","description":"Keep importing after a per-file failure (default false)"},
			"confirm":{"type":"boolean","description":"MUST be true to actually execute"}
		},"required":["dir","database","confirm"]}`),
		Annotations: AnnAction(false),
		Destructive: true,
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Dir             string   `json:"dir"`
				Database        string   `json:"database"`
				ExcludeFiles    []string `json:"exclude_files"`
				ExcludeGlob     string   `json:"exclude_glob"`
				ContinueOnError bool     `json:"continue_on_error"`
				Confirm         bool     `json:"confirm"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if !a.Confirm {
				return map[string]any{"error": "confirm:true required for sql_import_dir"}
			}
			if !allowedDatabases[a.Database] {
				return map[string]any{"error": "database must be one of acore_world/acore_characters/acore_auth"}
			}
			if deps.ExecDB == nil {
				return map[string]any{"error": "ops_rw pool not configured"}
			}
			absDir, err := resolveUnderRoot(deps.BaseRoot, a.Dir)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			info, err := os.Stat(absDir)
			if err != nil {
				return map[string]any{"error": "stat dir: " + err.Error()}
			}
			if !info.IsDir() {
				return map[string]any{"error": "path is not a directory: " + absDir}
			}
			// Validate the exclude_glob compiles before doing any work.
			if a.ExcludeGlob != "" {
				if _, err := filepath.Match(a.ExcludeGlob, "test.sql"); err != nil {
					return map[string]any{"error": "invalid exclude_glob: " + err.Error()}
				}
			}
			excludeSet := map[string]bool{}
			for _, name := range a.ExcludeFiles {
				excludeSet[name] = true
			}

			entries, err := os.ReadDir(absDir)
			if err != nil {
				return map[string]any{"error": "read dir: " + err.Error()}
			}
			files := []string{}
			for _, e := range entries {
				if e.IsDir() {
					continue
				}
				if !strings.HasSuffix(strings.ToLower(e.Name()), ".sql") {
					continue
				}
				files = append(files, e.Name())
			}
			sort.Strings(files)

			ctxAll, cancelAll := context.WithTimeout(ctx, sqlImportDirTimeout)
			defer cancelAll()

			results := make([]sqlImportFileResult, 0, len(files))
			imported, skipped, failed := 0, 0, 0
			for _, name := range files {
				if excludeSet[name] {
					results = append(results, sqlImportFileResult{File: name, Database: a.Database,
						Skipped: true, SkipReason: "in exclude_files"})
					skipped++
					continue
				}
				if a.ExcludeGlob != "" {
					if match, _ := filepath.Match(a.ExcludeGlob, name); match {
						results = append(results, sqlImportFileResult{File: name, Database: a.Database,
							Skipped: true, SkipReason: "matches exclude_glob"})
						skipped++
						continue
					}
				}
				c, cancel := context.WithTimeout(ctxAll, sqlImportFileTimeout)
				r := importOneFile(c, deps.ExecDB, filepath.Join(absDir, name), a.Database)
				cancel()
				results = append(results, r)
				if r.Ok {
					imported++
				} else {
					failed++
					if !a.ContinueOnError {
						return map[string]any{
							"dir": absDir, "database": a.Database, "files": results,
							"total_files": len(files), "imported": imported,
							"skipped": skipped, "failed": failed,
							"halted_on": name,
							"error":     fmt.Sprintf("import halted at %s (continue_on_error=false)", name),
						}
					}
				}
			}
			return map[string]any{
				"dir": absDir, "database": a.Database, "files": results,
				"total_files": len(files), "imported": imported,
				"skipped": skipped, "failed": failed,
			}
		},
	})
}

// importOneFile reads the file, USE <db>'s a fresh connection, then Execs the
// content as a single (multi-statement) Exec. The driver must be opened with
// multiStatements=true.
func importOneFile(ctx context.Context, db *sql.DB, abs, database string) sqlImportFileResult {
	out := sqlImportFileResult{File: filepath.Base(abs), Database: database}
	t0 := time.Now()
	st, err := os.Stat(abs)
	if err != nil {
		out.Error = "stat: " + err.Error()
		return out
	}
	if st.Size() > int64(sqlImportMaxFileMB)*1024*1024 {
		out.Error = fmt.Sprintf("file too large (%d bytes; cap %d MB)", st.Size(), sqlImportMaxFileMB)
		return out
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		out.Error = "read: " + err.Error()
		return out
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		out.Error = "acquire conn: " + err.Error()
		return out
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "USE `"+database+"`"); err != nil {
		out.Error = "USE " + database + ": " + err.Error()
		return out
	}
	res, err := conn.ExecContext(ctx, string(content))
	if err != nil {
		out.Error = "exec: " + err.Error()
		return out
	}
	if ra, _ := res.RowsAffected(); ra > 0 {
		out.RowsAffected = ra
	}
	out.BytesImported = len(content)
	out.DurationMs = int(time.Since(t0).Milliseconds())
	out.Ok = true
	return out
}

// resolveUnderRoot validates that the caller-supplied path lives under root
// after symlink resolution. Path may be absolute or relative to root. Returns
// the absolute, symlink-resolved path on success.
//
// Identical model to apps/ops-api/internal/files/files.go's Resolver but
// scoped to a single root and slimmed to one-shot use.
func resolveUnderRoot(root, p string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("OPS_WOW_ROOT not configured")
	}
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	abs := p
	if !filepath.IsAbs(p) {
		abs = filepath.Join(root, p)
	}
	clean := filepath.Clean(abs)
	// EvalSymlinks resolves ../ and any symlink chain — required to defend
	// against a hostile symlink under the bind mount that points to /etc.
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		// Allow nonexistent paths through (so the caller gets a clean "stat" /
		// "read" error rather than a path-resolution failure) — but only after
		// rejecting any obvious traversal in the cleaned path.
		if !strings.HasPrefix(clean, root+string(os.PathSeparator)) && clean != root {
			return "", fmt.Errorf("path %q escapes root %q", abs, root)
		}
		return clean, nil
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("eval root: %w", err)
	}
	if !strings.HasPrefix(resolved, rootResolved+string(os.PathSeparator)) && resolved != rootResolved {
		return "", fmt.Errorf("path %q escapes root %q", resolved, rootResolved)
	}
	return resolved, nil
}
