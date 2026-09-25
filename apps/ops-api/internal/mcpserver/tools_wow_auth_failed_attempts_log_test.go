package mcpserver

import (
	"strings"
	"testing"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
)

// Real AzerothCore authserver failure lines (AuthSession.cpp) — the fixtures
// the default pattern + extractors are built against. Keeping them verbatim
// here means a future log-format change that breaks extraction trips a test.
const (
	authLineInvalidPw   = `'203.0.113.7:51001' [AuthChallenge] account ALICE tried to login with invalid password!`
	authLineBannedAcct  = `'203.0.113.7:51005' [AuthChallenge] Banned account ALICE tried to login!`
	authLineSessionInv  = `'198.51.100.42:40003' [ERROR] user ADMIN tried to login, but session is invalid.`
	authLineBannedIP    = `[AuthSession::CheckIpCallback] Banned ip '192.0.2.5:33333' tries to login!`
	authLineSuccess     = `'10.0.0.9:22222' User 'OPERATOR' successfully authenticated`
	authLineNoIPMatched = `[AuthChallenge] account GHOST tried to login with invalid password!`
)

// extractAuthFailedIP must pull the bare source IP from the '<ip>:<port>'
// prefix, drop the port, and reject out-of-range octets / non-IP lines.
func TestExtractAuthFailedIP(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{authLineInvalidPw, "203.0.113.7"},
		{authLineBannedIP, "192.0.2.5"},
		{authLineSessionInv, "198.51.100.42"},
		{"no ip in this line at all", ""},
		{"out of range 999.1.2.3 boom", ""}, // 999 > 255
		{"partial 3.3.5 build string", ""},  // only 3 octets
	}
	for _, c := range cases {
		if got := extractAuthFailedIP(c.in); got != c.want {
			t.Errorf("extractAuthFailedIP(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

// extractAuthFailedAccount best-effort pulls the targeted account/username and
// returns "" for IP-level bans (no account in the line).
func TestExtractAuthFailedAccount(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{authLineInvalidPw, "ALICE"},
		{authLineBannedAcct, "ALICE"},
		{authLineSessionInv, "ADMIN"},
		{`'198.51.100.4:5' [AuthChallenge] account Bot_42 got banned for '600' seconds because it failed to authenticate '5' times`, "Bot_42"},
		{authLineBannedIP, ""}, // IP-level ban carries no account
		{authLineSuccess, ""},  // success line: not a failure, no extraction
	}
	for _, c := range cases {
		if got := extractAuthFailedAccount(c.in); got != c.want {
			t.Errorf("extractAuthFailedAccount(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

// Default pattern matches every authserver failure phrasing but NOT the success
// line — the load-bearing precision property (a false match on "successfully
// authenticated" would attribute legitimate logins to attackers).
func TestWowAuthFailedDefaultPattern(t *testing.T) {
	m, err := buildErrorsMatcher(wowAuthFailedAttemptsDefaultPattern, false)
	if err != nil {
		t.Fatalf("matcher: %v", err)
	}
	cases := []struct {
		in   string
		want bool
	}{
		{authLineInvalidPw, true},
		{authLineBannedAcct, true},
		{authLineSessionInv, true},
		{authLineBannedIP, true},
		{authLineNoIPMatched, true},
		{authLineSuccess, false},
		{`'198.51.100.4:5' [AuthChallenge] Account 'OPERATOR' is not locked to ip`, false}, // "is not locked" != "is locked to ip"
		{"INFO heartbeat", false},
	}
	for _, c := range cases {
		if got := m(c.in); got != c.want {
			t.Errorf("default pattern match(%q): got %v want %v", c.in, got, c.want)
		}
	}
}

// Aggregator buckets by source IP: two hits from the same IP coalesce into one
// bucket; a different IP forms a second bucket.
func TestAggregateAuthFailedAttempts_BucketsByIP(t *testing.T) {
	matcher, _ := buildErrorsMatcher(wowAuthFailedAttemptsDefaultPattern, false)
	lines := []dockerlog.Line{
		errLine("stdout", authLineInvalidPw), // 203.0.113.7
		errLine("stdout", `'203.0.113.7:51002' [AuthChallenge] account BOB tried to login with invalid password!`), // 203.0.113.7
		errLine("stdout", authLineSessionInv), // 198.51.100.42
	}
	ips, stats := aggregateAuthFailedAttempts(feedErrorLines(lines), matcher, "", 3, 1000, func() {})
	if stats.scanned != 3 || stats.matchCount != 3 || stats.unattributed != 0 {
		t.Errorf("stats: scanned=%d matchCount=%d unattributed=%d want 3/3/0", stats.scanned, stats.matchCount, stats.unattributed)
	}
	if len(ips) != 2 {
		t.Fatalf("buckets: want 2 IPs got %d", len(ips))
	}
	if ips["203.0.113.7"].count != 2 {
		t.Errorf("203.0.113.7 attempts: want 2 got %d", ips["203.0.113.7"].count)
	}
	if ips["198.51.100.42"].count != 1 {
		t.Errorf("198.51.100.42 attempts: want 1 got %d", ips["198.51.100.42"].count)
	}
}

// A matched line with no extractable IP must increment `unattributed` and must
// NOT create a phantom bucket — the graceful-degradation signal that tells the
// operator the log format drifted out from under the IP extractor.
func TestAggregateAuthFailedAttempts_UnattributedNoBucket(t *testing.T) {
	matcher, _ := buildErrorsMatcher(wowAuthFailedAttemptsDefaultPattern, false)
	lines := []dockerlog.Line{
		errLine("stdout", authLineInvalidPw),   // attributed
		errLine("stdout", authLineNoIPMatched), // matched, no IP
	}
	ips, stats := aggregateAuthFailedAttempts(feedErrorLines(lines), matcher, "", 3, 1000, func() {})
	if stats.matchCount != 2 {
		t.Errorf("matchCount: want 2 got %d", stats.matchCount)
	}
	if stats.unattributed != 1 {
		t.Errorf("unattributed: want 1 got %d", stats.unattributed)
	}
	if len(ips) != 1 {
		t.Errorf("buckets: want 1 (no phantom bucket for the IP-less line) got %d", len(ips))
	}
}

// Per-IP account sub-aggregation: same IP hitting two accounts → distinctAccounts
// 2; the same account twice → that account's per-IP count is 2.
func TestAggregateAuthFailedAttempts_PerIPAccountTally(t *testing.T) {
	matcher, _ := buildErrorsMatcher(wowAuthFailedAttemptsDefaultPattern, false)
	lines := []dockerlog.Line{
		errLine("stdout", authLineInvalidPw),  // 203.0.113.7 / ALICE
		errLine("stdout", authLineBannedAcct), // 203.0.113.7 / ALICE (2nd)
		errLine("stdout", `'203.0.113.7:51002' [AuthChallenge] account BOB tried to login with invalid password!`), // 203.0.113.7 / BOB
	}
	ips, _ := aggregateAuthFailedAttempts(feedErrorLines(lines), matcher, "", 3, 1000, func() {})
	a := ips["203.0.113.7"]
	if a == nil {
		t.Fatal("missing 203.0.113.7 bucket")
	}
	if a.count != 3 {
		t.Errorf("attempts: want 3 got %d", a.count)
	}
	if len(a.accounts) != 2 {
		t.Errorf("distinctAccounts: want 2 got %d", len(a.accounts))
	}
	if a.accounts["ALICE"] != 2 {
		t.Errorf("ALICE per-IP count: want 2 got %d", a.accounts["ALICE"])
	}
	if a.accounts["BOB"] != 1 {
		t.Errorf("BOB per-IP count: want 1 got %d", a.accounts["BOB"])
	}
}

// Stream filter narrows the scan — stderr-only must drop stdout hits.
func TestAggregateAuthFailedAttempts_StreamFilter(t *testing.T) {
	matcher, _ := buildErrorsMatcher(wowAuthFailedAttemptsDefaultPattern, false)
	lines := []dockerlog.Line{
		errLine("stdout", authLineInvalidPw),
		errLine("stderr", authLineSessionInv),
	}
	ips, stats := aggregateAuthFailedAttempts(feedErrorLines(lines), matcher, "stderr", 3, 1000, func() {})
	if stats.matchCount != 1 {
		t.Errorf("matchCount: want 1 (stderr-only) got %d", stats.matchCount)
	}
	if _, ok := ips["198.51.100.42"]; !ok {
		t.Errorf("expected the stderr IP bucket to survive, got %v", ips)
	}
}

// Scan-cap: aggregator bails at maxScan, flags scanTruncated, and cancels the
// producer so it doesn't wedge on a full channel.
func TestAggregateAuthFailedAttempts_ScanCapBails(t *testing.T) {
	matcher, _ := buildErrorsMatcher(wowAuthFailedAttemptsDefaultPattern, false)
	lines := make([]dockerlog.Line, 50)
	for i := range lines {
		lines[i] = errLine("stdout", authLineInvalidPw)
	}
	cancelCalled := false
	ips, stats := aggregateAuthFailedAttempts(feedErrorLines(lines), matcher, "", 3, 10, func() { cancelCalled = true })
	if !stats.scanTruncated {
		t.Error("expected scanTruncated=true after maxScan exceeded")
	}
	if !cancelCalled {
		t.Error("expected cancel() on scan-cap hit")
	}
	if stats.scanned <= 10 {
		t.Errorf("scanned: want >10 (one over the cap) got %d", stats.scanned)
	}
	if len(ips) == 0 {
		t.Error("expected at least one bucket from lines before the cap")
	}
}

// First/last TS tracking per bucket, with the zero-TS guard.
func TestAggregateAuthFailedAttempts_FirstLastTimestamps(t *testing.T) {
	matcher, _ := buildErrorsMatcher(wowAuthFailedAttemptsDefaultPattern, false)
	t1 := time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 6, 6, 11, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	lines := []dockerlog.Line{
		errLineAt("stdout", authLineInvalidPw, t2),
		errLineAt("stdout", authLineInvalidPw, t1), // earliest
		errLineAt("stdout", authLineInvalidPw, t3), // latest
		errLine("stdout", authLineInvalidPw),       // zero TS — must NOT clobber
	}
	ips, _ := aggregateAuthFailedAttempts(feedErrorLines(lines), matcher, "", 3, 1000, func() {})
	a := ips["203.0.113.7"]
	if a == nil {
		t.Fatal("missing bucket")
	}
	if !a.firstTs.Equal(t1) {
		t.Errorf("firstTs: want %v got %v", t1, a.firstTs)
	}
	if !a.lastTs.Equal(t3) {
		t.Errorf("lastTs: want %v got %v", t3, a.lastTs)
	}
}

// Sample cap: samples per IP stop growing at sampleLimit even as attempts climb.
func TestAggregateAuthFailedAttempts_SampleLimitCaps(t *testing.T) {
	matcher, _ := buildErrorsMatcher(wowAuthFailedAttemptsDefaultPattern, false)
	lines := make([]dockerlog.Line, 5)
	for i := range lines {
		lines[i] = errLine("stdout", authLineInvalidPw)
	}
	ips, _ := aggregateAuthFailedAttempts(feedErrorLines(lines), matcher, "", 2, 1000, func() {})
	a := ips["203.0.113.7"]
	if a.count != 5 {
		t.Errorf("attempts: want 5 got %d", a.count)
	}
	if len(a.samples) != 2 {
		t.Errorf("samples cap broken: want 2 got %d", len(a.samples))
	}
}

// topAuthFailedAttempts: attempts desc with ip-asc tiebreaker on tied counts.
func TestTopAuthFailedAttempts_SortDescTiebreaker(t *testing.T) {
	ips := map[string]*authFailedAttemptStat{
		"10.0.0.3": {ip: "10.0.0.3", count: 5, accounts: map[string]int{}},
		"10.0.0.1": {ip: "10.0.0.1", count: 5, accounts: map[string]int{}}, // tied with .3 → ip-asc wins
		"10.0.0.2": {ip: "10.0.0.2", count: 9, accounts: map[string]int{}},
		"10.0.0.4": {ip: "10.0.0.4", count: 1, accounts: map[string]int{}},
	}
	got := topAuthFailedAttempts(ips, 10)
	want := []string{"10.0.0.2", "10.0.0.1", "10.0.0.3", "10.0.0.4"}
	if len(got) != len(want) {
		t.Fatalf("rows: want %d got %d", len(want), len(got))
	}
	for i, w := range want {
		if got[i]["ip"] != w {
			t.Errorf("row %d: want %q got %v", i, w, got[i]["ip"])
		}
	}
}

// topAuthFailedAttempts truncates to topN (after sorting).
func TestTopAuthFailedAttempts_TruncatesToTopN(t *testing.T) {
	ips := map[string]*authFailedAttemptStat{
		"10.0.0.1": {ip: "10.0.0.1", count: 10, accounts: map[string]int{}},
		"10.0.0.2": {ip: "10.0.0.2", count: 9, accounts: map[string]int{}},
		"10.0.0.3": {ip: "10.0.0.3", count: 8, accounts: map[string]int{}},
	}
	got := topAuthFailedAttempts(ips, 2)
	if len(got) != 2 {
		t.Fatalf("topN truncation: want 2 got %d", len(got))
	}
	if got[0]["ip"] != "10.0.0.1" || got[1]["ip"] != "10.0.0.2" {
		t.Errorf("wrong top-2: %v %v", got[0]["ip"], got[1]["ip"])
	}
}

// Empty input → empty (non-nil) slice so JSON consumers don't have to nil-check.
func TestTopAuthFailedAttempts_EmptyReturnsEmptySlice(t *testing.T) {
	got := topAuthFailedAttempts(map[string]*authFailedAttemptStat{}, 10)
	if got == nil {
		t.Error("want non-nil empty slice, got nil")
	}
	if len(got) != 0 {
		t.Errorf("want length 0, got %d", len(got))
	}
}

// timestamp keys present only when tracked; omitted entirely for zero-TS buckets.
func TestTopAuthFailedAttempts_EmitsTimestampsWhenTracked(t *testing.T) {
	ts := time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC)
	ips := map[string]*authFailedAttemptStat{
		"10.0.0.1": {ip: "10.0.0.1", count: 1, accounts: map[string]int{}, firstTs: ts, lastTs: ts},
		"10.0.0.2": {ip: "10.0.0.2", count: 1, accounts: map[string]int{}},
	}
	got := topAuthFailedAttempts(ips, 10)
	var withTs, noTs map[string]any
	for _, r := range got {
		switch r["ip"] {
		case "10.0.0.1":
			withTs = r
		case "10.0.0.2":
			noTs = r
		}
	}
	if withTs == nil || noTs == nil {
		t.Fatalf("missing rows: with=%v no=%v", withTs, noTs)
	}
	if _, ok := withTs["firstTs"]; !ok {
		t.Error("tracked row missing firstTs")
	}
	if _, ok := noTs["firstTs"]; ok {
		t.Error("zero-TS row must omit firstTs entirely")
	}
}

// topAuthFailedAccounts: attempts desc with account-asc tiebreaker, capped,
// empty → non-nil slice.
func TestTopAuthFailedAccounts_SortCapEmpty(t *testing.T) {
	if got := topAuthFailedAccounts(map[string]int{}, 5); got == nil || len(got) != 0 {
		t.Errorf("empty: want non-nil len-0 slice, got %v", got)
	}
	accts := map[string]int{"BOB": 3, "ALICE": 3, "CAROL": 9, "DAVE": 1}
	got := topAuthFailedAccounts(accts, 3)
	if len(got) != 3 {
		t.Fatalf("cap: want 3 got %d", len(got))
	}
	// CAROL(9) first, then ALICE(3) before BOB(3) by account-asc, DAVE(1) drops.
	want := []string{"CAROL", "ALICE", "BOB"}
	for i, w := range want {
		if got[i]["account"] != w {
			t.Errorf("row %d: want %q got %v", i, w, got[i]["account"])
		}
	}
}

// Golden end-to-end (aggregate → top): a credential-stuffing IP (many accounts),
// a targeted IP (one account), an IP-level ban (no account), a success line that
// must be ignored, and an IP-less matched line that lands in `unattributed`.
func TestWowAuthFailedAttempts_GoldenPath(t *testing.T) {
	matcher, _ := buildErrorsMatcher(wowAuthFailedAttemptsDefaultPattern, false)
	lines := []dockerlog.Line{
		errLine("stdout", authLineInvalidPw), // A / ALICE
		errLine("stdout", `'203.0.113.7:51002' [AuthChallenge] account BOB tried to login with invalid password!`),   // A / BOB
		errLine("stdout", `'203.0.113.7:51003' [AuthChallenge] account CAROL tried to login with invalid password!`), // A / CAROL
		errLine("stdout", `'203.0.113.7:51004' [AuthChallenge] account ALICE tried to login with invalid password!`), // A / ALICE
		errLine("stdout", authLineBannedAcct), // A / ALICE (banned)
		errLine("stdout", `'198.51.100.42:40001' [AuthChallenge] account ADMIN tried to login with invalid password!`), // B / ADMIN
		errLine("stdout", `'198.51.100.42:40002' [AuthChallenge] account ADMIN tried to login with invalid password!`), // B / ADMIN
		errLine("stdout", authLineSessionInv),  // B / ADMIN
		errLine("stdout", authLineBannedIP),    // C / (no account)
		errLine("stdout", authLineSuccess),     // ignored
		errLine("stdout", authLineNoIPMatched), // unattributed
	}
	ips, stats := aggregateAuthFailedAttempts(feedErrorLines(lines), matcher, "", 3, 100000, func() {})

	if stats.scanned != 11 {
		t.Errorf("scanned: want 11 got %d", stats.scanned)
	}
	if stats.matchCount != 10 {
		t.Errorf("matchCount: want 10 (all but the success line) got %d", stats.matchCount)
	}
	if stats.unattributed != 1 {
		t.Errorf("unattributed: want 1 got %d", stats.unattributed)
	}
	if len(ips) != 3 {
		t.Fatalf("distinctIps: want 3 got %d", len(ips))
	}

	rows := topAuthFailedAttempts(ips, 25)
	if len(rows) != 3 {
		t.Fatalf("rows: want 3 got %d", len(rows))
	}
	// Ordering: A(5) > B(3) > C(1).
	if rows[0]["ip"] != "203.0.113.7" || rows[0]["attempts"] != 5 {
		t.Errorf("row0: want 203.0.113.7/5 got %v/%v", rows[0]["ip"], rows[0]["attempts"])
	}
	if rows[0]["distinctAccounts"] != 3 {
		t.Errorf("credential-stuffing IP distinctAccounts: want 3 got %v", rows[0]["distinctAccounts"])
	}
	if rows[1]["ip"] != "198.51.100.42" || rows[1]["attempts"] != 3 {
		t.Errorf("row1: want 198.51.100.42/3 got %v/%v", rows[1]["ip"], rows[1]["attempts"])
	}
	if rows[1]["distinctAccounts"] != 1 {
		t.Errorf("targeted IP distinctAccounts: want 1 got %v", rows[1]["distinctAccounts"])
	}
	if rows[2]["ip"] != "192.0.2.5" || rows[2]["attempts"] != 1 {
		t.Errorf("row2: want 192.0.2.5/1 got %v/%v", rows[2]["ip"], rows[2]["attempts"])
	}
	if rows[2]["distinctAccounts"] != 0 {
		t.Errorf("IP-ban row distinctAccounts: want 0 got %v", rows[2]["distinctAccounts"])
	}
	// The credential-stuffing IP's top account is ALICE (3 hits).
	accts, ok := rows[0]["accounts"].([]map[string]any)
	if !ok || len(accts) != 3 {
		t.Fatalf("row0 accounts: want 3-entry slice got %v", rows[0]["accounts"])
	}
	if accts[0]["account"] != "ALICE" || accts[0]["attempts"] != 3 {
		t.Errorf("row0 top account: want ALICE/3 got %v/%v", accts[0]["account"], accts[0]["attempts"])
	}
}

// Description-keyword regression: agent tool-selection latches on these to
// distinguish this tool from its siblings. Presence is load-bearing.
func TestWowAuthFailedAttemptsLogTool_DescriptionMentionsKeyDistinctions(t *testing.T) {
	reg := NewRegistry()
	RegisterWowAuthFailedAttemptsLogTool(reg, ContainerDeps{DefaultContainer: "ac-authserver"})
	tool, ok := reg.Get("wow_auth_failed_attempts_log")
	if !ok {
		t.Fatal("wow_auth_failed_attempts_log not registered")
	}
	for _, kw := range []string{
		"ac-authserver",
		"wow_failed_logins_top",
		"ops_log_errors_top",
		"container_logs_grep",
		"source IP",
		"credential-stuffing",
		"brute-force",
		"invalid password",
		"distinctAccounts",
		"unattributed",
		"topN",
		"Read-only",
	} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

func TestWowAuthFailedAttemptsLogTool_IsReadOnly(t *testing.T) {
	reg := NewRegistry()
	RegisterWowAuthFailedAttemptsLogTool(reg, ContainerDeps{DefaultContainer: "ac-authserver"})
	tool, ok := reg.Get("wow_auth_failed_attempts_log")
	if !ok {
		t.Fatal("wow_auth_failed_attempts_log not registered")
	}
	if tool.Destructive {
		t.Error("Destructive: got true want false (read-only tool)")
	}
	if !strings.Contains(string(tool.Annotations), `"readOnlyHint":true`) {
		t.Errorf("annotations missing readOnlyHint:true: %s", tool.Annotations)
	}
}

// Registration must default the container to ac-authserver when deps are empty
// (NOT the worldserver default the generic container tools fall back to).
func TestWowAuthFailedAttemptsLogTool_DefaultsContainerWhenDepsEmpty(t *testing.T) {
	reg := NewRegistry()
	RegisterWowAuthFailedAttemptsLogTool(reg, ContainerDeps{})
	if _, ok := reg.Get("wow_auth_failed_attempts_log"); !ok {
		t.Fatal("wow_auth_failed_attempts_log not registered when DefaultContainer empty")
	}
}
