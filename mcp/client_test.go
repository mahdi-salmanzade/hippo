package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockTransport implements transport with programmable responses.
// Each Send looks up a handler keyed by method name; handlers receive
// the raw params and return a result or an error. Missing handlers
// return a -32601 method-not-found style reply so tests fail loudly
// rather than hanging.
type mockTransport struct {
	handlers map[string]func(params json.RawMessage) (any, *jsonrpcError)

	mu           sync.Mutex
	closed       bool
	dead         chan struct{}
	closeOnce    sync.Once
	sendCalls    []string
	disconnected bool
}

func newMockTransport() *mockTransport {
	return &mockTransport{
		handlers: map[string]func(json.RawMessage) (any, *jsonrpcError){},
		dead:     make(chan struct{}),
	}
}

func (m *mockTransport) handle(method string, fn func(json.RawMessage) (any, *jsonrpcError)) {
	m.handlers[method] = fn
}

func (m *mockTransport) Send(ctx context.Context, req *jsonrpcMessage) (*jsonrpcMessage, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errTransportClosed
	}
	m.sendCalls = append(m.sendCalls, req.Method)
	m.mu.Unlock()

	h, ok := m.handlers[req.Method]
	if !ok {
		return &jsonrpcMessage{
			JSONRPC: jsonrpcVersion,
			ID:      req.ID,
			Error:   &jsonrpcError{Code: -32601, Message: "method not found: " + req.Method},
		}, nil
	}

	// Run the handler in a goroutine so slow handlers don't pin the
	// test goroutine past ctx cancellation. Real transports have this
	// property (they select on ctx.Done during their reads); the mock
	// must match.
	type result struct {
		body *jsonrpcMessage
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		res, jsonErr := h(req.Params)
		resp := &jsonrpcMessage{JSONRPC: jsonrpcVersion, ID: req.ID}
		if jsonErr != nil {
			resp.Error = jsonErr
			ch <- result{body: resp}
			return
		}
		if res != nil {
			body, err := json.Marshal(res)
			if err != nil {
				ch <- result{err: err}
				return
			}
			resp.Result = body
		}
		ch <- result{body: resp}
	}()

	select {
	case r := <-ch:
		return r.body, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.dead:
		return nil, errTransportClosed
	}
}

func (m *mockTransport) Notify(ctx context.Context, req *jsonrpcMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sendCalls = append(m.sendCalls, "notify:"+req.Method)
	return nil
}

func (m *mockTransport) Disconnected() <-chan struct{} { return m.dead }

func (m *mockTransport) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		close(m.dead)
	})
	return nil
}

func (m *mockTransport) markDead() {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.disconnected = true
		m.mu.Unlock()
		close(m.dead)
	})
}

// discardLogger returns a logger that drops every record.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// standardMock wires initialize + tools/list handlers onto a fresh
// transport so individual tests don't repeat boilerplate.
func standardMock(t *testing.T, tools []mcpServerTool) *mockTransport {
	t.Helper()
	m := newMockTransport()
	m.handle("initialize", func(p json.RawMessage) (any, *jsonrpcError) {
		return initializeResult{
			ProtocolVersion: ProtocolVersion,
			ServerInfo:      serverInfo{Name: "mock", Version: "0"},
		}, nil
	})
	m.handle("tools/list", func(p json.RawMessage) (any, *jsonrpcError) {
		return toolsListResult{Tools: tools}, nil
	})
	return m
}

// connectWithTransport skips the transport factory and plugs in a
// pre-built transport directly. Used by every unit test.
func connectWithTransport(t *testing.T, mock *mockTransport, opts ...Option) *Client {
	t.Helper()
	c := newClient(append(opts, WithLogger(discardLogger()))...)
	c.factory = func(ctx context.Context) (transport, error) {
		// Fresh mock on reconnect would need a factory that returns
		// distinct transports; tests that want reconnect supply their
		// own factory via test helpers.
		return mock, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	return c
}

func TestConnectInitializesAndListsTools(t *testing.T) {
	mock := standardMock(t, []mcpServerTool{
		{Name: "echo", Description: "echoes", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "add", Description: "adds", InputSchema: json.RawMessage(`{"type":"object"}`)},
	})
	c := connectWithTransport(t, mock)
	defer c.Close()

	if len(c.Tools()) != 2 {
		t.Fatalf("tools = %d; want 2", len(c.Tools()))
	}
	if c.Name() != "mock" {
		t.Errorf("Name = %q; want mock", c.Name())
	}
	if !c.Connected() {
		t.Error("Connected returned false")
	}
}

func TestPrefixAppliedToToolNames(t *testing.T) {
	mock := standardMock(t, []mcpServerTool{
		{Name: "search", InputSchema: json.RawMessage(`{}`)},
	})
	c := connectWithTransport(t, mock, WithPrefix("dubai"))
	defer c.Close()
	tools := c.Tools()
	if len(tools) != 1 || tools[0].Name() != "dubai_search" {
		t.Fatalf("got %v; want [dubai_search]", tools)
	}
}

func TestPrefixValidationSkipsBadNames(t *testing.T) {
	mock := standardMock(t, []mcpServerTool{
		{Name: "valid_one", InputSchema: json.RawMessage(`{}`)},
		{Name: "has space", InputSchema: json.RawMessage(`{}`)},
		{Name: "dots.are.bad", InputSchema: json.RawMessage(`{}`)},
	})
	c := connectWithTransport(t, mock)
	defer c.Close()
	tools := c.Tools()
	if len(tools) != 1 || tools[0].Name() != "valid_one" {
		var names []string
		for _, tt := range tools {
			names = append(names, tt.Name())
		}
		t.Fatalf("got %v; want [valid_one]", names)
	}
}

func TestToolExecuteRoundtrip(t *testing.T) {
	mock := standardMock(t, []mcpServerTool{
		{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)},
	})
	mock.handle("tools/call", func(p json.RawMessage) (any, *jsonrpcError) {
		var call toolsCallParams
		_ = json.Unmarshal(p, &call)
		var args struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(call.Arguments, &args)
		return toolsCallResult{
			Content: []toolContent{{Type: "text", Text: "echoed: " + args.Text}},
		}, nil
	})
	c := connectWithTransport(t, mock)
	defer c.Close()

	tool := c.Tools()[0]
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Errorf("IsError; Content=%q", res.Content)
	}
	if res.Content != "echoed: hi" {
		t.Errorf("Content = %q; want 'echoed: hi'", res.Content)
	}
}

