package mcpserver

import (
	"strings"
	"testing"
	"time"
)

func TestBuildGrepMatcher_SubstringDefault(t *testing.T) {
	m, err := buildGrepMatcher("OOM", false, false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !m("kernel: OOM killer invoked") {
		t.Error("expected substring hit")
	}
	if m("kernel: oom killer invoked") {
		t.Error("case-sensitive default must not match lowercase")
	}
}

func TestBuildGrepMatcher_CaseInsensitiveSubstring(t *testing.T) {
	m, err := buildGrepMatcher("OOM", false, true)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !m("kernel: oom killer invoked") {
		t.Error("case-insensitive substring must match lowercase")
	}
	if !m("kernel: OOM killer invoked") {
		t.Error("case-insensitive must still match original case")
	}
}

// Regex with caseInsensitive=true must fold to (?i) at compile, not via
// ToLower on every line — the latter would alter the input the regex sees
// and break patterns like `[A-Z]+` that the operator expects to match
// uppercase tokens specifically.
func TestBuildGrepMatcher_RegexCaseInsensitiveUsesInlineFlag(t *testing.T) {
	m, err := buildGrepMatcher(`exitCode\s*=\s*\d+`, true, true)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !m("exitCode = 137") {
		t.Error("expected regex hit")
	}
	if !m("ExitCode = 137") {
		t.Error("expected case-insensitive regex hit")
	}
}

func TestBuildGrepMatcher_BadRegex(t *testing.T) {
	if _, err := buildGrepMatcher("[unclosed", true, false); err == nil {
		t.Error("expected error for malformed regex")
	}
}

func TestResolveGrepSince_DurationFormats(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		in        string
		wantDelta int64 // seconds before now
		tolerance int64
	}{
		{"", 3600, 2}, // default 1h
		{"30m", 1800, 2},
		{"12h", 12 * 3600, 2},
		{"7d", 7 * 86400, 2},
	}
	for _, c := range cases {
		got, err := resolveGrepSince(c.in)
		if err != nil {
			t.Errorf("resolveGrepSince(%q): %v", c.in, err)
			continue
		}
		want := now - c.wantDelta
		if got < want-c.tolerance || got > want+c.tolerance {
			t.Errorf("resolveGrepSince(%q): got %d want ~%d", c.in, got, want)
		}
	}
}

// Window cap: a 30d request must be clamped to 7d so the daemon doesn't get
// asked for retention it doesn't have, and a clamped value gives the caller
// a deterministic since=now-7d rather than a daemon-side silent truncation.
func TestResolveGrepSince_ClampsBeyondMaxWindow(t *testing.T) {
	now := time.Now().Unix()
	got, err := resolveGrepSince("30d")
	if err != nil {
		t.Fatalf("resolveGrepSince(30d): %v", err)
	}
	want := now - int64(containerLogsGrepMaxWindow.Seconds())
	if got < want-2 || got > want+2 {
		t.Errorf("30d clamp: got %d want ~%d (7d ago)", got, want)
	}
}

func TestResolveGrepSince_UnixSecondsPassthrough(t *testing.T) {
	now := time.Now().Unix()
	in := now - 3600 // 1h ago
	got, err := resolveGrepSince(formatInt64(in))
	if err != nil {
		t.Fatalf("resolveGrepSince(unix): %v", err)
	}
	if got != in {
		t.Errorf("unix passthrough: got %d want %d", got, in)
	}
}

func TestResolveGrepSince_RejectsFuture(t *testing.T) {
	future := time.Now().Add(1 * time.Hour).Unix()
	if _, err := resolveGrepSince(formatInt64(future)); err == nil {
		t.Error("expected error for future unix timestamp")
	}
}

func TestResolveGrepSince_RejectsGarbage(t *testing.T) {
	if _, err := resolveGrepSince("not-a-thing"); err == nil {
		t.Error("expected error for non-duration non-unix input")
	}
}

func TestResolveGrepSince_RejectsNegative(t *testing.T) {
	if _, err := resolveGrepSince("-1h"); err == nil {
		t.Error("expected error for negative duration")
	}
}

func TestNormalizeStreamFilter(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		{"", "", false},
		{"both", "", false},
		{"BOTH", "", false},
		{"stdout", "stdout", false},
		{"STDOUT", "stdout", false},
		{"stderr", "stderr", false},
		{"junk", "", true},
	}
	for _, c := range cases {
		got, err := normalizeStreamFilter(c.in)
		if c.err {
			if err == nil {
				t.Errorf("normalizeStreamFilter(%q): expected err", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizeStreamFilter(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalizeStreamFilter(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

func TestClampGrepInt(t *testing.T) {
	cases := []struct {
		v, def, lo, hi, want int
	}{
		{0, 200, 1, 2000, 200},   // zero → default
		{-5, 200, 1, 2000, 200},  // negative → default
		{50, 200, 1, 2000, 50},   // in range
		{5000, 200, 1, 2000, 2000}, // over hi → hi
		{1, 200, 10, 2000, 10},   // under lo → lo (after positive check)
	}
	for _, c := range cases {
		got := clampGrepInt(c.v, c.def, c.lo, c.hi)
		if got != c.want {
			t.Errorf("clampGrepInt(%d,%d,%d,%d): got %d want %d", c.v, c.def, c.lo, c.hi, got, c.want)
		}
	}
}

// Description-keyword guard: agent tool-selection depends on the
// description, so the keywords that differentiate this tool from
// `ops_logs_tail` and `container_events` need a regression test.
func TestContainerLogsGrepTool_DescriptionMentionsKeyDistinctions(t *testing.T) {
	reg := NewRegistry()
	RegisterContainerLogsGrepTool(reg, ContainerDeps{DefaultContainer: "ac-worldserver"})
	tool, ok := reg.Get("container_logs_grep")
	if !ok {
		t.Fatal("container_logs_grep not registered")
	}
	for _, kw := range []string{"ops_logs_tail", "multi-hour", "maxMatches", "regex", "ac-worldserver"} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

func TestContainerLogsGrepTool_IsReadOnly(t *testing.T) {
	reg := NewRegistry()
	RegisterContainerLogsGrepTool(reg, ContainerDeps{DefaultContainer: "ac-worldserver"})
	tool, ok := reg.Get("container_logs_grep")
	if !ok {
		t.Fatal("container_logs_grep not registered")
	}
	if tool.Destructive {
		t.Error("Destructive: got true want false (read-only tool)")
	}
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint:true: %s", tool.Annotations)
	}
}

// Default container fallback mirrors container_events: nil deps default to
// ac-worldserver so the tool is usable without OPS_ADMIN_DEFAULT_CONTAINER set.
func TestContainerLogsGrepTool_DefaultsContainerWhenDepsEmpty(t *testing.T) {
	reg := NewRegistry()
	RegisterContainerLogsGrepTool(reg, ContainerDeps{})
	if _, ok := reg.Get("container_logs_grep"); !ok {
		t.Fatal("container_logs_grep not registered when DefaultContainer empty")
	}
}
