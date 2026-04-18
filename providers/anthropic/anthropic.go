// Package anthropic implements a hippo.Provider backed by Anthropic's
// Messages API (POST /v1/messages).
//
// This package talks directly to api.anthropic.com over net/http; it
// does not depend on any Anthropic SDK. Streaming, tool calling, and
// extended thinking are deliberately out of scope for the first pass;
// see Stream (returns ErrNotImplemented) and the TODO notes.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

// anthropicVersion is the required API-version header value for the
// Messages API. Bumping it is a breaking change for callers who depend
// on specific response shapes, so pin an explicit stable value.
const anthropicVersion = "2023-06-01"

// defaultTimeout is the default *http.Client timeout. Overridable via
// WithTimeout; fully replaceable via WithHTTPClient.
const defaultTimeout = 60 * time.Second

// defaultBaseURL is Anthropic's production endpoint. WithBaseURL
// overrides it (for test doubles and corporate proxies).
const defaultBaseURL = "https://api.anthropic.com"

// provider implements hippo.Provider. It is unexported: callers only
// interact with the hippo.Provider returned by New.
type provider struct {
	apiKey     string
	model      string
	baseURL    string
	timeout    time.Duration
	httpClient *http.Client
}

// Option configures a provider during construction.
type Option func(*provider)

// WithAPIKey supplies the Anthropic API key. Required; New returns an
// error if this option is omitted.
func WithAPIKey(key string) Option {
	return func(p *provider) { p.apiKey = key }
}

// WithModel sets the default model id. If a Call specifies Call.Model,
// it wins; otherwise this value is used. Default: claude-opus-4-7.
func WithModel(model string) Option {
	return func(p *provider) { p.model = model }
}

// WithBaseURL overrides the default https://api.anthropic.com endpoint.
// Useful for httptest servers and corporate proxies.
func WithBaseURL(u string) Option {
	return func(p *provider) { p.baseURL = u }
}

// WithTimeout overrides the default 60s *http.Client timeout. Ignored
// if WithHTTPClient is also supplied.
func WithTimeout(d time.Duration) Option {
	return func(p *provider) { p.timeout = d }
}

// WithHTTPClient replaces the internal *http.Client entirely. Use it
// when you need full control (custom transports, test doubles, proxies
// with client certs). WithTimeout is ignored when this is set.
func WithHTTPClient(c *http.Client) Option {
	return func(p *provider) { p.httpClient = c }
}

// New returns a hippo.Provider backed by the Anthropic Messages API.
// WithAPIKey is required; every other option has a sane default.
func New(opts ...Option) (hippo.Provider, error) {
	p := &provider{
		model:   defaultModel,
		baseURL: defaultBaseURL,
		timeout: defaultTimeout,
	}
	for _, o := range opts {
		o(p)
	}
	if p.apiKey == "" {
		return nil, errors.New("anthropic: API key is required (use WithAPIKey)")
	}
	if p.httpClient == nil {
		p.httpClient = &http.Client{Timeout: p.timeout}
	}
	return p, nil
}

// Name returns "anthropic".
func (p *provider) Name() string { return "anthropic" }

// Models returns a defensive copy of the static model catalogue.
func (p *provider) Models() []hippo.ModelInfo {
	out := make([]hippo.ModelInfo, len(modelCatalog))
	copy(out, modelCatalog)
	return out
}

// Privacy returns PrivacyCloudOK. Anthropic is a hosted API.
func (p *provider) Privacy() hippo.PrivacyTier { return hippo.PrivacyCloudOK }

// EstimateCost returns a pre-flight USD estimate for c. It uses a
// rough len/4 heuristic for input tokens and assumes max_tokens output,
// which is enough for a router's budget-feasibility check — the actual
// cost after a Call is computed from the server-reported usage.
func (p *provider) EstimateCost(c hippo.Call) (float64, error) {
	model := p.model
	if c.Model != "" {
		model = c.Model
	}
	pr, ok := lookupPricing(model)
	if !ok {
		return 0, fmt.Errorf("anthropic: unknown model %q", model)
	}
	inputChars := len(c.Prompt)
	for _, m := range c.Messages {
		inputChars += len(m.Content)
	}
	inputTokens := inputChars / 4
	output := c.MaxTokens
	if output == 0 {
		output = 1024
	}
	const perMillion = 1_000_000.0
	return float64(inputTokens)*pr.inputPerMTok/perMillion +
		float64(output)*pr.outputPerMTok/perMillion, nil
}

