package budget

import (
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/mahdi-salmanzade/hippo"
)

func TestRemainingWithNoCeiling(t *testing.T) {
	b := New()
	if got := b.Remaining(); got != math.Inf(1) {
		t.Errorf("Remaining() = %v, want +Inf", got)
	}
}

func TestRemainingWithCeiling(t *testing.T) {
	b := New(WithCeiling(5.00))
	if got := b.Remaining(); got != 5.00 {
		t.Errorf("Remaining() = %v, want 5.00", got)
	}
}

func TestChargeReducesRemaining(t *testing.T) {
	b := New(WithCeiling(1.00))
	// Haiku: 1000 input tokens = $0.001; 200 output = $0.001. Total = $0.002.
	usage := hippo.Usage{InputTokens: 1000, OutputTokens: 200}
	if err := b.Charge("anthropic", "claude-haiku-4-5", usage); err != nil {
		t.Fatalf("Charge: %v", err)
	}
	want := 1.00 - 0.002
	if got := b.Remaining(); math.Abs(got-want) > 1e-9 {
		t.Errorf("Remaining() = %v, want %v", got, want)
	}
}

func TestChargeAccumulatesAcrossCalls(t *testing.T) {
	b := New()
	usage := hippo.Usage{InputTokens: 1000, OutputTokens: 1000}
	for i := 0; i < 5; i++ {
		if err := b.Charge("anthropic", "claude-haiku-4-5", usage); err != nil {
			t.Fatal(err)
		}
	}
	// 5 × (1000·$1/M + 1000·$5/M) = 5 × 0.006 = 0.030
	want := 0.030
	if got := b.Spent(); math.Abs(got-want) > 1e-9 {
		t.Errorf("Spent() = %v, want %v", got, want)
	}
}

func TestEstimateCostMatchesChargeForSameUsage(t *testing.T) {
	b := New()
	usage := hippo.Usage{InputTokens: 12, OutputTokens: 34, CachedTokens: 5}
	est, err := b.EstimateCost("anthropic", "claude-haiku-4-5", usage)
	if err != nil {
		t.Fatalf("EstimateCost: %v", err)
	}
	if err := b.Charge("anthropic", "claude-haiku-4-5", usage); err != nil {
		t.Fatal(err)
	}
	if got := b.Spent(); math.Abs(got-est) > 1e-12 {
		t.Errorf("Spent() = %v, want EstimateCost = %v", got, est)
	}
}

func TestEstimateCostUnknownModelReturnsZero(t *testing.T) {
	b := New()
	cost, err := b.EstimateCost("anthropic", "made-up-model", hippo.Usage{InputTokens: 100})
	if cost != 0 {
		t.Errorf("cost = %v, want 0 for unknown model", cost)
	}
	if !errors.Is(err, hippo.ErrUnknownPricing) {
		t.Errorf("err = %v, want wrapping ErrUnknownPricing", err)
	}
}

func TestChargeUnknownModelDoesNotBlock(t *testing.T) {
	// Per the Pass 3 design: an unknown model pricing is a warning,
	// not a fatal error. Charge should surface the error but still
	// update Spent (by 0, since cost is 0).
	b := New()
	startingSpent := b.Spent()
	err := b.Charge("anthropic", "made-up-model", hippo.Usage{InputTokens: 100})
	if !errors.Is(err, hippo.ErrUnknownPricing) {
		t.Errorf("Charge err = %v, want wrapping ErrUnknownPricing", err)
	}
	if got := b.Spent(); got != startingSpent {
		t.Errorf("Spent() = %v, want unchanged (%v) for unknown model",
			got, startingSpent)
	}
}

func TestRemainingClampsAtZero(t *testing.T) {
	b := New(WithCeiling(0.001))
	// Charge far more than the ceiling.
	usage := hippo.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}
	_ = b.Charge("anthropic", "claude-opus-4-7", usage)
	if got := b.Remaining(); got != 0 {
		t.Errorf("Remaining() after over-spend = %v, want 0", got)
	}
}

