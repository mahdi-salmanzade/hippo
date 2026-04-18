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

// PricingTable maps (provider, model) pairs to USD-per-token rates.
//
// Rates are stored per million tokens to match how providers publish
// their pricing. The canonical source is budget/pricing.yaml, embedded
// into the binary at build time; WithPricing lets tests or callers
// override it with a custom table.
type PricingTable struct {
	// Rates is keyed on "provider:model". The nested YAML layout is
	// flattened into this slug form at parse time so lookup is a
	// single map access.
	Rates map[string]Rate
}

// Rate is the per-million-token pricing for one model.
type Rate struct {
	// Input is USD per 1,000,000 input tokens.
	Input float64 `yaml:"input"`
	// Output is USD per 1,000,000 output tokens.
	Output float64 `yaml:"output"`
	// CacheWrite is USD per 1,000,000 cache-creation input tokens.
	// Zero means the provider does not surcharge for cache writes.
	CacheWrite float64 `yaml:"cache_write"`
	// CacheRead is USD per 1,000,000 cache-read input tokens. Zero
	// means the provider does not discount cached input.
	CacheRead float64 `yaml:"cache_read"`
}

// Lookup returns the Rate for "provider:model" or the zero Rate with
// ok=false if the slug is unknown. Callers should treat a missing
// entry as "price unknown" (best-effort $0) rather than an error —
// see the Tracker implementation and the Pass 3 spec.
func (p *PricingTable) Lookup(provider, model string) (Rate, bool) {
	if p == nil || p.Rates == nil {
		return Rate{}, false
	}
	slug := provider + ":" + model
	r, ok := p.Rates[slug]
	if ok {
		return r, true
	}
	// Tolerate dated model ids (e.g. "claude-haiku-4-5-20250930") by
	// matching the longest registered model prefix. Anthropic returns
	// the dated form in its response even when called with the alias.
	var bestKey string
	prefix := provider + ":"
	for k := range p.Rates {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if strings.HasPrefix(slug, k) && len(k) > len(bestKey) {
			bestKey = k
		}
	}
	if bestKey == "" {
		return Rate{}, false
	}
	return p.Rates[bestKey], true
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
// The input must be the nested provider → model → rate layout used by
// budget/pricing.yaml.
func ParsePricing(data []byte) (*PricingTable, error) {
	var nested map[string]map[string]Rate
	if err := yaml.Unmarshal(data, &nested); err != nil {
		return nil, fmt.Errorf("budget: unmarshal pricing: %w", err)
	}
	rates := make(map[string]Rate, len(nested)*4)
	for provider, models := range nested {
		for model, rate := range models {
			rates[provider+":"+model] = rate
		}
	}
	return &PricingTable{Rates: rates}, nil
}
