package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// fakeGitRunner records the (dir, args) pairs it sees and returns canned
// responses keyed by argv. Lets us assert the exact git invocation a tool
// produced without spawning a real `git` process.
type fakeGitRunner struct {
	calls []struct {
		Dir  string
		Args []string
	}
	stdoutBy map[string]string // first arg ("rev-parse"/"pull") → canned stdout
	stderrBy map[string]string
	errBy    map[string]error
}

func (f *fakeGitRunner) RunGit(_ context.Context, dir string, args ...string) (string, string, error) {
	f.calls = append(f.calls, struct {
		Dir  string
		Args []string
	}{Dir: dir, Args: append([]string(nil), args...)})
	if len(args) == 0 {
		return "", "", nil
	}
	return f.stdoutBy[args[0]], f.stderrBy[args[0]], f.errBy[args[0]]
}

// makeRepoStub creates a minimal directory with a .git child so gitPullOne's
// "not a git repo" check passes — we never actually run git against it.
func makeRepoStub(t *testing.T, name string) string {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return repo
}

func TestGitPullCore_HappyPath(t *testing.T) {
	repo := makeRepoStub(t, "core")
	runner := &fakeGitRunner{
		stdoutBy: map[string]string{"rev-parse": "deadbeef\n", "pull": "Already up to date.\n"},
	}

	reg := NewRegistry()
	RegisterGitTools(reg, GitDeps{WowRoot: repo, Runner: runner})

	tool, ok := reg.Get("git_pull_core")
	if !ok {
		t.Fatalf("git_pull_core not registered")
	}
	res := tool.Handler(context.Background(), json.RawMessage(`{}`), "test").(gitRepoResult)
	if !res.Ok {
		t.Fatalf("expected ok=true, got %+v", res)
	}
	if res.HashBefore != "deadbeef" || res.HashAfter != "deadbeef" {
		t.Errorf("hash before/after = %q/%q, want deadbeef/deadbeef", res.HashBefore, res.HashAfter)
	}
	if !strings.Contains(res.Output, "Already up to date") {
		t.Errorf("output = %q, want 'Already up to date'", res.Output)
	}
	// Exactly 3 git calls expected: rev-parse, pull, rev-parse.
	if len(runner.calls) != 3 {
		t.Errorf("got %d git calls, want 3: %+v", len(runner.calls), runner.calls)
	}
	if runner.calls[1].Args[0] != "pull" || runner.calls[1].Args[1] != "--ff-only" {
		t.Errorf("expected pull --ff-only, got %v", runner.calls[1].Args)
	}
}

func TestGitPullCore_PullFails(t *testing.T) {
	repo := makeRepoStub(t, "core")
	runner := &fakeGitRunner{
		stdoutBy: map[string]string{"rev-parse": "abc123\n"},
		stderrBy: map[string]string{"pull": "fatal: Not possible to fast-forward, aborting.\n"},
		errBy:    map[string]error{"pull": errors.New("exit status 128")},
	}
	reg := NewRegistry()
	RegisterGitTools(reg, GitDeps{WowRoot: repo, Runner: runner})
	tool, _ := reg.Get("git_pull_core")
	res := tool.Handler(context.Background(), json.RawMessage(`{}`), "test").(gitRepoResult)
	if res.Ok {
		t.Errorf("expected ok=false on pull failure, got %+v", res)
	}
	if !strings.Contains(res.Stderr, "Not possible to fast-forward") {
		t.Errorf("stderr not propagated: %q", res.Stderr)
	}
	if res.Error == "" {
		t.Errorf("expected error string, got empty")
	}
}

func TestGitPullCore_NotARepo(t *testing.T) {
	root := t.TempDir() // no .git child
	runner := &fakeGitRunner{}
	reg := NewRegistry()
	RegisterGitTools(reg, GitDeps{WowRoot: root, Runner: runner})
	tool, _ := reg.Get("git_pull_core")
	res := tool.Handler(context.Background(), json.RawMessage(`{}`), "test").(gitRepoResult)
	if res.Ok {
		t.Fatalf("expected ok=false for non-repo")
	}
	if !strings.Contains(res.Error, "not a git repo") {
		t.Errorf("error = %q, want 'not a git repo'", res.Error)
	}
	if len(runner.calls) != 0 {
		t.Errorf("expected zero git calls when path is not a repo, got %d", len(runner.calls))
	}
}

