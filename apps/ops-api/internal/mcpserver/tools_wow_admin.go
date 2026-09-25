package mcpserver

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
)

// DockerExecer is the subset of *dockerlog.Client that this file uses.
// Abstracted for tests so tool argv assembly is unit-testable without
// spawning real containers.
type DockerExecer interface {
	Exec(ctx context.Context, container string, opts dockerlog.ExecOpts) (*dockerlog.ExecResult, error)
	Inspect(ctx context.Context, container string) (*dockerlog.Inspect, error)
	Stop(ctx context.Context, container string, timeoutSec int) error
	Start(ctx context.Context, container string) error
}

type WowDeps struct {
	Docker         DockerExecer
	BackupDir      string            // /backups inside the container
	WowRoot        string            // /wow-root — for backup_dir / restore_dir
	DBContainer    string            // ac-database
	WorldContainer string            // ac-worldserver
	VolumeMounts   map[string]string // logical name → local path inside ops-api container, e.g. {"db":"/volumes/db"}
	VolumeNames    map[string]string // logical name → docker volume name (for documentation in tool output)
	VolumeOwners   map[string]string // logical name → container that mounts it (must be stopped before restore)

	// AuthDB is opened against acore_auth using the ops_rw user — separate
	// from ExecDB which targets acore_characters by default.
	AuthDB *sql.DB
}

const (
	wowExecTimeout    = 30 * time.Second
	wowBackupTimeout  = 30 * time.Minute // mysqldump on a multi-GB acore_world can take a while
	wowRestoreTimeout = 30 * time.Minute
)

var (
	// IPv4 / hostname-ish — defends realm address updates from stray quote injection.
	reRealmAddress = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)
	// Generic backup filename — used by wow_list_backups / wow_prune_old_backups
	// and as the BASE check for restore. Restore tools then layer on a
	// type-specific check (wow-dir-* vs wow-volume-<name>-* vs wow-<db>-*.sql.gz)
	// so a caller can't restore a dir backup over a volume mount, etc.
	reBackupFilename       = regexp.MustCompile(`^wow-[a-zA-Z0-9._-]+\.(?:tar\.gz|sql\.gz)$`)
	// Per-type filename guards. Timestamp is the YYYYMMDD-HHMMSS produced by
	// time.Format("20060102-150405") — pinning it lets the volume regex use
	// a tight `[a-z][a-z0-9]*` capture for the logical name without ambiguity.
	reBackupFilenameDir    = regexp.MustCompile(`^wow-dir-\d{8}-\d{6}(?:-[a-zA-Z0-9._-]+)?\.tar\.gz$`)
	reBackupFilenameDB     = regexp.MustCompile(`^wow-(?:acore_world|acore_characters|acore_auth|all)-\d{8}-\d{6}\.sql\.gz$`)
	reBackupFilenameVolume = regexp.MustCompile(`^wow-volume-([a-z][a-z0-9]*)-\d{8}-\d{6}\.tar\.gz$`)
)

// RegisterWowTools registers the 13 master_wow.sh-equivalent tools.
//
// Tools split into four conceptual groups:
//
//	account / realm: wow_create_account, wow_set_gm_level, wow_check_realmlist,
//	                 wow_update_realm_ip, wow_update_realm_port
//	backup:          wow_backup_dir, wow_backup_db, wow_backup_volumes
//	restore:         wow_restore_dir, wow_restore_db, wow_restore_volume_db,
//	                 wow_restore_volume_client
//	housekeeping:    wow_list_backups (read-only), wow_prune_old_backups
//
// All destructive tools require confirm:true (per db_exec convention) and
// inherit OPS_ADMIN_ALLOW_ACTIONS + per-tool rate limits + audit. Restore-volume
// tools additionally pre-check that the owning container is stopped — restoring
// a volume mounted by a running container can corrupt it.
func RegisterWowTools(reg *Registry, deps WowDeps) {
	registerWowAccountTools(reg, deps)
	registerWowRealmTools(reg, deps)
	registerWowBackupTools(reg, deps)
	registerWowRestoreTools(reg, deps)
	registerWowHousekeepingTools(reg, deps)
}

// -------------------------- account (direct SQL via SRP6) --------------------------

// AzerothCore ships no `worldserver-cli` binary — the original tool tried to
// `docker exec ac-worldserver worldserver-cli ...` and got
// "executable file not found in $PATH". master_wow.sh has the same bug.
//
// Fix: skip the daemon entirely, write straight to acore_auth via the ops_rw
// pool. wow_create_account computes the SRP6 verifier in-process (mirrors
// AzerothCore SRP6.cpp::MakeRegistrationData — see srp6.go) and INSERTs into
// `account`; wow_set_gm_level UPSERTs into `account_access` (level=0 deletes,
// to match the AzerothCore CLI's "no row == player" semantic). ops_rw needs
// SELECT/INSERT/UPDATE/DELETE on those two tables — granted manually on
// ac-database, not in repo migrations. The running worldserver isn't
// consulted; new rows are picked up on the next login attempt.

