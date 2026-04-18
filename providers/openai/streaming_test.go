package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

func writeSSE(w io.Writer, name, data string) {
	io.WriteString(w, "event: "+name+"\n")
	io.WriteString(w, "data: "+data+"\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func streamHandler(events [][2]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, e := range events {
			writeSSE(w, e[0], e[1])
		}
	}
}

func drainStream(t *testing.T, ch <-chan hippo.StreamChunk) []hippo.StreamChunk {
	t.Helper()
	var chunks []hippo.StreamChunk
	deadline := time.After(3 * time.Second)
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return chunks
			}
			chunks = append(chunks, c)
		case <-deadline:
			t.Fatalf("drainStream timed out after %d chunks", len(chunks))
			return chunks
		}
	}
}

// completedEvent returns a response.completed payload with the given
// usage. Tests that don't care about usage numbers use the zero call:
// completedEvent("gpt-5-nano", 1, 1, 0).
func completedEvent(model string, in, out, cached int) string {
	type tokDetails struct {
		Cached int `json:"cached_tokens"`
	}
	type usage struct {
		InputTokens        int        `json:"input_tokens"`
		OutputTokens       int        `json:"output_tokens"`
		InputTokensDetails tokDetails `json:"input_tokens_details"`
	}
	type resp struct {
		Model string `json:"model"`
		Usage usage  `json:"usage"`
	}
	type envelope struct {
		Response resp `json:"response"`
	}
	b, _ := json.Marshal(envelope{Response: resp{
		Model: model,
		Usage: usage{InputTokens: in, OutputTokens: out,
			InputTokensDetails: tokDetails{Cached: cached}},
	}})
	return string(b)
}

func TestStreamEmitsOutputTextDeltas(t *testing.T) {
	events := [][2]string{
		{"response.created", `{"response":{"model":"gpt-5-nano"}}`},
		{"response.output_item.added", `{"output_index":0,"item":{"type":"message","id":"msg_1"}}`},
		{"response.content_part.added", `{"output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}`},
		{"response.output_text.delta", `{"output_index":0,"content_index":0,"delta":"Hello"}`},
		{"response.output_text.delta", `{"output_index":0,"content_index":0,"delta":", "}`},
		{"response.output_text.delta", `{"output_index":0,"content_index":0,"delta":"world!"}`},
		{"response.output_item.done", `{"output_index":0,"item":{"type":"message"}}`},
		{"response.completed", completedEvent("gpt-5-nano", 10, 3, 0)},
	}
	server := httptest.NewServer(streamHandler(events))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL), WithModel("gpt-5-nano"))
	ch, err := pr.Stream(context.Background(), hippo.Call{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := drainStream(t, ch)

	var text strings.Builder
	var terminal *hippo.StreamChunk
	for i := range chunks {
		c := chunks[i]
		switch c.Type {
		case hippo.StreamChunkText:
			text.WriteString(c.Delta)
		case hippo.StreamChunkUsage:
			terminal = &chunks[i]
		case hippo.StreamChunkError:
			t.Fatalf("unexpected error: %v", c.Error)
		}
	}
	if text.String() != "Hello, world!" {
		t.Errorf("assembled text = %q, want %q", text.String(), "Hello, world!")
	}
	if terminal == nil {
		t.Fatal("no terminal usage chunk")
	}
	if terminal.Provider != "openai" || terminal.Model != "gpt-5-nano" {
		t.Errorf("terminal provider/model = %s/%s", terminal.Provider, terminal.Model)
	}
	if terminal.Usage.InputTokens != 10 || terminal.Usage.OutputTokens != 3 {
		t.Errorf("terminal usage = %+v, want input=10 output=3", terminal.Usage)
	}
	if terminal.CostUSD <= 0 {
		t.Errorf("cost = %v, want > 0", terminal.CostUSD)
	}
}

func TestStreamEmitsReasoningDeltas(t *testing.T) {
	events := [][2]string{
		{"response.created", `{"response":{"model":"gpt-5"}}`},
		{"response.output_item.added", `{"output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`},
		{"response.reasoning_summary_text.delta", `{"output_index":0,"delta":"Considering"}`},
		{"response.reasoning_summary_text.delta", `{"output_index":0,"delta":" options..."}`},
		{"response.output_item.done", `{"output_index":0,"item":{"type":"reasoning"}}`},
		{"response.output_item.added", `{"output_index":1,"item":{"type":"message","id":"msg_2"}}`},
		{"response.output_text.delta", `{"output_index":1,"delta":"Done."}`},
		{"response.output_item.done", `{"output_index":1,"item":{"type":"message"}}`},
		{"response.completed", completedEvent("gpt-5", 15, 5, 0)},
	}
	server := httptest.NewServer(streamHandler(events))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL), WithModel("gpt-5"))
	ch, err := pr.Stream(context.Background(), hippo.Call{Prompt: "solve"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := drainStream(t, ch)

	var thinking, text strings.Builder
	for _, c := range chunks {
		switch c.Type {
		case hippo.StreamChunkThinking:
			thinking.WriteString(c.Delta)
		case hippo.StreamChunkText:
			text.WriteString(c.Delta)
		}
	}
	if thinking.String() != "Considering options..." {
		t.Errorf("thinking = %q, want %q", thinking.String(), "Considering options...")
	}
	if text.String() != "Done." {
		t.Errorf("text = %q, want %q", text.String(), "Done.")
	}
}

func TestStreamReassemblesToolCall(t *testing.T) {
	events := [][2]string{
		{"response.created", `{"response":{"model":"gpt-5-nano"}}`},
		{"response.output_item.added", `{"output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"get_weather"}}`},
		{"response.function_call_arguments.delta", `{"output_index":0,"delta":"{\"loca"}`},
		{"response.function_call_arguments.delta", `{"output_index":0,"delta":"tion\":"}`},
		{"response.function_call_arguments.delta", `{"output_index":0,"delta":"\"SF\"}"}`},
		{"response.output_item.done", `{"output_index":0,"item":{"type":"function_call"}}`},
		{"response.completed", completedEvent("gpt-5-nano", 10, 5, 0)},
	}
	server := httptest.NewServer(streamHandler(events))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	ch, err := pr.Stream(context.Background(), hippo.Call{Prompt: "weather?"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := drainStream(t, ch)

	var tools []*hippo.ToolCall
	for _, c := range chunks {
		if c.Type == hippo.StreamChunkToolCall {
			tools = append(tools, c.ToolCall)
		}
	}
	if len(tools) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(tools))
	}
	tc := tools[0]
	if tc.ID != "call_abc" {
		t.Errorf("ID = %q, want call_abc (should prefer call_id over item id)", tc.ID)
	}
	if tc.Name != "get_weather" {
		t.Errorf("Name = %q, want get_weather", tc.Name)
	}
	var args map[string]any
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("Arguments is not valid JSON: %v (raw: %s)", err, tc.Arguments)
	}
	if args["location"] != "SF" {
		t.Errorf("args[location] = %v, want SF", args["location"])
	}
}

