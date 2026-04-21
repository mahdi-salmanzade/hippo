// Command routing is hippo's full privacy-tiered routing demo.
// Registers three providers (Anthropic, OpenAI, Ollama), the embedded
// default policy, a SQLite memory store with a couple of seeded
// records, and a $0.10 budget ceiling. Runs two Calls against the
// same Brain:
//
//  1. Task=Reason, Privacy=CloudOK   - routes to Anthropic (or OpenAI
//     as fallback), showing the cloud path.
//  2. Task=Protect, Privacy=LocalOnly - routes to Ollama, showing the
//     local path.
//
// Both calls hydrate memory from the same store; the second one pays
// $0 against the budget because Ollama is zero-cost.
//
// Run with:
//
//	ANTHROPIC_API_KEY=... OPENAI_API_KEY=... go run ./examples/routing
//
// Any provider that's unreachable / unconfigured is silently dropped
// from registration. If neither Ollama nor a cloud key is available
// the Privacy=LocalOnly or Privacy=CloudOK Call will return
// ErrNoRoutableProvider - that's the correct behaviour, and the demo
// surfaces it rather than hiding it.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/budget"
	"github.com/mahdi-salmanzade/hippo/internal/dotenv"
	"github.com/mahdi-salmanzade/hippo/memory/sqlite"
	"github.com/mahdi-salmanzade/hippo/providers/anthropic"
	"github.com/mahdi-salmanzade/hippo/providers/ollama"
	"github.com/mahdi-salmanzade/hippo/providers/openai"
	yamlrouter "github.com/mahdi-salmanzade/hippo/router/yaml"
)

const ollamaBaseURL = "http://localhost:11434"

func main() {
	_ = dotenv.Load()

	opts := []hippo.Option{}
	opts = append(opts, maybeAnthropic()...)
	opts = append(opts, maybeOpenAI()...)
	opts = append(opts, maybeOllama()...)

	if len(opts) == 0 {
		log.Fatal("no providers available - set ANTHROPIC_API_KEY, OPENAI_API_KEY, or start a local Ollama")
	}

	r, err := yamlrouter.Load("")
	if err != nil {
		log.Fatal(err)
	}
	opts = append(opts, hippo.WithRouter(r))

	mem, err := sqlite.Open(":memory:")
	if err != nil {
		log.Fatal(err)
	}
	defer mem.Close()
	opts = append(opts, hippo.WithMemory(mem))

	bud := budget.New(budget.WithCeiling(0.10))
	opts = append(opts, hippo.WithBudget(bud))

	ctx := context.Background()
	for _, rec := range []hippo.Record{
		{Kind: hippo.MemoryProfile, Timestamp: time.Now(), Content: "User prefers TypeScript.", Tags: []string{"preference"}, Importance: 0.9},
		{Kind: hippo.MemoryEpisodic, Timestamp: time.Now().Add(-1 * time.Hour), Content: "Working on the billing refactor; tests still failing.", Tags: []string{"billing", "wip"}, Importance: 0.7},
	} {
		r := rec
		if err := mem.Add(ctx, &r); err != nil {
			log.Fatalf("seed: %v", err)
		}
	}

	b, err := hippo.New(opts...)
	if err != nil {
		log.Fatal(err)
	}
	defer b.Close()

	runCloudCall(ctx, b, bud)
	fmt.Println()
	runLocalCall(ctx, b, bud)
}

func runCloudCall(ctx context.Context, b *hippo.Brain, bud hippo.BudgetTracker) {
	fmt.Println("=== Task=Reason, Privacy=CloudOK ===")
	resp, err := b.Call(ctx, hippo.Call{
		Task:      hippo.TaskReason,
		Prompt:    "What should I work on next?",
		UseMemory: hippo.MemoryScope{Mode: hippo.MemoryScopeRecent},
		MaxTokens: 150,
	})
	if err != nil {
		if errors.Is(err, hippo.ErrNoRoutableProvider) {
			fmt.Println("(no cloud provider registered; skipping)")
			return
		}
		log.Fatalf("cloud call: %v", err)
	}
	fmt.Printf("routed to: %s/%s\n", resp.Provider, resp.Model)
	fmt.Printf("memory hits: %d\n", len(resp.MemoryHits))
	fmt.Println("response:", resp.Text)
	fmt.Printf("cost: $%.6f  (spent total: $%.6f / remaining: $%.6f)\n",
		resp.CostUSD, bud.Spent(), bud.Remaining())
}

func runLocalCall(ctx context.Context, b *hippo.Brain, bud hippo.BudgetTracker) {
	fmt.Println("=== Task=Protect, Privacy=LocalOnly ===")
	resp, err := b.Call(ctx, hippo.Call{
		Task:      hippo.TaskProtect,
		Privacy:   hippo.PrivacyLocalOnly,
		Prompt:    "Summarise my working-memory notes - don't let this leave the device.",
		UseMemory: hippo.MemoryScope{Mode: hippo.MemoryScopeRecent},
		MaxTokens: 150,
	})
	if err != nil {
		if errors.Is(err, hippo.ErrNoRoutableProvider) {
			fmt.Println("(no local provider registered - install Ollama to exercise this path)")
			return
		}
		log.Fatalf("local call: %v", err)
	}
	fmt.Printf("routed to: %s/%s\n", resp.Provider, resp.Model)
	fmt.Printf("memory hits: %d\n", len(resp.MemoryHits))
	fmt.Println("response:", resp.Text)
	fmt.Printf("cost: $%.6f  (spent total: $%.6f / remaining: $%.6f)\n",
		resp.CostUSD, bud.Spent(), bud.Remaining())
}

func maybeAnthropic() []hippo.Option {
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		return nil
	}
	p, err := anthropic.New(anthropic.WithAPIKey(key))
	if err != nil {
		log.Printf("anthropic: %v - skipping", err)
		return nil
	}
	return []hippo.Option{hippo.WithProvider(p)}
}

func maybeOpenAI() []hippo.Option {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return nil
	}
	p, err := openai.New(openai.WithAPIKey(key))
	if err != nil {
		log.Printf("openai: %v - skipping", err)
		return nil
	}
	return []hippo.Option{hippo.WithProvider(p)}
}

func maybeOllama() []hippo.Option {
	if !ollamaReachable(ollamaBaseURL) {
		return nil
	}
	p, err := ollama.New(ollama.WithBaseURL(ollamaBaseURL))
	if err != nil {
		log.Printf("ollama: %v - skipping", err)
		return nil
	}
	return []hippo.Option{hippo.WithProvider(p)}
}

func ollamaReachable(baseURL string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
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
