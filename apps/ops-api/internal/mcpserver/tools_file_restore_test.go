package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureRestoreDeps mirrors fixtureDeps in tools_file_diff_test.go but for
// FileRestoreDeps. Layouts a temp config root + backup dir with the given
// file + backups.
func fixtureRestoreDeps(t *testing.T, file, current string, backups map[int64]string) FileRestoreDeps {
	t.Helper()
	root := t.TempDir()
	configRoot := filepath.Join(root, "configs")
	backupDir := filepath.Join(root, "backups")
	if err := os.MkdirAll(configRoot, 0o755); err != nil {
		t.Fatalf("mkdir configRoot: %v", err)
	}
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatalf("mkdir backupDir: %v", err)
	}
	if current != "" {
		if err := os.WriteFile(filepath.Join(configRoot, file), []byte(current), 0o644); err != nil {
			t.Fatalf("write current: %v", err)
		}
	}
	for ts, content := range backups {
		name := fileBackupName(file, ts)
		if err := os.WriteFile(filepath.Join(backupDir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write backup ts=%d: %v", ts, err)
		}
	}
	return FileRestoreDeps{
		ConfigRoot:   configRoot,
		BackupDir:    backupDir,
		AllowedFiles: []string{file},
	}
}

func callRestore(t *testing.T, deps FileRestoreDeps, args map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res := handleFileRestore(deps, raw)
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T: %v", res, res)
	}
	return m
}

func TestFileRestore_RequiresConfirm(t *testing.T) {
	deps := fixtureRestoreDeps(t, "ok.conf", "current\n", map[int64]string{100: "old\n"})
	res := callRestore(t, deps, map[string]any{"file": "ok.conf"})
	if errStr, _ := res["error"].(string); !strings.Contains(errStr, "confirm:true required") {
		t.Errorf("expected confirm-required error, got: %v", res["error"])
	}
}

func TestFileRestore_AllowlistRejection(t *testing.T) {
	deps := fixtureRestoreDeps(t, "ok.conf", "x\n", map[int64]string{1: "y\n"})
	res := callRestore(t, deps, map[string]any{"file": "evil.conf", "confirm": true})
	if errStr, _ := res["error"].(string); !strings.Contains(errStr, "OPS_ADMIN_ALLOWED_FILES") {
		t.Errorf("expected allowlist error, got: %v", res["error"])
	}
}

func TestFileRestore_UnsafeBasename(t *testing.T) {
	deps := fixtureRestoreDeps(t, "ok.conf", "x", nil)
	for _, bad := range []string{"../etc/passwd", "a/b", ".."} {
		res := callRestore(t, deps, map[string]any{"file": bad, "confirm": true})
		if errStr, _ := res["error"].(string); !strings.Contains(errStr, "basename") &&
			!strings.Contains(errStr, "OPS_ADMIN_ALLOWED_FILES") {
			t.Errorf("expected basename or allowlist rejection for %q, got: %v", bad, res["error"])
		}
	}
}

func TestFileRestore_NoBackups(t *testing.T) {
	deps := fixtureRestoreDeps(t, "ok.conf", "x\n", nil)
	res := callRestore(t, deps, map[string]any{"file": "ok.conf", "confirm": true})
	if errStr, _ := res["error"].(string); !strings.Contains(errStr, "no backups") {
		t.Errorf("expected no-backups error, got: %v", res["error"])
	}
}

func TestFileRestore_BackupTsNotFound(t *testing.T) {
	deps := fixtureRestoreDeps(t, "ok.conf", "x\n", map[int64]string{100: "y\n"})
	res := callRestore(t, deps, map[string]any{
		"file": "ok.conf", "backupTs": 999, "confirm": true,
	})
	if errStr, _ := res["error"].(string); !strings.Contains(errStr, "no backup with ts=999") {
		t.Errorf("expected ts-not-found error, got: %v", res["error"])
	}
	if _, ok := res["availableBackups"]; !ok {
		t.Errorf("expected availableBackups in error response")
	}
}

