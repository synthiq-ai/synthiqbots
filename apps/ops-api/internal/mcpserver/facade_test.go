package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// fakeReg builds a registry with a representative slice of the real tool
// names — one from every classification branch, including the three that have
// historically been easy to misroute (wow_list_backups and
// wow_prune_old_backups look like player tools; worldserver_health carries no
// wow_ prefix at all).
func fakeReg(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry()
	names := []string{
		"ah_market_summary", "ah_undercutters",
		"db_query", "db_exec", "db_redundant_indexes",
		"container_list", "container_restart",
		"ops_status", "ops_logs_tail", "ops_files_read",
		"file_write", "git_pull_all", "sql_import_file",
		"request_observer_clip", "get_observer_clip", "render_tactical_map",
		"wow_backup_db", "wow_restore_db", "wow_list_backups", "wow_prune_old_backups",
		"wow_guild_roster", "wow_guild_bank_activity", "wow_guild_membership_churn",
		"wow_player_lookup", "wow_create_account", "wow_mail_summary", "wow_set_gm_level",
		"wow_bot_lookup", "wow_bots_fleet_status",
		"wow_check_realmlist", "wow_update_realm_ip", "wow_events_timeline",
		"worldserver_health",
	}
	destructive := map[string]bool{
		"db_exec": true, "container_restart": true, "file_write": true,
		"git_pull_all": true, "sql_import_file": true, "wow_restore_db": true,
		"wow_create_account": true, "wow_set_gm_level": true, "wow_update_realm_ip": true,
		"wow_prune_old_backups": true,
	}
	for _, n := range names {
		n := n
		r.Register(Tool{
			Name:        n,
			Description: "desc " + n,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer"}}}`),
			Destructive: destructive[n],
			Handler: func(_ context.Context, args json.RawMessage, _ string) any {
				return map[string]any{"called": n, "args": string(args)}
			},
		})
	}
	return r
}

func TestInstallFacadesFoldsEveryTool(t *testing.T) {
	r := fakeReg(t)
	total := r.Len()

	facades, folded := InstallFacades(r)
	if folded != total {
		t.Fatalf("folded %d of %d tools — some tool was left unclassified", folded, total)
	}
	if facades == 0 {
		t.Fatal("no facades installed")
	}
	// Every original must survive dispatch and vanish from advertisement.
	for _, n := range []string{"ah_market_summary", "wow_list_backups", "worldserver_health"} {
		tool, ok := r.Get(n)
		if !ok {
			t.Fatalf("%s no longer dispatchable", n)
		}
		if !tool.Hidden {
			t.Fatalf("%s still advertised", n)
		}
	}
	for _, n := range r.AdvertisedSorted() {
		tool, _ := r.Get(n)
		if !tool.IsFacade {
			t.Fatalf("advertised non-facade tool %q", n)
		}
	}
}

