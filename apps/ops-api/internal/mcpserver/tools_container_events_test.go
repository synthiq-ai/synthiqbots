package mcpserver

import (
	"strings"
	"testing"
	"time"
)

// goldenEventsOomDieSequence is the NDJSON sequence the Docker daemon emits
// when ac-worldserver gets reaped by the kernel OOM killer. The `oom` event
// arrives one second before `die` (exitCode=137). Operator forensics depends
// on seeing BOTH events in order: a `die` without a preceding `oom` indicates
// a manual SIGKILL or app-level crash, not a memory exhaustion.
const goldenEventsOomDieSequence = `{"Type":"container","Action":"start","Actor":{"ID":"abc","Attributes":{"name":"ac-worldserver","image":"acore/ac-wotlk-worldserver:master"}},"time":1700000100,"timeNano":1700000100000000000}
{"Type":"container","Action":"oom","Actor":{"ID":"abc","Attributes":{"name":"ac-worldserver","image":"acore/ac-wotlk-worldserver:master"}},"time":1700000200,"timeNano":1700000200000000000}
{"Type":"container","Action":"die","Actor":{"ID":"abc","Attributes":{"name":"ac-worldserver","image":"acore/ac-wotlk-worldserver:master","exitCode":"137"}},"time":1700000201,"timeNano":1700000201000000000}
{"Type":"container","Action":"start","Actor":{"ID":"abc","Attributes":{"name":"ac-worldserver","image":"acore/ac-wotlk-worldserver:master"}},"time":1700000300,"timeNano":1700000300000000000}
`

func TestParseContainerEvents_OomDieSequence(t *testing.T) {
	events, truncated := parseContainerEvents([]byte(goldenEventsOomDieSequence), 100)
	if truncated {
		t.Errorf("not expecting truncated=true")
	}
	if got, want := len(events), 4; got != want {
		t.Fatalf("event count: got %d want %d", got, want)
	}
	if got, want := events[0]["action"], "start"; got != want {
		t.Errorf("events[0].action: got %v want %v", got, want)
	}
	if got, want := events[1]["action"], "oom"; got != want {
		t.Errorf("events[1].action: got %v want %v", got, want)
	}
	if got, want := events[2]["action"], "die"; got != want {
		t.Errorf("events[2].action: got %v want %v", got, want)
	}
	if got, want := events[2]["exitCode"], "137"; got != want {
		t.Errorf("events[2].exitCode: got %v want %v", got, want)
	}
	if got, want := events[2]["ts"], int64(1700000201); got != want {
		t.Errorf("events[2].ts: got %v want %v", got, want)
	}
	// tsHuman is the deterministic UTC representation operators copy/paste
	// into incident timelines. Guard the format so a future stdlib change
	// (or someone setting LOCAL_TIME=true) doesn't silently drift it.
	if got, want := events[2]["tsHuman"], "2023-11-14T22:16:41Z"; got != want {
		t.Errorf("events[2].tsHuman: got %v want %v", got, want)
	}
	// Attribute curation: name and image survive, arbitrary labels would not.
	attrs, ok := events[2]["attributes"].(map[string]string)
	if !ok {
		t.Fatalf("events[2].attributes: not map[string]string")
	}
	if got, want := attrs["name"], "ac-worldserver"; got != want {
		t.Errorf("attrs.name: got %v want %v", got, want)
	}
	if got, want := attrs["image"], "acore/ac-wotlk-worldserver:master"; got != want {
		t.Errorf("attrs.image: got %v want %v", got, want)
	}
}

// die without a preceding oom emits NO exitCode field on rows that lack one —
// operators reading the result depend on the absence (presence = OOM kill is
// likely). A bug that defaulted exitCode to "0" would mask the signal.
func TestParseContainerEvents_OmitsMissingExitCode(t *testing.T) {
	body := `{"Type":"container","Action":"start","Actor":{"ID":"x","Attributes":{"name":"x"}},"time":1700000000}` + "\n"
	events, _ := parseContainerEvents([]byte(body), 100)
	if got, want := len(events), 1; got != want {
		t.Fatalf("event count: got %d want %d", got, want)
	}
	if _, present := events[0]["exitCode"]; present {
		t.Errorf("events[0].exitCode should be absent when the daemon omits it")
	}
}

// Truncated must be set when the daemon emits more than maxRows. The cap
// keeps the response under the 25 KB MCP envelope on busy crash-loop windows.
func TestParseContainerEvents_TruncatedOnOverflow(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 60; i++ {
		b.WriteString(`{"Type":"container","Action":"start","Actor":{"ID":"x","Attributes":{"name":"x"}},"time":1700000000}` + "\n")
	}
	events, truncated := parseContainerEvents([]byte(b.String()), 50)
	if !truncated {
		t.Errorf("expected truncated=true, got false")
	}
	if got, want := len(events), 50; got != want {
		t.Errorf("event count: got %d want %d", got, want)
	}
}

