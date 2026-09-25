package mcpserver

import (
	"strings"
	"testing"
)

// evRow builds the minimal event-row shape parseContainerEvents emits — the
// merge only reads `ts` (int64) and tags `container`; `action` rides along so
// the tests can assert ordering by a human-readable field.
func evRow(ts int64, action string) map[string]any {
	return map[string]any{"ts": ts, "action": action}
}

// The headline behavior: events from multiple containers interleave into one
// timestamp-sorted sequence, and every row is tagged with the container it came
// from. Without the tag a cross-container `die` is unattributable — the whole
// point of the tool.
func TestMergeContainerEventTimeline_SortsAndTagsAcrossContainers(t *testing.T) {
	perContainer := map[string][]map[string]any{
		"ac-database":    {evRow(100, "start"), evRow(400, "die")},
		"ac-worldserver": {evRow(200, "start"), evRow(300, "die")},
	}
	events, truncated, merged := mergeContainerEventTimeline(perContainer, 100)
	if truncated {
		t.Errorf("not expecting truncated=true")
	}
	if got, want := merged, 4; got != want {
		t.Errorf("merged: got %d want %d", got, want)
	}
	if got, want := len(events), 4; got != want {
		t.Fatalf("event count: got %d want %d", got, want)
	}
	wantTs := []int64{100, 200, 300, 400}
	wantContainer := []string{"ac-database", "ac-worldserver", "ac-worldserver", "ac-database"}
	for i := range events {
		if got := events[i]["ts"].(int64); got != wantTs[i] {
			t.Errorf("events[%d].ts: got %d want %d", i, got, wantTs[i])
		}
		if got := events[i]["container"].(string); got != wantContainer[i] {
			t.Errorf("events[%d].container: got %q want %q", i, got, wantContainer[i])
		}
	}
}

// Same-second events from DIFFERENT containers break the tie by container name
// ascending. The deterministic tiebreaker is load-bearing: Go's map iteration
// is randomized, so without it the merge would flake on ties.
func TestMergeContainerEventTimeline_SameTsTiebreakerIsContainerAsc(t *testing.T) {
	perContainer := map[string][]map[string]any{
		"ac-worldserver": {evRow(100, "die")},
		"ac-database":    {evRow(100, "die")},
		"ac-authserver":  {evRow(100, "die")},
	}
	events, _, _ := mergeContainerEventTimeline(perContainer, 100)
	want := []string{"ac-authserver", "ac-database", "ac-worldserver"}
	if len(events) != len(want) {
		t.Fatalf("event count: got %d want %d", len(events), len(want))
	}
	for i := range events {
		if got := events[i]["container"].(string); got != want[i] {
			t.Errorf("events[%d].container: got %q want %q (container-asc tiebreaker)", i, got, want[i])
		}
	}
}

// Same-second events from the SAME container preserve daemon emission order via
// the per-input seq key — an `oom` and the `die` it precedes must stay
// oom-before-die even when they share a second, or the OOM-cascade signal
// inverts.
func TestMergeContainerEventTimeline_SameTsSameContainerPreservesOrder(t *testing.T) {
	perContainer := map[string][]map[string]any{
		"ac-worldserver": {evRow(100, "oom"), evRow(100, "die")},
	}
	events, _, _ := mergeContainerEventTimeline(perContainer, 100)
	if got, want := len(events), 2; got != want {
		t.Fatalf("event count: got %d want %d", got, want)
	}
	if got, want := events[0]["action"].(string), "oom"; got != want {
		t.Errorf("events[0].action: got %q want %q", got, want)
	}
	if got, want := events[1]["action"].(string), "die"; got != want {
		t.Errorf("events[1].action: got %q want %q", got, want)
	}
}

