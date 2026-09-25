package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// callReset registers the tool against a mock ExecDB and invokes the handler.
func callReset(t *testing.T, deps DBDeps, args map[string]any) map[string]any {
	t.Helper()
	reg := NewRegistry()
	RegisterWowResetCharacterTool(reg, deps)
	tool, ok := reg.Get("wow_reset_character")
	if !ok {
		t.Fatal("wow_reset_character not registered")
	}
	raw, _ := json.Marshal(args)
	res, ok := tool.Handler(context.Background(), raw, "test").(map[string]any)
	if !ok {
		t.Fatalf("handler did not return map[string]any")
	}
	return res
}

func TestWowResetCharacter_Validation(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string // substring expected in the error
	}{
		{"no identifier", map[string]any{"confirm": true}, "either name or guid"},
		{"both identifiers", map[string]any{"guid": 1, "name": "x", "confirm": true}, "not both"},
		{"missing confirm", map[string]any{"guid": 1}, "confirm:true required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// ExecDB present so we get past the nil-pool guard to the arg checks.
			db, _, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()
			res := callReset(t, DBDeps{ExecDB: db}, tc.args)
			errStr, _ := res["error"].(string)
			if errStr == "" || !regexp.MustCompile(regexp.QuoteMeta(tc.want)).MatchString(errStr) {
				t.Fatalf("want error containing %q, got %+v", tc.want, res)
			}
		})
	}
}

func TestWowResetCharacter_RefusesOnline(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM characters WHERE guid = ?")).
		WithArgs(int64(20007)).
		WillReturnRows(sqlmock.NewRows(
			[]string{"guid", "account", "name", "race", "class", "level", "online"}).
			AddRow(20007, 1001, "Claude", 10, 2, 12, 1)) // online = 1

	res := callReset(t, DBDeps{ExecDB: db}, map[string]any{"guid": 20007, "confirm": true})
	if errStr, _ := res["error"].(string); errStr == "" ||
		!regexp.MustCompile("online").MatchString(errStr) {
		t.Fatalf("expected online-refusal error, got %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock: %v", err)
	}
}

func TestWowResetCharacter_DryRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM characters WHERE guid = ?")).
		WithArgs(int64(20005)).
		WillReturnRows(sqlmock.NewRows(
			[]string{"guid", "account", "name", "race", "class", "level", "online"}).
			AddRow(20005, 1001, "Dia", 1, 1, 3, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM acore_world.playercreateinfo WHERE race = ? AND class = ?")).
		WithArgs(1, 1).
		WillReturnRows(sqlmock.NewRows(
			[]string{"map", "zone", "position_x", "position_y", "position_z", "orientation"}).
			AddRow(0, 12, -8949.95, -132.49, 83.53, 0.0))

	// One COUNT(*) per wipe table, then item_instance, mail_items, mail.
	for _, tbl := range characterWipeTables {
		mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM `" + tbl.table + "`")).
			WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	}
	for _, tbl := range []string{"item_instance", "mail_items", "mail"} {
		mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*) FROM `" + tbl + "`")).
			WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	}

	res := callReset(t, DBDeps{ExecDB: db}, map[string]any{"guid": 20005, "dry_run": true})
	if res["dry_run"] != true {
		t.Fatalf("expected dry_run=true, got %+v", res)
	}
	if res["error"] != nil {
		t.Fatalf("unexpected error: %+v", res["error"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock: %v", err)
	}
}

func TestWowResetCharacter_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta("USE `acore_characters`")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM characters WHERE name = ?")).
		WithArgs("Dia").
		WillReturnRows(sqlmock.NewRows(
			[]string{"guid", "account", "name", "race", "class", "level", "online"}).
			AddRow(20005, 1001, "Dia", 1, 1, 3, 0))
	mock.ExpectQuery(regexp.QuoteMeta("FROM acore_world.playercreateinfo")).
		WithArgs(1, 1).
		WillReturnRows(sqlmock.NewRows(
			[]string{"map", "zone", "position_x", "position_y", "position_z", "orientation"}).
			AddRow(0, 12, -8949.95, -132.49, 83.53, 0.0))

	mock.ExpectBegin()
	for _, tbl := range characterWipeTables {
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM `" + tbl.table + "`")).
			WithArgs(int64(20005)).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	for _, tbl := range []string{"item_instance", "mail_items", "mail"} {
		mock.ExpectExec(regexp.QuoteMeta("DELETE FROM `" + tbl + "`")).
			WithArgs(int64(20005)).
			WillReturnResult(sqlmock.NewResult(0, 0))
	}
	mock.ExpectExec(regexp.QuoteMeta("UPDATE characters SET")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	res := callReset(t, DBDeps{ExecDB: db}, map[string]any{"name": "Dia", "confirm": true})
	if res["ok"] != true {
		t.Fatalf("expected ok=true, got %+v", res)
	}
	if res["previous_level"] != 3 {
		t.Errorf("expected previous_level=3, got %v", res["previous_level"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock: %v", err)
	}
}