func registerWowAccountTools(reg *Registry, deps WowDeps) {
	reg.Register(Tool{
		Name: "wow_create_account",
		Description: "Create a WoW account by INSERTing into `acore_auth.account` with an " +
			"SRP6 verifier computed in-process (mirrors AzerothCore SRP6.cpp). REQUIRES " +
			"`confirm:true`. Username 4–16 chars, password 4–16 chars, [a-zA-Z0-9._-] only. " +
			"Username is uppercased before insert (acore convention; the unique-key check " +
			"folds case implicitly). Password is redacted in the audit row. Returns " +
			"{username, account_id, ok}. Does NOT touch the running worldserver — new row " +
			"is picked up on next login.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"username":{"type":"string","minLength":4,"maxLength":16},
			"password":{"type":"string","minLength":4,"maxLength":16},
			"email":{"type":"string","description":"Optional; defaults to ''"},
			"confirm":{"type":"boolean"}
		},"required":["username","password","confirm"]}`),
		Annotations: AnnAction(false),
		Destructive: true,
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Username, Password, Email string
				Confirm                   bool
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if !a.Confirm {
				return map[string]any{"error": "confirm:true required"}
			}
			if !validAccountField(a.Username) || !validAccountField(a.Password) {
				return map[string]any{"error": "username/password must be 4–16 chars, [a-zA-Z0-9._-]"}
			}
			if deps.AuthDB == nil {
				return map[string]any{"error": "OPS_DB_AUTH_DSN not configured"}
			}
			c, cancel := context.WithTimeout(ctx, wowExecTimeout)
			defer cancel()
			upper := strings.ToUpper(a.Username)
			salt, verifier, err := Srp6CalcVerifier(a.Username, a.Password)
			if err != nil {
				return map[string]any{"error": "srp6: " + err.Error()}
			}
			// INSERT IGNORE so a duplicate username yields rowsAffected=0
			// instead of a driver error — caller checks ok via that.
			res, err := deps.AuthDB.ExecContext(c,
				"INSERT IGNORE INTO `account` (username, salt, verifier, email, reg_mail, joindate) "+
					"VALUES (?, ?, ?, ?, ?, NOW())",
				upper, salt[:], verifier[:], a.Email, a.Email)
			if err != nil {
				return map[string]any{"error": "insert account: " + err.Error()}
			}
			ra, _ := res.RowsAffected()
			id, _ := res.LastInsertId()
			if ra == 0 {
				return map[string]any{"error": "account already exists", "username": upper}
			}
			return map[string]any{"ok": true, "username": upper, "account_id": id}
		},
	})

	reg.Register(Tool{
		Name: "wow_set_gm_level",
		Description: "Set the GM level on an existing account via UPSERT into " +
			"`acore_auth.account_access` (PK is (id, RealmID)). Level 0–4 (0=player, " +
			"4=admin). realm_id defaults to -1 (all realms). REQUIRES `confirm:true`. " +
			"Setting level=0 DELETEs the access row entirely (matches AzerothCore CLI " +
			"semantics — a player has no row, not a level-0 row). Does NOT touch the " +
			"running worldserver — picked up on next login.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"username":{"type":"string"},
			"level":{"type":"integer","minimum":0,"maximum":4},
			"realm_id":{"type":"integer","description":"-1 = all realms (default)"},
			"confirm":{"type":"boolean"}
		},"required":["username","level","confirm"]}`),
		Annotations: AnnAction(true),
		Destructive: true,
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Username string
				Level    int
				RealmID  *int `json:"realm_id"`
				Confirm  bool
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if !a.Confirm {
				return map[string]any{"error": "confirm:true required"}
			}
			if !validAccountField(a.Username) {
				return map[string]any{"error": "username must be 4–16 chars, [a-zA-Z0-9._-]"}
			}
			if a.Level < 0 || a.Level > 4 {
				return map[string]any{"error": "level must be 0–4"}
			}
			realmID := -1
			if a.RealmID != nil {
				realmID = *a.RealmID
			}
			if deps.AuthDB == nil {
				return map[string]any{"error": "OPS_DB_AUTH_DSN not configured"}
			}
			c, cancel := context.WithTimeout(ctx, wowExecTimeout)
			defer cancel()
			upper := strings.ToUpper(a.Username)
			var accountID int64
			if err := deps.AuthDB.QueryRowContext(c,
				"SELECT id FROM `account` WHERE username = ?", upper,
			).Scan(&accountID); err != nil {
				if err == sql.ErrNoRows {
					return map[string]any{"error": "account not found", "username": upper}
				}
				return map[string]any{"error": "lookup account: " + err.Error()}
			}
			if a.Level == 0 {
				res, err := deps.AuthDB.ExecContext(c,
					"DELETE FROM `account_access` WHERE id = ? AND RealmID = ?",
					accountID, realmID)
				if err != nil {
					return map[string]any{"error": "delete access: " + err.Error()}
				}
				ra, _ := res.RowsAffected()
				return map[string]any{"ok": true, "username": upper, "account_id": accountID,
					"level": 0, "realm_id": realmID, "rows_affected": ra}
			}
			// REPLACE INTO is the cleanest UPSERT against (id, RealmID) PK.
			res, err := deps.AuthDB.ExecContext(c,
				"REPLACE INTO `account_access` (id, gmlevel, RealmID, comment) VALUES (?, ?, ?, '')",
				accountID, a.Level, realmID)
			if err != nil {
				return map[string]any{"error": "upsert access: " + err.Error()}
			}
			ra, _ := res.RowsAffected()
			return map[string]any{"ok": true, "username": upper, "account_id": accountID,
				"level": a.Level, "realm_id": realmID, "rows_affected": ra}
		},
	})
}

// -------------------------- realm (acore_auth) --------------------------

