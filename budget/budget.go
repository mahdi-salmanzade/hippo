// Package budget holds implementations of hippo.BudgetTracker together
// with the pricing table they consult.
//
// The BudgetTracker interface lives in the root hippo package; this
// package imports it. Constructors (Daily, Unlimited) return
// hippo.BudgetTracker directly.
package budget

import (
	"context"
	"sync"

	"github.com/mahdi-salmanzade/hippo"
)

// Daily returns a hippo.BudgetTracker capped at limit USD per rolling
// 24-hour window, backed by the default embedded pricing table.
//
// This is the lightweight default used by WithBudget in examples.
func Daily(limitUSD float64) hippo.BudgetTracker {
	// TODO: implement rolling-window tracker.
	return &memoryTracker{limit: limitUSD, pricing: DefaultPricing()}
}

// Unlimited returns a hippo.BudgetTracker that never rejects a call.
// Useful for local-only setups where Ollama is the sole provider.
func Unlimited() hippo.BudgetTracker {
	return &memoryTracker{limit: 0, pricing: DefaultPricing(), unlimited: true}
}

// memoryTracker is the default in-memory BudgetTracker implementation.
type memoryTracker struct {
	mu        sync.Mutex
	limit     float64
	spent     float64
	pricing   *PricingTable
	unlimited bool
	// TODO: rolling window (ring of timestamped charges) for Daily.
}

func (t *memoryTracker) EstimateCost(c hippo.Call) (float64, error) {
	_ = c
	// TODO: look up per-model rate in t.pricing, multiply by token
	// estimate. For now return 0 so stub calls don't trip the limit.
	return 0, nil
}

func (t *memoryTracker) Charge(ctx context.Context, callID string, amountUSD float64) error {
	_ = ctx
	_ = callID
	t.mu.Lock()
	defer t.mu.Unlock()
	t.spent += amountUSD
	return nil
}

func (t *memoryTracker) Remaining() float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.unlimited {
		return 0 // treated as "unbounded" by the router
	}
	return t.limit - t.spent
}
