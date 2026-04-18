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

// New constructs a Brain from the supplied options.
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
	return &Brain{cfg: c, logger: c.logger}, nil
}

// Call dispatches c to a registered provider and returns the response.
//
// Pass 1 routing is intentionally minimal:
//   - If no provider is registered, Call returns ErrNoProviderAvailable.
//   - If more than one provider is registered, the first is used and a
//     warning is logged. Real router-driven selection arrives in pass 3.
//   - If c.Privacy requires a stronger tier than the chosen provider
//     can honor, Call returns ErrPrivacyViolation.
//
// Future passes will layer memory hydration and budget enforcement
// here; they are no-ops today.
func (b *Brain) Call(ctx context.Context, c Call) (*Response, error) {
	if len(b.cfg.providers) == 0 {
		return nil, ErrNoProviderAvailable
	}
	if len(b.cfg.providers) > 1 {
		b.logger.Warn("hippo: multiple providers registered; using first (router arrives in pass 3)",
			"count", len(b.cfg.providers),
			"chosen", b.cfg.providers[0].Name(),
		)
	}
	p := b.cfg.providers[0]

	// Provider must offer at least the privacy tier the Call demands.
	// Tiers are ordered with higher = stricter (see PrivacyTier in
	// types.go), so p.Privacy() >= c.Privacy is the satisfaction test.
	if p.Privacy() < c.Privacy {
		return nil, ErrPrivacyViolation
	}

	return p.Call(ctx, c)
}

// Stream is the streaming counterpart to Call. Streaming is not wired
// yet; callers receive ErrNotImplemented.
func (b *Brain) Stream(ctx context.Context, c Call) (<-chan StreamChunk, error) {
	_ = ctx
	_ = c
	return nil, ErrNotImplemented
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