func registerWowRealmTools(reg *Registry, deps WowDeps) {
	reg.Register(Tool{
		Name: "wow_check_realmlist",
		Description: "SELECT id, name, address, port, icon, timezone, allowedSecurityLevel FROM " +
			"realmlist on acore_auth via the ops_rw user. Read-only.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, _ json.RawMessage, _ string) any {
			if deps.AuthDB == nil {
				return map[string]any{"error": "OPS_DB_AUTH_DSN not configured"}
			}
			c, cancel := context.WithTimeout(ctx, dbQueryTimeout)
			defer cancel()
			rows, err := deps.AuthDB.QueryContext(c,
				"SELECT id, name, address, port, icon, timezone, allowedSecurityLevel FROM realmlist")
			if err != nil {
				return map[string]any{"error": "query: " + err.Error()}
			}
			defer rows.Close()
			out := []map[string]any{}
			for rows.Next() {
				var id, port, icon, tz, sec int
				var name, addr string
				if err := rows.Scan(&id, &name, &addr, &port, &icon, &tz, &sec); err != nil {
					return map[string]any{"error": "scan: " + err.Error()}
				}
				out = append(out, map[string]any{
					"id": id, "name": name, "address": addr, "port": port,
					"icon": icon, "timezone": tz, "allowedSecurityLevel": sec,
				})
			}
			return map[string]any{"realms": out, "count": len(out)}
		},
	})

	reg.Register(Tool{
		Name: "wow_update_realm_ip",
		Description: "UPDATE realmlist SET address=? WHERE id=? on acore_auth. Address must be a " +
			"hostname or IPv4 (rejects anything containing quotes / spaces / SQL metachars). " +
			"REQUIRES `confirm:true`.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"address":{"type":"string","description":"Hostname or IPv4"},
			"realm_id":{"type":"integer","minimum":1},
			"confirm":{"type":"boolean"}
		},"required":["address","realm_id","confirm"]}`),
		Annotations: AnnAction(true),
		Destructive: true,
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Address string
				RealmID int `json:"realm_id"`
				Confirm bool
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if !a.Confirm {
				return map[string]any{"error": "confirm:true required"}
			}
			if !reRealmAddress.MatchString(a.Address) {
				return map[string]any{"error": "address must match [a-zA-Z0-9._-]"}
			}
			// Belt-and-braces: IPv4-or-DNS-looking only. net.ParseIP returns
			// non-nil for valid IPv4/IPv6; LookupHost would resolve, which we
			// don't actually want to require during the call.
			if ip := net.ParseIP(a.Address); ip == nil && !looksLikeHostname(a.Address) {
				return map[string]any{"error": "address must be a hostname or IPv4"}
			}
			if a.RealmID < 1 {
				return map[string]any{"error": "realm_id must be >= 1"}
			}
			if deps.AuthDB == nil {
				return map[string]any{"error": "OPS_DB_AUTH_DSN not configured"}
			}
			c, cancel := context.WithTimeout(ctx, dbExecTimeout)
			defer cancel()
			res, err := deps.AuthDB.ExecContext(c,
				"UPDATE realmlist SET address=? WHERE id=?", a.Address, a.RealmID)
			if err != nil {
				return map[string]any{"error": "exec: " + err.Error()}
			}
			ra, _ := res.RowsAffected()
			return map[string]any{"ok": true, "rows_affected": ra, "address": a.Address, "realm_id": a.RealmID}
		},
	})

	reg.Register(Tool{
		Name: "wow_update_realm_port",
		Description: "UPDATE realmlist SET port=? WHERE id=? on acore_auth. Port must be 1024–65535. " +
			"REQUIRES `confirm:true`.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"port":{"type":"integer","minimum":1024,"maximum":65535},
			"realm_id":{"type":"integer","minimum":1},
			"confirm":{"type":"boolean"}
		},"required":["port","realm_id","confirm"]}`),
		Annotations: AnnAction(true),
		Destructive: true,
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Port    int
				RealmID int `json:"realm_id"`
				Confirm bool
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if !a.Confirm {
				return map[string]any{"error": "confirm:true required"}
			}
			if a.Port < 1024 || a.Port > 65535 {
				return map[string]any{"error": "port must be 1024–65535"}
			}
			if a.RealmID < 1 {
				return map[string]any{"error": "realm_id must be >= 1"}
			}
			if deps.AuthDB == nil {
				return map[string]any{"error": "OPS_DB_AUTH_DSN not configured"}
			}
			c, cancel := context.WithTimeout(ctx, dbExecTimeout)
			defer cancel()
			res, err := deps.AuthDB.ExecContext(c,
				"UPDATE realmlist SET port=? WHERE id=?", a.Port, a.RealmID)
			if err != nil {
				return map[string]any{"error": "exec: " + err.Error()}
			}
			ra, _ := res.RowsAffected()
			return map[string]any{"ok": true, "rows_affected": ra, "port": a.Port, "realm_id": a.RealmID}
		},
	})
}

// -------------------------- backup --------------------------

