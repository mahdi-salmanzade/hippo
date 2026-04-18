package anthropic

import (
	"strings"

	"github.com/mahdi-salmanzade/hippo"
)

// defaultModel is used when WithModel is not supplied and the incoming
// Call does not pin a model. Opus is hippo's out-of-the-box "just works"
// pick; callers who care about cost will pin a cheaper model.
const defaultModel = "claude-opus-4-7"

// modelPricing is per-1M-token pricing for one Anthropic model.
//
// Anthropic bills input tokens in three buckets: plain input, cache
// writes (cache_creation_input_tokens, 1.25x input rate), and cache
// reads (cache_read_input_tokens, 0.1x input rate). Output tokens are
// billed once.
type modelPricing struct {
	inputPerMTok      float64
	outputPerMTok     float64
	cacheWritePerMTok float64
	cacheReadPerMTok  float64
}

// pricing is the per-model price table used by cost estimation and the
// post-hoc cost computation in Call.
//
// Rates are USD per 1,000,000 tokens, sourced from Anthropic's public
// pricing page. Update when Anthropic changes prices; this table is the
// single source of truth for the provider.
var pricing = map[string]modelPricing{
	"claude-opus-4-7":   {inputPerMTok: 15.00, outputPerMTok: 75.00, cacheWritePerMTok: 18.75, cacheReadPerMTok: 1.50},
	"claude-sonnet-4-6": {inputPerMTok: 3.00, outputPerMTok: 15.00, cacheWritePerMTok: 3.75, cacheReadPerMTok: 0.30},
	"claude-haiku-4-5":  {inputPerMTok: 1.00, outputPerMTok: 5.00, cacheWritePerMTok: 1.25, cacheReadPerMTok: 0.10},
}

// modelCatalog is the list returned by Provider.Models. Each entry
// matches a key in pricing so cost calculations and the public model
// catalogue stay in sync.
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

// lookupPricing resolves a model id to a modelPricing entry.
//
// Anthropic's Messages API often echoes back a dated model id like
// "claude-haiku-4-5-20250930" when the request was made against the
// alias "claude-haiku-4-5". We match the longest pricing-table key
// that is a prefix of model, so either form resolves correctly.
func lookupPricing(model string) (modelPricing, bool) {
	if p, ok := pricing[model]; ok {
		return p, true
	}
	var bestKey string
	for k := range pricing {
		if strings.HasPrefix(model, k) && len(k) > len(bestKey) {
			bestKey = k
		}
	}
	if bestKey == "" {
		return modelPricing{}, false
	}
	return pricing[bestKey], true
}

// computeCost returns the USD cost of a response with the given token
// counts against model's pricing entry. Models unknown to the table
// return 0 and a false ok — callers should surface that to the user
// rather than silently pricing at zero.
func computeCost(model string, inputTokens, outputTokens, cacheCreationTokens, cacheReadTokens int) (cost float64, ok bool) {
	p, ok := lookupPricing(model)
	if !ok {
		return 0, false
	}
	const perMillion = 1_000_000.0
	cost = float64(inputTokens)*p.inputPerMTok/perMillion +
		float64(outputTokens)*p.outputPerMTok/perMillion +
		float64(cacheCreationTokens)*p.cacheWritePerMTok/perMillion +
		float64(cacheReadTokens)*p.cacheReadPerMTok/perMillion
	return cost, true
}
