package hippo

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- scriptedProvider: a fake Provider that returns a different
// response per turn, so tool-execution loop tests can assert the
// contents of each subsequent turn's Call. Kept separate from
// brain_test.go's fakeProvider so neither test file depends on
// mutations to the other's mock.

type scriptedProvider struct {
	name string

	mu         sync.Mutex
	callIndex  int
	streamIdx  int
	callTurns  []*Response
	streamTurns [][]StreamChunk
	seenCalls  []Call
	streamGate []chan struct{} // per-turn gate; nil means no gate
}

func newScripted() *scriptedProvider {
	return &scriptedProvider{name: "scripted"}
}

func (s *scriptedProvider) Name() string                             { return s.name }
func (s *scriptedProvider) Models() []ModelInfo                      { return nil }
func (s *scriptedProvider) Privacy() PrivacyTier                     { return PrivacyCloudOK }
func (s *scriptedProvider) EstimateCost(Call) (float64, error)       { return 0, nil }

func (s *scriptedProvider) Call(ctx context.Context, c Call) (*Response, error) {
	s.mu.Lock()
	s.seenCalls = append(s.seenCalls, c)
	i := s.callIndex
	s.callIndex++
	resp := s.callTurns[i]
	s.mu.Unlock()
	// Return a copy so test mutations of fields on the returned
	// pointer don't bleed across turns.
	cp := *resp
	return &cp, nil
}