func registerWowBackupTools(reg *Registry, deps WowDeps) {
	reg.Register(Tool{
		Name: "wow_backup_dir",
		Description: "Tar+gzip the entire AzerothCore directory ($OPS_WOW_ROOT, default /wow-root) " +
			"to $OPS_WOW_BACKUP_DIR/wow-dir-<ts>.tar.gz, excluding .git/. REQUIRES `confirm:true`. " +
			"Returns {file, bytes, duration_ms}.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"label":{"type":"string","description":"Optional suffix on the filename, e.g. \"pre-deploy\""},
			"confirm":{"type":"boolean"}
		},"required":["confirm"]}`),
		Annotations: AnnAction(false),
		Destructive: true,
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Label   string
				Confirm bool
			}
			_ = json.Unmarshal(raw, &a)
			if !a.Confirm {
				return map[string]any{"error": "confirm:true required"}
			}
			ts := time.Now().UTC().Format("20060102-150405")
			suffix := ""
			if a.Label != "" {
				if !validLabel(a.Label) {
					return map[string]any{"error": "label must be [a-zA-Z0-9._-]"}
				}
				suffix = "-" + a.Label
			}
			out := filepath.Join(deps.BackupDir, "wow-dir-"+ts+suffix+".tar.gz")
			c, cancel := context.WithTimeout(ctx, wowBackupTimeout)
			defer cancel()
			n, err := tarGz(c, deps.WowRoot, out, []string{".git"})
			if err != nil {
				_ = os.Remove(out)
				return map[string]any{"error": err.Error()}
			}
			return map[string]any{"ok": true, "file": out, "bytes": n}
		},
	})

	reg.Register(Tool{
		Name: "wow_backup_db",
		Description: "mysqldump one of acore_world / acore_characters / acore_auth (or `all`) into " +
			"$OPS_WOW_BACKUP_DIR/wow-<db>-<ts>.sql.gz. Runs `mysqldump --add-drop-table " +
			"--databases <db>` inside $OPS_DB_CONTAINER (default ac-database) and gzips on the " +
			"fly. REQUIRES `confirm:true`. Returns {file, bytes, duration_ms}.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"database":{"type":"string","enum":["acore_world","acore_characters","acore_auth","all"]},
			"db_user":{"type":"string","description":"mysql user (default 'root')"},
			"db_pass":{"type":"string","description":"mysql password (REDACTED in audit)"},
			"confirm":{"type":"boolean"}
		},"required":["database","db_pass","confirm"]}`),
		Annotations: AnnAction(false),
		Destructive: true,
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Database string `json:"database"`
				DBUser   string `json:"db_user"`
				DBPass   string `json:"db_pass"`
				Confirm  bool   `json:"confirm"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if !a.Confirm {
				return map[string]any{"error": "confirm:true required"}
			}
			if a.DBPass == "" {
				return map[string]any{"error": "db_pass required"}
			}
			dbName := a.Database
			if dbName != "acore_world" && dbName != "acore_characters" && dbName != "acore_auth" && dbName != "all" {
				return map[string]any{"error": "database must be acore_world/acore_characters/acore_auth/all"}
			}
			user := a.DBUser
			if user == "" {
				user = "root"
			}
			args := []string{"mysqldump", "-u" + user, "-p" + a.DBPass, "--add-drop-table"}
			if dbName == "all" {
				args = append(args, "--all-databases")
			} else {
				args = append(args, "--databases", dbName)
			}
			ts := time.Now().UTC().Format("20060102-150405")
			out := filepath.Join(deps.BackupDir, fmt.Sprintf("wow-%s-%s.sql.gz", dbName, ts))
			f, err := os.Create(out)
			if err != nil {
				return map[string]any{"error": "create: " + err.Error()}
			}
			defer f.Close()
			gz := gzip.NewWriter(f)
			c, cancel := context.WithTimeout(ctx, wowBackupTimeout)
			defer cancel()
			t0 := time.Now()
			res, err := deps.Docker.Exec(c, deps.DBContainer, dockerlog.ExecOpts{
				Cmd:    args,
				Stdout: gz, // stream straight into the gzip writer
			})
			// gz.Close flushes the gzip trailer + checksum. If it errors,
			// the resulting file is unreadable — surface that even on a
			// successful exec rather than reporting ok=true with a corrupt
			// dump. Likewise check f.Close (writes pending OS buffers).
			gzCloseErr := gz.Close()
			fCloseErr := f.Close()
			st, _ := os.Stat(out)
			if err != nil {
				_ = os.Remove(out)
				return map[string]any{"error": "exec: " + err.Error()}
			}
			if res.ExitCode != 0 {
				_ = os.Remove(out)
				return map[string]any{
					"error":     fmt.Sprintf("mysqldump exit %d", res.ExitCode),
					"stderr":    string(res.Stderr),
					"exit_code": res.ExitCode,
				}
			}
			if gzCloseErr != nil {
				_ = os.Remove(out)
				return map[string]any{"error": "gzip close: " + gzCloseErr.Error()}
			}
			if fCloseErr != nil {
				_ = os.Remove(out)
				return map[string]any{"error": "file close: " + fCloseErr.Error()}
			}
			return map[string]any{
				"ok": true, "file": out, "bytes": fileSize(st),
				"duration_ms": int(time.Since(t0).Milliseconds()),
			}
		},
	})

	reg.Register(Tool{
		Name: "wow_backup_volumes",
		Description: "Tar+gzip a docker volume's mount directory (configured via " +
			"OPS_WOW_VOLUME_MOUNTS_JSON). One of {db, client}. UNSAFE for live mysql data " +
			"unless the database is stopped first — prefer wow_backup_db for the database. " +
			"REQUIRES `confirm:true`.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"volume":{"type":"string","description":"Logical name: db or client"},
			"confirm":{"type":"boolean"}
		},"required":["volume","confirm"]}`),
		Annotations: AnnAction(false),
		Destructive: true,
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Volume  string
				Confirm bool
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if !a.Confirm {
				return map[string]any{"error": "confirm:true required"}
			}
			mount, ok := deps.VolumeMounts[a.Volume]
			if !ok {
				return map[string]any{
					"error":              fmt.Sprintf("unknown volume %q; configure OPS_WOW_VOLUME_MOUNTS_JSON", a.Volume),
					"known_volumes":      keysOf(deps.VolumeMounts),
				}
			}
			if _, err := os.Stat(mount); err != nil {
				return map[string]any{"error": "volume mount not present in container: " + err.Error()}
			}
			ts := time.Now().UTC().Format("20060102-150405")
			out := filepath.Join(deps.BackupDir, fmt.Sprintf("wow-volume-%s-%s.tar.gz", a.Volume, ts))
			c, cancel := context.WithTimeout(ctx, wowBackupTimeout)
			defer cancel()
			n, err := tarGz(c, mount, out, nil)
			if err != nil {
				_ = os.Remove(out)
				return map[string]any{"error": err.Error()}
			}
			return map[string]any{"ok": true, "file": out, "bytes": n,
				"docker_volume": deps.VolumeNames[a.Volume]}
		},
	})
}

// -------------------------- restore --------------------------

func registerWowRestoreTools(reg *Registry, deps WowDeps) {
	reg.Register(Tool{
		Name: "wow_restore_dir",
		Description: "Untar a wow-dir-*.tar.gz backup back into $OPS_WOW_ROOT (the same directory " +
			"wow_backup_dir reads from). The backup file must be a basename inside " +
			"$OPS_WOW_BACKUP_DIR (no path traversal). REQUIRES `confirm:true`. WARNING: " +
			"overwrites the AzerothCore tree in place — symlinks/hardlinks in the archive " +
			"and parent-directory symlink traversal are refused at extraction time.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"backup_file":{"type":"string","description":"Basename, e.g. wow-dir-20260504-101530.tar.gz"},
			"confirm":{"type":"boolean"}
		},"required":["backup_file","confirm"]}`),
		Annotations: AnnAction(false),
		Destructive: true,
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				BackupFile string `json:"backup_file"`
				Confirm    bool   `json:"confirm"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if !a.Confirm {
				return map[string]any{"error": "confirm:true required"}
			}
			if !reBackupFilenameDir.MatchString(a.BackupFile) {
				return map[string]any{"error": "backup_file must be a wow-dir-*.tar.gz basename"}
			}
			abs := filepath.Join(deps.BackupDir, a.BackupFile)
			if _, err := os.Stat(abs); err != nil {
				return map[string]any{"error": "stat backup: " + err.Error()}
			}
			// tarGz writes entries relative to srcResolved (the WowRoot itself),
			// so restore extracts back into WowRoot — NOT its parent. Extracting
			// to filepath.Dir(WowRoot) would scatter README.md/modules/ one level
			// above the intended root.
			c, cancel := context.WithTimeout(ctx, wowRestoreTimeout)
			defer cancel()
			n, err := untarGz(c, abs, deps.WowRoot)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return map[string]any{"ok": true, "file": abs, "extracted_to": deps.WowRoot, "files": n}
		},
	})

	reg.Register(Tool{
		Name: "wow_restore_db",
		Description: "Pipe `gunzip < <backup>` into `mysql -u<user> -p<pass> <database>` running " +
			"inside $OPS_DB_CONTAINER. Backup file must be a basename inside $OPS_WOW_BACKUP_DIR. " +
			"REQUIRES `confirm:true`. The password is redacted in the audit row.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"backup_file":{"type":"string","description":"Basename, e.g. wow-acore_world-20260504-101530.sql.gz"},
			"database":{"type":"string","enum":["acore_world","acore_characters","acore_auth","all"]},
			"db_user":{"type":"string","description":"mysql user (default 'root')"},
			"db_pass":{"type":"string","description":"mysql password (REDACTED in audit)"},
			"confirm":{"type":"boolean"}
		},"required":["backup_file","database","db_pass","confirm"]}`),
		Annotations: AnnAction(false),
		Destructive: true,
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				BackupFile string `json:"backup_file"`
				Database   string
				DBUser     string `json:"db_user"`
				DBPass     string `json:"db_pass"`
				Confirm    bool
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return map[string]any{"error": "decode args: " + err.Error()}
			}
			if !a.Confirm {
				return map[string]any{"error": "confirm:true required"}
			}
			if a.DBPass == "" {
				return map[string]any{"error": "db_pass required"}
			}
			if a.Database != "acore_world" && a.Database != "acore_characters" && a.Database != "acore_auth" && a.Database != "all" {
				return map[string]any{"error": "database must be acore_world/acore_characters/acore_auth/all"}
			}
			if !reBackupFilenameDB.MatchString(a.BackupFile) {
				return map[string]any{"error": "backup_file must be a wow-<db>-<ts>.sql.gz basename"}
			}
			abs := filepath.Join(deps.BackupDir, a.BackupFile)
			f, err := os.Open(abs)
			if err != nil {
				return map[string]any{"error": "open backup: " + err.Error()}
			}
			defer f.Close()
			gz, err := gzip.NewReader(f)
			if err != nil {
				return map[string]any{"error": "gunzip: " + err.Error()}
			}
			defer gz.Close()
			user := a.DBUser
			if user == "" {
				user = "root"
			}
			args := []string{"mysql", "-u" + user, "-p" + a.DBPass}
			if a.Database != "all" {
				args = append(args, a.Database)
			}
			c, cancel := context.WithTimeout(ctx, wowRestoreTimeout)
			defer cancel()
			t0 := time.Now()
			res, err := deps.Docker.Exec(c, deps.DBContainer, dockerlog.ExecOpts{
				Cmd:   args,
				Stdin: gz,
			})
			if err != nil {
				return map[string]any{"error": "exec: " + err.Error()}
			}
			out := map[string]any{
				"file": abs, "exit_code": res.ExitCode,
				"duration_ms": int(time.Since(t0).Milliseconds()),
			}
			if res.ExitCode == 0 {
				out["ok"] = true
			} else {
				out["error"] = fmt.Sprintf("mysql exit %d", res.ExitCode)
				out["stderr"] = string(res.Stderr)
			}
			return out
		},
	})

	reg.Register(Tool{
		Name: "wow_restore_volume_db",
		Description: "Restore the database docker volume from a wow-volume-db-*.tar.gz backup. " +
			"Stops $OPS_DB_CONTAINER first (safety: restoring a live mysql volume corrupts " +
			"it), wipes the mount dir, untars, restarts the container. REQUIRES `confirm:true`. " +
			"HIGHEST BLAST RADIUS — entire database state is replaced.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"backup_file":{"type":"string"},
			"confirm":{"type":"boolean"}
		},"required":["backup_file","confirm"]}`),
		Annotations: AnnAction(false),
		Destructive: true,
		Handler: makeVolumeRestoreHandler(deps, "db", deps.DBContainer),
	})

	reg.Register(Tool{
		Name: "wow_restore_volume_client",
		Description: "Restore the client-data docker volume from a wow-volume-client-*.tar.gz backup. " +
			"Stops the owning container (configured via OPS_WOW_VOLUME_OWNERS_JSON), wipes the " +
			"mount dir, untars, restarts. REQUIRES `confirm:true`.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"backup_file":{"type":"string"},
			"confirm":{"type":"boolean"}
		},"required":["backup_file","confirm"]}`),
		Annotations: AnnAction(false),
		Destructive: true,
		Handler: makeVolumeRestoreHandler(deps, "client", deps.VolumeOwners["client"]),
	})
}