// Truncation keeps the OLDEST maxRows (matching container_events scan-order
// truncation) and reports the full pre-cap count via `merged`. The operator
// narrows `since` to surface a more recent slice.
func TestMergeContainerEventTimeline_TruncatesOldestKept(t *testing.T) {
	perContainer := map[string][]map[string]any{
		"ac-database":    {evRow(1, "start"), evRow(4, "die")},
		"ac-worldserver": {evRow(2, "start"), evRow(3, "die"), evRow(5, "start")},
	}
	events, truncated, merged := mergeContainerEventTimeline(perContainer, 3)
	if !truncated {
		t.Errorf("expected truncated=true")
	}
	if got, want := merged, 5; got != want {
		t.Errorf("merged: got %d want %d", got, want)
	}
	if got, want := len(events), 3; got != want {
		t.Fatalf("event count: got %d want %d", got, want)
	}
	// Oldest three by ts are 1,2,3 — the newest (4,5) are dropped.
	wantTs := []int64{1, 2, 3}
	for i := range events {
		if got := events[i]["ts"].(int64); got != wantTs[i] {
			t.Errorf("events[%d].ts: got %d want %d (oldest kept)", i, got, wantTs[i])
		}
	}
}

// Empty input is a valid "no events anywhere in window" response — events must
// be a non-nil empty slice so JSON gives `"events":[]` not `"events":null`,
// which would crash a follow-up tool that iterates the array.
func TestMergeContainerEventTimeline_EmptyPreservesSliceNotNil(t *testing.T) {
	events, truncated, merged := mergeContainerEventTimeline(map[string][]map[string]any{}, 100)
	if events == nil {
		t.Errorf("events should be empty slice, not nil")
	}
	if got, want := len(events), 0; got != want {
		t.Errorf("event count: got %d want %d", got, want)
	}
	if truncated {
		t.Errorf("truncated on empty input: got true want false")
	}
	if merged != 0 {
		t.Errorf("merged on empty input: got %d want 0", merged)
	}
}

func TestNormalizeTimelineContainers(t *testing.T) {
	// nil + empty fall back to the default triad.
	if got := normalizeTimelineContainers(nil); !equalStrings(got, wowEventsTimelineContainers) {
		t.Errorf("nil input: got %v want triad %v", got, wowEventsTimelineContainers)
	}
	if got := normalizeTimelineContainers([]string{}); !equalStrings(got, wowEventsTimelineContainers) {
		t.Errorf("empty input: got %v want triad %v", got, wowEventsTimelineContainers)
	}
	// All-blank cleans to nothing → triad fallback (not an empty query set).
	if got := normalizeTimelineContainers([]string{"", "   "}); !equalStrings(got, wowEventsTimelineContainers) {
		t.Errorf("all-blank input: got %v want triad %v", got, wowEventsTimelineContainers)
	}
	// Trim + dedup preserving first-seen order. The dup would otherwise be
	// fetched twice and double-count its events in the merge.
	got := normalizeTimelineContainers([]string{" ac-worldserver ", "ac-database", "ac-worldserver"})
	want := []string{"ac-worldserver", "ac-database"}
	if !equalStrings(got, want) {
		t.Errorf("trim+dedup: got %v want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Description must mention the stack triad, the DB-cascade exit code, and the
// sibling tool it supersedes so the agent picks `wow_events_timeline` over
// three `container_events` calls when the operator says "walk me through the
// whole stack outage". Anthropic's SDK reads the description for tool selection;
// load-bearing keywords need a regression guard.
func TestWowEventsTimelineTool_DescriptionMentionsStackForensics(t *testing.T) {
	reg := NewRegistry()
	RegisterWowEventsTimelineTool(reg, ContainerDeps{})
	tool, ok := reg.Get("wow_events_timeline")
	if !ok {
		t.Fatal("wow_events_timeline not registered")
	}
	for _, kw := range []string{"ac-database", "ac-worldserver", "ac-authserver", "timeline", "container_events", "137", "Read-only"} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}

// Annotation must be the read-only marker — the timeline is a daemon query, not
// a state change. AnnAction would gate it behind OPS_ADMIN_ALLOW_ACTIONS, a
// usability regression for a read tool.
func TestWowEventsTimelineTool_IsReadOnly(t *testing.T) {
	reg := NewRegistry()
	RegisterWowEventsTimelineTool(reg, ContainerDeps{})
	tool, ok := reg.Get("wow_events_timeline")
	if !ok {
		t.Fatal("wow_events_timeline not registered")
	}
	if tool.Destructive {
		t.Errorf("Destructive: got true want false (read-only tool)")
	}
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint:true: %s", tool.Annotations)
	}
}
