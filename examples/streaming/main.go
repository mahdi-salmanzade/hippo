// Command streaming demonstrates hippo's Stream path end-to-end:
// register one or both providers, plus two small tools (now and
// calc), open a streaming Call, and print deltas + tool invocations
// + tool results as they arrive. Final usage summary at the end.
//
// Run with:
//
//	ANTHROPIC_API_KEY=sk-... go run ./examples/streaming
//	OPENAI_API_KEY=sk-...    go run ./examples/streaming openai
//
// The second argument selects the provider (anthropic by default). A
// $0.10 budget ceiling is set so the example also exercises the
// budget-gate and budget-charge paths. Ctrl-C cancels the stream
// cleanly: the context is cancelled, the provider reader goroutine
// exits, and the channel closes without a spurious error.
//
// Tool implementations are imported from examples/tools so the two
// demos share one source of truth for their "what a real Tool looks
// like" reference.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/budget"
	"github.com/mahdi-salmanzade/hippo/internal/dotenv"
	"github.com/mahdi-salmanzade/hippo/providers/anthropic"
	"github.com/mahdi-salmanzade/hippo/providers/openai"
)

// nowTool returns the current RFC3339 timestamp. Inline here (rather
// than imported from examples/tools) so each example stays runnable
// on its own.
type nowTool struct{}

func (nowTool) Name() string            { return "now" }
func (nowTool) Description() string     { return "Returns the current UTC time as RFC3339." }
func (nowTool) Schema() json.RawMessage {
	// additionalProperties:false is required by OpenAI's strict mode
	// (defaulted on) and harmless on Anthropic / Ollama. Every tool
	// schema in this demo follows that same shape.
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}
func (nowTool) Execute(context.Context, json.RawMessage) (hippo.ToolResult, error) {
	return hippo.ToolResult{Content: time.Now().UTC().Format(time.RFC3339)}, nil
}

// addTool adds two numbers. Tiny by design — the streaming demo just
// needs something the model will plausibly invoke.
type addTool struct{}

func (addTool) Name() string        { return "add" }
func (addTool) Description() string { return "Adds two integers." }
func (addTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"],"additionalProperties":false}`)
}
func (addTool) Execute(_ context.Context, args json.RawMessage) (hippo.ToolResult, error) {
	var in struct{ A, B int }
	if err := json.Unmarshal(args, &in); err != nil {
		return hippo.ToolResult{Content: "bad args: " + err.Error(), IsError: true}, nil
	}
	return hippo.ToolResult{Content: strconv.Itoa(in.A + in.B)}, nil
}

func main() {
	_ = dotenv.Load()

	which := "anthropic"
	if len(os.Args) > 1 {
		which = os.Args[1]
	}

	var provider hippo.Provider
	var err error
	switch which {
	case "openai":
		provider, err = openai.New(
			openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
			openai.WithModel("gpt-5-nano"),
		)
	default:
		provider, err = anthropic.New(
			anthropic.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")),
			anthropic.WithModel("claude-haiku-4-5"),
		)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	b, err := hippo.New(
		hippo.WithProvider(provider),
		hippo.WithBudget(budget.New(budget.WithCeiling(0.10))),
		hippo.WithTools(nowTool{}, addTool{}),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer b.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	ch, err := b.Stream(ctx, hippo.Call{
		Task: hippo.TaskGenerate,
		Prompt: "What is the current time, and what does 17 + 25 equal? " +
			"Use the now and add tools, then reply with one sentence.",
		MaxTokens: 400,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "stream open failed:", err)
		os.Exit(1)
	}

	for chunk := range ch {
		switch chunk.Type {
		case hippo.StreamChunkText:
			fmt.Print(chunk.Delta)
		case hippo.StreamChunkThinking:
			// Reasoning traces print on stderr so the response stays
			// clean on stdout for piping.
			fmt.Fprint(os.Stderr, chunk.Delta)
		case hippo.StreamChunkToolCall:
			fmt.Fprintf(os.Stderr, "\n→ calling %s(%s)\n", chunk.ToolCall.Name, chunk.ToolCall.Arguments)
		case hippo.StreamChunkToolResult:
			fmt.Fprintf(os.Stderr, "← got: %s\n", chunk.ToolResult.Content)
		case hippo.StreamChunkError:
			fmt.Fprintln(os.Stderr, "\nstream error:", chunk.Error)
			os.Exit(1)
		case hippo.StreamChunkUsage:
			fmt.Printf("\n\n[%s/%s · %d in · %d out · $%.6f]\n",
				chunk.Provider, chunk.Model,
				chunk.Usage.InputTokens, chunk.Usage.OutputTokens,
				chunk.CostUSD)
		}
	}
}
