package sqlite

import (
	"context"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

// openMem returns a fresh :memory: store, closed automatically on
// test teardown. Every test starts with a clean schema.
func openMem(t *testing.T) hippo.Memory {
	t.Helper()
	// Use a unique DSN fragment per test so sql.DB's connection pool
	// doesn't accidentally share state across parallel tests (modernc
	// treats each :memory: URI as a separate database per connection
	// otherwise, which breaks schema setup for the first query).
	mem, err := Open(":memory:", WithMaxOpenConns(1))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	return mem
}

func TestOpenInMemory(t *testing.T) {
	mem := openMem(t)
	if mem == nil {
		t.Fatal("Open returned nil store")
	}
	// Second Close should be idempotent.
	if err := mem.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := mem.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestAddAssignsID(t *testing.T) {
	mem := openMem(t)
	rec := &hippo.Record{
		Kind:    hippo.MemoryEpisodic,
		Content: "auto id please",
	}
	if err := mem.Add(context.Background(), rec); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(rec.ID) != 26 {
		t.Errorf("expected 26-char ULID, got %q (len %d)", rec.ID, len(rec.ID))
	}
	// Crockford Base32: uppercase alphanumerics minus I, L, O, U.
	valid := regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	if !valid.MatchString(rec.ID) {
		t.Errorf("id %q not Crockford Base32", rec.ID)
	}
	if rec.Timestamp.IsZero() {
		t.Error("Timestamp should have been defaulted to now")
	}
}

func TestAddRejectsEmptyContent(t *testing.T) {
	mem := openMem(t)
	err := mem.Add(context.Background(), &hippo.Record{Content: ""})
	if err == nil {
		t.Fatal("expected error on empty content, got nil")
	}
}

func TestAddClampsImportance(t *testing.T) {
	mem := openMem(t)
	ctx := context.Background()

	high := &hippo.Record{Kind: hippo.MemoryEpisodic, Content: "too high", Importance: 2.0}
	if err := mem.Add(ctx, high); err != nil {
		t.Fatal(err)
	}
	if high.Importance != 1.0 {
		t.Errorf("importance 2.0 not clamped to 1.0, got %v", high.Importance)
	}

	low := &hippo.Record{Kind: hippo.MemoryEpisodic, Content: "too low", Importance: -0.5}
	if err := mem.Add(ctx, low); err != nil {
		t.Fatal(err)
	}
	if low.Importance != 0.0 {
		t.Errorf("importance -0.5 not clamped to 0.0, got %v", low.Importance)
	}
}

func TestRecallByKeyword(t *testing.T) {
	mem := openMem(t)
	ctx := context.Background()
	seed := []hippo.Record{
		{Kind: hippo.MemoryEpisodic, Content: "refactoring the billing module today"},
		{Kind: hippo.MemoryEpisodic, Content: "coffee meeting with Sam"},
		{Kind: hippo.MemoryEpisodic, Content: "billing tests still failing after refactor"},
	}
	for i := range seed {
		if err := mem.Add(ctx, &seed[i]); err != nil {
			t.Fatalf("Add[%d]: %v", i, err)
		}
	}

	got, err := mem.Recall(ctx, "billing refactor", hippo.MemoryQuery{})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 matches for \"billing refactor\", got %d", len(got))
	}
	// Both matches should mention billing AND (refactor OR refactoring).
	for _, r := range got {
		c := r.Content
		if !(regexp.MustCompile(`(?i)billing`).MatchString(c) &&
			regexp.MustCompile(`(?i)refactor`).MatchString(c)) {
			t.Errorf("unexpected match: %q", c)
		}
	}
}

