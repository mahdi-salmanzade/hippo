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

// client is the Anthropic Messages-API implementation of hippo.Provider.
// It is unexported on purpose: users interact with it only through the
// hippo.Provider returned by New.
type client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	// TODO: model catalogue, default model, extended-thinking toggle.
}

// Option configures a client during construction.
type Option func(*client)

// WithBaseURL overrides the default api.anthropic.com endpoint. Useful
// for testing against a local proxy.
func WithBaseURL(u string) Option { return func(c *client) { c.baseURL = u } }

// WithHTTPClient supplies a custom *http.Client. When unset, a client
// with a 60s timeout is used.
func WithHTTPClient(h *http.Client) Option { return func(c *client) { c.httpClient = h } }

// New constructs an Anthropic provider bound to the supplied API key.
func New(apiKey string, opts ...Option) hippo.Provider {
	c := &client{
		apiKey:  apiKey,
		baseURL: "https://api.anthropic.com",
	}
	for _, o := range opts {
		o(c)
	}
	// TODO: default http.Client with timeout, seed model catalogue.
	return c
}

// Name returns "anthropic".
func (c *client) Name() string { return "anthropic" }

// Models returns the currently supported Claude models.
func (c *client) Models() []hippo.ModelInfo {
	// TODO: populate with Opus/Sonnet/Haiku 4.x entries.
	return nil
}

// Privacy returns PrivacyCloudOK. Anthropic is a hosted provider.
func (c *client) Privacy() hippo.PrivacyTier { return hippo.PrivacyCloudOK }

// EstimateCost returns a USD estimate for call using the configured
// pricing table. It does not perform a network call.
func (c *client) EstimateCost(call hippo.Call) (float64, error) {
	_ = call
	// TODO: tokenize prompt, look up model rate, return estimate.
	return 0, nil
}

// Call executes a Messages request synchronously.
func (c *client) Call(ctx context.Context, call hippo.Call) (*hippo.Response, error) {
	_ = ctx
	_ = call
	// TODO: build Messages API request, POST, parse response, compute cost.
	panic("anthropic: Call not implemented")
}

// Stream executes a Messages request in streaming mode. The returned
// channel is closed when the stream terminates.
func (c *client) Stream(ctx context.Context, call hippo.Call) (<-chan hippo.StreamChunk, error) {
	_ = ctx
	_ = call
	// TODO: SSE stream via internal/sse, accumulate tool args, emit on Final.
	panic("anthropic: Stream not implemented")
}
