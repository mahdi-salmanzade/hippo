// Command semantic demonstrates hippo's v0.2 semantic memory: seeds a
// small knowledge base, runs keyword / semantic / hybrid queries
// against it, and shows how nucleus temporal expansion pulls
// conversation-adjacent turns into the result set.
//
// Requires a reachable Ollama daemon with an embedding model pulled.
// The default model is nomic-embed-text; override with --model.
//
// Usage:
//
//	ollama pull nomic-embed-text
//	go run ./examples/semantic
//
// No cloud keys required — everything runs locally against Ollama.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/memory/sqlite"
	"github.com/mahdi-salmanzade/hippo/providers/ollama"
)

func main() {
	baseURL := flag.String("base-url", "http://localhost:11434", "Ollama endpoint")
	model := flag.String("model", ollama.DefaultEmbeddingModel, "embedding model id")
	flag.Parse()

	emb := ollama.NewEmbedder(
		ollama.WithEmbedderBaseURL(*baseURL),
		ollama.WithEmbedderModel(*model),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Probe the embedder so we fail fast with a readable error rather
	// than running the seeding script against a dead daemon.
	if _, err := emb.Embed(ctx, []string{"probe"}); err != nil {
		log.Fatalf("ollama embed probe: %v\n(hint: ollama pull %s)", err, *model)
	}
	fmt.Printf("embedder: %s (dims=%d)\n", emb.Name(), emb.Dimensions())

	dir, err := os.MkdirTemp("", "hippo-semantic-*")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)
	dbPath := dir + "/mem.db"

	mem, err := sqlite.Open(dbPath, sqlite.WithEmbedder(emb))
	if err != nil {
		log.Fatal(err)
	}
	defer mem.Close()

	// Seed 20 records across three loosely-related topical clusters
	// with interleaved timestamps so temporal expansion has something
	// to pull in.
	seeds := seedRecords()
	for i := range seeds {
		if err := mem.Add(ctx, &seeds[i]); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Printf("seeded %d records\n", len(seeds))

	// Kick the backfill worker and wait until the backlog drains.
	bf := mem.(interface {
		StartBackfill(context.Context, sqlite.BackfillConfig) (func(), error)
		BackfillStatus(context.Context) (sqlite.BackfillStatus, error)
	})
	stop, err := bf.StartBackfill(ctx, sqlite.BackfillConfig{
		Embedder:  emb,
		BatchSize: 5,
		Interval:  50 * time.Millisecond,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer stop()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		st, _ := bf.BackfillStatus(ctx)
		if st.Pending == 0 && st.Embedded == int64(len(seeds)) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	st, _ := bf.BackfillStatus(ctx)
	fmt.Printf("backfill done: %d/%d embedded\n\n", st.Embedded, st.Total)

	// Run three queries through three modes so the difference in
	// retrieval is visible.
	queries := []struct {
		text string
		note string
	}{
		{"kubernetes upgrade timeline", "multi-word — keyword OR-matches 'kubernetes' and 'upgrade'"},
		{"feline companions", "semantic-only win — no token overlap with 'cats'/'kittens'"},
		{"invoice processing", "billing adjacency — hybrid + temporal expansion shine"},
	}

	for _, q := range queries {
		fmt.Printf("=== query: %q (%s) ===\n", q.text, q.note)
		for _, mode := range []struct {
			label string
			query hippo.MemoryQuery
		}{
			{"keyword", hippo.MemoryQuery{Limit: 3}},
			{"semantic", hippo.MemoryQuery{Semantic: true, Limit: 3}},
			{"hybrid+nucleus", hippo.MemoryQuery{
				Semantic:          true,
				HybridWeight:      0.6,
				TemporalExpansion: 30 * time.Minute,
				Limit:             5,
			}},
		} {
			recs, err := mem.Recall(ctx, q.text, mode.query)
			if err != nil {
				log.Printf("%s: %v", mode.label, err)
				continue
			}
			fmt.Printf("  %-15s  %d hits\n", mode.label, len(recs))
			for _, r := range recs {
				preview := r.Content
				if len(preview) > 70 {
					preview = preview[:70] + "…"
				}
				fmt.Printf("    - %s  [imp=%.2f]  %s\n", r.Timestamp.Format("15:04"), r.Importance, preview)
			}
		}
		fmt.Println()
	}
}

// seedRecords returns 20 episodic records about three topical clusters:
// kubernetes ops, cat behaviour, and billing. Timestamps interleave so
// temporal expansion finds adjacent "other topic" rows sometimes.
func seedRecords() []hippo.Record {
	base := time.Now().Add(-2 * time.Hour)
	step := 6 * time.Minute
	contents := []string{
		"Bumped the production kubernetes cluster to 1.30 last night.",
		"Noticed the orange cat napping on the keyboard again.",
		"Issued refund for invoice #4812, customer was happy.",
		"Rollback plan for the k8s upgrade uses stork snapshots.",
		"Kittens love playing with the string on the chair.",
		"Need to audit the billing reconciliation report for March.",
		"Helm chart for the api service lags by two minor versions.",
		"Felines in the office tend to pick one human to follow around.",
		"Payments cron job re-ran after the DST change broke the schedule.",
		"kube-proxy got restarted twice today — probably connection drain tuning.",
		"The tabby is chasing shadows on the wall this afternoon.",
		"Billing dashboard still shows the old MRR calculation formula.",
		"etcd metrics spiked during the 1.30 upgrade but recovered.",
		"Cats sleep 12-16 hours a day on average; ours hits 18.",
		"Invoice #4901 is stuck in review, not sure who the owner is.",
		"Cluster autoscaler evicted a finance pod during rebalance.",
		"The black kitten learned to open the pantry door somehow.",
		"Payments reconciliation matches for April, ready for review.",
		"Post-upgrade smoke tests on kubernetes: all nodes Ready.",
		"The calico cat is the unofficial ops mascot now.",
	}
	out := make([]hippo.Record, len(contents))
	for i, c := range contents {
		out[i] = hippo.Record{
			Kind:       hippo.MemoryEpisodic,
			Content:    c,
			Timestamp:  base.Add(time.Duration(i) * step),
			Importance: 0.5,
		}
	}
	return out
}