// Malformed lines (trailing blank, partial record) are skipped, not surfaced.
// bufio.Scanner gives us line-level framing so a bad line can't desync the
// parser the way json.Decoder would.
func TestParseContainerEvents_SkipsMalformed(t *testing.T) {
	body := `{"Type":"container","Action":"start","Actor":{"ID":"x","Attributes":{"name":"x"}},"time":1700000000}
not json line
{"Type":"container","Action":"die","Actor":{"ID":"x","Attributes":{"name":"x","exitCode":"0"}},"time":1700000100}

`
	events, _ := parseContainerEvents([]byte(body), 100)
	if got, want := len(events), 2; got != want {
		t.Fatalf("event count: got %d want %d", got, want)
	}
	if got, want := events[0]["action"], "start"; got != want {
		t.Errorf("events[0].action: got %v want %v", got, want)
	}
	if got, want := events[1]["action"], "die"; got != want {
		t.Errorf("events[1].action: got %v want %v", got, want)
	}
}

// Empty body is a valid "no events in window" response — return empty slice,
// not nil, so JSON serialization gives `"events":[]` not `"events":null`.
// The operator parses the count and the events array separately; null would
// crash a follow-up tool call that iterates events.
func TestParseContainerEvents_EmptyBody(t *testing.T) {
	events, truncated := parseContainerEvents([]byte(""), 100)
	if events == nil {
		t.Errorf("events should be empty slice, not nil")
	}
	if got, want := len(events), 0; got != want {
		t.Errorf("event count: got %d want %d", got, want)
	}
	if truncated {
		t.Errorf("truncated on empty body: got true want false")
	}
}

func TestParseDurationDays(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"24h", 24 * time.Hour},
		{"30m", 30 * time.Minute},
		{"7d", 7 * 24 * time.Hour},
		{"1d", 24 * time.Hour},
		{"5s", 5 * time.Second},
	}
	for _, c := range cases {
		got, err := parseDurationDays(c.in)
		if err != nil {
			t.Errorf("parseDurationDays(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseDurationDays(%q): got %v want %v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"bad", "Xd", "-1d"} {
		if _, err := parseDurationDays(bad); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestResolveEventTimestamp(t *testing.T) {
	now := time.Now().Unix()

	// "now" literal
	got, err := resolveEventTimestamp("now", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got < now-2 || got > now+2 {
		t.Errorf("now: got %d want ~%d", got, now)
	}

	// Past relative — `isPast=true` subtracts
	got, err = resolveEventTimestamp("24h", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if got < now-86400-2 || got > now-86400+2 {
		t.Errorf("24h ago: got %d want ~%d", got, now-86400)
	}

	// Empty triggers default
	got, err = resolveEventTimestamp("", "1h", true)
	if err != nil {
		t.Fatal(err)
	}
	if got < now-3600-2 || got > now-3600+2 {
		t.Errorf("default 1h: got %d want ~%d", got, now-3600)
	}

	// Unix seconds passthrough
	got, err = resolveEventTimestamp("1700000000", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1700000000 {
		t.Errorf("unix passthrough: got %d want 1700000000", got)
	}

	// Days extension
	got, err = resolveEventTimestamp("7d", "", true)
	if err != nil {
		t.Fatal(err)
	}
	want := now - 7*86400
	if got < want-2 || got > want+2 {
		t.Errorf("7d ago: got %d want ~%d", got, want)
	}

	// Bad input bubbles up
	if _, err := resolveEventTimestamp("not-a-duration", "", true); err == nil {
		t.Errorf("expected error for bad input")
	}
}

// Tool description must mention the OOM signal and exit code 137 so the
// agent picks `container_events` when the operator says "did ac-worldserver
// get OOM-killed overnight?". Anthropic's SDK reads the description as part
// of the tool-selection prompt; load-bearing keywords need a regression guard.
func TestContainerEventsTool_DescriptionMentionsOOMForensics(t *testing.T) {
	reg := NewRegistry()
	RegisterContainerEventsTool(reg, ContainerDeps{DefaultContainer: "ac-worldserver"})
	tool, ok := reg.Get("container_events")
	if !ok {
		t.Fatal("container_events not registered")
	}
	for _, kw := range []string{"OOM", "ac-worldserver", "137", "die", "exit code"} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// Annotation must be the read-only marker — `container_events` is a daemon
// query, not a state change. AnnAction would gate it behind
// OPS_ADMIN_ALLOW_ACTIONS, which would be a usability regression: read tools
// must work without the flag flip.
func TestContainerEventsTool_IsReadOnly(t *testing.T) {
	reg := NewRegistry()
	RegisterContainerEventsTool(reg, ContainerDeps{DefaultContainer: "ac-worldserver"})
	tool, ok := reg.Get("container_events")
	if !ok {
		t.Fatal("container_events not registered")
	}
	if tool.Destructive {
		t.Errorf("Destructive: got true want false (read-only tool)")
	}
	// AnnRead() emits {"readOnlyHint":true,...}; assert the boolean is set.
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint:true: %s", tool.Annotations)
	}
}
