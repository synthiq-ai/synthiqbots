package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/synthiq-ai/synthiqbots/apps/ops-api/internal/dockerlog"
)

const (
	// wowAuthFailedAttemptsDefaultContainer is the authserver, NOT the
	// worldserver. The failed-login lines this tool aggregates are emitted by
	// the SRP6 logon path in ac-authserver (AuthSession.cpp), not by the
	// world process. Override via the `name` arg for a renamed container.
	wowAuthFailedAttemptsDefaultContainer = "ac-authserver"

	// wowAuthFailedAttemptsDefaultSince — the brute-force-triage question shape
	// ("who's been hammering auth?") usually spans more than the 1h default the
	// generic ops_log_errors_top uses; a day is the natural overnight-attack
	// window. Still bounded by the same 7d ceiling (opsLogErrorsTopMaxWindow)
	// via the shared resolveErrorsSince parser.
	wowAuthFailedAttemptsDefaultSince = "24h"

	// wowAuthFailedAttemptsDefaultMaxScan / HardMax mirror the sibling grep
	// tools so a crash-loop or flood window stays under ~10s wall clock.
	wowAuthFailedAttemptsDefaultMaxScan = 200_000
	wowAuthFailedAttemptsHardMaxScan    = 1_000_000

	// wowAuthFailedAttemptsDefaultTopN / MaxTopN — top attacking IPs an operator
	// scans visually. Matches the wow_failed_logins_top default cardinality.
	wowAuthFailedAttemptsDefaultTopN = 25
	wowAuthFailedAttemptsMaxTopN     = 200

	// wowAuthFailedAttemptsDefaultSampleLimit / MaxSampleLimit — raw sample
	// lines kept per IP bucket so the operator can eyeball the exact phrasing
	// (banned vs invalid-password vs invalid-session) without a follow-up grep.
	wowAuthFailedAttemptsDefaultSampleLimit = 3
	wowAuthFailedAttemptsMaxSampleLimit     = 10

	// wowAuthFailedAttemptsMaxAccountsPerIP caps the per-IP targeted-account
	// breakdown surfaced in the response. distinctAccounts always carries the
	// pre-cap total so a credential-stuffing IP hitting 500 accounts still
	// reports the true breadth even though only the top N names are listed.
	wowAuthFailedAttemptsMaxAccountsPerIP = 20

	// wowAuthFailedAttemptsMaxSampleBytes clips each emitted sample line. Auth
	// failure lines are short (one statement) but the cap keeps the envelope
	// bounded if a custom pattern matches a fat line. Same 200B budget +
	// rune-boundary back-up as the sibling tools (clipBytes).
	wowAuthFailedAttemptsMaxSampleBytes = 200

	// wowAuthFailedAttemptsDefaultPattern is the severity filter applied when
	// the caller passes no `pattern`. It is anchored on the literal phrasings
	// AzerothCore's authserver emits on a rejected logon (AuthSession.cpp):
	//   '<ip>:<port>' [AuthChallenge] account <name> tried to login with invalid password!   (LOG_INFO server.authserver.hack)
	//   '<ip>:<port>' [AuthChallenge] Banned account <name> tried to login!                  (LOG_INFO server.authserver.banned)
	//   '<ip>:<port>' [AuthChallenge] Temporarily banned account <name> tried to login!      (LOG_INFO)
	//   [AuthSession::CheckIpCallback] Banned ip '<ip>:<port>' tries to login!               (LOG_DEBUG session)
	//   '<ip>:<port>' [AuthChallenge] account <name> got banned for ... failed to authenticate ... times  (LOG_DEBUG)
	//   '<ip>:<port>' [ERROR] user <name> tried to login, but session is invalid.            (LOG_ERROR server.authserver.hack)
	//   [AuthChallenge] Account '<name>' is locked to IP ...                                 (LOG_DEBUG)
	// "successfully authenticated" is the SUCCESS line and contains none of
	// these tokens, so it never matches. Override `pattern` for a custom scope.
	wowAuthFailedAttemptsDefaultPattern = `(?i)(invalid password|tried to login|tries to login|failed to authenticate|banned account|banned ip|session is invalid|is locked to ip)`
)

