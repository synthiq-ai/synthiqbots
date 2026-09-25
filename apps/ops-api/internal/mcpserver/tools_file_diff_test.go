package mcpserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixtureDeps lays out a temp config root + backup dir with the given file
// + backup payloads, and returns deps wired to file_diff.
func fixtureDeps(t *testing.T, file string, current string, backups map[int64]string) FileDiffDeps {
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
	return FileDiffDeps{
		ConfigRoot:   configRoot,
		BackupDir:    backupDir,
		AllowedFiles: []string{file},
	}
}

func fileBackupName(file string, ts int64) string {
	return file + "." + intStr(ts) + ".bak"
}

func intStr(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func callDiff(t *testing.T, deps FileDiffDeps, args map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res := handleFileDiff(deps, raw)
	m, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T: %v", res, res)
	}
	return m
}

func TestFileDiff_AllowlistRejection(t *testing.T) {
	deps := fixtureDeps(t, "ok.conf", "x\n", map[int64]string{1: "y\n"})
	res := callDiff(t, deps, map[string]any{"file": "evil.conf"})
	if errStr, _ := res["error"].(string); !strings.Contains(errStr, "OPS_ADMIN_ALLOWED_FILES") {
		t.Errorf("expected allowlist error, got: %v", res["error"])
	}
}

func TestFileDiff_UnsafeBasename(t *testing.T) {
	deps := fixtureDeps(t, "ok.conf", "x", nil)
	for _, bad := range []string{"../etc/passwd", "a/b", ".."} {
		res := callDiff(t, deps, map[string]any{"file": bad})
		if errStr, _ := res["error"].(string); !strings.Contains(errStr, "basename") &&
			!strings.Contains(errStr, "OPS_ADMIN_ALLOWED_FILES") {
			t.Errorf("expected basename or allowlist rejection for %q, got: %v", bad, res["error"])
		}
	}
}

func TestFileDiff_NoBackups(t *testing.T) {
	deps := fixtureDeps(t, "ok.conf", "x\n", nil)
	res := callDiff(t, deps, map[string]any{"file": "ok.conf"})
	if errStr, _ := res["error"].(string); !strings.Contains(errStr, "no backups") {
		t.Errorf("expected no-backups error, got: %v", res["error"])
	}
}

func TestFileDiff_PicksLatestByDefault(t *testing.T) {
	deps := fixtureDeps(t, "ok.conf",
		"line1\nline2 NEW\nline3\n",
		map[int64]string{
			100: "line1\nline2 OLD\nline3\n",
			200: "line1\nline2 OLDER\nline3\n",
			300: "line1\nline2 NEWEST_BACKUP\nline3\n",
		})
	res := callDiff(t, deps, map[string]any{"file": "ok.conf"})
	if e, ok := res["error"]; ok {
		t.Fatalf("unexpected error: %v", e)
	}
	if got, _ := res["backupTs"].(int64); got != 300 {
		t.Errorf("expected backupTs=300 (latest), got %v", res["backupTs"])
	}
	diff, _ := res["diff"].(string)
	if !strings.Contains(diff, "-line2 NEWEST_BACKUP") || !strings.Contains(diff, "+line2 NEW") {
		t.Errorf("expected diff lines vs ts=300, got: %s", diff)
	}
}

func TestFileDiff_SpecificBackupTs(t *testing.T) {
	deps := fixtureDeps(t, "ok.conf",
		"line1\nNEW\nline3\n",
		map[int64]string{
			100: "line1\nOLD_100\nline3\n",
			200: "line1\nOLD_200\nline3\n",
		})
	res := callDiff(t, deps, map[string]any{"file": "ok.conf", "backupTs": 100})
	if e, ok := res["error"]; ok {
		t.Fatalf("unexpected error: %v", e)
	}
	if got, _ := res["backupTs"].(int64); got != 100 {
		t.Errorf("expected backupTs=100, got %v", res["backupTs"])
	}
	diff, _ := res["diff"].(string)
	if !strings.Contains(diff, "-OLD_100") {
		t.Errorf("expected to see OLD_100 line removed, got diff:\n%s", diff)
	}
}

func TestFileDiff_BackupTsNotFound(t *testing.T) {
	deps := fixtureDeps(t, "ok.conf",
		"x\n",
		map[int64]string{100: "y\n"})
	res := callDiff(t, deps, map[string]any{"file": "ok.conf", "backupTs": 999})
	if errStr, _ := res["error"].(string); !strings.Contains(errStr, "no backup with ts=999") {
		t.Errorf("expected ts-not-found error, got: %v", res["error"])
	}
	if _, ok := res["availableBackups"]; !ok {
		t.Errorf("expected availableBackups in error response")
	}
}

