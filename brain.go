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
// The flow — in Pass 3 form — is:
//
//  1. Selection. If a Router is configured, Route picks the
//     provider+model (and checks privacy / cost caps). Otherwise the
//     first registered provider is used with a privacy check.
//  2. Dispatch. The selected provider handles the Call. The Brain
//     backfills Response.Provider and Response.Model from the
//     Decision if the provider didn't already set them.
//
// Memory hydration and budget enforcement layer in on top of this in
// the next two commits.
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
