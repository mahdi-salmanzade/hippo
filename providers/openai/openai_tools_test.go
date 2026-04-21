package openai

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

func TestCallSendsToolsAsFunctions(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, minimalSuccessBody("gpt-5-nano"))
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL), WithModel("gpt-5-nano"))
	_, err := pr.Call(context.Background(), hippo.Call{
		Prompt: "hi",
		Tools: []hippo.Tool{
			stubTool{name: "get_weather", description: "weather",
				schema: `{"type":"object","properties":{"city":{"type":"string"}}}`},
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var parsed responseRequest
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("body parse: %v", err)
	}
	if len(parsed.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(parsed.Tools))
	}
	if parsed.Tools[0].Type != "function" {
		t.Errorf("tools[0].Type = %q, want function", parsed.Tools[0].Type)
	}
	if parsed.Tools[0].Name != "get_weather" {
		t.Errorf("tools[0].Name = %q", parsed.Tools[0].Name)
	}
	if !parsed.Tools[0].Strict {
		t.Error("tools[0].Strict = false, want true (default)")
	}
}

func TestCallStrictModeDefault(t *testing.T) {
	// Pinning the strict:true default as load-bearing - callers
	// don't pass it explicitly; flipping it silently changes schema
	// enforcement behaviour. Keep this asserted in isolation.
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, minimalSuccessBody("gpt-5-nano"))
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	_, err := pr.Call(context.Background(), hippo.Call{
		Prompt: "hi",
		Tools:  []hippo.Tool{stubTool{name: "x", schema: `{"type":"object"}`}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// Raw check for strict:true in the serialised body so a future
	// rename of the Go field can't mask the wire behaviour.
	if !bytesContains(gotBody, `"strict":true`) {
		t.Errorf("request body missing strict:true; body=%s", string(gotBody))
	}
}

func TestCallParsesFunctionCallOutput(t *testing.T) {
	const body = `{
		"id":"resp_1","object":"response","model":"gpt-5-nano","status":"completed",
		"output":[
			{"type":"function_call","id":"fc_x","call_id":"call_abc","name":"get_weather","arguments":"{\"city\":\"SF\"}"}
		],
		"usage":{"input_tokens":10,"output_tokens":5,
			"input_tokens_details":{"cached_tokens":0},
			"output_tokens_details":{"reasoning_tokens":0}}
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	resp, err := pr.Call(context.Background(), hippo.Call{
		Prompt: "weather?",
		Tools:  []hippo.Tool{stubTool{name: "get_weather", schema: `{"type":"object"}`}},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_abc" {
		t.Errorf("ToolCall.ID = %q, want call_abc (should prefer call_id over item id)", tc.ID)
	}
	if tc.Name != "get_weather" {
		t.Errorf("ToolCall.Name = %q", tc.Name)
	}
	var args map[string]any
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("Arguments not valid JSON: %v", err)
	}
	if args["city"] != "SF" {
		t.Errorf("args[city] = %v, want SF", args["city"])
	}
}

func TestCallFollowupRequestIncludesFunctionCallOutput(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, minimalSuccessBody("gpt-5-nano"))
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	_, err := pr.Call(context.Background(), hippo.Call{
		Messages: []hippo.Message{
			{Role: "user", Content: "weather?"},
			{Role: "assistant", ToolCalls: []hippo.ToolCall{
				{ID: "call_abc", Name: "get_weather",
					Arguments: json.RawMessage(`{"city":"SF"}`)},
			}},
			{Role: "tool", ToolCallID: "call_abc", Content: "sunny"},
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var parsed responseRequest
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("parse body: %v", err)
	}

	var items []json.RawMessage
	if err := json.Unmarshal(parsed.Input, &items); err != nil {
		t.Fatalf("input is not an array: %v; raw=%s", err, parsed.Input)
	}
	// Items: user message, function_call, function_call_output.
	if len(items) != 3 {
		t.Fatalf("input items = %d, want 3; raw=%s", len(items), parsed.Input)
	}

	// Item 1: function_call with call_id + arguments-as-string.
	var fc inputFunctionCall
	if err := json.Unmarshal(items[1], &fc); err != nil {
		t.Fatalf("item[1] parse: %v", err)
	}
	if fc.Type != "function_call" || fc.CallID != "call_abc" || fc.Name != "get_weather" {
		t.Errorf("item[1] = %+v", fc)
	}
	if fc.Arguments != `{"city":"SF"}` {
		t.Errorf("item[1].arguments = %q, want JSON-encoded string", fc.Arguments)
	}

	// Item 2: function_call_output correlated by call_id.
	var out inputFunctionCallOutput
	if err := json.Unmarshal(items[2], &out); err != nil {
		t.Fatalf("item[2] parse: %v", err)
	}
	if out.Type != "function_call_output" || out.CallID != "call_abc" || out.Output != "sunny" {
		t.Errorf("item[2] = %+v", out)
	}
}

func bytesContains(haystack []byte, needle string) bool {
	return string(haystack) != "" && len(needle) > 0 && indexOf(haystack, needle) >= 0
}

func indexOf(haystack []byte, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == needle {
			return i
		}
	}
	return -1
}
