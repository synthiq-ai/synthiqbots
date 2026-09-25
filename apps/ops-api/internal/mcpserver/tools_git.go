package mcpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// GitRunner runs `git -C dir <args...>`. Abstracted for tests — production
// uses execGitRunner which shells out via os/exec.
type GitRunner interface {
	RunGit(ctx context.Context, dir string, args ...string) (stdout string, stderr string, err error)
}

type execGitRunner struct{}

// gitInsteadOfArgs transparently rewrites GitHub SSH remotes to anonymous
// HTTPS at invocation time. The ops-api image ships git but NOT an ssh client
// (debian-slim, no openssh-client), so a module whose `origin` is a
// git@github.com: / ssh://git@github.com/ URL (e.g. mod-ah-bot, mod-ollama-chat)
// fails `git pull` with "cannot run ssh: No such file or directory". Those
// public repos pull fine over HTTPS, so we rewrite via url.<base>.insteadOf —
// no ssh key, no per-repo `git remote set-url`. Harmless for HTTPS remotes
// (insteadOf only rewrites matching prefixes) and a no-op for local-only ops
// like rev-parse.
var gitInsteadOfArgs = []string{
	"-c", "url.https://github.com/.insteadOf=git@github.com:",
	"-c", "url.https://github.com/.insteadOf=ssh://git@github.com/",
}

// buildGitArgs assembles the full `git` argv: the -C <dir> selector, the
// SSH->HTTPS insteadOf rewrites (both must precede the subcommand as git global
// options), then the caller's args.
func buildGitArgs(dir string, args []string) []string {
	full := make([]string, 0, 2+len(gitInsteadOfArgs)+len(args))
	full = append(full, "-C", dir)
	full = append(full, gitInsteadOfArgs...)
	full = append(full, args...)
	return full
}

func (execGitRunner) RunGit(ctx context.Context, dir string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, "git", buildGitArgs(dir, args)...)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	return so.String(), se.String(), err
}

// DefaultGitRunner is the production runner — exposed so main.go can wire
// without importing the unexported type.
func DefaultGitRunner() GitRunner { return execGitRunner{} }

type GitDeps struct {
	WowRoot       string    // /wow-root (the AzerothCore tree)
	Runner        GitRunner // injectable for tests
	ModulePattern string    // regex; defaults to ^mod-[a-z0-9-]+$

	// ExecDB is the ops_rw pool used by the import_sql:true branch on
	// git_pull_module / git_pull_all. Same DSN sql_import_dir uses
	// (multiStatements=true required). nil = import_sql is refused with a
	// clear "ops_rw not configured" error so the caller knows the env gap.
	ExecDB *sql.DB
}

const (
	gitPullTimeout = 60 * time.Second
)

// gitRepoResult is the per-repo response shape for all three git tools.
type gitRepoResult struct {
	Repo       string `json:"repo"` // "core" or "modules/<name>"
	Path       string `json:"path"` // absolute path inside the container
	HashBefore string `json:"hash_before,omitempty"`
	HashAfter  string `json:"hash_after,omitempty"`
	Output     string `json:"output"`           // git pull stdout (trimmed to ~4 KB)
	Stderr     string `json:"stderr,omitempty"` // git stderr (trimmed; usually progress info)
	Ok         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
}

