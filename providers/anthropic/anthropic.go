// Package anthropic implements the hippo Provider interface for
// Anthropic's Claude models via the Messages API.
//
// This package talks directly to api.anthropic.com over net/http; it does
// not depend on Anthropic's Go SDK. Streaming uses SSE via the
// internal/sse helper.
package anthropic

import (
	"context"
	"net/http"

	"github.com/mahdi-salmanzade/hippo"
)

// Provider is the Anthropic Messages-API implementation of hippo.Provider.
type Provider struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	// TODO: model catalogue, default model, extended-thinking toggle.
}

// Option configures a Provider during construction.
type Option func(*Provider)

// WithBaseURL overrides the default api.anthropic.com endpoint. Useful
// for testing against a local proxy.
func WithBaseURL(u string) Option { return func(p *Provider) { p.baseURL = u } }

// WithHTTPClient supplies a custom *http.Client. When unset, a client
// with a 60s timeout is used.
func WithHTTPClient(c *http.Client) Option { return func(p *Provider) { p.httpClient = c } }

// New constructs a Provider bound to the supplied API key.
func New(apiKey string, opts ...Option) *Provider {
	p := &Provider{
		apiKey:  apiKey,
		baseURL: "https://api.anthropic.com",
	}
	for _, o := range opts {
		o(p)
	}
	// TODO: default http.Client with timeout, seed model catalogue.
	return p
}

// Name returns "anthropic".
func (p *Provider) Name() string { return "anthropic" }

// Models returns the currently supported Claude models.
func (p *Provider) Models() []hippo.ModelInfo {
	// TODO: populate with Opus/Sonnet/Haiku 4.x entries.
	return nil
}

// Privacy returns PrivacyCloudOK. Anthropic is a hosted provider.
func (p *Provider) Privacy() hippo.PrivacyTier { return hippo.PrivacyCloudOK }

// EstimateCost returns a USD estimate for c using the configured pricing
// table. It does not perform a network call.
func (p *Provider) EstimateCost(c hippo.Call) (float64, error) {
	_ = c
	// TODO: tokenize prompt, look up model rate, return estimate.
	return 0, nil
}

// Call executes a Messages request synchronously.
func (p *Provider) Call(ctx context.Context, c hippo.Call) (*hippo.Response, error) {
	_ = ctx
	_ = c
	// TODO: build Messages API request, POST, parse response, compute cost.
	panic("anthropic: Call not implemented")
}

// Stream executes a Messages request in streaming mode. The returned
// channel is closed when the stream terminates.
func (p *Provider) Stream(ctx context.Context, c hippo.Call) (<-chan hippo.StreamChunk, error) {
	_ = ctx
	_ = c
	// TODO: SSE stream via internal/sse, accumulate tool args, emit on Final.
	panic("anthropic: Stream not implemented")
}