func TestFileRestore_RestoresLatestByDefault(t *testing.T) {
	deps := fixtureRestoreDeps(t, "ok.conf",
		"current_value\n",
		map[int64]string{
			100: "old_100\n",
			200: "old_200\n",
			300: "newest_backup\n",
		})
	res := callRestore(t, deps, map[string]any{"file": "ok.conf", "confirm": true})
	if e, ok := res["error"]; ok {
		t.Fatalf("unexpected error: %v", e)
	}
	if got, _ := res["restoredFromTs"].(int64); got != 300 {
		t.Errorf("expected restoredFromTs=300 (latest), got %v", res["restoredFromTs"])
	}
	// Verify file contents match the picked backup.
	body, err := os.ReadFile(filepath.Join(deps.ConfigRoot, "ok.conf"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(body) != "newest_backup\n" {
		t.Errorf("expected restored body to match ts=300 backup, got %q", string(body))
	}
	if got, _ := res["restoredBytes"].(int); got != len("newest_backup\n") {
		t.Errorf("expected restoredBytes=14, got %v", res["restoredBytes"])
	}
}

func TestFileRestore_PicksSpecificTs(t *testing.T) {
	deps := fixtureRestoreDeps(t, "ok.conf",
		"new\n",
		map[int64]string{100: "old_100\n", 200: "old_200\n"})
	res := callRestore(t, deps, map[string]any{
		"file": "ok.conf", "backupTs": 100, "confirm": true,
	})
	if e, ok := res["error"]; ok {
		t.Fatalf("unexpected error: %v", e)
	}
	if got, _ := res["restoredFromTs"].(int64); got != 100 {
		t.Errorf("expected restoredFromTs=100, got %v", res["restoredFromTs"])
	}
	body, err := os.ReadFile(filepath.Join(deps.ConfigRoot, "ok.conf"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(body) != "old_100\n" {
		t.Errorf("expected body=old_100, got %q", string(body))
	}
}

func TestFileRestore_SnapshotsCurrentBeforeOverwrite(t *testing.T) {
	deps := fixtureRestoreDeps(t, "ok.conf",
		"CURRENT_BODY\n",
		map[int64]string{100: "OLD_BODY\n"})
	res := callRestore(t, deps, map[string]any{"file": "ok.conf", "confirm": true})
	if e, ok := res["error"]; ok {
		t.Fatalf("unexpected error: %v", e)
	}
	preBackup, _ := res["preRestoreBackup"].(string)
	if preBackup == "" {
		t.Fatalf("expected preRestoreBackup path in response, got: %v", res)
	}
	body, err := os.ReadFile(preBackup)
	if err != nil {
		t.Fatalf("read pre-restore backup: %v", err)
	}
	if string(body) != "CURRENT_BODY\n" {
		t.Errorf("expected pre-restore backup to contain prior body, got %q", string(body))
	}
	// Sanity: the new backup must live under deps.BackupDir.
	rel, err := filepath.Rel(deps.BackupDir, preBackup)
	if err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("expected pre-restore backup under BackupDir; got rel=%q err=%v", rel, err)
	}
}

func TestFileRestore_RoundTripIsReversible(t *testing.T) {
	// Drop the original-current to a known body, then restore an older
	// backup. The pre-restore backup that's created should let us "redo"
	// the original value via a second restore call.
	deps := fixtureRestoreDeps(t, "ok.conf",
		"V2\n",
		map[int64]string{100: "V1\n"})
	res1 := callRestore(t, deps, map[string]any{"file": "ok.conf", "confirm": true})
	if e, ok := res1["error"]; ok {
		t.Fatalf("first restore: unexpected error: %v", e)
	}
	body, err := os.ReadFile(filepath.Join(deps.ConfigRoot, "ok.conf"))
	if err != nil || string(body) != "V1\n" {
		t.Fatalf("expected V1 after first restore, got body=%q err=%v", string(body), err)
	}
	// The pre-restore backup carries the old V2.  Its ts is "now-ish";
	// extract it from the path so we can ask for it explicitly.
	pre, _ := res1["preRestoreBackup"].(string)
	if pre == "" {
		t.Fatalf("expected preRestoreBackup, got: %v", res1)
	}
	preBase := filepath.Base(pre)
	mid := strings.TrimSuffix(strings.TrimPrefix(preBase, "ok.conf."), ".bak")
	if mid == "" {
		t.Fatalf("could not parse ts from pre-restore name %q", preBase)
	}
	// "Redo" by restoring the pre-restore backup.
	args := map[string]any{
		"file":     "ok.conf",
		"backupTs": mid,
		"confirm":  true,
	}
	res2 := callRestore(t, deps, args)
	if e, ok := res2["error"]; ok {
		t.Fatalf("second restore: unexpected error: %v", e)
	}
	body, err = os.ReadFile(filepath.Join(deps.ConfigRoot, "ok.conf"))
	if err != nil || string(body) != "V2\n" {
		t.Errorf("expected V2 after redo, got body=%q err=%v", string(body), err)
	}
}

func TestFileRestore_NoCurrentFileIsFreshCreate(t *testing.T) {
	deps := fixtureRestoreDeps(t, "ok.conf",
		"", // no current file written
		map[int64]string{100: "fresh\n"})
	res := callRestore(t, deps, map[string]any{"file": "ok.conf", "confirm": true})
	if e, ok := res["error"]; ok {
		t.Fatalf("unexpected error: %v", e)
	}
	if _, has := res["preRestoreBackup"]; has {
		t.Errorf("expected NO preRestoreBackup when there was no current file, got: %v", res["preRestoreBackup"])
	}
	if fresh, _ := res["createdFresh"].(bool); !fresh {
		t.Errorf("expected createdFresh=true, got: %v", res["createdFresh"])
	}
	body, err := os.ReadFile(filepath.Join(deps.ConfigRoot, "ok.conf"))
	if err != nil {
		t.Fatalf("read restored file: %v", err)
	}
	if string(body) != "fresh\n" {
		t.Errorf("expected body=fresh, got %q", string(body))
	}
}

func TestFileRestore_BackupTooLarge(t *testing.T) {
	// Build a backup just over the 5 MiB cap.
	huge := strings.Repeat("a", fileRestoreMaxBackupBytes+1)
	deps := fixtureRestoreDeps(t, "ok.conf",
		"current\n",
		map[int64]string{100: huge})
	res := callRestore(t, deps, map[string]any{"file": "ok.conf", "confirm": true})
	if errStr, _ := res["error"].(string); !strings.Contains(errStr, "backup too large") {
		t.Errorf("expected size-cap error, got: %v", res["error"])
	}
	// Verify the current file was NOT overwritten.
	body, err := os.ReadFile(filepath.Join(deps.ConfigRoot, "ok.conf"))
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if string(body) != "current\n" {
		t.Errorf("expected current file untouched on size cap, got %q", string(body))
	}
}

func TestFileRestore_BackupAgeSeconds(t *testing.T) {
	tsRecent := time.Now().Unix() - 60
	deps := fixtureRestoreDeps(t, "ok.conf",
		"a\n",
		map[int64]string{tsRecent: "b\n"})
	res := callRestore(t, deps, map[string]any{"file": "ok.conf", "confirm": true})
	if e, ok := res["error"]; ok {
		t.Fatalf("unexpected error: %v", e)
	}
	age, _ := res["backupAgeSeconds"].(int64)
	if age < 50 || age > 1000 {
		t.Errorf("expected ageSeconds ~60, got %d", age)
	}
}

func TestFileRestore_MissingConfigRoot(t *testing.T) {
	deps := FileRestoreDeps{ConfigRoot: "", BackupDir: "/tmp", AllowedFiles: []string{"ok.conf"}}
	res := callRestore(t, deps, map[string]any{"file": "ok.conf", "confirm": true})
	if errStr, _ := res["error"].(string); !strings.Contains(errStr, "OPS_ADMIN_CONFIG_ROOT") {
		t.Errorf("expected ConfigRoot error, got: %v", res["error"])
	}
}

func TestFileRestore_MissingBackupDir(t *testing.T) {
	deps := FileRestoreDeps{ConfigRoot: "/tmp", BackupDir: "", AllowedFiles: []string{"ok.conf"}}
	res := callRestore(t, deps, map[string]any{"file": "ok.conf", "confirm": true})
	if errStr, _ := res["error"].(string); !strings.Contains(errStr, "OPS_ADMIN_BACKUP_DIR") {
		t.Errorf("expected BackupDir error, got: %v", res["error"])
	}
}
