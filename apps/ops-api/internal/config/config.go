package config

import (
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// jsonUnmarshal is a tiny indirection so the helpers don't need to know about
// the JSON package directly — keeps the env-parsing surface focused.
func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

type Config struct {
	BindAddr             string
	OpsBearerToken       string
	MCPBearerToken       string
	MCPURL               string
	WorldserverContainer string
	DBDSN                string
	FileRoots            map[string]string
	LogLevel             string
	RequestTimeout       time.Duration
	StreamTimeout        time.Duration

	// Admin MCP
	DBExecDSN             string              // ops_rw user DSN; empty => db_exec returns "not configured"
	AdminAllowActions     bool                // master gate for state-changing tools
	AdminMcpFacades       bool                // advertise consolidated facade tools (OPS_MCP_FACADES=0 to serve the flat list)
	AdminContainerDefault string              // default container name for container_* tools
	AdminWriteGlobs       map[string][]string // root → list of glob patterns allowed for file_write
	AdminAllowedFiles     []string            // basenames allowed for file_set_key
	AdminAllowedKeys      map[string][]string // basename → list of key globs editable
	AdminDeniedKeys       []string            // always-deny key patterns (Token/Password/etc)
	AdminBackupDir        string              // base dir for timestamped .bak copies
	AdminConfigRoot       string              // dir under which AdminAllowedFiles resolve
	AdminRateLimits       map[string]int      // tool name → calls/minute/IP

	// Feedback ingest (POST /v1/feedback) — see docs/feedback-loop.md
	FeedbackStorageDir string // base dir for screenshot files; empty => endpoint 503s
	FeedbackMaxImageMB int    // multipart image size cap (MiB); default 12
	FeedbackMaxMetaKB  int    // metadata JSON cap (KiB); default 256

	// master_wow.sh / git tooling — see docs/admin-mcp.md "wow_* tool family".
	// Both default paths assume the canonical mod-ollama-chat compose mount layout.
	WowRoot           string            // /wow-root → /srv/wow/docker/wow/azerothcore-wotlk
	WowBackupDir      string            // /backups → /opt/backups/wow on the host
	WowDBContainer    string            // ac-database
	WowWorldContainer string            // ac-worldserver
	WowVolumes        map[string]string // logical name → docker volume name (db / client) — for tool output annotation
	WowVolumeMounts   map[string]string // logical name → in-container path the docker volume is bind-mounted at
	WowVolumeOwners   map[string]string // logical name → container that mounts it (used by wow_restore_volume_*)
	WowModulePattern  string            // regex restricting mod-* names for git pull / sql import
	WowDBAuthDSN      string            // ops_rw DSN bound to acore_auth (realm tools)
	WowImportDSN      string            // ops_rw DSN with multiStatements=true (sql import); falls back to DBExecDSN

	// Feedback dispatch worker (PR 4 of feedback loop). Drains pending
	// rows in mod_ollama_chat_feedback, sends each to a vision-capable
	// Synthiq agent, persists the response. Disabled by default — set
	// FeedbackDispatchEnable=1 + the URL/Bearer/Model trio to start.
	FeedbackDispatchEnable     bool
	FeedbackDispatchURL        string // OpenAI-compatible chat-completions base
	FeedbackDispatchBearer     string
	FeedbackDispatchModel      string // e.g. claude-sonnet-4-6 (vision-capable)
	FeedbackDispatchPollSec    int    // default 30
	FeedbackDispatchStaleSec   int    // default 600 (rewind dispatched rows older than this)
	FeedbackDispatchMaxImageMB int    // default 16 (per-row image cap)

	// Capture-delivery substrate (Phase 0 of the screenshot/video feature).
	// Hosts rendered tactical maps and observer screenshots/clips, returns a
	// clickable URL, and pushes the artifact out-of-band. See docs/capture.md.
	ArtifactDir     string // base dir for hosted artifacts; empty => artifact endpoints 503
	ArtifactBaseURL string // public base for the returned URL, e.g. https://ops.wow.example.com
	ArtifactMaxMB   int    // upload/serve size cap (MiB); default 25

	// Out-of-band push. Each channel is enabled iff its token is set; both may
	// be empty (then a capture just returns the URL with pushed:[]).
	TelegramBotToken string // OPS_TELEGRAM_BOT_TOKEN
	TelegramChatID   string // OPS_TELEGRAM_CHAT_ID (numeric id or @channelusername)
	SlackBotToken    string // OPS_SLACK_BOT_TOKEN (xoxb-…, needs files:write)
	SlackChannel     string // OPS_SLACK_CHANNEL (channel id, e.g. C0123ABCD)

	// Observer screenshots (Phase 2). request_observer_screenshot whispers a
	// command to an in-world GM observer character (via the gameplay
	// whisper_player tool), then waits for the feedback-daemon to ship the shot.
	ObserverChar          string // OPS_OBSERVER_CHAR; empty => the tool refuses (feature off)
	ObserverWhisperBotGID uint64 // OPS_OBSERVER_WHISPER_BOT_GUID; bot that sends the whisper (default 20007, leader)
	ObserverTTLSec        int    // OPS_OBSERVER_TTL_SEC; how long to wait for a shot (default 90)

	// Observer video clips (Phase 3). request_observer_clip returns immediately
	// (recording outruns the MCP transport timeout) and the daemon ships an MP4;
	// get_observer_clip fetches the URL afterwards.
	ObserverClipTTLSec int // OPS_OBSERVER_CLIP_TTL_SEC; queue lifetime for a clip (default 180)
	ObserverClipMaxSec int // OPS_OBSERVER_CLIP_MAX_SEC; longest clip a caller may request (default 30)
	ClipMaxMB          int // OPS_CLIP_MAX_MB; clip upload size cap (MiB); default 100
}

func Load() (*Config, error) {
	c := &Config{
		BindAddr:             getEnv("OPS_API_LISTEN", ":8080"),
		OpsBearerToken:       os.Getenv("OPS_BEARER_TOKEN"),
		MCPBearerToken:       os.Getenv("MCP_BEARER_TOKEN"),
		MCPURL:               getEnv("MCP_URL", "http://host.docker.internal:18790"),
		WorldserverContainer: getEnv("WORLDSERVER_CONTAINER", "ac-worldserver"),
		DBDSN:                os.Getenv("DB_DSN"),
		// /logs is mounted read-only to expose worldserver's application-level
		// log files (Server.log, Errors.log, Playerbots.log) for forensic reads
		// via ops_files_{list,read,search}. The bind-mount is opt-in on the
		// compose side — without it the resolver simply reports the root as
		// unavailable, no startup error.
		FileRoots:             parseRoots(getEnv("OPS_FILE_ROOTS", "/repo,/configs,/logs")),
		LogLevel:              getEnv("LOG_LEVEL", "info"),
		RequestTimeout:        30 * time.Second,
		StreamTimeout:         5 * time.Minute,
		DBExecDSN:             os.Getenv("DB_EXEC_DSN"),
		AdminAllowActions:     os.Getenv("OPS_ADMIN_ALLOW_ACTIONS") != "0",
		AdminMcpFacades:       os.Getenv("OPS_MCP_FACADES") != "0",
		AdminContainerDefault: getEnv("OPS_ADMIN_CONTAINER_DEFAULT", "ac-worldserver"),
		AdminBackupDir:        getEnv("OPS_ADMIN_BACKUP_DIR", "/backups/wow-conf"),
		AdminConfigRoot:       getEnv("OPS_ADMIN_CONFIG_ROOT", "/configs"),
		AdminAllowedFiles:     splitCSV(os.Getenv("OPS_ADMIN_ALLOWED_FILES")),
		// Deny patterns are lowercased + glob-matched against lowercased keys
		// (case-insensitive). Cover both TitleCase (.conf keys like
		// `OllamaChat.Mcp.BearerToken`) AND SCREAMING_SNAKE (env vars like
		// `DB_DSN`, `DATABASE_INFO`) conventions — the underscores in env keys
		// mean a `*DatabaseInfo*` pattern doesn't match `DATABASE_INFO`, so we
		// add `*Database*` and `*DSN*` etc explicitly.
		AdminDeniedKeys: splitCSVDefault(os.Getenv("OPS_ADMIN_DENIED_KEYS"),
			"*Token*,*Password*,*Passwd*,*Secret*,*BearerToken*,*ApiKey*,*Api_Key*,*DSN*,*Database*,*Credential*"),
		AdminWriteGlobs:    parseRootGlobs(os.Getenv("OPS_ADMIN_WRITE_GLOBS")),
		AdminAllowedKeys:   parseAllowedKeysJSON(os.Getenv("OPS_ADMIN_ALLOWED_KEYS_JSON")),
		AdminRateLimits:    parseRateLimitsJSON(os.Getenv("OPS_ADMIN_RATE_LIMITS_JSON")),
		FeedbackStorageDir: os.Getenv("OPS_FEEDBACK_STORAGE_DIR"),
		FeedbackMaxImageMB: atoiDefault(os.Getenv("OPS_FEEDBACK_MAX_IMAGE_MB"), 12),
		FeedbackMaxMetaKB:  atoiDefault(os.Getenv("OPS_FEEDBACK_MAX_META_KB"), 256),

		FeedbackDispatchEnable:     os.Getenv("FEEDBACK_DISPATCH_ENABLE") == "1",
		FeedbackDispatchURL:        os.Getenv("FEEDBACK_DISPATCH_URL"),
		FeedbackDispatchBearer:     os.Getenv("FEEDBACK_DISPATCH_BEARER"),
		FeedbackDispatchModel:      os.Getenv("FEEDBACK_DISPATCH_MODEL"),
		FeedbackDispatchPollSec:    atoiDefault(os.Getenv("FEEDBACK_DISPATCH_POLL_SEC"), 30),
		FeedbackDispatchStaleSec:   atoiDefault(os.Getenv("FEEDBACK_DISPATCH_STALE_SEC"), 600),
		FeedbackDispatchMaxImageMB: atoiDefault(os.Getenv("FEEDBACK_DISPATCH_MAX_IMAGE_MB"), 16),

		WowRoot:           getEnv("OPS_WOW_ROOT", "/wow-root"),
		WowBackupDir:      getEnv("OPS_WOW_BACKUP_DIR", "/backups"),
		WowDBContainer:    getEnv("OPS_DB_CONTAINER", "ac-database"),
		WowWorldContainer: getEnv("OPS_WORLD_CONTAINER", "ac-worldserver"),
		// Volume names follow the docker-compose v2 prefix scheme:
		// `<project>_<service-volume>`. The defaults match the production
		// stack on game-host (project = "azerothcore-wotlk"). Override with
		// OPS_WOW_VOLUMES_JSON if a different compose project is in use.
		WowVolumes: parseWowVolumesJSON(getEnv("OPS_WOW_VOLUMES_JSON",
			`{"db":"azerothcore-wotlk_ac-database-data","client":"azerothcore-wotlk_ac-client-data"}`)),
		// In-container paths the docker volumes are bind-mounted at. Compose
		// must add `azerothcore-wotlk_ac-database-data:/volumes/db` and
		// `azerothcore-wotlk_ac-client-data:/volumes/client` to ops-api for
		// wow_backup_volumes / wow_restore_volume_* to work — otherwise those
		// tools refuse with "volume mount not present in container".
		WowVolumeMounts: parseWowVolumesJSON(getEnv("OPS_WOW_VOLUME_MOUNTS_JSON",
			`{"db":"/volumes/db","client":"/volumes/client"}`)),
		// Container that owns each volume. wow_restore_volume_* stops the owner
		// before wiping the volume contents (a live mysql process holding open
		// file handles into the volume would corrupt the restore).
		WowVolumeOwners: parseWowVolumesJSON(getEnv("OPS_WOW_VOLUME_OWNERS_JSON",
			`{"db":"ac-database","client":"ac-worldserver"}`)),
		WowModulePattern: getEnv("OPS_MODULE_NAME_PATTERN", `^mod-[a-z0-9-]+$`),
		WowDBAuthDSN:     os.Getenv("OPS_DB_AUTH_DSN"),
		WowImportDSN:     os.Getenv("OPS_DB_IMPORT_DSN"),

		ArtifactDir:     os.Getenv("OPS_ARTIFACT_DIR"),
		ArtifactBaseURL: strings.TrimRight(getEnv("OPS_ARTIFACT_BASE_URL", "https://ops.wow.example.com"), "/"),
		ArtifactMaxMB:   atoiDefault(os.Getenv("OPS_ARTIFACT_MAX_MB"), 25),

		TelegramBotToken: os.Getenv("OPS_TELEGRAM_BOT_TOKEN"),
		TelegramChatID:   os.Getenv("OPS_TELEGRAM_CHAT_ID"),
		SlackBotToken:    os.Getenv("OPS_SLACK_BOT_TOKEN"),
		SlackChannel:     os.Getenv("OPS_SLACK_CHANNEL"),

		ObserverChar:          os.Getenv("OPS_OBSERVER_CHAR"),
		ObserverWhisperBotGID: atou64Default(os.Getenv("OPS_OBSERVER_WHISPER_BOT_GUID"), 20007),
		ObserverTTLSec:        atoiDefault(os.Getenv("OPS_OBSERVER_TTL_SEC"), 90),

		ObserverClipTTLSec: atoiDefault(os.Getenv("OPS_OBSERVER_CLIP_TTL_SEC"), 180),
		ObserverClipMaxSec: atoiDefault(os.Getenv("OPS_OBSERVER_CLIP_MAX_SEC"), 30),
		ClipMaxMB:          atoiDefault(os.Getenv("OPS_CLIP_MAX_MB"), 100),
	}
	if c.OpsBearerToken == "" {
		return nil, errors.New("OPS_BEARER_TOKEN is required")
	}
	return c, nil
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func splitCSVDefault(s, def string) []string {
	if s == "" {
		s = def
	}
	return splitCSV(s)
}

// parseRootGlobs parses "root1=glob1|glob2;root2=glob3" into a map.
// Example: "configs=*.conf|*.conf.dist;repo=apps/ops-api/**".
func parseRootGlobs(s string) map[string][]string {
	out := map[string][]string{}
	if s == "" {
		return out
	}
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eq := strings.Index(part, "=")
		if eq < 1 {
			continue
		}
		root := strings.TrimSpace(part[:eq])
		globs := splitOnPipe(part[eq+1:])
		if len(globs) > 0 {
			out[root] = globs
		}
	}
	return out
}

func splitOnPipe(s string) []string {
	parts := strings.Split(s, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// parseAllowedKeysJSON expects a JSON object: {"file.conf":["Glob.*","OtherKey"]}.
func parseAllowedKeysJSON(s string) map[string][]string {
	out := map[string][]string{}
	if s == "" {
		return out
	}
	if err := jsonUnmarshal(s, &out); err != nil {
		// Soft-fail: log via stderr indirectly by leaving the map empty (admin
		// will see no allowlist and writes will be rejected).
		return map[string][]string{}
	}
	return out
}

// parseRateLimitsJSON expects a JSON object: {"db_query":30,"db_exec":5}.
func parseRateLimitsJSON(s string) map[string]int {
	out := map[string]int{}
	if s == "" {
		return out
	}
	if err := jsonUnmarshal(s, &out); err != nil {
		return map[string]int{}
	}
	return out
}

// parseWowVolumesJSON expects {"db":"vol-name","client":"other-vol"}.
// Soft-fails to an empty map (wow_backup_volumes / wow_restore_volume_*
// will then refuse with a clear error rather than crashing at startup).
func parseWowVolumesJSON(s string) map[string]string {
	out := map[string]string{}
	if s == "" {
		return out
	}
	if err := jsonUnmarshal(s, &out); err != nil {
		return map[string]string{}
	}
	return out
}

func parseRoots(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// Bare path → derive label from basename. "label=path" → explicit label.
		if i := strings.Index(part, "="); i > 0 {
			out[part[:i]] = part[i+1:]
		} else {
			label := strings.TrimPrefix(part, "/")
			if label == "" {
				continue
			}
			out[label] = part
		}
	}
	return out
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func atou64Default(s string, def uint64) uint64 {
	if s == "" {
		return def
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n == 0 {
		return def
	}
	return n
}
