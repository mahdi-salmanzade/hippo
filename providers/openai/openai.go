// Package openai implements a hippo.Provider backed by OpenAI's
// Responses API (POST /v1/responses).
//
// Responses is OpenAI's 2025 successor to Chat Completions. Key
// differences that drove the adapter shape here:
//
//   - System prompts go in the top-level "instructions" field, not
//     interleaved as a system-role entry in "input". This mirrors
//     Anthropic's "system" field, so the hippo Provider system-role
//     contract (see interfaces.go) maps cleanly.
//   - Responses are structured as an array of output items, each of
//     which is a message, a reasoning trace, or a tool call. Message
//     text lands in Response.Text; reasoning summaries land in
//     Response.Thinking.
//   - Usage reports "input_tokens" / "output_tokens" with nested
//     "cached_tokens" and "reasoning_tokens" detail blocks.
//
// Pass 5 scope: synchronous, no streaming, no tool calling. Stream
// returns ErrNotImplemented (Pass 6); Call.Tools are ignored with a
// debug log (Pass 8). Stateful conversations via previous_response_id
// are intentionally out of scope — hippo owns state at the memory
// layer, the provider is stateless.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/budget"
)

// defaultModel is picked when WithModel and Call.Model are both empty.
// gpt-5 is OpenAI's general-purpose frontier model, analogous to our
// Anthropic default of Opus.
const defaultModel = "gpt-5"

// defaultTimeout is the default *http.Client timeout. Overridable via
// WithTimeout; fully replaceable via WithHTTPClient.
const defaultTimeout = 60 * time.Second

// defaultBaseURL is OpenAI's production endpoint. WithBaseURL
// overrides it (for test doubles and OpenAI-compatible proxies).
const defaultBaseURL = "https://api.openai.com"

// provider implements hippo.Provider. Unexported: callers only see the
// hippo.Provider returned by New.
type provider struct {
	apiKey       string
	model        string
	baseURL      string
	timeout      time.Duration
	httpClient   *http.Client
	organization string
	project      string
}

// Option configures a provider during construction.
type Option func(*provider)

// WithAPIKey supplies the OpenAI API key. Required; New returns an
// error if this option is omitted.
func WithAPIKey(key string) Option { return func(p *provider) { p.apiKey = key } }

// WithModel sets the default model id. Call.Model wins when set;
// otherwise this is used. Default: gpt-5.
func WithModel(model string) Option { return func(p *provider) { p.model = model } }

// WithBaseURL overrides the default https://api.openai.com endpoint.
// Useful for httptest servers and OpenAI-compatible endpoints (though
// the Responses API is OpenAI-specific — most compat shims only speak
// Chat Completions).
func WithBaseURL(u string) Option { return func(p *provider) { p.baseURL = u } }

// WithTimeout overrides the default 60s *http.Client timeout. Ignored
// if WithHTTPClient is also supplied.
func WithTimeout(d time.Duration) Option { return func(p *provider) { p.timeout = d } }

// WithHTTPClient replaces the internal *http.Client entirely. Use it
// for custom transports, test doubles, or proxies with client certs.
// WithTimeout is ignored when this is set.
func WithHTTPClient(c *http.Client) Option {
	return func(p *provider) { p.httpClient = c }
}

// WithOrganization sets the OpenAI-Organization header, used by
// multi-org OpenAI customers to attribute usage.
func WithOrganization(org string) Option {
	return func(p *provider) { p.organization = org }
}

// WithProject sets the OpenAI-Project header, used when a single key
// spans multiple projects within one org.
func WithProject(proj string) Option {
	return func(p *provider) { p.project = proj }
}

// New returns a hippo.Provider backed by OpenAI's Responses API.
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
		return nil, errors.New("openai: API key is required (use WithAPIKey)")
	}
	if p.httpClient == nil {
		p.httpClient = &http.Client{Timeout: p.timeout}
	}
	return p, nil
}

// Name returns "openai".
func (p *provider) Name() string { return "openai" }

// Models returns a catalogue derived from budget.DefaultPricing() so
// the pricing table stays the single source of truth for which OpenAI
// models hippo knows about. MaxOutputTokens is a conservative default
// per model; callers that need precise caps should set Call.MaxTokens
// explicitly and let OpenAI's per-model limit surface as a 400.
func (p *provider) Models() []hippo.ModelInfo {
	ids := budget.DefaultPricing().Models("openai")
	sort.Strings(ids) // stable order for Models()
	out := make([]hippo.ModelInfo, 0, len(ids))
	for _, id := range ids {
		mp, _ := budget.DefaultPricing().Lookup("openai", id)
		out = append(out, hippo.ModelInfo{
			ID:                id,
			DisplayName:       displayNameFor(id),
			ContextTokens:     mp.ContextWindow,
			MaxOutputTokens:   maxOutputTokensFor(id),
			SupportsTools:     true,
			SupportsStreaming: true,
		})
	}
	return out
}

