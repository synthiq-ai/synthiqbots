package mcpserver

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
)

func mustNewGzWriter(t *testing.T, w io.Writer) *gzip.Writer {
	t.Helper()
	return gzip.NewWriter(w)
}
func mustNewTarWriter(t *testing.T, w io.Writer) *tar.Writer {
	t.Helper()
	return tar.NewWriter(w)
}
func makeSymlinkHeader(t *testing.T, name, target string) *tar.Header {
	t.Helper()
	return &tar.Header{Name: name, Linkname: target, Typeflag: tar.TypeSymlink, Mode: 0o777}
}
func makeRegularHeader(t *testing.T, name string, body []byte) *tar.Header {
	t.Helper()
	return &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}
}

// fakeDocker captures Exec/Inspect/Stop/Start calls for assertion. Returns
// canned ExecResult by command-name (first argv element).
type fakeDocker struct {
	execCalls    []struct{ Container string; Cmd []string; HasStdin bool }
	stopCalls    []string
	startCalls   []string
	inspectCalls []string

	execResultBy map[string]*dockerlog.ExecResult // keyed by argv[0]
	execErrBy    map[string]error
	inspectBy    map[string]*dockerlog.Inspect
}

func (f *fakeDocker) Exec(_ context.Context, container string, opts dockerlog.ExecOpts) (*dockerlog.ExecResult, error) {
	f.execCalls = append(f.execCalls, struct{ Container string; Cmd []string; HasStdin bool }{
		Container: container, Cmd: append([]string(nil), opts.Cmd...), HasStdin: opts.Stdin != nil,
	})
	if len(opts.Cmd) == 0 {
		return nil, errors.New("empty cmd")
	}
	if err, ok := f.execErrBy[opts.Cmd[0]]; ok {
		return nil, err
	}
	if res, ok := f.execResultBy[opts.Cmd[0]]; ok {
		// If a Stdout writer was provided, write the canned stdout into it
		// and zero out the buffer field — matches production semantics.
		if opts.Stdout != nil && len(res.Stdout) > 0 {
			_, _ = opts.Stdout.Write(res.Stdout)
			cp := *res
			cp.Stdout = nil
			return &cp, nil
		}
		return res, nil
	}
	return &dockerlog.ExecResult{ExitCode: 0}, nil
}
func (f *fakeDocker) Inspect(_ context.Context, container string) (*dockerlog.Inspect, error) {
	f.inspectCalls = append(f.inspectCalls, container)
	if v, ok := f.inspectBy[container]; ok {
		return v, nil
	}
	return &dockerlog.Inspect{}, nil
}
func (f *fakeDocker) Stop(_ context.Context, container string, _ int) error {
	f.stopCalls = append(f.stopCalls, container)
	return nil
}
func (f *fakeDocker) Start(_ context.Context, container string) error {
	f.startCalls = append(f.startCalls, container)
	return nil
}

