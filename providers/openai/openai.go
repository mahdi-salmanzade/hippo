// Package openai implements the hippo Provider interface for OpenAI's
// Chat Completions and Responses APIs.
package openai

import (
	"context"
	"net/http"

	"github.com/mahdi-salmanzade/hippo"
)

type client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	// TODO: organization id, project id, default model.
}

// Option configures a client during construction.
type Option func(*client)

// WithBaseURL overrides the default api.openai.com endpoint.
func WithBaseURL(u string) Option { return func(c *client) { c.baseURL = u } }

// WithHTTPClient supplies a custom *http.Client.
func WithHTTPClient(h *http.Client) Option { return func(c *client) { c.httpClient = h } }

// New constructs an OpenAI provider bound to the supplied API key.
func New(apiKey string, opts ...Option) hippo.Provider {
	c := &client{apiKey: apiKey, baseURL: "https://api.openai.com"}
	for _, o := range opts {
		o(c)
	}
	// TODO: default http.Client, model catalogue.
	return c
}

// Name returns "openai".
func (c *client) Name() string { return "openai" }

// Models returns the currently supported OpenAI models.
func (c *client) Models() []hippo.ModelInfo {
	// TODO: populate GPT-4x and embedding entries.
	return nil
}

// Privacy returns PrivacyCloudOK.
func (c *client) Privacy() hippo.PrivacyTier { return hippo.PrivacyCloudOK }

// EstimateCost returns a USD estimate without a network call.
func (c *client) EstimateCost(call hippo.Call) (float64, error) {
	_ = call
	// TODO: tokenize + lookup.
	return 0, nil
}

// Call executes a Chat Completions request synchronously.
func (c *client) Call(ctx context.Context, call hippo.Call) (*hippo.Response, error) {
	_ = ctx
	_ = call
	// TODO: build request, POST /v1/chat/completions, parse response.
	panic("openai: Call not implemented")
}

// Stream executes a Chat Completions request in streaming mode.
func (c *client) Stream(ctx context.Context, call hippo.Call) (<-chan hippo.StreamChunk, error) {
	_ = ctx
	_ = call
	// TODO: SSE stream via internal/sse, map deltas, emit Final.
	panic("openai: Stream not implemented")
}
