package anthropic

import (
	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/budget"
)

// defaultModel is used when WithModel is not supplied and the incoming
// Call does not pin a model. Opus is hippo's out-of-the-box "just works"
// pick; callers who care about cost will pin a cheaper model.
const defaultModel = "claude-opus-4-7"

// modelCatalog is the list returned by Provider.Models.
//
// Rates live in budget/pricing.yaml (the canonical source); this
// catalogue holds the provider-specific metadata that pricing doesn't
// cover - display names, per-model max output tokens, and capability
// flags. The entries here must stay aligned with the Anthropic entries
// in budget/pricing.yaml so Models() and EstimateCost() describe the
// same set of models.
var modelCatalog = []hippo.ModelInfo{
	{
		ID:                "claude-opus-4-7",
		DisplayName:       "Claude Opus 4.7",
		ContextTokens:     200_000,
		MaxOutputTokens:   32_000,
		SupportsTools:     true,
		SupportsStreaming: true,
	},
	{
		ID:                "claude-sonnet-4-6",
		DisplayName:       "Claude Sonnet 4.6",
		ContextTokens:     200_000,
		MaxOutputTokens:   64_000,
		SupportsTools:     true,
		SupportsStreaming: true,
	},
	{
		ID:                "claude-haiku-4-5",
		DisplayName:       "Claude Haiku 4.5",
		ContextTokens:     200_000,
		MaxOutputTokens:   64_000,
		SupportsTools:     true,
		SupportsStreaming: true,
	},
}

// lookupPricing resolves a model id to its ModelPricing entry, via
// budget.DefaultPricing().Lookup. Exists as a thin shim so the rest of
// the Anthropic adapter doesn't reach into budget directly in three
// separate places. Prefix-matching on dated ids (e.g.
// "claude-haiku-4-5-20250930") is handled inside budget.Lookup.
func lookupPricing(model string) (budget.ModelPricing, bool) {
	return budget.DefaultPricing().Lookup("anthropic", model)
}

// computeCost returns the USD cost of a response with the given token
// counts. Anthropic bills cache writes at a premium rate distinct from
// plain input, so we compute from the raw per-bucket counts here rather
// than going through BudgetTracker.EstimateCost (which only sees the
// collapsed CachedTokens total).
//
// Returns (0, false) for models unknown to the pricing table; callers
// surface that to the user instead of silently pricing at zero.
func computeCost(model string, inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int) (cost float64, ok bool) {
	p, ok := lookupPricing(model)
	if !ok {
		return 0, false
	}
	const perMillion = 1_000_000.0
	cost = float64(inputTokens)*p.InputPerMtok/perMillion +
		float64(outputTokens)*p.OutputPerMtok/perMillion +
		float64(cacheCreationTokens)*p.CacheWritePerMtok/perMillion +
		float64(cacheReadTokens)*p.CacheReadPerMtok/perMillion
	return cost, true
}