var (
	// reAuthFailedIPv4 extracts the FIRST dotted-quad in a line — the
	// single-quoted '<ip>:<port>' client prefix on authserver failure lines.
	// Capture group 1 is the bare IP; the optional :port that follows is left
	// outside the group so buckets coalesce per source host, not per ephemeral
	// client port. Generic-by-design: independent of the exact log phrasing, so
	// it survives a wording change as long as the IP:port prefix stays.
	reAuthFailedIPv4 = regexp.MustCompile(`\b(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})\b`)

	// reAuthFailedAccount best-effort extracts the targeted account/username.
	// Anchored on "account <name>" / "user <name>" immediately preceding a
	// failure verb so we don't grab the wrong token. Best-effort: a line whose
	// account can't be parsed still counts toward the IP bucket (the primary
	// key), it just doesn't add to that IP's targeted-account set.
	reAuthFailedAccount = regexp.MustCompile(`(?i)\b(?:account|user)\s+'?([A-Za-z0-9_.\-]{1,32})'?\s+(?:tried|got banned|failed to authenticate|is locked)`)
)

// RegisterWowAuthFailedAttemptsLogTool registers `wow_auth_failed_attempts_log`
// — aggregate ac-authserver failed-login log lines BY SOURCE IP over a window.
//
// The operator question this answers: "which IPs have been hammering auth, and
// are they spraying many accounts (credential stuffing) or grinding one
// (targeted brute force)?" — the historical, log-derived complement to
// wow_failed_logins_top.
//
// wow_failed_logins_top reads acore_auth.account.failed_logins: a point-in-time
// counter the auth server RESETS to 0 on the next successful login, and it only
// carries last_ip (the single most-recent source). This tool reads the daemon
// log stream instead, so it (a) survives the counter reset, capturing attempts
// the DB has already forgotten, and (b) attributes every attempt to its OWN
// source IP, exposing the full set of attacking hosts — the brute-force-SOURCE
// view the DB counter structurally cannot give.
//
// Distinct from ops_log_errors_top, which buckets by NORMALIZED line (and
// normalizes the IP away to <ip>, losing the source) — this buckets by the
// extracted source IP and sub-aggregates the targeted accounts per IP.
//
// Docker-SDK based (ContainerDeps), so it builds/works against a DOWN auth
// stack: `docker logs` serves historical lines for stopped/exited containers,
// which is exactly the forensic case ("what hit us before authserver died?").
func RegisterWowAuthFailedAttemptsLogTool(reg *Registry, deps ContainerDeps) {
	if deps.DefaultContainer == "" {
		deps.DefaultContainer = wowAuthFailedAttemptsDefaultContainer
	}

	reg.Register(Tool{
		Name: "wow_auth_failed_attempts_log",
		Description: "Aggregate ac-authserver failed-login log lines BY SOURCE IP across a window, for " +
			"brute-force / credential-stuffing triage. Lines passing the severity filter (default RE2 " +
			"anchored on AzerothCore authserver phrasings: \"invalid password\", \"tried to login\", " +
			"\"banned account\", \"failed to authenticate\", \"session is invalid\", \"is locked to ip\") " +
			"are parsed for their '<ip>:<port>' client prefix; each is tallied per source IP with the set " +
			"of targeted accounts (best-effort extracted). Returns {name, since, until, scanned, " +
			"matchCount, unattributed, distinctIps, topN, ips:[{ip, attempts, distinctAccounts, accounts:" +
			"[{account, attempts}], firstTs, lastTs, samples}]} sorted by attempts desc (ip asc " +
			"tiebreaker). distinctAccounts per IP distinguishes credential stuffing (many accounts) from " +
			"targeted brute force (one account). HISTORICAL, log-derived complement to wow_failed_logins_top " +
			"(which reads acore_auth.account.failed_logins — a counter the auth server RESETS on the next " +
			"successful login and which only keeps last_ip): this survives the reset and exposes every " +
			"attacking IP, not just the most recent. Distinct from ops_log_errors_top (buckets by " +
			"normalized line and normalizes the IP away). Built on the same docker log stream primitive as " +
			"container_logs_grep, so it works against a DOWN auth stack (docker logs serves stopped " +
			"containers). `unattributed` counts matched lines with no extractable IP — if it approaches " +
			"matchCount the log format drifted and `pattern` needs tuning. Default container ac-authserver, " +
			"window 24h (max 7d). topN default 25 (max 200), sampleLimit default 3 (max 10). Bounded by " +
			"maxScanLines (default 200000, max 1000000). Read-only.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"Container name or id (default ac-authserver)"},
			"pattern":{"type":"string","description":"RE2 severity filter (default matches AzerothCore authserver failed-login phrasings)"},
			"caseInsensitive":{"type":"boolean","description":"Fold case before matching (default false; the default pattern already includes (?i))"},
			"since":{"type":"string","description":"Lookback (\"12h\", \"7d\", \"30m\") or unix seconds (default 24h, max 7d)"},
			"topN":{"type":"integer","description":"Number of source IPs to return (default 25, max 200)"},
			"sampleLimit":{"type":"integer","description":"Sample raw lines kept per IP (default 3, max 10)"},
			"maxScanLines":{"type":"integer","description":"Cap on lines SCANNED before bailing (default 200000, max 1000000)"},
			"stream":{"type":"string","description":"Restrict to \"stdout\" or \"stderr\" (default both)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Name            string `json:"name"`
				Pattern         string `json:"pattern"`
				CaseInsensitive bool   `json:"caseInsensitive"`
				Since           string `json:"since"`
				TopN            int    `json:"topN"`
				SampleLimit     int    `json:"sampleLimit"`
				MaxScanLines    int    `json:"maxScanLines"`
				Stream          string `json:"stream"`
			}
			_ = json.Unmarshal(raw, &a)

			name := pickContainer(a.Name, deps.DefaultContainer)

			pattern := strings.TrimSpace(a.Pattern)
			if pattern == "" {
				pattern = wowAuthFailedAttemptsDefaultPattern
			}
			matcher, err := buildErrorsMatcher(pattern, a.CaseInsensitive)
			if err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}

			// Reuse the shared since-parser (Go duration / "Nd" / unix epoch,
			// 7d cap, future+garbage rejected). Substitute the 24h default for
			// an empty arg so we don't have to fork the parser just to change
			// its default constant.
			sinceArg := strings.TrimSpace(a.Since)
			if sinceArg == "" {
				sinceArg = wowAuthFailedAttemptsDefaultSince
			}
			sinceUnix, err := resolveErrorsSince(sinceArg)
			if err != nil {
				return map[string]any{"error": "since: " + err.Error(), "name": name}
			}

			streamFilter, err := normalizeErrorsStreamFilter(a.Stream)
			if err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}

			topN := clampErrorsInt(a.TopN, wowAuthFailedAttemptsDefaultTopN, 1, wowAuthFailedAttemptsMaxTopN)
			sampleLimit := clampErrorsInt(a.SampleLimit, wowAuthFailedAttemptsDefaultSampleLimit, 1, wowAuthFailedAttemptsMaxSampleLimit)
			maxScan := clampErrorsInt(a.MaxScanLines, wowAuthFailedAttemptsDefaultMaxScan, 1, wowAuthFailedAttemptsHardMaxScan)

			scanCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			ch, _, err := deps.Docker.Stream(scanCtx, name, dockerlog.LogOpts{
				Tail:  "all",
				Since: strconv.FormatInt(sinceUnix, 10),
			})
			if err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}

			ips, stats := aggregateAuthFailedAttempts(ch, matcher, streamFilter, sampleLimit, maxScan, cancel)
			result := topAuthFailedAttempts(ips, topN)

			streamReported := streamFilter
			if streamReported == "" {
				streamReported = "both"
			}

			return map[string]any{
				"name":            name,
				"since":           sinceUnix,
				"until":           time.Now().Unix(),
				"pattern":         pattern,
				"caseInsensitive": a.CaseInsensitive,
				"stream":          streamReported,
				"scanned":         stats.scanned,
				"matchCount":      stats.matchCount,
				"unattributed":    stats.unattributed,
				"distinctIps":     len(ips),
				"scanTruncated":   stats.scanTruncated,
				"topN":            topN,
				"sampleLimit":     sampleLimit,
				"ips":             result,
			}
		},
	})
}