func TestFileDiff_IdenticalFlagWhenSame(t *testing.T) {
	body := "line1\nline2\nline3\n"
	deps := fixtureDeps(t, "ok.conf", body, map[int64]string{100: body})
	res := callDiff(t, deps, map[string]any{"file": "ok.conf"})
	if e, ok := res["error"]; ok {
		t.Fatalf("unexpected error: %v", e)
	}
	if id, _ := res["identical"].(bool); !id {
		t.Errorf("expected identical=true, got: %v", res)
	}
	if added, _ := res["addedLines"].(int); added != 0 {
		t.Errorf("expected addedLines=0, got %v", res["addedLines"])
	}
	if removed, _ := res["removedLines"].(int); removed != 0 {
		t.Errorf("expected removedLines=0, got %v", res["removedLines"])
	}
}

func TestFileDiff_AddedRemovedCounts(t *testing.T) {
	deps := fixtureDeps(t, "ok.conf",
		"a\nb\nc\nd\ne\n",
		map[int64]string{100: "a\nb\nc\n"})
	res := callDiff(t, deps, map[string]any{"file": "ok.conf"})
	if e, ok := res["error"]; ok {
		t.Fatalf("unexpected error: %v", e)
	}
	if added, _ := res["addedLines"].(int); added != 2 {
		t.Errorf("expected addedLines=2, got %v", res["addedLines"])
	}
	if removed, _ := res["removedLines"].(int); removed != 0 {
		t.Errorf("expected removedLines=0, got %v", res["removedLines"])
	}
}

func TestFileDiff_ReverseDirection(t *testing.T) {
	deps := fixtureDeps(t, "ok.conf",
		"x=NEW\n",
		map[int64]string{100: "x=OLD\n"})
	resForward := callDiff(t, deps, map[string]any{"file": "ok.conf"})
	if e, ok := resForward["error"]; ok {
		t.Fatalf("unexpected error: %v", e)
	}
	resReverse := callDiff(t, deps, map[string]any{"file": "ok.conf", "reverse": true})
	if e, ok := resReverse["error"]; ok {
		t.Fatalf("unexpected error reverse: %v", e)
	}
	// Forward: from=backup, to=current → addedLines=1 (x=NEW) removedLines=1 (x=OLD)
	// Reverse: from=current, to=backup → addedLines=1 (x=OLD) removedLines=1 (x=NEW)
	addF, _ := resForward["addedLines"].(int)
	addR, _ := resReverse["addedLines"].(int)
	if addF != 1 || addR != 1 {
		t.Errorf("expected addedLines=1 in both directions; forward=%d reverse=%d", addF, addR)
	}
	dF, _ := resForward["diff"].(string)
	dR, _ := resReverse["diff"].(string)
	if !strings.Contains(dF, "+x=NEW") {
		t.Errorf("forward diff should add x=NEW, got: %s", dF)
	}
	if !strings.Contains(dR, "+x=OLD") {
		t.Errorf("reverse diff should add x=OLD, got: %s", dR)
	}
}

func TestFileDiff_TruncationCap(t *testing.T) {
	// Build a synthetic delta: 50 changed lines, then cap output at 5.
	var fromB, toB strings.Builder
	for i := 0; i < 50; i++ {
		fromB.WriteString("old-")
		fromB.WriteString(intStr(int64(i)))
		fromB.WriteByte('\n')
		toB.WriteString("new-")
		toB.WriteString(intStr(int64(i)))
		toB.WriteByte('\n')
	}
	deps := fixtureDeps(t, "ok.conf",
		toB.String(),
		map[int64]string{100: fromB.String()})
	res := callDiff(t, deps, map[string]any{"file": "ok.conf", "maxLines": 5})
	if e, ok := res["error"]; ok {
		t.Fatalf("unexpected error: %v", e)
	}
	if t2, _ := res["truncated"].(bool); !t2 {
		t.Errorf("expected truncated=true, got: %v", res["truncated"])
	}
	diff, _ := res["diff"].(string)
	gotLines := strings.Count(diff, "\n") + 1
	if gotLines > 5 {
		t.Errorf("expected ≤5 diff lines, got %d:\n%s", gotLines, diff)
	}
}

