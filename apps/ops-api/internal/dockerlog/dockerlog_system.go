package dockerlog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// InfoRaw fetches /info, the daemon-level snapshot — host CPU/memory totals,
// container counts (running/stopped/paused), image count, OS/kernel/storage
// driver, server warnings. Returns the raw JSON; the caller summarizes the
// fields it cares about (mirrors the InspectRaw pattern). Used by
// container_system_info, which closes the gap when per-container tools are
// useless because the container is gone (e.g. ac-worldserver missing from
// docker ps -a after a rough OOM exit).
func (c *Client) InfoRaw(ctx context.Context) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+"/info", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker info http %d: %s", resp.StatusCode, truncate(body, 200))
	}
	return json.RawMessage(body), nil
}

// SystemDFRaw fetches /system/df — disk usage broken down by Images,
// Containers, Volumes, BuildCache. Useful when the host is under disk
// pressure and the operator needs to know which Docker object class is the
// heaviest. Slower than /info (the daemon walks every layer/volume); on hosts
// with hundreds of build-cache entries the call can take several seconds, so
// container_system_info gates it behind disk:true (default true; set false
// when a fast snapshot is preferred).
func (c *Client) SystemDFRaw(ctx context.Context) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+"/system/df", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker system df http %d: %s", resp.StatusCode, truncate(body, 200))
	}
	return json.RawMessage(body), nil
}
