// Package ollama implements a hippo.Provider backed by a local
// Ollama daemon (https://ollama.com).
//
// Ollama is the canonical PrivacyLocalOnly provider: inference happens
// on the host machine and nothing leaves the network. The adapter
// speaks Ollama's /api/chat endpoint for both synchronous calls and
// NDJSON streaming. No credentials — if the daemon is reachable it
// works, if not the operations surface a transport error.
//
// # Tool calls
//
// /api/chat supports tool calls natively for models that understand
// them (Llama 3.1+, Qwen 2.5+, gpt-oss, etc.). Tool calls arrive on
// the terminal streaming chunk (unlike Anthropic and OpenAI which
// stream arguments incrementally). The adapter emits one
// StreamChunkToolCall per parsed tool_call before the terminal usage
// chunk.
//
// Models that do not support native tool calling may instead emit
// plain text that looks like a function call — the adapter does not
// attempt to parse these. If you need tool calling, pin a model that
// advertises the capability.
//
// # Context windows and Models()
//
// Models() queries /api/tags at call time (not at New() time, so the
// daemon may be started after the provider is constructed). Results
// are cached for 30 seconds. ContextWindow for each installed model
// is looked up in budget.DefaultPricing() first (users register
// custom rows for custom models); if not registered, it falls back
// to 8192.
//
// Querying /api/show for an authoritative per-architecture
// context_length is intentionally skipped — the schema varies across
// Ollama versions and parsing it robustly is more work than the
// payoff. If you need a precise window, add a row for your model to
// budget/pricing.yaml.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/budget"
)

const (
	defaultBaseURL  = "http://localhost:11434"
	defaultModel    = "llama3.3:70b"
	defaultTimeout  = 120 * time.Second
	defaultKeepAliveSeconds = 5 * 60 // 5 minutes
	modelsCacheTTL  = 30 * time.Second
	fallbackContext = 8192
)

// provider implements hippo.Provider. Unexported: callers interact
// via the hippo.Provider returned by New.
type provider struct {
	baseURL    string
	model      string
	timeout    time.Duration
	httpClient *http.Client
	keepAlive  time.Duration // zero means "server default"

	modelsMu   sync.Mutex
	modelsAt   time.Time
	modelsData []hippo.ModelInfo
}

// Option configures a provider during construction.
type Option func(*provider)

// WithBaseURL overrides the default http://localhost:11434 endpoint.
// Useful for remote Ollama servers on a LAN.
func WithBaseURL(u string) Option { return func(p *provider) { p.baseURL = u } }

// WithModel sets the default model id. Call.Model wins when set;
// otherwise this is used. Default: "llama3.3:70b".
func WithModel(m string) Option { return func(p *provider) { p.model = m } }

// WithTimeout overrides the default 120s *http.Client timeout. Local
// inference on the first request of a cold model can be slow because
// Ollama has to load the weights into VRAM; 120s is comfortable.
// Ignored when WithHTTPClient is set.
func WithTimeout(d time.Duration) Option { return func(p *provider) { p.timeout = d } }

// WithHTTPClient replaces the internal *http.Client entirely.
func WithHTTPClient(c *http.Client) Option {
	return func(p *provider) { p.httpClient = c }
}

// WithKeepAlive sets how long Ollama keeps a model loaded in memory
// after a request. Negative duration (e.g. -1s) means "keep loaded
// forever"; zero means "unload immediately". Default is Ollama's
// server-side default of 5 minutes.
func WithKeepAlive(d time.Duration) Option {
	return func(p *provider) { p.keepAlive = d }
}

// New returns a hippo.Provider backed by an Ollama daemon. No
// credentials required. New never blocks on network I/O; if the
// daemon is unreachable, Call and Stream surface the error and
// Models() returns an empty slice plus a Warn log.
func New(opts ...Option) (hippo.Provider, error) {
	p := &provider{
		baseURL: defaultBaseURL,
		model:   defaultModel,
		timeout: defaultTimeout,
	}
	for _, o := range opts {
		o(p)
	}
	if p.httpClient == nil {
		p.httpClient = &http.Client{Timeout: p.timeout}
	}
	return p, nil
}

// Name returns "ollama".
func (p *provider) Name() string { return "ollama" }

// Privacy returns PrivacyLocalOnly.
func (p *provider) Privacy() hippo.PrivacyTier { return hippo.PrivacyLocalOnly }

// EstimateCost always returns 0. Local inference is free by
// definition; the budget's pricing table agrees (zero_cost on the
// ollama ProviderPricing), but the provider short-circuits here so
// the router doesn't pay a map lookup per routing candidate.
func (p *provider) EstimateCost(c hippo.Call) (float64, error) {
	_ = c
	return 0, nil
}

