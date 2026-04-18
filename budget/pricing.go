package budget

import (
	_ "embed"
	"fmt"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed pricing.yaml
var pricingYAML []byte

// PricingTable is the parsed pricing.yaml: provider → model → rate.
// This is the canonical source for pricing across every provider
// hippo supports; providers call DefaultPricing().Lookup rather than
// maintaining their own duplicate tables.
//
// Lookups are O(1) on exact hits and O(M) for prefix-matched dated
// model ids, where M is the number of models registered for the
// target provider. That's small (single digits per provider) so the
// scan stays cheap enough to do on every Call.
type PricingTable struct {
	// Providers is keyed by Provider.Name() (e.g. "anthropic",
	// "openai"). The nested Models map holds per-model pricing.
	Providers map[string]ProviderPricing `yaml:"providers"`
}

// ProviderPricing groups a provider's model pricing under one key.
type ProviderPricing struct {
	// Models maps a concrete model id (e.g. "claude-haiku-4-5") to
	// its pricing. Prefix-match handles dated id variants like
	// "claude-haiku-4-5-20250930".
	Models map[string]ModelPricing `yaml:"models"`
	// ZeroCost, when true, tells Lookup that this provider's
	// inference is free for every model, registered or not. Unknown
	// model ids fall through to a zero ModelPricing with ok=true
	// rather than the usual (zero, false) "price unknown" signal.
	//
	// This exists for Ollama: users install their own models, so any
	// complete registry is impossible, and every installed model
	// prices at $0. Without this flag, budget.Charge would log a
	// noisy "unknown pricing" warning on every Ollama call for
	// models the user happens to run that we haven't listed.
	ZeroCost bool `yaml:"zero_cost,omitempty"`
}

// ModelPricing is the per-million-token pricing for one model plus a
// small amount of capability metadata used by EstimateCost heuristics.
//
// The two "cache" fields are kept separate because providers price
// caching differently:
//   - Anthropic: CacheReadPerMtok (cheap, 0.1× input) and
//     CacheWritePerMtok (premium, 1.25× input) are billed independently.
//   - OpenAI: CachedInputPerMtok is the single cached-input discount
//     rate; there is no cache-write surcharge on the Responses API.
//
// costOf picks whichever cached-rate field is non-zero for the rate,
// so the unified hippo.Usage.CachedTokens counter works across both.
type ModelPricing struct {
	InputPerMtok       float64 `yaml:"input_per_mtok"`
	OutputPerMtok      float64 `yaml:"output_per_mtok"`
	CacheReadPerMtok   float64 `yaml:"cache_read_per_mtok,omitempty"`
	CacheWritePerMtok  float64 `yaml:"cache_write_per_mtok,omitempty"`
	CachedInputPerMtok float64 `yaml:"cached_input_per_mtok,omitempty"`
	ContextWindow      int     `yaml:"context_window"`
	SupportsReasoning  bool    `yaml:"supports_reasoning,omitempty"`
}

// Lookup returns the ModelPricing for (provider, model). If the exact
// model id is not registered, Lookup falls back to the longest
// registered key that is a prefix of model — this is how we tolerate
// dated variants like "claude-haiku-4-5-20250930" that the Anthropic
// API echoes back, or "gpt-5-2026-02-15" from OpenAI.
//
// An unknown (provider, model) pair returns the zero ModelPricing with
// ok=false. Callers should treat that as "price unknown" (best-effort
// $0) rather than a fatal error.
func (t *PricingTable) Lookup(provider, model string) (ModelPricing, bool) {
	if t == nil || t.Providers == nil {
		return ModelPricing{}, false
	}
	pp, ok := t.Providers[provider]
	if !ok {
		return ModelPricing{}, false
	}
	if m, ok := pp.Models[model]; ok {
		return m, true
	}
	var bestKey string
	for k := range pp.Models {
		if strings.HasPrefix(model, k) && len(k) > len(bestKey) {
			bestKey = k
		}
	}
	if bestKey != "" {
		return pp.Models[bestKey], true
	}
	if pp.ZeroCost {
		// Fallthrough for zero-cost providers (Ollama): unknown
		// model ids return an all-zero ModelPricing with ok=true so
		// budget.Charge silently records a $0 spend instead of
		// warning on every call for a locally-installed model the
		// YAML table doesn't happen to list.
		return ModelPricing{}, true
	}
	return ModelPricing{}, false
}

// Models returns the model ids registered under provider. Order is
// unspecified; callers that need deterministic ordering must sort.
func (t *PricingTable) Models(provider string) []string {
	if t == nil || t.Providers == nil {
		return nil
	}
	pp, ok := t.Providers[provider]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(pp.Models))
	for k := range pp.Models {
		out = append(out, k)
	}
	return out
}

var (
	defaultOnce    sync.Once
	defaultPricing *PricingTable
	defaultErr     error
)

// DefaultPricing returns the table parsed from the embedded
// pricing.yaml. Parsed once and memoised; subsequent calls return the
// same pointer. The returned *PricingTable is safe for concurrent
// reads but must not be mutated — construct a fresh copy via
// ParsePricing if you need to edit.
func DefaultPricing() *PricingTable {
	defaultOnce.Do(func() {
		defaultPricing, defaultErr = ParsePricing(pricingYAML)
	})
	if defaultErr != nil {
		// A parse failure on an embedded asset is a build-time
		// mistake; panic is appropriate (it can only happen if
		// someone committed malformed YAML).
		panic(fmt.Sprintf("budget: parse embedded pricing.yaml: %v", defaultErr))
	}
	return defaultPricing
}

// ParsePricing unmarshals a pricing-yaml document into a PricingTable.
// The input must be the nested providers → models layout used by
// budget/pricing.yaml.
func ParsePricing(data []byte) (*PricingTable, error) {
	var t PricingTable
	if err := yaml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("budget: unmarshal pricing: %w", err)
	}
	return &t, nil
}
