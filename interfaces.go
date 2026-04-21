package hippo

import (
	"context"
	"time"
)

// Provider is the contract every LLM backend must satisfy.
//
// Implementations live in hippo/providers/<name> and are constructed with
// a New function that returns hippo.Provider. The root hippo package does
// not import any concrete provider: users wire them in from their own
// main.go via WithProvider, which inverts the dependency direction.
//
// # System-role message contract
//
// Provider adapters MUST translate Message entries with Role == "system"
// into the upstream API's native system-prompt mechanism. Each API does
// this slightly differently (Anthropic: top-level "system" field;
// OpenAI/Ollama: a system-role entry in the messages array is native;
// Gemini: "systemInstruction" top-level field; Cohere: "preamble"
// parameter) - the translation is a ~10-line switch inside each
// adapter.
//
// This matters because hippo.Brain's memory-hydration path injects
// recalled records as a system-role Message prepended to Call.Messages.
// An adapter that silently drops the system role would lose the memory
// context; one that passes it through as a plain user message would
// pollute the transcript. Always route it to the provider's native
// system channel.
type Provider interface {
	// Name returns a short, stable identifier (e.g. "anthropic").
	Name() string
	// Models enumerates the models this provider exposes.
	Models() []ModelInfo
	// Privacy reports the strongest tier this provider can satisfy.
	Privacy() PrivacyTier
	// EstimateCost returns a pre-flight USD estimate for a Call. It
	// must be cheap: the router may call it many times across
	// candidates.
	EstimateCost(c Call) (float64, error)
	// Call executes a Call synchronously and returns the full Response.
	Call(ctx context.Context, c Call) (*Response, error)
	// Stream executes a Call and emits StreamChunk values on the
	// returned channel. The channel is closed when the stream ends
	// (success or error).
	Stream(ctx context.Context, c Call) (<-chan StreamChunk, error)
}

// Embedder produces vector embeddings for text. Implementations are
// typically local models (Ollama's nomic-embed-text, mxbai-embed-large)
// but cloud embedders could implement this too. hippo uses the returned
// vectors for semantic memory retrieval; callers that want to use a
// Brain without semantic memory never need to construct one.
//
// All implementations must be safe for concurrent use - backfill and
// on-demand recall embedding may call the same Embedder from different
// goroutines.
type Embedder interface {
	// Name identifies the embedder so a memory store can detect when
	// the embedding model has changed and flag records for re-embedding
	// (e.g. "ollama:nomic-embed-text").
	Name() string
	// Dimensions reports the fixed vector length this embedder
	// produces. Zero is acceptable for embedders that derive the
	// dimensionality lazily from the first successful call; callers
	// should not assume Dimensions is available before Embed succeeds
	// once.
	Dimensions() int
	// Embed returns one vector per input text, in the same order.
	// Implementations may batch server-side for efficiency, but the
	// returned shape is always len(texts) outer × Dimensions inner.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Memory is the persistence contract for hippo's memory layer.
//
// Implementations live in hippo/memory/<backend>. Backends must be safe
// for concurrent use.
type Memory interface {
	// Add persists a record. If rec.ID is empty, the backend assigns a
	// new one and mutates rec in place (backends may use any scheme,
	// but ULID is recommended).
	Add(ctx context.Context, rec *Record) error
	// Recall returns records matching q, ranked by a backend-defined
	// relevance heuristic against query. If the backend supports
	// embeddings, it SHOULD use them; otherwise lexical ranking is
	// acceptable.
	Recall(ctx context.Context, query string, q MemoryQuery) ([]Record, error)
	// Prune deletes records older than before. Profile records are
	// exempt unless the backend is configured otherwise.
	Prune(ctx context.Context, before time.Time) error
	// Close releases resources.
	Close() error
}

// Router is the policy interface that chooses which Provider and model
// should serve a given Call.
//
// Implementations live in hippo/router and must be safe for concurrent
// use. Route must not perform network I/O - it is called on every
// dispatch and must stay cheap.
//
// The providers slice is the Brain's registered provider list, passed
// in on every Route call so the router itself stays stateless w.r.t.
// the provider set. budget is the BudgetTracker's Remaining() at call
// time; routers may use it to skip providers that would overrun.
type Router interface {
	// Name returns a short identifier for logging.
	Name() string
	// Route picks a Provider/Model for c.
	Route(ctx context.Context, c Call, providers []Provider, budget float64) (Decision, error)
}

// BudgetTracker caps LLM spend. Implementations live in hippo/budget
// and must be safe for concurrent use; Brain calls Charge and
// Remaining from multiple goroutines.
//
// The unit of accounting is (provider, model, Usage): the tracker
// owns the pricing table that turns a token count into a USD figure.
// That keeps provider code from duplicating price math (eventually -
// see the Pass 5 consolidation note in providers/anthropic/pricing.go).
type BudgetTracker interface {
	// EstimateCost returns the USD cost of the given Usage against
	// the tracker's pricing table. A nil error with cost=0 is a
	// valid "price unknown" answer; callers should not treat an
	// unknown model as a hard failure.
	EstimateCost(provider, model string, usage Usage) (float64, error)
	// Charge records a real cost after a Call completes. Unlike
	// EstimateCost it mutates the running total.
	Charge(provider, model string, usage Usage) error
	// Remaining returns the USD budget still available. Trackers
	// without a ceiling return math.Inf(1).
	Remaining() float64
	// Spent returns the cumulative USD charged so far.
	Spent() float64
}
