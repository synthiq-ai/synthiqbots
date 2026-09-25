package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
)

// RegisterContainerStatsTool registers `container_stats` — a point-in-time
// resource snapshot for a running container. Closes the gap between
// container_inspect (config-only) and ops_logs_tail (stream-only): operators
// previously had no way to ask "is anything in distress *right now*" without
// shelling into the host. ac-worldserver in particular gets OOM-killed (exit
// 137) periodically, and surfacing memory pressure before the kill lets the
// operator restart the container preemptively.
//
// Backed by the Docker stats endpoint with stream=false — the daemon waits ~1 s
// internally to deliver a CPU delta, so this tool is slower than the other
// container_* tools (typically ~1.1 s wall-clock).
func RegisterContainerStatsTool(reg *Registry, deps ContainerDeps) {
	if deps.DefaultContainer == "" {
		deps.DefaultContainer = "ac-worldserver"
	}

	reg.Register(Tool{
		Name: "container_stats",
		Description: "Point-in-time CPU/memory/network/disk-IO snapshot for a container. " +
			"Defaults to ac-worldserver. Returns derived `cpuPercent`, `memPercent`, " +
			"`memUsageBytes`, `memLimitBytes`, `memRssBytes` (usage minus page cache), " +
			"`netRxBytes`/`netTxBytes` (summed across interfaces), and " +
			"`blkReadBytes`/`blkWriteBytes`. Use this before restarting a container to " +
			"check whether memory pressure is the actual cause (ac-worldserver " +
			"exit-code 137 = OOM kill — `memPercent` >90% pre-crash is the smoking gun). " +
			"Latency is ~1 s — Docker waits internally for a CPU delta sample.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"name":{"type":"string","description":"Container name or id (default ac-worldserver)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			var a struct{ Name string }
			_ = json.Unmarshal(raw, &a)
			name := pickContainer(a.Name, deps.DefaultContainer)
			body, err := deps.Docker.StatsRaw(ctx, name)
			if err != nil {
				return map[string]any{"error": err.Error(), "name": name}
			}
			out, err := summarizeStats(body)
			if err != nil {
				return map[string]any{"error": "decode: " + err.Error(), "name": name}
			}
			out["name"] = name
			return out
		},
	})
}

// dockerStats is the subset of /containers/{id}/stats we surface. Field names
// match the upstream JSON keys exactly so json.Unmarshal does the work.
type dockerStats struct {
	Read    string `json:"read"`
	Preread string `json:"preread"`
	CPU     struct {
		Usage struct {
			Total uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		System     uint64 `json:"system_cpu_usage"`
		OnlineCPUs int    `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPU struct {
		Usage struct {
			Total uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
		System uint64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	Memory struct {
		Usage uint64 `json:"usage"`
		Limit uint64 `json:"limit"`
		Stats struct {
			Cache uint64 `json:"cache"`
		} `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
	BlkIO struct {
		Recursive []struct {
			Op    string `json:"op"`
			Value uint64 `json:"value"`
		} `json:"io_service_bytes_recursive"`
	} `json:"blkio_stats"`
	PIDs struct {
		Current uint64 `json:"current"`
		Limit   uint64 `json:"limit"`
	} `json:"pids_stats"`
}

// summarizeStats parses Docker's stats document and emits the derived fields
// operators ask for ("how much CPU/RAM is this thing using right now"). Split
// out from the handler so unit tests can drive it with golden inputs without
// standing up a fake docker socket.
func summarizeStats(body json.RawMessage) (map[string]any, error) {
	var s dockerStats
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, err
	}

	cpuPct := computeCPUPercent(s)
	memUsage := s.Memory.Usage
	memLimit := s.Memory.Limit
	memCache := s.Memory.Stats.Cache
	memRSS := uint64(0)
	if memUsage > memCache {
		memRSS = memUsage - memCache
	}
	memPct := 0.0
	if memLimit > 0 {
		memPct = float64(memRSS) / float64(memLimit) * 100.0
	}

	var netRx, netTx uint64
	for _, n := range s.Networks {
		netRx += n.RxBytes
		netTx += n.TxBytes
	}

	var blkR, blkW uint64
	// Op names are case-sensitive in the API (older versions emit "Read"/"Write",
	// newer "read"/"write"). Match both — guessing wrong silently zeroes out the
	// disk-IO column in the response, which is the kind of bug that survives
	// review because nobody reads disk numbers until they're missing.
	for _, e := range s.BlkIO.Recursive {
		switch e.Op {
		case "read", "Read":
			blkR += e.Value
		case "write", "Write":
			blkW += e.Value
		}
	}

	out := map[string]any{
		"read":           s.Read,
		"cpuPercent":     roundTo(cpuPct, 2),
		"onlineCpus":     s.CPU.OnlineCPUs,
		"memUsageBytes":  memUsage,
		"memRssBytes":    memRSS,
		"memCacheBytes":  memCache,
		"memLimitBytes":  memLimit,
		"memPercent":     roundTo(memPct, 2),
		"memUsageHuman":  humanBytes(memRSS),
		"memLimitHuman":  humanBytes(memLimit),
		"netRxBytes":     netRx,
		"netTxBytes":     netTx,
		"blkReadBytes":   blkR,
		"blkWriteBytes":  blkW,
		"pidsCurrent":    s.PIDs.Current,
		"pidsLimit":      s.PIDs.Limit,
	}
	return out, nil
}

// computeCPUPercent reproduces `docker stats`'s formula. Returns 0 when the
// precpu sample is unusable: cold container (precpu zero — first-ever reading,
// or one-shot=true mode) and any case where the delta would underflow.
//
// Treating zero-precpu as "cold" rather than "valid prior" matters: without
// the guard, the formula divides cpu_stats by system_cpu_stats, producing the
// container's *cumulative* CPU share since boot (typically a small but nonzero
// number for short-lived containers, ~40% in test). That's neither current
// utilization nor a meaningful average — the operator would read it as "the
// container is using 40% CPU right now" and act on bogus data.
func computeCPUPercent(s dockerStats) float64 {
	if s.PreCPU.System == 0 {
		return 0
	}
	cpuDelta := float64(s.CPU.Usage.Total) - float64(s.PreCPU.Usage.Total)
	sysDelta := float64(s.CPU.System) - float64(s.PreCPU.System)
	if cpuDelta <= 0 || sysDelta <= 0 {
		return 0
	}
	n := s.CPU.OnlineCPUs
	if n <= 0 {
		n = 1
	}
	return (cpuDelta / sysDelta) * float64(n) * 100.0
}

// humanBytes formats with binary-prefix units (MiB/GiB) the way `docker stats`
// does. Two decimals, no trailing zero stripping — predictable width.
func humanBytes(b uint64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
		GiB = 1024 * MiB
	)
	switch {
	case b >= GiB:
		return fmt.Sprintf("%.2f GiB", float64(b)/float64(GiB))
	case b >= MiB:
		return fmt.Sprintf("%.2f MiB", float64(b)/float64(MiB))
	case b >= KiB:
		return fmt.Sprintf("%.2f KiB", float64(b)/float64(KiB))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func roundTo(f float64, digits int) float64 {
	mul := 1.0
	for i := 0; i < digits; i++ {
		mul *= 10
	}
	return float64(int64(f*mul+0.5)) / mul
}
