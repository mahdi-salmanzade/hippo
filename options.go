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

	// tools is the immutable tool registry wired via WithTools. Nil
	// when no tools are configured, which disables the tool-call
	// loop entirely.
	tools *ToolSet

	// maxToolHops caps the number of (tool-call → execute → feed-back)
	// rounds any single Call may traverse. Default 10. After the cap
	// is hit the Call returns with Response.Err =
	// ErrMaxToolHopsExceeded and whatever the final turn produced.
	maxToolHops int

	// maxParallelTools caps how many tools execute concurrently
	// within one turn when the provider returns multiple tool calls
	// at once. Default 4; set to 1 for fully sequential execution.
	maxParallelTools int
}

// Default option values. Exported as vars rather than consts so a
// future WithDefault helper can read them without duplicating the
// numbers here; tests that assert the defaults reference these.
const (
	defaultMaxToolHops      = 10
	defaultMaxParallelTools = 4
)

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

// WithTools registers the tools the Brain makes available to every
// Call. Tools are static for the Brain's lifetime; there is no
// registry-mutation API on purpose.
//
// A single call that supplies WithTools more than once (or mixes
// with NewToolSet manually) overrides the earlier registration —
// the last WithTools wins. The validation that rejects duplicate
// or malformed names happens here, so the error surfaces from
// hippo.New rather than the first dispatched Call.
//
// Pass zero tools to leave tool calling off.
func WithTools(tools ...Tool) Option {
	return func(c *config) error {
		if len(tools) == 0 {
			c.tools = nil
			return nil
		}
		ts, err := NewToolSet(tools...)
		if err != nil {
			return err
		}
		c.tools = ts
		return nil
	}
}

// WithMaxToolHops caps how many rounds of (tool-call → execute →
// feed-back) a single Call may traverse. Default: 10. After the
// cap is hit, the Call returns the final provider response with
// Response.Err = ErrMaxToolHopsExceeded — the response body is
// still usable; callers can inspect ToolCalls and continue manually.
//
// n <= 0 is treated as "use default".
func WithMaxToolHops(n int) Option {
	return func(c *config) error {
		if n > 0 {
			c.maxToolHops = n
		}
		return nil
	}
}

// WithMaxParallelTools caps how many tools execute concurrently
// within one turn when the provider returns multiple tool calls.
// Default: 4. Setting to 1 forces sequential execution, useful
// when the tools share a limited resource (database connection,
// rate-limited API) that a fan-out would exhaust.
//
// n <= 0 is treated as "use default".
func WithMaxParallelTools(n int) Option {
	return func(c *config) error {
		if n > 0 {
			c.maxParallelTools = n
		}
		return nil
	}
}
