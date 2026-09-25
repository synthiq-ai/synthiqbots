package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	dbInnodbStatusTimeout = 5 * time.Second
	// dbInnodbStatusRawMaxBytes caps the raw status text in the response. A
	// healthy server returns ~20-40 KB; one with a giant LATEST DETECTED
	// DEADLOCK or thousands of active transactions can push past 100 KB.
	// Capping at 48 KiB keeps the MCP response under ~64 KiB even after the
	// parsed sections add their own copy of the body.
	dbInnodbStatusRawMaxBytes = 48 * 1024
)

// dbInnodbHistoryList extracts "History list length N" from the TRANSACTIONS
// section. Climbing values signal long-running read views holding undo log —
// the canonical "purge falling behind" symptom.
var dbInnodbHistoryList = regexp.MustCompile(`(?m)^History list length (\d+)\b`)

// dbInnodbPendingIO extracts "Pending normal aio reads: N" / "Pending
// flushes (fsync) log: N, buffer pool: M" from the FILE I/O section. Useful
// for spotting disk-bound stalls.
var dbInnodbPendingReads = regexp.MustCompile(`Pending normal aio reads:\s*\[?(\d+)`)
var dbInnodbPendingWrites = regexp.MustCompile(`Pending normal aio writes:\s*\[?(\d+)`)

// dbInnodbTrxCount counts "---TRANSACTION" markers inside the TRANSACTIONS
// section. Each active or recently-purged transaction emits one. Sleep
// connections don't count — only those holding a trx context.
var dbInnodbTrxMarker = regexp.MustCompile(`(?m)^---TRANSACTION `)

// RegisterDBInnodbStatusTool registers `db_innodb_status` — a parsed wrapper
// over `SHOW ENGINE INNODB STATUS`. Operators previously couldn't run the
// statement via db_query (it's not a SELECT and emits a single 50+ KB row
// that db_query's truncation guards would mangle). This tool splits the
// status text into named sections, surfaces a handful of key counters as
// structured fields, and flags `hasDeadlock:true` when a LATEST DETECTED
// DEADLOCK section is present.
//
// The InnoDB monitor is the canonical first call for deadlock + lock-wait
// forensics, and complements db_processlist (running queries) and
// db_global_status (counters): processlist shows WHO is waiting,
// db_innodb_status shows WHAT they're contending on and any past collision.
func RegisterDBInnodbStatusTool(reg *Registry, deps DBDeps) {
	reg.Register(Tool{
		Name: "db_innodb_status",
		Description: "Parsed snapshot of SHOW ENGINE INNODB STATUS. " +
			"Splits the InnoDB monitor output into named sections " +
			"(BACKGROUND THREAD, SEMAPHORES, LATEST DETECTED DEADLOCK, " +
			"TRANSACTIONS, FILE I/O, LOG, BUFFER POOL AND MEMORY, ROW " +
			"OPERATIONS, etc.) and surfaces structured fields: `hasDeadlock` " +
			"(true when LATEST DETECTED DEADLOCK is present), " +
			"`latestDeadlockText` (the full deadlock block, present iff " +
			"hasDeadlock), `historyListLength` (uncommitted-undo backlog — " +
			"climbing values mean purge is falling behind), `pendingReads` " +
			"and `pendingWrites` (aio queue depth), `activeTransactions` " +
			"(count of ---TRANSACTION markers in the TRANSACTIONS section). " +
			"`section` (optional) returns only one section by name " +
			"(case-insensitive prefix match — e.g. `section:\"buffer pool\"` " +
			"resolves to BUFFER POOL AND MEMORY). `includeRaw` (default true) " +
			"controls whether the full status text is echoed back — set false " +
			"to save tokens when you only want the parsed fields. The raw " +
			"text is capped at 48 KiB with `rawTruncated:true` when elided. " +
			"Canonical first-call for deadlock + lock-wait forensics; pairs " +
			"with db_processlist (who is waiting) and db_global_status " +
			"(rate counters). Read-only, uses the ops_ro pool. ops_ro must " +
			"have the global PROCESS privilege; without it MySQL returns " +
			"`Access denied; you need the PROCESS privilege` and this tool " +
			"surfaces that error verbatim. 5 s timeout.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"section":{"type":"string","description":"Return only the named section (case-insensitive prefix match, e.g. 'transactions', 'buffer pool', 'latest detected deadlock'). Omit to return all sections."},
			"includeRaw":{"type":"boolean","description":"Include the full raw status text in statusRaw. Default true. Set false to drop the raw blob and return only the parsed fields."}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct {
				Section    string `json:"section"`
				IncludeRaw *bool  `json:"includeRaw"`
			}
			if len(raw) > 0 && string(raw) != "null" {
				if err := json.Unmarshal(raw, &a); err != nil {
					return map[string]any{"error": "decode args: " + err.Error()}
				}
			}
			if deps.QueryDB == nil {
				return map[string]any{"error": "ops_ro pool not configured (DB_DSN empty?)"}
			}
			includeRaw := true
			if a.IncludeRaw != nil {
				includeRaw = *a.IncludeRaw
			}
			c, cancel := context.WithTimeout(ctx, dbInnodbStatusTimeout)
			defer cancel()
			out, err := collectInnodbStatus(c, deps.QueryDB, a.Section, includeRaw)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			return out
		},
	})
}

