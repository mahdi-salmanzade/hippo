package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

// defaultInitTimeout is how long Connect waits for initialize to
// complete before giving up. MCP servers typically respond in under
// a second; the 15s cap is generous enough for servers that need to
// boot a model or perform slow startup work.
const defaultInitTimeout = 15 * time.Second

// Default reconnect schedule.
const (
	defaultReconnectBase = 1 * time.Second
	defaultReconnectMax  = 60 * time.Second
)

// Client is a live connection to one MCP server. Construct via
// Connect (stdio) or ConnectHTTP (Streamable HTTP). Once Connected,
// call Tools() to get a []hippo.Tool slice suitable for passing to
// hippo.WithMCPClients.
type Client struct {
	prefix      string
	log         *slog.Logger
	initTimeout time.Duration

	reconnectEnabled bool
	reconnectBase    time.Duration
	reconnectMax     time.Duration

	// factory rebuilds the transport on reconnect. Connect and
	// ConnectHTTP supply closures that bake in their stdio command
	// or URL+headers so the reconnect loop is transport-agnostic.
	factory func(ctx context.Context) (transport, error)

	// mu guards transport, tools, and serverName across reconnects.
	mu         sync.RWMutex
	transport  transport
	tools      []hippo.Tool
	serverName string

	connected atomic.Bool
	nextID    atomic.Int64

	rootCtx    context.Context
	cancelRoot context.CancelFunc

	closed atomic.Bool
	wg     sync.WaitGroup
}

// Option configures a Client. Applied in order; later options
// override earlier ones.
type Option func(*Client)

// WithPrefix namespaces every tool exposed by this server. Empty
// string (the default) leaves tool names untouched. Prefix + "_" +
// remote name is the resulting hippo.Tool.Name(); tools whose
// prefixed name fails hippo's name validation are skipped with a
// Warn log.
func WithPrefix(prefix string) Option {
	return func(c *Client) { c.prefix = prefix }
}

// WithLogger sets the structured logger. Defaults to a discard logger
// so library callers who never wire logging get silent output.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) { c.log = l }
}

// WithInitTimeout caps the initialize handshake. Default 15s.
func WithInitTimeout(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.initTimeout = d
		}
	}
}

// WithReconnect configures the auto-reconnect loop. enabled=false
// disables reconnection entirely — Tools() returns an empty slice
// after the transport dies. Otherwise the loop uses exponential
// backoff from baseDelay, capped at maxDelay.
func WithReconnect(enabled bool, baseDelay, maxDelay time.Duration) Option {
	return func(c *Client) {
		c.reconnectEnabled = enabled
		if baseDelay > 0 {
			c.reconnectBase = baseDelay
		}
		if maxDelay > 0 {
			c.reconnectMax = maxDelay
		}
	}
}

