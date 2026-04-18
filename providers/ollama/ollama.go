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

type client struct {
	baseURL    string
	httpClient *http.Client
	// TODO: keep-alive settings, default model.
}

// Option configures a client during construction.
type Option func(*client)

// WithBaseURL overrides the default http://localhost:11434 endpoint.
func WithBaseURL(u string) Option { return func(c *client) { c.baseURL = u } }

// WithHTTPClient supplies a custom *http.Client.
func WithHTTPClient(h *http.Client) Option { return func(c *client) { c.httpClient = h } }

// New constructs an Ollama provider. Ollama does not use API keys; the
// endpoint is reachable or it isn't.
func New(opts ...Option) hippo.Provider {
	c := &client{baseURL: "http://localhost:11434"}
	for _, o := range opts {
		o(c)
	}
	// TODO: default http.Client, probe endpoint for installed models.
	return c
}

// Name returns "ollama".
func (c *client) Name() string { return "ollama" }

// Models returns the models installed in the local Ollama daemon.
func (c *client) Models() []hippo.ModelInfo {
	// TODO: call /api/tags and map into ModelInfo.
	return nil
}

// Privacy returns PrivacyLocalOnly.
func (c *client) Privacy() hippo.PrivacyTier { return hippo.PrivacyLocalOnly }

// EstimateCost returns 0 for local Ollama calls (no monetary cost).
func (c *client) EstimateCost(call hippo.Call) (float64, error) {
	_ = call
	return 0, nil
}

// Call executes a local chat request.
func (c *client) Call(ctx context.Context, call hippo.Call) (*hippo.Response, error) {
	_ = ctx
	_ = call
	// TODO: POST /api/chat, parse, return.
	panic("ollama: Call not implemented")
}

// Stream executes a local chat request in streaming mode.
func (c *client) Stream(ctx context.Context, call hippo.Call) (<-chan hippo.StreamChunk, error) {
	_ = ctx
	_ = call
	// TODO: NDJSON stream from /api/chat with stream=true.
	panic("ollama: Stream not implemented")
}
