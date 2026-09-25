package mcpserver

import (
	"strings"
	"testing"
)

// productionToolNames is the exact tools/list output of the running ops-api,
// captured 2026-08-10 (125 tools). Classification is the one part of this
// change that can silently misroute a tool into the wrong facade or drop it
// entirely, and a hand-written fixture would not catch a name the real
// registry has but the fixture forgot. This list is that ground truth.
//
// Refresh it when tools are added: it is a snapshot, not a spec.
var productionToolNames = []string{
	"ah_bidder_activity",
	"ah_cross_house_arbitrage",
	"ah_cross_house_arbitrage_depth",
	"ah_floor_saturation",
	"ah_item_contest_ratio",
	"ah_item_demand",
	"ah_item_price_dispersion",
	"ah_market_bid_activity",
	"ah_market_category_breakdown",
	"ah_market_expiry_timeline",
	"ah_market_price_outliers",
	"ah_market_price_outliers_by_seller",
	"ah_market_price_percentiles",
	"ah_market_summary",
	"ah_market_top_items",
	"ah_market_top_sellers",
	"ah_seller_house_footprint",
	"ah_undercutters",
	"container_events",
	"container_inspect",
	"container_list",
	"container_logs_grep",
	"container_logs_grep_context",
	"container_logs_grep_context_window_summary",
	"container_logs_grep_context_window_summary_topn",
	"container_restart",
	"container_start",
	"container_stats",
	"container_stop",
	"container_system_info",
	"db_auto_increment_headroom",
	"db_charset_collation_audit",
	"db_column_type_drift",
	"db_connection_summary",
	"db_connection_summary_growth",
	"db_engine_distribution",
	"db_exec",
	"db_explain",
	"db_global_status",
	"db_global_status_growth",
	"db_index_data_ratio",
	"db_index_key_length_audit",
	"db_index_stats_staleness",
	"db_innodb_status",
	"db_low_cardinality_indexes",
	"db_over_indexed_tables",
	"db_processlist",
	"db_query",
	"db_redundant_indexes",
	"db_row_format_audit",
	"db_size_summary",
	"db_table_bloat",
	"db_table_info",
	"db_unindexed_tables",
	"db_widest_composite_indexes",
	"file_diff",
	"file_restore_backup",
	"file_set_key",
	"file_write",
	"get_observer_clip",
	"git_pull_all",
	"git_pull_core",
	"git_pull_module",
	"ops_audit_admin",
	"ops_audit_admin_latency_profile",
	"ops_audit_admin_summary",
	"ops_audit_gateway",
	"ops_audit_summary",
	"ops_audit_tactical",
	"ops_files_list",
	"ops_files_read",
	"ops_files_search",
	"ops_log_errors_histogram",
	"ops_log_errors_top",
	"ops_log_errors_top_normalize_preview",
	"ops_log_severity_breakdown",
	"ops_logs_tail",
	"ops_status",
	"render_tactical_map",
	"request_observer_clip",
	"request_observer_screenshot",
	"sql_import_dir",
	"sql_import_file",
	"worldserver_health",
	"wow_auth_failed_attempts_log",
	"wow_backup_db",
	"wow_backup_dir",
	"wow_backup_volumes",
	"wow_bot_latency_profile",
	"wow_bot_lookup",
	"wow_bot_source_channel_breakdown",
	"wow_bot_token_usage",
	"wow_bots_fleet_status",
	"wow_check_realmlist",
	"wow_create_account",
	"wow_events_timeline",
	"wow_failed_logins_top",
	"wow_guild_bank_activity",
	"wow_guild_bank_item_breakdown",
	"wow_guild_bank_item_flow",
	"wow_guild_bank_summary",
	"wow_guild_bank_tab_utilization",
	"wow_guild_bank_top_actors",
	"wow_guild_membership_actors",
	"wow_guild_membership_churn",
	"wow_guild_membership_targets",
	"wow_guild_roster",
	"wow_guild_summary",
	"wow_list_backups",
	"wow_mail_item_breakdown",
	"wow_mail_oldest",
	"wow_mail_summary",
	"wow_mail_unread",
	"wow_online_players",
	"wow_player_lookup",
	"wow_prune_old_backups",
	"wow_recent_logins",
	"wow_reset_character",
	"wow_restore_db",
	"wow_restore_dir",
	"wow_restore_volume_client",
	"wow_restore_volume_db",
	"wow_set_gm_level",
	"wow_update_realm_ip",
	"wow_update_realm_port",
}

func TestEveryProductionToolClassifies(t *testing.T) {
	if len(productionToolNames) != 125 {
		t.Fatalf("snapshot has %d names, expected the captured 125", len(productionToolNames))
	}
	counts := map[string]int{}
	for _, name := range productionToolNames {
		f := classify(name)
		if f == "" {
			t.Errorf("%s classified into no facade — it would stay advertised flat", name)
			continue
		}
		if _, ok := specFor(f); !ok {
			t.Errorf("%s classified as %q, which has no facadeSpec", name, f)
			continue
		}
		counts[f]++
	}

	// Expected distribution, derived from the prefix census of the live
	// registry. A shift here means classify() changed behaviour.
	want := map[string]int{
		"ah": 18, "db": 25, "container": 12, "ops": 15, "deploy": 9,
		"observer": 4, "wow_backup": 9, "wow_guild": 11, "wow_player": 12,
		"wow_bots": 5, "wow_realm": 5,
	}
	for facade, n := range want {
		if counts[facade] != n {
			t.Errorf("facade %s holds %d tools, want %d", facade, counts[facade], n)
		}
	}
	for facade := range counts {
		if _, ok := want[facade]; !ok {
			t.Errorf("unexpected facade %s with %d tools", facade, counts[facade])
		}
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	if total != len(productionToolNames) {
		t.Errorf("classified %d of %d production tools", total, len(productionToolNames))
	}
}

// Action names must stay unique inside each facade, or one tool silently
// shadows another and becomes unreachable.
func TestProductionActionNamesDoNotCollide(t *testing.T) {
	perFacade := map[string]map[string]string{}
	for _, name := range productionToolNames {
		f := classify(name)
		spec, ok := specFor(f)
		if !ok {
			continue
		}
		if perFacade[f] == nil {
			perFacade[f] = map[string]string{}
		}
		a := actionName(name, spec, perFacade[f])
		if prev, dup := perFacade[f][a]; dup {
			t.Errorf("facade %s: action %q claimed by both %s and %s", f, a, prev, name)
		}
		perFacade[f][a] = name
	}
	// Spot-check that stripping actually shortened things, since that is
	// what the model has to type.
	if got := perFacade["ah"]["market_summary"]; got != "ah_market_summary" {
		t.Errorf("ah.market_summary -> %q, want ah_market_summary", got)
	}
	if got := perFacade["wow_guild"]["bank_activity"]; got != "wow_guild_bank_activity" {
		t.Errorf("wow_guild.bank_activity -> %q", got)
	}
	for f, acts := range perFacade {
		for a := range acts {
			if strings.HasPrefix(a, "wow_") {
				t.Errorf("facade %s: action %q kept its wow_ prefix", f, a)
			}
		}
	}
}
