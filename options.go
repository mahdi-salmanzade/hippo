package hippo

import (
	"log/slog"
)

// Option configures a Brain. Options are applied in order by New; later
// options override earlier ones for scalar fields and append for slice
// fields (e.g. providers).
type Option func(*config) error

// config is the internal, pre-validation form of a Brain. It is not
// exported; callers mutate it via Option values.
type config struct {
	providers []Provider
	memory    Memory
	budget    BudgetTracker
	router    Router
	logger    *slog.Logger
	// defaultModel is used when a Call neither pins a Model nor supplies
	// enough context for the router to choose.
	defaultModel string
}

// WithProvider registers an LLM provider. Call WithProvider once per
// provider; order determines the preference used by the default router
// when no explicit policy is set.
func WithProvider(p Provider) Option {
	return func(c *config) error {
		// TODO: validate p != nil and that its Name is unique.
		c.providers = append(c.providers, p)
		return nil
	}
}

// WithMemory attaches a Memory backend. Without one, Calls that set
// UseMemory to anything other than MemoryScopeNone will fail with
// ErrMemoryUnavailable.
func WithMemory(m Memory) Option {
	return func(c *config) error {
		c.memory = m
		return nil
	}
}

// WithBudget attaches a BudgetTracker that enforces a spending cap across
// all calls on the Brain.
func WithBudget(b BudgetTracker) Option {
	return func(c *config) error {
		c.budget = b
		return nil
	}
}

// WithRouter installs a Router. If unset, Brain uses a simple round-robin
// over registered providers constrained by privacy tier.
func WithRouter(r Router) Option {
	return func(c *config) error {
		c.router = r
		return nil
	}
}

// WithLogger wires a structured logger for Brain internals. If unset, a
// discard logger is used.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) error {
		c.logger = l
		return nil
	}
}

// WithDefaultModel sets the model to use when a Call has no Model and the
// router returns no decision.
func WithDefaultModel(model string) Option {
	return func(c *config) error {
		c.defaultModel = model
		return nil
	}
}
