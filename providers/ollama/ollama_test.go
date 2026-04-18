package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

func TestNewDefaults(t *testing.T) {
	pr, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c, ok := pr.(*provider)
	if !ok {
		t.Fatalf("unexpected type %T", pr)
	}
	if c.baseURL != defaultBaseURL {
		t.Errorf("baseURL = %q, want %q", c.baseURL, defaultBaseURL)
	}
	if c.model != defaultModel {
		t.Errorf("model = %q, want %q", c.model, defaultModel)
	}
	if c.timeout != defaultTimeout {
		t.Errorf("timeout = %v, want %v", c.timeout, defaultTimeout)
	}
	if c.httpClient == nil {
		t.Error("httpClient is nil")
	}
	if c.Privacy() != hippo.PrivacyLocalOnly {
		t.Errorf("Privacy() = %v, want PrivacyLocalOnly", c.Privacy())
	}
	if c.Name() != "ollama" {
		t.Errorf("Name() = %q, want ollama", c.Name())
	}
}

func TestEstimateCostZero(t *testing.T) {
	pr, _ := New()
	cost, err := pr.EstimateCost(hippo.Call{Prompt: strings.Repeat("x", 10000), MaxTokens: 5000})
	if err != nil {
		t.Fatalf("EstimateCost err: %v", err)
	}
	if cost != 0 {
		t.Errorf("EstimateCost = %v, want 0 for local inference", cost)
	}
}

func TestModelsFetchedFromServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{
			"models": [
				{"name": "llama3.3:70b", "model": "llama3.3:70b", "size": 40000000000},
				{"name": "qwen2.5-coder:32b", "model": "qwen2.5-coder:32b", "size": 19000000000}
			]
		}`)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithBaseURL(server.URL))
	models := pr.Models()
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	// Sorted alphabetically; llama3.3:70b < qwen2.5-coder:32b.
	if models[0].ID != "llama3.3:70b" {
		t.Errorf("models[0].ID = %q, want llama3.3:70b", models[0].ID)
	}
	// ContextTokens should come from pricing.yaml for registered
	// models — 131072 for llama3.3:70b.
	if models[0].ContextTokens != 131072 {
		t.Errorf("llama3.3:70b ContextTokens = %d, want 131072", models[0].ContextTokens)
	}
}

func TestModelsUnreachableServerReturnsEmpty(t *testing.T) {
	// Point at a closed port — Dial fails fast.
	pr, _ := New(WithBaseURL("http://127.0.0.1:1"))
	models := pr.Models()
	if len(models) != 0 {
		t.Errorf("got %d models from unreachable server, want 0", len(models))
	}
}

func TestModelsCached(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"models":[{"name":"llama3.3:70b","model":"llama3.3:70b"}]}`)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithBaseURL(server.URL))
	_ = pr.Models()
	_ = pr.Models()
	_ = pr.Models()
	if got := calls.Load(); got != 1 {
		t.Errorf("server saw %d /api/tags calls, want 1 (two should have been cached)", got)
	}
}

func TestModelsUnknownIDGetsFallbackContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		// Model id not in pricing.yaml — should fall back to 8192.
		io.WriteString(w, `{"models":[{"name":"phi4:14b","model":"phi4:14b"}]}`)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithBaseURL(server.URL))
	models := pr.Models()
	if len(models) != 1 {
		t.Fatalf("got %d, want 1", len(models))
	}
	if models[0].ContextTokens != fallbackContext {
		t.Errorf("ContextTokens = %d, want %d (fallback for unknown model)",
			models[0].ContextTokens, fallbackContext)
	}
}

// okChatBody returns a valid /api/chat non-stream response JSON with
// the given text and token counts.
func okChatBody(model, text string, in, out int) string {
	body, _ := json.Marshal(chatResponse{
		Model: model,
		Message: chatMessage{
			Role:    "assistant",
			Content: text,
		},
		Done:            true,
		DoneReason:      "stop",
		PromptEvalCount: in,
		EvalCount:       out,
	})
	return string(body)
}

func TestCallRoundtrip(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %q, want /api/chat", r.URL.Path)
		}
		if r.Header.Get("content-type") != "application/json" {
			t.Errorf("content-type = %q", r.Header.Get("content-type"))
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, okChatBody("llama3.3:70b", "Hello, world!", 12, 4))
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithBaseURL(server.URL), WithModel("llama3.3:70b"))
	resp, err := pr.Call(context.Background(), hippo.Call{Prompt: "hi", MaxTokens: 100})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Text != "Hello, world!" {
		t.Errorf("Text = %q", resp.Text)
	}
	if resp.Provider != "ollama" {
		t.Errorf("Provider = %q", resp.Provider)
	}
	if resp.Model != "llama3.3:70b" {
		t.Errorf("Model = %q", resp.Model)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 4 {
		t.Errorf("Usage = %+v, want 12/4", resp.Usage)
	}
	if resp.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want 0", resp.CostUSD)
	}

	// Verify the request body shape: stream=false, options.num_predict=100.
	var parsed chatRequest
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("request body parse: %v", err)
	}
	if parsed.Stream {
		t.Error("stream=true in non-streaming Call body")
	}
	if parsed.Options == nil || parsed.Options.NumPredict != 100 {
		t.Errorf("options = %+v, want NumPredict=100", parsed.Options)
	}
}

func TestCallIncludesSystemInMessages(t *testing.T) {
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
			{Role: "system", Content: "You are terse."},
			{Role: "user", Content: "Hi."},
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var parsed chatRequest
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if len(parsed.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (system kept in-line, not hoisted)", len(parsed.Messages))
	}
	if parsed.Messages[0].Role != "system" || parsed.Messages[0].Content != "You are terse." {
		t.Errorf("messages[0] = %+v, want system + 'You are terse.'", parsed.Messages[0])
	}
}

