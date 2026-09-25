package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactArgsForAudit(t *testing.T) {
	cases := []struct {
		name      string
		tool      string
		args      string
		mustHide  []string
		mustKeep  []string
	}{
		{
			name:     "wow_create_account redacts password",
			tool:     "wow_create_account",
			args:     `{"username":"alice","password":"hunter2","confirm":true}`,
			mustHide: []string{"hunter2"},
			mustKeep: []string{"alice", "REDACTED"},
		},
		{
			name:     "wow_backup_db redacts db_pass but keeps db_user",
			tool:     "wow_backup_db",
			args:     `{"database":"acore_world","db_user":"root","db_pass":"sup3r","confirm":true}`,
			mustHide: []string{"sup3r"},
			mustKeep: []string{"root", "acore_world", "REDACTED"},
		},
		{
			name:     "wow_restore_db redacts db_pass",
			tool:     "wow_restore_db",
			args:     `{"backup_file":"wow-acore_world-20260101-000000.sql.gz","database":"acore_world","db_pass":"x"}`,
			mustHide: []string{`"db_pass":"x"`},
			mustKeep: []string{"REDACTED"},
		},
		{
			name:     "unrelated tool unchanged",
			tool:     "wow_check_realmlist",
			args:     `{"foo":"bar"}`,
			mustHide: nil,
			mustKeep: []string{"bar"},
		},
		{
			name:     "wow_set_gm_level has empty redaction list — args untouched",
			tool:     "wow_set_gm_level",
			args:     `{"username":"u1","level":4,"confirm":true}`,
			mustHide: nil,
			mustKeep: []string{"u1"},
		},
	}
	for _, tc := range cases {
		out := redactArgsForAudit(tc.tool, json.RawMessage(tc.args))
		s := string(out)
		for _, hide := range tc.mustHide {
			if strings.Contains(s, hide) {
				t.Errorf("%s: secret %q leaked in output: %s", tc.name, hide, s)
			}
		}
		for _, keep := range tc.mustKeep {
			if !strings.Contains(s, keep) {
				t.Errorf("%s: expected substring %q in output: %s", tc.name, keep, s)
			}
		}
	}
}

func TestRedactArgsForAudit_MalformedArgs(t *testing.T) {
	// Invalid JSON: must not panic, must return original unchanged.
	out := redactArgsForAudit("wow_create_account", json.RawMessage(`not json`))
	if string(out) != "not json" {
		t.Errorf("malformed args were modified: %s", out)
	}
	// Empty args: must not panic.
	out = redactArgsForAudit("wow_create_account", nil)
	if len(out) != 0 {
		t.Errorf("empty args modified: %s", out)
	}
}