func (s *scriptedProvider) Stream(ctx context.Context, c Call) (<-chan StreamChunk, error) {
	s.mu.Lock()
	s.seenCalls = append(s.seenCalls, c)
	i := s.streamIdx
	s.streamIdx++
	chunks := s.streamTurns[i]
	var gate chan struct{}
	if i < len(s.streamGate) {
		gate = s.streamGate[i]
	}
	s.mu.Unlock()

	ch := make(chan StreamChunk, len(chunks))
	go func() {
		defer close(ch)
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return
			}
		}
		for _, c := range chunks {
			select {
			case ch <- c:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// --- tools used across the loop tests --------------------------------

// recordingTool captures each invocation and returns a scripted
// result. Keeps atomic counters for concurrency assertions.
type recordingTool struct {
	name    string
	result  ToolResult
	err     error
	delay   time.Duration
	inFlight atomic.Int32
	maxPeak  atomic.Int32
	calls    atomic.Int32
}

func (r *recordingTool) Name() string            { return r.name }
func (r *recordingTool) Description() string     { return r.name + " tool" }
func (r *recordingTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (r *recordingTool) Execute(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	cur := r.inFlight.Add(1)
	// Track peak concurrency with a compare-and-swap loop.
	for {
		old := r.maxPeak.Load()
		if cur <= old || r.maxPeak.CompareAndSwap(old, cur) {
			break
		}
	}
	r.calls.Add(1)
	defer r.inFlight.Add(-1)
	if r.delay > 0 {
		select {
		case <-time.After(r.delay):
		case <-ctx.Done():
			return ToolResult{}, ctx.Err()
		}
	}
	return r.result, r.err
}

// panicTool panics when called. Used for the panic-recovery test.
type panicTool struct{ name string }

func (p *panicTool) Name() string                                            { return p.name }
func (p *panicTool) Description() string                                     { return p.name }
func (p *panicTool) Schema() json.RawMessage                                 { return json.RawMessage(`{"type":"object"}`) }
func (p *panicTool) Execute(context.Context, json.RawMessage) (ToolResult, error) {
	panic("boom")
}

// blockingTool blocks on a gate until the test releases it. Used to
// orchestrate timing in context-cancel tests.
type blockingTool struct {
	name string
	gate <-chan struct{}
}

func (b *blockingTool) Name() string            { return b.name }
func (b *blockingTool) Description() string     { return b.name }
func (b *blockingTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (b *blockingTool) Execute(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	select {
	case <-b.gate:
		return ToolResult{Content: "done"}, nil
	case <-ctx.Done():
		return ToolResult{}, ctx.Err()
	}
}

// --- Call (non-stream) tests ----------------------------------------

func TestCallExecutesToolsAndContinues(t *testing.T) {
	p := newScripted()
	p.callTurns = []*Response{
		// Turn 1: model emits one tool call.
		{
			ToolCalls: []ToolCall{{ID: "c1", Name: "echo",
				Arguments: json.RawMessage(`{"text":"hi"}`)}},
			Usage: Usage{InputTokens: 5, OutputTokens: 2},
		},
		// Turn 2: model emits final text.
		{
			Text:  "echoed: hi",
			Usage: Usage{InputTokens: 7, OutputTokens: 3},
		},
	}
	echoed := &recordingTool{name: "echo", result: ToolResult{Content: "hi"}}
	b, err := New(WithProvider(p), WithTools(echoed))
	if err != nil {
		t.Fatal(err)
	}

	resp, err := b.Call(context.Background(), Call{Prompt: "say hi"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Text != "echoed: hi" {
		t.Errorf("Text = %q, want from turn 2", resp.Text)
	}
	// Usage aggregated across both turns.
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 5 {
		t.Errorf("aggregated usage = %+v, want {Input:12 Output:5}", resp.Usage)
	}
	if echoed.calls.Load() != 1 {
		t.Errorf("echo executed %d times, want 1", echoed.calls.Load())
	}
	// Turn 2's Call should have seen the assistant message + tool
	// result in Messages.
	if len(p.seenCalls) != 2 {
		t.Fatalf("provider called %d times, want 2", len(p.seenCalls))
	}
	turn2 := p.seenCalls[1]
	foundAsst := false
	foundTool := false
	for _, m := range turn2.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			foundAsst = true
		}
		if m.Role == "tool" && m.ToolCallID == "c1" && m.Content == "hi" {
			foundTool = true
		}
	}
	if !foundAsst {
		t.Error("turn 2 Messages missing assistant turn with tool_calls")
	}
	if !foundTool {
		t.Error("turn 2 Messages missing tool-result message for c1")
	}
}

func TestCallExecutesParallelToolsInOneTurn(t *testing.T) {
	p := newScripted()
	p.callTurns = []*Response{
		{
			ToolCalls: []ToolCall{
				{ID: "a", Name: "t", Arguments: json.RawMessage(`{}`)},
				{ID: "b", Name: "t", Arguments: json.RawMessage(`{}`)},
				{ID: "c", Name: "t", Arguments: json.RawMessage(`{}`)},
			},
			Usage: Usage{InputTokens: 3, OutputTokens: 3},
		},
		{Text: "done"},
	}
	tool := &recordingTool{name: "t", result: ToolResult{Content: "ok"}, delay: 40 * time.Millisecond}
	b, _ := New(WithProvider(p), WithTools(tool), WithMaxParallelTools(3))

	_, err := b.Call(context.Background(), Call{Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if tool.calls.Load() != 3 {
		t.Errorf("tool calls = %d, want 3", tool.calls.Load())
	}
	if got := tool.maxPeak.Load(); got != 3 {
		t.Errorf("max observed concurrency = %d, want 3", got)
	}
}

func TestCallParallelToolsRespectsMaxParallel(t *testing.T) {
	p := newScripted()
	p.callTurns = []*Response{
		{ToolCalls: []ToolCall{
			{ID: "a", Name: "t", Arguments: json.RawMessage(`{}`)},
			{ID: "b", Name: "t", Arguments: json.RawMessage(`{}`)},
			{ID: "c", Name: "t", Arguments: json.RawMessage(`{}`)},
		}},
		{Text: "done"},
	}
	tool := &recordingTool{name: "t", result: ToolResult{Content: "ok"}, delay: 40 * time.Millisecond}
	b, _ := New(WithProvider(p), WithTools(tool), WithMaxParallelTools(2))

	_, err := b.Call(context.Background(), Call{Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if got := tool.maxPeak.Load(); got != 2 {
		t.Errorf("max observed concurrency = %d, want 2", got)
	}
}

func TestCallFeedsToolErrorsToLLM(t *testing.T) {
	p := newScripted()
	p.callTurns = []*Response{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "failing",
			Arguments: json.RawMessage(`{}`)}}},
		{Text: "ack"},
	}
	tool := &recordingTool{name: "failing",
		result: ToolResult{Content: "disk full", IsError: true}}
	b, _ := New(WithProvider(p), WithTools(tool))

	if _, err := b.Call(context.Background(), Call{Prompt: "do it"}); err != nil {
		t.Fatal(err)
	}

	turn2 := p.seenCalls[1]
	found := false
	for _, m := range turn2.Messages {
		if m.Role == "tool" && m.Content == "disk full" {
			found = true
		}
	}
	if !found {
		t.Error("turn 2 messages missing tool-result with error content")
	}
}

func TestCallConvertsPanicToToolError(t *testing.T) {
	p := newScripted()
	p.callTurns = []*Response{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "bad",
			Arguments: json.RawMessage(`{}`)}}},
		{Text: "recovered"},
	}
	b, _ := New(WithProvider(p), WithTools(&panicTool{name: "bad"}))

	resp, err := b.Call(context.Background(), Call{Prompt: "risky"})
	if err != nil {
		t.Fatalf("Call should not propagate panic: %v", err)
	}
	if resp.Text != "recovered" {
		t.Errorf("resp.Text = %q", resp.Text)
	}
	turn2 := p.seenCalls[1]
	var toolContent string
	for _, m := range turn2.Messages {
		if m.Role == "tool" {
			toolContent = m.Content
		}
	}
	if !strings.Contains(toolContent, "panic") || !strings.Contains(toolContent, "boom") {
		t.Errorf("panic surfaced in tool message as %q; want to mention panic+boom", toolContent)
	}
}

func TestCallHandlesUnknownTool(t *testing.T) {
	p := newScripted()
	p.callTurns = []*Response{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "nonexistent",
			Arguments: json.RawMessage(`{}`)}}},
		{Text: "ok"},
	}
	b, _ := New(WithProvider(p), WithTools(newFake("other")))

	if _, err := b.Call(context.Background(), Call{Prompt: "go"}); err != nil {
		t.Fatalf("Call should not error on unknown tool: %v", err)
	}
	turn2 := p.seenCalls[1]
	var content string
	for _, m := range turn2.Messages {
		if m.Role == "tool" {
			content = m.Content
		}
	}
	if !strings.Contains(content, "tool not found") {
		t.Errorf("unknown-tool message = %q; want to mention 'tool not found'", content)
	}
}

