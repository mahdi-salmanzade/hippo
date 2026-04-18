// Package budget implements hippo's cost tracking: a USD-denominated
// spend ledger, a pricing table keyed by provider and model, and the
// Tracker interface that Brain consults before dispatching a Call.
//
// Budget data is in-memory by default; persistent trackers are expected
// to wrap the in-memory tracker and flush to their store of choice.
package budget

import (
	"context"
	"sync"

	"github.com/mahdi-salmanzade/hippo"
)

// Tracker is the contract for anything that caps LLM spend.
//
// Implementations must be safe for concurrent use; Brain will call
// EstimateCost and Charge from multiple goroutines.
type Tracker interface {
	// EstimateCost returns the expected USD cost of c using the
	// Tracker's pricing table. It does not reserve budget.
	EstimateCost(c hippo.Call) (float64, error)
	// Charge records actual USD spent after a Call completes. It must
	// be idempotent if called with the same (callID, amount) pair.
	Charge(ctx context.Context, callID string, amountUSD float64) error
	// Remaining returns the USD budget still available.
	Remaining() float64
}

// Daily returns a Tracker capped at limit USD per rolling 24-hour window,
// backed by the default embedded pricing table.
//
// This is the lightweight default used by WithBudget in examples.
func Daily(limitUSD float64) Tracker {
	// TODO: implement rolling-window tracker.
	return &memoryTracker{limit: limitUSD, pricing: DefaultPricing()}
}

// Unlimited returns a Tracker that never rejects a call. Useful for
// local-only setups where Ollama is the sole provider.
func Unlimited() Tracker {
	return &memoryTracker{limit: 0, pricing: DefaultPricing(), unlimited: true}
}

// memoryTracker is the default in-memory Tracker implementation.
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
