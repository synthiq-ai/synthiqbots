package mcpserver

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Facade consolidation.
//
// The registry advertises 125 flat tools totalling ~201k characters of name +
// description + inputSchema — roughly 50k tokens paid by every caller before
// it reads the operator's request. Worse, wow-mcp-bridge truncates discovery
// at 74 entries, so 51 tools are dispatchable-but-invisible.
//
// InstallFacades groups the flat tools into a handful of facade tools that
// take {action, params}. The originals stay registered and stay dispatchable
// by their original name — only their *advertisement* is suppressed via
// Tool.Hidden. Registry.NamesSorted (advertisement) and Registry.Get
// (dispatch) were already decoupled, so no alias table or shim handler is
// needed and nothing that calls a tool by name breaks.
//
// Facades carry no handler. server.handleToolsCall resolves
// facade+action -> target tool *before* the destructive-action gate, the rate
// limiter, and the audit write, so all three continue to see the real tool
// name and the real arguments.

// facadeSpec declares one facade: its advertised name, the prefix stripped
// from member tool names to form action names, and its description.
type facadeSpec struct {
	Name  string
	Strip string
	Desc  string
}

// facadeSpecs is the advertised set, in a stable order.
var facadeSpecs = []facadeSpec{
	{"ah", "ah_", "Auction house analytics: market summaries, price percentiles and dispersion, outliers, undercutters, seller and bidder activity, cross-house arbitrage. Read-only."},
	{"db", "db_", "MySQL introspection and query: ad-hoc query/exec/explain, schema and index audits (redundant, unindexed, over-indexed, low-cardinality, key length, type drift), size and bloat, engine and charset audits, processlist, global status and growth."},
	{"container", "container_", "Docker container operations for the AzerothCore stack: list, inspect, stats, events, system info, start/stop/restart, and the log-grep ladder (grep, context, window summary, top-N)."},
	{"ops", "ops_", "ops-api introspection: gateway/tactical/admin audit tables and summaries, admin latency profile, log tail, severity breakdown, error histograms and top-N, and module file list/read/search."},
	{"deploy", "", "Repository and config deployment: file diff/write/set_key/restore_backup, git pull (core, module, all), and SQL import (file, dir)."},
	{"observer", "", "Observer capture and rendering: request a screenshot or clip, fetch a rendered clip, render the tactical map."},
	{"wow_backup", "wow_", "Backup and restore: dump/restore the databases, back up and restore directories and Docker volumes, list backups, prune old ones."},
	{"wow_guild", "wow_guild_", "Guild analytics: roster and summary, bank activity, item breakdown and flow, tab utilisation, top actors, membership churn with actor and target breakdowns."},
	{"wow_player", "wow_", "Player and account operations: account creation, GM level, character reset, player lookup, online players, recent logins, failed-login analytics, and mailbox summaries."},
	{"wow_bots", "wow_", "Playerbot telemetry: per-bot lookup, latency profile, token usage, source-channel breakdown, fleet status."},
	{"wow_realm", "wow_", "Realm configuration and health: realmlist check, realm IP and port updates, worldserver health, event timeline."},
}

// classify maps a registered tool name to its facade. Returns "" for tools
// that should stay advertised flat (there are currently none, but a future
// tool that matches nothing falls through and keeps its own entry rather than
// disappearing).
//
// Order matters: the wow_ rules are checked most-specific first.
func classify(name string) string {
	switch {
	case strings.HasPrefix(name, "ah_"):
		return "ah"
	case strings.HasPrefix(name, "db_"):
		return "db"
	case strings.HasPrefix(name, "container_"):
		return "container"
	case strings.HasPrefix(name, "ops_"):
		return "ops"
	case strings.HasPrefix(name, "file_"),
		strings.HasPrefix(name, "git_"),
		strings.HasPrefix(name, "sql_"):
		return "deploy"
	case strings.Contains(name, "observer"), name == "render_tactical_map":
		return "observer"
	case name == "worldserver_health":
		return "wow_realm"
	}

	if !strings.HasPrefix(name, "wow_") {
		return ""
	}
	switch {
	// Backup/restore first — "wow_list_backups" and "wow_prune_old_backups"
	// would otherwise fall through to wow_player.
	case strings.Contains(name, "backup"), strings.Contains(name, "restore"):
		return "wow_backup"
	case strings.HasPrefix(name, "wow_guild_"):
		return "wow_guild"
	case strings.HasPrefix(name, "wow_bot_"), strings.HasPrefix(name, "wow_bots_"):
		return "wow_bots"
	case strings.Contains(name, "realm"):
		return "wow_realm"
	case name == "wow_events_timeline":
		return "wow_realm"
	default:
		// Accounts, logins, characters, online players, mail.
		return "wow_player"
	}
}

// actionName strips the facade's declared prefix. Falls back to the full tool
// name when stripping would produce an empty or colliding action — the
// registry is the source of truth, so a name we cannot shorten safely keeps
// its long form rather than silently shadowing a sibling.
func actionName(tool string, spec facadeSpec, taken map[string]string) string {
	a := tool
	if spec.Strip != "" && strings.HasPrefix(tool, spec.Strip) {
		if trimmed := strings.TrimPrefix(tool, spec.Strip); trimmed != "" {
			a = trimmed
		}
	}
	if _, clash := taken[a]; clash {
		return tool
	}
	return a
}

