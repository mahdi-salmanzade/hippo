// Command ollama demonstrates hippo's Ollama provider end-to-end
// against a local daemon: non-streaming Call, streaming Stream, and
// the privacy tier that makes Ollama the default LocalOnly backend.
//
// Run with:
//
//	go run ./examples/ollama                       # default http://localhost:11434
//	OLLAMA_HOST=http://host:11434 go run ./examples/ollama
//
// If the daemon isn't reachable the program prints an install hint
// and exits 0 - local-only infrastructure shouldn't break example
// runs on machines that haven't set it up.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/providers/ollama"
)

const defaultOllamaURL = "http://localhost:11434"

func main() {
	baseURL := defaultOllamaURL
	if v := os.Getenv("OLLAMA_HOST"); v != "" {
		baseURL = v
	}

	if !reachable(baseURL) {
		fmt.Fprintln(os.Stderr, "Ollama daemon not reachable at", baseURL)
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Install with:  brew install ollama  (or see https://ollama.com/download)")
		fmt.Fprintln(os.Stderr, "Start with:    ollama serve")
		fmt.Fprintln(os.Stderr, "Pull a model:  ollama pull qwen2.5:0.5b   # smallest, ~400MB")
		return
	}

	p, err := ollama.New(ollama.WithBaseURL(baseURL))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	model := pickModel(p)
	if model == "" {
		fmt.Fprintln(os.Stderr, "No models installed. Pull one with:")
		fmt.Fprintln(os.Stderr, "  ollama pull qwen2.5:0.5b")
		return
	}

	b, err := hippo.New(hippo.WithProvider(p))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	fmt.Printf("=== non-stream Call against %s ===\n", model)
	resp, err := b.Call(ctx, hippo.Call{
		Model:     model,
		Prompt:    "In one sentence, describe what Go's channels are.",
		MaxTokens: 150,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "Call:", err)
		os.Exit(1)
	}
	fmt.Println(resp.Text)
	fmt.Printf("[usage: %d in / %d out · latency %dms]\n\n",
		resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.LatencyMS)

	fmt.Printf("=== streaming Call against %s ===\n", model)
	ch, err := b.Stream(ctx, hippo.Call{
		Model:     model,
		Prompt:    "List three Go best practices, one per line.",
		MaxTokens: 200,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "Stream:", err)
		os.Exit(1)
	}
	for chunk := range ch {
		switch chunk.Type {
		case hippo.StreamChunkText:
			fmt.Print(chunk.Delta)
		case hippo.StreamChunkError:
			fmt.Fprintln(os.Stderr, "\nstream error:", chunk.Error)
			os.Exit(1)
		case hippo.StreamChunkUsage:
			fmt.Printf("\n[%s/%s · %d in / %d out · cost $%.4f]\n",
				chunk.Provider, chunk.Model,
				chunk.Usage.InputTokens, chunk.Usage.OutputTokens, chunk.CostUSD)
		}
	}
}

// reachable probes /api/version with a short timeout so the example
// prints an actionable message rather than a stack trace when the
// daemon isn't running.
func reachable(baseURL string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/version", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// pickModel returns a small model if one is installed, else the
// first thing Models() reports, else "".
func pickModel(p hippo.Provider) string {
	models := p.Models()
	if len(models) == 0 {
		return ""
	}
	preferred := []string{
		"qwen2.5:0.5b", "qwen2.5:1.5b", "phi3.5:3.8b", "phi3:3.8b",
		"llama3.2:1b", "llama3.2:3b",
	}
	installed := map[string]bool{}
	for _, m := range models {
		installed[m.ID] = true
	}
	for _, p := range preferred {
		if installed[p] {
			return p
		}
	}
	return models[0].ID
}
