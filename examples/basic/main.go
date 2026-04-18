// Command basic is the minimal end-to-end hippo example: construct a
// Brain with a single provider and issue one Call.
//
// Run with:
//
//	ANTHROPIC_API_KEY=sk-... go run ./examples/basic
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/providers/anthropic"
)

func main() {
	p, err := anthropic.New(
		anthropic.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")),
		anthropic.WithModel("claude-opus-4-7"),
	)
	if err != nil {
		log.Fatal(err)
	}

	brain, err := hippo.New(hippo.WithProvider(p))
	if err != nil {
		log.Fatal(err)
	}
	defer brain.Close()

	resp, err := brain.Call(context.Background(), hippo.Call{
		Prompt:    "Say hi in 5 words.",
		MaxTokens: 50,
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(resp.Text)
	fmt.Printf("usage: %+v\n", resp.Usage)
	fmt.Printf("cost:  $%.6f\n", resp.CostUSD)
	fmt.Printf("latency: %dms\n", resp.LatencyMS)
}