// TestWowCreateAccount_HappyPath verifies the SQL path: SRP6 verifier is
// computed in-process and INSERTed into account via ops_rw. Tied to the
// post-2026-05-05 fix that swapped the broken `worldserver-cli` exec for
// direct SQL.
func TestWowCreateAccount_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	// Match the INSERT IGNORE shape; sqlmock matches by regex substring of the
	// QuoteMeta'd query — pin enough of it to catch a column-list refactor.
	mock.ExpectExec(regexp.QuoteMeta("INSERT IGNORE INTO `account` (username, salt, verifier, email, reg_mail, joindate)")).
		WithArgs("TESTUSER", sqlmock.AnyArg(), sqlmock.AnyArg(), "", "").
		WillReturnResult(sqlmock.NewResult(2005, 1))

	reg := NewRegistry()
	RegisterWowTools(reg, WowDeps{Docker: &fakeDocker{}, AuthDB: db})
	tool, _ := reg.Get("wow_create_account")
	args, _ := json.Marshal(map[string]any{"username": "testuser", "password": "secretpw", "confirm": true})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)
	if res["ok"] != true {
		t.Fatalf("expected ok=true, got %+v", res)
	}
	if res["username"] != "TESTUSER" {
		t.Errorf("username should be uppercased: %v", res["username"])
	}
	if res["account_id"].(int64) != 2005 {
		t.Errorf("account_id=%v, want 2005", res["account_id"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock: %v", err)
	}
}

// TestWowCreateAccount_DuplicateUsername confirms INSERT IGNORE's
// rowsAffected=0 path surfaces a clean "account already exists" error rather
// than masquerading as success.
func TestWowCreateAccount_DuplicateUsername(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectExec(regexp.QuoteMeta("INSERT IGNORE INTO `account`")).
		WillReturnResult(sqlmock.NewResult(0, 0))

	reg := NewRegistry()
	RegisterWowTools(reg, WowDeps{Docker: &fakeDocker{}, AuthDB: db})
	tool, _ := reg.Get("wow_create_account")
	args, _ := json.Marshal(map[string]any{"username": "dupuser", "password": "secretpw", "confirm": true})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)
	if msg, _ := res["error"].(string); !strings.Contains(msg, "already exists") {
		t.Errorf("expected duplicate error, got %+v", res)
	}
}

func TestWowCreateAccount_BadInputs(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer db.Close()
	reg := NewRegistry()
	RegisterWowTools(reg, WowDeps{Docker: &fakeDocker{}, AuthDB: db})
	tool, _ := reg.Get("wow_create_account")

	cases := []struct {
		name        string
		args        map[string]any
		wantErrSubs string
	}{
		{"missing confirm", map[string]any{"username": "u123", "password": "p123"}, "confirm:true required"},
		{"too short user", map[string]any{"username": "u", "password": "p123", "confirm": true}, "must be 4–16"},
		{"semicolon in user", map[string]any{"username": "user;rm -rf /", "password": "p123", "confirm": true}, "must be 4–16"},
	}
	for _, tc := range cases {
		args, _ := json.Marshal(tc.args)
		res := tool.Handler(context.Background(), args, "test").(map[string]any)
		if !strings.Contains(res["error"].(string), tc.wantErrSubs) {
			t.Errorf("%s: error=%q, want substring %q", tc.name, res["error"], tc.wantErrSubs)
		}
	}
}

// TestWowCreateAccount_NoAuthDB pins the no-DSN failure mode — the original
// bug pre-2026-05-05 was wow-ops-api silently accepting an empty AuthDB and
// then crashing in the handler. This locks in the user-facing error.
func TestWowCreateAccount_NoAuthDB(t *testing.T) {
	reg := NewRegistry()
	RegisterWowTools(reg, WowDeps{Docker: &fakeDocker{}, AuthDB: nil})
	tool, _ := reg.Get("wow_create_account")
	args, _ := json.Marshal(map[string]any{"username": "u123", "password": "p123", "confirm": true})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)
	if msg, _ := res["error"].(string); !strings.Contains(msg, "OPS_DB_AUTH_DSN") {
		t.Errorf("expected OPS_DB_AUTH_DSN error, got %+v", res)
	}
}

// TestWowSetGmLevel_HappyPath_Upsert pins the REPLACE INTO path for level >= 1.
// The PK is (id, RealmID); REPLACE INTO covers both first-insert and
// already-promoted-on-this-realm cases.
func TestWowSetGmLevel_HappyPath_Upsert(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM `account` WHERE username = ?")).
		WithArgs("TESTUSER").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1234))
	mock.ExpectExec(regexp.QuoteMeta("REPLACE INTO `account_access`")).
		WithArgs(int64(1234), 3, -1).
		WillReturnResult(sqlmock.NewResult(0, 1))
	reg := NewRegistry()
	RegisterWowTools(reg, WowDeps{Docker: &fakeDocker{}, AuthDB: db})
	tool, _ := reg.Get("wow_set_gm_level")
	args, _ := json.Marshal(map[string]any{"username": "testuser", "level": 3, "confirm": true})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)
	if res["ok"] != true {
		t.Fatalf("expected ok=true, got %+v", res)
	}
	if res["realm_id"] != -1 {
		t.Errorf("realm_id should default to -1: %v", res["realm_id"])
	}
}

