package mcpserver

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// fixtureInnodbStatus is a trimmed-but-realistic SHOW ENGINE INNODB STATUS
// body. Preserves the three-line dash/equals header pattern, all canonical
// section headings, and the per-section markers the parser keys on
// (History list length, ---TRANSACTION, Pending normal aio reads/writes).
// Lifted from a live MySQL 8.0.x worldserver run, identifiers scrubbed.
const fixtureInnodbStatus = `
=====================================
2026-05-20 10:00:00 0x7f0000000700 INNODB MONITOR OUTPUT
=====================================
Per second averages calculated from the last 14 seconds
-----------------
BACKGROUND THREAD
-----------------
srv_master_thread loops: 1234 srv_active, 0 srv_shutdown, 5678 srv_idle
----------
SEMAPHORES
----------
OS WAIT ARRAY INFO: reservation count 42
Mutex spin waits 100, rounds 200, OS waits 5
------------------------
LATEST DETECTED DEADLOCK
------------------------
2026-05-20 09:55:00 0x7f0000001700
*** (1) TRANSACTION:
TRANSACTION 12345, ACTIVE 5 sec starting index read
mysql tables in use 1, locked 1
LOCK WAIT 2 lock struct(s), heap size 1136, 1 row lock(s)
*** (1) HOLDS THE LOCK(S):
RECORD LOCKS space id 5 page no 4 n bits 80 of table acore_characters.characters
*** WE ROLL BACK TRANSACTION (1)
------------
TRANSACTIONS
------------
Trx id counter 99999
Purge done for trx's n:o < 99990 undo n:o < 0 state: running but idle
History list length 17
LIST OF TRANSACTIONS FOR EACH SESSION:
---TRANSACTION 421000000000000, not started
---TRANSACTION 99998, ACTIVE 2 sec
mysql tables in use 1, locked 0
2 lock struct(s), heap size 1136
---TRANSACTION 99997, ACTIVE 1 sec
mysql tables in use 1, locked 0
--------
FILE I/O
--------
I/O thread 0 state: waiting for completed aio requests (insert buffer thread)
Pending normal aio reads: [0, 0, 0, 0] , aio writes: [0, 0, 0, 0] ,
Pending normal aio writes: [0, 0, 0, 0] , log i/o's: 0
ibuf aio reads:, log i/o's:, sync i/o's:
Pending flushes (fsync) log: 0; buffer pool: 0
-------------------------------------
INSERT BUFFER AND ADAPTIVE HASH INDEX
-------------------------------------
Ibuf: size 1, free list len 0, seg size 2, 0 merges
---
LOG
---
Log sequence number          50000000
Log buffer assigned up to    50000000
Log buffer completed up to   50000000
----------------------
BUFFER POOL AND MEMORY
----------------------
Total large memory allocated 137363456
Dictionary memory allocated 200000
Buffer pool size   8192
Free buffers       4096
Database pages     4000
--------------
ROW OPERATIONS
--------------
0 queries inside InnoDB, 0 queries in queue
0 read views open inside InnoDB
Process ID=1234, Main thread ID=139000000, state: sleeping
`

// TestDBInnodbStatus_DescriptionMentionsContext keeps the description
// anchored to the keywords the agent uses to pick this tool. Same shape as
// the db_processlist / db_global_status description guards.
func TestDBInnodbStatus_DescriptionMentionsContext(t *testing.T) {
	reg := NewRegistry()
	RegisterDBInnodbStatusTool(reg, DBDeps{})
	tool, ok := reg.Get("db_innodb_status")
	if !ok {
		t.Fatal("db_innodb_status not registered")
	}
	for _, kw := range []string{"INNODB STATUS", "hasDeadlock", "historyListLength", "PROCESS privilege", "deadlock"} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q", kw)
		}
	}
}

func TestDBInnodbStatus_NoPoolReturnsError(t *testing.T) {
	reg := NewRegistry()
	RegisterDBInnodbStatusTool(reg, DBDeps{}) // QueryDB nil
	tool, _ := reg.Get("db_innodb_status")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, _ := got["error"].(string); !strings.Contains(msg, "ops_ro pool not configured") {
		t.Errorf("expected pool-not-configured error, got %v", got)
	}
}

