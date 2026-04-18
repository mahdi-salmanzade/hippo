package hippo

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- test doubles ---------------------------------------------------

// fakeProvider is a minimal hippo.Provider for brain-level tests.
// Real provider tests live in the provider subpackages; this struct
// exists only to let the Brain flow be tested without a network.
type fakeProvider struct {
	name     string
	privacy  PrivacyTier
	cost     float64
	resp     *Response
	err      error
	seenCall Call
	calls    int
	mu       sync.Mutex
}

func (f *fakeProvider) Name() string                               { return f.name }
func (f *fakeProvider) Models() []ModelInfo                        { return nil }
func (f *fakeProvider) Privacy() PrivacyTier                       { return f.privacy }
func (f *fakeProvider) EstimateCost(Call) (float64, error)         { return f.cost, nil }
func (f *fakeProvider) Call(ctx context.Context, c Call) (*Response, error) {
	f.mu.Lock()
	f.seenCall = c
	f.calls++
	resp, err := f.resp, f.err
	f.mu.Unlock()
	if resp == nil && err == nil {
		resp = &Response{}
	}
	return resp, err
}
func (f *fakeProvider) Stream(ctx context.Context, c Call) (<-chan StreamChunk, error) {
	return nil, ErrNotImplemented
}

// fakeMemory is a tiny in-slice Memory. Enough for brain tests;
// real backend tests live in memory/sqlite.
type fakeMemory struct {
	mu         sync.Mutex
	recallHits []Record
	recallErr  error
	added      []Record
}

func (f *fakeMemory) Add(ctx context.Context, rec *Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if rec.ID == "" {
		rec.ID = "fake-id"
	}
	f.added = append(f.added, *rec)
	return nil
}
func (f *fakeMemory) Recall(ctx context.Context, query string, q MemoryQuery) ([]Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Record(nil), f.recallHits...), f.recallErr
}
func (f *fakeMemory) Prune(ctx context.Context, before time.Time) error { return nil }
func (f *fakeMemory) Close() error                                      { return nil }

// fakeBudget is a deterministic BudgetTracker for brain tests.
type fakeBudget struct {
	mu        sync.Mutex
	remaining float64
	spent     float64
	charges   []chargeEvent
}

type chargeEvent struct {
	Provider string
	Model    string
	Usage    Usage
}

func (b *fakeBudget) EstimateCost(provider, model string, usage Usage) (float64, error) {
	return 0, nil
}
func (b *fakeBudget) Charge(provider, model string, usage Usage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.charges = append(b.charges, chargeEvent{provider, model, usage})
	b.spent += 0.001 // any positive number for test visibility
	b.remaining -= 0.001
	return nil
}
func (b *fakeBudget) Remaining() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.remaining
}
func (b *fakeBudget) Spent() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

// fakeRouter always returns a pre-set Decision.
type fakeRouter struct {
	decision Decision
	err      error
	calls    int
}

func (r *fakeRouter) Name() string { return "fake" }
func (r *fakeRouter) Route(ctx context.Context, c Call, providers []Provider, budget float64) (Decision, error) {
	r.calls++
	return r.decision, r.err
}

// --- tests ----------------------------------------------------------

func TestBrainCallNoProvider(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = b.Call(context.Background(), Call{Prompt: "hi"})
	if !errors.Is(err, ErrNoProviderAvailable) {
		t.Errorf("err = %v, want ErrNoProviderAvailable", err)
	}
}

