package dockerlog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Client talks to the Docker Engine API.
//
// Two transports supported:
//   - unix socket (default, e.g. /var/run/docker.sock)
//   - TCP (DOCKER_HOST env var, e.g. tcp://192.168.100.11:2375)
//
// We avoid pulling the full docker SDK — we only need a handful of endpoints
// (containers/{id}/{json,logs,restart,stop,start} and containers/json), all
// stable since API v1.24.
type Client struct {
	socket  string // unix socket path; empty when using tcpHost
	tcpHost string // host:port for tcp transport; empty when using socket
	http    *http.Client
}

// New constructs a Client. If $DOCKER_HOST is set and parses as a tcp:// URL
// the client uses TCP, otherwise it falls back to the given socket path
// (or /var/run/docker.sock if socket=="").
func New(socket string) *Client {
	if env := strings.TrimSpace(os.Getenv("DOCKER_HOST")); env != "" {
		if strings.HasPrefix(env, "tcp://") {
			host := strings.TrimPrefix(env, "tcp://")
			tr := &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					d := net.Dialer{Timeout: 5 * time.Second}
					return d.DialContext(ctx, "tcp", host)
				},
			}
			return &Client{tcpHost: host, http: &http.Client{Transport: tr}}
		}
		if strings.HasPrefix(env, "unix://") {
			socket = strings.TrimPrefix(env, "unix://")
		}
	}
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{
		socket: socket,
		http:   &http.Client{Transport: tr}, // no client-level timeout: SSE follow is long-running
	}
}

// baseURL is the per-request URL prefix. The host portion is irrelevant for the
// unix-socket transport (DialContext ignores it) but must be syntactically valid.
func (c *Client) baseURL() string {
	if c.tcpHost != "" {
		return "http://" + c.tcpHost
	}
	return "http://docker"
}

// Transport returns "tcp" or "unix" — used in error messages and tool descriptions.
func (c *Client) Transport() string {
	if c.tcpHost != "" {
		return "tcp"
	}
	return "unix"
}

type Inspect struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		Restarting bool   `json:"Restarting"`
		StartedAt  string `json:"StartedAt"`
		ExitCode   int    `json:"ExitCode"`
	} `json:"State"`
	Image        string `json:"Image"`
	RestartCount int    `json:"RestartCount"`
}

func (c *Client) Inspect(ctx context.Context, container string) (*Inspect, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL()+"/containers/"+url.PathEscape(container)+"/json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker inspect http %d", resp.StatusCode)
	}
	var out Inspect
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// InspectRaw returns the raw JSON of /containers/{id}/json so admin tools can
// surface fields not modeled in our typed Inspect (network, mounts, env, etc.).
func (c *Client) InspectRaw(ctx context.Context, container string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL()+"/containers/"+url.PathEscape(container)+"/json", nil)
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
		return nil, fmt.Errorf("docker inspect http %d: %s", resp.StatusCode, truncate(body, 200))
	}
	return json.RawMessage(body), nil
}

// ContainerSummary is the subset of /containers/json we surface.
type ContainerSummary struct {
	ID      string            `json:"id"`
	Names   []string          `json:"names"`
	Image   string            `json:"image"`
	State   string            `json:"state"`
	Status  string            `json:"status"`
	Created int64             `json:"created"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// List returns all containers (running and stopped).
func (c *Client) List(ctx context.Context) ([]ContainerSummary, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL()+"/containers/json?all=1", nil)
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
		return nil, fmt.Errorf("docker list http %d: %s", resp.StatusCode, truncate(body, 200))
	}
	var raw []struct {
		ID      string            `json:"Id"`
		Names   []string          `json:"Names"`
		Image   string            `json:"Image"`
		State   string            `json:"State"`
		Status  string            `json:"Status"`
		Created int64             `json:"Created"`
		Labels  map[string]string `json:"Labels"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode list: %w", err)
	}
	out := make([]ContainerSummary, 0, len(raw))
	for _, r := range raw {
		out = append(out, ContainerSummary{
			ID: r.ID, Names: r.Names, Image: r.Image,
			State: r.State, Status: r.Status, Created: r.Created, Labels: r.Labels,
		})
	}
	return out, nil
}

// StatsRaw fetches a single point-in-time stats snapshot from
// /containers/{id}/stats?stream=false. With stream=false (and the default
// one-shot=false) the daemon returns one sample where precpu_stats reflects a
// reading from ~1 s prior, so a CPU delta IS computable from a single call —
// at the cost of ~1 s server-side latency. Returns the raw JSON; the caller
// parses out the fields it surfaces (matches the InspectRaw pattern above).
func (c *Client) StatsRaw(ctx context.Context, container string) (json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL()+"/containers/"+url.PathEscape(container)+"/stats?stream=false", nil)
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
		return nil, fmt.Errorf("docker stats http %d: %s", resp.StatusCode, truncate(body, 200))
	}
	return json.RawMessage(body), nil
}

// Restart issues POST /containers/{id}/restart?t=<seconds>. Returns nil on 204.
func (c *Client) Restart(ctx context.Context, container string, timeoutSec int) error {
	return c.lifecycleAction(ctx, container, "restart", timeoutSec)
}

// Stop issues POST /containers/{id}/stop?t=<seconds>. 304 (already stopped) is treated as success.
func (c *Client) Stop(ctx context.Context, container string, timeoutSec int) error {
	return c.lifecycleAction(ctx, container, "stop", timeoutSec)
}

// Start issues POST /containers/{id}/start. 304 (already running) is treated as success.
func (c *Client) Start(ctx context.Context, container string) error {
	return c.lifecycleAction(ctx, container, "start", 0)
}