// authFailedAttemptStat holds the per-source-IP aggregates during a scan.
// accounts maps targeted account name -> per-IP attempt count for that account.
type authFailedAttemptStat struct {
	ip       string
	count    int
	accounts map[string]int
	samples  []string
	firstTs  time.Time
	lastTs   time.Time
}

// authFailedAttemptStats accounts for what the aggregator saw vs. attributed.
// `unattributed` is matched lines with no extractable IP — surfaced so the
// operator can tell when the log format has drifted out from under the parser.
type authFailedAttemptStats struct {
	scanned       int
	matchCount    int
	unattributed  int
	scanTruncated bool
}

// aggregateAuthFailedAttempts drains the line channel, filters by matcher +
// stream, extracts the source IP (and best-effort the targeted account) from
// each hit, and tallies per-IP. `cancel` is invoked on scan-cap so the producer
// goroutine exits instead of wedging on a full channel.
//
// A matched line whose IP can't be parsed is counted in `unattributed` and does
// NOT create a bucket — the IP is this tool's primary key, and a sentinel
// "(unknown)" bucket would silently masquerade as a real attacker.
func aggregateAuthFailedAttempts(
	lines <-chan dockerlog.Line,
	matcher func(string) bool,
	streamFilter string,
	sampleLimit, maxScan int,
	cancel context.CancelFunc,
) (map[string]*authFailedAttemptStat, authFailedAttemptStats) {
	ips := make(map[string]*authFailedAttemptStat)
	stats := authFailedAttemptStats{}

	for ln := range lines {
		stats.scanned++
		if stats.scanned > maxScan {
			stats.scanTruncated = true
			cancel()
			break
		}
		if streamFilter != "" && ln.Stream != streamFilter {
			continue
		}
		if !matcher(ln.Msg) {
			continue
		}
		stats.matchCount++

		ip := extractAuthFailedIP(ln.Msg)
		if ip == "" {
			stats.unattributed++
			continue
		}

		entry, ok := ips[ip]
		if !ok {
			entry = &authFailedAttemptStat{ip: ip, accounts: map[string]int{}}
			ips[ip] = entry
		}
		entry.count++
		if acct := extractAuthFailedAccount(ln.Msg); acct != "" {
			entry.accounts[acct]++
		}
		if len(entry.samples) < sampleLimit {
			entry.samples = append(entry.samples, clipBytes(ln.Msg, wowAuthFailedAttemptsMaxSampleBytes))
		}
		if !ln.TS.IsZero() {
			if entry.firstTs.IsZero() || ln.TS.Before(entry.firstTs) {
				entry.firstTs = ln.TS
			}
			if ln.TS.After(entry.lastTs) {
				entry.lastTs = ln.TS
			}
		}
	}

	return ips, stats
}