func TestConcurrentCharge(t *testing.T) {
	b := New()
	usage := hippo.Usage{InputTokens: 100, OutputTokens: 100}
	const goroutines = 5
	const perGoroutine = 100

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if err := b.Charge("anthropic", "claude-haiku-4-5", usage); err != nil {
					t.Errorf("Charge: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	// Each charge: 100·$1/M + 100·$5/M = $0.0006. 500 calls → $0.30.
	want := float64(goroutines*perGoroutine) * 0.0006
	if got := b.Spent(); math.Abs(got-want) > 1e-9 {
		t.Errorf("Spent() = %v after %d concurrent charges, want %v",
			got, goroutines*perGoroutine, want)
	}
}

func TestWithPricingOverride(t *testing.T) {
	custom := &PricingTable{
		Providers: map[string]ProviderPricing{
			"custom": {
				Models: map[string]ModelPricing{
					"model-x": {InputPerMtok: 100.0, OutputPerMtok: 200.0},
				},
			},
		},
	}
	b := New(WithPricing(custom))
	cost, err := b.EstimateCost("custom", "model-x", hippo.Usage{InputTokens: 1000, OutputTokens: 1000})
	if err != nil {
		t.Fatalf("EstimateCost: %v", err)
	}
	// 1000·100/M + 1000·200/M = 0.1 + 0.2 = 0.3
	if math.Abs(cost-0.3) > 1e-9 {
		t.Errorf("cost = %v, want 0.3", cost)
	}
}

func TestPricingLookupOllamaZeroCostFallsThrough(t *testing.T) {
	p := DefaultPricing()
	// Registered Ollama model — exact hit with real context window.
	r, ok := p.Lookup("ollama", "llama3.3:70b")
	if !ok {
		t.Fatal("ollama llama3.3:70b not resolved")
	}
	if r.ContextWindow <= 0 {
		t.Errorf("registered ollama model has ContextWindow=%d, want > 0", r.ContextWindow)
	}
	if r.InputPerMtok != 0 || r.OutputPerMtok != 0 {
		t.Errorf("ollama rates = %+v, want all zero", r)
	}

	// Unregistered Ollama model — zero_cost fallback kicks in, ok=true
	// with zero values so budget.Charge records $0 rather than warning.
	r2, ok2 := p.Lookup("ollama", "some-random-model-the-user-pulled:latest")
	if !ok2 {
		t.Fatal("ollama zero_cost fallback did not trigger; unknown model returned ok=false")
	}
	if r2 != (ModelPricing{}) {
		t.Errorf("ollama fallback returned %+v, want zero ModelPricing", r2)
	}

	// Unregistered model for a non-zero-cost provider still returns
	// ok=false — the fallback is ollama-specific.
	if _, ok := p.Lookup("anthropic", "no-such-model"); ok {
		t.Error("anthropic unknown model returned ok=true; zero-cost semantics should not apply")
	}
}

func TestChargeUnknownOllamaModelDoesNotWarn(t *testing.T) {
	b := New()
	// Parallels TestChargeUnknownModelDoesNotBlock but asserts the
	// happier path for ollama: err is nil, not ErrUnknownPricing.
	err := b.Charge("ollama", "phi4:14b", hippo.Usage{InputTokens: 100, OutputTokens: 50})
	if err != nil {
		t.Errorf("ollama Charge returned %v, want nil (zero-cost fallthrough)", err)
	}
	if got := b.Spent(); got != 0 {
		t.Errorf("Spent() = %v, want 0 after ollama charge", got)
	}
}

func TestPricingLookupHandlesDatedModelIds(t *testing.T) {
	p := DefaultPricing()
	r, ok := p.Lookup("anthropic", "claude-haiku-4-5-20250930")
	if !ok {
		t.Fatal("dated haiku id not resolved")
	}
	want := p.Providers["anthropic"].Models["claude-haiku-4-5"]
	if r != want {
		t.Errorf("rate = %+v, want %+v", r, want)
	}
}

func TestPricingLookupKnowsOpenAI(t *testing.T) {
	p := DefaultPricing()
	r, ok := p.Lookup("openai", "gpt-5-nano")
	if !ok {
		t.Fatal("openai gpt-5-nano not resolved")
	}
	if r.InputPerMtok <= 0 || r.OutputPerMtok <= 0 {
		t.Errorf("openai gpt-5-nano rates look wrong: %+v", r)
	}
	if r.CachedInputPerMtok <= 0 {
		t.Errorf("openai gpt-5-nano cached_input_per_mtok = %v, want > 0",
			r.CachedInputPerMtok)
	}
}

func TestEstimateCostUsesCachedInputRateForOpenAI(t *testing.T) {
	b := New()
	// Pricing table has OpenAI pricing under "cached_input_per_mtok"
	// (CachedInputPerMtok), not CacheReadPerMtok. costOf must pick
	// the non-zero rate so cached tokens aren't double-priced at the
	// full input rate or silently billed at zero.
	usage := hippo.Usage{InputTokens: 1000, OutputTokens: 100, CachedTokens: 500}
	cost, err := b.EstimateCost("openai", "gpt-5-nano", usage)
	if err != nil {
		t.Fatalf("EstimateCost: %v", err)
	}
	// gpt-5-nano: input $0.05/Mtok, output $0.40/Mtok, cached $0.005/Mtok.
	// plain  = (1000-500)*0.05/M = 2.5e-5
	// cached = 500*0.005/M = 2.5e-6
	// out    = 100*0.40/M = 4.0e-5
	want := 2.5e-5 + 2.5e-6 + 4.0e-5
	if math.Abs(cost-want) > 1e-12 {
		t.Errorf("cost = %v, want %v", cost, want)
	}
}