func (c *Client) lifecycleAction(ctx context.Context, container, action string, timeoutSec int) error {
	q := url.Values{}
	if action != "start" && timeoutSec > 0 {
		q.Set("t", strconv.Itoa(timeoutSec))
	}
	u := c.baseURL() + "/containers/" + url.PathEscape(container) + "/" + action
	if e := q.Encode(); e != "" {
		u += "?" + e
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(nil))
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 204: success. 304: already in target state (treated as success per docker convention).
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified {
		return nil
	}
	body, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("docker %s http %d: %s", action, resp.StatusCode, truncate(body, 200))
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

type LogOpts struct {
	Tail   string // "200" or "all"
	Since  string // unix seconds OR Go duration like "15m" — we convert duration to unix sec
	Follow bool
}

// Line is one decoded log frame.
type Line struct {
	Stream string // "stdout" or "stderr"
	TS     time.Time
	Msg    string
}

// Stream returns a reader that yields decoded log lines until ctx is cancelled or
// (for !Follow) the stream closes. The closer must be called by the caller.
//
// Docker's logs endpoint multiplexes stdout/stderr with an 8-byte frame header
// when the container has TTY=false. When TTY=true (e.g. ac-worldserver) the
// output is plain bytes — we have to detect this via Inspect first and branch
// the decoder accordingly, otherwise the framed-decoder would read leading
// bytes as bogus frame headers and either deadlock or silently emit nothing.
func (c *Client) Stream(ctx context.Context, container string, opts LogOpts) (<-chan Line, io.Closer, error) {
	insp, err := c.Inspect(ctx, container)
	if err != nil {
		return nil, nil, fmt.Errorf("inspect for tty mode: %w", err)
	}
	tty := false
	{
		var raw struct {
			Config struct {
				Tty bool `json:"Tty"`
			} `json:"Config"`
		}
		// Re-fetch with Tty field — our Inspect struct doesn't include it. Cheap.
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			c.baseURL()+"/containers/"+url.PathEscape(container)+"/json", nil)
		if r, e := c.http.Do(req); e == nil {
			_ = json.NewDecoder(r.Body).Decode(&raw)
			r.Body.Close()
			tty = raw.Config.Tty
		}
		_ = insp
	}

	q := url.Values{}
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	q.Set("timestamps", "1")
	if opts.Tail != "" {
		q.Set("tail", opts.Tail)
	}
	if opts.Since != "" {
		if sec, err := normalizeSince(opts.Since); err == nil && sec > 0 {
			q.Set("since", strconv.FormatInt(sec, 10))
		}
	}
	if opts.Follow {
		q.Set("follow", "1")
	}

	u := c.baseURL()+"/containers/" + url.PathEscape(container) + "/logs?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, nil, fmt.Errorf("docker logs http %d", resp.StatusCode)
	}

	out := make(chan Line, 32)
	go func() {
		defer close(out)
		defer resp.Body.Close()
		if tty {
			decodeRaw(resp.Body, out, ctx.Done())
		} else {
			decodeFrames(resp.Body, out, ctx.Done())
		}
	}()
	return out, resp.Body, nil
}

// decodeRaw handles TTY-mode log streams: line-by-line text with timestamps,
// no multiplex header. Stream is reported as "stdout" since TTY merges both
// streams at the kernel level.
func decodeRaw(r io.Reader, out chan<- Line, done <-chan struct{}) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		select {
		case <-done:
			return
		default:
		}
		ts, msg := splitTimestamp(sc.Text())
		select {
		case out <- Line{Stream: "stdout", TS: ts, Msg: msg}:
		case <-done:
			return
		}
	}
}

// normalizeSince accepts either "15m"/"2h" (relative duration) or a unix-seconds
// epoch. The Docker API wants unix seconds in the `since` query param.
func normalizeSince(s string) (int64, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d).Unix(), nil
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		return v, nil
	}
	return 0, fmt.Errorf("invalid since=%q", s)
}

// decodeFrames consumes the multiplexed Docker log stream and emits Lines.
// Frame layout: [streamType(1), pad(3), size(4 BE)] then size bytes of payload.
// When TTY is enabled on the container the stream is plain (no header), but
// ac-worldserver runs without a TTY so we always see frames.
func decodeFrames(r io.Reader, out chan<- Line, done <-chan struct{}) {
	br := bufio.NewReader(r)
	hdr := make([]byte, 8)
	for {
		select {
		case <-done:
			return
		default:
		}
		if _, err := io.ReadFull(br, hdr); err != nil {
			return
		}
		streamType := hdr[0]
		size := binary.BigEndian.Uint32(hdr[4:8])
		if size == 0 {
			continue
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(br, buf); err != nil {
			return
		}
		// Docker prepends an RFC3339Nano timestamp + space to each line when
		// timestamps=1. Try to split it off.
		ts, msg := splitTimestamp(string(buf))
		stream := "stdout"
		if streamType == 2 {
			stream = "stderr"
		}
		select {
		case out <- Line{Stream: stream, TS: ts, Msg: msg}:
		case <-done:
			return
		}
	}
}

func splitTimestamp(s string) (time.Time, string) {
	// trim trailing newline
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	sp := -1
	for i := 0; i < len(s) && i < 64; i++ {
		if s[i] == ' ' {
			sp = i
			break
		}
	}
	if sp < 0 {
		return time.Time{}, s
	}
	if t, err := time.Parse(time.RFC3339Nano, s[:sp]); err == nil {
		return t, s[sp+1:]
	}
	return time.Time{}, s
}