func TestStreamHandlesMultipleToolCalls(t *testing.T) {
	events := [][2]string{
		{"response.created", `{"response":{"model":"gpt-5-nano"}}`},
		{"response.output_item.added", `{"output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"toolA"}}`},
		{"response.function_call_arguments.delta", `{"output_index":0,"delta":"{\"x\":1}"}`},
		{"response.output_item.done", `{"output_index":0,"item":{"type":"function_call"}}`},
		{"response.output_item.added", `{"output_index":1,"item":{"type":"function_call","id":"fc_2","call_id":"call_2","name":"toolB"}}`},
		{"response.function_call_arguments.delta", `{"output_index":1,"delta":"{\"y\":2}"}`},
		{"response.output_item.done", `{"output_index":1,"item":{"type":"function_call"}}`},
		{"response.completed", completedEvent("gpt-5-nano", 8, 10, 0)},
	}
	server := httptest.NewServer(streamHandler(events))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	ch, err := pr.Stream(context.Background(), hippo.Call{Prompt: "multi"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := drainStream(t, ch)

	var names []string
	for _, c := range chunks {
		if c.Type == hippo.StreamChunkToolCall {
			names = append(names, c.ToolCall.Name)
		}
	}
	want := []string{"toolA", "toolB"}
	if len(names) != len(want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
	for i := range names {
		if names[i] != want[i] {
			t.Errorf("tool #%d = %q, want %q", i, names[i], want[i])
		}
	}
}

func TestStreamHandlesResponseIncomplete(t *testing.T) {
	// response.incomplete is a normal stop (hit max_output_tokens,
	// not an error). The adapter must emit a StreamChunkUsage, not a
	// StreamChunkError.
	events := [][2]string{
		{"response.created", `{"response":{"model":"gpt-5-nano"}}`},
		{"response.output_item.added", `{"output_index":0,"item":{"type":"message","id":"msg_inc"}}`},
		{"response.output_text.delta", `{"output_index":0,"delta":"partial"}`},
		{"response.output_item.done", `{"output_index":0,"item":{"type":"message"}}`},
		{"response.incomplete", completedEvent("gpt-5-nano", 3, 1, 0)},
	}
	server := httptest.NewServer(streamHandler(events))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	ch, err := pr.Stream(context.Background(), hippo.Call{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := drainStream(t, ch)

	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	last := chunks[len(chunks)-1]
	if last.Type != hippo.StreamChunkUsage {
		t.Errorf("terminal type = %q, want %q (incomplete is not an error)",
			last.Type, hippo.StreamChunkUsage)
	}
}

func TestStreamHandlesResponseFailed(t *testing.T) {
	events := [][2]string{
		{"response.created", `{"response":{"model":"gpt-5-nano"}}`},
		{"response.failed", `{"response":{"error":{"message":"the model overheated","code":"server_error"}}}`},
	}
	server := httptest.NewServer(streamHandler(events))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	ch, err := pr.Stream(context.Background(), hippo.Call{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := drainStream(t, ch)

	if len(chunks) == 0 {
		t.Fatal("no chunks")
	}
	last := chunks[len(chunks)-1]
	if last.Type != hippo.StreamChunkError {
		t.Errorf("terminal type = %q, want %q", last.Type, hippo.StreamChunkError)
	}
	if last.Error == nil || !strings.Contains(last.Error.Error(), "overheated") {
		t.Errorf("error = %v, want to include upstream message", last.Error)
	}
}

func TestStreamRespectsContextCancel(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSE(w, "response.created", `{"response":{"model":"gpt-5-nano"}}`)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := pr.Stream(ctx, hippo.Call{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	time.AfterFunc(50*time.Millisecond, cancel)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("channel did not close within 2s of ctx cancel")
		}
	}
}

func TestStreamHandshakeAuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("bad"), WithBaseURL(server.URL))
	ch, err := pr.Stream(context.Background(), hippo.Call{Prompt: "hi"})
	if ch != nil {
		t.Error("Stream returned non-nil channel alongside error")
	}
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
}
