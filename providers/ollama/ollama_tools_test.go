package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mahdi-salmanzade/hippo"
)

type stubTool struct {
	name, description, schema string
}

func (s stubTool) Name() string            { return s.name }
func (s stubTool) Description() string     { return s.description }
func (s stubTool) Schema() json.RawMessage { return json.RawMessage(s.schema) }
func (s stubTool) Execute(context.Context, json.RawMessage) (hippo.ToolResult, error) {
	return hippo.ToolResult{}, nil
}

func TestCallSendsToolsInRequest(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, okChatBody("llama3.1:8b", "ok", 1, 1))
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithBaseURL(server.URL), WithModel("llama3.1:8b"))
	_, err := pr.Call(context.Background(), hippo.Call{
		Prompt: "hi",
		Tools: []hippo.Tool{
			stubTool{name: "weather", description: "fetch",
				schema: `{"type":"object","properties":{"city":{"type":"string"}}}`},
			stubTool{name: "calc", description: "math", schema: ""},
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var parsed chatRequest
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if len(parsed.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(parsed.Tools))
	}
	if parsed.Tools[0].Type != "function" {
		t.Errorf("tools[0].Type = %q, want function", parsed.Tools[0].Type)
	}
	if parsed.Tools[0].Function.Name != "weather" {
		t.Errorf("tools[0].Function.Name = %q", parsed.Tools[0].Function.Name)
	}
	// Empty schema defaults to {"type":"object"}.
	var defaulted map[string]any
	_ = json.Unmarshal(parsed.Tools[1].Function.Parameters, &defaulted)
	if defaulted["type"] != "object" {
		t.Errorf("default parameters = %v, want type:object", defaulted)
	}
}

func TestCallParsesTerminalToolCalls(t *testing.T) {
	// Non-streaming: /api/chat returns a single JSON object with
	// Done:true and tool_calls populated on the Message.
	body, _ := json.Marshal(chatResponse{
		Model: "llama3.1:8b",
		Message: chatMessage{
			Role: "assistant",
			ToolCalls: []chatToolCall{
				{Function: struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				}{Name: "weather", Arguments: json.RawMessage(`{"city":"SF"}`)}},
			},
		},
		Done:            true,
		DoneReason:      "tool_calls",
		PromptEvalCount: 5,
		EvalCount:       3,
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write(body)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithBaseURL(server.URL))
	resp, err := pr.Call(context.Background(), hippo.Call{Prompt: "weather?"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.Name != "weather" {
		t.Errorf("Name = %q", tc.Name)
	}
	// Ollama doesn't assign IDs; the adapter synthesises tool_<i>.
	if tc.ID != "tool_0" {
		t.Errorf("ID = %q, want tool_0 (synthesised)", tc.ID)
	}
}

func TestCallFollowupRequestIncludesRoleToolMessages(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, okChatBody("m", "ok", 1, 1))
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithBaseURL(server.URL))
	_, err := pr.Call(context.Background(), hippo.Call{
		Messages: []hippo.Message{
			{Role: "user", Content: "weather?"},
			{Role: "assistant", ToolCalls: []hippo.ToolCall{
				{ID: "tool_0", Name: "weather",
					Arguments: json.RawMessage(`{"city":"SF"}`)},
			}},
			{Role: "tool", ToolCallID: "tool_0", Content: "sunny"},
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var parsed chatRequest
	_ = json.Unmarshal(gotBody, &parsed)
	if len(parsed.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(parsed.Messages))
	}

	// Assistant message preserves tool_calls.
	asst := parsed.Messages[1]
	if asst.Role != "assistant" {
		t.Errorf("msg[1].Role = %q", asst.Role)
	}
	if len(asst.ToolCalls) != 1 || asst.ToolCalls[0].Function.Name != "weather" {
		t.Errorf("msg[1].ToolCalls = %+v", asst.ToolCalls)
	}

	// Tool result message has role:"tool" + tool_call_id + content.
	tool := parsed.Messages[2]
	if tool.Role != "tool" {
		t.Errorf("msg[2].Role = %q, want tool", tool.Role)
	}
	if tool.ToolCallID != "tool_0" {
		t.Errorf("msg[2].ToolCallID = %q", tool.ToolCallID)
	}
	if tool.Content != "sunny" {
		t.Errorf("msg[2].Content = %q", tool.Content)
	}
}