func TestRecallByKind(t *testing.T) {
	mem := openMem(t)
	ctx := context.Background()
	for _, r := range []hippo.Record{
		{Kind: hippo.MemoryWorking, Content: "w1"},
		{Kind: hippo.MemoryEpisodic, Content: "e1"},
		{Kind: hippo.MemoryProfile, Content: "p1"},
	} {
		rec := r
		if err := mem.Add(ctx, &rec); err != nil {
			t.Fatal(err)
		}
	}

	got, err := mem.Recall(ctx, "", hippo.MemoryQuery{
		Kinds: []hippo.MemoryKind{hippo.MemoryProfile},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "p1" {
		t.Errorf("want [p1], got %v", got)
	}
}

func TestRecallByTags(t *testing.T) {
	mem := openMem(t)
	ctx := context.Background()
	for _, r := range []hippo.Record{
		{Kind: hippo.MemoryEpisodic, Content: "billing work", Tags: []string{"billing", "wip"}},
		{Kind: hippo.MemoryEpisodic, Content: "coffee chat", Tags: []string{"social"}},
		{Kind: hippo.MemoryEpisodic, Content: "deploy prep", Tags: []string{"deploy", "wip"}},
	} {
		rec := r
		if err := mem.Add(ctx, &rec); err != nil {
			t.Fatal(err)
		}
	}

	got, err := mem.Recall(ctx, "", hippo.MemoryQuery{
		Tags: []string{"wip"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results with tag wip, got %d", len(got))
	}
	for _, r := range got {
		found := false
		for _, tag := range r.Tags {
			if tag == "wip" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("result %q missing wip tag; got tags=%v", r.Content, r.Tags)
		}
	}
}

func TestRecallByTimeRange(t *testing.T) {
	mem := openMem(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, delta := range []time.Duration{-3 * 24 * time.Hour, -24 * time.Hour, 24 * time.Hour} {
		rec := hippo.Record{
			Kind:      hippo.MemoryEpisodic,
			Timestamp: base.Add(delta),
			Content:   string(rune('a' + i)),
		}
		if err := mem.Add(ctx, &rec); err != nil {
			t.Fatal(err)
		}
	}

	got, err := mem.Recall(ctx, "", hippo.MemoryQuery{
		Since: base.Add(-2 * 24 * time.Hour),
		Until: base.Add(12 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "b" {
		t.Errorf("want [b], got %v", got)
	}
}

func TestRecallByImportance(t *testing.T) {
	mem := openMem(t)
	ctx := context.Background()
	for i, imp := range []float64{0.2, 0.6, 0.9} {
		rec := hippo.Record{Kind: hippo.MemoryEpisodic, Content: string(rune('a' + i)), Importance: imp}
		if err := mem.Add(ctx, &rec); err != nil {
			t.Fatal(err)
		}
	}
	got, err := mem.Recall(ctx, "", hippo.MemoryQuery{MinImportance: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 results with importance >= 0.5, got %d", len(got))
	}
	for _, r := range got {
		if r.Importance < 0.5 {
			t.Errorf("got record with importance %v below threshold", r.Importance)
		}
	}
}

func TestRecallEmptyQueryReturnsByRecency(t *testing.T) {
	mem := openMem(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Seed in non-chronological order to prove ordering is by timestamp.
	for i, delta := range []time.Duration{-10 * time.Minute, -30 * time.Minute, -1 * time.Minute} {
		rec := hippo.Record{
			Kind:      hippo.MemoryEpisodic,
			Timestamp: base.Add(delta),
			Content:   string(rune('a' + i)),
		}
		if err := mem.Add(ctx, &rec); err != nil {
			t.Fatal(err)
		}
	}
	got, err := mem.Recall(ctx, "", hippo.MemoryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 records, got %d", len(got))
	}
	// Expected order newest first: c, a, b.
	want := []string{"c", "a", "b"}
	for i, r := range got {
		if r.Content != want[i] {
			t.Errorf("pos %d: got %q, want %q", i, r.Content, want[i])
		}
	}
}

func TestRecallCombinesFilters(t *testing.T) {
	mem := openMem(t)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, r := range []hippo.Record{
		{Kind: hippo.MemoryWorking, Content: "billing wip", Tags: []string{"wip"}, Timestamp: base.Add(-1 * time.Hour), Importance: 0.9},
		{Kind: hippo.MemoryEpisodic, Content: "billing wip", Tags: []string{"wip"}, Timestamp: base.Add(-1 * time.Hour), Importance: 0.9}, // wrong kind
		{Kind: hippo.MemoryWorking, Content: "billing wip", Tags: []string{"other"}, Timestamp: base.Add(-1 * time.Hour), Importance: 0.9},  // wrong tag
		{Kind: hippo.MemoryWorking, Content: "billing wip", Tags: []string{"wip"}, Timestamp: base.Add(-48 * time.Hour), Importance: 0.9}, // too old
		{Kind: hippo.MemoryWorking, Content: "billing wip", Tags: []string{"wip"}, Timestamp: base.Add(-1 * time.Hour), Importance: 0.2},  // low importance
	} {
		rec := r
		if err := mem.Add(ctx, &rec); err != nil {
			t.Fatal(err)
		}
	}

	got, err := mem.Recall(ctx, "billing", hippo.MemoryQuery{
		Kinds:         []hippo.MemoryKind{hippo.MemoryWorking},
		Tags:          []string{"wip"},
		Since:         base.Add(-24 * time.Hour),
		MinImportance: 0.5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 record matching all filters, got %d", len(got))
	}
}

func TestPruneRemovesOldWorkingOnly(t *testing.T) {
	mem := openMem(t)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	recent := time.Now()

	for _, r := range []hippo.Record{
		{Kind: hippo.MemoryWorking, Content: "w-old", Timestamp: old},
		{Kind: hippo.MemoryWorking, Content: "w-new", Timestamp: recent},
		{Kind: hippo.MemoryEpisodic, Content: "e-old", Timestamp: old},
		{Kind: hippo.MemoryEpisodic, Content: "e-new", Timestamp: recent},
	} {
		rec := r
		if err := mem.Add(ctx, &rec); err != nil {
			t.Fatal(err)
		}
	}

	cutoff := time.Now().Add(-1 * time.Hour)
	if err := mem.Prune(ctx, cutoff); err != nil {
		t.Fatal(err)
	}

	got, err := mem.Recall(ctx, "", hippo.MemoryQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"w-new": false, "e-old": false, "e-new": false}
	for _, r := range got {
		if _, ok := want[r.Content]; !ok {
			t.Errorf("unexpected record survived prune: %q", r.Content)
			continue
		}
		want[r.Content] = true
	}
	for c, seen := range want {
		if !seen {
			t.Errorf("expected %q to survive prune but it's gone", c)
		}
	}
}

func TestConcurrentReads(t *testing.T) {
	// WAL mode needs a real file for concurrent readers to be
	// meaningful; a :memory: database is per-connection in modernc's
	// driver, so the test server would see an empty schema on any
	// connection other than the one that created it.
	path := t.TempDir() + "/hippo_test.db"
	mem, err := Open(path, WithMaxOpenConns(8))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })

	ctx := context.Background()
	for i := 0; i < 20; i++ {
		rec := hippo.Record{Kind: hippo.MemoryEpisodic, Content: "doc number " + string(rune('a'+i%26))}
		if err := mem.Add(ctx, &rec); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := mem.Recall(ctx, "doc", hippo.MemoryQuery{Limit: 20})
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Recall: %v", err)
	}
}

func TestFTSRebuildOnUpdate(t *testing.T) {
	mem := openMem(t).(*store)
	ctx := context.Background()
	rec := &hippo.Record{Kind: hippo.MemoryEpisodic, Content: "original text here"}
	if err := mem.Add(ctx, rec); err != nil {
		t.Fatal(err)
	}

	// Simulate an out-of-band content update (no high-level API for
	// mutating content yet; the memories_au trigger is what keeps FTS
	// in sync, so we drive the test against raw SQL).
	_, err := mem.db.ExecContext(ctx,
		`UPDATE memories SET content = ? WHERE id = ?`,
		"entirely different words", rec.ID)
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	oldHits, err := mem.Recall(ctx, "original", hippo.MemoryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(oldHits) != 0 {
		t.Errorf("expected 0 hits for pre-update term, got %d", len(oldHits))
	}

	newHits, err := mem.Recall(ctx, "different words", hippo.MemoryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(newHits) != 1 {
		t.Errorf("expected 1 hit for post-update term, got %d", len(newHits))
	}
}

func TestAddNilRecordReturnsError(t *testing.T) {
	mem := openMem(t)
	if err := mem.Add(context.Background(), nil); err == nil {
		t.Fatal("expected error on nil record")
	}
}

func TestPruneRejectsZeroTime(t *testing.T) {
	mem := openMem(t)
	err := mem.Prune(context.Background(), time.Time{})
	if err == nil {
		t.Fatal("expected error on zero time")
	}
}
