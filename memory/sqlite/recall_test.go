package sqlite

import (
	"context"
	"math"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

// stubEmbedder is a deterministic embedder for tests. Each word gets
// a fixed unit vector along one axis; similar words collide on the
// same axis; completely unseen words fall through to a tiny random
// (but repeatable) vector so cosine stays bounded.
type stubEmbedder struct {
	name     string
	dims     int
	axis     map[string]int
	called   atomic.Int64
}

func newStubEmbedder() *stubEmbedder {
	return &stubEmbedder{
		name: "stub:v1",
		dims: 8,
		axis: map[string]int{
			"cats":      0,
			"felines":   0,
			"kittens":   0,
			"dogs":      1,
			"puppies":   1,
			"billing":   2,
			"payments":  2,
			"invoices":  2,
			"kubernetes": 3,
			"docker":     3,
		},
	}
}

func (s *stubEmbedder) Name() string     { return s.name }
func (s *stubEmbedder) Dimensions() int  { return s.dims }

func (s *stubEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	s.called.Add(int64(len(texts)))
	out := make([][]float32, len(texts))
	for i, t := range texts {
		vec := make([]float32, s.dims)
		matched := false
		// Check each axis-word to see if the text contains it.
		for word, ax := range s.axis {
			if containsCI(t, word) {
				vec[ax] += 1
				matched = true
			}
		}
		if !matched {
			// Put a small component on the last axis so unrelated
			// records don't score identically (prevents ties from
			// masking test intent).
			vec[s.dims-1] = 0.1
		}
		// Normalize
		var norm float32
		for _, v := range vec {
			norm += v * v
		}
		norm = float32(math.Sqrt(float64(norm)))
		if norm > 0 {
			for j := range vec {
				vec[j] /= norm
			}
		}
		out[i] = vec
	}
	return out, nil
}

func containsCI(a, b string) bool {
	if len(a) < len(b) {
		return false
	}
	la := []rune(a)
	lb := []rune(b)
	for i := 0; i+len(lb) <= len(la); i++ {
		match := true
		for j := range lb {
			ac, bc := la[i+j], lb[j]
			if ac >= 'A' && ac <= 'Z' {
				ac += 'a' - 'A'
			}
			if bc >= 'A' && bc <= 'Z' {
				bc += 'a' - 'A'
			}
			if ac != bc {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// openMemWithEmbedder returns a file-backed store with the embedder
// attached. File-backed because :memory: doesn't survive MaxOpenConns=1
// concurrency the backfill test wants.
func openMemWithEmbedder(t *testing.T, emb hippo.Embedder) hippo.Memory {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mem.db")
	m, err := Open(path, WithEmbedder(emb))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// addEmbedded is a test helper that inserts a record with a pre-computed
// embedding so tests don't depend on the backfill worker.
func addEmbedded(t *testing.T, m hippo.Memory, emb hippo.Embedder, r hippo.Record) string {
	t.Helper()
	ctx := context.Background()
	vecs, err := emb.Embed(ctx, []string{r.Content})
	if err != nil {
		t.Fatal(err)
	}
	r.Embedding = vecs[0]
	if err := m.Add(ctx, &r); err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func TestRecallSemanticFindsNonKeywordMatch(t *testing.T) {
	emb := newStubEmbedder()
	mem := openMemWithEmbedder(t, emb)
	ctx := context.Background()
	base := time.Now()

	addEmbedded(t, mem, emb, hippo.Record{Kind: hippo.MemoryEpisodic, Content: "cats are independent", Timestamp: base})
	addEmbedded(t, mem, emb, hippo.Record{Kind: hippo.MemoryEpisodic, Content: "dogs like walks", Timestamp: base})
	addEmbedded(t, mem, emb, hippo.Record{Kind: hippo.MemoryEpisodic, Content: "invoices are due tomorrow", Timestamp: base})

	// "felines" maps to the same axis as "cats" via the stub embedder,
	// so semantic finds it even though FTS5 wouldn't.
	got, err := mem.Recall(ctx, "felines", hippo.MemoryQuery{Semantic: true, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Content != "cats are independent" {
		t.Fatalf("expected top hit 'cats are independent'; got %v", got)
	}
}

func TestRecallHybridWeightExtremes(t *testing.T) {
	emb := newStubEmbedder()
	mem := openMemWithEmbedder(t, emb)
	ctx := context.Background()

	// Exact keyword match on "billing" with a stale timestamp vs a
	// semantic-near neighbor ("payments") with a recent one.
	addEmbedded(t, mem, emb, hippo.Record{Kind: hippo.MemoryEpisodic, Content: "billing refactor", Timestamp: time.Now().Add(-1 * time.Hour)})
	addEmbedded(t, mem, emb, hippo.Record{Kind: hippo.MemoryEpisodic, Content: "payments module overhaul", Timestamp: time.Now()})

	// Pure-keyword: the exact word match wins.
	gotK, err := mem.Recall(ctx, "billing", hippo.MemoryQuery{
		Semantic: true, HybridWeight: 0.0, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotK) == 0 || gotK[0].Content != "billing refactor" {
		t.Errorf("keyword-only top hit = %+v; want 'billing refactor'", gotK)
	}

	// Pure-semantic: both records live on the same axis, so the one
	// with higher effective importance (more recent) wins.
	gotS, err := mem.Recall(ctx, "billing", hippo.MemoryQuery{
		Semantic: true, HybridWeight: 1.0, Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotS) != 2 {
		t.Fatalf("semantic-only: len=%d", len(gotS))
	}
}

func TestRecallSemanticWithoutEmbedderFallsBack(t *testing.T) {
	mem := openMem(t)
	ctx := context.Background()
	base := time.Now()
	if err := mem.Add(ctx, &hippo.Record{Kind: hippo.MemoryEpisodic, Content: "cats", Timestamp: base}); err != nil {
		t.Fatal(err)
	}
	// Semantic=true but no embedder configured: should fall through
	// to keyword (and still find an exact match).
	got, err := mem.Recall(ctx, "cats", hippo.MemoryQuery{Semantic: true, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("fallback got %d records; want 1", len(got))
	}
}

func TestRecallTemporalExpansionPullsNeighbors(t *testing.T) {
	emb := newStubEmbedder()
	mem := openMemWithEmbedder(t, emb)
	ctx := context.Background()
	base := time.Now()

	// Seed a semantic match plus three close-in-time neighbors that
	// wouldn't match on keyword or semantic.
	addEmbedded(t, mem, emb, hippo.Record{Kind: hippo.MemoryEpisodic, Content: "kubernetes upgrade plan", Timestamp: base})
	for i := 1; i <= 3; i++ {
		addEmbedded(t, mem, emb, hippo.Record{
			Kind:      hippo.MemoryEpisodic,
			Content:   "random chatter " + string(rune('A'+i-1)),
			Timestamp: base.Add(time.Duration(i*5) * time.Minute),
		})
	}
	// A clearly-outside-window neighbor we should NOT pull in.
	addEmbedded(t, mem, emb, hippo.Record{
		Kind:      hippo.MemoryEpisodic,
		Content:   "outside the window",
		Timestamp: base.Add(3 * time.Hour),
	})

	got, err := mem.Recall(ctx, "docker", hippo.MemoryQuery{
		Semantic: true, Limit: 5, TemporalExpansion: 30 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Expect the k8s hit plus 3 in-window neighbors, not the 3h-away one.
	var sawOutside bool
	for _, r := range got {
		if r.Content == "outside the window" {
			sawOutside = true
		}
	}
	if sawOutside {
		t.Errorf("temporal expansion pulled a record outside its window: %+v", got)
	}
	if len(got) < 2 {
		t.Errorf("expected semantic hit + neighbors, got %d: %v", len(got), got)
	}
}

func TestImportanceDecayWorkingKind(t *testing.T) {
	mem := openMem(t)
	ctx := context.Background()
	// 48h ago with Working half-life 24h → multiplier ≈ 0.25.
	err := mem.Add(ctx, &hippo.Record{
		Kind:       hippo.MemoryWorking,
		Content:    "decaying fact",
		Timestamp:  time.Now().Add(-48 * time.Hour),
		Importance: 1.0,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := mem.Recall(ctx, "", hippo.MemoryQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d records", len(got))
	}
	// Effective importance should be ~0.25. Allow broad slack since
	// access-count boost may push it up slightly after Recall.
	if got[0].Importance < 0.2 || got[0].Importance > 0.5 {
		t.Errorf("decayed importance = %v; want ~0.25", got[0].Importance)
	}
}

func TestImportanceDecayProfileKindUnchanged(t *testing.T) {
	mem := openMem(t)
	ctx := context.Background()
	err := mem.Add(ctx, &hippo.Record{
		Kind:       hippo.MemoryProfile,
		Content:    "profile fact",
		Timestamp:  time.Now().Add(-1000 * time.Hour),
		Importance: 0.8,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := mem.Recall(ctx, "", hippo.MemoryQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Importance < 0.79 || got[0].Importance > 1.0 {
		t.Errorf("profile importance = %v; want ~0.8 (no decay)", got[0].Importance)
	}
}

func TestMinImportanceFilterUsesEffective(t *testing.T) {
	mem := openMem(t)
	ctx := context.Background()
	base := time.Now()
	// Fresh high-base record survives the cutoff.
	if err := mem.Add(ctx, &hippo.Record{
		Kind: hippo.MemoryWorking, Content: "fresh", Timestamp: base, Importance: 0.9,
	}); err != nil {
		t.Fatal(err)
	}
	// Old Working record — base 0.9 but 72h ago; effective ≈ 0.9 * exp(-3) ≈ 0.045.
	if err := mem.Add(ctx, &hippo.Record{
		Kind: hippo.MemoryWorking, Content: "stale", Timestamp: base.Add(-72 * time.Hour), Importance: 0.9,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := mem.Recall(ctx, "", hippo.MemoryQuery{MinImportance: 0.5, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d; want 1 (only fresh survives)", len(got))
	}
	if got[0].Content != "fresh" {
		t.Errorf("wrong record survived: %+v", got[0])
	}
}

func TestAccessCountBoostsImportance(t *testing.T) {
	mem := openMem(t)
	ctx := context.Background()
	base := time.Now()
	_ = mem.Add(ctx, &hippo.Record{Kind: hippo.MemoryEpisodic, Content: "hot one", Timestamp: base, Importance: 0.5})
	_ = mem.Add(ctx, &hippo.Record{Kind: hippo.MemoryEpisodic, Content: "cold one", Timestamp: base, Importance: 0.5})

	// Pound on "hot one" a few times so access_count climbs.
	for i := 0; i < 10; i++ {
		if _, err := mem.Recall(ctx, "hot", hippo.MemoryQuery{Limit: 1}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := mem.Recall(ctx, "", hippo.MemoryQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d", len(got))
	}
	var hot, cold hippo.Record
	for _, r := range got {
		if r.Content == "hot one" {
			hot = r
		} else if r.Content == "cold one" {
			cold = r
		}
	}
	if hot.Importance <= cold.Importance {
		t.Errorf("access boost failed: hot=%v cold=%v", hot.Importance, cold.Importance)
	}
}
