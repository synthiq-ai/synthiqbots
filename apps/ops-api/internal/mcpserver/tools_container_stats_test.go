package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
)

// goldenStats is a representative /containers/.../stats?stream=false response
// from a Docker 24.x daemon. Trimmed to the fields summarizeStats reads —
// adding fields not in dockerStats is fine (json.Unmarshal ignores unknowns)
// but keeping the fixture small documents what's actually load-bearing.
const goldenStats = `{
  "read": "2026-04-30T16:00:00.123456789Z",
  "preread": "2026-04-30T15:59:59.123456789Z",
  "cpu_stats": {
    "cpu_usage": {"total_usage": 200000000},
    "system_cpu_usage": 1000000000,
    "online_cpus": 4
  },
  "precpu_stats": {
    "cpu_usage": {"total_usage": 100000000},
    "system_cpu_usage": 500000000
  },
  "memory_stats": {
    "usage": 2147483648,
    "limit": 8589934592,
    "stats": {"cache": 536870912}
  },
  "networks": {
    "eth0": {"rx_bytes": 1000, "tx_bytes": 2000},
    "eth1": {"rx_bytes": 500,  "tx_bytes": 750}
  },
  "blkio_stats": {
    "io_service_bytes_recursive": [
      {"op": "read",  "value": 1024},
      {"op": "write", "value": 4096},
      {"op": "Read",  "value": 512},
      {"op": "Write", "value": 256},
      {"op": "sync",  "value": 99999}
    ]
  },
  "pids_stats": {"current": 25, "limit": 1024}
}`

func TestSummarizeStats_Golden(t *testing.T) {
	out, err := summarizeStats(json.RawMessage(goldenStats))
	if err != nil {
		t.Fatalf("summarizeStats: %v", err)
	}

	// CPU% = (delta_cpu / delta_sys) * n_cpus * 100
	//      = (100M / 500M) * 4 * 100 = 80.00
	if got, want := out["cpuPercent"], 80.0; got != want {
		t.Errorf("cpuPercent: got %v want %v", got, want)
	}
	if got, want := out["onlineCpus"], 4; got != want {
		t.Errorf("onlineCpus: got %v want %v", got, want)
	}

	// Memory: usage=2GiB, cache=512MiB, rss=1.5GiB, limit=8GiB → 18.75% of limit.
	if got, want := out["memUsageBytes"].(uint64), uint64(2147483648); got != want {
		t.Errorf("memUsageBytes: got %v want %v", got, want)
	}
	if got, want := out["memRssBytes"].(uint64), uint64(1610612736); got != want {
		t.Errorf("memRssBytes: got %v want %v", got, want)
	}
	if got, want := out["memPercent"], 18.75; got != want {
		t.Errorf("memPercent: got %v want %v", got, want)
	}
	if got, want := out["memUsageHuman"].(string), "1.50 GiB"; got != want {
		t.Errorf("memUsageHuman: got %v want %v", got, want)
	}

	// Network totals are summed across both interfaces.
	if got, want := out["netRxBytes"].(uint64), uint64(1500); got != want {
		t.Errorf("netRxBytes: got %v want %v", got, want)
	}
	if got, want := out["netTxBytes"].(uint64), uint64(2750); got != want {
		t.Errorf("netTxBytes: got %v want %v", got, want)
	}

	// BlkIO accumulates lower-case AND title-case op variants — the bug we'd
	// regress is silently dropping the "Read"/"Write" entries on older daemons.
	if got, want := out["blkReadBytes"].(uint64), uint64(1536); got != want {
		t.Errorf("blkReadBytes: got %v want %v", got, want)
	}
	if got, want := out["blkWriteBytes"].(uint64), uint64(4352); got != want {
		t.Errorf("blkWriteBytes: got %v want %v", got, want)
	}

	if got, want := out["pidsCurrent"].(uint64), uint64(25); got != want {
		t.Errorf("pidsCurrent: got %v want %v", got, want)
	}
}

