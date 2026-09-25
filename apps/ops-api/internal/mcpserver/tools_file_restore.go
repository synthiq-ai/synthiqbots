package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	fileRestoreMaxBackupBytes = 5 * 1024 * 1024 // 5 MiB cap, matches file_write hard ceiling
)

// FileRestoreDeps wires file_restore_backup to the same allowlist + backup
// directory + config root as file_set_key / file_diff. We deliberately reuse
// AllowedFiles so the agent can only restore files it could have edited in
// the first place — there is no separate restore allowlist.
type FileRestoreDeps struct {
	ConfigRoot   string
	BackupDir    string
	AllowedFiles []string
}

// RegisterFileRestoreTool registers `file_restore_backup` — the rollback half
// of the file_diff loop. After file_set_key / file_write writes a *.bak
// snapshot of the prior content, file_diff lets the operator inspect that
// snapshot, but until now the only way to roll back was to file_write the
// whole backup body manually. file_restore_backup closes that gap with a
// single atomic restore that ALSO snapshots the current state to a new *.bak
// first — so a mistaken restore is itself reversible.
func RegisterFileRestoreTool(reg *Registry, deps FileRestoreDeps) {
	reg.Register(Tool{
		Name: "file_restore_backup",
		Description: "Restore an allowlisted .conf file (under OPS_ADMIN_CONFIG_ROOT) from one " +
			"of its timestamped backups under OPS_ADMIN_BACKUP_DIR. Defaults to the most " +
			"recent backup; pass `backupTs` to pick a specific one. Before overwriting, the " +
			"CURRENT file is itself backed up to OPS_ADMIN_BACKUP_DIR with a fresh timestamp " +
			"— so a mistaken restore can be undone by calling this tool again with the new " +
			"backupTs. Requires `confirm:true`. `file` must be in OPS_ADMIN_ALLOWED_FILES " +
			"(same allowlist as file_set_key / file_diff). After a successful restore on a " +
			".conf, call config_reload via the gameplay MCP to apply.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"file":{"type":"string","description":"Basename of the file (must be in OPS_ADMIN_ALLOWED_FILES)"},
			"backupTs":{"type":"integer","description":"Specific backup unix timestamp (default: latest backup)"},
			"confirm":{"type":"boolean","description":"MUST be true to actually restore"}
		},"required":["file","confirm"]}`),
		Annotations: AnnAction(true),
		Destructive: true,
		Handler: func(_ context.Context, raw json.RawMessage, _ string) any {
			return handleFileRestore(deps, raw)
		},
	})
}

func handleFileRestore(deps FileRestoreDeps, raw json.RawMessage) any {
	var a struct {
		File     string `json:"file"`
		BackupTs any    `json:"backupTs"`
		Confirm  bool   `json:"confirm"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return map[string]any{"error": "decode args: " + err.Error()}
	}
	if !a.Confirm {
		return map[string]any{"error": "confirm:true required for file_restore_backup"}
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

	// Read backup contents with the same size cap atomicWrite implicitly
	// inherits via FILE_WRITE: a runaway *.bak file shouldn't blow up memory
	// or produce a partially-flushed restore.
	st, err := os.Stat(picked.Path)
	if err != nil {
		return map[string]any{"error": "stat backup: " + err.Error()}
	}
	if st.Size() > fileRestoreMaxBackupBytes {
		return map[string]any{"error": fmt.Sprintf(
			"backup too large: %d bytes (cap %d)", st.Size(), fileRestoreMaxBackupBytes)}
	}
	data, err := os.ReadFile(picked.Path)
	if err != nil {
		return map[string]any{"error": "read backup: " + err.Error()}
	}

	// Snapshot the CURRENT file before overwriting — this makes the restore
	// itself reversible. Skip when the current file does not exist (the
	// restore degenerates into a fresh create with no rollback target).
	var preRestoreBackup string
	if _, statErr := os.Stat(currentPath); statErr == nil {
		b, err := backupFile(currentPath, a.File, deps.BackupDir)
		if err != nil {
			return map[string]any{"error": "pre-restore backup: " + err.Error()}
		}
		preRestoreBackup = b
	}

	if err := atomicWrite(currentPath, data, 0); err != nil {
		return map[string]any{
			"error":            err.Error(),
			"preRestoreBackup": preRestoreBackup,
		}
	}

	out := map[string]any{
		"ok":               true,
		"file":             a.File,
		"currentPath":      currentPath,
		"restoredFromTs":   picked.Ts,
		"restoredFromPath": picked.Path,
		"restoredBytes":    len(data),
		"backupAgeSeconds": int64(time.Since(time.Unix(picked.Ts, 0)).Seconds()),
		"hint":             "Call config_reload via the gameplay MCP to apply the restored values.",
	}
	if preRestoreBackup != "" {
		out["preRestoreBackup"] = preRestoreBackup
	} else {
		out["createdFresh"] = true
	}
	return out
}