// makeVolumeRestoreHandler builds the shared logic for both restore_volume_*
// tools. The owning container is captured at registration time so each tool's
// schema stays single-purpose (no extra `volume` arg from the caller).
//
// The volume backup-file regex is anchored to the SAME logical name as the
// tool's volume so wow_restore_volume_db can never accept a wow-volume-client
// archive (or, worse, a wow-dir-* archive that would scatter unrelated files
// into the freshly-wiped volume mount).
func makeVolumeRestoreHandler(deps WowDeps, volume, owner string) ToolHandler {
	return func(ctx context.Context, raw json.RawMessage, _ string) any {
		var a struct {
			BackupFile string `json:"backup_file"`
			Confirm    bool
		}
		if err := json.Unmarshal(raw, &a); err != nil {
			return map[string]any{"error": "decode args: " + err.Error()}
		}
		if !a.Confirm {
			return map[string]any{"error": "confirm:true required"}
		}
		m := reBackupFilenameVolume.FindStringSubmatch(a.BackupFile)
		if m == nil {
			return map[string]any{"error": "backup_file must be a wow-volume-<name>-<ts>.tar.gz basename"}
		}
		if m[1] != volume {
			return map[string]any{
				"error": fmt.Sprintf("backup_file is for volume %q, this tool restores %q", m[1], volume),
			}
		}
		mount, ok := deps.VolumeMounts[volume]
		if !ok {
			return map[string]any{"error": fmt.Sprintf("volume %q not configured", volume)}
		}
		if owner == "" {
			return map[string]any{"error": fmt.Sprintf("owner container for volume %q not configured", volume)}
		}
		abs := filepath.Join(deps.BackupDir, a.BackupFile)
		if _, err := os.Stat(abs); err != nil {
			return map[string]any{"error": "stat backup: " + err.Error()}
		}
		// Pre-check: container must be stopped, OR we stop it now.
		insp, err := deps.Docker.Inspect(ctx, owner)
		if err != nil {
			return map[string]any{"error": "inspect " + owner + ": " + err.Error()}
		}
		startedRunning := insp.State.Running
		if startedRunning {
			if err := deps.Docker.Stop(ctx, owner, 30); err != nil {
				return map[string]any{"error": "stop " + owner + ": " + err.Error()}
			}
		}
		// Wipe the mount dir, then extract.
		if err := wipeDirContents(mount); err != nil {
			// Try to restart the container before bailing.
			if startedRunning {
				_ = deps.Docker.Start(ctx, owner)
			}
			return map[string]any{"error": "wipe mount: " + err.Error()}
		}
		c, cancel := context.WithTimeout(ctx, wowRestoreTimeout)
		defer cancel()
		n, err := untarGz(c, abs, mount)
		if err != nil {
			if startedRunning {
				_ = deps.Docker.Start(ctx, owner)
			}
			return map[string]any{"error": "untar: " + err.Error()}
		}
		out := map[string]any{
			"ok": true, "file": abs, "extracted_to": mount, "files": n,
			"owner_container": owner, "owner_was_running": startedRunning,
		}
		if startedRunning {
			if err := deps.Docker.Start(ctx, owner); err != nil {
				out["restart_error"] = err.Error()
				out["ok"] = false
			}
		}
		return out
	}
}