func TestGitPullModule_NameValidation(t *testing.T) {
	root := t.TempDir()
	runner := &fakeGitRunner{}
	reg := NewRegistry()
	RegisterGitTools(reg, GitDeps{WowRoot: root, Runner: runner})
	tool, _ := reg.Get("git_pull_module")

	cases := []struct {
		name   string
		reject bool
	}{
		{"mod-ollama-chat", false},
		{"mod-playerbots", false},
		{"mod-ah-bot", false},
		{"../etc/passwd", true},
		{"mod-../escape", true},
		{"MOD-Capitalized", true},
		{"core", true},
		{"", true},
	}
	for _, tc := range cases {
		args, _ := json.Marshal(map[string]string{"name": tc.name})
		res := tool.Handler(context.Background(), args, "test")
		m, ok := res.(map[string]any)
		if tc.reject {
			if !ok || m["error"] == nil {
				if r, isResult := res.(gitRepoResult); isResult && r.Error == "" {
					t.Errorf("name=%q: expected rejection, got success: %+v", tc.name, r)
				}
			}
		} else {
			// Non-reject: handler reaches gitPullOne, which fails because the
			// stub dir doesn't exist — but we expect a gitRepoResult, not an
			// args-validation error map.
			if _, isResult := res.(gitRepoResult); !isResult {
				t.Errorf("name=%q: expected gitRepoResult, got %T (%v)", tc.name, res, res)
			}
		}
	}
}

