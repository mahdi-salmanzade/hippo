// Package router holds implementations of hippo.Router together with
// the configuration types they consume.
//
// The Router interface and Decision return type live in the root hippo
// package; this package imports them. Policy and TaskPolicy here are the
// declarative config shape consumed by YAMLRouter — not every Router
// implementation has to use them.
package router

import "github.com/mahdi-salmanzade/hippo"

// Policy is the declarative description of how each TaskKind should be
// routed. YAMLRouter unmarshals YAML into this shape.
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