// TestDBInnodbStatus_ParsesFixture verifies the parser locates every
// canonical section, surfaces the derived fields, and reports the deadlock.
// This is the integration test for parseInnodbStatus — driven off the real
// fixture so a future regex tweak that breaks the dash-pattern detection
// trips this test before it ships.
func TestDBInnodbStatus_ParsesFixture(t *testing.T) {
	out := parseInnodbStatus(fixtureInnodbStatus, "", true)

	if got, _ := out["hasDeadlock"].(bool); !got {
		t.Errorf("hasDeadlock should be true")
	}
	if _, ok := out["latestDeadlockText"].(string); !ok {
		t.Errorf("latestDeadlockText missing")
	}
	if got, _ := out["historyListLength"].(int64); got != 17 {
		t.Errorf("historyListLength: %v", got)
	}
	if got, _ := out["pendingReads"].(int64); got != 0 {
		t.Errorf("pendingReads: %v", got)
	}
	if got, _ := out["pendingWrites"].(int64); got != 0 {
		t.Errorf("pendingWrites: %v", got)
	}
	// Three ---TRANSACTION markers in the fixture.
	if got, _ := out["activeTransactions"].(int64); got != 3 {
		t.Errorf("activeTransactions: %v", got)
	}

	sections, _ := out["sections"].(map[string]string)
	for _, want := range []string{
		"INNODB MONITOR OUTPUT",
		"BACKGROUND THREAD",
		"SEMAPHORES",
		"LATEST DETECTED DEADLOCK",
		"TRANSACTIONS",
		"FILE I/O",
		"INSERT BUFFER AND ADAPTIVE HASH INDEX",
		"LOG",
		"BUFFER POOL AND MEMORY",
		"ROW OPERATIONS",
	} {
		if _, ok := sections[want]; !ok {
			t.Errorf("section %q missing", want)
		}
	}

	// sectionNames preserves canonical order — INNODB MONITOR OUTPUT first,
	// ROW OPERATIONS last for the canonical sections present in the fixture.
	names, _ := out["sectionNames"].([]string)
	if len(names) < 2 || names[0] != "INNODB MONITOR OUTPUT" {
		t.Errorf("sectionNames[0]: %v", names)
	}
	if names[len(names)-1] != "ROW OPERATIONS" {
		t.Errorf("sectionNames[last]: %v", names[len(names)-1])
	}

	// Raw included by default and not truncated (fixture is ~2 KB).
	if _, ok := out["statusRaw"].(string); !ok {
		t.Errorf("statusRaw missing")
	}
	if _, ok := out["rawTruncated"]; ok {
		t.Errorf("rawTruncated should be absent for small fixture")
	}
}

// TestDBInnodbStatus_NoDeadlock verifies hasDeadlock:false when the
// LATEST DETECTED DEADLOCK section is absent. Healthy servers omit it.
func TestDBInnodbStatus_NoDeadlock(t *testing.T) {
	body := strings.Replace(fixtureInnodbStatus,
		"------------------------\nLATEST DETECTED DEADLOCK\n------------------------\n",
		"", 1)
	// Strip the deadlock body too — leave the SEMAPHORES → TRANSACTIONS
	// neighbours untouched. Easier to slice on the next header.
	idx := strings.Index(body, "------------\nTRANSACTIONS\n------------")
	semaphoresEnd := strings.Index(body, "OS waits 5\n") + len("OS waits 5\n")
	body = body[:semaphoresEnd] + body[idx:]

	out := parseInnodbStatus(body, "", false)
	if got, _ := out["hasDeadlock"].(bool); got {
		t.Errorf("hasDeadlock should be false when section absent")
	}
	if _, ok := out["latestDeadlockText"]; ok {
		t.Errorf("latestDeadlockText should be absent when no deadlock")
	}
	// includeRaw=false drops the raw blob.
	if _, ok := out["statusRaw"]; ok {
		t.Errorf("statusRaw should be absent when includeRaw=false")
	}
}

