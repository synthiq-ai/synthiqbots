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
	"strings"
	"time"
)

// ExecOpts configures a one-shot exec inside a running container.
//
// Stdout / Stderr default to in-memory buffers captured on the result. Pass
// io.Writer values to stream output (mysqldump → gzip writer, etc.) — the
// captured fields will be empty in that case.
type ExecOpts struct {
	Cmd    []string
	Stdin  io.Reader // optional; pass nil for no stdin
	Stdout io.Writer // optional; nil → captured into ExecResult.Stdout
	Stderr io.Writer // optional; nil → captured into ExecResult.Stderr
	Env    []string  // optional extra env, KEY=VALUE
}

// ExecResult is the outcome of an Exec call.
type ExecResult struct {
	Stdout   []byte // empty if opts.Stdout was set
	Stderr   []byte // empty if opts.Stderr was set
	ExitCode int
}

// Exec runs Cmd inside container and returns stdout/stderr/exit code.
//
// Implements the docker exec dance:
//
//  1. POST /containers/{id}/exec  → {Id}
//  2. POST /exec/{Id}/start (hijacked) — multiplexed stdout/stderr
//     stream comes back; if stdin was attached we write it on the same conn.
//  3. GET  /exec/{Id}/json → {ExitCode}
//
// We bypass the http.Client for step 2 because the response is a hijacked
// bi-directional stream and the stdlib http transport can't safely keep that
// open while we write stdin. The dial helper below produces a raw conn over
// either the unix socket OR the TCP host depending on transport.
func (c *Client) Exec(ctx context.Context, container string, opts ExecOpts) (*ExecResult, error) {
	if len(opts.Cmd) == 0 {
		return nil, fmt.Errorf("exec: empty cmd")
	}
	attachStdin := opts.Stdin != nil

	// Step 1: create exec instance.
	createBody := map[string]any{
		"AttachStdin":  attachStdin,
		"AttachStdout": true,
		"AttachStderr": true,
		"Tty":          false,
		"Cmd":          opts.Cmd,
	}
	if len(opts.Env) > 0 {
		createBody["Env"] = opts.Env
	}
	createJSON, _ := json.Marshal(createBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL()+"/containers/"+url.PathEscape(container)+"/exec",
		bytes.NewReader(createJSON))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}
	createBodyResp, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("exec create http %d: %s", resp.StatusCode, truncate(createBodyResp, 200))
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(createBodyResp, &created); err != nil || created.ID == "" {
		return nil, fmt.Errorf("exec create decode: %w (body=%s)", err, truncate(createBodyResp, 200))
	}

	// Step 2: hijacked start.
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("exec dial: %w", err)
	}
	defer conn.Close()
	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(d)
	} else {
		_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	}
	startJSON, _ := json.Marshal(map[string]any{"Detach": false, "Tty": false})
	startReq := "POST /exec/" + url.PathEscape(created.ID) + "/start HTTP/1.1\r\n" +
		"Host: docker\r\n" +
		"Content-Type: application/json\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: tcp\r\n" +
		fmt.Sprintf("Content-Length: %d\r\n", len(startJSON)) +
		"\r\n"
	if _, err := conn.Write([]byte(startReq)); err != nil {
		return nil, fmt.Errorf("exec start write headers: %w", err)
	}
	if _, err := conn.Write(startJSON); err != nil {
		return nil, fmt.Errorf("exec start write body: %w", err)
	}

	br := bufio.NewReader(conn)
	// Drain HTTP status line + headers until the empty line that precedes the
	// hijacked stream. Both 101 (Upgrade success) and 200 (no upgrade — happens
	// against some daemon versions) are accepted.
	statusLine, err := br.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("exec start read status: %w", err)
	}
	if !strings.Contains(statusLine, " 101 ") && !strings.Contains(statusLine, " 200 ") {
		// Drain remaining headers + body for diagnostics.
		body, _ := io.ReadAll(br)
		return nil, fmt.Errorf("exec start unexpected status: %s body=%s",
			strings.TrimSpace(statusLine), truncate(body, 200))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("exec start read headers: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	// Stdin (if any) flushes in a goroutine while we read demuxed output.
	stdinErr := make(chan error, 1)
	if attachStdin {
		go func() {
			_, err := io.Copy(conn, opts.Stdin)
			// Close the write half so docker sees EOF on stdin. For unix
			// sockets and TCP this is conn.(*net.UnixConn|*net.TCPConn).CloseWrite.
			if cw, ok := conn.(interface{ CloseWrite() error }); ok {
				_ = cw.CloseWrite()
			}
			stdinErr <- err
		}()
	}

	stdoutBuf, stderrBuf, err := drainExecStream(br, opts.Stdout, opts.Stderr)
	if err != nil {
		return nil, fmt.Errorf("exec drain: %w", err)
	}
	if attachStdin {
		// Don't fail the call on a stdin EOF — it's expected; only surface real errors.
		if e := <-stdinErr; e != nil && e != io.EOF && !strings.Contains(e.Error(), "closed") {
			return nil, fmt.Errorf("exec stdin: %w", e)
		}
	}

	// Step 3: poll exit code (ContainerExecInspect equivalent).
	inspReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.baseURL()+"/exec/"+url.PathEscape(created.ID)+"/json", nil)
	if err != nil {
		return nil, err
	}
	inspResp, err := c.http.Do(inspReq)
	if err != nil {
		return nil, fmt.Errorf("exec inspect: %w", err)
	}
	defer inspResp.Body.Close()
	if inspResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(inspResp.Body)
		return nil, fmt.Errorf("exec inspect http %d: %s", inspResp.StatusCode, truncate(body, 200))
	}
	var inspBody struct {
		Running  bool `json:"Running"`
		ExitCode int  `json:"ExitCode"`
	}
	if err := json.NewDecoder(inspResp.Body).Decode(&inspBody); err != nil {
		return nil, fmt.Errorf("exec inspect decode: %w", err)
	}
	if inspBody.Running {
		// We finished draining the exec stream but the daemon still says
		// the exec is running — most likely the connection was force-closed.
		// Surface this rather than silently report exit 0.
		return nil, fmt.Errorf("exec finished streaming but inspect reports still running (id=%s)", created.ID)
	}

	res := &ExecResult{ExitCode: inspBody.ExitCode}
	if opts.Stdout == nil {
		res.Stdout = stdoutBuf
	}
	if opts.Stderr == nil {
		res.Stderr = stderrBuf
	}
	return res, nil
}

