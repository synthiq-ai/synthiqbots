package mcpserver

import "encoding/json"

// argsRedactionByTool is the per-tool list of argument keys whose values
// must be replaced with "***REDACTED***" before the args land in the
// admin audit row.
//
// Tools that take secrets in arguments (passwords, mysql credentials)
// register here. The redaction runs against the parsed JSON object, so
// keys are matched case-sensitively against the JSON-tag-resolved name
// the schema advertises (e.g. `db_pass` not `DBPass`).
var argsRedactionByTool = map[string][]string{
	"wow_create_account": {"password"},
	"wow_set_gm_level":   {},
	"wow_backup_db":      {"db_pass"},
	"wow_restore_db":     {"db_pass"},
}

// redactArgsForAudit returns a copy of `args` with each key listed in the
// per-tool redaction map replaced with "***REDACTED***". Returns the input
// untouched when the tool has no redaction entry or the args don't decode
// as a JSON object — auditing must never fail because of this helper.
//
// Mirrors the redactDocker pattern in tools_container.go but operates on
// MCP request arguments rather than container env arrays.
func redactArgsForAudit(tool string, args json.RawMessage) json.RawMessage {
	keys, ok := argsRedactionByTool[tool]
	if !ok || len(keys) == 0 || len(args) == 0 {
		return args
	}
	var obj map[string]any
	if err := json.Unmarshal(args, &obj); err != nil {
		return args
	}
	for _, k := range keys {
		if _, present := obj[k]; present {
			obj[k] = "***REDACTED***"
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return args
	}
	return out
}