// TestDBInnodbStatus_SectionFilter — `section:"buffer pool"` should
// case-insensitive prefix-match BUFFER POOL AND MEMORY and return only its
// body. The full sections blob and derived counters are skipped to keep the
// response surface predictable for narrow follow-up queries.
func TestDBInnodbStatus_SectionFilter(t *testing.T) {
	out := parseInnodbStatus(fixtureInnodbStatus, "buffer pool", true)
	if got, _ := out["section"].(string); got != "BUFFER POOL AND MEMORY" {
		t.Errorf("section: %v", got)
	}
	body, _ := out["body"].(string)
	if !strings.Contains(body, "Buffer pool size") {
		t.Errorf("body missing expected content: %q", body)
	}
	// Derived fields and full sections map are suppressed in single-section mode.
	if _, ok := out["sections"]; ok {
		t.Errorf("sections map should be suppressed when section filter set")
	}
	if _, ok := out["hasDeadlock"]; ok {
		t.Errorf("hasDeadlock should be suppressed when section filter set")
	}
}

// TestDBInnodbStatus_SectionFilterMiss returns an error listing the
// available section names. Mirrors the "available" hint pattern used
// elsewhere (db_table_info, ops_files_read).
func TestDBInnodbStatus_SectionFilterMiss(t *testing.T) {
	out := parseInnodbStatus(fixtureInnodbStatus, "nonexistent-cluster", true)
	msg, _ := out["error"].(string)
	if !strings.Contains(msg, "section \"nonexistent-cluster\" not found") {
		t.Errorf("error: %v", msg)
	}
	if !strings.Contains(msg, "TRANSACTIONS") {
		t.Errorf("error should list canonical sections: %v", msg)
	}
}

// TestDBInnodbStatus_RawTruncation — feed parser a fake body padded past
// the 48 KiB cap and verify the raw response is elided with the truncated
// flag set. Uses a synthetic body (real fixture is only ~2 KB).
func TestDBInnodbStatus_RawTruncation(t *testing.T) {
	pad := strings.Repeat("padding line\n", 8000) // ~104 KB
	body := fixtureInnodbStatus + pad

	out := parseInnodbStatus(body, "", true)
	if got, _ := out["rawTruncated"].(bool); !got {
		t.Errorf("rawTruncated not set")
	}
	if raw, _ := out["statusRaw"].(string); len(raw) != dbInnodbStatusRawMaxBytes+len("…") {
		t.Errorf("statusRaw wrong length: %d", len(raw))
	}
	if got, _ := out["statusBytes"].(int64); got != int64(len(body)) {
		t.Errorf("statusBytes: %v, want %d", got, len(body))
	}
}

// TestDBInnodbStatus_HandlerWiring — full handler path with sqlmock,
// verifying the SHOW ENGINE INNODB STATUS row is scanned and the parsed
// fields land in the response.
func TestDBInnodbStatus_HandlerWiring(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SHOW ENGINE INNODB STATUS")).
		WillReturnRows(sqlmock.NewRows([]string{"Type", "Name", "Status"}).
			AddRow("InnoDB", "", fixtureInnodbStatus))

	reg := NewRegistry()
	RegisterDBInnodbStatusTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_innodb_status")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	if msg, ok := got["error"]; ok {
		t.Fatalf("unexpected error: %v", msg)
	}
	if hd, _ := got["hasDeadlock"].(bool); !hd {
		t.Errorf("hasDeadlock not surfaced through handler")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet mock expectations: %v", err)
	}
}

// TestDBInnodbStatus_PrivilegeDeniedSurfaced — when ops_ro lacks the PROCESS
// privilege, MySQL returns a scan error from the driver. Surface it verbatim
// so the operator gets the actionable hint mentioned in the tool description.
func TestDBInnodbStatus_PrivilegeDeniedSurfaced(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SHOW ENGINE INNODB STATUS")).
		WillReturnError(strErr("Error 1227: Access denied; you need (at least one of) the PROCESS privilege(s) for this operation"))

	reg := NewRegistry()
	RegisterDBInnodbStatusTool(reg, DBDeps{QueryDB: db})
	tool, _ := reg.Get("db_innodb_status")
	resp := tool.Handler(context.Background(), json.RawMessage(`{}`), "")
	got, _ := resp.(map[string]any)
	msg, _ := got["error"].(string)
	if !strings.Contains(msg, "PROCESS privilege") {
		t.Errorf("expected PROCESS privilege error, got: %v", msg)
	}
}

// strErr is a tiny error type to feed sqlmock.WillReturnError with a
// message string — avoids pulling in fmt.Errorf at the call sites.
type strErr string

func (e strErr) Error() string { return string(e) }
