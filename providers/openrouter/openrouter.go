// Package openrouter implements the hippo Provider interface for
// OpenRouter, an aggregator that proxies requests to many upstream LLM
// providers through a single OpenAI-compatible API.
package openrouter

import (
	"context"
	"net/http"

	"github.com/mahdi-salmanzade/hippo"
)

type client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	// TODO: HTTP-Referer / X-Title attribution headers required by
	// OpenRouter for free-tier rate-limit tiering.
}

// Option configures a client during construction.
type Option func(*client)

// WithBaseURL overrides the default openrouter.ai/api/v1 endpoint.
func WithBaseURL(u string) Option { return func(c *client) { c.baseURL = u } }

// WithHTTPClient supplies a custom *http.Client.
func WithHTTPClient(h *http.Client) Option { return func(c *client) { c.httpClient = h } }

// New constructs an OpenRouter provider bound to the supplied API key.
func New(apiKey string, opts ...Option) hippo.Provider {
	c := &client{
		apiKey:  apiKey,
		baseURL: "https://openrouter.ai/api/v1",
	}
	for _, o := range opts {
		o(c)
	}
	// TODO: default http.Client, model catalogue pulled from /models.
	return c
}

// Name returns "openrouter".
func (c *client) Name() string { return "openrouter" }

// Models returns the currently advertised OpenRouter models.
func (c *client) Models() []hippo.ModelInfo {
	// TODO: fetch /models on first access and cache.
	return nil
}

// Privacy returns PrivacyCloudOK.
func (c *client) Privacy() hippo.PrivacyTier { return hippo.PrivacyCloudOK }

// EstimateCost returns a USD estimate using OpenRouter's published rates.
func (c *client) EstimateCost(call hippo.Call) (float64, error) {
	_ = call
	return 0, nil
}

// Call executes a request via OpenRouter's OpenAI-compatible endpoint.
func (c *client) Call(ctx context.Context, call hippo.Call) (*hippo.Response, error) {
	_ = ctx
	_ = call
	panic("openrouter: Call not implemented")
}

// Stream executes a request via OpenRouter's streaming endpoint.
func (c *client) Stream(ctx context.Context, call hippo.Call) (<-chan hippo.StreamChunk, error) {
	_ = ctx
	_ = call
	panic("openrouter: Stream not implemented")
}
