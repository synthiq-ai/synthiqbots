package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
)

// TestWorldserverHealth_DescriptionMentionsContext pins the description
// keywords the operator-agent uses to pick this tool over a sequence of
// ops_status + container_stats + ops_logs_tail. Drop the composite framing
// and the agent will fall back to the multi-call sequence — which is the
// exact friction this tool removes.
func TestWorldserverHealth_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterWorldserverHealthTool(reg, OpsDeps{})
	tool, ok := reg.Get("worldserver_health")
	if !ok {
		t.Fatal("worldserver_health not registered")
	}
	for _, kw := range []string{
		"ac-worldserver",
		"ops_status",
		"container_stats",
		"ops_logs_tail",
		"checks[]",
		"allOk",
		"lastError",
		"exit 137",
		"OOM",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
	if tool.Destructive {
		t.Error("worldserver_health must not be destructive")
	}
}

// TestWorldserverHealth_RejectsBadLookback covers the input-validation
// surface — bad duration, zero, negative, over 24h. Ensures the tool fails
// closed before it tries to stream logs.
func TestWorldserverHealth_RejectsBadLookback(t *testing.T) {
	reg := NewRegistry()
	RegisterWorldserverHealthTool(reg, OpsDeps{})
	tool, _ := reg.Get("worldserver_health")

	cases := []struct {
		name   string
		args   string
		errSub string
	}{
		{"unparseable", `{"errorLookback":"banana"}`, "duration"},
		{"zero", `{"errorLookback":"0s"}`, "positive"},
		{"negative", `{"errorLookback":"-1h"}`, "positive"},
		{"over_max", `{"errorLookback":"25h"}`, "24h max"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := tool.Handler(context.Background(), json.RawMessage(tc.args), "")
			got, _ := resp.(map[string]any)
			msg, _ := got["error"].(string)
			if !strings.Contains(msg, tc.errSub) {
				t.Errorf("expected error containing %q, got %v", tc.errSub, got)
			}
		})
	}
}

// TestWorldserverHealth_NilServerReturnsError covers the deps short-circuit.
// In production main wires deps.Server, but a misregistration shouldn't
// panic — it should surface a clean error.
func TestWorldserverHealth_NilServerReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterWorldserverHealthTool(reg, OpsDeps{})
	tool, _ := reg.Get("worldserver_health")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	msg, _ := got["error"].(string)
	if !strings.Contains(msg, "Server") {
		t.Errorf("expected Server-not-configured error, got %v", got)
	}
}

// TestComputeUptime covers the StartedAt parsing surface — RFC3339Nano,
// older RFC3339, missing/empty, future timestamps (clock skew).
func TestComputeUptime(t *testing.T) {
	tenMinAgo := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339Nano)
	twoDaysAgo := time.Now().UTC().Add(-49 * time.Hour).Format(time.RFC3339)

	cases := []struct {
		name        string
		startedAt   string
		running     bool
		wantSecsMin int64
		wantHumSub  string
	}{
		{"running_10m", tenMinAgo, true, 590, "m"},
		{"running_2d", twoDaysAgo, true, 49*3600 - 5, "d"},
		{"not_running", tenMinAgo, false, 0, ""},
		{"empty_startedAt", "", true, 0, ""},
		{"unparseable", "garbage", true, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			secs, hum := computeUptime(tc.startedAt, tc.running)
			if tc.wantSecsMin > 0 && secs < tc.wantSecsMin {
				t.Errorf("uptime secs=%d, want >=%d", secs, tc.wantSecsMin)
			}
			if tc.wantSecsMin == 0 && secs != 0 {
				t.Errorf("uptime secs=%d, want 0", secs)
			}
			if tc.wantHumSub == "" {
				if hum != "" {
					t.Errorf("uptime human=%q, want empty", hum)
				}
			} else if !strings.Contains(hum, tc.wantHumSub) {
				t.Errorf("uptime human=%q, want substring %q", hum, tc.wantHumSub)
			}
		})
	}
}