// extractAuthFailedIP returns the first dotted-quad in the line (the source IP
// from the '<ip>:<port>' prefix), or "" if none / out-of-range octets. The
// octet bound rejects stray dotted numbers that aren't real IPv4 addresses.
func extractAuthFailedIP(msg string) string {
	m := reAuthFailedIPv4.FindStringSubmatch(msg)
	if m == nil {
		return ""
	}
	for _, oct := range strings.Split(m[1], ".") {
		n, err := strconv.Atoi(oct)
		if err != nil || n > 255 {
			return ""
		}
	}
	return m[1]
}

// extractAuthFailedAccount best-effort returns the targeted account/username,
// or "" if the line doesn't carry one (IP-level bans, format drift).
func extractAuthFailedAccount(msg string) string {
	m := reAuthFailedAccount.FindStringSubmatch(msg)
	if m == nil {
		return ""
	}
	return m[1]
}

// topAuthFailedAttempts sorts the per-IP buckets by attempts desc with ip-asc
// as the deterministic tiebreaker (Go map iteration is randomized, so without
// it tied-count rows would flake across runs), truncates to topN, and projects
// to the response shape — including a per-IP top targeted-account breakdown.
func topAuthFailedAttempts(ips map[string]*authFailedAttemptStat, topN int) []map[string]any {
	if len(ips) == 0 {
		return []map[string]any{}
	}
	keys := make([]string, 0, len(ips))
	for k := range ips {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ki, kj := keys[i], keys[j]
		if ips[ki].count != ips[kj].count {
			return ips[ki].count > ips[kj].count
		}
		return ki < kj
	})
	if len(keys) > topN {
		keys = keys[:topN]
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		p := ips[k]
		row := map[string]any{
			"ip":               p.ip,
			"attempts":         p.count,
			"distinctAccounts": len(p.accounts),
			"accounts":         topAuthFailedAccounts(p.accounts, wowAuthFailedAttemptsMaxAccountsPerIP),
			"samples":          p.samples,
		}
		if !p.firstTs.IsZero() {
			row["firstTs"] = p.firstTs.UTC().Format(time.RFC3339Nano)
		}
		if !p.lastTs.IsZero() {
			row["lastTs"] = p.lastTs.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, row)
	}
	return out
}

// topAuthFailedAccounts projects an IP's targeted-account map to a sorted,
// capped list — attempts desc with account-asc tiebreaker (same map-iter
// determinism guard as the IP sort). Returns an empty (non-nil) slice when no
// account was extractable for the IP so the JSON shape stays stable.
func topAuthFailedAccounts(accounts map[string]int, maxAccounts int) []map[string]any {
	if len(accounts) == 0 {
		return []map[string]any{}
	}
	keys := make([]string, 0, len(accounts))
	for k := range accounts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		ki, kj := keys[i], keys[j]
		if accounts[ki] != accounts[kj] {
			return accounts[ki] > accounts[kj]
		}
		return ki < kj
	})
	if len(keys) > maxAccounts {
		keys = keys[:maxAccounts]
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{"account": k, "attempts": accounts[k]})
	}
	return out
}
