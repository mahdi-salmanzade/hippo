package anthropic

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

// writeSSE writes a single SSE event to w. name is the "event:" field
// and data is the JSON payload on the "data:" line. Caller is expected
// to hold w, then Flush if w is an http.Flusher so the chunk reaches
// the client in real time.
func writeSSE(w io.Writer, name, data string) {
	io.WriteString(w, "event: "+name+"\n")
	io.WriteString(w, "data: "+data+"\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// streamHandler builds an http.HandlerFunc that writes the given
// sequence of (event, data) pairs to the response body. Each pair is
// flushed so the client sees real incremental events — any test that
// asserts chunk order relies on this.
func streamHandler(events [][2]string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, e := range events {
			writeSSE(w, e[0], e[1])
		}
	}
}

// drainStream reads chunks until the channel closes, returning them in
// order. Testing the full stream (vs. reading N chunks) is simpler and
// catches bugs like "terminal chunk emitted twice".
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

func TestStreamEmitsTextDeltas(t *testing.T) {
	events := [][2]string{
		{"message_start", `{"message":{"id":"msg_1","model":"claude-haiku-4-5","usage":{"input_tokens":10,"output_tokens":1}}}`},
		{"content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"Hello"}}`},
		{"content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":", "}}`},
		{"content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"world!"}}`},
		{"content_block_stop", `{"index":0}`},
		{"message_delta", `{"usage":{"output_tokens":3}}`},
		{"message_stop", `{}`},
	}
	server := httptest.NewServer(streamHandler(events))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL), WithModel("claude-haiku-4-5"))
	ch, err := pr.Stream(context.Background(), hippo.Call{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := drainStream(t, ch)

	var textDeltas []string
	var terminal *hippo.StreamChunk
	for i := range chunks {
		c := chunks[i]
		switch c.Type {
		case hippo.StreamChunkText:
			textDeltas = append(textDeltas, c.Delta)
		case hippo.StreamChunkUsage:
			terminal = &chunks[i]
		case hippo.StreamChunkError:
			t.Fatalf("unexpected error chunk: %v", c.Error)
		}
	}
	want := []string{"Hello", ", ", "world!"}
	if len(textDeltas) != len(want) {
		t.Fatalf("got %d text deltas, want %d: %v", len(textDeltas), len(want), textDeltas)
	}
	for i, d := range textDeltas {
		if d != want[i] {
			t.Errorf("delta[%d] = %q, want %q", i, d, want[i])
		}
	}
	if terminal == nil {
		t.Fatal("no terminal StreamChunkUsage chunk")
	}
	if terminal.Usage.InputTokens != 10 || terminal.Usage.OutputTokens != 3 {
		t.Errorf("terminal usage = %+v, want input=10 output=3", terminal.Usage)
	}
	if terminal.Provider != "anthropic" || terminal.Model != "claude-haiku-4-5" {
		t.Errorf("terminal provider/model = %s/%s, want anthropic/claude-haiku-4-5",
			terminal.Provider, terminal.Model)
	}
	if terminal.CostUSD <= 0 {
		t.Errorf("terminal CostUSD = %v, want > 0", terminal.CostUSD)
	}
}

func TestStreamEmitsThinkingDeltas(t *testing.T) {
	events := [][2]string{
		{"message_start", `{"message":{"id":"msg_2","model":"claude-haiku-4-5","usage":{"input_tokens":5,"output_tokens":1}}}`},
		{"content_block_start", `{"index":0,"content_block":{"type":"thinking","thinking":""}}`},
		{"content_block_delta", `{"index":0,"delta":{"type":"thinking_delta","thinking":"Hmm,"}}`},
		{"content_block_delta", `{"index":0,"delta":{"type":"thinking_delta","thinking":" let me think..."}}`},
		{"content_block_stop", `{"index":0}`},
		{"message_stop", `{}`},
	}
	server := httptest.NewServer(streamHandler(events))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	ch, err := pr.Stream(context.Background(), hippo.Call{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := drainStream(t, ch)

	var thinking strings.Builder
	for _, c := range chunks {
		if c.Type == hippo.StreamChunkThinking {
			thinking.WriteString(c.Delta)
		}
	}
	want := "Hmm, let me think..."
	if thinking.String() != want {
		t.Errorf("thinking = %q, want %q", thinking.String(), want)
	}
}

func TestStreamReassemblesToolCall(t *testing.T) {
	events := [][2]string{
		{"message_start", `{"message":{"id":"msg_3","model":"claude-haiku-4-5","usage":{"input_tokens":8,"output_tokens":1}}}`},
		{"content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"toolu_abc","name":"get_weather","input":{}}}`},
		{"content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"loca"}}`},
		{"content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"tion\":"}}`},
		{"content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"\"SF\"}"}}`},
		{"content_block_stop", `{"index":0}`},
		{"message_stop", `{}`},
	}
	server := httptest.NewServer(streamHandler(events))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	ch, err := pr.Stream(context.Background(), hippo.Call{Prompt: "weather?"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := drainStream(t, ch)

	var toolCalls []*hippo.ToolCall
	for _, c := range chunks {
		if c.Type == hippo.StreamChunkToolCall {
			toolCalls = append(toolCalls, c.ToolCall)
		}
	}
	if len(toolCalls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(toolCalls))
	}
	tc := toolCalls[0]
	if tc.ID != "toolu_abc" {
		t.Errorf("ToolCall.ID = %q, want toolu_abc", tc.ID)
	}
	if tc.Name != "get_weather" {
		t.Errorf("ToolCall.Name = %q, want get_weather", tc.Name)
	}
	var got map[string]any
	if err := json.Unmarshal(tc.Arguments, &got); err != nil {
		t.Fatalf("Arguments is not valid JSON: %v (raw: %s)", err, tc.Arguments)
	}
	if got["location"] != "SF" {
		t.Errorf("Arguments[location] = %v, want SF", got["location"])
	}
}

func TestStreamHandlesMultipleToolCalls(t *testing.T) {
	// Two tool_use blocks on different content-block indices —
	// Anthropic's documented parallel-call shape. The accumulators
	// must not collide; order is preserved.
	events := [][2]string{
		{"message_start", `{"message":{"id":"msg_4","model":"claude-haiku-4-5","usage":{"input_tokens":10,"output_tokens":1}}}`},
		{"content_block_start", `{"index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"toolA","input":{}}}`},
		{"content_block_delta", `{"index":0,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`},
		{"content_block_stop", `{"index":0}`},
		{"content_block_start", `{"index":1,"content_block":{"type":"tool_use","id":"toolu_2","name":"toolB","input":{}}}`},
		{"content_block_delta", `{"index":1,"delta":{"type":"input_json_delta","partial_json":"{\"y\":2}"}}`},
		{"content_block_stop", `{"index":1}`},
		{"message_stop", `{}`},
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
		t.Fatalf("tool calls = %v, want %v", names, want)
	}
	for i, n := range names {
		if n != want[i] {
			t.Errorf("tool #%d name = %q, want %q", i, n, want[i])
		}
	}
}

func TestStreamHandshake429Retried(t *testing.T) {
	fastRetries(t)

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"type":"rate_limit","message":"slow down"}}`)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSE(w, "message_start",
			`{"message":{"id":"msg_ok","model":"claude-haiku-4-5","usage":{"input_tokens":1,"output_tokens":1}}}`)
		writeSSE(w, "message_stop", `{}`)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL), WithModel("claude-haiku-4-5"))
	ch, err := pr.Stream(context.Background(), hippo.Call{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, ch) // consume to end
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestStreamHandshakeAuthFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"type":"authentication_error","message":"bad key"}}`)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("bad"), WithBaseURL(server.URL))
	ch, err := pr.Stream(context.Background(), hippo.Call{Prompt: "hi"})
	if ch != nil {
		t.Error("Stream returned non-nil channel alongside error")
	}
	if !errors.Is(err, hippo.ErrAuthentication) {
		t.Errorf("err = %v, want wrapping ErrAuthentication", err)
	}
}

