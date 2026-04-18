// Package ollama implements the hippo Provider interface for a local
// Ollama daemon at http://localhost:11434.
//
// Ollama is the canonical PrivacyLocalOnly provider: its Privacy tier
// reflects that inference never leaves the host machine.
package ollama

import (
	"context"
	"net/http"

	"github.com/mahdi-salmanzade/hippo"
)

// Provider is the Ollama implementation of hippo.Provider.
type Provider struct {
	baseURL    string
	httpClient *http.Client
	// TODO: keep-alive settings, default model.
}

// Option configures a Provider during construction.
type Option func(*Provider)

// WithBaseURL overrides the default http://localhost:11434 endpoint.
func WithBaseURL(u string) Option { return func(p *Provider) { p.baseURL = u } }

// WithHTTPClient supplies a custom *http.Client.
func WithHTTPClient(c *http.Client) Option { return func(p *Provider) { p.httpClient = c } }

// New constructs a Provider. Ollama does not use API keys; the endpoint
// is reachable or it isn't.
func New(opts ...Option) *Provider {
	p := &Provider{baseURL: "http://localhost:11434"}
	for _, o := range opts {
		o(p)
	}
	// TODO: default http.Client, probe endpoint for installed models.
	return p
}

// Name returns "ollama".
func (p *Provider) Name() string { return "ollama" }

// Models returns the models installed in the local Ollama daemon.
func (p *Provider) Models() []hippo.ModelInfo {
	// TODO: call /api/tags and map into ModelInfo.
	return nil
}

// Privacy returns PrivacyLocalOnly.
func (p *Provider) Privacy() hippo.PrivacyTier { return hippo.PrivacyLocalOnly }

// EstimateCost returns 0 for local Ollama calls (no monetary cost).
func (p *Provider) EstimateCost(c hippo.Call) (float64, error) {
	_ = c
	return 0, nil
}

// Call executes a local chat request.
func (p *Provider) Call(ctx context.Context, c hippo.Call) (*hippo.Response, error) {
	_ = ctx
	_ = c
	// TODO: POST /api/chat, parse, return.
	panic("ollama: Call not implemented")
}

// Stream executes a local chat request in streaming mode.
func (p *Provider) Stream(ctx context.Context, c hippo.Call) (<-chan hippo.StreamChunk, error) {
	_ = ctx
	_ = c
	// TODO: NDJSON stream from /api/chat with stream=true.
	panic("ollama: Stream not implemented")
}
