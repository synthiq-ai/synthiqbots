package mcpserver

import (
	"context"
	"encoding/json"
	"sort"
)

// RegisterContainerSystemInfoTool registers `container_system_info` — a
// Docker-daemon-level snapshot covering host capacity, container population,
// and (optional) disk usage breakdown. Closes the gap between the per-
// container tools (which only work when a container exists) and a host-wide
// "is the daemon healthy?" view. After ac-worldserver is OOM-killed and
// auto-removed, container_inspect/container_stats both return errors — this
// tool still returns useful info because it queries the daemon directly.
//
// Composition:
//   - /info → host CPU/memory totals, container counts, OS/kernel, server
//     warnings (security deprecations, cgroup mode, etc.).
//   - /system/df → image/container/volume/build-cache disk usage. Slower
//     (the daemon walks every layer); gated behind disk:true.
func RegisterContainerSystemInfoTool(reg *Registry, deps ContainerDeps) {
	reg.Register(Tool{
		Name: "container_system_info",
		Description: "Docker daemon snapshot: host CPU/memory totals, container counts " +
			"(running/paused/stopped/total), image count, OS/kernel/storage-driver, " +
			"and server warnings. With disk:true (default) also returns a disk-usage " +
			"breakdown — total layer bytes, per-class size (images/containers/volumes/" +
			"build-cache), and the top 5 volumes by size. Use this when per-container " +
			"tools (container_inspect, container_stats) fail because the container is " +
			"gone — the daemon answers even when ac-worldserver was OOM-killed and " +
			"auto-removed (exit 137 with --rm). Set disk:false for a sub-100 ms " +
			"snapshot when /system/df's multi-second walk is too slow.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{
			"disk":{"type":"boolean","description":"Include /system/df disk-usage breakdown (default true; set false to skip the slower call)"}
		}}`),
		Annotations: AnnRead(),
		Handler: func(ctx context.Context, raw json.RawMessage, _ string) any {
			// Default disk:true — operators almost always want disk; the gate
			// exists for the edge case where the call is too slow under a
			// stressed daemon. Decode arg with a sentinel so omitted == true.
			var a struct {
				Disk *bool `json:"disk"`
			}
			_ = json.Unmarshal(raw, &a)
			includeDisk := true
			if a.Disk != nil {
				includeDisk = *a.Disk
			}

			info, err := deps.Docker.InfoRaw(ctx)
			if err != nil {
				return map[string]any{"error": "info: " + err.Error()}
			}
			var df json.RawMessage
			if includeDisk {
				df, err = deps.Docker.SystemDFRaw(ctx)
				if err != nil {
					// Don't fail the whole call — return /info data plus the
					// /system/df error so the operator still gets the host
					// snapshot. Disk is the optional half.
					out, perr := summarizeSystemInfo(info, nil)
					if perr != nil {
						return map[string]any{"error": "decode info: " + perr.Error()}
					}
					out["diskError"] = err.Error()
					return out
				}
			}
			out, err := summarizeSystemInfo(info, df)
			if err != nil {
				return map[string]any{"error": "decode: " + err.Error()}
			}
			return out
		},
	})
}

// dockerInfo is the subset of /info we surface. Field names match the
// upstream JSON keys exactly so json.Unmarshal does the work.
type dockerInfo struct {
	ServerVersion     string   `json:"ServerVersion"`
	KernelVersion     string   `json:"KernelVersion"`
	OperatingSystem   string   `json:"OperatingSystem"`
	OSType            string   `json:"OSType"`
	Architecture      string   `json:"Architecture"`
	NCPU              int      `json:"NCPU"`
	MemTotal          uint64   `json:"MemTotal"`
	Containers        int      `json:"Containers"`
	ContainersRunning int      `json:"ContainersRunning"`
	ContainersPaused  int      `json:"ContainersPaused"`
	ContainersStopped int      `json:"ContainersStopped"`
	Images            int      `json:"Images"`
	Driver            string   `json:"Driver"`
	CgroupVersion     string   `json:"CgroupVersion"`
	Warnings          []string `json:"Warnings"`
}

// dockerSystemDF is the subset of /system/df we surface. We keep volumes typed
// because we want to surface the heaviest by size; everything else is summed.
type dockerSystemDF struct {
	LayersSize uint64 `json:"LayersSize"`
	Images     []struct {
		Size uint64 `json:"Size"`
	} `json:"Images"`
	Containers []struct {
		SizeRw uint64 `json:"SizeRw"`
	} `json:"Containers"`
	Volumes []struct {
		Name      string `json:"Name"`
		UsageData struct {
			Size uint64 `json:"Size"`
		} `json:"UsageData"`
	} `json:"Volumes"`
	BuildCache []struct {
		Size uint64 `json:"Size"`
	} `json:"BuildCache"`
}

// summarizeSystemInfo merges /info + /system/df into a single response.
// Split out from the handler so unit tests can drive it with golden inputs
// without needing a fake docker socket. Pass df=nil to skip the disk block.
func summarizeSystemInfo(infoRaw, dfRaw json.RawMessage) (map[string]any, error) {
	var info dockerInfo
	if err := json.Unmarshal(infoRaw, &info); err != nil {
		return nil, err
	}
	out := map[string]any{
		"serverVersion":     info.ServerVersion,
		"kernelVersion":     info.KernelVersion,
		"operatingSystem":   info.OperatingSystem,
		"osType":            info.OSType,
		"architecture":      info.Architecture,
		"nCPU":              info.NCPU,
		"memTotalBytes":     info.MemTotal,
		"memTotalHuman":     humanBytes(info.MemTotal),
		"containers":        info.Containers,
		"containersRunning": info.ContainersRunning,
		"containersPaused":  info.ContainersPaused,
		"containersStopped": info.ContainersStopped,
		"images":            info.Images,
		"storageDriver":     info.Driver,
		"cgroupVersion":     info.CgroupVersion,
	}
	// Always emit warnings as an array (even when empty) so callers don't
	// have to nil-check. The MCP wire format prefers explicit empty arrays
	// over missing keys.
	if info.Warnings == nil {
		out["warnings"] = []string{}
	} else {
		out["warnings"] = info.Warnings
	}

	if dfRaw == nil {
		return out, nil
	}

	var df dockerSystemDF
	if err := json.Unmarshal(dfRaw, &df); err != nil {
		// Surface the disk-decode error but still return /info data — same
		// principle as the handler's per-half error fallback.
		out["diskError"] = "decode system_df: " + err.Error()
		return out, nil
	}

	var imagesSize, containersSize, volumesSize, buildCacheSize uint64
	for _, im := range df.Images {
		imagesSize += im.Size
	}
	for _, c := range df.Containers {
		containersSize += c.SizeRw
	}
	for _, v := range df.Volumes {
		volumesSize += v.UsageData.Size
	}
	for _, b := range df.BuildCache {
		buildCacheSize += b.Size
	}

	// Top 5 volumes by size — the heaviest are usually database volumes
	// (acore-db-data is gigabytes), surfacing them tells the operator where
	// the disk pressure actually lives. Returned as []map[string]any to match
	// the rest of this package's wire format (every other tool emits maps),
	// so callers don't see a typed Go struct in the JSON-RPC response.
	vols := make([]map[string]any, 0, len(df.Volumes))
	for _, v := range df.Volumes {
		if v.UsageData.Size == 0 && v.Name == "" {
			continue
		}
		vols = append(vols, map[string]any{
			"name":      v.Name,
			"sizeBytes": v.UsageData.Size,
			"sizeHuman": humanBytes(v.UsageData.Size),
		})
	}
	sort.Slice(vols, func(i, j int) bool {
		return vols[i]["sizeBytes"].(uint64) > vols[j]["sizeBytes"].(uint64)
	})
	if len(vols) > 5 {
		vols = vols[:5]
	}

	disk := map[string]any{
		"layersSizeBytes":     df.LayersSize,
		"layersSizeHuman":     humanBytes(df.LayersSize),
		"imagesSizeBytes":     imagesSize,
		"imagesSizeHuman":     humanBytes(imagesSize),
		"containersSizeBytes": containersSize,
		"containersSizeHuman": humanBytes(containersSize),
		"volumesSizeBytes":    volumesSize,
		"volumesSizeHuman":    humanBytes(volumesSize),
		"buildCacheSizeBytes": buildCacheSize,
		"buildCacheSizeHuman": humanBytes(buildCacheSize),
		"imageCount":          len(df.Images),
		"volumeCount":         len(df.Volumes),
		"buildCacheCount":     len(df.BuildCache),
		"topVolumes":          vols,
	}
	out["disk"] = disk
	return out, nil
}