// newClient builds a Client with defaults and applies options. Does
// not perform any I/O — Connect / ConnectHTTP do that.
func newClient(opts ...Option) *Client {
	c := &Client{
		log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		initTimeout:      defaultInitTimeout,
		reconnectEnabled: true,
		reconnectBase:    defaultReconnectBase,
		reconnectMax:     defaultReconnectMax,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Connect launches command and speaks MCP over its stdin/stdout.
// Blocks until the initialize handshake succeeds or the init timeout
// elapses. On success, Client.Tools() is populated and a reconnect
// goroutine is spawned (unless disabled via WithReconnect).
func Connect(ctx context.Context, command []string, opts ...Option) (*Client, error) {
	if len(command) == 0 {
		return nil, errors.New("mcp: Connect: empty command")
	}
	c := newClient(opts...)
	c.factory = func(ctx context.Context) (transport, error) {
		return startStdioTransport(ctx, command, c.log)
	}
	if err := c.start(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// ConnectHTTP opens a Streamable HTTP connection to url and runs the
// initialize handshake.
func ConnectHTTP(ctx context.Context, url string, opts ...Option) (*Client, error) {
	return ConnectHTTPWithHeaders(ctx, url, nil, opts...)
}

// ConnectHTTPWithHeaders is the header-aware variant; headers are sent
// on every request. Use for Bearer auth or API keys.
func ConnectHTTPWithHeaders(ctx context.Context, url string, headers http.Header, opts ...Option) (*Client, error) {
	if url == "" {
		return nil, errors.New("mcp: ConnectHTTP: empty URL")
	}
	c := newClient(opts...)
	if headers == nil {
		headers = http.Header{}
	}
	c.factory = func(ctx context.Context) (transport, error) {
		return startHTTPTransport(url, headers, c.log), nil
	}
	if err := c.start(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// start runs the first handshake synchronously and, on success,
// spawns the reconnect monitor.
func (c *Client) start(ctx context.Context) error {
	c.rootCtx, c.cancelRoot = context.WithCancel(ctx)
	if err := c.handshake(ctx); err != nil {
		c.cancelRoot()
		return err
	}
	if c.reconnectEnabled {
		c.wg.Add(1)
		go c.reconnectLoop()
	}
	return nil
}

// handshake builds a fresh transport via the factory, runs
// initialize + initialized + tools/list, and atomically installs the
// result into the Client.
func (c *Client) handshake(ctx context.Context) error {
	initCtx, cancel := context.WithTimeout(ctx, c.initTimeout)
	defer cancel()

	tr, err := c.factory(c.rootCtx)
	if err != nil {
		return err
	}

	info, err := c.sendInitialize(initCtx, tr)
	if err != nil {
		_ = tr.Close()
		return fmt.Errorf("mcp: initialize: %w", err)
	}

	if info.ProtocolVersion != "" && info.ProtocolVersion != ProtocolVersion {
		c.log.Warn("mcp: protocol version mismatch (continuing anyway)",
			"server", info.ServerInfo.Name,
			"server_version", info.ProtocolVersion,
			"client_version", ProtocolVersion)
	}

	initializedMsg, err := newNotification("notifications/initialized", struct{}{})
	if err != nil {
		_ = tr.Close()
		return err
	}
	if err := tr.Notify(initCtx, initializedMsg); err != nil {
		_ = tr.Close()
		return fmt.Errorf("mcp: notify initialized: %w", err)
	}

	serverTools, err := c.sendToolsList(initCtx, tr)
	if err != nil {
		// A server that doesn't support tools shouldn't fail the
		// whole connection — the Client is still valid for future
		// capability discovery. Log and continue with zero tools.
		c.log.Warn("mcp: tools/list failed; continuing with empty tool set",
			"server", info.ServerInfo.Name, "err", err)
		serverTools = nil
	}

	converted, skipped := c.buildTools(serverTools)
	for _, s := range skipped {
		c.log.Warn("mcp: tool skipped (invalid hippo name)",
			"remote_name", s.remote, "attempted_name", s.local, "reason", s.reason)
	}

	c.mu.Lock()
	prev := c.transport
	c.transport = tr
	c.tools = converted
	c.serverName = info.ServerInfo.Name
	c.mu.Unlock()
	c.connected.Store(true)

	if prev != nil {
		_ = prev.Close()
	}
	return nil
}

// sendInitialize fires the initialize request and returns the parsed
// result.
func (c *Client) sendInitialize(ctx context.Context, tr transport) (*initializeResult, error) {
	params := initializeParams{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    capabilities{},
		ClientInfo: clientInfo{
			Name:    clientName,
			Version: ClientVersion,
		},
	}
	req, err := newRequest(c.nextID.Add(1), "initialize", params)
	if err != nil {
		return nil, err
	}
	resp, err := tr.Send(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	var res initializeResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return nil, fmt.Errorf("mcp: decode initialize: %w", err)
	}
	return &res, nil
}

// sendToolsList fires tools/list and returns the raw server tool
// definitions.
func (c *Client) sendToolsList(ctx context.Context, tr transport) ([]mcpServerTool, error) {
	req, err := newRequest(c.nextID.Add(1), "tools/list", struct{}{})
	if err != nil {
		return nil, err
	}
	resp, err := tr.Send(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	var res toolsListResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		return nil, fmt.Errorf("mcp: decode tools/list: %w", err)
	}
	return res.Tools, nil
}

// skippedTool records one tool that failed prefix+validation so the
// caller can log it. Kept small to avoid import cycles.
type skippedTool struct {
	remote string
	local  string
	reason string
}

// buildTools converts each server tool into an mcpTool, applying the
// prefix and dropping any whose prefixed name fails hippo's name
// validation. Validation uses the same regex hippo's core ToolSet
// requires, indirectly, by funneling through NewToolSet in hippo.
// Here we duplicate the regex to avoid a cycle.
func (c *Client) buildTools(raw []mcpServerTool) ([]hippo.Tool, []skippedTool) {
	out := make([]hippo.Tool, 0, len(raw))
	var skipped []skippedTool
	for _, s := range raw {
		name := s.Name
		if c.prefix != "" {
			name = c.prefix + "_" + name
		}
		if !isValidToolName(name) {
			skipped = append(skipped, skippedTool{
				remote: s.Name, local: name,
				reason: "hippo.Tool name pattern",
			})
			continue
		}
		out = append(out, &mcpTool{
			name:        name,
			remoteName:  s.Name,
			description: s.Description,
			schema:      append(json.RawMessage(nil), s.InputSchema...),
			client:      c,
		})
	}
	return out, skipped
}

// reconnectLoop watches the current transport's Disconnected channel
// and re-runs the handshake on failure, with exponential backoff.
func (c *Client) reconnectLoop() {
	defer c.wg.Done()
	for {
		c.mu.RLock()
		tr := c.transport
		c.mu.RUnlock()
		if tr == nil {
			return
		}

		select {
		case <-c.rootCtx.Done():
			return
		case <-tr.Disconnected():
			if c.closed.Load() {
				return
			}
			c.connected.Store(false)
			c.log.Warn("mcp: transport disconnected; starting reconnect loop")
		}

		attempt := 0
		for {
			if c.closed.Load() || c.rootCtx.Err() != nil {
				return
			}
			delay := backoffDelay(attempt, c.reconnectBase, c.reconnectMax)
			c.log.Info("mcp: reconnecting",
				"attempt", attempt+1, "delay", delay)
			select {
			case <-c.rootCtx.Done():
				return
			case <-time.After(delay):
			}
			if err := c.handshake(c.rootCtx); err != nil {
				attempt++
				c.log.Warn("mcp: reconnect failed", "err", err)
				continue
			}
			c.log.Info("mcp: reconnected")
			break
		}
	}
}

// backoffDelay returns the exponential delay for the given attempt,
// capped at max. attempt=0 yields base; each subsequent attempt
// doubles the previous value.
func backoffDelay(attempt int, base, max time.Duration) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	d := base
	for i := 0; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}

// Tools returns a snapshot of the server's tools translated into
// hippo.Tool. Safe for concurrent callers; the slice itself must not
// be mutated.
func (c *Client) Tools() []hippo.Tool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]hippo.Tool, len(c.tools))
	copy(out, c.tools)
	return out
}

// Name returns the server's self-reported name, or an empty string
// before the first successful handshake.
func (c *Client) Name() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.serverName
}

// Prefix reports the configured namespace prefix, possibly empty.
func (c *Client) Prefix() string { return c.prefix }

// Connected reports whether the transport is currently usable.
func (c *Client) Connected() bool { return c.connected.Load() }

// Close terminates the connection and stops the reconnect loop.
// Idempotent.
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	if c.cancelRoot != nil {
		c.cancelRoot()
	}
	c.mu.Lock()
	tr := c.transport
	c.transport = nil
	c.tools = nil
	c.mu.Unlock()
	c.connected.Store(false)
	if tr != nil {
		_ = tr.Close()
	}
	// Block on reconnect goroutine exit so callers can trust that
	// Close leaves no stray goroutines behind.
	c.wg.Wait()
	return nil
}

// currentTransport returns the live transport under a read lock, or
// nil if the Client is disconnected. mcpTool uses it to route calls
// without holding the lock while the request is in flight.
func (c *Client) currentTransport() transport {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.transport
}