// Cold container with no precpu sample: CPU% must be zero (not NaN / not a
// negative-overflow value). The daemon returns a snapshot with precpu zeroed
// on the very first stream=false call against a freshly-started container.
func TestSummarizeStats_ColdContainerHasZeroCPU(t *testing.T) {
	cold := `{
  "cpu_stats": {
    "cpu_usage": {"total_usage": 100000000},
    "system_cpu_usage": 500000000,
    "online_cpus": 2
  },
  "precpu_stats": {
    "cpu_usage": {"total_usage": 0},
    "system_cpu_usage": 0
  },
  "memory_stats": {"usage": 0, "limit": 1024, "stats": {}},
  "networks": {},
  "blkio_stats": {},
  "pids_stats": {"current": 0, "limit": 0}
}`
	out, err := summarizeStats(json.RawMessage(cold))
	if err != nil {
		t.Fatalf("summarizeStats: %v", err)
	}
	if got := out["cpuPercent"]; got != 0.0 {
		t.Errorf("cpuPercent on cold container: got %v want 0", got)
	}
}

// Memory limit==0 (cgroupv2 unconstrained / no limit set) must not divide by
// zero — the summarizer should keep memPercent=0 and emit raw usage bytes.
// Real-world hit: the wow-mcp-bridge container runs with no memory limit
// configured, so without this guard the tool would panic on the operator's
// first call against it.
func TestSummarizeStats_NoLimitDoesNotPanic(t *testing.T) {
	noLimit := `{
  "cpu_stats": {"cpu_usage": {"total_usage": 1}, "system_cpu_usage": 1, "online_cpus": 1},
  "precpu_stats": {"cpu_usage": {"total_usage": 0}, "system_cpu_usage": 0},
  "memory_stats": {"usage": 1048576, "limit": 0, "stats": {"cache": 0}},
  "networks": {},
  "blkio_stats": {},
  "pids_stats": {}
}`
	out, err := summarizeStats(json.RawMessage(noLimit))
	if err != nil {
		t.Fatalf("summarizeStats: %v", err)
	}
	if got := out["memPercent"]; got != 0.0 {
		t.Errorf("memPercent with limit=0: got %v want 0", got)
	}
	if got, want := out["memUsageBytes"].(uint64), uint64(1048576); got != want {
		t.Errorf("memUsageBytes: got %v want %v", got, want)
	}
}

func TestSummarizeStats_BadJSON(t *testing.T) {
	_, err := summarizeStats(json.RawMessage("not json"))
	if err == nil {
		t.Fatal("expected decode error, got nil")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.00 KiB"},
		{1024 * 1024, "1.00 MiB"},
		{1024 * 1024 * 1024, "1.00 GiB"},
		{1610612736, "1.50 GiB"},
		{16 * 1024 * 1024 * 1024, "16.00 GiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d): got %q want %q", c.in, got, c.want)
		}
	}
}

// summarizeStats truncates cache>usage gracefully (avoids underflow on uint64
// subtraction). A handful of Docker 20.x builds report cache > usage when the
// reading races with a kernel page-cache flush.
func TestSummarizeStats_CacheLargerThanUsage(t *testing.T) {
	weird := `{
  "cpu_stats": {"cpu_usage": {"total_usage": 1}, "system_cpu_usage": 1, "online_cpus": 1},
  "precpu_stats": {"cpu_usage": {"total_usage": 0}, "system_cpu_usage": 0},
  "memory_stats": {"usage": 1000, "limit": 10000, "stats": {"cache": 2000}},
  "networks": {},
  "blkio_stats": {},
  "pids_stats": {}
}`
	out, err := summarizeStats(json.RawMessage(weird))
	if err != nil {
		t.Fatalf("summarizeStats: %v", err)
	}
	if got := out["memRssBytes"].(uint64); got != 0 {
		t.Errorf("memRssBytes when cache>usage: got %v want 0", got)
	}
}

// Tool description is read by Anthropic's SDK as part of the prompt — it must
// mention the OOM signal and ac-worldserver context so the agent picks this
// tool when the operator says "is the worldserver close to OOM?". Sanity-check
// the description in case future edits drop the load-bearing keywords.
func TestContainerStatsTool_DescriptionMentionsOOMSignal(t *testing.T) {
	reg := NewRegistry()
	RegisterContainerStatsTool(reg, ContainerDeps{DefaultContainer: "ac-worldserver"})
	tool, ok := reg.Get("container_stats")
	if !ok {
		t.Fatal("container_stats not registered")
	}
	for _, kw := range []string{"OOM", "ac-worldserver", "memPercent", "137"} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}