// displayNameFor produces a human-readable label for an OpenAI model
// id. Unknown ids fall back to the id itself.
func displayNameFor(id string) string {
	switch id {
	case "gpt-5":
		return "GPT-5"
	case "gpt-5-mini":
		return "GPT-5 Mini"
	case "gpt-5-nano":
		return "GPT-5 Nano"
	case "o4-mini":
		return "o4-mini"
	default:
		return id
	}
}

// maxOutputTokensFor is a conservative per-model output cap. OpenAI
// does not expose this via any API, so we hardcode values that are
// known to be safe; callers can override via Call.MaxTokens.
func maxOutputTokensFor(id string) int {
	switch id {
	case "gpt-5", "gpt-5-mini", "gpt-5-nano":
		return 16_000
	case "o4-mini":
		return 32_000
	default:
		return 4_096
	}
}

// Privacy returns PrivacyCloudOK. OpenAI is a hosted API.
func (p *provider) Privacy() hippo.PrivacyTier { return hippo.PrivacyCloudOK }

// EstimateCost returns a pre-flight USD estimate for c. Uses the same
// len/4 heuristic as the Anthropic adapter, plus a 30% input buffer
// for reasoning models to leave headroom for the reasoning trace —
// OpenAI bills reasoning tokens as output tokens, so under-estimating
// them causes the router to pick too-cheap models.
func (p *provider) EstimateCost(c hippo.Call) (float64, error) {
	model := p.model
	if c.Model != "" {
		model = c.Model
	}
	rate, ok := budget.DefaultPricing().Lookup("openai", model)
	if !ok {
		return 0, fmt.Errorf("openai: unknown model %q", model)
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
	if rate.SupportsReasoning {
		output = (output * 130) / 100
	}
	const perMillion = 1_000_000.0
	return float64(inputTokens)*rate.InputPerMtok/perMillion +
		float64(output)*rate.OutputPerMtok/perMillion, nil
}

// Wire-format types for the Responses API. Package-private so we can
// evolve them without leaking shape into the hippo public API.

type inputMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// inputFunctionCall echoes a prior tool call back to the Responses
// API on a follow-up request. "arguments" must be a JSON-encoded
// string (not an object) per the API shape.
type inputFunctionCall struct {
	Type      string `json:"type"`  // always "function_call"
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// inputFunctionCallOutput feeds a tool's output back on a
// follow-up request. "output" is the plain string the tool returned.
type inputFunctionCallOutput struct {
	Type   string `json:"type"` // always "function_call_output"
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// wireTool is one entry in the request's top-level tools array.
// strict:true enforces schema-compliant argument generation.
type wireTool struct {
	Type        string          `json:"type"` // always "function"
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict,omitempty"`
}

type responseRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	Tools           []wireTool      `json:"tools,omitempty"`
}

type responseContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// responseOutputItem covers all four output-item types we care about:
// "message" (role + content blocks), "reasoning" (summary blocks),
// and "function_call" (call_id + name + arguments string).
type responseOutputItem struct {
	Type    string                 `json:"type"`
	Role    string                 `json:"role,omitempty"`
	Content []responseContentBlock `json:"content,omitempty"`
	Summary []responseContentBlock `json:"summary,omitempty"`
	// function_call fields:
	ID        string `json:"id,omitempty"`      // the item id, not call_id
	CallID    string `json:"call_id,omitempty"` // what hippo.ToolCall.ID gets
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"` // JSON-encoded string
}

type responseInputTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type responseOutputTokensDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

type responseUsage struct {
	InputTokens         int                         `json:"input_tokens"`
	OutputTokens        int                         `json:"output_tokens"`
	InputTokensDetails  responseInputTokensDetails  `json:"input_tokens_details"`
	OutputTokensDetails responseOutputTokensDetails `json:"output_tokens_details"`
}

type responseError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type responseResponse struct {
	ID     string               `json:"id"`
	Object string               `json:"object"`
	Model  string               `json:"model"`
	Status string               `json:"status"`
	Output []responseOutputItem `json:"output"`
	Usage  responseUsage        `json:"usage"`
	Error  *responseError       `json:"error,omitempty"`
}

type errorEnvelope struct {
	Error responseError `json:"error"`
}

// Call implements hippo.Provider.
func (p *provider) Call(ctx context.Context, c hippo.Call) (*hippo.Response, error) {
	model := p.model
	if c.Model != "" {
		model = c.Model
	}
	maxTokens := c.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	req, err := p.buildRequestBody(c, model, maxTokens)
	if err != nil {
		return nil, err
	}
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
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

	var rr responseResponse
	if err := json.Unmarshal(respBody, &rr); err != nil {
		return nil, fmt.Errorf("openai: parse response: %w", err)
	}

	var text, thinking strings.Builder
	var toolCalls []hippo.ToolCall
	for _, item := range rr.Output {
		switch item.Type {
		case "message":
			for _, block := range item.Content {
				if block.Type == "output_text" {
					text.WriteString(block.Text)
				}
			}
		case "reasoning":
			for _, block := range item.Summary {
				if block.Type == "summary_text" {
					thinking.WriteString(block.Text)
				}
			}
		case "function_call", "tool_call":
			// The Responses API emits arguments as a JSON-encoded
			// string; pass it through as RawMessage. An empty
			// arguments field (tool takes no input) becomes "{}".
			args := item.Arguments
			if args == "" {
				args = "{}"
			}
			id := item.CallID
			if id == "" {
				id = item.ID
			}
			toolCalls = append(toolCalls, hippo.ToolCall{
				ID:        id,
				Name:      item.Name,
				Arguments: json.RawMessage(args),
			})
		}
	}

	rate, ratesOK := budget.DefaultPricing().Lookup("openai", rr.Model)
	var cost float64
	if ratesOK {
		const perMillion = 1_000_000.0
		cached := rr.Usage.InputTokensDetails.CachedTokens
		plain := rr.Usage.InputTokens - cached
		if plain < 0 {
			plain = 0
		}
		cost = float64(plain)*rate.InputPerMtok/perMillion +
			float64(cached)*rate.CachedInputPerMtok/perMillion +
			float64(rr.Usage.OutputTokens)*rate.OutputPerMtok/perMillion
	}

	return &hippo.Response{
		Text:      text.String(),
		Thinking:  thinking.String(),
		ToolCalls: toolCalls,
		Usage: hippo.Usage{
			InputTokens:  rr.Usage.InputTokens,
			OutputTokens: rr.Usage.OutputTokens,
			CachedTokens: rr.Usage.InputTokensDetails.CachedTokens,
		},
		CostUSD:    cost,
		Provider:   "openai",
		Model:      rr.Model,
		LatencyMS:  latencyMS,
		ReceivedAt: time.Now(),
	}, nil
}

// Stream opens a streaming Responses request and returns a channel of
// incremental hippo.StreamChunk values. Implementation is in
// streaming.go.
func (p *provider) Stream(ctx context.Context, c hippo.Call) (<-chan hippo.StreamChunk, error) {
	return p.stream(ctx, c)
}

// buildRequestBody maps a hippo.Call into the Responses request shape.
//
// Message translation:
//
//   - system-role messages fold into the top-level "instructions"
//     field (the Responses API's native system channel).
//   - plain user/assistant messages serialise as {role, content}
//     entries in the input array.
//   - assistant messages with ToolCalls serialise as an optional
//     assistant message (when Content is non-empty) followed by one
//     function_call input-item per tool call. arguments is
//     JSON-encoded-as-string per API shape.
//   - role:"tool" messages become function_call_output items
//     correlated by call_id.
//
// Tool translation: each hippo.Tool maps to one {type:"function",
// name, description, parameters, strict:true} entry in the top-level
// tools array. strict:true enforces the schema on argument generation.
//
// Prompt-only Calls (no Messages) send Input as a bare JSON string —
// the cheapest shape the API accepts. Adding tools or any Messages
// entry switches to the array form.
func (p *provider) buildRequestBody(c hippo.Call, model string, maxTokens int) (responseRequest, error) {
	var systemParts []string
	var items []json.RawMessage

	appendJSON := func(v any) error {
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("openai: marshal input item: %w", err)
		}
		items = append(items, raw)
		return nil
	}

	for _, m := range c.Messages {
		switch {
		case m.Role == "system":
			systemParts = append(systemParts, m.Content)

		case m.Role == "tool":
			if err := appendJSON(inputFunctionCallOutput{
				Type:   "function_call_output",
				CallID: m.ToolCallID,
				Output: m.Content,
			}); err != nil {
				return responseRequest{}, err
			}

		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			if m.Content != "" {
				if err := appendJSON(inputMessage{Role: "assistant", Content: m.Content}); err != nil {
					return responseRequest{}, err
				}
			}
			for _, tc := range m.ToolCalls {
				args := tc.Arguments
				if len(args) == 0 {
					args = json.RawMessage(`{}`)
				}
				if err := appendJSON(inputFunctionCall{
					Type:      "function_call",
					CallID:    tc.ID,
					Name:      tc.Name,
					Arguments: string(args),
				}); err != nil {
					return responseRequest{}, err
				}
			}

		default:
			if err := appendJSON(inputMessage{Role: m.Role, Content: m.Content}); err != nil {
				return responseRequest{}, err
			}
		}
	}

	tools, err := translateTools(c.Tools)
	if err != nil {
		return responseRequest{}, err
	}

	// Choose input shape:
	//   - No items + no tools + prompt present: bare JSON string
	//   - Otherwise: array form with the trailing Prompt (if any)
	//     appended as a user message.
	var input json.RawMessage
	if len(items) == 0 && len(tools) == 0 {
		if c.Prompt == "" {
			return responseRequest{},
				errors.New("openai: Call requires Prompt or at least one user/assistant Message")
		}
		promptJSON, err := json.Marshal(c.Prompt)
		if err != nil {
			return responseRequest{}, fmt.Errorf("openai: marshal prompt: %w", err)
		}
		input = promptJSON
	} else {
		if c.Prompt != "" {
			if err := appendJSON(inputMessage{Role: "user", Content: c.Prompt}); err != nil {
				return responseRequest{}, err
			}
		}
		arr, err := json.Marshal(items)
		if err != nil {
			return responseRequest{}, fmt.Errorf("openai: marshal input array: %w", err)
		}
		input = arr
	}

	return responseRequest{
		Model:           model,
		Input:           input,
		Instructions:    strings.Join(systemParts, "\n\n"),
		MaxOutputTokens: maxTokens,
		Tools:           tools,
	}, nil
}

// translateTools maps hippo.Tool instances into Responses-API tool
// entries. strict:true is the default (per spec); a nil schema is
// replaced with an empty object schema so the request isn't rejected.
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
			Type:        "function",
			Name:        t.Name(),
			Description: t.Description(),
			Parameters:  schema,
			Strict:      true,
		})
	}
	return out, nil
}

