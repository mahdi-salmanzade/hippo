package hippo

import (
	"context"
	"io"
	"log/slog"
)

// Brain is the top-level hippo entry point. A Brain bundles a set of
// Providers, an optional Memory, an optional BudgetTracker, and a Router
// into a single object against which you issue Calls.
//
// A Brain is safe for concurrent use by multiple goroutines. Close the
// Brain when you are done to release memory and provider resources.
type Brain struct {
	cfg    config
	logger *slog.Logger
	// TODO: internal provider registry, memory handle, router handle,
	// budget handle, shared http.Client, in-flight Call accounting.
}

// New constructs a Brain from the supplied options. It validates that at
// least one provider is registered and that the router (if any) is
// compatible with the registered providers.
//
// New never blocks on network I/O; provider authentication is deferred
// until the first Call.
func New(opts ...Option) (*Brain, error) {
	c := config{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, opt := range opts {
		if err := opt(&c); err != nil {
			return nil, err
		}
	}
	// TODO: validate at least one provider; install default router if
	// none; wire budget/memory handles.
	return &Brain{cfg: c, logger: c.logger}, nil
}

// Call dispatches c to the provider chosen by the router and returns the
// full Response. The provided context controls cancellation and deadline;
// callers are responsible for setting a timeout appropriate to their use.
//
// If Call's UseMemory is non-zero, the Brain queries its Memory backend
// for relevant records and injects them as additional system-role
// messages before handing off to the provider.
//
// Call returns ErrBudgetExceeded if the pre-flight cost estimate exceeds
// either c.MaxCostUSD or the Brain's BudgetTracker remaining balance.
func (b *Brain) Call(ctx context.Context, c Call) (*Response, error) {
	_ = ctx
	_ = c
	// TODO: route → memory-inject → budget-check → provider.Call →
	// record usage on budget tracker → optionally persist outcome to
	// memory → return Response.
	panic("hippo: Brain.Call not implemented")
}

// Stream is the streaming counterpart to Call. The returned channel
// delivers StreamChunk values until the stream ends, at which point the
// channel is closed. The final chunk has Final == true and carries the
// authoritative Usage and CostUSD.
//
// If Stream returns an error, no channel is returned and no stream was
// opened. In-flight stream errors surface via the Err field of a
// StreamChunk with Final == true.
func (b *Brain) Stream(ctx context.Context, c Call) (<-chan StreamChunk, error) {
	_ = ctx
	_ = c
	// TODO: same pipeline as Call but forward the provider's stream
	// channel, accumulating usage on the Brain's budget tracker once the
	// terminal chunk arrives.
	panic("hippo: Brain.Stream not implemented")
}

// Close releases all resources held by the Brain, including the Memory
// backend (if any) and any per-provider long-lived connections. It is
// safe to call Close multiple times.
func (b *Brain) Close() error {
	// TODO: close memory, flush budget tracker, close provider
	// resources. Aggregate errors.
	if b.cfg.memory != nil {
		return b.cfg.memory.Close()
	}
	return nil
}
