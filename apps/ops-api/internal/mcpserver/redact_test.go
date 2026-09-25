package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactDockerEnv(t *testing.T) {
	denied := []string{"*Token*", "*Password*", "*Passwd*", "*Secret*", "*BearerToken*", "*ApiKey*", "*Api_Key*", "*DSN*", "*Database*", "*Credential*"}
	raw := `{
		"Config": {
			"Env": [
				"OPS_BEARER_TOKEN=hunter2",
				"DB_DSN=user:pw@tcp(h)/db",
				"PATH=/usr/bin",
				"MCP_BEARER_TOKEN=abc",
				"OPS_ADMIN_DENIED_KEYS=*Token*",
				"DATABASE_INFO=secret"
			],
			"Labels": {
				"com.example.token": "leaks",
				"com.example.harmless": "ok"
			}
		}
	}`
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	redactDocker(doc, denied)
	out, _ := json.Marshal(doc)
	s := string(out)
	if strings.Contains(s, "hunter2") {
		t.Errorf("OPS_BEARER_TOKEN value leaked: %s", s)
	}
	if strings.Contains(s, "user:pw@tcp") {
		t.Errorf("DB_DSN value leaked (Password pattern should match): %s", s)
	}
	if !strings.Contains(s, "PATH=/usr/bin") {
		t.Errorf("non-secret env was redacted: %s", s)
	}
	if !strings.Contains(s, "DATABASE_INFO=***REDACTED***") {
		t.Errorf("DATABASE_INFO should match *DatabaseInfo* (case-insensitive). Output: %s", s)
	}
	if strings.Contains(s, "abc") {
		t.Errorf("MCP_BEARER_TOKEN value leaked: %s", s)
	}
	if !strings.Contains(s, "***REDACTED***") {
		t.Errorf("expected redaction marker in output: %s", s)
	}
}

func TestRedactDockerNilSafe(t *testing.T) {
	redactDocker(nil, []string{"*"}) // must not panic
	redactDocker(map[string]any{}, []string{"*"})
	redactDocker(map[string]any{"Config": nil}, []string{"*"})
}

func TestForbiddenSchemaRegex(t *testing.T) {
	hits := []string{
		"SELECT * FROM mysql.user",
		"select 1 from `mysql`.user",
		"SELECT 1 FROM information_schema.tables",
		"select * from performance_schema.events_statements_history",
		"SELECT version FROM sys.version",
	}
	for _, s := range hits {
		if !reForbiddenSchema.MatchString(s) {
			t.Errorf("expected forbidden-schema match: %q", s)
		}
	}
	misses := []string{
		"SELECT 1 FROM acore_characters.characters",
		"SELECT 1",
		"SHOW TABLES",
		"SELECT 1 FROM `acore_world`.creature_template",
	}
	for _, s := range misses {
		if reForbiddenSchema.MatchString(s) {
			t.Errorf("forbidden-schema regex matched safe SQL: %q", s)
		}
	}
}