func TestToolExecuteServerErrorIsIsError(t *testing.T) {
	mock := standardMock(t, []mcpServerTool{
		{Name: "bad", InputSchema: json.RawMessage(`{}`)},
	})
	mock.handle("tools/call", func(p json.RawMessage) (any, *jsonrpcError) {
		return nil, &jsonrpcError{Code: -32000, Message: "nope"}
	})
	c := connectWithTransport(t, mock)
	defer c.Close()

	res, err := c.Tools()[0].Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute returned err: %v", err)
	}
	if !res.IsError {
		t.Error("want IsError=true")
	}
	if !strings.Contains(res.Content, "nope") {
		t.Errorf("Content = %q; want to include 'nope'", res.Content)
	}
}

func TestToolExecuteOnDisconnectReturnsIsError(t *testing.T) {
	mock := standardMock(t, []mcpServerTool{
		{Name: "echo", InputSchema: json.RawMessage(`{}`)},
	})
	c := connectWithTransport(t, mock, WithReconnect(false, 0, 0))
	tool := c.Tools()[0]
	c.Close()
	res, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("expected IsError on disconnect")
	}
}

func TestConnectTimesOutOnStalledInitialize(t *testing.T) {
	block := make(chan struct{})
	m := newMockTransport()
	m.handle("initialize", func(p json.RawMessage) (any, *jsonrpcError) {
		<-block
		return nil, nil
	})
	c := newClient(WithLogger(discardLogger()), WithInitTimeout(50*time.Millisecond))
	c.factory = func(ctx context.Context) (transport, error) { return m, nil }

	err := c.start(context.Background())
	close(block)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		// initialize wraps with "mcp: initialize: …" — check the
		// message path too, since errors.Is on the wrapped form may
		// or may not unwrap depending on wrap helper.
		if !strings.Contains(err.Error(), "context deadline") {
			t.Errorf("err = %v; want deadline", err)
		}
	}
}

func TestBackoffDelayCappedAtMax(t *testing.T) {
	base := 1 * time.Second
	max := 10 * time.Second
	for _, attempt := range []int{0, 1, 2, 3, 4, 5, 10, 20} {
		d := backoffDelay(attempt, base, max)
		if d > max {
			t.Errorf("attempt=%d delay=%v exceeds max=%v", attempt, d, max)
		}
		if d < base {
			t.Errorf("attempt=%d delay=%v below base=%v", attempt, d, base)
		}
	}
	if backoffDelay(0, base, max) != base {
		t.Errorf("attempt=0 should equal base")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	mock := standardMock(t, nil)
	c := connectWithTransport(t, mock)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close returned %v", err)
	}
}

func TestReconnectRestoresToolList(t *testing.T) {
	// Factory that returns a fresh mock each time. First call
	// exposes just [echo]; subsequent reconnects expose [echo, add].
	var mu sync.Mutex
	calls := 0
	factory := func(ctx context.Context) (transport, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		tools := []mcpServerTool{
			{Name: "echo", InputSchema: json.RawMessage(`{}`)},
		}
		if calls >= 2 {
			tools = append(tools, mcpServerTool{Name: "add", InputSchema: json.RawMessage(`{}`)})
		}
		m := standardMock(t, tools)
		return m, nil
	}

	c := newClient(
		WithLogger(discardLogger()),
		WithReconnect(true, 10*time.Millisecond, 40*time.Millisecond),
	)
	c.factory = factory
	if err := c.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if len(c.Tools()) != 1 {
		t.Fatalf("initial tool count = %d", len(c.Tools()))
	}

	// Kill the first transport to trigger reconnect.
	c.mu.RLock()
	tr := c.transport.(*mockTransport)
	c.mu.RUnlock()
	tr.markDead()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.Tools()) == 2 && c.Connected() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("reconnect did not refresh tools; have %d, connected=%v", len(c.Tools()), c.Connected())
}