// TestWowSetGmLevel_LevelZero_Deletes pins the level=0 → DELETE branch
// (mirrors AzerothCore CLI semantics — a regular player has no row, not a
// gmlevel=0 row).
func TestWowSetGmLevel_LevelZero_Deletes(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM `account` WHERE username = ?")).
		WithArgs("TESTUSER").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1234))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM `account_access`")).
		WithArgs(int64(1234), -1).
		WillReturnResult(sqlmock.NewResult(0, 1))
	reg := NewRegistry()
	RegisterWowTools(reg, WowDeps{Docker: &fakeDocker{}, AuthDB: db})
	tool, _ := reg.Get("wow_set_gm_level")
	args, _ := json.Marshal(map[string]any{"username": "testuser", "level": 0, "confirm": true})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)
	if res["ok"] != true {
		t.Fatalf("expected ok=true, got %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock: %v", err)
	}
}

// TestWowSetGmLevel_AccountNotFound surfaces a clean error when the SELECT
// returns sql.ErrNoRows — caller mistyped the username, etc.
func TestWowSetGmLevel_AccountNotFound(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM `account` WHERE username = ?")).
		WithArgs("NOSUCH").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	reg := NewRegistry()
	RegisterWowTools(reg, WowDeps{Docker: &fakeDocker{}, AuthDB: db})
	tool, _ := reg.Get("wow_set_gm_level")
	args, _ := json.Marshal(map[string]any{"username": "nosuch", "level": 4, "confirm": true})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)
	if msg, _ := res["error"].(string); !strings.Contains(msg, "account not found") {
		t.Errorf("expected not-found, got %+v", res)
	}
}

func TestWowCheckRealmlist(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	mock.ExpectQuery("SELECT id, name, address, port, icon, timezone, allowedSecurityLevel FROM realmlist").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "address", "port", "icon", "timezone", "allowedSecurityLevel"}).
			AddRow(1, "Slayobot", "192.168.100.11", 8085, 0, 1, 0))
	docker := &fakeDocker{}
	reg := NewRegistry()
	RegisterWowTools(reg, WowDeps{Docker: docker, AuthDB: db})
	tool, _ := reg.Get("wow_check_realmlist")
	res := tool.Handler(context.Background(), json.RawMessage(`{}`), "test").(map[string]any)
	if res["count"] != 1 {
		t.Errorf("count=%v", res["count"])
	}
	realms := res["realms"].([]map[string]any)
	if realms[0]["address"] != "192.168.100.11" {
		t.Errorf("address=%v", realms[0]["address"])
	}
}

