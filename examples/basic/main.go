// Command basic is a minimal hippo example: construct a Brain with a
// single provider and issue one Call.
//
// Run with:
//
//	ANTHROPIC_API_KEY=sk-... go run ./examples/basic
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

	resp, err := b.Call(context.Background(), hippo.Call{
		Task:   hippo.TaskGenerate,
		Prompt: "Say hello in one sentence.",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(resp.Text)
}
