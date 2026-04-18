// Package router defines the policy layer that chooses which Provider
// and model should serve a given Call.
//
// A Router is the component between the Brain and its providers. It
// takes a Call plus the Brain's remaining budget and returns a Decision
// identifying the chosen provider/model and a human-readable reason.
//
// hippo ships with a YAML-driven Router (see yaml.go) that is
// hot-reloadable; custom Routers can be supplied via hippo.WithRouter.
package router

import (
	"context"

	"github.com/mahdi-salmanzade/hippo"
)

// Router is the policy interface. Implementations must be safe for
// concurrent use.
type Router interface {
	// Name returns a short identifier for logging.
	Name() string
	// Route picks a Provider/Model for c given the remaining USD
	// budget. It must not perform network I/O.
	Route(ctx context.Context, c hippo.Call, budget float64) (Decision, error)
}

// Decision is the Router's response: which provider to call, which model
// to use, and how much it is expected to cost.
type Decision struct {
	// Provider is the hippo.Provider.Name to dispatch to.
	Provider string
	// Model is the concrete model id to pass to the provider.
	Model string
	// EstimatedCostUSD is the Router's pre-flight cost estimate.
	EstimatedCostUSD float64
	// Reason is a human-readable one-liner explaining the choice.
	Reason string
}

// Policy is a declarative description of how each TaskKind should be
// routed. It is what yaml.go unmarshals into.
type Policy struct {
	// Tasks maps each TaskKind to the policy governing it.
	Tasks map[hippo.TaskKind]TaskPolicy
}

// TaskPolicy is the per-TaskKind policy section.
type TaskPolicy struct {
	// Privacy is the minimum privacy tier required for this task.
	Privacy hippo.PrivacyTier
	// Prefer lists "provider:model" slugs in preference order.
	Prefer []string
	// Fallback lists "provider:model" slugs tried when Prefer entries
	// are unavailable (e.g. exceeded per-provider rate limits).
	Fallback []string
	// MaxCostUSD caps the per-call spend for this task. Zero means use
	// the Call's MaxCostUSD or the Brain budget.
	MaxCostUSD float64
}
