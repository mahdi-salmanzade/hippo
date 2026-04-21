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
// which is enough for a router's budget-feasibility check - the actual
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
	return float64(inputTokens)*pr.InputPerMtok/perMillion +
		float64(output)*pr.OutputPerMtok/perMillion, nil
}

// Wire-format types for the Messages API. These are package-private so
// we can evolve them without leaking shape into the hippo public API.

// wireMessage has flexible Content because Anthropic's Messages API
// accepts either a plain string (the common single-text case) or a
// typed content-block array (tool_use on assistant turns, tool_result
// on user turns). Using json.RawMessage lets buildRequestBody choose
// the cheapest shape per message.
type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// wireTool mirrors Anthropic's tool schema on the request side.
// input_schema is passed through verbatim from hippo.Tool.Schema().
type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type messagesRequest struct {
	Model     string        `json:"model"`
	Messages  []wireMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
	System    string        `json:"system,omitempty"`
	Stream    bool          `json:"stream,omitempty"`
	Tools     []wireTool    `json:"tools,omitempty"`
}

// outBlock is a content block hippo emits on the request side. Only
// the fields relevant to Type are populated per block; omitempty
// keeps the wire payload clean.
type outBlock struct {
	Type string `json:"type"`
	// text blocks
	Text string `json:"text,omitempty"`
	// tool_use (assistant turn echoing a prior tool call back to
	// the API on a follow-up request)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result (user turn feeding a tool's output back)
	ToolUseID string `json:"tool_use_id,omitempty"`
	// tool_result's inner content, as a plain string. Anthropic
	// accepts either a string or an array here; we use string for
	// simplicity since hippo.ToolResult.Content is already a string.
	Content string `json:"content,omitempty"`
	IsError bool   `json:"is_error,omitempty"`
}

