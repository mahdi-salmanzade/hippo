// Command openai is the minimal end-to-end hippo example using the
// OpenAI Responses API: construct a Brain with a single provider and
// issue one Call.
//
// Run with:
//
//	OPENAI_API_KEY=sk-... go run ./examples/openai
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/internal/dotenv"
	"github.com/mahdi-salmanzade/hippo/providers/openai"
)

func main() {
	// Load .env from the current directory (or any parent). Missing
	// file is not an error; already-set env vars win.
	_ = dotenv.Load()

	p, err := openai.New(
		openai.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
		openai.WithModel("gpt-5-nano"),
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
