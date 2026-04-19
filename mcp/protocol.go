// Package mcp is hippo's client for the Model Context Protocol. It
// connects to MCP servers and exposes their tools as hippo.Tool
// instances so Brain callers can invoke them transparently alongside
// locally-registered tools.
//
// # Scope
//
// Tools only — hippo's MCP integration in v0.1.0 does not expose
// prompts or resources. Notifications (tools/list_changed) are not
// subscribed to; tool lists refresh on each reconnect instead.
//
// # Transports
//
// Two transports are implemented:
//
//   - stdio: launches a subprocess and speaks newline-delimited
//     JSON-RPC 2.0 over its stdin/stdout. See Connect.
//   - Streamable HTTP: the post-2025 MCP HTTP transport. Requests are
//     POSTed and responses arrive as either application/json (single)
//     or text/event-stream (multi-message) bodies. See ConnectHTTP.
//
// # Lifecycle
//
// Connect and ConnectHTTP block until the initialize handshake
// completes or the init timeout elapses. After that, a background
// goroutine monitors the transport and reconnects with exponential
// backoff on failure. Callers only need to handle the error return of
// the initial Connect.
package mcp

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the MCP revision hippo targets. Servers on older
// versions still work — MCP is deliberately loose about version
// matching — but a Warn log is emitted if the server reports a
// different version on initialize.
const ProtocolVersion = "2025-06-18"

// clientName / clientVersion are the identity hippo announces on the
// initialize request. The version is filled in by hippo.Version or
// the binary's build metadata; the default keeps server-side logs
// readable when a third-party embedder hasn't set it.
const clientName = "hippo"

// ClientVersion is the version string hippo sends on initialize.
// Callers (typically the root hippo package) may override this at
// startup — for example, the web package sets it from the binary's
// build-time ldflags.
var ClientVersion = "dev"

// jsonrpcVersion is pinned to the JSON-RPC 2.0 level. MCP has not
// introduced a newer JSON-RPC revision; if that ever changes, this
// becomes a per-message field.
const jsonrpcVersion = "2.0"

// jsonrpcMessage is the unified envelope. A request has Method + ID;
// a notification has Method without ID; a response has ID plus either
// Result or Error. Using one struct with omitempty keeps the marshal
// path trivial at the cost of light ambiguity for readers.
type jsonrpcMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *jsonrpcError    `json:"error,omitempty"`
}

// jsonrpcError is the error payload on a response. Code follows the
// JSON-RPC 2.0 registry (-32700..-32000 reserved); MCP adds its own
// application-level codes but we do not interpret them beyond logging.
type jsonrpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error makes jsonrpcError satisfy the error interface — useful when
// surfacing server-side failures through Go's error chain.
func (e *jsonrpcError) Error() string {
	return fmt.Sprintf("mcp: server error %d: %s", e.Code, e.Message)
}

// newRequest builds a jsonrpcMessage shaped as a request, with an
// int ID serialised to RawMessage so the caller can match responses.
func newRequest(id int64, method string, params any) (*jsonrpcMessage, error) {
	idRaw, err := json.Marshal(id)
	if err != nil {
		return nil, err
	}
	idMsg := json.RawMessage(idRaw)
	msg := &jsonrpcMessage{
		JSONRPC: jsonrpcVersion,
		ID:      &idMsg,
		Method:  method,
	}
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("mcp: marshal params: %w", err)
		}
		msg.Params = p
	}
	return msg, nil
}

// newNotification is the ID-less variant used for initialized and
// shutdown-ish one-shots. Servers do not respond to notifications.
func newNotification(method string, params any) (*jsonrpcMessage, error) {
	msg := &jsonrpcMessage{
		JSONRPC: jsonrpcVersion,
		Method:  method,
	}
	if params != nil {
		p, err := json.Marshal(params)
		if err != nil {
			return nil, fmt.Errorf("mcp: marshal params: %w", err)
		}
		msg.Params = p
	}
	return msg, nil
}

// idString extracts an ID as a stable string key for the pending-map
// lookup. IDs are JSON values (string | int | null); comparing the
// raw JSON bytes works for both numeric and string forms because the
// client produced them.
func idString(raw *json.RawMessage) string {
	if raw == nil {
		return ""
	}
	return string(*raw)
}

// MCP-specific payloads follow. Only the bits hippo actually uses are
// modelled — capability negotiation is declared as empty client
// capabilities, and resources/prompts are ignored.

type initializeParams struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    capabilities `json:"capabilities"`
	ClientInfo      clientInfo   `json:"clientInfo"`
}

// capabilities is intentionally empty on the client side in v0.1.0.
// Adding listChanged subscriptions later requires populating a "tools"
// sub-field; for now the empty object is enough to satisfy the spec's
// initialize contract.
type capabilities struct{}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      serverInfo         `json:"serverInfo"`
}

type serverCapabilities struct {
	Tools *toolsCapability `json:"tools,omitempty"`
}

type toolsCapability struct {
	ListChanged *bool `json:"listChanged,omitempty"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type toolsListResult struct {
	Tools []mcpServerTool `json:"tools"`
}

type mcpServerTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type toolsCallResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError"`
}

// toolContent covers the typed content-block shape MCP servers return
// from tools/call. For v0.1 hippo only renders the "text" case;
// image/resource blocks are replaced with a placeholder so the LLM
// knows something non-textual arrived without seeing binary noise.
type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}
