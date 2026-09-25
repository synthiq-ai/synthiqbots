package mcpserver

import "testing"

// These regexes are the gateway between safe and unsafe SQL — exercise the
// handful of edge cases that have bitten before.
func TestSQLAllowlistRegexes(t *testing.T) {
	read := []string{
		"SELECT 1",
		"  select * from t",
		"SHOW TABLES",
		"EXPLAIN SELECT 1",
		"DESCRIBE t",
		"DESC t",
	}
	for _, s := range read {
		if !reReadStatement.MatchString(s) {
			t.Errorf("read regex did not match: %q", s)
		}
	}
	notRead := []string{"INSERT INTO t VALUES (1)", "UPDATE t SET x=1", "DELETE FROM t", "TRUNCATE t"}
	for _, s := range notRead {
		if reReadStatement.MatchString(s) {
			t.Errorf("read regex matched non-read: %q", s)
		}
	}

	write := []string{
		"INSERT INTO t VALUES (1)",
		"  update t set x=1 where id=1",
		"DELETE FROM t WHERE id=1",
	}
	for _, s := range write {
		if !reWriteStatement.MatchString(s) {
			t.Errorf("write regex did not match: %q", s)
		}
	}

	banned := []string{
		"SELECT * FROM t; DROP TABLE u",
		"TRUNCATE TABLE t",
		"ALTER TABLE t ADD COLUMN x INT",
		"CREATE TABLE t (id INT)",
		"GRANT SELECT ON t TO u",
		"REVOKE SELECT ON t FROM u",
	}
	for _, s := range banned {
		if !reBannedWrite.MatchString(s) {
			t.Errorf("banned regex did not match: %q", s)
		}
	}
}

func TestUpdateDeleteRequireWhere(t *testing.T) {
	cases := []struct {
		sql     string
		isMod   bool
		hasWhere bool
	}{
		{"UPDATE t SET x=1", true, false},                 // BAD
		{"DELETE FROM t", true, false},                    // BAD
		{"UPDATE t SET x=1 WHERE id=1", true, true},        // GOOD
		{"DELETE FROM t WHERE id=1", true, true},           // GOOD
		{"INSERT INTO t VALUES (1)", false, false},         // INSERT skips this guard
		{"SELECT 1", false, false},                         // SELECT skips this guard
	}
	for _, c := range cases {
		gotMod := reUpdateOrDelete.MatchString(c.sql)
		if gotMod != c.isMod {
			t.Errorf("reUpdateOrDelete(%q)=%v want %v", c.sql, gotMod, c.isMod)
		}
		if !gotMod {
			continue
		}
		gotWhere := reHasWhere.MatchString(c.sql)
		if gotWhere != c.hasWhere {
			t.Errorf("reHasWhere(%q)=%v want %v", c.sql, gotWhere, c.hasWhere)
		}
	}
}
