package hippo

import "errors"

// Sentinel errors. Callers should compare with errors.Is, not ==, because
// wrapping is expected.
var (
	// ErrBudgetExceeded is returned when a Call's estimated cost
	// exceeds either Call.MaxCostUSD or the Brain-level budget tracker.
	ErrBudgetExceeded = errors.New("hippo: budget exceeded")

	// ErrNoProviderAvailable is returned when the router cannot find any
	// provider that satisfies the Call's Task, Privacy, and budget
	// constraints.
	ErrNoProviderAvailable = errors.New("hippo: no provider available")

	// ErrPrivacyViolation is returned if a router attempts to send a
	// Call to a provider whose Privacy tier is weaker than the Call
	// requires. Treated as a bug, not a fallback condition.
	ErrPrivacyViolation = errors.New("hippo: privacy violation")

	// ErrProviderNotFound is returned when a Call references a provider
	// by name that has not been registered on the Brain.
	ErrProviderNotFound = errors.New("hippo: provider not found")

	// ErrMemoryUnavailable is returned when a Call requests memory but
	// no Memory has been attached to the Brain.
	ErrMemoryUnavailable = errors.New("hippo: memory not configured")

	// ErrNotImplemented is returned by stubs that have not yet been
	// implemented. Production code should never see this; it exists so
	// scaffolding can compile with a typed error instead of panicking.
	ErrNotImplemented = errors.New("hippo: not implemented")
)