// TestHumanDuration pins the format-string boundaries: <1m → "Ns", <1h →
// "Nm", <1d → "NhMm" (or "Nh" when M=0), >=1d → "NdMh" (or "Nd" when M=0).
// Mirrors `docker ps`'s "Up 2d" column rather than Go's default duration
// stringifier.
func TestHumanDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{59 * time.Second, "59s"},
		{61 * time.Second, "1m"},
		{59 * time.Minute, "59m"},
		{61 * time.Minute, "1h1m"},
		{60 * time.Minute, "1h"},
		{2 * time.Hour, "2h"},
		{2*time.Hour + 30*time.Minute, "2h30m"},
		{23*time.Hour + 59*time.Minute, "23h59m"},
		{24 * time.Hour, "1d"},
		{49 * time.Hour, "2d1h"},
		{72 * time.Hour, "3d"},
	}
	for _, tc := range cases {
		got := humanDuration(tc.d)
		if got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestHealthErrorRegex_Matches pins the curated severity-keyword set against
// representative ac-worldserver log fixtures. Adding a new pattern that
// breaks one of these positives has to update this fixture intentionally —
// silent regression of the operator's "is this an error?" heuristic is the
// failure mode to guard against.
func TestHealthErrorRegex_Matches(t *testing.T) {
	positive := []string{
		"\x1b[31;1m[Ollama Chat] HTTP request failed - no response from 192.168.100.19\x1b[0m",
		"FATAL: cannot bind socket on port 8085",
		"Critical: lua state corruption detected",
		"Exception caught in WorldSession::Update",
		"Cannot connect to MySQL: connection refused",
		"connection refused on 127.0.0.1:3306",
		"AHBot panic — duplicate auction id",
		"failed to load DBC file",
		"ERROR loading map 530",
		"segfault at address 0x0",
		"stack trace below",
		"access denied for user 'ops_ro'@'localhost'",
		"corrupt DBC entry: skipped row 17",
	}
	for _, s := range positive {
		if !reHealthError.MatchString(s) {
			t.Errorf("expected positive match: %q", s)
		}
	}
	negative := []string{
		"[Ollama Chat] HTTP Request - Protocol: http, Host: 192.168.100.19",
		"Random Bots Stats: 3 online",
		"Bots class: paladin: 1, avg lvl: 2",
		"AHBot [10002]: Begin Performing Update Cycle",
		"Loaded 1024 spell entries",
		"Server is now ready for connections.",
		// "errorlevel" / "failure" don't currently match because we use \berror\b
		// and \bfailed?\b — non-word-boundary embeddings shouldn't fire.
		"the suberrorlevel field was 0",
		"failurefoo not a real word here",
	}
	for _, s := range negative {
		if reHealthError.MatchString(s) {
			t.Errorf("expected NO match: %q", s)
		}
	}
}

// TestPickLatestError covers the selection logic when multiple matching
// lines fall in the window. The "most recent" pick prefers the highest TS;
// when all TSes are zero we fall back to last-by-iteration.
func TestPickLatestError(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		got, n := pickLatestError(nil)
		if got != nil || n != 0 {
			t.Errorf("expected nil/0, got %v / %d", got, n)
		}
	})

	t.Run("picks_max_ts", func(t *testing.T) {
		t1, _ := time.Parse(time.RFC3339, "2026-05-07T12:00:00Z")
		t2, _ := time.Parse(time.RFC3339, "2026-05-07T12:30:00Z")
		t3, _ := time.Parse(time.RFC3339, "2026-05-07T12:15:00Z")
		lines := []dockerlog.Line{
			{Stream: "stdout", TS: t1, Msg: "ERROR a"},
			{Stream: "stderr", TS: t2, Msg: "ERROR b"},
			{Stream: "stdout", TS: t3, Msg: "ERROR c"},
		}
		got, n := pickLatestError(lines)
		if n != 3 {
			t.Errorf("count = %d, want 3", n)
		}
		if msg, _ := got["msg"].(string); msg != "ERROR b" {
			t.Errorf("msg = %q, want %q", msg, "ERROR b")
		}
		if stream, _ := got["stream"].(string); stream != "stderr" {
			t.Errorf("stream = %q, want %q", stream, "stderr")
		}
	})

	t.Run("zero_ts_picks_last", func(t *testing.T) {
		lines := []dockerlog.Line{
			{Stream: "stdout", TS: time.Time{}, Msg: "ERROR first"},
			{Stream: "stdout", TS: time.Time{}, Msg: "ERROR last"},
		}
		got, n := pickLatestError(lines)
		if n != 2 {
			t.Errorf("count = %d, want 2", n)
		}
		if msg, _ := got["msg"].(string); msg != "ERROR last" {
			t.Errorf("msg = %q, want %q", msg, "ERROR last")
		}
	})

	t.Run("ansi_normalized", func(t *testing.T) {
		ts, _ := time.Parse(time.RFC3339, "2026-05-07T12:00:00Z")
		raw := "\x1b[31;1m[Ollama Chat] HTTP request failed - no response from 192.168.100.19\x1b[0m"
		lines := []dockerlog.Line{{Stream: "stdout", TS: ts, Msg: raw}}
		got, n := pickLatestError(lines)
		if n != 1 {
			t.Errorf("count = %d, want 1", n)
		}
		if msg, _ := got["msg"].(string); msg != raw {
			t.Errorf("msg should be raw (with ANSI), got %q", msg)
		}
		norm, _ := got["msgNormalized"].(string)
		if strings.Contains(norm, "\x1b") {
			t.Errorf("msgNormalized still has ANSI codes: %q", norm)
		}
		if !strings.Contains(norm, "HTTP request failed") {
			t.Errorf("msgNormalized lost content: %q", norm)
		}
	})
}

// TestChecksAllOK covers the rollup of the per-check ok flag into the
// top-level allOk that operators branch on.
func TestChecksAllOK(t *testing.T) {
	if !checksAllOK(nil) {
		t.Error("empty checks should be allOk=true (vacuous)")
	}
	if !checksAllOK([]map[string]any{{"name": "a", "ok": true}, {"name": "b", "ok": true}}) {
		t.Error("all-ok should be true")
	}
	if checksAllOK([]map[string]any{{"name": "a", "ok": true}, {"name": "b", "ok": false}}) {
		t.Error("any-failed should be false")
	}
	if checksAllOK([]map[string]any{{"name": "a"}}) {
		t.Error("missing ok key should be treated as false")
	}
}

// TestAnsiEscapeStripping pins reAnsiEscape against the worldserver's
// actual color-code pattern. Operator-readable normalization breaks if the
// regex starts missing the trailing reset code or the leading color set.
func TestAnsiEscapeStripping(t *testing.T) {
	cases := []struct{ in, want string }{
		{"\x1b[31;1mfoo\x1b[0m", "foo"},
		{"\x1b[36mhello\x1b[0m", "hello"},
		{"plain text", "plain text"},
		{"\x1b[0m", ""},
		{"foo \x1b[31mbar\x1b[0m baz", "foo bar baz"},
	}
	for _, tc := range cases {
		got := reAnsiEscape.ReplaceAllString(tc.in, "")
		if got != tc.want {
			t.Errorf("strip(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
