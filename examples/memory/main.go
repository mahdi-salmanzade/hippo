// Command memory demonstrates hippo's SQLite-backed memory store in
// isolation — no provider, no LLM, no network. It opens a temporary
// database, writes a handful of records across the three kinds, then
// recalls them by keyword and by recency.
//
// Run with:
//
//	go run ./examples/memory
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/internal/dotenv"
	"github.com/mahdi-salmanzade/hippo/memory/sqlite"
)

func main() {
	// .env isn't required for this example (no provider), but we
	// still call Load so future composite examples that mix memory
	// and LLMs pick up the same workflow.
	_ = dotenv.Load()

	tmp, err := os.MkdirTemp("", "hippo-memory-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	dbPath := filepath.Join(tmp, "hippo.db")
	mem, err := sqlite.Open(dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer mem.Close()

	ctx := context.Background()
	now := time.Now()

	seed := []hippo.Record{
		{
			Kind:       hippo.MemoryProfile,
			Timestamp:  now.Add(-30 * 24 * time.Hour),
			Content:    "User prefers concise, one-paragraph answers.",
			Tags:       []string{"preference", "style"},
			Importance: 0.9,
		},
		{
			Kind:       hippo.MemoryEpisodic,
			Timestamp:  now.Add(-2 * time.Hour),
			Content:    "Spent 2h refactoring the billing module. Tests still failing.",
			Tags:       []string{"billing", "refactor", "wip"},
			Importance: 0.7,
		},
		{
			Kind:       hippo.MemoryEpisodic,
			Timestamp:  now.Add(-1 * time.Hour),
			Content:    "Skipped lunch to finish the deploy. Successful push to staging.",
			Tags:       []string{"deploy", "ops"},
			Importance: 0.4,
		},
		{
			Kind:       hippo.MemoryWorking,
			Timestamp:  now.Add(-10 * time.Minute),
			Content:    "Currently debugging a race condition in the billing cache eviction.",
			Tags:       []string{"billing", "debug", "wip"},
			Importance: 0.6,
		},
		{
			Kind:       hippo.MemoryWorking,
			Timestamp:  now.Add(-2 * time.Minute),
			Content:    "Thinking about taking a coffee break.",
			Tags:       []string{"personal"},
			Importance: 0.1,
		},
	}
	for i := range seed {
		if err := mem.Add(ctx, &seed[i]); err != nil {
			log.Fatalf("Add[%d]: %v", i, err)
		}
	}

	fmt.Println("== Seeded memory store at", dbPath)
	fmt.Printf("   %d records written across working/episodic/profile\n\n", len(seed))

	// 1. Keyword FTS5 recall — should surface both billing records.
	fmt.Println("== Recall: keyword \"billing\"")
	results, err := mem.Recall(ctx, "billing", hippo.MemoryQuery{Limit: 5})
	if err != nil {
		log.Fatal(err)
	}
	printResults(results)

	// 2. Tag + kind filter — WIP items, episodic or working.
	fmt.Println("== Recall: tag=wip, kind in {working, episodic}")
	results, err = mem.Recall(ctx, "", hippo.MemoryQuery{
		Tags:  []string{"wip"},
		Kinds: []hippo.MemoryKind{hippo.MemoryWorking, hippo.MemoryEpisodic},
		Limit: 5,
	})
	if err != nil {
		log.Fatal(err)
	}
	printResults(results)

	// 3. Importance filter — only records with importance >= 0.5.
	fmt.Println("== Recall: importance >= 0.5")
	results, err = mem.Recall(ctx, "", hippo.MemoryQuery{
		MinImportance: 0.5,
		Limit:         5,
	})
	if err != nil {
		log.Fatal(err)
	}
	printResults(results)

	// 4. Pure recency — no query, no filters.
	fmt.Println("== Recall: all, newest first")
	results, err = mem.Recall(ctx, "", hippo.MemoryQuery{Limit: 5})
	if err != nil {
		log.Fatal(err)
	}
	printResults(results)
}

func printResults(rs []hippo.Record) {
	if len(rs) == 0 {
		fmt.Println("   (no results)")
		fmt.Println()
		return
	}
	for _, r := range rs {
		fmt.Printf("   [%-8s imp=%.2f] %s\n", r.Kind, r.Importance, r.Content)
		if len(r.Tags) > 0 {
			fmt.Printf("             tags=%v\n", r.Tags)
		}
	}
	fmt.Println()
}