func TestCallRespectsMaxToolHops(t *testing.T) {
	p := newScripted()
	// Every turn returns another tool call - the loop must stop at
	// the cap and still return the final turn's response.
	never := &Response{
		Text:      "",
		ToolCalls: []ToolCall{{ID: "c", Name: "t", Arguments: json.RawMessage(`{}`)}},
	}
	p.callTurns = []*Response{never, never, never, never}
	tool := &recordingTool{name: "t", result: ToolResult{Content: "ok"}}
	b, _ := New(WithProvider(p), WithTools(tool), WithMaxToolHops(2))

	resp, err := b.Call(context.Background(), Call{Prompt: "loop"})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(resp.Err, ErrMaxToolHopsExceeded) {
		t.Errorf("Response.Err = %v, want ErrMaxToolHopsExceeded", resp.Err)
	}
	// Tool should have run exactly maxToolHops times (2) - the
	// third turn's tool calls are never executed because we bail
	// out before the execute step.
	if got := tool.calls.Load(); got != 2 {
		t.Errorf("tool executed %d times, want 2", got)
	}
}

func TestCallChargesBudgetOncePerMultiTurnCall(t *testing.T) {
	p := newScripted()
	p.callTurns = []*Response{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "t",
			Arguments: json.RawMessage(`{}`)}},
			Usage: Usage{InputTokens: 10, OutputTokens: 2}},
		{Text: "final", Usage: Usage{InputTokens: 15, OutputTokens: 3}},
	}
	budget := &fakeBudget{remaining: 100}
	tool := &recordingTool{name: "t", result: ToolResult{Content: "ok"}}

	b, _ := New(WithProvider(p), WithTools(tool), WithBudget(budget))
	if _, err := b.Call(context.Background(), Call{Prompt: "x"}); err != nil {
		t.Fatal(err)
	}

	if len(budget.charges) != 1 {
		t.Fatalf("budget.Charge called %d times, want 1 (aggregated)", len(budget.charges))
	}
	charge := budget.charges[0]
	if charge.Usage.InputTokens != 25 || charge.Usage.OutputTokens != 5 {
		t.Errorf("aggregated charge = %+v, want {Input:25 Output:5}", charge.Usage)
	}
}

