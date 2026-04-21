package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mahdi-salmanzade/hippo"
)

// stubTool is a minimal hippo.Tool for request-shape tests. The
// Execute method is never called here - these tests only exercise
// the provider's translation of hippo.Tool into Anthropic's
// native tool schema plus the response-side parsing.
type stubTool struct {
	name, description, schema string
}

func (s stubTool) Name() string                 { return s.name }
func (s stubTool) Description() string          { return s.description }
func (s stubTool) Schema() json.RawMessage      { return json.RawMessage(s.schema) }
func (s stubTool) Execute(context.Context, json.RawMessage) (hippo.ToolResult, error) {
	return hippo.ToolResult{}, nil
}

func TestCallSendsToolsInRequest(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{
			"id":"msg_1","type":"message","role":"assistant",
			"content":[{"type":"text","text":"ok"}],
			"model":"claude-haiku-4-5",
			"usage":{"input_tokens":1,"output_tokens":1}
		}`)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	_, err := pr.Call(context.Background(), hippo.Call{
		Prompt: "hi",
		Tools: []hippo.Tool{
			stubTool{name: "get_weather", description: "fetch weather",
				schema: `{"type":"object","properties":{"city":{"type":"string"}}}`},
			stubTool{name: "calculator", description: "do math", schema: ""},
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var parsed messagesRequest
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("request body parse: %v", err)
	}
	if len(parsed.Tools) != 2 {
		t.Fatalf("tools in body = %d, want 2", len(parsed.Tools))
	}
	if parsed.Tools[0].Name != "get_weather" {
		t.Errorf("tools[0].Name = %q, want get_weather", parsed.Tools[0].Name)
	}
	if parsed.Tools[0].Description != "fetch weather" {
		t.Errorf("tools[0].Description = %q", parsed.Tools[0].Description)
	}
	// Verify input_schema is passed through, not re-serialised.
	var schema map[string]any
	if err := json.Unmarshal(parsed.Tools[0].InputSchema, &schema); err != nil {
		t.Fatalf("input_schema parse: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("input_schema.type = %v, want object", schema["type"])
	}
	// Empty schema from the tool is defaulted to {"type":"object"}.
	var defaulted map[string]any
	_ = json.Unmarshal(parsed.Tools[1].InputSchema, &defaulted)
	if defaulted["type"] != "object" {
		t.Errorf("default input_schema = %v, want {type:object}", defaulted)
	}
}

func TestCallParsesToolUseContentBlock(t *testing.T) {
	const body = `{
		"id":"msg_1","type":"message","role":"assistant",
		"content":[
			{"type":"text","text":"Let me check."},
			{"type":"tool_use","id":"toolu_abc","name":"get_weather","input":{"city":"SF"}}
		],
		"model":"claude-haiku-4-5","stop_reason":"tool_use",
		"usage":{"input_tokens":10,"output_tokens":20}
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	resp, err := pr.Call(context.Background(), hippo.Call{Prompt: "weather?"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Text != "Let me check." {
		t.Errorf("Text = %q, want 'Let me check.'", resp.Text)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "toolu_abc" || tc.Name != "get_weather" {
		t.Errorf("ToolCall = %+v, want ID=toolu_abc Name=get_weather", tc)
	}
	var args map[string]any
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("Arguments parse: %v", err)
	}
	if args["city"] != "SF" {
		t.Errorf("Arguments[city] = %v, want SF", args["city"])
	}
}

func TestCallFollowupRequestIncludesToolResult(t *testing.T) {
	// Simulate a second hippo Call that arrives after the model
	// invoked get_weather on turn 1 - the messages carry an
	// assistant tool_use echo plus a role:"tool" result. Verify the
	// wire body has the right content-block shape for both.
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{
			"id":"msg_2","type":"message","role":"assistant",
			"content":[{"type":"text","text":"It is sunny."}],
			"model":"claude-haiku-4-5",
			"usage":{"input_tokens":5,"output_tokens":4}
		}`)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	_, err := pr.Call(context.Background(), hippo.Call{
		Messages: []hippo.Message{
			{Role: "user", Content: "weather in SF?"},
			{Role: "assistant", Content: "", ToolCalls: []hippo.ToolCall{
				{ID: "toolu_abc", Name: "get_weather",
					Arguments: json.RawMessage(`{"city":"SF"}`)},
			}},
			{Role: "tool", ToolCallID: "toolu_abc", Content: "sunny, 72F"},
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var parsed messagesRequest
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if len(parsed.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (user, assistant[tool_use], user[tool_result])", len(parsed.Messages))
	}

	// message 0: bare-string user turn.
	var m0 string
	if err := json.Unmarshal(parsed.Messages[0].Content, &m0); err != nil {
		t.Fatalf("msg[0] content (expected bare string): %v", err)
	}
	if m0 != "weather in SF?" {
		t.Errorf("msg[0] = %q", m0)
	}

	// message 1: assistant with a tool_use block.
	var m1 []outBlock
	if err := json.Unmarshal(parsed.Messages[1].Content, &m1); err != nil {
		t.Fatalf("msg[1] content (expected block array): %v", err)
	}
	if parsed.Messages[1].Role != "assistant" {
		t.Errorf("msg[1] role = %q, want assistant", parsed.Messages[1].Role)
	}
	if len(m1) != 1 || m1[0].Type != "tool_use" || m1[0].ID != "toolu_abc" ||
		m1[0].Name != "get_weather" {
		t.Errorf("msg[1] blocks = %+v, want one tool_use block with id+name", m1)
	}

	// message 2: user with a tool_result block.
	if parsed.Messages[2].Role != "user" {
		t.Errorf("msg[2] role = %q, want user (tool results are user turns)", parsed.Messages[2].Role)
	}
	var m2 []outBlock
	if err := json.Unmarshal(parsed.Messages[2].Content, &m2); err != nil {
		t.Fatalf("msg[2] content: %v", err)
	}
	if len(m2) != 1 || m2[0].Type != "tool_result" {
		t.Fatalf("msg[2] blocks = %+v, want one tool_result", m2)
	}
	if m2[0].ToolUseID != "toolu_abc" || m2[0].Content != "sunny, 72F" {
		t.Errorf("tool_result = %+v, want tool_use_id=toolu_abc content='sunny, 72F'", m2[0])
	}
}

func TestCallFoldsParallelToolResults(t *testing.T) {
	// Two role:"tool" messages in sequence must fold into a single
	// user turn with multiple tool_result blocks - that's how
	// Anthropic expects parallel-call results to arrive.
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{
			"id":"msg","type":"message","role":"assistant",
			"content":[{"type":"text","text":"done"}],
			"model":"m","usage":{"input_tokens":1,"output_tokens":1}
		}`)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	_, err := pr.Call(context.Background(), hippo.Call{
		Messages: []hippo.Message{
			{Role: "user", Content: "two things please"},
			{Role: "assistant", ToolCalls: []hippo.ToolCall{
				{ID: "toolu_1", Name: "alpha", Arguments: json.RawMessage(`{}`)},
				{ID: "toolu_2", Name: "beta", Arguments: json.RawMessage(`{}`)},
			}},
			{Role: "tool", ToolCallID: "toolu_1", Content: "A"},
			{Role: "tool", ToolCallID: "toolu_2", Content: "B"},
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var parsed messagesRequest
	_ = json.Unmarshal(gotBody, &parsed)
	if len(parsed.Messages) != 3 {
		t.Fatalf("messages = %d, want 3 (parallel tool results fold into one user turn)", len(parsed.Messages))
	}
	var blocks []outBlock
	_ = json.Unmarshal(parsed.Messages[2].Content, &blocks)
	if len(blocks) != 2 {
		t.Errorf("tool_result blocks = %d, want 2", len(blocks))
	}
}
