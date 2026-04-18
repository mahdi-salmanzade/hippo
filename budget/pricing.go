package budget

// PricingTable maps (provider, model) pairs to USD-per-token rates.
//
// Rates are stored per million tokens to match how providers publish
// their pricing. The table is loaded from pricing.yaml (embedded in the
// binary at build time via go:embed) and may be overridden at runtime
// for testing.
type PricingTable struct {
	// Entries maps the slug "provider:model" to its Rate.
	Entries map[string]Rate
}

// Rate is the per-million-token pricing for one model.
type Rate struct {
	// InputUSDPerM is USD per 1,000,000 input tokens.
	InputUSDPerM float64
	// OutputUSDPerM is USD per 1,000,000 output tokens.
	OutputUSDPerM float64
	// CachedInputUSDPerM is the rate for cache-hit input tokens. If
	// zero, assume the provider does not discount cached input.
	CachedInputUSDPerM float64
	// EmbeddingUSDPerM is for embedding models; zero for chat models.
	EmbeddingUSDPerM float64
}

// DefaultPricing returns the pricing table embedded at build time. The
// returned *PricingTable is safe to read concurrently but must not be
// mutated; construct a copy first.
func DefaultPricing() *PricingTable {
	// TODO: parse the embedded pricing.yaml (once) and return the
	// singleton. Use sync.Once.
	return &PricingTable{Entries: map[string]Rate{}}
}

// Lookup returns the Rate for "provider:model" or the zero Rate if the
// slug is unknown. Callers should treat a zero Rate as "price unknown"
// and fall back to conservative estimation.
func (p *PricingTable) Lookup(providerModel string) Rate {
	if p == nil || p.Entries == nil {
		return Rate{}
	}
	return p.Entries[providerModel]
}