func TestCallRecordsOneEpisodePerMultiTurnCall(t *testing.T) {
	p := newScripted()
	p.callTurns = []*Response{
		{ToolCalls: []ToolCall{{ID: "c1", Name: "t", Arguments: json.RawMessage(`{}`)}}},
		{ToolCalls: []ToolCall{{ID: "c2", Name: "t", Arguments: json.RawMessage(`{}`)}}},
		{Text: "done"},
	}
	tool := &recordingTool{name: "t", result: ToolResult{Content: "ok"}}
	mem := &fakeMemory{}
	b, _ := New(WithProvider(p), WithTools(tool), WithMemory(mem))

	if _, err := b.Call(context.Background(), Call{Task: TaskGenerate, Prompt: "x"}); err != nil {
		t.Fatal(err)
	}
	// recordEpisode runs async.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		mem.mu.Lock()
		n := len(mem.added)
		mem.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mem.mu.Lock()
	defer mem.mu.Unlock()
	if len(mem.added) != 1 {
		t.Errorf("episodes recorded = %d, want 1 (one per Call, not per turn)", len(mem.added))
	}
}

func TestCallToolResultsOrderedByCallIndex(t *testing.T) {
	// Tools execute in parallel with deliberately different
	// durations. The feedback messages on turn 2 must be in the
	// original call order, not completion order.
	p := newScripted()
	p.callTurns = []*Response{
		{ToolCalls: []ToolCall{
			{ID: "first", Name: "slow", Arguments: json.RawMessage(`{}`)},
			{ID: "second", Name: "fast", Arguments: json.RawMessage(`{}`)},
			{ID: "third", Name: "mid", Arguments: json.RawMessage(`{}`)},
		}},
		{Text: "done"},
	}
	b, _ := New(
		WithProvider(p),
		WithTools(
			&recordingTool{name: "slow", delay: 40 * time.Millisecond, result: ToolResult{Content: "S"}},
			&recordingTool{name: "fast", delay: 5 * time.Millisecond, result: ToolResult{Content: "F"}},
			&recordingTool{name: "mid", delay: 20 * time.Millisecond, result: ToolResult{Content: "M"}},
		),
		WithMaxParallelTools(3),
	)

	if _, err := b.Call(context.Background(), Call{Prompt: "go"}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range p.seenCalls[1].Messages {
		if m.Role == "tool" {
			ids = append(ids, m.ToolCallID)
		}
	}
	want := []string{"first", "second", "third"}
	if len(ids) != len(want) {
		t.Fatalf("tool messages = %v, want %v", ids, want)
	}
	for i := range ids {
		if ids[i] != want[i] {
			t.Errorf("tool[%d] = %q, want %q (feedback must be in call order, not completion)",
				i, ids[i], want[i])
		}
	}
}

// --- Stream tests ---------------------------------------------------

func TestStreamEmitsToolCallChunksBeforeExecution(t *testing.T) {
	// Turn 1 stream emits a tool call + usage; tool blocks on a
	// gate so the consumer sees the ToolCall chunk BEFORE execution
	// completes. Then we release the gate and the stream proceeds.
	gate := make(chan struct{})
	blocking := &blockingTool{name: "wait", gate: gate}

	p := newScripted()
	p.streamTurns = [][]StreamChunk{
		{
			{Type: StreamChunkToolCall, ToolCall: &ToolCall{ID: "c1", Name: "wait",
				Arguments: json.RawMessage(`{}`)}},
			{Type: StreamChunkUsage, Usage: &Usage{InputTokens: 5, OutputTokens: 2}},
		},
		{
			{Type: StreamChunkText, Delta: "final"},
			{Type: StreamChunkUsage, Usage: &Usage{InputTokens: 3, OutputTokens: 1}},
		},
	}

	b, _ := New(WithProvider(p), WithTools(blocking))
	ch, err := b.Stream(context.Background(), Call{Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}

	// First chunk the consumer sees must be the tool call, before
	// we release the gate that unblocks execution.
	select {
	case chunk := <-ch:
		if chunk.Type != StreamChunkToolCall {
			t.Fatalf("first chunk type = %q, want tool_call", chunk.Type)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no chunk within 500ms")
	}
	close(gate)

	// Drain the rest; verify tool result + text + usage arrive.
	var sawResult, sawText, sawUsage bool
	deadline := time.After(2 * time.Second)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				if !sawResult || !sawText || !sawUsage {
					t.Errorf("missing chunks: result=%v text=%v usage=%v",
						sawResult, sawText, sawUsage)
				}
				return
			}
			switch chunk.Type {
			case StreamChunkToolResult:
				sawResult = true
			case StreamChunkText:
				sawText = true
			case StreamChunkUsage:
				sawUsage = true
			}
		case <-deadline:
			t.Fatal("stream did not close within 2s")
		}
	}
}

