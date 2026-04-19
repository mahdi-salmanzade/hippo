package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/mahdi-salmanzade/hippo"
)

// mcpTool wraps one MCP server tool as a hippo.Tool. Execute sends
// tools/call over the live transport and flattens the response into
// a hippo.ToolResult.
//
// Lifecycle: the tool holds a pointer to its owning Client so the
// live transport is resolved at call time rather than baked in.
// Reconnects therefore transparently apply to all issued tools.
type mcpTool struct {
	name        string
	remoteName  string
	description string
	schema      json.RawMessage
	client      *Client
}

func (t *mcpTool) Name() string        { return t.name }
func (t *mcpTool) Description() string { return t.description }
func (t *mcpTool) Schema() json.RawMessage {
	if len(t.schema) == 0 {
		// hippo.Tool callers assume a JSON Schema object; an empty
		// body would fail provider validation. Return the minimal
		// valid object schema as a safe fallback.
		return json.RawMessage(`{"type":"object"}`)
	}
	return t.schema
}

// Execute sends tools/call and maps the response to a hippo.ToolResult.
// Transport or protocol errors come back as IsError:true results so
// the LLM sees the failure and can recover, matching hippo.Tool's
// "expected failures are IsError, unexpected panics are error" contract.
func (t *mcpTool) Execute(ctx context.Context, args json.RawMessage) (hippo.ToolResult, error) {
	tr := t.client.currentTransport()
	if tr == nil || !t.client.Connected() {
		return hippo.ToolResult{
			Content: fmt.Sprintf("mcp server %q unreachable", t.client.Name()),
			IsError: true,
		}, nil
	}

	params := toolsCallParams{Name: t.remoteName, Arguments: args}
	req, err := newRequest(t.client.nextID.Add(1), "tools/call", params)
	if err != nil {
		return hippo.ToolResult{
			Content: fmt.Sprintf("mcp marshal error: %v", err),
			IsError: true,
		}, nil
	}

	resp, err := tr.Send(ctx, req)
	if err != nil {
		return hippo.ToolResult{
			Content: fmt.Sprintf("mcp transport error: %v", err),
			IsError: true,
		}, nil
	}
	if resp.Error != nil {
		return hippo.ToolResult{
			Content: fmt.Sprintf("mcp server error: %s", resp.Error.Message),
			IsError: true,
		}, nil
	}

	var result toolsCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return hippo.ToolResult{
			Content: fmt.Sprintf("mcp invalid response: %v", err),
			IsError: true,
		}, nil
	}

	var sb strings.Builder
	for _, c := range result.Content {
		switch c.Type {
		case "text":
			sb.WriteString(c.Text)
		default:
			// Image / resource blocks are rendered as a hint so the
			// LLM knows non-text content arrived without us pushing
			// binary noise onto the transcript.
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(fmt.Sprintf("[non-text content: %s]", c.Type))
		}
	}

	return hippo.ToolResult{
		Content: sb.String(),
		IsError: result.IsError,
	}, nil
}

// nameRE mirrors hippo's core tool name validation. Duplicated here
// to avoid a circular import back into the hippo root package.
var nameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,63}$`)

func isValidToolName(name string) bool { return nameRE.MatchString(name) }
