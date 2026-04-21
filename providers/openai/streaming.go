package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/budget"
	"github.com/mahdi-salmanzade/hippo/internal/sse"
)

// stream is the concrete Stream implementation for the Responses API.
// The public Stream method in openai.go is a one-line forwarder; the
// guts live here to keep openai.go focused on the non-streaming path.
func (p *provider) stream(ctx context.Context, c hippo.Call) (<-chan hippo.StreamChunk, error) {
	model := p.model
	if c.Model != "" {
		model = c.Model
	}
	maxTokens := c.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}

	if len(c.Tools) > 0 {
		slog.Debug("openai: tool calls not supported in Pass 6 stream path; ignoring Call.Tools")
	}

	req, err := p.buildRequestBody(c, model, maxTokens)
	if err != nil {
		return nil, err
	}
	req.Stream = true
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal stream request: %w", err)
	}

	httpResp, err := p.openStream(ctx, reqBody)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, _ := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		return nil, classifyHTTPError(httpResp.StatusCode, body)
	}

	out := make(chan hippo.StreamChunk, 16)
	go p.readStream(ctx, httpResp, model, out)
	return out, nil
}

// openStream POSTs to /v1/responses with stream:true, retrying the
// same 429 / 5xx conditions as the non-streaming path. On 2xx it
// returns the *http.Response with its Body still open - the caller
// owns closing it.
func (p *provider) openStream(ctx context.Context, reqBody []byte) (*http.Response, error) {
	const maxAttempts = 3
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * retryBaseDelay
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			p.baseURL+"/v1/responses", bytes.NewReader(reqBody))
		if err != nil {
			return nil, fmt.Errorf("openai: build stream request: %w", err)
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("accept", "text/event-stream")
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
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("openai: stream request failed: %w", err)
		}

		if httpResp.StatusCode == http.StatusTooManyRequests || httpResp.StatusCode >= 500 {
			if attempt < maxAttempts-1 {
				io.Copy(io.Discard, httpResp.Body)
				httpResp.Body.Close()
				continue
			}
		}
		return httpResp, nil
	}
	// Unreachable: every path inside the loop either continues or
	// returns. Satisfies the compiler.
	return nil, nil
}

// openaiToolAccumulator buffers the streamed function-call arguments
// for one tool call. Keyed by output_index; id/name are captured when
// response.output_item.added announces the item, args accumulate via
// response.function_call_arguments.delta, and the finished ToolCall is
// emitted on response.output_item.done.
type openaiToolAccumulator struct {
	id      string
	name    string
	argsBuf bytes.Buffer
}

// readStream drains the Responses SSE body, translates events into
// hippo.StreamChunk, and closes out. Always closes the http body. One
// of the StreamChunkUsage or StreamChunkError terminal chunks is
// emitted before close on a normal end; ctx cancellation closes without
// a terminal error chunk.
func (p *provider) readStream(
	ctx context.Context,
	httpResp *http.Response,
	requestedModel string,
	out chan<- hippo.StreamChunk,
) {
	defer close(out)
	defer httpResp.Body.Close()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			httpResp.Body.Close()
		case <-done:
		}
	}()

	scanner := sse.NewScanner(httpResp.Body)
	accumulators := map[int]*openaiToolAccumulator{}
	// echoedModel is the model id as returned in response.created /
	// response.completed; fall back to the requested model if the
	// server never echoes one.
	echoedModel := requestedModel

	emit := func(chunk hippo.StreamChunk) bool {
		select {
		case out <- chunk:
			return true
		case <-ctx.Done():
			return false
		}
	}

	for {
		ev, err := scanner.Next(ctx)
		if err != nil {
			if err == io.EOF {
				// Clean EOF without response.completed - unusual but
				// not fatal; surface as error so the caller doesn't
				// mistake it for a successful usage chunk.
				emit(hippo.StreamChunk{Type: hippo.StreamChunkError,
					Error: fmt.Errorf("openai: stream ended without response.completed")})
				return
			}
			if ctx.Err() != nil {
				return
			}
			emit(hippo.StreamChunk{Type: hippo.StreamChunkError,
				Error: fmt.Errorf("openai: stream read: %w", err)})
			return
		}

		terminal, err := handleOpenAIEvent(ev, accumulators, &echoedModel, emit)
		if err != nil {
			emit(hippo.StreamChunk{Type: hippo.StreamChunkError, Error: err})
			return
		}
		if terminal {
			return
		}
	}
}

