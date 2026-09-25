package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func newImportMockDB(t *testing.T) (deps SqlImportDeps, mock sqlmock.Sqlmock, root string) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	root = t.TempDir()
	return SqlImportDeps{WowRoot: root, BaseRoot: root, ExecDB: db}, mock, root
}

func writeFixture(t *testing.T, root, rel, body string) string {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return full
}

func TestSqlImportFile_HappyPath(t *testing.T) {
	deps, mock, root := newImportMockDB(t)
	writeFixture(t, root, "fixture.sql", "INSERT INTO foo VALUES (1);")

	mock.ExpectExec("USE `acore_world`").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO foo VALUES (1)")).
		WillReturnResult(sqlmock.NewResult(0, 1))

	reg := NewRegistry()
	RegisterSqlImportTools(reg, deps)
	tool, _ := reg.Get("sql_import_file")
	args, _ := json.Marshal(map[string]any{
		"path": "fixture.sql", "database": "acore_world", "confirm": true,
	})
	res := tool.Handler(context.Background(), args, "test").(sqlImportFileResult)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res)
	}
	if res.RowsAffected != 1 {
		t.Errorf("rows_affected = %d, want 1", res.RowsAffected)
	}
	if res.BytesImported != len("INSERT INTO foo VALUES (1);") {
		t.Errorf("bytes_imported = %d", res.BytesImported)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestSqlImportFile_MissingConfirm(t *testing.T) {
	deps, _, _ := newImportMockDB(t)
	reg := NewRegistry()
	RegisterSqlImportTools(reg, deps)
	tool, _ := reg.Get("sql_import_file")
	args, _ := json.Marshal(map[string]any{
		"path": "fixture.sql", "database": "acore_world", "confirm": false,
	})
	m := tool.Handler(context.Background(), args, "test").(map[string]any)
	if !strings.Contains(m["error"].(string), "confirm:true required") {
		t.Errorf("expected confirm error, got %v", m["error"])
	}
}

func TestSqlImportFile_BadDatabase(t *testing.T) {
	deps, _, _ := newImportMockDB(t)
	reg := NewRegistry()
	RegisterSqlImportTools(reg, deps)
	tool, _ := reg.Get("sql_import_file")
	args, _ := json.Marshal(map[string]any{
		"path": "fixture.sql", "database": "mysql", "confirm": true,
	})
	m := tool.Handler(context.Background(), args, "test").(map[string]any)
	if !strings.Contains(m["error"].(string), "database must be one of") {
		t.Errorf("expected database allowlist error, got %v", m["error"])
	}
}

func TestSqlImportFile_PathEscapeRejected(t *testing.T) {
	deps, _, _ := newImportMockDB(t)
	reg := NewRegistry()
	RegisterSqlImportTools(reg, deps)
	tool, _ := reg.Get("sql_import_file")
	args, _ := json.Marshal(map[string]any{
		"path": "../../../etc/passwd", "database": "acore_world", "confirm": true,
	})
	m := tool.Handler(context.Background(), args, "test").(map[string]any)
	if m["error"] == nil || !strings.Contains(m["error"].(string), "escapes root") {
		t.Errorf("expected escape rejection, got %v", m["error"])
	}
}

func TestSqlImportDir_AlphabeticalOrderAndExclude(t *testing.T) {
	deps, mock, root := newImportMockDB(t)
	writeFixture(t, root, "sql/001_keep.sql", "SELECT 1;")
	writeFixture(t, root, "sql/002_skip_exact.sql", "SELECT 2;")
	writeFixture(t, root, "sql/003_skip_glob.sql", "SELECT 3;")
	writeFixture(t, root, "sql/004_keep.sql", "SELECT 4;")

	// Only 001 and 004 should be imported. Each runs USE then the SELECT.
	mock.ExpectExec("USE `acore_characters`").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("SELECT 1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("USE `acore_characters`").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("SELECT 4").WillReturnResult(sqlmock.NewResult(0, 0))

	reg := NewRegistry()
	RegisterSqlImportTools(reg, deps)
	tool, _ := reg.Get("sql_import_dir")
	args, _ := json.Marshal(map[string]any{
		"dir": "sql", "database": "acore_characters", "confirm": true,
		"exclude_files": []string{"002_skip_exact.sql"},
		"exclude_glob":  "*_skip_glob.sql",
	})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)
	if res["imported"] != 2 || res["skipped"] != 2 || res["failed"] != 0 {
		t.Errorf("imported=%v skipped=%v failed=%v", res["imported"], res["skipped"], res["failed"])
	}
	files := res["files"].([]sqlImportFileResult)
	if len(files) != 4 {
		t.Errorf("want 4 file entries, got %d", len(files))
	}
	// Verify alphabetical order and skip reasons.
	wantOrder := []string{"001_keep.sql", "002_skip_exact.sql", "003_skip_glob.sql", "004_keep.sql"}
	for i, f := range files {
		if f.File != wantOrder[i] {
			t.Errorf("file[%d] = %s, want %s", i, f.File, wantOrder[i])
		}
	}
	if !files[1].Skipped || files[1].SkipReason != "in exclude_files" {
		t.Errorf("002 should be skipped via exclude_files, got %+v", files[1])
	}
	if !files[2].Skipped || files[2].SkipReason != "matches exclude_glob" {
		t.Errorf("003 should be skipped via exclude_glob, got %+v", files[2])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestSqlImportDir_HaltsOnError(t *testing.T) {
	deps, mock, root := newImportMockDB(t)
	writeFixture(t, root, "sql/001.sql", "SELECT 1;")
	writeFixture(t, root, "sql/002.sql", "BROKEN SQL;")
	writeFixture(t, root, "sql/003.sql", "SELECT 3;")

	mock.ExpectExec("USE `acore_world`").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("SELECT 1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("USE `acore_world`").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("BROKEN SQL").WillReturnError(sqlError("syntax"))

	reg := NewRegistry()
	RegisterSqlImportTools(reg, deps)
	tool, _ := reg.Get("sql_import_dir")
	args, _ := json.Marshal(map[string]any{
		"dir": "sql", "database": "acore_world", "confirm": true,
		// continue_on_error omitted → defaults to false → halt after 002
	})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)
	if res["halted_on"] != "002.sql" {
		t.Errorf("halted_on = %v, want 002.sql", res["halted_on"])
	}
	if res["imported"] != 1 || res["failed"] != 1 {
		t.Errorf("imported=%v failed=%v", res["imported"], res["failed"])
	}
	// 003 should NOT have been attempted.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestSqlImportDir_ContinueOnError(t *testing.T) {
	deps, mock, root := newImportMockDB(t)
	writeFixture(t, root, "sql/001.sql", "SELECT 1;")
	writeFixture(t, root, "sql/002.sql", "BROKEN;")
	writeFixture(t, root, "sql/003.sql", "SELECT 3;")

	mock.ExpectExec("USE `acore_world`").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("SELECT 1").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("USE `acore_world`").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("BROKEN").WillReturnError(sqlError("syntax"))
	mock.ExpectExec("USE `acore_world`").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("SELECT 3").WillReturnResult(sqlmock.NewResult(0, 0))

	reg := NewRegistry()
	RegisterSqlImportTools(reg, deps)
	tool, _ := reg.Get("sql_import_dir")
	args, _ := json.Marshal(map[string]any{
		"dir": "sql", "database": "acore_world", "confirm": true,
		"continue_on_error": true,
	})
	res := tool.Handler(context.Background(), args, "test").(map[string]any)
	if res["imported"] != 2 || res["failed"] != 1 || res["halted_on"] != nil {
		t.Errorf("imported=%v failed=%v halted_on=%v", res["imported"], res["failed"], res["halted_on"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// sqlError wraps a string for sqlmock's WillReturnError. sqlmock is fine with
// any error implementing error; we just want a recognizable instance.
type stringErr string

func (s stringErr) Error() string { return string(s) }

func sqlError(s string) error { return stringErr(s) }