func TestCallHonorsKeepAlive(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, okChatBody("m", "ok", 1, 1))
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithBaseURL(server.URL), WithKeepAlive(10*time.Minute))
	_, err := pr.Call(context.Background(), hippo.Call{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var parsed chatRequest
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if parsed.KeepAlive != "10m0s" {
		t.Errorf("keep_alive = %q, want 10m0s", parsed.KeepAlive)
	}
}

func TestCallReturnsModelNotFoundOn404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"model 'bogus:1.0' not found, try pulling it first"}`)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithBaseURL(server.URL), WithModel("bogus:1.0"))
	_, err := pr.Call(context.Background(), hippo.Call{Prompt: "hi"})
	if !errors.Is(err, hippo.ErrModelNotFound) {
		t.Errorf("err = %v, want wrapping ErrModelNotFound", err)
	}
}

// --- streaming unit tests ---

func TestStreamEmitsTextDeltas(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		writeNDJSON(w, chatResponse{Model: "m", Message: chatMessage{Role: "assistant", Content: "Hello"}})
		writeNDJSON(w, chatResponse{Model: "m", Message: chatMessage{Role: "assistant", Content: ", "}})
		writeNDJSON(w, chatResponse{Model: "m", Message: chatMessage{Role: "assistant", Content: "world!"}})
		writeNDJSON(w, chatResponse{Model: "m", Done: true,
			PromptEvalCount: 5, EvalCount: 3,
		})
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithBaseURL(server.URL))
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
		t.Fatal("no terminal chunk")
	}
	if terminal.Usage.InputTokens != 5 || terminal.Usage.OutputTokens != 3 {
		t.Errorf("terminal usage = %+v", terminal.Usage)
	}
	if terminal.Provider != "ollama" {
		t.Errorf("terminal Provider = %q", terminal.Provider)
	}
	if terminal.CostUSD != 0 {
		t.Errorf("CostUSD = %v, want 0", terminal.CostUSD)
	}
}

func TestStreamEmitsToolCallsOnTerminal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/x-ndjson")
		// Ollama emits tool calls on the terminal chunk only.
		writeNDJSON(w, chatResponse{Model: "m", Done: true,
			Message: chatMessage{
				Role: "assistant",
				ToolCalls: []chatToolCall{
					{Function: struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					}{Name: "get_weather", Arguments: json.RawMessage(`{"location":"SF"}`)}},
				},
			},
			PromptEvalCount: 7, EvalCount: 2,
		})
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithBaseURL(server.URL))
	ch, err := pr.Stream(context.Background(), hippo.Call{Prompt: "weather?"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := drainStream(t, ch)

	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want at least 2 (tool_call + usage)", len(chunks))
	}
	// Tool call must precede the terminal usage chunk.
	if chunks[0].Type != hippo.StreamChunkToolCall {
		t.Errorf("first chunk type = %q, want %q", chunks[0].Type, hippo.StreamChunkToolCall)
	}
	if chunks[0].ToolCall == nil {
		t.Fatal("tool call chunk has nil ToolCall")
	}
	if chunks[0].ToolCall.Name != "get_weather" {
		t.Errorf("ToolCall.Name = %q", chunks[0].ToolCall.Name)
	}
	var args map[string]any
	if err := json.Unmarshal(chunks[0].ToolCall.Arguments, &args); err != nil {
		t.Fatalf("ToolCall.Arguments not valid JSON: %v", err)
	}
	if args["location"] != "SF" {
		t.Errorf("args[location] = %v, want SF", args["location"])
	}
	last := chunks[len(chunks)-1]
	if last.Type != hippo.StreamChunkUsage {
		t.Errorf("last chunk type = %q, want %q", last.Type, hippo.StreamChunkUsage)
	}
}

func TestStreamHandlesUnreachableServer(t *testing.T) {
	pr, _ := New(WithBaseURL("http://127.0.0.1:1"))
	ch, err := pr.Stream(context.Background(), hippo.Call{Prompt: "hi"})
	if ch != nil {
		t.Error("channel returned alongside unreachable-server error")
	}
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestStreamRespectsContextCancel(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		writeNDJSON(w, chatResponse{Model: "m", Message: chatMessage{Content: "first"}})
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithBaseURL(server.URL))
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

func TestStreamHandlesIncompleteNDJSON(t *testing.T) {
	// Server writes a valid first object, then a partial JSON and
	// closes the connection. The decoder should surface an error
	// rather than a silent success.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		writeNDJSON(w, chatResponse{Model: "m", Message: chatMessage{Content: "hi"}})
		io.WriteString(w, `{"model":"m","message":{"role":"assist`) // truncated
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithBaseURL(server.URL))
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
		t.Errorf("terminal type = %q, want %q (incomplete NDJSON should surface an error)",
			last.Type, hippo.StreamChunkError)
	}
}

// --- helpers ---

func writeNDJSON(w io.Writer, obj chatResponse) {
	b, _ := json.Marshal(obj)
	w.Write(b)
	io.WriteString(w, "\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func drainStream(t *testing.T, ch <-chan hippo.StreamChunk) []hippo.StreamChunk {
	t.Helper()
	var out []hippo.StreamChunk
	deadline := time.After(3 * time.Second)
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, c)
		case <-deadline:
			t.Fatalf("drainStream timed out after %d chunks", len(out))
			return out
		}
	}
}