// collectInnodbStatus runs SHOW ENGINE INNODB STATUS, parses the single
// returned row, and packs the response. Split out from the handler so tests
// can drive it with sqlmock.
func collectInnodbStatus(ctx context.Context, db *sql.DB, sectionFilter string, includeRaw bool) (map[string]any, error) {
	row := db.QueryRowContext(ctx, "SHOW ENGINE INNODB STATUS")
	var typ, name, status sql.NullString
	if err := row.Scan(&typ, &name, &status); err != nil {
		return nil, fmt.Errorf("show engine innodb status: %w", err)
	}
	return parseInnodbStatus(status.String, sectionFilter, includeRaw), nil
}

// parseInnodbStatus walks the status text, locates the section dividers,
// and builds the structured response. Pure-function so tests can feed it
// a captured-from-real-server status blob without a DB round trip.
func parseInnodbStatus(status, sectionFilter string, includeRaw bool) map[string]any {
	out := map[string]any{
		"generatedAt": time.Now().UTC(),
	}

	sections := splitInnodbSections(status)

	// Section name lookup: keys are the original ALL CAPS titles; we also
	// normalize to lowercase for the case-insensitive prefix filter.
	if sectionFilter != "" {
		needle := strings.ToLower(strings.TrimSpace(sectionFilter))
		var matched string
		for name := range sections {
			if strings.HasPrefix(strings.ToLower(name), needle) {
				matched = name
				break
			}
		}
		if matched == "" {
			out["error"] = fmt.Sprintf("section %q not found; available: %s",
				sectionFilter, strings.Join(sortedSectionNames(sections), ", "))
			return out
		}
		out["section"] = matched
		out["body"] = sections[matched]
		return out
	}

	// Derived fields. We search the full status text (not just one section)
	// because some markers can land in either the TRANSACTIONS or LOG section
	// depending on MySQL version, and we'd rather hit the marker once than
	// gate it on a section that may be renamed in MySQL 9.
	hasDeadlock := false
	if body, ok := sections["LATEST DETECTED DEADLOCK"]; ok {
		hasDeadlock = true
		out["latestDeadlockText"] = body
	}
	out["hasDeadlock"] = hasDeadlock

	if m := dbInnodbHistoryList.FindStringSubmatch(status); m != nil {
		out["historyListLength"] = parseInt64(m[1])
	}
	if m := dbInnodbPendingReads.FindStringSubmatch(status); m != nil {
		out["pendingReads"] = parseInt64(m[1])
	}
	if m := dbInnodbPendingWrites.FindStringSubmatch(status); m != nil {
		out["pendingWrites"] = parseInt64(m[1])
	}
	if trxBody, ok := sections["TRANSACTIONS"]; ok {
		out["activeTransactions"] = int64(len(dbInnodbTrxMarker.FindAllStringIndex(trxBody, -1)))
	}

	// Full sections map. Keys are the original ALL CAPS titles so operators
	// can echo them back verbatim into `section:"..."` on a follow-up call.
	out["sections"] = sections
	out["sectionNames"] = sortedSectionNames(sections)

	if includeRaw {
		if len(status) > dbInnodbStatusRawMaxBytes {
			out["statusRaw"] = status[:dbInnodbStatusRawMaxBytes] + "…"
			out["rawTruncated"] = true
		} else {
			out["statusRaw"] = status
		}
		out["statusBytes"] = int64(len(status))
	}
	return out
}

