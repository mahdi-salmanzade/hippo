// Command routing is hippo's end-to-end Pass 3 demo: an Anthropic
// provider, the embedded default policy, a SQLite memory store with a
// couple of seeded records, and a $0.10 budget ceiling. It shows the
// three Pass 3 pillars in one place — routing, memory hydration, and
// cost accounting.
//
// Run with:
//
//	ANTHROPIC_API_KEY=sk-... go run ./examples/routing
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/budget"
	"github.com/mahdi-salmanzade/hippo/internal/dotenv"
	"github.com/mahdi-salmanzade/hippo/memory/sqlite"
	"github.com/mahdi-salmanzade/hippo/providers/anthropic"
	yamlrouter "github.com/mahdi-salmanzade/hippo/router/yaml"
)

func main() {
	_ = dotenv.Load()

	// 1. Provider. Anthropic with whichever model the router picks.
	p, err := anthropic.New(anthropic.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")))
	if err != nil {
		log.Fatal(err)
	}

	// 2. Router. Empty path → embedded default_policy.yaml.
	r, err := yamlrouter.Load("")
	if err != nil {
		log.Fatal(err)
	}

	// 3. Memory. In-memory SQLite is enough for a one-shot demo.
	mem, err := sqlite.Open(":memory:")
	if err != nil {
		log.Fatal(err)
	}
	defer mem.Close()

	// 4. Budget. $0.10 ceiling — plenty for a one-turn call, tight
	// enough to see Spent / Remaining move.
	bud := budget.New(budget.WithCeiling(0.10))

	// Seed two facts so hydration has something to retrieve.
	ctx := context.Background()
	for _, rec := range []hippo.Record{
		{Kind: hippo.MemoryProfile, Timestamp: time.Now(), Content: "User prefers TypeScript.", Tags: []string{"preference"}, Importance: 0.9},
		{Kind: hippo.MemoryEpisodic, Timestamp: time.Now().Add(-1 * time.Hour), Content: "Working on the billing refactor; tests still failing.", Tags: []string{"billing", "wip"}, Importance: 0.7},
	} {
		r := rec // capture
		if err := mem.Add(ctx, &r); err != nil {
			log.Fatalf("seed: %v", err)
		}
	}

	// 5. Wire the Brain.
	b, err := hippo.New(
		hippo.WithProvider(p),
		hippo.WithRouter(r),
		hippo.WithMemory(mem),
		hippo.WithBudget(bud),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer b.Close()

	resp, err := b.Call(ctx, hippo.Call{
		Task:      hippo.TaskReason,
		Prompt:    "What should I work on next?",
		UseMemory: hippo.MemoryScope{Mode: hippo.MemoryScopeRecent},
		MaxTokens: 200,
	})
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("== Routed to:   %s / %s\n", resp.Provider, resp.Model)
	fmt.Printf("== Memory hits: %d records attached\n", len(resp.MemoryHits))
	for _, id := range resp.MemoryHits {
		fmt.Printf("    - %s\n", id)
	}
	fmt.Println()
	fmt.Println("== Response:")
	fmt.Println(resp.Text)
	fmt.Println()
	fmt.Printf("== Usage:       %d in / %d out / %d cached\n",
		resp.Usage.InputTokens, resp.Usage.OutputTokens, resp.Usage.CachedTokens)
	fmt.Printf("== Call cost:   $%.6f (from provider's pricing)\n", resp.CostUSD)
	fmt.Printf("== Budget:      $%.6f spent / $%.6f remaining\n", bud.Spent(), bud.Remaining())
	fmt.Printf("== Latency:     %dms\n", resp.LatencyMS)
}