func TestFileDiff_AvailableBackupsLimited(t *testing.T) {
	// Drop 15 backups; assert availableBackups returns ≤10, newest first.
	backups := map[int64]string{}
	for i := int64(1); i <= 15; i++ {
		backups[i] = "ts" + intStr(i) + "\n"
	}
	deps := fixtureDeps(t, "ok.conf", "current\n", backups)
	res := callDiff(t, deps, map[string]any{"file": "ok.conf"})
	if e, ok := res["error"]; ok {
		t.Fatalf("unexpected error: %v", e)
	}
	avail, ok := res["availableBackups"].([]map[string]any)
	if !ok {
		t.Fatalf("expected []map availableBackups, got %T", res["availableBackups"])
	}
	if len(avail) != 10 {
		t.Errorf("expected 10 entries, got %d", len(avail))
	}
	// First entry should be ts=15 (newest).
	if first, _ := avail[0]["ts"].(int64); first != 15 {
		t.Errorf("expected first ts=15, got %v", avail[0]["ts"])
	}
}

func TestFileDiff_BackupAgeSeconds(t *testing.T) {
	// Use a ts slightly in the past, assert ageSeconds is non-negative.
	tsRecent := time.Now().Unix() - 60
	deps := fixtureDeps(t, "ok.conf",
		"a\n",
		map[int64]string{tsRecent: "b\n"})
	res := callDiff(t, deps, map[string]any{"file": "ok.conf"})
	if e, ok := res["error"]; ok {
		t.Fatalf("unexpected error: %v", e)
	}
	age, _ := res["backupAgeSeconds"].(int64)
	if age < 50 || age > 1000 {
		t.Errorf("expected ageSeconds ~60, got %d", age)
	}
}

func TestListBackups_IgnoresMalformed(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"ok.conf.100.bak",       // valid
		"ok.conf.200.bak",       // valid
		"ok.conf.bak",           // missing ts
		"ok.conf.notats.bak",    // non-numeric ts
		"other.conf.300.bak",    // different basename
		"ok.conf.100.bak.extra", // trailing junk
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}
	got, err := listBackups(dir, "ok.conf")
	if err != nil {
		t.Fatalf("listBackups: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 valid backups, got %d: %+v", len(got), got)
	}
	if got[0].Ts != 200 || got[1].Ts != 100 {
		t.Errorf("expected newest-first sort, got %+v", got)
	}
}

func TestListBackups_MissingDir(t *testing.T) {
	got, err := listBackups("/no/such/path", "ok.conf")
	if err != nil {
		t.Errorf("expected nil error for missing dir, got: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil slice for missing dir, got: %+v", got)
	}
}

func TestEditScript_AllEqual(t *testing.T) {
	from := []string{"a", "b", "c"}
	to := []string{"a", "b", "c"}
	script := editScript(from, to)
	if len(script) != 3 {
		t.Fatalf("expected 3 ops, got %d", len(script))
	}
	for _, o := range script {
		if o.kind != ' ' {
			t.Errorf("expected all equal ops, got %c %q", o.kind, o.text)
		}
	}
}

func TestEditScript_AllInsert(t *testing.T) {
	script := editScript(nil, []string{"x", "y"})
	if len(script) != 2 || script[0].kind != '+' || script[1].kind != '+' {
		t.Fatalf("expected 2 inserts, got %+v", script)
	}
}

func TestEditScript_AllDelete(t *testing.T) {
	script := editScript([]string{"x", "y"}, nil)
	if len(script) != 2 || script[0].kind != '-' || script[1].kind != '-' {
		t.Fatalf("expected 2 deletes, got %+v", script)
	}
}

func TestUnifiedDiff_HunkHeaderArithmetic(t *testing.T) {
	// from: 1..6  to: 1, 2, X, 4, 5, 6
	from := []string{"1", "2", "3", "4", "5", "6"}
	to := []string{"1", "2", "X", "4", "5", "6"}
	added, removed, lines, truncated := unifiedDiff(from, to, 1, 100)
	if added != 1 || removed != 1 {
		t.Errorf("expected +1/-1, got +%d/-%d", added, removed)
	}
	if truncated {
		t.Errorf("did not expect truncation")
	}
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "@@") {
		t.Fatalf("expected hunk header, got: %v", lines)
	}
	// Hunk should cover lines 2..4 in both files (1 line context each side around line 3).
	// Header format: @@ -2,3 +2,3 @@
	if lines[0] != "@@ -2,3 +2,3 @@" {
		t.Errorf("expected '@@ -2,3 +2,3 @@', got %q", lines[0])
	}
}