func TestClassifyRoutesAmbiguousNames(t *testing.T) {
	cases := map[string]string{
		"wow_list_backups":      "wow_backup",
		"wow_prune_old_backups": "wow_backup",
		"wow_restore_volume_db": "wow_backup",
		"worldserver_health":    "wow_realm",
		"wow_check_realmlist":   "wow_realm",
		"wow_events_timeline":   "wow_realm",
		"wow_bots_fleet_status": "wow_bots",
		"wow_bot_lookup":        "wow_bots",
		"wow_mail_summary":      "wow_player",
		"wow_guild_roster":      "wow_guild",
		"render_tactical_map":   "observer",
		"get_observer_clip":     "observer",
		"sql_import_file":       "deploy",
	}
	for name, want := range cases {
		if got := classify(name); got != want {
			t.Errorf("classify(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestActionNamesAreUniqueAndStripped(t *testing.T) {
	r := fakeReg(t)
	InstallFacades(r)

	f, ok := r.Get("wow_backup")
	if !ok {
		t.Fatal("wow_backup facade missing")
	}
	// wow_ stripped, and no two actions collide onto one target.
	if target, ok := f.Actions["backup_db"]; !ok || target != "wow_backup_db" {
		t.Fatalf("wow_backup actions = %v, want backup_db -> wow_backup_db", f.Actions)
	}
	seen := map[string]string{}
	for _, n := range r.AdvertisedSorted() {
		fa, _ := r.Get(n)
		for action, target := range fa.Actions {
			key := n + "/" + action
			if prev, dup := seen[target]; dup {
				t.Errorf("tool %q reachable via both %q and %q", target, prev, key)
			}
			seen[target] = key
		}
	}
}

func TestFacadeAnnotationsNotReadOnlyWhenAnyActionDestructive(t *testing.T) {
	r := fakeReg(t)
	InstallFacades(r)

	// db holds db_exec (destructive) — must not claim readOnlyHint.
	dbF, _ := r.Get("db")
	if strings.Contains(string(dbF.Annotations), `"readOnlyHint":true`) {
		t.Errorf("db facade advertises readOnlyHint:true but contains db_exec")
	}
	// ah is entirely read-only.
	ahF, _ := r.Get("ah")
	if !strings.Contains(string(ahF.Annotations), `"readOnlyHint":true`) {
		t.Errorf("ah facade should be read-only, got %s", ahF.Annotations)
	}
}

func TestResolveFacadeDispatchAndErrors(t *testing.T) {
	r := fakeReg(t)
	InstallFacades(r)
	f, _ := r.Get("ah")

	target, name, args, describeOnly, _, err := r.ResolveFacade(f, json.RawMessage(`{"action":"market_summary","params":{"limit":5}}`))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if name != "ah_market_summary" || describeOnly {
		t.Fatalf("resolved to %q describeOnly=%v", name, describeOnly)
	}
	if string(args) != `{"limit":5}` {
		t.Fatalf("params not passed through verbatim: %s", args)
	}
	if target.Handler == nil {
		t.Fatal("resolved target has no handler")
	}

	// Missing params must still hand the handler a valid empty object, not nil.
	_, _, args, _, _, err = r.ResolveFacade(f, json.RawMessage(`{"action":"market_summary"}`))
	if err != nil || string(args) != `{}` {
		t.Fatalf("empty params = %q err=%v, want {}", args, err)
	}

	// describe returns the schema and executes nothing.
	_, name, _, describeOnly, schema, err := r.ResolveFacade(f, json.RawMessage(`{"describe":"undercutters"}`))
	if err != nil || !describeOnly || name != "ah_undercutters" {
		t.Fatalf("describe: name=%q describeOnly=%v err=%v", name, describeOnly, err)
	}
	if !strings.Contains(string(schema), `"limit"`) {
		t.Fatalf("describe returned schema %s, want the original inputSchema", schema)
	}

	// Unknown and missing actions are errors that list the valid set.
	if _, _, _, _, _, err = r.ResolveFacade(f, json.RawMessage(`{"action":"nope"}`)); err == nil ||
		!strings.Contains(err.Error(), "market_summary") {
		t.Fatalf("unknown action error should list valid actions, got %v", err)
	}
	if _, _, _, _, _, err = r.ResolveFacade(f, json.RawMessage(`{}`)); err == nil {
		t.Fatal("missing action should error")
	}
}

func TestDescribeRoundTripsOriginalSchema(t *testing.T) {
	r := fakeReg(t)
	orig, _ := r.Get("db_query")
	want := string(orig.InputSchema)
	InstallFacades(r)

	f, _ := r.Get("db")
	_, _, _, _, schema, err := r.ResolveFacade(f, json.RawMessage(`{"describe":"query"}`))
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if string(schema) != want {
		t.Fatalf("describe schema = %s, want %s", schema, want)
	}
}

// Production tool-entry size, measured 2026-08-10 against the running
// ops-api: tools/list returned 125 tools totalling 201,114 characters of
// name + description + inputSchema.
//
// The fixture above uses toy descriptions (~96 chars/tool). At that scale the
// facades' own prose is larger than what it replaces and folding is a net
// loss — which is the honest result for a small registry, and the reason this
// consolidation is worth doing at 125 tools and would not be at 15. To
// measure the ratio that actually applies in production, the size test pads
// each entry to the real average.
const (
	prodTools     = 125
	prodBytes     = 201114
	prodEntrySize = prodBytes / prodTools // 1608
)

// The whole point of the change: at production entry sizes, advertisement
// must get materially smaller.
func TestFacadeAdvertisementIsSmaller(t *testing.T) {
	flat := prodScaleReg(t)
	flatSize := advertisedSize(flat)

	folded := prodScaleReg(t)
	InstallFacades(folded)
	foldedSize := advertisedSize(folded)

	if foldedSize >= flatSize {
		t.Fatalf("advertisement grew: %d -> %d bytes", flatSize, foldedSize)
	}
	saved := 100 * (1 - float64(foldedSize)/float64(flatSize))
	t.Logf("advertised payload %d -> %d bytes (%.0f%% smaller), %d -> %d tools",
		flatSize, foldedSize, saved, len(flat.AdvertisedSorted()), len(folded.AdvertisedSorted()))

	// The spec claims ~86%. Hold the floor well below that so a few added
	// tools don't fail the build, but high enough that a regression which
	// quietly re-advertises the flat list is caught.
	if saved < 60 {
		t.Errorf("only %.0f%% saved, expected >60%% at production entry sizes", saved)
	}
}

// prodScaleReg pads fakeReg's entries to the measured production average so
// the size ratio reflects reality rather than fixture artefacts.
func prodScaleReg(t *testing.T) *Registry {
	t.Helper()
	r := fakeReg(t)
	for _, n := range r.NamesSorted() {
		tool, _ := r.Get(n)
		pad := prodEntrySize - len(tool.Name) - len(tool.InputSchema)
		if pad > 0 {
			tool.Description = strings.Repeat("x", pad)
		}
		r.Register(tool)
	}
	return r
}

func advertisedSize(r *Registry) int {
	n := 0
	for _, name := range r.AdvertisedSorted() {
		t, _ := r.Get(name)
		n += len(t.Name) + len(t.Description) + len(t.InputSchema) + len(t.Annotations)
	}
	return n
}