func TestGitPullAll_HappyPath(t *testing.T) {
	root := t.TempDir()
	// Stub out core .git
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Stub out two modules and one non-matching dir.
	for _, name := range []string{"mod-ollama-chat", "mod-playerbots", "not-a-module"} {
		if err := os.MkdirAll(filepath.Join(root, "modules", name, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeGitRunner{
		stdoutBy: map[string]string{"rev-parse": "abc\n", "pull": "Already up to date.\n"},
	}
	reg := NewRegistry()
	RegisterGitTools(reg, GitDeps{WowRoot: root, Runner: runner})
	tool, _ := reg.Get("git_pull_all")
	res := tool.Handler(context.Background(), json.RawMessage(`{}`), "test").(map[string]any)

	repos := res["repos"].([]gitRepoResult)
	if len(repos) != 3 {
		t.Fatalf("expected 3 repos (core + 2 mods), got %d: %+v", len(repos), repos)
	}
	if res["ok_count"] != 3 || res["error_count"] != 0 {
		t.Errorf("ok_count=%v error_count=%v", res["ok_count"], res["error_count"])
	}
	// Verify the non-matching dir was skipped.
	for _, r := range repos {
		if strings.Contains(r.Repo, "not-a-module") {
			t.Errorf("non-matching dir 'not-a-module' should have been skipped, found: %+v", r)
		}
	}
}

// TestGitPullModule_ImportSQL_StandardAndOptional pins the post-2026-05-05
// extension: when import_sql:true is set, the handler walks the pulled
// module's data/sql/ tree and auto-imports the standard buckets via ops_rw
// while listing the optional files for explicit opt-in.
//
// Fixture mimics mod-individual-progression's shape: one standard
// data/sql/world/base/*.sql + two optional/sql/world/zz_optional_*.sql files.
// Standard file goes through importOneFile (USE acore_world; ExecContext);
// optional files surface in optional_available without being imported.
func TestGitPullModule_ImportSQL_StandardAndOptional(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "modules", "mod-individual-progression")
	if err := os.MkdirAll(filepath.Join(module, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkDirFS(t, module, "data/sql/world/base/zone_redridge.sql")
	mkDirFS(t, module, "optional/sql/world/zz_optional_phasing.sql")
	mkDirFS(t, module, "optional/sql/world/zz_optional_aq_quest_nerf.sql")

	runner := &fakeGitRunner{
		stdoutBy: map[string]string{"rev-parse": "abc\n", "pull": "Already up to date.\n"},
	}
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_world`")).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("-- test fixture")).WillReturnResult(sqlmock.NewResult(0, 0))

	reg := NewRegistry()
	RegisterGitTools(reg, GitDeps{WowRoot: root, Runner: runner, ExecDB: db})
	tool, _ := reg.Get("git_pull_module")

	args, _ := json.Marshal(map[string]any{
		"name":       "mod-individual-progression",
		"import_sql": true,
	})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)

	if res["ok"] != true {
		t.Fatalf("expected pull ok=true, got %+v", res)
	}
	imported, ok := res["imported"].([]moduleImportedDir)
	if !ok || len(imported) != 1 {
		t.Fatalf("expected 1 imported bucket, got %T %+v", res["imported"], res["imported"])
	}
	if imported[0].Database != "acore_world" {
		t.Errorf("imported database = %q, want acore_world", imported[0].Database)
	}
	optAvail, ok := res["optional_available"].([]ModuleSQLFile)
	if !ok || len(optAvail) != 2 {
		t.Fatalf("expected 2 optional files, got %T %+v", res["optional_available"], res["optional_available"])
	}
}

// TestGitPullModule_ImportSQL_OptIntoOneOptional pins the include-list
// behavior: passing optional_includes:["zz_optional_phasing.sql"] imports
// that file while still skipping the other optionals.
func TestGitPullModule_ImportSQL_OptIntoOneOptional(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "modules", "mod-individual-progression")
	if err := os.MkdirAll(filepath.Join(module, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkDirFS(t, module, "optional/sql/world/zz_optional_phasing.sql")
	mkDirFS(t, module, "optional/sql/world/zz_optional_aq_quest_nerf.sql")

	runner := &fakeGitRunner{
		stdoutBy: map[string]string{"rev-parse": "abc\n", "pull": "Already up to date.\n"},
	}
	db, mock, _ := sqlmock.New()
	defer db.Close()
	// Only ONE import expected — the file the caller opted in to.
	mock.ExpectExec(regexp.QuoteMeta("USE `acore_world`")).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("-- test fixture")).WillReturnResult(sqlmock.NewResult(0, 0))

	reg := NewRegistry()
	RegisterGitTools(reg, GitDeps{WowRoot: root, Runner: runner, ExecDB: db})
	tool, _ := reg.Get("git_pull_module")
	args, _ := json.Marshal(map[string]any{
		"name":              "mod-individual-progression",
		"import_sql":        true,
		"optional_includes": []string{"zz_optional_phasing.sql"},
	})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)

	imported, _ := res["optional_imported"].([]moduleImportedFile)
	if len(imported) != 1 {
		t.Fatalf("optional_imported len: got %d, want 1", len(imported))
	}
	if !strings.HasSuffix(imported[0].RelPath, "zz_optional_phasing.sql") {
		t.Errorf("imported wrong file: %s", imported[0].RelPath)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock (the second optional file should NOT have been imported): %v", err)
	}
}

// TestGitPullModule_ImportSQL_PlayerbotsUnrecognized covers mod-playerbots'
// quirk: data/sql/playerbots/ targets an unknown DB. The handler must list
// it under unrecognized rather than blindly trying to import.
func TestGitPullModule_ImportSQL_PlayerbotsUnrecognized(t *testing.T) {
	root := t.TempDir()
	module := filepath.Join(root, "modules", "mod-playerbots")
	if err := os.MkdirAll(filepath.Join(module, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkDirFS(t, module, "data/sql/playerbots/base/init.sql")

	runner := &fakeGitRunner{
		stdoutBy: map[string]string{"rev-parse": "abc\n", "pull": "Already up to date.\n"},
	}
	db, _, _ := sqlmock.New()
	defer db.Close()
	// No exec expectations — unrecognized = no SQL is run.

	reg := NewRegistry()
	RegisterGitTools(reg, GitDeps{WowRoot: root, Runner: runner, ExecDB: db})
	tool, _ := reg.Get("git_pull_module")
	args, _ := json.Marshal(map[string]any{
		"name":       "mod-playerbots",
		"import_sql": true,
	})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)

	unrec, _ := res["unrecognized"].([]ModuleSQLEntry)
	if len(unrec) != 1 {
		t.Fatalf("unrecognized: got %d, want 1\n%+v", len(unrec), res)
	}
	if !strings.Contains(unrec[0].Reason, "playerbots") {
		t.Errorf("reason should name the unknown DB: %q", unrec[0].Reason)
	}
}

// TestBuildGitArgs_RewritesGitHubSSHToHTTPS verifies the ops-api injects the
// SSH->HTTPS insteadOf rewrites (the image has no ssh client) and that they
// precede the subcommand as git global options. Regression guard for the
// mod-ah-bot / mod-ollama-chat "cannot run ssh: No such file or directory"
// pull failures.
func TestBuildGitArgs_RewritesGitHubSSHToHTTPS(t *testing.T) {
	got := buildGitArgs("/wow-root/modules/mod-ah-bot", []string{"pull", "--ff-only"})
	joined := strings.Join(got, " ")

	wants := []string{
		"-C /wow-root/modules/mod-ah-bot",
		"url.https://github.com/.insteadOf=git@github.com:",
		"url.https://github.com/.insteadOf=ssh://git@github.com/",
		"pull --ff-only",
	}
	for _, w := range wants {
		if !strings.Contains(joined, w) {
			t.Errorf("buildGitArgs missing %q in %q", w, joined)
		}
	}

	// The -c rewrites are git GLOBAL options: they MUST come before the
	// subcommand, else git treats them as subcommand args and ignores them.
	idx := func(target string) int {
		for i, a := range got {
			if a == target {
				return i
			}
		}
		return -1
	}
	cfgIdx := idx("url.https://github.com/.insteadOf=git@github.com:")
	pullIdx := idx("pull")
	if cfgIdx == -1 || pullIdx == -1 {
		t.Fatalf("expected both insteadOf flag and pull subcommand in %v", got)
	}
	if cfgIdx > pullIdx {
		t.Errorf("insteadOf -c (idx %d) must precede subcommand (idx %d): %v", cfgIdx, pullIdx, got)
	}

	// -c flags carry their values as the immediately following argv element.
	if got[cfgIdx-1] != "-c" {
		t.Errorf("insteadOf value at %d not preceded by -c: %v", cfgIdx, got)
	}
}
