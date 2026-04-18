// Command memory demonstrates hippo's memory-aware mode: persist a fact,
// then issue a follow-up Call that pulls it back in via UseMemory.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/memory"
	memsqlite "github.com/mahdi-salmanzade/hippo/memory/sqlite"
	"github.com/mahdi-salmanzade/hippo/providers/anthropic"
)

func main() {
	store, err := memsqlite.Open("hippo.db")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	b, err := hippo.New(
		hippo.WithProvider(anthropic.New(os.Getenv("ANTHROPIC_API_KEY"))),
		hippo.WithMemory(store),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer b.Close()

	ctx := context.Background()

	_ = store.Add(ctx, &memory.Record{
		Kind:       memory.Profile,
		Timestamp:  time.Now(),
		Content:    "User prefers concise, one-paragraph answers.",
		Tags:       []string{"preference", "style"},
		Importance: 0.9,
	})

	resp, err := b.Call(ctx, hippo.Call{
		Task:      hippo.TaskGenerate,
		Prompt:    "Explain the CAP theorem.",
		UseMemory: hippo.MemoryScope{Mode: hippo.MemoryScopeFull},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(resp.Text)
	fmt.Printf("memory hits: %v\n", resp.MemoryHits)
}
