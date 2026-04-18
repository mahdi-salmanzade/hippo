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
// Implementations live in hippo/router. Implementations must be safe for
// concurrent use. Route must not perform network I/O.
type Router interface {
	// Name returns a short identifier for logging.
	Name() string
	// Route picks a Provider/Model for c given the remaining USD
	// budget.
	Route(ctx context.Context, c Call, budget float64) (Decision, error)
}

// BudgetTracker caps LLM spend.
//
// Implementations live in hippo/budget. Implementations must be safe for
// concurrent use; Brain will call EstimateCost and Charge from multiple
// goroutines.
type BudgetTracker interface {
	// EstimateCost returns the expected USD cost of c using the
	// Tracker's pricing table. It does not reserve budget.
	EstimateCost(c Call) (float64, error)
	// Charge records actual USD spent after a Call completes. It must
	// be idempotent if called with the same (callID, amount) pair.
	Charge(ctx context.Context, callID string, amountUSD float64) error
	// Remaining returns the USD budget still available.
	Remaining() float64
}
