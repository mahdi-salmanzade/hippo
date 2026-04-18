package hippo

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
)

// Brain is the top-level hippo entry point. A Brain bundles a set of
// Providers, an optional Memory, an optional BudgetTracker, and an
// optional Router into a single object against which you issue Calls.
//
// A Brain is safe for concurrent use by multiple goroutines. Close the
// Brain when you are done to release memory and provider resources.
type Brain struct {
	cfg    config
	logger *slog.Logger
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

// Call dispatches c to a provider and returns the response.
//
// Pass 3 flow:
//
//  1. Selection. Router picks provider+model + cost estimate.
//     Without a router, the first registered provider is used.
//  2. Budget gate. If the estimated cost exceeds either
//     Call.MaxCostUSD or the BudgetTracker's Remaining(), Call
//     returns ErrBudgetExceeded before the provider is contacted.
//  3. Dispatch. Provider serves the request.
//  4. Charge. After a successful call, BudgetTracker.Charge records
//     the real usage. Charge errors are logged, not surfaced —
//     accounting gaps should not fail a call that already succeeded.
//
// Memory hydration and episode recording layer in on top of this in
// the next commit.
func (b *Brain) Call(ctx context.Context, c Call) (*Response, error) {
	if len(b.cfg.providers) == 0 {
		return nil, ErrNoProviderAvailable
	}

	decision, err := b.decide(ctx, c)
	if err != nil {
		return nil, err
	}

	p := providerByName(b.cfg.providers, decision.Provider)
	if p == nil {
		return nil, fmt.Errorf("hippo: router picked unregistered provider %q", decision.Provider)
	}

	// Budget gate — belt-and-suspenders against the router. The
	// router already applied the same ceiling, but a Brain without a
	// Router still gets enforcement here, and a hand-tuned
	// Router.Route that skipped the budget check is caught.
	if b.cfg.budget != nil {
		remaining := b.cfg.budget.Remaining()
		if decision.EstimatedCostUSD > remaining {
			return nil, fmt.Errorf("%w: estimate $%.6f > remaining $%.6f",
				ErrBudgetExceeded, decision.EstimatedCostUSD, remaining)
		}
	}
	if c.MaxCostUSD > 0 && decision.EstimatedCostUSD > c.MaxCostUSD {
		return nil, fmt.Errorf("%w: estimate $%.6f > Call.MaxCostUSD $%.6f",
			ErrBudgetExceeded, decision.EstimatedCostUSD, c.MaxCostUSD)
	}

	enriched := c
	if decision.Model != "" {
		enriched.Model = decision.Model
	}

	resp, err := p.Call(ctx, enriched)
	if err != nil {
		return nil, err
	}

	// The Brain owns routing metadata — backfill if the provider
	// adapter didn't populate these fields itself.
	if resp.Provider == "" {
		resp.Provider = decision.Provider
	}
	if resp.Model == "" {
		resp.Model = decision.Model
	}

	// Charge real usage. Unknown pricing is a warning, not a failure
	// — hippo prefers serving under-priced calls to blocking on a
	// pricing-table gap.
	if b.cfg.budget != nil {
		if err := b.cfg.budget.Charge(resp.Provider, resp.Model, resp.Usage); err != nil {
			b.logger.Warn("hippo: budget charge failed", "err", err,
				"provider", resp.Provider, "model", resp.Model)
		}
	}

	return resp, nil
}

// decide resolves the Call to a Decision. With a Router configured,
// it delegates; without one, it falls back to the Pass 1 behaviour
// (first registered provider, with a privacy check). The no-router
// fallback exists so hippo.New() with zero options stays usable.
func (b *Brain) decide(ctx context.Context, c Call) (Decision, error) {
	if b.cfg.router != nil {
		budget := math.Inf(1)
		if b.cfg.budget != nil {
			budget = b.cfg.budget.Remaining()
		}
		d, err := b.cfg.router.Route(ctx, c, b.cfg.providers, budget)
		if err != nil {
			return Decision{}, fmt.Errorf("hippo: route: %w", err)
		}
		return d, nil
	}

	// No router — legacy "first provider" dispatch.
	if len(b.cfg.providers) > 1 {
		b.logger.Warn("hippo: multiple providers + no router; using first (register WithRouter to route)",
			"count", len(b.cfg.providers),
			"chosen", b.cfg.providers[0].Name(),
		)
	}
	p := b.cfg.providers[0]
	if p.Privacy() < c.Privacy {
		return Decision{}, ErrPrivacyViolation
	}
	return Decision{Provider: p.Name()}, nil
}

// providerByName returns the registered provider with the given
// Name, or nil if absent. Linear scan: provider lists are small
// (typically < 10 entries).
func providerByName(providers []Provider, name string) Provider {
	for _, p := range providers {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

// Stream is the streaming counterpart to Call. Streaming is not wired
// yet; callers receive ErrNotImplemented.
func (b *Brain) Stream(ctx context.Context, c Call) (<-chan StreamChunk, error) {
	_ = ctx
	_ = c
	return nil, ErrNotImplemented
}

// Close releases all resources held by the Brain, including the Memory
// backend (if any). Safe to call more than once.
func (b *Brain) Close() error {
	// TODO: close memory, flush budget tracker, close provider
	// resources. Aggregate errors.
	if b.cfg.memory != nil {
		return b.cfg.memory.Close()
	}
	return nil
}