// handleOpenAIEvent processes one SSE event from the Responses API.
// Returns terminal=true when the event closes the stream (success or
// failure); in the success case (response.completed or
// response.incomplete) the terminal StreamChunkUsage has already been
// emitted by this function.
func handleOpenAIEvent(
	ev *sse.Event,
	accumulators map[int]*openaiToolAccumulator,
	echoedModel *string,
	emit func(hippo.StreamChunk) bool,
) (terminal bool, err error) {
	switch ev.Event {
	case "response.created", "response.in_progress":
		// response.created carries the echoed model id; capture it so
		// the terminal chunk can report the dated variant (e.g.
		// "gpt-5-2026-02-15") rather than the alias the caller sent.
		var payload struct {
			Response struct {
				Model string `json:"model"`
			} `json:"response"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err == nil && payload.Response.Model != "" {
			*echoedModel = payload.Response.Model
		}
		return false, nil

	case "response.output_item.added":
		var payload struct {
			OutputIndex int `json:"output_index"`
			Item        struct {
				Type   string `json:"type"`
				ID     string `json:"id"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			} `json:"item"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return false, fmt.Errorf("openai: parse response.output_item.added: %w", err)
		}
		if payload.Item.Type == "function_call" {
			id := payload.Item.CallID
			if id == "" {
				id = payload.Item.ID
			}
			accumulators[payload.OutputIndex] = &openaiToolAccumulator{
				id:   id,
				name: payload.Item.Name,
			}
		}
		return false, nil

	case "response.content_part.added":
		// Content parts inside message items - purely metadata here,
		// no delta text yet. Deltas arrive via response.output_text.delta.
		return false, nil

	case "response.output_text.delta":
		var payload struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return false, fmt.Errorf("openai: parse output_text.delta: %w", err)
		}
		if payload.Delta != "" {
			if !emit(hippo.StreamChunk{Type: hippo.StreamChunkText, Delta: payload.Delta}) {
				return true, nil
			}
		}
		return false, nil

	case "response.reasoning_summary_text.delta":
		var payload struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return false, fmt.Errorf("openai: parse reasoning_summary_text.delta: %w", err)
		}
		if payload.Delta != "" {
			if !emit(hippo.StreamChunk{Type: hippo.StreamChunkThinking, Delta: payload.Delta}) {
				return true, nil
			}
		}
		return false, nil

	case "response.reasoning.delta":
		// Encrypted reasoning deltas - not human-readable; dropped
		// intentionally (see package doc in openai.go).
		return false, nil

	case "response.function_call_arguments.delta":
		var payload struct {
			OutputIndex int    `json:"output_index"`
			Delta       string `json:"delta"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return false, fmt.Errorf("openai: parse function_call_arguments.delta: %w", err)
		}
		acc, ok := accumulators[payload.OutputIndex]
		if !ok {
			return false, fmt.Errorf("openai: function_call_arguments.delta for unknown output_index %d",
				payload.OutputIndex)
		}
		acc.argsBuf.WriteString(payload.Delta)
		return false, nil

	case "response.output_item.done":
		var payload struct {
			OutputIndex int `json:"output_index"`
			Item        struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return false, fmt.Errorf("openai: parse output_item.done: %w", err)
		}
		if payload.Item.Type != "function_call" {
			return false, nil
		}
		acc, ok := accumulators[payload.OutputIndex]
		if !ok {
			return false, nil
		}
		delete(accumulators, payload.OutputIndex)
		args := acc.argsBuf.Bytes()
		if len(args) == 0 {
			args = []byte("{}")
		}
		if !emit(hippo.StreamChunk{
			Type: hippo.StreamChunkToolCall,
			ToolCall: &hippo.ToolCall{
				ID:        acc.id,
				Name:      acc.name,
				Arguments: append([]byte(nil), args...),
			},
		}) {
			return true, nil
		}
		return false, nil

	case "response.completed", "response.incomplete":
		// Both are "stream ended normally" from hippo's perspective.
		// response.incomplete typically means max_output_tokens was
		// reached - not an error, just a natural stop. The response
		// payload carries the final usage.
		var payload struct {
			Response struct {
				Model string `json:"model"`
				Usage struct {
					InputTokens        int `json:"input_tokens"`
					OutputTokens       int `json:"output_tokens"`
					InputTokensDetails struct {
						CachedTokens int `json:"cached_tokens"`
					} `json:"input_tokens_details"`
				} `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return true, fmt.Errorf("openai: parse %s: %w", ev.Event, err)
		}
		if payload.Response.Model != "" {
			*echoedModel = payload.Response.Model
		}
		emit(buildOpenAIUsageChunk(*echoedModel,
			payload.Response.Usage.InputTokens,
			payload.Response.Usage.OutputTokens,
			payload.Response.Usage.InputTokensDetails.CachedTokens))
		return true, nil

	case "response.failed":
		var payload struct {
			Response struct {
				Error struct {
					Message string `json:"message"`
					Code    string `json:"code"`
				} `json:"error"`
			} `json:"response"`
		}
		_ = json.Unmarshal(ev.Data, &payload)
		msg := payload.Response.Error.Message
		if msg == "" {
			msg = "stream failed"
		}
		return true, fmt.Errorf("openai: %s", msg)

	case "error":
		// Top-level error event, distinct from response.failed. Rare
		// (usually precedes response.failed) but documented.
		var payload struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(ev.Data, &payload)
		msg := payload.Error.Message
		if msg == "" {
			msg = string(ev.Data)
		}
		return true, fmt.Errorf("openai: stream error: %s", msg)
	}

	// Unknown event types are dropped silently so new Responses-API
	// event additions don't break existing clients.
	return false, nil
}

// buildOpenAIUsageChunk turns terminal usage numbers into the final
// StreamChunkUsage. Cost is computed via budget.DefaultPricing so the
// CachedInputPerMtok rate applies to cached tokens correctly.
func buildOpenAIUsageChunk(model string, inputTokens, outputTokens, cachedTokens int) hippo.StreamChunk {
	rate, ok := budget.DefaultPricing().Lookup("openai", model)
	var cost float64
	if ok {
		const perMillion = 1_000_000.0
		plain := inputTokens - cachedTokens
		if plain < 0 {
			plain = 0
		}
		cost = float64(plain)*rate.InputPerMtok/perMillion +
			float64(cachedTokens)*rate.CachedInputPerMtok/perMillion +
			float64(outputTokens)*rate.OutputPerMtok/perMillion
	}
	return hippo.StreamChunk{
		Type: hippo.StreamChunkUsage,
		Usage: &hippo.Usage{
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			CachedTokens: cachedTokens,
		},
		CostUSD:  cost,
		Provider: "openai",
		Model:    model,
	}
}