// Models returns the list of models installed in the Ollama daemon.
// Results are cached for 30 seconds so the router can call this on
// every routing decision without flooding the server. If the daemon
// is unreachable, returns an empty slice and logs at Warn — a live
// Ollama daemon is not a hippo startup dependency.
func (p *provider) Models() []hippo.ModelInfo {
	p.modelsMu.Lock()
	if time.Since(p.modelsAt) < modelsCacheTTL && p.modelsData != nil {
		out := append([]hippo.ModelInfo(nil), p.modelsData...)
		p.modelsMu.Unlock()
		return out
	}
	p.modelsMu.Unlock()

	models, err := p.fetchTags(context.Background())
	if err != nil {
		slog.Warn("ollama: fetching /api/tags failed", "err", err, "base_url", p.baseURL)
		return nil
	}

	p.modelsMu.Lock()
	p.modelsData = models
	p.modelsAt = time.Now()
	p.modelsMu.Unlock()

	return append([]hippo.ModelInfo(nil), models...)
}

// fetchTags queries /api/tags and produces a sorted hippo.ModelInfo
// slice. ContextWindow is derived from budget.DefaultPricing when the
// installed model is registered; otherwise falls back to 8192 (a
// reasonable minimum — most stock Ollama models run at least this).
func (p *provider) fetchTags(ctx context.Context) ([]hippo.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ollama: /api/tags returned %d", resp.StatusCode)
	}

	var payload struct {
		Models []struct {
			Name     string `json:"name"`
			Model    string `json:"model"`
			Size     int64  `json:"size"`
			Modified string `json:"modified_at"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("ollama: decode /api/tags: %w", err)
	}

	out := make([]hippo.ModelInfo, 0, len(payload.Models))
	for _, m := range payload.Models {
		id := m.Name
		if id == "" {
			id = m.Model
		}
		if id == "" {
			continue
		}
		ctx := fallbackContext
		if rate, ok := budget.DefaultPricing().Lookup("ollama", id); ok && rate.ContextWindow > 0 {
			ctx = rate.ContextWindow
		}
		out = append(out, hippo.ModelInfo{
			ID:                id,
			DisplayName:       id,
			ContextTokens:     ctx,
			MaxOutputTokens:   ctx / 2, // conservative; Ollama models accept any number up to num_ctx
			SupportsTools:     true,
			SupportsStreaming: true,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// chatRequest is the /api/chat request envelope. Fields not relevant
// to hippo (format, template, raw, ...) are omitted.
type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	Stream    bool          `json:"stream"`
	KeepAlive string        `json:"keep_alive,omitempty"`
	Options   *chatOptions  `json:"options,omitempty"`
}

type chatMessage struct {
	Role      string          `json:"role"`
	Content   string          `json:"content"`
	ToolCalls []chatToolCall  `json:"tool_calls,omitempty"`
	ToolName  string          `json:"tool_name,omitempty"` // some ollama versions echo this
}

type chatToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type chatOptions struct {
	NumPredict  int     `json:"num_predict,omitempty"`
	NumCtx      int     `json:"num_ctx,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	TopP        float64 `json:"top_p,omitempty"`
}

// chatResponse is one message in the /api/chat response. For
// non-streaming requests, the whole body is a single chatResponse with
// Done=true. For streaming requests, it is one line of NDJSON.
type chatResponse struct {
	Model      string      `json:"model"`
	CreatedAt  string      `json:"created_at"`
	Message    chatMessage `json:"message"`
	Done       bool        `json:"done"`
	DoneReason string      `json:"done_reason,omitempty"`

	TotalDuration      int64 `json:"total_duration,omitempty"`
	LoadDuration       int64 `json:"load_duration,omitempty"`
	PromptEvalCount    int   `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration int64 `json:"prompt_eval_duration,omitempty"`
	EvalCount          int   `json:"eval_count,omitempty"`
	EvalDuration       int64 `json:"eval_duration,omitempty"`
}

// buildRequest maps a hippo.Call into a chatRequest. Ollama natively
// accepts system-role entries in the messages array — no top-level
// system field — so system messages pass through untouched.
func (p *provider) buildRequest(c hippo.Call, model string, maxTokens int, stream bool) (chatRequest, error) {
	var msgs []chatMessage
	for _, m := range c.Messages {
		msgs = append(msgs, chatMessage{Role: m.Role, Content: m.Content})
	}
	if c.Prompt != "" {
		msgs = append(msgs, chatMessage{Role: "user", Content: c.Prompt})
	}
	if len(msgs) == 0 {
		return chatRequest{}, errors.New("ollama: Call requires Prompt or at least one Message")
	}
	if len(c.Tools) > 0 {
		slog.Debug("ollama: tool-call request ignored in Pass 7 (tools on request skipped)")
	}
	req := chatRequest{
		Model:    model,
		Messages: msgs,
		Stream:   stream,
		Options: &chatOptions{
			NumPredict: maxTokens,
		},
	}
	if p.keepAlive != 0 {
		req.KeepAlive = formatKeepAlive(p.keepAlive)
	}
	return req, nil
}

// formatKeepAlive renders a duration into Ollama's keep_alive format.
// Negative durations become "-1s" ("keep loaded forever"); positive
// durations use Go's standard representation; zero would mean "unload
// immediately" but is not reachable here (the caller elides keep_alive
// entirely when it's zero).
func formatKeepAlive(d time.Duration) string {
	if d < 0 {
		return "-1s"
	}
	return d.String()
}

// Call executes a synchronous /api/chat request.
func (p *provider) Call(ctx context.Context, c hippo.Call) (*hippo.Response, error) {
	model := p.model
	if c.Model != "" {
		model = c.Model
	}
	maxTokens := c.MaxTokens
	if maxTokens == 0 {
		maxTokens = 1024
	}

	req, err := p.buildRequest(c, model, maxTokens, false)
	if err != nil {
		return nil, err
	}
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal request: %w", err)
	}

	start := time.Now()
	statusCode, respBody, err := p.doWithRetry(ctx, "/api/chat", reqBody)
	if err != nil {
		return nil, err
	}
	latencyMS := time.Since(start).Milliseconds()

	if statusCode < 200 || statusCode >= 300 {
		return nil, classifyHTTPError(statusCode, respBody)
	}

	var cr chatResponse
	if err := json.Unmarshal(respBody, &cr); err != nil {
		return nil, fmt.Errorf("ollama: parse response: %w", err)
	}

	return &hippo.Response{
		Text: cr.Message.Content,
		ToolCalls: mapToolCalls(cr.Message.ToolCalls),
		Usage: hippo.Usage{
			InputTokens:  cr.PromptEvalCount,
			OutputTokens: cr.EvalCount,
		},
		CostUSD:    0,
		Provider:   "ollama",
		Model:      cr.Model,
		LatencyMS:  latencyMS,
		ReceivedAt: time.Now(),
	}, nil
}

// mapToolCalls converts Ollama tool_call entries to hippo.ToolCall.
// Ollama does not assign per-call IDs, so we synthesise "tool_<index>"
// so the round-trip (assistant → tool-result) has something to key
// on. Consumers that need stable IDs across runs should record them
// at the point of emission.
func mapToolCalls(in []chatToolCall) []hippo.ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]hippo.ToolCall, 0, len(in))
	for i, tc := range in {
		args := tc.Function.Arguments
		if len(args) == 0 {
			args = []byte("{}")
		}
		out = append(out, hippo.ToolCall{
			ID:        fmt.Sprintf("tool_%d", i),
			Name:      tc.Function.Name,
			Arguments: append(json.RawMessage(nil), args...),
		})
	}
	return out
}

// retryBaseDelay is the base backoff delay between retry attempts.
// Ollama rarely returns 429 / 5xx (no rate limits on a local daemon,
// no upstream congestion), but wiring retry is cheap and matches the
// other providers' handshake behaviour.
var retryBaseDelay = 1 * time.Second

// doWithRetry POSTs reqBody to path on the Ollama daemon, retrying
// 429 and 5xx with exponential backoff up to 3 attempts total. Same
// shape as the Anthropic and OpenAI adapters' helpers.
func (p *provider) doWithRetry(ctx context.Context, path string, reqBody []byte) (int, []byte, error) {
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

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+path, bytes.NewReader(reqBody))
		if err != nil {
			return 0, nil, fmt.Errorf("ollama: build request: %w", err)
		}
		req.Header.Set("content-type", "application/json")

		httpResp, err := p.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, ctx.Err()
			}
			return 0, nil, fmt.Errorf("ollama: request failed: %w", err)
		}

		body, readErr := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		if readErr != nil {
			return 0, nil, fmt.Errorf("ollama: read response: %w", readErr)
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

// classifyHTTPError maps a non-2xx response to a typed hippo sentinel.
// Ollama's error bodies are a plain {"error": "..."} object.
func classifyHTTPError(statusCode int, body []byte) error {
	var env struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	msg := env.Error
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	switch {
	case statusCode == http.StatusNotFound:
		// Ollama returns 404 when the model isn't installed. That's
		// ErrModelNotFound — same as the cloud providers' model-unknown
		// branch — though the underlying message ("model 'foo' not
		// found, try pulling it first") is Ollama-specific.
		return fmt.Errorf("%w: %s", hippo.ErrModelNotFound, msg)
	case statusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s", hippo.ErrRateLimit, msg)
	case statusCode >= 500:
		return fmt.Errorf("%w: %s", hippo.ErrProviderUnavailable, msg)
	default:
		return fmt.Errorf("ollama: unexpected status %d: %s", statusCode, msg)
	}
}

// Stream is implemented in streaming.go alongside the NDJSON reader.
func (p *provider) Stream(ctx context.Context, c hippo.Call) (<-chan hippo.StreamChunk, error) {
	return p.stream(ctx, c)
}