// InstallFacades classifies every currently-registered tool, hides the
// classified ones from advertisement, and registers the facade tools that
// stand in for them. Call once at startup, after all tools are registered.
//
// Returns the number of facades installed and the number of tools folded in.
func InstallFacades(r *Registry) (facades, folded int) {
	// action -> target tool name, per facade.
	members := map[string]map[string]string{}

	for _, name := range r.NamesSorted() {
		t, ok := r.Get(name)
		if !ok || t.IsFacade {
			continue
		}
		f := classify(name)
		if f == "" {
			continue
		}
		spec, ok := specFor(f)
		if !ok {
			continue
		}
		if members[f] == nil {
			members[f] = map[string]string{}
		}
		members[f][actionName(name, spec, members[f])] = name

		t.Hidden = true
		r.Register(t)
		folded++
	}

	for _, spec := range facadeSpecs {
		acts := members[spec.Name]
		if len(acts) == 0 {
			continue
		}
		r.Register(Tool{
			Name:        spec.Name,
			Description: spec.Desc + facadeDescSuffix,
			InputSchema: facadeSchema(acts),
			Annotations: facadeAnnotations(r, acts),
			IsFacade:    true,
			Actions:     acts,
		})
		facades++
	}
	return facades, folded
}

const facadeDescSuffix = ` Call with {"action": "<name>", "params": {...}}. Pass {"describe": "<action>"} to get that action's full argument schema without executing it.`

func specFor(name string) (facadeSpec, bool) {
	for _, s := range facadeSpecs {
		if s.Name == name {
			return s, true
		}
	}
	return facadeSpec{}, false
}

// facadeSchema builds the facade's advertised inputSchema: the action enum
// plus a free-form params object.
//
// This is where the token saving comes from, and it is a real trade: the 125
// per-action schemas are no longer advertised, so arguments are not validated
// client-side. Handlers already validate their own arguments and return
// structured errors, and the `describe` field fetches any single action's
// original schema on demand.
func facadeSchema(acts map[string]string) json.RawMessage {
	names := make([]string, 0, len(acts))
	for a := range acts {
		names = append(names, a)
	}
	sort.Strings(names)

	enum, _ := json.Marshal(names)
	return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{`+
		`"action":{"type":"string","enum":%s,"description":"Operation to perform."},`+
		`"params":{"type":"object","description":"Arguments for the chosen action. Use describe to fetch its schema."},`+
		`"describe":{"type":"string","description":"Return this action's argument schema instead of executing anything."}`+
		`}}`, enum))
}

// facadeAnnotations reports the facade as read-only only when every action
// behind it is read-only. A facade containing one destructive action must not
// advertise readOnlyHint:true — clients use that hint to decide whether to
// prompt. The per-call destructive gate is enforced against the resolved
// target regardless of what this hint says.
func facadeAnnotations(r *Registry, acts map[string]string) json.RawMessage {
	for _, target := range acts {
		if t, ok := r.Get(target); ok && t.Destructive {
			return AnnAction(false)
		}
	}
	return AnnRead()
}

// ResolveFacade maps a facade call to its target tool. It returns the target
// tool, the raw arguments to hand the target's handler, and the target's name
// (so the rate limiter and audit log see the real tool, not the facade).
//
// describeOnly is true when the caller asked for a schema rather than
// execution; schema is then the target's advertised inputSchema.
func (r *Registry) ResolveFacade(f Tool, args json.RawMessage) (
	target Tool, targetName string, targetArgs json.RawMessage, describeOnly bool, schema json.RawMessage, err error) {

	var p struct {
		Action   string          `json:"action"`
		Params   json.RawMessage `json:"params"`
		Describe string          `json:"describe"`
	}
	if len(args) > 0 {
		if e := json.Unmarshal(args, &p); e != nil {
			return Tool{}, "", nil, false, nil, fmt.Errorf("arguments: %w", e)
		}
	}

	want := p.Action
	if p.Describe != "" {
		want = p.Describe
	}
	if want == "" {
		return Tool{}, "", nil, false, nil, fmt.Errorf(
			"%s: missing \"action\" (one of: %s)", f.Name, strings.Join(sortedActions(f), ", "))
	}

	name, ok := f.Actions[want]
	if !ok {
		return Tool{}, "", nil, false, nil, fmt.Errorf(
			"%s: unknown action %q (one of: %s)", f.Name, want, strings.Join(sortedActions(f), ", "))
	}
	t, ok := r.Get(name)
	if !ok {
		return Tool{}, "", nil, false, nil, fmt.Errorf("%s: action %q maps to unregistered tool %q", f.Name, want, name)
	}

	if p.Describe != "" {
		return t, name, nil, true, t.InputSchema, nil
	}

	a := p.Params
	if len(a) == 0 {
		a = json.RawMessage(`{}`)
	}
	return t, name, a, false, nil, nil
}

// mustDescribeJSON renders a describe response: the resolved tool's real name,
// description, destructive flag and original inputSchema. Never fails — a
// marshal error degrades to a JSON error object rather than a 500, since the
// caller is mid-discovery and a hard failure there is worse than a partial
// answer.
func mustDescribeJSON(name string, t Tool, schema json.RawMessage) []byte {
	if len(schema) == 0 {
		schema = json.RawMessage(`{}`)
	}
	b, err := json.Marshal(map[string]any{
		"tool":        name,
		"description": t.Description,
		"destructive": t.Destructive,
		"inputSchema": schema,
	})
	if err != nil {
		return []byte(`{"error":"describe: marshal failed"}`)
	}
	return b
}

func sortedActions(f Tool) []string {
	out := make([]string, 0, len(f.Actions))
	for a := range f.Actions {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}
