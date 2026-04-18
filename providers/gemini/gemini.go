// Package gemini implements the hippo Provider interface for Google's
// Gemini models via the generativelanguage.googleapis.com REST API.
package gemini

import (
	"context"
	"net/http"

	"github.com/mahdi-salmanzade/hippo"
)

// Provider is the Gemini implementation of hippo.Provider.
type Provider struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
	// TODO: default model, safety settings.
}

// Option configures a Provider during construction.
type Option func(*Provider)

// WithBaseURL overrides the default Gemini endpoint.
func WithBaseURL(u string) Option { return func(p *Provider) { p.baseURL = u } }

// WithHTTPClient supplies a custom *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(p *Provider) { p.httpClient = c } }

// New constructs a Provider bound to the supplied API key.
func New(apiKey string, opts ...Option) *Provider {
	p := &Provider{
		apiKey:  apiKey,
		baseURL: "https://generativelanguage.googleapis.com",
	}
	for _, o := range opts {
		o(p)
	}
	// TODO: default http.Client, model catalogue.
	return p
}

// Name returns "gemini".
func (p *Provider) Name() string { return "gemini" }

// Models returns the currently supported Gemini models.
func (p *Provider) Models() []hippo.ModelInfo { return nil }

// Privacy returns PrivacyCloudOK.
func (p *Provider) Privacy() hippo.PrivacyTier { return hippo.PrivacyCloudOK }

// EstimateCost returns a USD estimate without a network call.
func (p *Provider) EstimateCost(c hippo.Call) (float64, error) {
	_ = c
	return 0, nil
}

// Call executes a generateContent request synchronously.
func (p *Provider) Call(ctx context.Context, c hippo.Call) (*hippo.Response, error) {
	_ = ctx
	_ = c
	panic("gemini: Call not implemented")
}

// Stream executes a streamGenerateContent request.
func (p *Provider) Stream(ctx context.Context, c hippo.Call) (<-chan hippo.StreamChunk, error) {
	_ = ctx
	_ = c
	panic("gemini: Stream not implemented")
}
