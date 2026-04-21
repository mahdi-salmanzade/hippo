package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

// openFileMem returns a file-backed store (not :memory:) so concurrent
// connections can all see inserted rows - needed for the backfill
// race test.
func openFileMem(t *testing.T, opts ...Option) *store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mem.db")
	m, err := Open(path, opts...)
	if err != nil {
		t.Fatal(err)
	}
	s := m.(*store)
	t.Cleanup(func() { _ = m.Close() })
	return s
}

func TestBackfillFillsMissingEmbeddings(t *testing.T) {
	emb := newStubEmbedder()
	s := openFileMem(t, WithEmbedder(emb))
	ctx := context.Background()

	// Seed records without embeddings.
	for i := 0; i < 5; i++ {
		if err := s.Add(ctx, &hippo.Record{
			Kind:    hippo.MemoryEpisodic,
			Content: fmt.Sprintf("billing turn %d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	stop, err := s.StartBackfill(ctx, BackfillConfig{
		Embedder: emb,
		Interval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// Wait up to 2s for the worker to catch up.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, err := s.BackfillStatus(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if st.Pending == 0 && st.Embedded == 5 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	st, _ := s.BackfillStatus(ctx)
	t.Fatalf("backfill did not complete: pending=%d embedded=%d last_error=%q",
		st.Pending, st.Embedded, st.LastError)
}

func TestBackfillConcurrentRecall(t *testing.T) {
	emb := newStubEmbedder()
	s := openFileMem(t, WithEmbedder(emb))
	ctx := context.Background()

	for i := 0; i < 30; i++ {
		if err := s.Add(ctx, &hippo.Record{
			Kind:    hippo.MemoryEpisodic,
			Content: fmt.Sprintf("cats in scenario %d", i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	stop, err := s.StartBackfill(ctx, BackfillConfig{
		Embedder:  emb,
		Interval:  1 * time.Millisecond,
		BatchSize: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	// Hammer Recall while the worker writes embeddings. With -race
	// enabled this catches any missing synchronisation.
	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				if _, err := s.Recall(ctx, "cats", hippo.MemoryQuery{Semantic: true, Limit: 5}); err != nil {
					t.Errorf("recall: %v", err)
					return
				}
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	close(done)
	wg.Wait()
}

func TestAutoPruneDeletesOldWorking(t *testing.T) {
	s := openFileMem(t)
	ctx := context.Background()

	// Old Working record - should be deleted by the rule.
	_ = s.Add(ctx, &hippo.Record{
		Kind: hippo.MemoryWorking, Content: "expired", Timestamp: time.Now().Add(-30 * 24 * time.Hour),
	})
	// Fresh one - survives.
	_ = s.Add(ctx, &hippo.Record{
		Kind: hippo.MemoryWorking, Content: "fresh", Timestamp: time.Now(),
	})

	if err := s.pruneOnce(ctx, PruneConfig{WorkingMaxAge: 7 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Recall(ctx, "", hippo.MemoryQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Content != "fresh" {
		t.Errorf("expected only 'fresh' to remain: %+v", rows)
	}
}

// TestAutoPrunePreservesImportantEpisodic verifies that the effective-
// importance cutoff admits records whose (base × decay) stays above
// the threshold. The tight-age window (110 days, just past the 90-day
// MaxAge but inside two half-lives) is deliberate so the base
// importance still dominates.
func TestAutoPrunePreservesImportantEpisodic(t *testing.T) {
	s := openFileMem(t)
	ctx := context.Background()
	// Age: 110 days. Episodic half-life 30 days → three ish half-lives
	// → decay multiplier ≈ 0.08. So base importance of 0.99 survives
	// the 0.01 cutoff (effective ≈ 0.08 > 0.01) while 0.1 does not.
	oldDate := time.Now().Add(-110 * 24 * time.Hour)
	_ = s.Add(ctx, &hippo.Record{
		Kind: hippo.MemoryEpisodic, Content: "lowimp", Timestamp: oldDate, Importance: 0.1,
	})
	_ = s.Add(ctx, &hippo.Record{
		Kind: hippo.MemoryEpisodic, Content: "highimp", Timestamp: oldDate, Importance: 0.99,
	})

	if err := s.pruneOnce(ctx, PruneConfig{
		EpisodicMaxAge:           90 * 24 * time.Hour,
		EpisodicImportanceCutoff: 0.01,
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := s.Recall(ctx, "", hippo.MemoryQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Content != "highimp" {
		t.Errorf("expected only 'highimp' to survive; got %+v", rows)
	}
}

func TestAutoPruneSkipsProfile(t *testing.T) {
	s := openFileMem(t)
	ctx := context.Background()
	_ = s.Add(ctx, &hippo.Record{
		Kind: hippo.MemoryProfile, Content: "user handle", Timestamp: time.Now().Add(-365 * 24 * time.Hour),
	})
	if err := s.pruneOnce(ctx, PruneConfig{
		WorkingMaxAge:            1 * time.Hour,
		EpisodicMaxAge:           1 * time.Hour,
		EpisodicImportanceCutoff: 1.0,
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Recall(ctx, "", hippo.MemoryQuery{Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("Profile should have survived; got %d rows", len(rows))
	}
}

func TestAutoPruneIsIdempotent(t *testing.T) {
	s := openFileMem(t)
	ctx := context.Background()
	_ = s.Add(ctx, &hippo.Record{
		Kind: hippo.MemoryWorking, Content: "expired", Timestamp: time.Now().Add(-30 * 24 * time.Hour),
	})
	for i := 0; i < 3; i++ {
		if err := s.pruneOnce(ctx, PruneConfig{WorkingMaxAge: 7 * 24 * time.Hour}); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	rows, _ := s.Recall(ctx, "", hippo.MemoryQuery{Limit: 5})
	if len(rows) != 0 {
		t.Errorf("expected 0 rows; got %d", len(rows))
	}
}