// -------------------------- housekeeping --------------------------

func registerWowHousekeepingTools(reg *Registry, deps WowDeps) {
	reg.Register(Tool{
		Name: "wow_list_backups",
		Description: "List wow-*.tar.gz / wow-*.sql.gz files in $OPS_WOW_BACKUP_DIR. Returns " +
			"{files:[{name, size_bytes, modified}, ...]} sorted newest-first. Read-only.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Annotations: AnnRead(),
		Handler: func(_ context.Context, _ json.RawMessage, _ string) any {
			entries, err := os.ReadDir(deps.BackupDir)
			if err != nil {
				return map[string]any{"error": "read dir: " + err.Error()}
			}
			type item struct {
				Name      string `json:"name"`
				SizeBytes int64  `json:"size_bytes"`
				Modified  string `json:"modified"`
			}
			items := []item{}
			for _, e := range entries {
				if e.IsDir() || !reBackupFilename.MatchString(e.Name()) {
					continue
				}
				st, err := e.Info()
				if err != nil {
					continue
				}
				items = append(items, item{
					Name: e.Name(), SizeBytes: st.Size(),
					Modified: st.ModTime().UTC().Format(time.RFC3339),
				})
			}
			sort.Slice(items, func(i, j int) bool { return items[i].Modified > items[j].Modified })
			return map[string]any{"files": items, "count": len(items), "dir": deps.BackupDir}
		},
	})

	reg.Register(Tool{
		Name: "wow_prune_old_backups",
		Description: "Delete wow-*.tar.gz / wow-*.sql.gz files older than `keep_days` days from " +
			"$OPS_WOW_BACKUP_DIR. `keep_days:0` deletes ALL matching files (equivalent to " +
			"master_wow.sh --prune-now). Use `dry_run:true` first to preview the deletion list. " +
			"REQUIRES `confirm:true` when dry_run=false.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"keep_days":{"type":"integer","minimum":0,"description":"Files older than this in days are deleted. 0 = delete ALL matching files. Default 7."},
			"dry_run":{"type":"boolean","description":"Preview without deleting (default false)"},
			"confirm":{"type":"boolean","description":"Required when dry_run=false"}
		}}`),
		Annotations: AnnAction(false),
		Destructive: true,
		Handler: func(_ context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				KeepDays *int `json:"keep_days"`
				DryRun   bool `json:"dry_run"`
				Confirm  bool `json:"confirm"`
			}
			_ = json.Unmarshal(raw, &a)
			keep := 7
			if a.KeepDays != nil {
				if *a.KeepDays < 0 {
					return map[string]any{"error": "keep_days must be >= 0"}
				}
				keep = *a.KeepDays
			}
			if !a.DryRun && !a.Confirm {
				return map[string]any{"error": "confirm:true required when dry_run=false"}
			}
			cutoff := time.Now().Add(-time.Duration(keep) * 24 * time.Hour)
			entries, err := os.ReadDir(deps.BackupDir)
			if err != nil {
				return map[string]any{"error": "read dir: " + err.Error()}
			}
			deleted := []string{}
			var bytesFreed int64
			for _, e := range entries {
				if e.IsDir() || !reBackupFilename.MatchString(e.Name()) {
					continue
				}
				st, err := e.Info()
				if err != nil {
					continue
				}
				if st.ModTime().After(cutoff) {
					continue
				}
				path := filepath.Join(deps.BackupDir, e.Name())
				bytesFreed += st.Size()
				if !a.DryRun {
					if err := os.Remove(path); err != nil {
						continue
					}
				}
				deleted = append(deleted, e.Name())
			}
			return map[string]any{
				"ok": true, "dry_run": a.DryRun, "keep_days": keep,
				"deleted": deleted, "count": len(deleted), "bytes_freed": bytesFreed,
			}
		},
	})
}

// -------------------------- helpers --------------------------

func validAccountField(s string) bool {
	if len(s) < 4 || len(s) > 16 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func validLabel(s string) bool {
	if len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func looksLikeHostname(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	// Permit a single label OR dotted labels. RFC 1123-ish.
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		for _, r := range label {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '-') {
				return false
			}
		}
	}
	return true
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func fileSize(st os.FileInfo) int64 {
	if st == nil {
		return 0
	}
	return st.Size()
}

func execResultToMap(action string, res *dockerlog.ExecResult, err error) map[string]any {
	if err != nil {
		return map[string]any{"error": "exec: " + err.Error(), "action": action}
	}
	out := map[string]any{
		"action":    action,
		"exit_code": res.ExitCode,
		"stdout":    string(res.Stdout),
	}
	if res.ExitCode == 0 {
		out["ok"] = true
	} else {
		out["error"] = fmt.Sprintf("exit %d", res.ExitCode)
		out["stderr"] = string(res.Stderr)
	}
	return out
}

// tarGz tars+gzips srcDir to outFile, excluding any path component in
// `excludeDirs` (matched on segment basename). Returns the bytes written
// to the gzip stream (compressed size on disk).
func tarGz(ctx context.Context, srcDir, outFile string, excludeDirs []string) (int64, error) {
	exMap := map[string]bool{}
	for _, e := range excludeDirs {
		exMap[e] = true
	}
	f, err := os.Create(outFile)
	if err != nil {
		return 0, fmt.Errorf("create %s: %w", outFile, err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	srcResolved, err := filepath.EvalSymlinks(srcDir)
	if err != nil {
		return 0, fmt.Errorf("eval src: %w", err)
	}

	err = filepath.Walk(srcResolved, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		// Skip excluded directories at any depth.
		for seg := range exMap {
			if info.IsDir() && info.Name() == seg {
				return filepath.SkipDir
			}
		}
		rel, err := filepath.Rel(srcResolved, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			if l, err := os.Readlink(path); err == nil {
				link = l
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		fr, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, fr)
		fr.Close()
		return err
	})
	if err != nil {
		return 0, err
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	st, err := os.Stat(outFile)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// untarGz extracts a .tar.gz into destDir. Skips any entry whose normalized
// name escapes destDir (defense against malicious archives). Returns count of
// entries extracted.
func untarGz(ctx context.Context, archive, destDir string) (int, error) {
	f, err := os.Open(archive)
	if err != nil {
		return 0, fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, fmt.Errorf("gunzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return 0, fmt.Errorf("mkdir dest: %w", err)
	}
	// EvalSymlinks (not just filepath.Abs) so the parent-symlink check inside
	// the loop compares apples-to-apples — on macOS /tmp is itself a symlink
	// to /private/tmp, so a bare Abs would diverge from the resolved parent
	// path of every extracted file.
	destResolved, err := filepath.EvalSymlinks(destDir)
	if err != nil {
		return 0, fmt.Errorf("resolve dest %s: %w", destDir, err)
	}
	count := 0
	for {
		select {
		case <-ctx.Done():
			return count, ctx.Err()
		default:
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, err
		}
		clean := filepath.Clean(hdr.Name)
		if clean == "." || clean == "/" {
			continue
		}
		target := filepath.Join(destResolved, clean)
		if !strings.HasPrefix(target, destResolved+string(os.PathSeparator)) && target != destResolved {
			return count, fmt.Errorf("archive entry escapes dest: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return count, err
			}
			// Re-resolve to catch a pre-existing directory-symlink in the
			// destination tree that MkdirAll would have followed (e.g.
			// destDir/foo was a symlink → /etc, and we just created /etc/bar
			// when the archive said foo/bar). EvalSymlinks turns the symlink
			// chain into the real on-disk path; if that real path no longer
			// sits under destResolved, refuse and surface the escape.
			if err := assertUnderDest(target, destResolved, hdr.Name); err != nil {
				return count, err
			}
		case tar.TypeSymlink, tar.TypeLink:
			// Refuse symlinks/hardlinks outright. A symlink whose Linkname
			// points outside destDir would let the next regular-file entry
			// be opened *through* it (TOCTOU): we can validate the textual
			// link target, but a later loop iteration that opens
			// destDir/escape/file resolves the symlink at io.Open time and
			// writes outside dest. Backups produced by tarGz never contain
			// symlinks under the AzerothCore tree we care about; rejecting
			// them is the simplest, complete fix.
			return count, fmt.Errorf("archive contains symlink/hardlink (%s -> %s); refusing to extract for safety",
				hdr.Name, hdr.Linkname)
		case tar.TypeReg:
			parent := filepath.Dir(target)
			if err := os.MkdirAll(parent, 0o755); err != nil {
				return count, err
			}
			// Same parent-symlink check as the directory branch — and it has
			// to run BEFORE we OpenFile, because OpenFile would happily
			// follow a directory symlink in the parent path and write
			// outside destDir. Lstat-then-Remove on the leaf catches the
			// case where the leaf itself is a symlink.
			if err := assertUnderDest(parent, destResolved, hdr.Name); err != nil {
				return count, err
			}
			if st, err := os.Lstat(target); err == nil {
				if st.Mode()&os.ModeSymlink != 0 {
					return count, fmt.Errorf("destination %s is a symlink; refusing to overwrite", hdr.Name)
				}
				if err := os.Remove(target); err != nil {
					return count, err
				}
			}
			// O_EXCL guards the leaf race window between Lstat and OpenFile —
			// if anyone planted a symlink at `target` since our Lstat, the
			// open with O_EXCL will fail with EEXIST.
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, os.FileMode(hdr.Mode))
			if err != nil {
				return count, err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return count, err
			}
			out.Close()
		}
		count++
	}
	return count, nil
}

// assertUnderDest re-evaluates symlinks on `path` and returns an error when
// the resolved on-disk path no longer sits under destResolved. Catches the
// case where MkdirAll followed a pre-existing directory-symlink in the
// extraction root (e.g. WowRoot/foo was a symlink to /etc, so MkdirAll
// happily produced /etc/bar when the archive said foo/bar).
//
// destResolved must already be EvalSymlinks-resolved by the caller (untarGz
// does this once at the top). entryName is the archive entry name, used only
// for the error message.
func assertUnderDest(path, destResolved, entryName string) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve %s for entry %s: %w", path, entryName, err)
	}
	if resolved != destResolved &&
		!strings.HasPrefix(resolved, destResolved+string(os.PathSeparator)) {
		return fmt.Errorf("archive entry %s resolved to %s which escapes dest %s",
			entryName, resolved, destResolved)
	}
	return nil
}

// wipeDirContents removes every entry inside dir but leaves dir itself in place.
// Used by restore_volume_* before extracting the archive.
func wipeDirContents(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Compile-time assertion that *dockerlog.Client satisfies DockerExecer.
var _ DockerExecer = (*dockerlog.Client)(nil)