// Wire-format types for the Messages API. These are package-private so
// we can evolve them without leaking shape into the hippo public API.

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesRequest struct {
	Model     string        `json:"model"`
	Messages  []wireMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
	System    string        `json:"system,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type wireUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type messagesResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Content    []contentBlock `json:"content"`
	Model      string         `json:"model"`
	StopReason string         `json:"stop_reason"`
	Usage      wireUsage      `json:"usage"`
}

type errorResponse struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Call implements hippo.Provider.
func (p *provider) Call(ctx context.Context, c hippo.Call) (*hippo.Response, error) {
	model := p.model
	if c.Model != "" {
		model = c.Model
	}
	maxTokens := c.MaxTokens
	if maxTokens == 0 {
		maxTokens = 1024
	}

	req, err := p.buildRequestBody(c, model, maxTokens)
	if err != nil {
		return nil, err
	}
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	start := time.Now()
	statusCode, respBody, err := p.doWithRetry(ctx, reqBody)
	if err != nil {
		return nil, err
	}
	latencyMS := time.Since(start).Milliseconds()

	if statusCode < 200 || statusCode >= 300 {
		return nil, classifyHTTPError(statusCode, respBody)
	}

	var mr messagesResponse
	if err := json.Unmarshal(respBody, &mr); err != nil {
		return nil, fmt.Errorf("anthropic: parse response: %w", err)
	}

	var text strings.Builder
	for _, block := range mr.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}

	cached := mr.Usage.CacheCreationInputTokens + mr.Usage.CacheReadInputTokens
	cost, _ := computeCost(mr.Model, mr.Usage.InputTokens, mr.Usage.OutputTokens,
		mr.Usage.CacheCreationInputTokens, mr.Usage.CacheReadInputTokens)

	return &hippo.Response{
		Text: text.String(),
		Usage: hippo.Usage{
			InputTokens:  mr.Usage.InputTokens,
			OutputTokens: mr.Usage.OutputTokens,
			CachedTokens: cached,
		},
		CostUSD:    cost,
		Provider:   "anthropic",
		Model:      mr.Model,
		LatencyMS:  latencyMS,
		ReceivedAt: time.Now(),
	}, nil
}

// Stream is not yet implemented. A later pass will wire SSE via
// internal/sse; for now, callers receive ErrNotImplemented.
func (p *provider) Stream(ctx context.Context, c hippo.Call) (<-chan hippo.StreamChunk, error) {
	return nil, hippo.ErrNotImplemented
}

// buildRequestBody maps a hippo.Call into the Anthropic Messages API
// request shape. System-role messages are folded into the top-level
// "system" field; user/assistant messages are passed through as-is.
// Call.Prompt, when set, is appended as a final user-role message.
func (p *provider) buildRequestBody(c hippo.Call, model string, maxTokens int) (messagesRequest, error) {
	var systemParts []string
	var msgs []wireMessage
	for _, m := range c.Messages {
		if m.Role == "system" {
			systemParts = append(systemParts, m.Content)
			continue
		}
		msgs = append(msgs, wireMessage{Role: m.Role, Content: m.Content})
	}
	if c.Prompt != "" {
		msgs = append(msgs, wireMessage{Role: "user", Content: c.Prompt})
	}
	if len(msgs) == 0 {
		return messagesRequest{}, errors.New("anthropic: Call requires Prompt or at least one user/assistant Message")
	}
	return messagesRequest{
		Model:     model,
		Messages:  msgs,
		MaxTokens: maxTokens,
		System:    strings.Join(systemParts, "\n\n"),
	}, nil
}

// retryBaseDelay is the base backoff delay between retry attempts.
// It is a var (not const) so tests can compress it to microseconds;
// production code must not mutate it. The retry schedule uses this as
// the first sleep and doubles on each subsequent attempt.
var retryBaseDelay = 1 * time.Second

// doWithRetry POSTs reqBody to /v1/messages, retrying on 429 and 5xx
// with exponential backoff (1s, 2s) up to 3 attempts total. Network
// errors are not retried — they return immediately, wrapping ctx errors
// when the context is the cause.
func (p *provider) doWithRetry(ctx context.Context, reqBody []byte) (int, []byte, error) {
	const maxAttempts = 3
	var (
		statusCode int
		respBody   []byte
	)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * retryBaseDelay
			select {
			case <-ctx.Done():
				return 0, nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			p.baseURL+"/v1/messages", bytes.NewReader(reqBody))
		if err != nil {
			return 0, nil, fmt.Errorf("anthropic: build request: %w", err)
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-api-key", p.apiKey)
		req.Header.Set("anthropic-version", anthropicVersion)

		httpResp, err := p.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, ctx.Err()
			}
			return 0, nil, fmt.Errorf("anthropic: request failed: %w", err)
		}

		body, readErr := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		if readErr != nil {
			return 0, nil, fmt.Errorf("anthropic: read response: %w", readErr)
		}

		statusCode = httpResp.StatusCode
		respBody = body

		if statusCode == http.StatusTooManyRequests || statusCode >= 500 {
			if attempt < maxAttempts-1 {
				continue
			}
		}
		return statusCode, respBody, nil
	}
	return statusCode, respBody, nil
}

// classifyHTTPError maps a non-2xx response to one of the typed
// sentinel errors in the root hippo package. The upstream message is
// wrapped so callers can inspect it with errors.Unwrap.
func classifyHTTPError(statusCode int, body []byte) error {
	var errResp errorResponse
	_ = json.Unmarshal(body, &errResp)
	msg := errResp.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	switch {
	case statusCode == http.StatusUnauthorized, statusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s", hippo.ErrAuthentication, msg)
	case statusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s", hippo.ErrRateLimit, msg)
	case statusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(msg), "model"):
		return fmt.Errorf("%w: %s", hippo.ErrModelNotFound, msg)
	case statusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s", hippo.ErrModelNotFound, msg)
	case statusCode >= 500:
		return fmt.Errorf("%w: %s", hippo.ErrProviderUnavailable, msg)
	default:
		return fmt.Errorf("anthropic: unexpected status %d: %s", statusCode, msg)
	}
}