// splitInnodbSections walks the section dividers and returns title→body.
// MySQL emits each header as three lines: separator, title, separator —
// both separators are runs of the SAME character (`=` for the top-level
// monitor-output banner, `-` for every other section), at least 3 chars
// long, with matching content (we don't check that the lengths match
// because MySQL pads dividers to the title width, which varies).
//
// We walk lines manually rather than regex because Go's RE2 doesn't
// support backreferences — there's no \1 to assert "same character on
// both separator lines". A line-state machine is also more obvious to
// read and easier to extend if MySQL 9 ships a new section shape.
//
// Bodies span from the closing separator of one header to the opening
// separator of the next header (or EOF). Duplicate titles win-last —
// InnoDB's BUFFER POOL AND MEMORY can appear twice on a server with
// multiple buffer pool instances (per-pool block followed by a "Total"
// block); operators usually want the "Total" block, so last-write
// semantics are correct.
func splitInnodbSections(status string) map[string]string {
	lines := strings.Split(status, "\n")
	type header struct {
		title    string
		bodyLine int // index of the first body line (after the closing separator)
	}
	var headers []header
	// Pass 1: locate every (sep, title, sep) triplet.
	for i := 0; i+2 < len(lines); i++ {
		if !isInnodbSeparator(lines[i]) || !isInnodbSeparator(lines[i+2]) {
			continue
		}
		// Both separator lines must be runs of the same character. The
		// banner kind (`=` vs `-`) varies by section and we don't care
		// which kind — we just need matching kinds top and bottom so we
		// don't false-match e.g. "----\nSome label\n====" embedded in a
		// deadlock dump or transaction body.
		if lines[i][0] != lines[i+2][0] {
			continue
		}
		title := strings.TrimSpace(lines[i+1])
		if title == "" {
			continue
		}
		// The top-level INNODB MONITOR OUTPUT banner is the only timestamped
		// section header — MySQL emits it as `<date> <time> <thread-id> INNODB
		// MONITOR OUTPUT`. Normalize to the bare section name so operators get
		// a stable key for `section:"INNODB MONITOR OUTPUT"` lookups; the raw
		// timestamp is still available in the section body's first line via
		// the per-second averages text.
		if strings.HasSuffix(title, "INNODB MONITOR OUTPUT") {
			title = "INNODB MONITOR OUTPUT"
		}
		headers = append(headers, header{title: title, bodyLine: i + 3})
		i += 2 // skip past the title + closing separator
	}
	out := map[string]string{}
	for i, h := range headers {
		end := len(lines)
		if i+1 < len(headers) {
			// Next header's opening separator is at bodyLine-3.
			end = headers[i+1].bodyLine - 3
		}
		body := strings.Join(lines[h.bodyLine:end], "\n")
		body = strings.TrimRight(body, "\n")
		out[h.title] = body
	}
	return out
}

// isInnodbSeparator reports whether a line is an InnoDB monitor banner
// separator — a run of ≥3 `=` or `-` characters with no other content.
// Anything shorter is too risky to treat as a section divider; deadlock
// dumps and transaction bodies legitimately contain short dash sequences.
func isInnodbSeparator(line string) bool {
	if len(line) < 3 {
		return false
	}
	c := line[0]
	if c != '=' && c != '-' {
		return false
	}
	for i := 1; i < len(line); i++ {
		if line[i] != c {
			return false
		}
	}
	return true
}

// sortedSectionNames returns the title keys of a section map in the order
// they appear in the InnoDB output. We can't recover original order from
// the map iteration, so we hard-code the canonical sequence MySQL uses and
// append any unknown titles at the end (futureproofs against MySQL 9 adding
// new sections).
func sortedSectionNames(sections map[string]string) []string {
	canonical := []string{
		"INNODB MONITOR OUTPUT",
		"BACKGROUND THREAD",
		"SEMAPHORES",
		"LATEST FOREIGN KEY ERROR",
		"LATEST DETECTED DEADLOCK",
		"TRANSACTIONS",
		"FILE I/O",
		"INSERT BUFFER AND ADAPTIVE HASH INDEX",
		"LOG",
		"BUFFER POOL AND MEMORY",
		"INDIVIDUAL BUFFER POOL INFO",
		"ROW OPERATIONS",
	}
	out := make([]string, 0, len(sections))
	seen := map[string]bool{}
	for _, name := range canonical {
		if _, ok := sections[name]; ok {
			out = append(out, name)
			seen[name] = true
		}
	}
	for name := range sections {
		if !seen[name] {
			out = append(out, name)
		}
	}
	return out
}
