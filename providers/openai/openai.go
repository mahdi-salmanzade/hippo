// Package openai implements the hippo Provider interface for OpenAI's
// Chat Completions and Responses APIs.
package openai

import (
	"context"
	"net/http"

	"github.com/mahdi-salmanzade/hippo"
)

// Provider is the OpenAI implementation of hippo.Provider.
type Provider struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	// TODO: organization id, project id, default model.
}

// Option configures a Provider during construction.
type Option func(*Provider)

// WithBaseURL overrides the default api.openai.com endpoint.
func WithBaseURL(u string) Option { return func(p *Provider) { p.baseURL = u } }

// WithHTTPClient supplies a custom *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(p *Provider) { p.httpClient = c } }

// New constructs a Provider bound to the supplied API key.
func New(apiKey string, opts ...Option) *Provider {
	p := &Provider{apiKey: apiKey, baseURL: "https://api.openai.com"}
	for _, o := range opts {
		o(p)
	}
	// TODO: default http.Client, model catalogue.
	return p
}

// Name returns "openai".
func (p *Provider) Name() string { return "openai" }

// Models returns the currently supported OpenAI models.
func (p *Provider) Models() []hippo.ModelInfo {
	// TODO: populate GPT-4x and embedding entries.
	return nil
}

// Privacy returns PrivacyCloudOK.
func (p *Provider) Privacy() hippo.PrivacyTier { return hippo.PrivacyCloudOK }

// EstimateCost returns a USD estimate without a network call.
func (p *Provider) EstimateCost(c hippo.Call) (float64, error) {
	_ = c
	// TODO: tokenize + lookup.
	return 0, nil
}

// Call executes a Chat Completions request synchronously.
func (p *Provider) Call(ctx context.Context, c hippo.Call) (*hippo.Response, error) {
	_ = ctx
	_ = c
	// TODO: build request, POST /v1/chat/completions, parse response.
	panic("openai: Call not implemented")
}

// Stream executes a Chat Completions request in streaming mode.
func (p *Provider) Stream(ctx context.Context, c hippo.Call) (<-chan hippo.StreamChunk, error) {
	_ = ctx
	_ = c
	// TODO: SSE stream via internal/sse, map deltas, emit Final.
	panic("openai: Stream not implemented")
}
