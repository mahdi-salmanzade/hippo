// Command streaming is a hippo streaming example: issue a Call and
// print StreamChunk deltas as they arrive.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/providers/anthropic"
)

func main() {
	b, err := hippo.New(
		hippo.WithProvider(anthropic.New(os.Getenv("ANTHROPIC_API_KEY"))),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer b.Close()

	ch, err := b.Stream(context.Background(), hippo.Call{
		Task:   hippo.TaskGenerate,
		Prompt: "Write a haiku about Go's goroutines.",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for chunk := range ch {
		if chunk.Err != nil {
			fmt.Fprintln(os.Stderr, chunk.Err)
			os.Exit(1)
		}
		fmt.Print(chunk.Delta)
		if chunk.Final {
			fmt.Printf("\n\n[%s/%s · %d in · %d out · $%.4f]\n",
				"provider", "model", // TODO: populate on Final chunk
				chunk.Usage.InputTokens, chunk.Usage.OutputTokens, chunk.CostUSD)
		}
	}
}
