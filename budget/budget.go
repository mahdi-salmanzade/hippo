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
// at the cheaper cache_read rate. Cache_write is not separately
// surfaced in hippo.Usage (Anthropic's two cache counters are
// summed into CachedTokens at the provider layer) so the tracker
// treats all cached tokens at the read rate. Pass 5 will revisit
// when cache accounting becomes per-provider.
func costOf(rate Rate, u hippo.Usage) float64 {
	const perMillion = 1_000_000.0
	plain := float64(u.InputTokens-u.CachedTokens) * rate.Input / perMillion
	if u.InputTokens < u.CachedTokens {
		// Defensive: if a provider ever reports more cached than
		// total input, drop the plain term rather than go negative.
		plain = 0
	}
	cached := float64(u.CachedTokens) * rate.CacheRead / perMillion
	out := float64(u.OutputTokens) * rate.Output / perMillion
	return plain + cached + out
}
