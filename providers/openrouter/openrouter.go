// Package openrouter implements the hippo Provider interface for
// OpenRouter, an aggregator that proxies requests to many upstream LLM
// providers through a single OpenAI-compatible API.
package openrouter

import (
	"context"
	"net/http"

	"github.com/mahdi-salmanzade/hippo"
)

// Provider is the OpenRouter implementation of hippo.Provider.
type Provider struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	// TODO: HTTP-Referer / X-Title attribution headers required by
	// OpenRouter for free-tier rate-limit tiering.
}

// Option configures a Provider during construction.
type Option func(*Provider)

// WithBaseURL overrides the default openrouter.ai/api/v1 endpoint.
func WithBaseURL(u string) Option { return func(p *Provider) { p.baseURL = u } }

// WithHTTPClient supplies a custom *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(p *Provider) { p.httpClient = c } }

// New constructs a Provider bound to the supplied API key.
func New(apiKey string, opts ...Option) *Provider {
	p := &Provider{
		apiKey:  apiKey,
		baseURL: "https://openrouter.ai/api/v1",
	}
	for _, o := range opts {
		o(p)
	}
	// TODO: default http.Client, model catalogue pulled from /models.
	return p
}

// Name returns "openrouter".
func (p *Provider) Name() string { return "openrouter" }

// Models returns the currently advertised OpenRouter models.
func (p *Provider) Models() []hippo.ModelInfo {
	// TODO: fetch /models on first access and cache.
	return nil
}

// Privacy returns PrivacyCloudOK.
func (p *Provider) Privacy() hippo.PrivacyTier { return hippo.PrivacyCloudOK }

// EstimateCost returns a USD estimate using OpenRouter's published rates.
func (p *Provider) EstimateCost(c hippo.Call) (float64, error) {
	_ = c
	return 0, nil
}

// Call executes a request via OpenRouter's OpenAI-compatible endpoint.
func (p *Provider) Call(ctx context.Context, c hippo.Call) (*hippo.Response, error) {
	_ = ctx
	_ = c
	panic("openrouter: Call not implemented")
}

// Stream executes a request via OpenRouter's streaming endpoint.
func (p *Provider) Stream(ctx context.Context, c hippo.Call) (<-chan hippo.StreamChunk, error) {
	_ = ctx
	_ = c
	panic("openrouter: Stream not implemented")
}
