package dockerlog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// EventsRaw fetches Docker daemon events for `container` over a bounded time
// window. Returns the raw NDJSON body (one JSON record per line) so the caller
// can parse the fields it needs without pulling the full event schema here.
//
// Both `sinceUnix` and `untilUnix` must be in the past for the call to
// terminate — without `until` the /events endpoint streams indefinitely, which
// would hang the tool handler. The caller is responsible for resolving
// "now"-relative durations to absolute unix seconds before calling.
//
// `eventTypes` filters the daemon-side response by Action (die / oom / kill /
// start / restart / stop / health_status / etc.). Pass nil OR an empty slice
// to disable the event filter — both result in "all container events in
// window". The daemon's filters API is permissive: unknown event types match
// nothing rather than erroring, so a typo silently returns zero rows. Caller
// is expected to validate beforehand if that matters.
func (c *Client) EventsRaw(ctx context.Context, container string, sinceUnix, untilUnix int64, eventTypes []string) (json.RawMessage, error) {
	q := url.Values{}
	q.Set("since", strconv.FormatInt(sinceUnix, 10))
	q.Set("until", strconv.FormatInt(untilUnix, 10))

	filters := map[string][]string{
		"container": {container},
		"type":      {"container"},
	}
	if len(eventTypes) > 0 {
		filters["event"] = eventTypes
	}
	fbytes, err := json.Marshal(filters)
	if err != nil {
		return nil, fmt.Errorf("marshal filters: %w", err)
	}
	q.Set("filters", string(fbytes))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL()+"/events?"+q.Encode(), nil)
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
		return nil, fmt.Errorf("docker events http %d: %s", resp.StatusCode, truncate(body, 200))
	}
	return json.RawMessage(body), nil
}
