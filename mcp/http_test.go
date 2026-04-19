package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// httpServer is a test helper implementing minimal MCP over
// Streamable HTTP with either application/json or text/event-stream
// responses per request.
type httpServer struct {
	t            *testing.T
	sse          bool
	headers      http.Header
	session      string
	sessionCalls atomic.Int64
}

func newHTTPServer(t *testing.T, sse bool) *httpServer {
	return &httpServer{t: t, sse: sse, session: "sess-abc"}
}

func (s *httpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.headers = r.Header.Clone()
	sid := r.Header.Get(mcpSessionHeader)
	if sid == s.session {
		s.sessionCalls.Add(1)
	}

	var req jsonrpcMessage
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	resp := &jsonrpcMessage{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		result, _ := json.Marshal(initializeResult{
			ProtocolVersion: ProtocolVersion,
			ServerInfo:      serverInfo{Name: "http-mock", Version: "0"},
		})
		resp.Result = result
		w.Header().Set(mcpSessionHeader, s.session)
	case "tools/list":
		result, _ := json.Marshal(toolsListResult{
			Tools: []mcpServerTool{{Name: "ping", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		})
		resp.Result = result
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return
	case "tools/call":
		result, _ := json.Marshal(toolsCallResult{
			Content: []toolContent{{Type: "text", Text: "pong"}},
		})
		resp.Result = result
	default:
		resp.Error = &jsonrpcError{Code: -32601, Message: "unknown"}
	}

	payload, _ := json.Marshal(resp)
	if s.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
		flusher.Flush()
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write(payload)
}

func TestHTTPTransportJSONResponse(t *testing.T) {
	srv := httptest.NewServer(newHTTPServer(t, false))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := ConnectHTTP(ctx, srv.URL, WithLogger(discardLogger()), WithReconnect(false, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if len(c.Tools()) != 1 {
		t.Fatalf("tools = %d", len(c.Tools()))
	}
	res, err := c.Tools()[0].Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "pong" {
		t.Errorf("Content = %q", res.Content)
	}
}

func TestHTTPTransportSSEResponse(t *testing.T) {
	srv := httptest.NewServer(newHTTPServer(t, true))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := ConnectHTTP(ctx, srv.URL, WithLogger(discardLogger()), WithReconnect(false, 0, 0))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	if len(c.Tools()) != 1 {
		t.Fatalf("tools = %d", len(c.Tools()))
	}
}

func TestHTTPTransportSendsSessionIDAfterInitialize(t *testing.T) {
	handler := newHTTPServer(t, false)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := ConnectHTTP(ctx, srv.URL, WithLogger(discardLogger()), WithReconnect(false, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// initialize + initialized + tools/list — the last two carry the
	// session id. Expect the session-match counter to have moved.
	if handler.sessionCalls.Load() == 0 {
		t.Errorf("expected session id to be echoed on subsequent requests")
	}
}

func TestHTTPTransportSendsCustomHeaders(t *testing.T) {
	handler := newHTTPServer(t, false)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	hdrs := http.Header{"Authorization": []string{"Bearer xyz"}}
	c, err := ConnectHTTPWithHeaders(ctx, srv.URL, hdrs, WithLogger(discardLogger()), WithReconnect(false, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !strings.Contains(handler.headers.Get("Authorization"), "Bearer xyz") {
		t.Errorf("Authorization = %q", handler.headers.Get("Authorization"))
	}
}