// inBlock is a content block parsed from a response payload.
// tool_use blocks carry id/name/input; text blocks carry text.
type inBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type wireUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type messagesResponse struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	Role       string    `json:"role"`
	Content    []inBlock `json:"content"`
	Model      string    `json:"model"`
	StopReason string    `json:"stop_reason"`
	Usage      wireUsage `json:"usage"`
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
	var toolCalls []hippo.ToolCall
	for _, block := range mr.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			args := block.Input
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			toolCalls = append(toolCalls, hippo.ToolCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: append(json.RawMessage(nil), args...),
			})
		}
	}

	cached := mr.Usage.CacheCreationInputTokens + mr.Usage.CacheReadInputTokens
	cost, _ := computeCost(mr.Model, mr.Usage.InputTokens, mr.Usage.OutputTokens,
		mr.Usage.CacheCreationInputTokens, mr.Usage.CacheReadInputTokens)

	return &hippo.Response{
		Text:      text.String(),
		ToolCalls: toolCalls,
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

// Stream opens a streaming Messages request and returns a channel of
// incremental hippo.StreamChunk values. Implementation is in
// streaming.go alongside the tool-call reassembler.
func (p *provider) Stream(ctx context.Context, c hippo.Call) (<-chan hippo.StreamChunk, error) {
	return p.stream(ctx, c)
}

// buildRequestBody maps a hippo.Call into the Anthropic Messages API
// request shape.
//
// Message translation:
//
//   - system-role messages fold into the top-level "system" field.
//   - assistant messages with tool calls serialise as a content-block
//     array mixing text and tool_use blocks. Plain assistant messages
//     stay as a bare string.
//   - role:"tool" messages become user turns carrying tool_result
//     content blocks (with tool_use_id matching the earlier call).
//     Anthropic has no "tool" role on the wire - tool outputs are
//     always user turns shaped as tool_result blocks.
//   - other messages (plain user, plain assistant) serialise as a
//     bare string for the cheapest possible payload.
//
// Tools on the Call translate to the request's top-level tools[]
// array with input_schema copied verbatim from Tool.Schema().
//
// Call.Prompt, when set, is appended as a trailing user message.
func (p *provider) buildRequestBody(c hippo.Call, model string, maxTokens int) (messagesRequest, error) {
	var systemParts []string
	var msgs []wireMessage

	// Consecutive role:"tool" messages fold into a single user turn
	// with all the tool_result blocks together, which is how
	// Anthropic expects parallel tool-call results to arrive. The
	// loop flushes on every role change.
	var pendingResults []outBlock
	flushResults := func() error {
		if len(pendingResults) == 0 {
			return nil
		}
		content, err := json.Marshal(pendingResults)
		if err != nil {
			return fmt.Errorf("anthropic: marshal tool_result blocks: %w", err)
		}
		msgs = append(msgs, wireMessage{Role: "user", Content: content})
		pendingResults = nil
		return nil
	}

	for _, m := range c.Messages {
		switch m.Role {
		case "system":
			if err := flushResults(); err != nil {
				return messagesRequest{}, err
			}
			systemParts = append(systemParts, m.Content)

		case "tool":
			pendingResults = append(pendingResults, outBlock{
				Type:      "tool_result",
				ToolUseID: m.ToolCallID,
				Content:   m.Content,
			})

		case "assistant":
			if err := flushResults(); err != nil {
				return messagesRequest{}, err
			}
			wm, err := buildAssistantMessage(m)
			if err != nil {
				return messagesRequest{}, err
			}
			msgs = append(msgs, wm)

		default: // "user" and anything else
			if err := flushResults(); err != nil {
				return messagesRequest{}, err
			}
			body, err := json.Marshal(m.Content)
			if err != nil {
				return messagesRequest{}, err
			}
			msgs = append(msgs, wireMessage{Role: m.Role, Content: body})
		}
	}
	if err := flushResults(); err != nil {
		return messagesRequest{}, err
	}

	if c.Prompt != "" {
		body, err := json.Marshal(c.Prompt)
		if err != nil {
			return messagesRequest{}, err
		}
		msgs = append(msgs, wireMessage{Role: "user", Content: body})
	}

	if len(msgs) == 0 {
		return messagesRequest{}, errors.New("anthropic: Call requires Prompt or at least one user/assistant Message")
	}

	tools, err := translateTools(c.Tools)
	if err != nil {
		return messagesRequest{}, err
	}

	return messagesRequest{
		Model:     model,
		Messages:  msgs,
		MaxTokens: maxTokens,
		System:    strings.Join(systemParts, "\n\n"),
		Tools:     tools,
	}, nil
}

// buildAssistantMessage serialises an assistant message. If it has
// tool calls, the Content is a content-block array mixing text (when
// non-empty) and tool_use blocks. Plain assistant messages serialise
// as a bare string for symmetry with the common case.
func buildAssistantMessage(m hippo.Message) (wireMessage, error) {
	if len(m.ToolCalls) == 0 {
		body, err := json.Marshal(m.Content)
		if err != nil {
			return wireMessage{}, err
		}
		return wireMessage{Role: "assistant", Content: body}, nil
	}
	blocks := make([]outBlock, 0, len(m.ToolCalls)+1)
	if m.Content != "" {
		blocks = append(blocks, outBlock{Type: "text", Text: m.Content})
	}
	for _, tc := range m.ToolCalls {
		args := tc.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		blocks = append(blocks, outBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Name,
			Input: args,
		})
	}
	content, err := json.Marshal(blocks)
	if err != nil {
		return wireMessage{}, err
	}
	return wireMessage{Role: "assistant", Content: content}, nil
}

// translateTools maps hippo.Tool instances into Anthropic's
// request-level tools array. A nil schema is replaced with an
// empty object-type schema so the request isn't rejected; a real
// schema passes through verbatim.
func translateTools(tools []hippo.Tool) ([]wireTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]wireTool, 0, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		schema := t.Schema()
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out = append(out, wireTool{
			Name:        t.Name(),
			Description: t.Description(),
			InputSchema: schema,
		})
	}
	return out, nil
}

// retryBaseDelay is the base backoff delay between retry attempts.
// It is a var (not const) so tests can compress it to microseconds;
// production code must not mutate it. The retry schedule uses this as
// the first sleep and doubles on each subsequent attempt.
var retryBaseDelay = 1 * time.Second

// doWithRetry POSTs reqBody to /v1/messages, retrying on 429 and 5xx
// with exponential backoff (1s, 2s) up to 3 attempts total. Network
// errors are not retried - they return immediately, wrapping ctx errors
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