// RegisterGitTools registers git_pull_core, git_pull_module, git_pull_all.
//
// All three are gated by Destructive — `git pull` mutates the on-disk state
// (and on a non-fast-forward leaves the tree in a half-merged state we can't
// auto-recover from). --ff-only is enforced so the worst case is "Already up
// to date" or a hard refusal — never a stray merge commit.
//
// Module name validation runs BEFORE filepath.Join to defend against `..`
// traversal — the regex anchors the whole string so `../etc/passwd` etc are
// rejected at the schema layer, not at the filesystem layer.
func RegisterGitTools(reg *Registry, deps GitDeps) {
	if deps.Runner == nil {
		deps.Runner = DefaultGitRunner()
	}
	if deps.ModulePattern == "" {
		deps.ModulePattern = `^mod-[a-z0-9-]+$`
	}
	modRe, err := regexp.Compile(deps.ModulePattern)
	if err != nil {
		// Bad pattern in env → fall back to safe default rather than crash.
		modRe = regexp.MustCompile(`^mod-[a-z0-9-]+$`)
	}

	reg.Register(Tool{
		Name: "git_pull_core",
		Description: "Run `git pull --ff-only` against the AzerothCore core repo at " +
			"$OPS_WOW_ROOT (defaults to /wow-root). Returns hash before/after, full git " +
			"stdout/stderr, and ok=true on a clean fast-forward (or 'Already up to date'). " +
			"Refuses any non-fast-forward.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Annotations: AnnAction(true),
		Destructive: true,
		Handler: func(ctx context.Context, _ json.RawMessage, _ string) any {
			c, cancel := context.WithTimeout(ctx, gitPullTimeout)
			defer cancel()
			res := gitPullOne(c, deps.Runner, "core", deps.WowRoot)
			return res
		},
	})

	reg.Register(Tool{
		Name: "git_pull_module",
		Description: "Run `git pull --ff-only` against a single AzerothCore module under " +
			"$OPS_WOW_ROOT/modules/<name>. The name must match $OPS_MODULE_NAME_PATTERN " +
			"(default ^mod-[a-z0-9-]+$) — anything else is rejected before any filesystem " +
			"access. Returns {repo, hash_before, hash_after, output, ok, error}.\n\n" +
			"With `import_sql:true`, after a successful pull the module's data/sql/ tree " +
			"is walked and standard files (data/sql/{auth,characters,world}/{base,updates}/" +
			"*.sql, plus mod-ah-bot's legacy db-world/) are auto-imported via ops_rw. " +
			"Optional files (any path under /optional/, filename matching zz_optional_*.sql, " +
			"or parent dir custom/archive/experimental) and unrecognized paths " +
			"(data/sql/<unknown-db>/) are LISTED, not imported. To import a specific " +
			"optional file, name its basename in `optional_includes`. Returns " +
			"{pull, imported:[{rel_dir, database, files:[{file, ok, ...}]}], " +
			"optional_available, unrecognized}.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"Module directory under modules/, e.g. mod-ollama-chat"},
			"import_sql":{"type":"boolean","description":"After pull, auto-import standard SQL via ops_rw and list optional files for explicit opt-in. Default false."},
			"optional_includes":{"type":"array","items":{"type":"string"},"description":"Basenames of optional .sql files to ALSO import. Listed files (under /optional/, matching zz_optional_*, or in custom/archive/experimental dirs) are gated on this list — empty = list-only, none imported."},
			"databases":{"type":"array","items":{"type":"string","enum":["acore_world","acore_characters","acore_auth"]},"description":"Filter which databases to touch. Default = all three. Use to limit blast radius (e.g. only acore_world for a content-only update)."}
		},"required":["name"]}`),
		Annotations: AnnAction(true),
		Destructive: true,
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Name             string   `json:"name"`
				ImportSQL        bool     `json:"import_sql"`
				OptionalIncludes []string `json:"optional_includes"`
				Databases        []string `json:"databases"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if a.Name == "" {
				return map[string]any{"error": "name is required"}
			}
			if !modRe.MatchString(a.Name) {
				return map[string]any{"error": fmt.Sprintf("name %q does not match %s", a.Name, deps.ModulePattern)}
			}
			path := filepath.Join(deps.WowRoot, "modules", a.Name)
			c, cancel := context.WithTimeout(ctx, gitPullTimeout)
			defer cancel()
			pullRes := gitPullOne(c, deps.Runner, "modules/"+a.Name, path)
			if !a.ImportSQL || !pullRes.Ok {
				return pullRes
			}
			// Pull succeeded and caller asked for SQL import — walk + import.
			imp := importModuleSQL(ctx, deps, path, a.OptionalIncludes, dbFilterSet(a.Databases))
			return mergePullAndImport(pullRes, imp)
		},
	})

	reg.Register(Tool{
		Name: "git_pull_all",
		Description: "Pull core + every directory under $OPS_WOW_ROOT/modules/ that matches " +
			"$OPS_MODULE_NAME_PATTERN. Runs sequentially. Returns {repos:[{repo, hash_before, " +
			"hash_after, output, ok, error}, ...], ok_count, error_count}. Partial failures " +
			"do NOT abort — each repo's success/failure is reported independently so the " +
			"agent can decide what to retry.\n\n" +
			"With `import_sql:true`, each successfully-pulled module's data/sql/ tree is " +
			"walked and standard files are auto-imported via ops_rw. Optional and " +
			"unrecognized paths are listed (per-module) so the agent can opt in by " +
			"calling git_pull_module with `optional_includes`. Core repo is skipped for " +
			"SQL import — module SQL only. `optional_includes` is shared across modules " +
			"by basename; pass per-module by calling git_pull_module afterward.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"import_sql":{"type":"boolean","description":"After each successful module pull, auto-import standard SQL via ops_rw and list optional files. Core repo is never imported. Default false."},
			"optional_includes":{"type":"array","items":{"type":"string"},"description":"Basenames of optional .sql files to ALSO import (matched across modules). Empty = list-only."},
			"databases":{"type":"array","items":{"type":"string","enum":["acore_world","acore_characters","acore_auth"]},"description":"Filter which databases to touch. Default = all three."}
		}}`),
		Annotations: AnnAction(true),
		Destructive: true,
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				ImportSQL        bool     `json:"import_sql"`
				OptionalIncludes []string `json:"optional_includes"`
				Databases        []string `json:"databases"`
			}
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &a); err != nil {
					return map[string]any{"error": "decode args: " + err.Error()}
				}
			}
			repos := []gitRepoResult{}
			imports := map[string]any{} // module -> import result; surfaced when ImportSQL is on
			// Per-repo timeout is gitPullTimeout; whole call gets gitPullTimeout * (1+modules).
			deadline := time.Now().Add(gitPullTimeout * 6) // ~6 repos in production
			ctxAll, cancelAll := context.WithDeadline(ctx, deadline)
			defer cancelAll()
			repos = append(repos, gitPullOne(ctxAll, deps.Runner, "core", deps.WowRoot))
			modulesDir := filepath.Join(deps.WowRoot, "modules")
			entries, err := os.ReadDir(modulesDir)
			if err != nil {
				return map[string]any{
					"error": "read modules dir " + modulesDir + ": " + err.Error(),
					"repos": repos,
				}
			}
			dbFilter := dbFilterSet(a.Databases)
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				if !modRe.MatchString(e.Name()) {
					continue
				}
				c, cancel := context.WithTimeout(ctxAll, gitPullTimeout)
				path := filepath.Join(modulesDir, e.Name())
				pullRes := gitPullOne(c, deps.Runner, "modules/"+e.Name(), path)
				cancel()
				repos = append(repos, pullRes)
				if a.ImportSQL && pullRes.Ok {
					imp := importModuleSQL(ctxAll, deps, path, a.OptionalIncludes, dbFilter)
					imports[e.Name()] = imp
				}
			}
			ok, errCount := 0, 0
			for _, r := range repos {
				if r.Ok {
					ok++
				} else {
					errCount++
				}
			}
			out := map[string]any{"repos": repos, "ok_count": ok, "error_count": errCount}
			if a.ImportSQL {
				out["imports"] = imports
			}
			return out
		},
	})
}

// dbFilterSet turns the `databases` arg into a set used by importModuleSQL to
// gate per-bucket importing. Empty input = no filter (all 3 acore_* DBs are
// allowed). Unknown names are dropped silently (the input schema enum already
// pre-validates).
func dbFilterSet(dbs []string) map[string]bool {
	if len(dbs) == 0 {
		return nil // nil = no filter
	}
	set := map[string]bool{}
	for _, d := range dbs {
		set[d] = true
	}
	return set
}

// moduleImportResult is the per-module SQL-import response embedded inside
// git_pull_module / git_pull_all output when import_sql:true.
type moduleImportResult struct {
	Imported          []moduleImportedDir  `json:"imported"`
	OptionalAvailable []ModuleSQLFile      `json:"optional_available"`
	OptionalImported  []moduleImportedFile `json:"optional_imported,omitempty"`
	Unrecognized      []ModuleSQLEntry     `json:"unrecognized"`
	Skipped           []moduleSkippedFile  `json:"skipped,omitempty"` // db-filtered or zz_optional_ not in includes
}

type moduleImportedDir struct {
	RelDir   string                `json:"rel_dir"`
	Database string                `json:"database"`
	Files    []sqlImportFileResult `json:"files"`
}

type moduleImportedFile struct {
	RelPath string              `json:"rel_path"`
	Result  sqlImportFileResult `json:"result"`
}

type moduleSkippedFile struct {
	RelPath string `json:"rel_path"`
	Reason  string `json:"reason"`
}

// importModuleSQL is the post-pull side of import_sql: walks the module tree
// (via walkModuleSQL), imports every Standard bucket through importOneFile,
// imports any Optional whose basename appears in optionalIncludes, and
// passes through Unrecognized as-is for the agent to decide.
//
// dbFilter (nil = allow all): if set, only buckets whose database is in the
// set are imported; everything else is reported in `skipped`.
//
// All imports run on a single per-call timeout — sqlImportDirTimeout already
// covers the worst-case tree (full mod-playerbots).
func importModuleSQL(parentCtx context.Context, deps GitDeps, modulePath string,
	optionalIncludes []string, dbFilter map[string]bool,
) moduleImportResult {
	plan := walkModuleSQL(modulePath)
	out := moduleImportResult{
		Imported:          []moduleImportedDir{},
		OptionalAvailable: plan.Optional,
		Unrecognized:      plan.Unrecognized,
	}
	if deps.ExecDB == nil {
		// No ops_rw pool — surface the gap without dropping pull progress.
		out.Skipped = append(out.Skipped, moduleSkippedFile{
			RelPath: "(all)", Reason: "ops_rw pool not configured (DB_EXEC_DSN/OPS_DB_IMPORT_DSN empty)",
		})
		return out
	}
	includeSet := map[string]bool{}
	for _, b := range optionalIncludes {
		includeSet[b] = true
	}

	ctx, cancel := context.WithTimeout(parentCtx, sqlImportDirTimeout)
	defer cancel()

	// 1. Standard buckets — but filter zz_optional_*.sql to Optional unless
	//    the agent explicitly opted in.
	for _, bucket := range plan.Standard {
		if dbFilter != nil && !dbFilter[bucket.Database] {
			for _, f := range bucket.Files {
				out.Skipped = append(out.Skipped, moduleSkippedFile{
					RelPath: filepath.Join(bucket.RelDir, f),
					Reason:  "database not in `databases` filter",
				})
			}
			continue
		}
		results := []sqlImportFileResult{}
		for _, f := range bucket.Files {
			abs := filepath.Join(modulePath, bucket.RelDir, f)
			if isOptionalFilename(f) && !includeSet[f] {
				out.Skipped = append(out.Skipped, moduleSkippedFile{
					RelPath: filepath.Join(bucket.RelDir, f),
					Reason:  "zz_optional_ prefix; add to optional_includes to import",
				})
				continue
			}
			perCtx, perCancel := context.WithTimeout(ctx, sqlImportFileTimeout)
			r := importOneFile(perCtx, deps.ExecDB, abs, bucket.Database)
			perCancel()
			results = append(results, r)
		}
		if len(results) > 0 {
			out.Imported = append(out.Imported, moduleImportedDir{
				RelDir:   bucket.RelDir,
				Database: bucket.Database,
				Files:    results,
			})
		}
	}

	// 2. Optional files — import only those the agent named, surface the rest
	//    in OptionalAvailable so the next call can opt in.
	for _, opt := range plan.Optional {
		if !includeSet[opt.Basename] {
			continue
		}
		if opt.Database == "" {
			out.Skipped = append(out.Skipped, moduleSkippedFile{
				RelPath: opt.RelPath,
				Reason:  "DB inference failed; cannot auto-route — call sql_import_file with explicit database",
			})
			continue
		}
		if dbFilter != nil && !dbFilter[opt.Database] {
			out.Skipped = append(out.Skipped, moduleSkippedFile{
				RelPath: opt.RelPath,
				Reason:  "database not in `databases` filter",
			})
			continue
		}
		abs := filepath.Join(modulePath, opt.RelPath)
		perCtx, perCancel := context.WithTimeout(ctx, sqlImportFileTimeout)
		r := importOneFile(perCtx, deps.ExecDB, abs, opt.Database)
		perCancel()
		out.OptionalImported = append(out.OptionalImported, moduleImportedFile{
			RelPath: opt.RelPath,
			Result:  r,
		})
	}
	return out
}

// mergePullAndImport flattens the {pull, import} composition for the
// git_pull_module response so the agent sees a single map at the top level
// rather than nested objects.
func mergePullAndImport(pull gitRepoResult, imp moduleImportResult) map[string]any {
	out := map[string]any{
		"repo":               pull.Repo,
		"path":               pull.Path,
		"hash_before":        pull.HashBefore,
		"hash_after":         pull.HashAfter,
		"output":             pull.Output,
		"stderr":             pull.Stderr,
		"ok":                 pull.Ok,
		"imported":           imp.Imported,
		"optional_available": imp.OptionalAvailable,
		"unrecognized":       imp.Unrecognized,
	}
	if pull.Error != "" {
		out["error"] = pull.Error
	}
	if len(imp.OptionalImported) > 0 {
		out["optional_imported"] = imp.OptionalImported
	}
	if len(imp.Skipped) > 0 {
		out["skipped"] = imp.Skipped
	}
	return out
}

// gitPullOne records the SHA before and after `git pull --ff-only`.
// On any failure (non-existent dir, dirty tree, non-ff, network) returns
// ok=false with the underlying stderr/error preserved verbatim — the agent
// reads stderr to decide whether to retry, fix conflicts, or give up.
func gitPullOne(ctx context.Context, runner GitRunner, repo, path string) gitRepoResult {
	out := gitRepoResult{Repo: repo, Path: path}
	if path == "" {
		out.Error = "empty path"
		return out
	}
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		out.Error = "not a git repo: " + path + " (" + err.Error() + ")"
		return out
	}
	if so, _, err := runner.RunGit(ctx, path, "rev-parse", "HEAD"); err == nil {
		out.HashBefore = strings.TrimSpace(so)
	}
	stdout, stderr, err := runner.RunGit(ctx, path, "pull", "--ff-only")
	out.Output = trimToBytes(stdout, 4096)
	out.Stderr = trimToBytes(stderr, 4096)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	if so, _, err := runner.RunGit(ctx, path, "rev-parse", "HEAD"); err == nil {
		out.HashAfter = strings.TrimSpace(so)
	}
	out.Ok = true
	return out
}

func trimToBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
