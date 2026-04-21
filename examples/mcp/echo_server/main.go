// Command echo_server is a minimal Model Context Protocol server over
// stdio. It exposes a single tool named `echo` that returns its input
// unchanged, plus an `add` tool that returns the sum of two numbers.
//
// The server exists primarily to back hippo's MCP integration test -
// an external dependency-free target the test can spawn - and to
// document what a hand-written MCP server looks like. No framework,
// no external package, ~150 lines of plain JSON-RPC.
//
// Usage:
//
//	go run ./examples/mcp/echo_server
//
// speaks MCP on stdin/stdout. It writes no progress chatter to stdout
// (that would corrupt the framing); diagnostic output goes to stderr.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// protocolVersion is echoed back on initialize. MCP servers and
// clients are permissive about version matching; we report the value
// hippo uses.
const protocolVersion = "2025-06-18"

type jsonrpcMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *jsonrpcError    `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() {
	if err := serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "echo_server:", err)
		os.Exit(1)
	}
}

// serve reads newline-delimited JSON-RPC messages from in and writes
// responses to out. Notifications don't get a response; requests do.
func serve(in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	writer := bufio.NewWriter(out)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg jsonrpcMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			fmt.Fprintln(os.Stderr, "echo_server: bad json:", err)
			continue
		}
		resp, drop := dispatch(&msg)
		if drop || resp == nil {
			continue
		}
		body, err := json.Marshal(resp)
		if err != nil {
			return err
		}
		if _, err := writer.Write(body); err != nil {
			return err
		}
		if err := writer.WriteByte('\n'); err != nil {
			return err
		}
		if err := writer.Flush(); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// dispatch maps an incoming message to its response. drop=true means
// the caller should skip writing anything (notifications).
func dispatch(msg *jsonrpcMessage) (resp *jsonrpcMessage, drop bool) {
	// Notification: no ID, no response.
	if msg.ID == nil {
		return nil, true
	}
	base := &jsonrpcMessage{JSONRPC: "2.0", ID: msg.ID}

	switch msg.Method {
	case "initialize":
		base.Result = map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "echo", "version": "0.1"},
		}
		return base, false

	case "tools/list":
		base.Result = map[string]any{
			"tools": []map[string]any{
				{
					"name":        "echo",
					"description": "Return the input text unchanged.",
					"inputSchema": map[string]any{
						"type":     "object",
						"required": []string{"text"},
						"properties": map[string]any{
							"text": map[string]any{"type": "string"},
						},
						"additionalProperties": false,
					},
				},
				{
					"name":        "add",
					"description": "Return the sum of a and b.",
					"inputSchema": map[string]any{
						"type":     "object",
						"required": []string{"a", "b"},
						"properties": map[string]any{
							"a": map[string]any{"type": "number"},
							"b": map[string]any{"type": "number"},
						},
						"additionalProperties": false,
					},
				},
			},
		}
		return base, false

	case "tools/call":
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &call); err != nil {
			base.Error = &jsonrpcError{Code: -32602, Message: err.Error()}
			return base, false
		}
		switch call.Name {
		case "echo":
			var args struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(call.Arguments, &args); err != nil {
				base.Result = errorResult(err.Error())
				return base, false
			}
			base.Result = textResult(args.Text)
		case "add":
			var args struct {
				A float64 `json:"a"`
				B float64 `json:"b"`
			}
			if err := json.Unmarshal(call.Arguments, &args); err != nil {
				base.Result = errorResult(err.Error())
				return base, false
			}
			base.Result = textResult(fmt.Sprintf("%v", args.A+args.B))
		default:
			base.Result = errorResult("unknown tool: " + call.Name)
		}
		return base, false

	case "shutdown":
		base.Result = struct{}{}
		return base, false

	default:
		base.Error = &jsonrpcError{Code: -32601, Message: "method not found: " + msg.Method}
		return base, false
	}
}

func textResult(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": false,
	}
}

func errorResult(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": true,
	}
}
