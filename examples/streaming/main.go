// Command streaming demonstrates hippo's Stream path end-to-end:
// register one or both providers, open a streaming Call, print text
// deltas as they arrive, and summarise usage + cost on completion.
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
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/budget"
	"github.com/mahdi-salmanzade/hippo/internal/dotenv"
	"github.com/mahdi-salmanzade/hippo/providers/anthropic"
	"github.com/mahdi-salmanzade/hippo/providers/openai"
)

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
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer b.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	ch, err := b.Stream(ctx, hippo.Call{
		Task:      hippo.TaskGenerate,
		Prompt:    "Write a four-sentence description of how goroutines and channels cooperate in Go.",
		MaxTokens: 300,
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
			fmt.Fprintf(os.Stderr, "\n[tool call: %s]\n", chunk.ToolCall.Name)
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
