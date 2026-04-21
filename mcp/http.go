package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mahdi-salmanzade/hippo/internal/sse"
)

// mcpSessionHeader is the header MCP servers use to issue and expect
// a session identifier over Streamable HTTP. Once the server returns
// one on the initialize response, hippo echoes it back on every
// subsequent request.
const mcpSessionHeader = "Mcp-Session-Id"

// httpTransport implements the MCP Streamable HTTP transport. Each
// request is a POST; the response Content-Type decides whether we
// parse one JSON-RPC message or scan an SSE stream for the matching
// response.
type httpTransport struct {
	url        string
	headers    http.Header
	client     *http.Client
	log        *slog.Logger

	sidMu sync.Mutex
	sid   string

	// dead is closed exactly once via deadOne (Close races markDead).
	dead     chan struct{}
	deadOne  sync.Once
	closed   chan struct{}
	closeOne sync.Once
}

// startHTTPTransport builds a httpTransport. Does not perform any
// network I/O - the Client drives the initialize handshake.
func startHTTPTransport(url string, headers http.Header, log *slog.Logger) *httpTransport {
	h := &httpTransport{
		url:     url,
		headers: headers.Clone(),
		client:  &http.Client{Timeout: 60 * time.Second},
		log:     log,
		dead:    make(chan struct{}),
		closed:  make(chan struct{}),
	}
	return h
}

func (t *httpTransport) Disconnected() <-chan struct{} { return t.dead }

func (t *httpTransport) Close() error {
	t.closeOne.Do(func() {
		close(t.closed)
		t.deadOne.Do(func() { close(t.dead) })
	})
	return nil
}

// Send POSTs req and returns the response with the matching id.
// Supports both application/json and text/event-stream response
// bodies (the SSE case may carry multiple JSON-RPC messages; we
// return the first one whose id matches).
func (t *httpTransport) Send(ctx context.Context, req *jsonrpcMessage) (*jsonrpcMessage, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: http marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mcp: http build request: %w", err)
	}
	httpReq.Header = t.headers.Clone()
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if sid := t.sessionID(); sid != "" {
		httpReq.Header.Set(mcpSessionHeader, sid)
	}

	resp, err := t.client.Do(httpReq)
	if err != nil {
		t.markDead()
		return nil, fmt.Errorf("mcp: http send: %w", err)
	}
	defer resp.Body.Close()

	if sid := resp.Header.Get(mcpSessionHeader); sid != "" {
		t.setSessionID(sid)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		// A 404 on a previously-valid session id means the server
		// restarted and forgot us; drop the sid so the next request
		// renegotiates. Caller still gets the error for this Send.
		if resp.StatusCode == http.StatusNotFound {
			t.setSessionID("")
		}
		return nil, fmt.Errorf("mcp: http %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	ct := resp.Header.Get("Content-Type")
	wantID := idString(req.ID)

	switch {
	case strings.HasPrefix(ct, "text/event-stream"):
		return readSSEResponse(ctx, resp.Body, wantID)
	case strings.HasPrefix(ct, "application/json") || ct == "":
		return readJSONResponse(resp.Body, wantID)
	default:
		return nil, fmt.Errorf("mcp: http unsupported Content-Type %q", ct)
	}
}

func (t *httpTransport) Notify(ctx context.Context, req *jsonrpcMessage) error {
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("mcp: http marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header = t.headers.Clone()
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if sid := t.sessionID(); sid != "" {
		httpReq.Header.Set(mcpSessionHeader, sid)
	}

	resp, err := t.client.Do(httpReq)
	if err != nil {
		t.markDead()
		return err
	}
	// Notifications may receive a 202 Accepted or a 200 with no body;
	// drain so the connection returns to the pool.
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return nil
}

func (t *httpTransport) sessionID() string {
	t.sidMu.Lock()
	defer t.sidMu.Unlock()
	return t.sid
}

func (t *httpTransport) setSessionID(sid string) {
	t.sidMu.Lock()
	t.sid = sid
	t.sidMu.Unlock()
}

func (t *httpTransport) markDead() {
	t.deadOne.Do(func() { close(t.dead) })
}

// readJSONResponse parses a single JSON-RPC message from the body.
// If the id does not match wantID, returns an error so the caller
// surfaces it to the user rather than dropping responses silently.
func readJSONResponse(r io.Reader, wantID string) (*jsonrpcMessage, error) {
	data, err := io.ReadAll(io.LimitReader(r, 8<<20)) // 8 MiB cap
	if err != nil {
		return nil, fmt.Errorf("mcp: http read: %w", err)
	}
	var msg jsonrpcMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("mcp: http decode: %w", err)
	}
	if wantID != "" && idString(msg.ID) != wantID {
		return nil, fmt.Errorf("mcp: response id %q did not match request %q",
			idString(msg.ID), wantID)
	}
	return &msg, nil
}

// readSSEResponse scans the body for frames until it finds the one
// carrying the JSON-RPC response with the requested id. Other frames
// (notifications, unrelated responses, comments) are dropped.
func readSSEResponse(ctx context.Context, r io.Reader, wantID string) (*jsonrpcMessage, error) {
	scanner := sse.NewScanner(r)
	for {
		ev, err := scanner.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("mcp: sse closed before response for id %q", wantID)
			}
			return nil, fmt.Errorf("mcp: sse read: %w", err)
		}
		if len(ev.Data) == 0 {
			continue
		}
		var msg jsonrpcMessage
		if err := json.Unmarshal(ev.Data, &msg); err != nil {
			continue
		}
		if idString(msg.ID) == wantID {
			return &msg, nil
		}
	}
}
