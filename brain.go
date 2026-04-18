package hippo

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"time"
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
//
// No-router default. If WithRouter is not supplied, Brain uses the
// first registered provider for every Call (after the privacy check).
// Multiple providers without a router logs a warning but still picks
// the first. This is the "simplest working setup" tier:
//
//	// simplest: one provider, no router
//	b, _ := hippo.New(hippo.WithProvider(p))
//
//	// with routing: policy picks provider+model per Call.Task
//	b, _ := hippo.New(
//	    hippo.WithProvider(anthropic.New(...)),
//	    hippo.WithProvider(openai.New(...)),
//	    hippo.WithRouter(yaml.Load("")), // embedded default policy
//	)
//
// The no-router path exists so new users can start with a single
// WithProvider and get a working Brain immediately; it is not a
// shortcut for the full router flow. For anything beyond a single
// provider, pass a Router explicitly.
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
//  2. Budget gate. Estimated cost checked against Call.MaxCostUSD
//     and BudgetTracker.Remaining(); exceeding either yields
//     ErrBudgetExceeded.
//  3. Memory hydration. When a Memory is wired and Call.UseMemory is
//     non-zero, the Brain Recalls matching records and prepends them
//     as a system-role message on the outgoing Call. Recall errors
//     are logged but do not block the Call — memory is a helper,
//     not a dependency.
//  4. Dispatch. Provider serves the enriched Call.
//  5. Charge. Post-call, BudgetTracker.Charge records real usage.
//  6. Episode. When Memory is wired, a summary of the call is
//     written back as an Episodic record. This happens in a
//     background goroutine so it doesn't add latency.
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

	// Memory hydration. Recall runs with the call's ctx so a
	// cancelled Call doesn't leave a hanging query.
	memoryHits := b.hydrateMemory(ctx, c)
	enriched = injectMemory(enriched, memoryHits)

	resp, err := p.Call(ctx, enriched)
	if err != nil {
		return nil, err
	}

	// The Brain owns routing and memory metadata — backfill if the
	// provider adapter didn't populate these fields itself.
	if resp.Provider == "" {
		resp.Provider = decision.Provider
	}
	if resp.Model == "" {
		resp.Model = decision.Model
	}
	resp.MemoryHits = recordIDs(memoryHits)

	// Charge real usage. Unknown pricing is a warning, not a failure
	// — hippo prefers serving under-priced calls to blocking on a
	// pricing-table gap.
	if b.cfg.budget != nil {
		if err := b.cfg.budget.Charge(resp.Provider, resp.Model, resp.Usage); err != nil {
			b.logger.Warn("hippo: budget charge failed", "err", err,
				"provider", resp.Provider, "model", resp.Model)
		}
	}

	// Episode recording runs async with context.Background() so a
	// cancelled call ctx doesn't abort the write.
	if b.cfg.memory != nil {
		go b.recordEpisode(c, resp)
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

// hydrateMemory recalls memory records for c and returns them, or nil
// if memory is not configured, the call opted out, or Recall failed.
// A failed recall is logged at Warn — memory is a helper, not a hard
// dependency — and an empty slice is returned. Call and Stream both
// use this so the hydration behaviour stays identical across paths.
func (b *Brain) hydrateMemory(ctx context.Context, c Call) []Record {
	if c.UseMemory.Mode == MemoryScopeNone || b.cfg.memory == nil {
		return nil
	}
	query, q := memoryQueryFromScope(c.UseMemory, c.Prompt)
	hits, err := b.cfg.memory.Recall(ctx, query, q)
	if err != nil {
		b.logger.Warn("hippo: memory recall failed (serving without memory)", "err", err)
		return nil
	}
	return hits
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

// memoryQueryFromScope translates a user-intent MemoryScope (what a
// Call asks for) into a concrete backend MemoryQuery (retrieval
// parameters) plus the full-text query string Recall should use.
//
// Mode semantics:
//   - Recent: "what happened lately" — empty query string, time
//     window, ordered by recency.
//   - ByTags: "records with these tags" — empty query string, tag
//     filter, ordered by recency.
//   - Full: "search everything relevant to the prompt" — prompt
//     feeds FTS5, no time filter, broader limit.
//
// The prompt is only used when Full mode asks for content matching.
// For Recent and ByTags, passing the prompt to Recall would turn a
// recency/tag lookup into an AND of every prompt token against
// record contents, which almost never matches.
func memoryQueryFromScope(s MemoryScope, prompt string) (string, MemoryQuery) {
	switch s.Mode {
	case MemoryScopeRecent:
		return "", MemoryQuery{
			Since: time.Now().Add(-24 * time.Hour),
			Limit: 5,
		}
	case MemoryScopeByTags:
		return "", MemoryQuery{
			Tags:  s.Tags,
			Limit: 10,
		}
	case MemoryScopeFull:
		return prompt, MemoryQuery{Limit: 20}
	default: // MemoryScopeNone — caller shouldn't reach here, but be safe.
		return "", MemoryQuery{}
	}
}

// injectMemory prepends recalled records to the outgoing Call as a
// system-role Message so providers that have a dedicated system
// field (Anthropic, OpenAI) fold it into the right place and
// providers that don't still see it first in the message list.
func injectMemory(c Call, records []Record) Call {
	if len(records) == 0 {
		return c
	}
	var b strings.Builder
	b.WriteString("[Relevant context from memory]\n")
	for _, r := range records {
		b.WriteString("- ")
		b.WriteString(r.Timestamp.Format(time.RFC3339))
		b.WriteString(": ")
		b.WriteString(r.Content)
		b.WriteByte('\n')
	}
	sysMsg := Message{Role: "system", Content: b.String()}
	c.Messages = append([]Message{sysMsg}, c.Messages...)
	return c
}

// recordEpisode stores an Episodic record summarising this Call.
// Summaries keep content raw (no LLM-summarisation) — the
// MemMachine-inspired "raw is queen" policy from Pass 2 applies
// here too. Response.Text is truncated to 500 chars for the trace
// body, which is enough to re-recognise the interaction later
// without blowing up the DB on long completions.
func (b *Brain) recordEpisode(c Call, resp *Response) {
	var content strings.Builder
	content.WriteString("prompt: ")
	content.WriteString(c.Prompt)
	content.WriteString("\n→ response: ")
	if len(resp.Text) > 500 {
		content.WriteString(resp.Text[:500])
		content.WriteString("…")
	} else {
		content.WriteString(resp.Text)
	}

	var tags []string
	if c.Task != "" {
		tags = append(tags, "task:"+string(c.Task))
	}
	if resp.Provider != "" {
		tags = append(tags, "provider:"+resp.Provider)
	}

	rec := &Record{
		Kind:       MemoryEpisodic,
		Timestamp:  time.Now(),
		Content:    content.String(),
		Tags:       tags,
		Importance: 0.5,
		Source:     "brain.Call",
	}
	if err := b.cfg.memory.Add(context.Background(), rec); err != nil {
		b.logger.Warn("hippo: episode record failed", "err", err)
	}
}

// recordIDs extracts the ID of every record in the slice.
func recordIDs(records []Record) []string {
	if len(records) == 0 {
		return nil
	}
	out := make([]string, len(records))
	for i, r := range records {
		out[i] = r.ID
	}
	return out
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
