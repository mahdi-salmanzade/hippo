// Package budget implements hippo.BudgetTracker together with the
// pricing table it consults.
//
// The BudgetTracker interface lives in the root hippo package; this
// package imports it. New returns a tracker backed by the embedded
// pricing.yaml; WithCeiling caps the running total.
package budget

import (
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/mahdi-salmanzade/hippo"
)

// Option configures a tracker during construction.
type Option func(*tracker)

// WithCeiling sets a hard USD cap. Remaining() clamps to 0 once Spent
// reaches the ceiling. Without this option the tracker is unbounded
// and Remaining reports math.Inf(1).
func WithCeiling(usd float64) Option {
	return func(t *tracker) {
		t.ceiling = usd
		t.hasCeiling = true
	}
}

// WithPricing overrides the embedded default pricing table. Useful
// for tests and for callers who bundle their own rates (e.g. a
// private model hosted at a custom endpoint).
func WithPricing(p *PricingTable) Option {
	return func(t *tracker) { t.pricing = p }
}

// New returns a fresh hippo.BudgetTracker. With no options it is
// unbounded: Remaining() returns +Inf and Charge never errors on
// spend. WithCeiling caps it; WithPricing swaps the rate table.
func New(opts ...Option) hippo.BudgetTracker {
	t := &tracker{pricing: DefaultPricing()}
	for _, o := range opts {
		o(t)
	}
	return t
}

// tracker is the in-memory BudgetTracker implementation.
type tracker struct {
	mu         sync.Mutex
	spent      float64
	ceiling    float64
	hasCeiling bool
	pricing    *PricingTable
}

// EstimateCost computes cost from Usage × rate. An unknown
// (provider, model) pair returns cost=0 and a wrapped
// hippo.ErrUnknownPricing error. Brain should log but not fail the
// call — the Pass 3 design prefers serving under-priced calls over
// blocking on a pricing-table update.
func (t *tracker) EstimateCost(provider, model string, usage hippo.Usage) (float64, error) {
	if t.pricing == nil {
		return 0, errors.New("budget: pricing table not configured")
	}
	rate, ok := t.pricing.Lookup(provider, model)
	if !ok {
		return 0, fmt.Errorf("%w: %s:%s", hippo.ErrUnknownPricing, provider, model)
	}
	return costOf(rate, usage), nil
}

// Charge records a cost against the running total.
func (t *tracker) Charge(provider, model string, usage hippo.Usage) error {
	cost, err := t.EstimateCost(provider, model, usage)
	// Unknown pricing still records whatever we could compute (0),
	// so the caller learns about the gap via the returned error but
	// the call is not blocked.
	t.mu.Lock()
	t.spent += cost
	t.mu.Unlock()
	return err
}

func (t *tracker) Remaining() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.hasCeiling {
		return math.Inf(1)
	}
	r := t.ceiling - t.spent
	if r < 0 {
		return 0
	}
	return r
}

func (t *tracker) Spent() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.spent
}

// costOf is the inner math: usage × rate, with cached tokens priced
// at whichever "cached input" rate the provider uses. Anthropic fills
// CacheReadPerMtok; OpenAI fills CachedInputPerMtok; costOf picks the
// non-zero one so the unified hippo.Usage.CachedTokens counter works
// across both dialects.
//
// Anthropic also has a CacheWritePerMtok (cache-creation surcharge)
// that is not surfaced via hippo.Usage. The Anthropic adapter
// computes the per-call cost itself using the richer bucket counts
// returned by the Messages API; the budget tracker only sees the
// coarse InputTokens / OutputTokens / CachedTokens split and so
// treats every cached token at the read rate. Pass 5 revisits this
// once we have per-provider cache accounting.
func costOf(rate ModelPricing, u hippo.Usage) float64 {
	const perMillion = 1_000_000.0
	cachedRate := rate.CacheReadPerMtok
	if cachedRate == 0 {
		cachedRate = rate.CachedInputPerMtok
	}
	plain := float64(u.InputTokens-u.CachedTokens) * rate.InputPerMtok / perMillion
	if u.InputTokens < u.CachedTokens {
		// Defensive: if a provider ever reports more cached than
		// total input, drop the plain term rather than go negative.
		plain = 0
	}
	cached := float64(u.CachedTokens) * cachedRate / perMillion
	out := float64(u.OutputTokens) * rate.OutputPerMtok / perMillion
	return plain + cached + out
}