// retryBaseDelay is the base backoff delay between retry attempts.
// Var, not const, so tests can compress it to microseconds; production
// code must not mutate it.
var retryBaseDelay = 1 * time.Second

// doWithRetry POSTs reqBody to /v1/responses, retrying on 429 and 5xx
// with exponential backoff (1s, 2s) up to 3 attempts total. Network
// errors are not retried; context cancellation is surfaced directly.
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
			p.baseURL+"/v1/responses", bytes.NewReader(reqBody))
		if err != nil {
			return 0, nil, fmt.Errorf("openai: build request: %w", err)
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("authorization", "Bearer "+p.apiKey)
		if p.organization != "" {
			req.Header.Set("openai-organization", p.organization)
		}
		if p.project != "" {
			req.Header.Set("openai-project", p.project)
		}

		httpResp, err := p.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return 0, nil, ctx.Err()
			}
			return 0, nil, fmt.Errorf("openai: request failed: %w", err)
		}

		body, readErr := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		if readErr != nil {
			return 0, nil, fmt.Errorf("openai: read response: %w", readErr)
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
	var env errorEnvelope
	_ = json.Unmarshal(body, &env)
	msg := env.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	codeLower := strings.ToLower(env.Error.Code)
	typeLower := strings.ToLower(env.Error.Type)
	msgLower := strings.ToLower(msg)

	// Content-policy / safety refusals can come back as 400 with
	// type="invalid_request_error" + code="content_policy_violation",
	// or 400 with a message containing "policy" / "moderation". The
	// policy check runs before the model-not-found branch so that a
	// policy rejection against a known-valid model doesn't get
	// miscategorised.
	if codeLower == "content_policy_violation" ||
		strings.Contains(typeLower, "content_policy") ||
		strings.Contains(msgLower, "content policy") ||
		strings.Contains(msgLower, "safety") ||
		strings.Contains(msgLower, "moderation") {
		return fmt.Errorf("%w: %s", hippo.ErrContentPolicy, msg)
	}

	switch {
	case statusCode == http.StatusUnauthorized, statusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s", hippo.ErrAuthentication, msg)
	case statusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%w: %s", hippo.ErrRateLimit, msg)
	case statusCode == http.StatusBadRequest &&
		(codeLower == "model_not_found" || strings.Contains(msgLower, "model")):
		return fmt.Errorf("%w: %s", hippo.ErrModelNotFound, msg)
	case statusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s", hippo.ErrModelNotFound, msg)
	case statusCode >= 500:
		return fmt.Errorf("%w: %s", hippo.ErrProviderUnavailable, msg)
	default:
		return fmt.Errorf("openai: unexpected status %d: %s", statusCode, msg)
	}
}