func TestWowUpdateRealmIP_Validation(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	docker := &fakeDocker{}
	reg := NewRegistry()
	RegisterWowTools(reg, WowDeps{Docker: docker, AuthDB: db})
	tool, _ := reg.Get("wow_update_realm_ip")

	// Reject SQL injection / invalid characters.
	bad := []string{"198.51.100.4'; DROP TABLE realmlist; --", "192.168.100.17 OR 1=1", "spaces in name"}
	for _, addr := range bad {
		args, _ := json.Marshal(map[string]any{"address": addr, "realm_id": 1, "confirm": true})
		res := tool.Handler(context.Background(), args, "test").(map[string]any)
		if res["error"] == nil {
			t.Errorf("expected reject for address=%q", addr)
		}
	}

	// Happy path.
	mock.ExpectExec("UPDATE realmlist SET address=\\? WHERE id=\\?").
		WithArgs("192.168.100.11", 1).
		WillReturnResult(sqlmock.NewResult(0, 1))
	args, _ := json.Marshal(map[string]any{"address": "192.168.100.11", "realm_id": 1, "confirm": true})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)
	if res["ok"] != true {
		t.Errorf("happy path res=%+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestWowListBackups(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "wow-acore_world-20260101-000000.sql.gz"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "wow-dir-20260102-000000.tar.gz"), []byte("yy"), 0o644)
	os.WriteFile(filepath.Join(dir, "ignore-me.txt"), []byte("zzz"), 0o644)
	docker := &fakeDocker{}
	reg := NewRegistry()
	RegisterWowTools(reg, WowDeps{Docker: docker, BackupDir: dir})
	tool, _ := reg.Get("wow_list_backups")
	res := tool.Handler(context.Background(), json.RawMessage(`{}`), "test").(map[string]any)
	if res["count"] != 2 {
		t.Errorf("count=%v, want 2", res["count"])
	}
}

func TestWowPruneOldBackups_DryRun(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "wow-acore_world-20100101-000000.sql.gz")
	os.WriteFile(old, []byte("old"), 0o644)
	// Backdate it.
	oldTime := mustParseTime(t, "2010-01-01T00:00:00Z")
	os.Chtimes(old, oldTime, oldTime)
	new := filepath.Join(dir, "wow-acore_world-99990101-000000.sql.gz")
	os.WriteFile(new, []byte("new"), 0o644)
	docker := &fakeDocker{}
	reg := NewRegistry()
	RegisterWowTools(reg, WowDeps{Docker: docker, BackupDir: dir})
	tool, _ := reg.Get("wow_prune_old_backups")

	// dry_run preview, no confirm needed.
	args, _ := json.Marshal(map[string]any{"keep_days": 7, "dry_run": true})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)
	if res["count"] != 1 {
		t.Errorf("dry_run count=%v, want 1", res["count"])
	}
	// File should still be on disk.
	if _, err := os.Stat(old); err != nil {
		t.Errorf("old file removed during dry_run")
	}

	// Real run requires confirm.
	args, _ = json.Marshal(map[string]any{"keep_days": 7, "dry_run": false})
	res = tool.Handler(context.Background(), args, "test").(map[string]any)
	if res["error"] == nil {
		t.Errorf("expected confirm requirement when dry_run=false")
	}

	args, _ = json.Marshal(map[string]any{"keep_days": 7, "dry_run": false, "confirm": true})
	res = tool.Handler(context.Background(), args, "test").(map[string]any)
	if res["count"] != 1 {
		t.Errorf("real prune count=%v", res["count"])
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old file should be gone, stat err=%v", err)
	}
	if _, err := os.Stat(new); err != nil {
		t.Errorf("new file should still exist: %v", err)
	}
}

func TestWowPruneOldBackups_Zero_DeletesAll(t *testing.T) {
	dir := t.TempDir()
	files := []struct {
		name string
		when time.Time
	}{
		{"wow-acore_world-20100101-000000.sql.gz", mustParseTime(t, "2010-01-01T00:00:00Z")},
		{"wow-acore_world-20260101-000000.sql.gz", time.Now().Add(-24 * time.Hour)},
		{"wow-dir-99990101-000000.tar.gz", time.Now()},
	}
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		os.WriteFile(p, []byte("x"), 0o644)
		os.Chtimes(p, f.when, f.when)
	}
	docker := &fakeDocker{}
	reg := NewRegistry()
	RegisterWowTools(reg, WowDeps{Docker: docker, BackupDir: dir})
	tool, _ := reg.Get("wow_prune_old_backups")

	args, _ := json.Marshal(map[string]any{"keep_days": 0, "confirm": true})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)
	if res["count"] != 3 {
		t.Errorf("keep_days=0 count=%v, want 3 (delete all)", res["count"])
	}
	if res["keep_days"] != 0 {
		t.Errorf("keep_days echoed=%v, want 0", res["keep_days"])
	}
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(dir, f.name)); !os.IsNotExist(err) {
			t.Errorf("file %s should be gone, err=%v", f.name, err)
		}
	}
}