func TestBrainCallDelegates(t *testing.T) {
	want := &Response{Text: "ok", Provider: "fake"}
	fp := &fakeProvider{name: "fake", privacy: PrivacyCloudOK, resp: want}
	b, _ := New(WithProvider(fp))

	got, err := b.Call(context.Background(), Call{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got != want {
		t.Errorf("Call returned %v, want %v", got, want)
	}
	if fp.calls != 1 {
		t.Errorf("provider.Call count = %d, want 1", fp.calls)
	}
}

func TestBrainCallPrivacyViolation(t *testing.T) {
	fp := &fakeProvider{name: "cloud", privacy: PrivacyCloudOK, resp: &Response{}}
	b, _ := New(WithProvider(fp))

	_, err := b.Call(context.Background(), Call{Prompt: "hi", Privacy: PrivacyLocalOnly})
	if !errors.Is(err, ErrPrivacyViolation) {
		t.Errorf("err = %v, want ErrPrivacyViolation", err)
	}
	if fp.calls != 0 {
		t.Errorf("provider.Call called %d times despite privacy mismatch, want 0", fp.calls)
	}
}

func TestBrainStreamReturnsNotImplemented(t *testing.T) {
	fp := &fakeProvider{name: "fake", privacy: PrivacyCloudOK}
	b, _ := New(WithProvider(fp))
	ch, err := b.Stream(context.Background(), Call{Prompt: "hi"})
	if ch != nil {
		t.Error("Stream returned non-nil channel, want nil")
	}
	if !errors.Is(err, ErrNotImplemented) {
		t.Errorf("err = %v, want ErrNotImplemented", err)
	}
}

func TestCallRoutesThroughPolicy(t *testing.T) {
	pA := &fakeProvider{name: "pA", privacy: PrivacyCloudOK, resp: &Response{Text: "A"}}
	pB := &fakeProvider{name: "pB", privacy: PrivacyCloudOK, resp: &Response{Text: "B"}}
	r := &fakeRouter{decision: Decision{Provider: "pB", Model: "modelB", EstimatedCostUSD: 0.01}}

	b, _ := New(
		WithProvider(pA),
		WithProvider(pB),
		WithRouter(r),
	)
	resp, err := b.Call(context.Background(), Call{Task: TaskGenerate, Prompt: "hi"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Text != "B" {
		t.Errorf("resp.Text = %q, want B (router picked pB)", resp.Text)
	}
	if pA.calls != 0 {
		t.Errorf("pA was called %d times, want 0 (router picked pB)", pA.calls)
	}
	if pB.calls != 1 {
		t.Errorf("pB.calls = %d, want 1", pB.calls)
	}
	if r.calls != 1 {
		t.Errorf("router.Route invoked %d times, want 1", r.calls)
	}
	// The Brain should have pinned the model for the provider.
	if pB.seenCall.Model != "modelB" {
		t.Errorf("provider saw Model=%q, want modelB", pB.seenCall.Model)
	}
}

func TestCallRespectsBudgetExceeded(t *testing.T) {
	fp := &fakeProvider{name: "fake", privacy: PrivacyCloudOK, resp: &Response{}}
	r := &fakeRouter{decision: Decision{Provider: "fake", Model: "m", EstimatedCostUSD: 1.00}}
	budget := &fakeBudget{remaining: 0.50}

	b, _ := New(
		WithProvider(fp),
		WithRouter(r),
		WithBudget(budget),
	)
	_, err := b.Call(context.Background(), Call{Task: TaskGenerate})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("err = %v, want wrapping ErrBudgetExceeded", err)
	}
	if fp.calls != 0 {
		t.Errorf("provider was called %d times despite budget guard, want 0", fp.calls)
	}
}

func TestCallRespectsCallMaxCostUSD(t *testing.T) {
	fp := &fakeProvider{name: "fake", privacy: PrivacyCloudOK, resp: &Response{}}
	r := &fakeRouter{decision: Decision{Provider: "fake", EstimatedCostUSD: 0.05}}

	b, _ := New(WithProvider(fp), WithRouter(r))
	_, err := b.Call(context.Background(), Call{Task: TaskGenerate, MaxCostUSD: 0.01})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Errorf("err = %v, want wrapping ErrBudgetExceeded on Call.MaxCostUSD", err)
	}
	if fp.calls != 0 {
		t.Errorf("provider was called %d times despite Call.MaxCostUSD guard, want 0", fp.calls)
	}
}

func TestCallHydratesMemory(t *testing.T) {
	fp := &fakeProvider{name: "fake", privacy: PrivacyCloudOK, resp: &Response{Text: "ok"}}
	mem := &fakeMemory{recallHits: []Record{
		{ID: "r1", Content: "User prefers TypeScript", Timestamp: time.Now()},
		{ID: "r2", Content: "Working on billing refactor", Timestamp: time.Now()},
	}}

	b, _ := New(WithProvider(fp), WithMemory(mem))
	resp, err := b.Call(context.Background(), Call{
		Prompt:    "What should I work on next?",
		UseMemory: MemoryScope{Mode: MemoryScopeRecent},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(fp.seenCall.Messages) == 0 {
		t.Fatal("provider did not receive a prepended system message from memory")
	}
	sys := fp.seenCall.Messages[0]
	if sys.Role != "system" {
		t.Errorf("first Message.Role = %q, want system", sys.Role)
	}
	if !strings.Contains(sys.Content, "User prefers TypeScript") {
		t.Errorf("memory record r1 not in injected system message: %q", sys.Content)
	}
	if !strings.Contains(sys.Content, "billing refactor") {
		t.Errorf("memory record r2 not in injected system message: %q", sys.Content)
	}
	wantHits := []string{"r1", "r2"}
	if len(resp.MemoryHits) != 2 || resp.MemoryHits[0] != wantHits[0] || resp.MemoryHits[1] != wantHits[1] {
		t.Errorf("resp.MemoryHits = %v, want %v", resp.MemoryHits, wantHits)
	}
}

func TestCallRecordsEpisode(t *testing.T) {
	fp := &fakeProvider{name: "fake", privacy: PrivacyCloudOK, resp: &Response{Text: "hello back"}}
	mem := &fakeMemory{}
	b, _ := New(WithProvider(fp), WithMemory(mem))

	_, err := b.Call(context.Background(), Call{Task: TaskGenerate, Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}

	// recordEpisode runs in a goroutine; poll briefly.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		mem.mu.Lock()
		n := len(mem.added)
		mem.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mem.mu.Lock()
	defer mem.mu.Unlock()
	if len(mem.added) != 1 {
		t.Fatalf("episode not recorded; added=%d", len(mem.added))
	}
	rec := mem.added[0]
	if rec.Kind != MemoryEpisodic {
		t.Errorf("episode Kind = %q, want MemoryEpisodic", rec.Kind)
	}
	if !strings.Contains(rec.Content, "hello") {
		t.Errorf("episode Content %q does not include prompt", rec.Content)
	}
	if !strings.Contains(rec.Content, "hello back") {
		t.Errorf("episode Content %q does not include response", rec.Content)
	}
	if !containsStr(rec.Tags, "task:generate") {
		t.Errorf("episode Tags = %v, want to include task:generate", rec.Tags)
	}
}

func TestCallHandlesMemoryRecallError(t *testing.T) {
	fp := &fakeProvider{name: "fake", privacy: PrivacyCloudOK, resp: &Response{Text: "ok"}}
	mem := &fakeMemory{recallErr: errors.New("db is sad")}
	b, _ := New(WithProvider(fp), WithMemory(mem))

	resp, err := b.Call(context.Background(), Call{
		Prompt:    "hi",
		UseMemory: MemoryScope{Mode: MemoryScopeRecent},
	})
	if err != nil {
		t.Fatalf("Call should not fail on memory error: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("resp.Text = %q, want ok", resp.Text)
	}
}

func TestCallChargesBudget(t *testing.T) {
	fp := &fakeProvider{
		name:    "fake",
		privacy: PrivacyCloudOK,
		resp: &Response{
			Text:     "ok",
			Usage:    Usage{InputTokens: 10, OutputTokens: 5},
			Provider: "fake",
			Model:    "m",
		},
	}
	r := &fakeRouter{decision: Decision{Provider: "fake", Model: "m"}}
	budget := &fakeBudget{remaining: 100.00}

	b, _ := New(WithProvider(fp), WithRouter(r), WithBudget(budget))
	_, err := b.Call(context.Background(), Call{Task: TaskGenerate})
	if err != nil {
		t.Fatal(err)
	}
	if got := budget.Spent(); got == 0 {
		t.Errorf("Spent() = %v, want > 0 after successful call", got)
	}
	if len(budget.charges) != 1 {
		t.Fatalf("charges recorded = %d, want 1", len(budget.charges))
	}
	ch := budget.charges[0]
	if ch.Provider != "fake" || ch.Model != "m" {
		t.Errorf("charge = %+v, want provider=fake model=m", ch)
	}
	if ch.Usage.InputTokens != 10 || ch.Usage.OutputTokens != 5 {
		t.Errorf("charge.Usage = %+v, want Input=10 Output=5", ch.Usage)
	}
}

func TestCallPropagatesProviderError(t *testing.T) {
	fp := &fakeProvider{name: "fake", privacy: PrivacyCloudOK, err: errors.New("boom")}
	mem := &fakeMemory{}
	b, _ := New(WithProvider(fp), WithMemory(mem))

	_, err := b.Call(context.Background(), Call{Prompt: "hi"})
	if err == nil || err.Error() != "boom" {
		t.Errorf("err = %v, want boom", err)
	}
	// No episode should be recorded on provider failure.
	time.Sleep(50 * time.Millisecond) // give any stray goroutine a moment
	mem.mu.Lock()
	defer mem.mu.Unlock()
	if len(mem.added) != 0 {
		t.Errorf("episode recorded despite provider error: %+v", mem.added)
	}
}

// containsStr is a tiny helper to keep the test readable without
// importing slices (Go 1.21+ has it but this stays compat-friendly).
func containsStr(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