// drainExecStream reads multiplexed exec output (8-byte header per frame,
// same shape as /logs) until EOF and writes stdout/stderr to the provided
// writers OR captures them into byte slices when the writer is nil.
func drainExecStream(br *bufio.Reader, stdoutW, stderrW io.Writer) ([]byte, []byte, error) {
	var (
		stdoutBuf bytes.Buffer
		stderrBuf bytes.Buffer
	)
	hdr := make([]byte, 8)
	for {
		_, err := io.ReadFull(br, hdr)
		if err != nil {
			// Clean EOF on a frame boundary = stream ended; that's normal.
			// ErrUnexpectedEOF mid-header = truncated stream — surface it
			// because the caller would otherwise treat the partial output
			// as a successful exec and miss data loss in mysqldump etc.
			if err == io.EOF {
				return stdoutBuf.Bytes(), stderrBuf.Bytes(), nil
			}
			return nil, nil, fmt.Errorf("read frame header: %w", err)
		}
		streamType := hdr[0]
		size := binary.BigEndian.Uint32(hdr[4:8])
		if size == 0 {
			continue
		}
		buf := make([]byte, size)
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil, nil, fmt.Errorf("read frame payload (%d bytes): %w", size, err)
		}
		switch streamType {
		case 1: // stdout
			if stdoutW != nil {
				if _, err := stdoutW.Write(buf); err != nil {
					return nil, nil, fmt.Errorf("write stdout: %w", err)
				}
			} else {
				stdoutBuf.Write(buf)
			}
		case 2: // stderr
			if stderrW != nil {
				if _, err := stderrW.Write(buf); err != nil {
					return nil, nil, fmt.Errorf("write stderr: %w", err)
				}
			} else {
				stderrBuf.Write(buf)
			}
		}
	}
}

// dial opens a raw connection to the Docker daemon (unix socket or tcp).
// Used by exec start, which is a hijacked HTTP request the http.Client
// can't keep open while we also write to it.
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	if c.tcpHost != "" {
		return d.DialContext(ctx, "tcp", c.tcpHost)
	}
	sock := c.socket
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	return d.DialContext(ctx, "unix", sock)
}
