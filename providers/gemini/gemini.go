// Package gemini implements the hippo Provider interface for Google's
// Gemini models via the generativelanguage.googleapis.com REST API.
package gemini

import (
	"context"
	"net/http"

	"github.com/mahdi-salmanzade/hippo"
)

type client struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	// TODO: default model, safety settings.
}

// Option configures a client during construction.
type Option func(*client)

// WithBaseURL overrides the default Gemini endpoint.
func WithBaseURL(u string) Option { return func(c *client) { c.baseURL = u } }

// WithHTTPClient supplies a custom *http.Client.
func WithHTTPClient(h *http.Client) Option { return func(c *client) { c.httpClient = h } }

// New constructs a Gemini provider bound to the supplied API key.
func New(apiKey string, opts ...Option) hippo.Provider {
	c := &client{
		apiKey:  apiKey,
		baseURL: "https://generativelanguage.googleapis.com",
	}
	for _, o := range opts {
		o(c)
	}
	// TODO: default http.Client, model catalogue.
	return c
}

// Name returns "gemini".
func (c *client) Name() string { return "gemini" }

// Models returns the currently supported Gemini models.
func (c *client) Models() []hippo.ModelInfo { return nil }

// Privacy returns PrivacyCloudOK.
func (c *client) Privacy() hippo.PrivacyTier { return hippo.PrivacyCloudOK }

// EstimateCost returns a USD estimate without a network call.
func (c *client) EstimateCost(call hippo.Call) (float64, error) {
	_ = call
	return 0, nil
}

// Call executes a generateContent request synchronously.
func (c *client) Call(ctx context.Context, call hippo.Call) (*hippo.Response, error) {
	_ = ctx
	_ = call
	panic("gemini: Call not implemented")
}

// Stream executes a streamGenerateContent request.
func (c *client) Stream(ctx context.Context, call hippo.Call) (<-chan hippo.StreamChunk, error) {
	_ = ctx
	_ = call
	panic("gemini: Stream not implemented")
}