func TestWowPruneOldBackups_Zero_RequiresConfirm(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "wow-dir-20260101-000000.tar.gz"), []byte("x"), 0o644)
	docker := &fakeDocker{}
	reg := NewRegistry()
	RegisterWowTools(reg, WowDeps{Docker: docker, BackupDir: dir})
	tool, _ := reg.Get("wow_prune_old_backups")

	args, _ := json.Marshal(map[string]any{"keep_days": 0})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)
	if res["error"] == nil {
		t.Errorf("expected confirm requirement on keep_days=0 + no dry_run + no confirm")
	}
}

func TestWowPruneOldBackups_Negative_Rejected(t *testing.T) {
	dir := t.TempDir()
	docker := &fakeDocker{}
	reg := NewRegistry()
	RegisterWowTools(reg, WowDeps{Docker: docker, BackupDir: dir})
	tool, _ := reg.Get("wow_prune_old_backups")

	args, _ := json.Marshal(map[string]any{"keep_days": -1, "confirm": true})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)
	if res["error"] == nil {
		t.Errorf("expected error for keep_days=-1")
	}
}

func TestWowBackupDir_TarRoundTrip(t *testing.T) {
	src := t.TempDir()
	bk := t.TempDir()
	// Populate src with a .git that should be excluded + a real file.
	os.MkdirAll(filepath.Join(src, ".git"), 0o755)
	os.WriteFile(filepath.Join(src, ".git", "config"), []byte("bad"), 0o644)
	os.WriteFile(filepath.Join(src, "README.md"), []byte("hello"), 0o644)
	os.MkdirAll(filepath.Join(src, "modules", "mod-foo"), 0o755)
	os.WriteFile(filepath.Join(src, "modules", "mod-foo", "x.txt"), []byte("y"), 0o644)

	docker := &fakeDocker{}
	reg := NewRegistry()
	RegisterWowTools(reg, WowDeps{Docker: docker, WowRoot: src, BackupDir: bk})
	tool, _ := reg.Get("wow_backup_dir")
	args, _ := json.Marshal(map[string]any{"label": "test", "confirm": true})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)
	if res["ok"] != true {
		t.Fatalf("backup failed: %+v", res)
	}
	// Restore to a fresh dir and confirm contents.
	dst := t.TempDir()
	abs := res["file"].(string)
	if _, err := untarGz(context.Background(), abs, dst); err != nil {
		t.Fatalf("untar: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "README.md")); err != nil {
		t.Errorf("README.md missing post-restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); err == nil {
		t.Errorf(".git should have been excluded but is present")
	}
}

func TestUntarGz_RejectsMaliciousArchives(t *testing.T) {
	// Build an in-memory tar.gz with a symlink that escapes destDir.
	// untarGz must refuse outright.
	dir := t.TempDir()
	bk := filepath.Join(dir, "evil.tar.gz")
	f, err := os.Create(bk)
	if err != nil {
		t.Fatal(err)
	}
	gz := mustNewGzWriter(t, f)
	tw := mustNewTarWriter(t, gz)
	if err := tw.WriteHeader(makeSymlinkHeader(t, "escape", "../../etc")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()

	dest := t.TempDir()
	_, err = untarGz(context.Background(), bk, dest)
	if err == nil || !strings.Contains(err.Error(), "symlink/hardlink") {
		t.Errorf("expected symlink/hardlink rejection, got err=%v", err)
	}

	// Build a benign tar (just a file under subdir/file.bin), but pre-plant
	// a directory symlink in the destination — assertUnderDest must catch
	// the parent-symlink traversal before OpenFile follows it.
	bk3 := filepath.Join(dir, "benign.tar.gz")
	f3, _ := os.Create(bk3)
	gz3 := mustNewGzWriter(t, f3)
	tw3 := mustNewTarWriter(t, gz3)
	hdrDir := &tar.Header{Name: "subdir", Typeflag: tar.TypeDir, Mode: 0o755}
	tw3.WriteHeader(hdrDir)
	hdrFile := makeRegularHeader(t, "subdir/file.bin", []byte("hi"))
	tw3.WriteHeader(hdrFile)
	tw3.Write([]byte("hi"))
	tw3.Close()
	gz3.Close()
	f3.Close()

	dest3 := t.TempDir()
	escapeTarget := t.TempDir()
	// Plant: dest3/subdir → escapeTarget. MkdirAll on dest3/subdir would
	// otherwise just succeed (symlink → existing dir), then OpenFile on
	// dest3/subdir/file.bin would write inside escapeTarget.
	if err := os.Symlink(escapeTarget, filepath.Join(dest3, "subdir")); err != nil {
		t.Fatal(err)
	}
	_, err = untarGz(context.Background(), bk3, dest3)
	if err == nil || !strings.Contains(err.Error(), "escapes dest") {
		t.Errorf("expected parent-symlink rejection, got err=%v", err)
	}
	// Confirm the file was NOT written into the escape target.
	if _, statErr := os.Stat(filepath.Join(escapeTarget, "file.bin")); statErr == nil {
		t.Errorf("file.bin was written into the symlink-escape target — defence missed")
	}

	// Build a tar with a regular entry whose name traverses (../etc/passwd).
	bk2 := filepath.Join(dir, "evil2.tar.gz")
	f2, _ := os.Create(bk2)
	gz2 := mustNewGzWriter(t, f2)
	tw2 := mustNewTarWriter(t, gz2)
	hdr := makeRegularHeader(t, "../../etc/passwd", []byte("rooty"))
	tw2.WriteHeader(hdr)
	tw2.Write([]byte("rooty"))
	tw2.Close()
	gz2.Close()
	f2.Close()
	_, err = untarGz(context.Background(), bk2, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "escapes dest") {
		t.Errorf("expected path-traversal rejection, got err=%v", err)
	}
}

func TestWowRestoreVolumeDB_StopsAndStartsContainer(t *testing.T) {
	bk := t.TempDir()
	mount := t.TempDir()
	// Create a dummy backup archive.
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "data.bin"), []byte("payload"), 0o644)
	bkFile := filepath.Join(bk, "wow-volume-db-20260101-000000.tar.gz")
	if _, err := tarGz(context.Background(), src, bkFile, nil); err != nil {
		t.Fatal(err)
	}
	// Pre-populate mount with garbage to confirm wipe.
	os.WriteFile(filepath.Join(mount, "stale.dat"), []byte("zzz"), 0o644)

	docker := &fakeDocker{
		inspectBy: map[string]*dockerlog.Inspect{
			"ac-database": {State: struct {
				Status     string `json:"Status"`
				Running    bool   `json:"Running"`
				Restarting bool   `json:"Restarting"`
				StartedAt  string `json:"StartedAt"`
				ExitCode   int    `json:"ExitCode"`
			}{Running: true, Status: "running"}},
		},
	}
	reg := NewRegistry()
	RegisterWowTools(reg, WowDeps{
		Docker:       docker,
		BackupDir:    bk,
		DBContainer:  "ac-database",
		VolumeMounts: map[string]string{"db": mount},
	})
	tool, _ := reg.Get("wow_restore_volume_db")
	args, _ := json.Marshal(map[string]any{
		"backup_file": "wow-volume-db-20260101-000000.tar.gz", "confirm": true,
	})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)
	if res["ok"] != true {
		t.Fatalf("restore failed: %+v", res)
	}
	if len(docker.stopCalls) != 1 || docker.stopCalls[0] != "ac-database" {
		t.Errorf("expected stop ac-database, got %v", docker.stopCalls)
	}
	if len(docker.startCalls) != 1 || docker.startCalls[0] != "ac-database" {
		t.Errorf("expected start ac-database, got %v", docker.startCalls)
	}
	// Stale file should be gone, restored file should be present.
	if _, err := os.Stat(filepath.Join(mount, "stale.dat")); !os.IsNotExist(err) {
		t.Errorf("stale.dat should have been wiped, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(mount, "data.bin")); err != nil {
		t.Errorf("data.bin missing from restored volume: %v", err)
	}
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return parsed
}