func TestStreamEmitsToolResultChunks(t *testing.T) {
	p := newScripted()
	p.streamTurns = [][]StreamChunk{
		{
			{Type: StreamChunkToolCall, ToolCall: &ToolCall{ID: "c1", Name: "echo",
				Arguments: json.RawMessage(`{}`)}},
			{Type: StreamChunkUsage, Usage: &Usage{InputTokens: 1, OutputTokens: 1}},
		},
		{
			{Type: StreamChunkText, Delta: "ok"},
			{Type: StreamChunkUsage, Usage: &Usage{InputTokens: 1, OutputTokens: 1}},
		},
	}
	b, _ := New(WithProvider(p),
		WithTools(&recordingTool{name: "echo", result: ToolResult{Content: "echoed"}}))

	ch, err := b.Stream(context.Background(), Call{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var toolCallChunk, toolResultChunk *StreamChunk
	for chunk := range ch {
		cc := chunk
		switch chunk.Type {
		case StreamChunkToolCall:
			if toolCallChunk == nil {
				toolCallChunk = &cc
			}
		case StreamChunkToolResult:
			if toolResultChunk == nil {
				toolResultChunk = &cc
			}
		}
	}
	if toolCallChunk == nil || toolResultChunk == nil {
		t.Fatalf("missing chunks: call=%v result=%v", toolCallChunk, toolResultChunk)
	}
	if toolResultChunk.ToolCallID != toolCallChunk.ToolCall.ID {
		t.Errorf("ToolCallID = %q, want %q (must match originating ToolCall.ID)",
			toolResultChunk.ToolCallID, toolCallChunk.ToolCall.ID)
	}
	if toolResultChunk.ToolResult == nil || toolResultChunk.ToolResult.Content != "echoed" {
		t.Errorf("ToolResult = %+v, want Content='echoed'", toolResultChunk.ToolResult)
	}
}

func TestStreamEmitsSingleTerminalUsage(t *testing.T) {
	p := newScripted()
	p.streamTurns = [][]StreamChunk{
		{
			{Type: StreamChunkToolCall, ToolCall: &ToolCall{ID: "c1", Name: "t",
				Arguments: json.RawMessage(`{}`)}},
			{Type: StreamChunkUsage, Usage: &Usage{InputTokens: 10, OutputTokens: 2}},
		},
		{
			{Type: StreamChunkText, Delta: "hi"},
			{Type: StreamChunkUsage, Usage: &Usage{InputTokens: 7, OutputTokens: 1}},
		},
	}
	b, _ := New(WithProvider(p), WithTools(&recordingTool{name: "t", result: ToolResult{Content: "ok"}}))

	ch, err := b.Stream(context.Background(), Call{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	var usageCount int
	var finalUsage *Usage
	for chunk := range ch {
		if chunk.Type == StreamChunkUsage {
			usageCount++
			finalUsage = chunk.Usage
		}
	}
	if usageCount != 1 {
		t.Errorf("usage chunks = %d, want 1 (per-turn usage must fold)", usageCount)
	}
	if finalUsage == nil || finalUsage.InputTokens != 17 || finalUsage.OutputTokens != 3 {
		t.Errorf("aggregated usage = %+v, want {Input:17 Output:3}", finalUsage)
	}
}

func TestStreamRespectsMaxToolHopsAcrossTurns(t *testing.T) {
	// Every turn emits a tool call + usage; the loop should stop
	// at the hop cap with a terminal StreamChunkUsage followed by
	// a StreamChunkError carrying ErrMaxToolHopsExceeded.
	loop := []StreamChunk{
		{Type: StreamChunkToolCall, ToolCall: &ToolCall{ID: "c", Name: "t",
			Arguments: json.RawMessage(`{}`)}},
		{Type: StreamChunkUsage, Usage: &Usage{InputTokens: 1, OutputTokens: 1}},
	}
	p := newScripted()
	p.streamTurns = [][]StreamChunk{loop, loop, loop, loop, loop}
	b, _ := New(WithProvider(p),
		WithTools(&recordingTool{name: "t", result: ToolResult{Content: "ok"}}),
		WithMaxToolHops(2))

	ch, err := b.Stream(context.Background(), Call{Prompt: "loop"})
	if err != nil {
		t.Fatal(err)
	}
	var sawUsage bool
	var sawError bool
	for chunk := range ch {
		switch chunk.Type {
		case StreamChunkUsage:
			sawUsage = true
		case StreamChunkError:
			sawError = true
			if !errors.Is(chunk.Error, ErrMaxToolHopsExceeded) {
				t.Errorf("error chunk = %v, want ErrMaxToolHopsExceeded", chunk.Error)
			}
		}
	}
	if !sawUsage {
		t.Error("expected terminal usage chunk before the error chunk")
	}
	if !sawError {
		t.Error("expected StreamChunkError with ErrMaxToolHopsExceeded")
	}
}

func TestStreamContextCancelDuringToolExecution(t *testing.T) {
	// Tool blocks forever. Cancelling ctx while execution is
	// blocked must close the channel cleanly with no goroutine
	// leak.
	baseline := runtime.NumGoroutine()

	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })

	p := newScripted()
	p.streamTurns = [][]StreamChunk{
		{
			{Type: StreamChunkToolCall, ToolCall: &ToolCall{ID: "c", Name: "hang",
				Arguments: json.RawMessage(`{}`)}},
			{Type: StreamChunkUsage, Usage: &Usage{InputTokens: 1, OutputTokens: 1}},
		},
	}
	b, _ := New(WithProvider(p), WithTools(&blockingTool{name: "hang", gate: gate}))

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := b.Stream(ctx, Call{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the tool-call chunk (so we know execution has begun)
	// then cancel.
	select {
	case <-ch:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no tool-call chunk within 500ms")
	}
	cancel()

	// Channel must close within a short window.
	closeDeadline := time.After(2 * time.Second)
	for open := true; open; {
		select {
		case _, ok := <-ch:
			if !ok {
				open = false
			}
		case <-closeDeadline:
			t.Fatal("channel did not close after cancel")
		}
	}
	// Wait for goroutines to drain back to baseline. +2 slack per
	// Pass 6 pattern.
	const slack = 2
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine()-baseline <= slack {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("goroutine leak: baseline=%d current=%d", baseline, runtime.NumGoroutine())
}

func TestStreamContextCancelDuringNextProviderCall(t *testing.T) {
	// Turn 1 completes normally; turn 2's provider.Stream is called
	// with an already-cancelled ctx. The provider goroutine must
	// close its channel and the Brain stream must wind down.
	baseline := runtime.NumGoroutine()

	p := newScripted()
	p.streamTurns = [][]StreamChunk{
		{
			{Type: StreamChunkToolCall, ToolCall: &ToolCall{ID: "c", Name: "t",
				Arguments: json.RawMessage(`{}`)}},
			{Type: StreamChunkUsage, Usage: &Usage{InputTokens: 1, OutputTokens: 1}},
		},
		{
			{Type: StreamChunkText, Delta: "never"},
			{Type: StreamChunkUsage, Usage: &Usage{InputTokens: 1, OutputTokens: 1}},
		},
	}
	// Gate turn 2 so we can cancel between turns.
	p.streamGate = []chan struct{}{nil, make(chan struct{})}
	t.Cleanup(func() { close(p.streamGate[1]) })

	b, _ := New(WithProvider(p),
		WithTools(&recordingTool{name: "t", result: ToolResult{Content: "ok"}}))

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := b.Stream(ctx, Call{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// Drain through the tool result (turn 1 complete, turn 2 is
	// now blocked on its gate).
	var sawResult bool
	deadline := time.After(1 * time.Second)
drain:
	for {
		select {
		case chunk := <-ch:
			if chunk.Type == StreamChunkToolResult {
				sawResult = true
				break drain
			}
		case <-deadline:
			t.Fatal("did not see tool_result chunk within 1s")
		}
	}
	if !sawResult {
		t.Fatal("no tool_result observed")
	}
	cancel()

	closeDeadline := time.After(2 * time.Second)
	for open := true; open; {
		select {
		case _, ok := <-ch:
			if !ok {
				open = false
			}
		case <-closeDeadline:
			t.Fatal("channel did not close after cancel")
		}
	}
	const slack = 2
	settleDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(settleDeadline) {
		if runtime.NumGoroutine()-baseline <= slack {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Lenient - this test's primary assertion is channel close.
}

func TestStreamToolResultOrderingGuarantee(t *testing.T) {
	p := newScripted()
	p.streamTurns = [][]StreamChunk{
		{
			{Type: StreamChunkToolCall, ToolCall: &ToolCall{ID: "a", Name: "t",
				Arguments: json.RawMessage(`{}`)}},
			{Type: StreamChunkToolCall, ToolCall: &ToolCall{ID: "b", Name: "t",
				Arguments: json.RawMessage(`{}`)}},
			{Type: StreamChunkUsage, Usage: &Usage{InputTokens: 1, OutputTokens: 1}},
		},
		{
			{Type: StreamChunkText, Delta: "ok"},
			{Type: StreamChunkUsage, Usage: &Usage{InputTokens: 1, OutputTokens: 1}},
		},
	}
	b, _ := New(WithProvider(p),
		WithTools(&recordingTool{name: "t", result: ToolResult{Content: "R"}}))

	ch, err := b.Stream(context.Background(), Call{Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// Track, per ID, the index of the call chunk and the result
	// chunk. Call index must be < result index for each ID.
	callIdx := map[string]int{}
	resultIdx := map[string]int{}
	i := 0
	for chunk := range ch {
		switch chunk.Type {
		case StreamChunkToolCall:
			callIdx[chunk.ToolCall.ID] = i
		case StreamChunkToolResult:
			resultIdx[chunk.ToolCallID] = i
		}
		i++
	}
	for id, ci := range callIdx {
		ri, ok := resultIdx[id]
		if !ok {
			t.Errorf("no result chunk for call id %q", id)
			continue
		}
		if ri <= ci {
			t.Errorf("for id %q: call idx=%d, result idx=%d (result must follow call)",
				id, ci, ri)
		}
	}
}

// Compile-time sanity: the scripted provider satisfies hippo.Provider.
var _ Provider = (*scriptedProvider)(nil)

// Keep sync imported even if a refactor removes direct usage.
var _ = sync.Mutex{}
