package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
)

// goldenInfo mirrors a Docker 28.x /info response. Trimmed to the fields
// summarizeSystemInfo reads — adding fields not in dockerInfo is fine
// (json.Unmarshal ignores unknowns) but keeping the fixture small documents
// what's load-bearing.
const goldenInfo = `{
  "ServerVersion": "28.0.1",
  "KernelVersion": "6.8.0-90-generic",
  "OperatingSystem": "Ubuntu 24.04.3 LTS",
  "OSType": "linux",
  "Architecture": "x86_64",
  "NCPU": 12,
  "MemTotal": 30795051008,
  "Containers": 42,
  "ContainersRunning": 38,
  "ContainersPaused": 0,
  "ContainersStopped": 4,
  "Images": 44,
  "Driver": "overlay2",
  "CgroupVersion": "2",
  "Warnings": ["[DEPRECATION NOTICE]: API is accessible without encryption"]
}`

const goldenSystemDF = `{
  "LayersSize": 24206429363,
  "Images": [
    {"Size": 5000000000},
    {"Size": 3000000000},
    {"Size": 1500000000}
  ],
  "Containers": [
    {"SizeRw": 100000000},
    {"SizeRw": 50000000}
  ],
  "Volumes": [
    {"Name": "acore-db-data",     "UsageData": {"Size": 8000000000}},
    {"Name": "wow-conf-backups",  "UsageData": {"Size": 2000000000}},
    {"Name": "wow-feedback",      "UsageData": {"Size": 500000000}},
    {"Name": "tiny-vol-1",        "UsageData": {"Size": 1024}},
    {"Name": "tiny-vol-2",        "UsageData": {"Size": 512}},
    {"Name": "tiny-vol-3",        "UsageData": {"Size": 256}},
    {"Name": "tiny-vol-4",        "UsageData": {"Size": 128}}
  ],
  "BuildCache": [
    {"Size": 1000000},
    {"Size": 500000}
  ]
}`

func TestSummarizeSystemInfo_GoldenInfoOnly(t *testing.T) {
	out, err := summarizeSystemInfo(json.RawMessage(goldenInfo), nil)
	if err != nil {
		t.Fatalf("summarizeSystemInfo: %v", err)
	}
	if got, want := out["serverVersion"].(string), "28.0.1"; got != want {
		t.Errorf("serverVersion: got %v want %v", got, want)
	}
	if got, want := out["nCPU"].(int), 12; got != want {
		t.Errorf("nCPU: got %v want %v", got, want)
	}
	if got, want := out["memTotalBytes"].(uint64), uint64(30795051008); got != want {
		t.Errorf("memTotalBytes: got %v want %v", got, want)
	}
	// 30795051008 → "28.68 GiB"
	if got, want := out["memTotalHuman"].(string), "28.68 GiB"; got != want {
		t.Errorf("memTotalHuman: got %v want %v", got, want)
	}
	if got, want := out["containers"].(int), 42; got != want {
		t.Errorf("containers: got %v want %v", got, want)
	}
	if got, want := out["containersRunning"].(int), 38; got != want {
		t.Errorf("containersRunning: got %v want %v", got, want)
	}
	if got, want := out["containersStopped"].(int), 4; got != want {
		t.Errorf("containersStopped: got %v want %v", got, want)
	}
	if got, want := out["storageDriver"].(string), "overlay2"; got != want {
		t.Errorf("storageDriver: got %v want %v", got, want)
	}
	if got, want := out["cgroupVersion"].(string), "2"; got != want {
		t.Errorf("cgroupVersion: got %v want %v", got, want)
	}
	warnings, ok := out["warnings"].([]string)
	if !ok || len(warnings) != 1 {
		t.Fatalf("warnings: got %v want one entry", out["warnings"])
	}
	if !strings.Contains(warnings[0], "DEPRECATION") {
		t.Errorf("warnings[0]: missing DEPRECATION marker: %q", warnings[0])
	}
	// Disk block must be absent when df=nil — saves operators wading through
	// stale zeros if they explicitly opted out.
	if _, ok := out["disk"]; ok {
		t.Error("disk: present when df=nil; expected omitted")
	}
}