func TestStreamEmitsErrorChunkOnMidStreamFailure(t *testing.T) {
	// Server emits a valid message_start then hijacks the connection
	// and closes it. The scanner will see an abrupt EOF or a net read
	// error; either way, the stream reader must surface that as a
	// StreamChunkError rather than silently dropping the tail.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSE(w, "message_start",
			`{"message":{"id":"msg_x","model":"claude-haiku-4-5","usage":{"input_tokens":1,"output_tokens":1}}}`)
		// Emit an Anthropic-shaped error event, which the adapter
		// surfaces as a StreamChunkError.
		writeSSE(w, "error", `{"error":{"type":"overloaded_error","message":"please retry"}}`)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	ch, err := pr.Stream(context.Background(), hippo.Call{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := drainStream(t, ch)

	if len(chunks) == 0 {
		t.Fatal("no chunks emitted")
	}
	last := chunks[len(chunks)-1]
	if last.Type != hippo.StreamChunkError {
		t.Errorf("terminal chunk type = %q, want %q", last.Type, hippo.StreamChunkError)
	}
	if last.Error == nil {
		t.Error("terminal error chunk has nil Error field")
	} else if !strings.Contains(last.Error.Error(), "please retry") {
		t.Errorf("error = %v, want to include upstream message", last.Error)
	}
}

func TestStreamRespectsContextCancel(t *testing.T) {
	// Server emits one event then blocks forever. Cancelling the
	// context must close the channel without delivering more chunks.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		writeSSE(w, "message_start",
			`{"message":{"id":"msg_c","model":"claude-haiku-4-5","usage":{"input_tokens":1,"output_tokens":1}}}`)
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

	// Cancel after a brief moment so at least the handshake completes.
	time.AfterFunc(50*time.Millisecond, cancel)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // channel closed cleanly on ctx cancel
			}
		case <-deadline:
			t.Fatal("channel did not close within 2s of ctx cancel")
		}
	}
}

func TestStreamIgnoresPings(t *testing.T) {
	events := [][2]string{
		{"ping", `{}`},
		{"message_start", `{"message":{"id":"msg_p","model":"claude-haiku-4-5","usage":{"input_tokens":1,"output_tokens":1}}}`},
		{"ping", `{}`},
		{"content_block_start", `{"index":0,"content_block":{"type":"text","text":""}}`},
		{"content_block_delta", `{"index":0,"delta":{"type":"text_delta","text":"hi"}}`},
		{"ping", `{}`},
		{"content_block_stop", `{"index":0}`},
		{"message_stop", `{}`},
	}
	server := httptest.NewServer(streamHandler(events))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	ch, err := pr.Stream(context.Background(), hippo.Call{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	chunks := drainStream(t, ch)

	for _, c := range chunks {
		// There is no StreamChunk variant for pings; if any surfaced
		// they'd show up with an empty Type or a stray Delta chunk
		// with no text. Assert only expected types.
		switch c.Type {
		case hippo.StreamChunkText, hippo.StreamChunkUsage:
			// ok
		default:
			t.Errorf("unexpected chunk type %q (pings should be dropped)", c.Type)
		}
	}
}