func TestSummarizeSystemInfo_GoldenWithDisk(t *testing.T) {
	out, err := summarizeSystemInfo(json.RawMessage(goldenInfo), json.RawMessage(goldenSystemDF))
	if err != nil {
		t.Fatalf("summarizeSystemInfo: %v", err)
	}
	disk, ok := out["disk"].(map[string]any)
	if !ok {
		t.Fatalf("disk block missing or wrong type: %T", out["disk"])
	}
	if got, want := disk["layersSizeBytes"].(uint64), uint64(24206429363); got != want {
		t.Errorf("layersSizeBytes: got %v want %v", got, want)
	}
	if got, want := disk["imagesSizeBytes"].(uint64), uint64(9500000000); got != want {
		t.Errorf("imagesSizeBytes: got %v want %v", got, want)
	}
	if got, want := disk["containersSizeBytes"].(uint64), uint64(150000000); got != want {
		t.Errorf("containersSizeBytes: got %v want %v", got, want)
	}
	if got, want := disk["volumesSizeBytes"].(uint64), uint64(10500001920); got != want {
		t.Errorf("volumesSizeBytes: got %v want %v", got, want)
	}
	if got, want := disk["buildCacheSizeBytes"].(uint64), uint64(1500000); got != want {
		t.Errorf("buildCacheSizeBytes: got %v want %v", got, want)
	}
	if got, want := disk["volumeCount"].(int), 7; got != want {
		t.Errorf("volumeCount: got %v want %v", got, want)
	}
	if got, want := disk["buildCacheCount"].(int), 2; got != want {
		t.Errorf("buildCacheCount: got %v want %v", got, want)
	}
	// Top volumes must be sorted descending and capped at 5. Wire format is
	// []map[string]any so callers don't have to import a typed struct.
	rawVols, ok := disk["topVolumes"].([]map[string]any)
	if !ok {
		t.Fatalf("topVolumes wrong type: %T", disk["topVolumes"])
	}
	if len(rawVols) != 5 {
		t.Fatalf("topVolumes len: got %d want 5", len(rawVols))
	}
	if rawVols[0]["name"].(string) != "acore-db-data" || rawVols[0]["sizeBytes"].(uint64) != 8000000000 {
		t.Errorf("topVolumes[0]: got %+v want acore-db-data 8e9", rawVols[0])
	}
	if rawVols[1]["name"].(string) != "wow-conf-backups" || rawVols[1]["sizeBytes"].(uint64) != 2000000000 {
		t.Errorf("topVolumes[1]: got %+v want wow-conf-backups 2e9", rawVols[1])
	}
	if rawVols[2]["name"].(string) != "wow-feedback" {
		t.Errorf("topVolumes[2]: got %+v want wow-feedback", rawVols[2])
	}
	// Tiny volumes 1-3 fill slots 3-4; tiny-vol-4 is dropped (cap at 5).
	for _, v := range rawVols {
		if v["name"].(string) == "tiny-vol-4" {
			t.Errorf("tiny-vol-4 should have been dropped past the 5-volume cap")
		}
	}
}

// /info-only mode (df=nil) is the fast path — operators set disk:false when
// /system/df is too slow on a stressed daemon. Verify summarizeSystemInfo
// emits the host snapshot without any disk block AND without erroring on the
// nil df argument.
func TestSummarizeSystemInfo_NilDFNoError(t *testing.T) {
	out, err := summarizeSystemInfo(json.RawMessage(goldenInfo), nil)
	if err != nil {
		t.Fatalf("summarizeSystemInfo with nil df: %v", err)
	}
	if _, ok := out["disk"]; ok {
		t.Error("disk should be absent when df=nil")
	}
	if _, ok := out["diskError"]; ok {
		t.Error("diskError should be absent when df=nil (operator opted out, not an error)")
	}
}

// Bad /info JSON must surface as a decode error. The handler maps this to the
// per-tool-call error envelope, but the summarizer's contract is "fail loudly
// on bad info" — info is the load-bearing half.
func TestSummarizeSystemInfo_BadInfoErrors(t *testing.T) {
	_, err := summarizeSystemInfo(json.RawMessage("not json"), nil)
	if err == nil {
		t.Fatal("expected decode error on bad info, got nil")
	}
}

// Bad /system/df JSON must NOT fail the whole call — info data still returns,
// with a diskError marker. Mirrors the handler's per-half tolerance.
func TestSummarizeSystemInfo_BadDFDoesNotKillInfo(t *testing.T) {
	out, err := summarizeSystemInfo(json.RawMessage(goldenInfo), json.RawMessage("not json"))
	if err != nil {
		t.Fatalf("summarizeSystemInfo with bad df: %v", err)
	}
	if got, want := out["serverVersion"].(string), "28.0.1"; got != want {
		t.Errorf("info data lost when df errored: got serverVersion=%v", got)
	}
	if _, ok := out["diskError"]; !ok {
		t.Error("diskError marker missing")
	}
	if _, ok := out["disk"]; ok {
		t.Error("disk block present despite df decode error")
	}
}

// Empty Warnings in /info should still surface as []string{} (not nil) so the
// MCP wire format is consistent. Operators hate `if warnings != nil` checks
// in their downstream tooling.
func TestSummarizeSystemInfo_EmptyWarningsAreEmptyArray(t *testing.T) {
	noWarnings := `{
  "ServerVersion": "28.0.1",
  "NCPU": 1,
  "MemTotal": 1024
}`
	out, err := summarizeSystemInfo(json.RawMessage(noWarnings), nil)
	if err != nil {
		t.Fatalf("summarizeSystemInfo: %v", err)
	}
	w, ok := out["warnings"].([]string)
	if !ok {
		t.Fatalf("warnings wrong type: %T", out["warnings"])
	}
	if len(w) != 0 {
		t.Errorf("warnings: got %v want empty", w)
	}
}

// Tool description is read by Anthropic's SDK as part of the prompt — it
// must mention the gap this tool plugs (per-container tools fail when the
// container is gone) and the disk:false escape hatch. Without these
// keywords the agent won't reach for this tool in the situations it was
// designed for.
func TestContainerSystemInfoTool_DescriptionMentionsKeyContext(t *testing.T) {
	reg := NewRegistry()
	RegisterContainerSystemInfoTool(reg, ContainerDeps{})
	tool, ok := reg.Get("container_system_info")
	if !ok {
		t.Fatal("container_system_info not registered")
	}
	for _, kw := range []string{"OOM-killed", "ac-worldserver", "container_inspect", "container_stats", "disk:false"} {
		if !strings.Contains(tool.Description, kw) {
			t.Errorf("description missing %q: %s", kw, tool.Description)
		}
	}
}
