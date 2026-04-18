package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/internal/sse"
)

// stream is the concrete Stream implementation. The public Stream
// method in anthropic.go is a one-line forwarder; the guts live here
// to keep anthropic.go focused on the non-streaming path.
func (p *provider) stream(ctx context.Context, c hippo.Call) (<-chan hippo.StreamChunk, error) {
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
	req.Stream = true
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal stream request: %w", err)
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
	go p.readStream(ctx, httpResp, out)
	return out, nil
}

// openStream POSTs to /v1/messages with stream:true, retrying 429 /
// 5xx responses with the same exponential backoff as doWithRetry. On
// success the returned *http.Response has its Body still open — the
// caller owns closing it.
//
// This is a sibling of doWithRetry rather than a shared helper because
// the two paths differ on the critical detail of body ownership: the
// non-stream path reads and closes inside the loop; the stream path
// hands the body out. Merging them would require a "read body?" flag
// that muddies both.
func (p *provider) openStream(ctx context.Context, reqBody []byte) (*http.Response, error) {
	const maxAttempts = 3
	var lastResp *http.Response
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
			p.baseURL+"/v1/messages", bytes.NewReader(reqBody))
		if err != nil {
			return nil, fmt.Errorf("anthropic: build stream request: %w", err)
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("accept", "text/event-stream")
		req.Header.Set("x-api-key", p.apiKey)
		req.Header.Set("anthropic-version", anthropicVersion)

		httpResp, err := p.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("anthropic: stream request failed: %w", err)
		}

		if httpResp.StatusCode == http.StatusTooManyRequests || httpResp.StatusCode >= 500 {
			if attempt < maxAttempts-1 {
				// Close the body so the connection can be reused by
				// the next attempt.
				io.Copy(io.Discard, httpResp.Body)
				httpResp.Body.Close()
				lastResp = nil
				continue
			}
		}
		return httpResp, nil
	}
	// Unreachable in practice — the final attempt always returns from
	// inside the loop — but the compiler wants a path out.
	return lastResp, nil
}

// toolCallAccumulator buffers the streamed arguments JSON for one
// tool_use content block. Keyed by the block's index inside the
// assistant message; id is asserted stable across deltas so a future
// Anthropic change that multiplexes parallel calls on the same index
// surfaces as a loud error rather than silent corruption.
type toolCallAccumulator struct {
	id      string
	name    string
	argsBuf bytes.Buffer
}

// streamUsage carries the token totals accumulated across
// message_start and message_delta events. Anthropic reports
// input_tokens up front and output_tokens progressively, so we combine
// both to emit the terminal StreamChunkUsage.
type streamUsage struct {
	model                 string
	inputTokens           int
	outputTokens          int
	cacheCreationTokens   int
	cacheReadTokens       int
}

// readStream drains the SSE body, translates events into
// hippo.StreamChunk, and writes them to out. Always closes out; always
// closes the http body. Terminal: either StreamChunkUsage (message_stop)
// or StreamChunkError (wire error). Context cancellation closes the
// channel without a StreamChunkError — the caller triggered it and
// already knows.
func (p *provider) readStream(ctx context.Context, httpResp *http.Response, out chan<- hippo.StreamChunk) {
	defer close(out)
	defer httpResp.Body.Close()

	// Body-close-on-cancel watcher: closing the body is the only way
	// to unblock a hung Read on the SSE scanner. done stops the watcher
	// when the stream ends naturally.
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
	accumulators := map[int]*toolCallAccumulator{}
	usage := streamUsage{}

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
				// Clean EOF without a message_stop — rare, but emit a
				// usage chunk from whatever we accumulated rather than
				// leave the caller uncertain.
				emit(buildUsageChunk(usage))
				return
			}
			if ctx.Err() != nil {
				return
			}
			emit(hippo.StreamChunk{Type: hippo.StreamChunkError,
				Error: fmt.Errorf("anthropic: stream read: %w", err)})
			return
		}

		terminal, err := handleAnthropicEvent(ev, accumulators, &usage, emit)
		if err != nil {
			emit(hippo.StreamChunk{Type: hippo.StreamChunkError, Error: err})
			return
		}
		if terminal {
			return
		}
	}
}

// handleAnthropicEvent processes one SSE event. Returns terminal=true
// when the event ends the stream (message_stop or error). emit is the
// send-with-ctx-cancel helper; its false return is treated as a signal
// to abort without surfacing a StreamChunkError (the context must have
// been cancelled).
func handleAnthropicEvent(
	ev *sse.Event,
	accumulators map[int]*toolCallAccumulator,
	usage *streamUsage,
	emit func(hippo.StreamChunk) bool,
) (terminal bool, err error) {
	// Anthropic also populates the JSON body's "type" field, which
	// always matches the SSE event: field. Prefer the SSE field — it's
	// cheaper than parsing JSON just to switch.
	switch ev.Event {
	case "ping", "":
		return false, nil

	case "message_start":
		var payload struct {
			Message struct {
				Model string `json:"model"`
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					OutputTokens             int `json:"output_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return false, fmt.Errorf("anthropic: parse message_start: %w", err)
		}
		usage.model = payload.Message.Model
		usage.inputTokens = payload.Message.Usage.InputTokens
		usage.outputTokens = payload.Message.Usage.OutputTokens
		usage.cacheCreationTokens = payload.Message.Usage.CacheCreationInputTokens
		usage.cacheReadTokens = payload.Message.Usage.CacheReadInputTokens
		return false, nil

	case "content_block_start":
		var payload struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return false, fmt.Errorf("anthropic: parse content_block_start: %w", err)
		}
		if payload.ContentBlock.Type == "tool_use" {
			accumulators[payload.Index] = &toolCallAccumulator{
				id:   payload.ContentBlock.ID,
				name: payload.ContentBlock.Name,
			}
		}
		return false, nil

	case "content_block_delta":
		var payload struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return false, fmt.Errorf("anthropic: parse content_block_delta: %w", err)
		}
		switch payload.Delta.Type {
		case "text_delta":
			if payload.Delta.Text != "" {
				if !emit(hippo.StreamChunk{Type: hippo.StreamChunkText, Delta: payload.Delta.Text}) {
					return true, nil
				}
			}
		case "thinking_delta":
			if payload.Delta.Thinking != "" {
				if !emit(hippo.StreamChunk{Type: hippo.StreamChunkThinking, Delta: payload.Delta.Thinking}) {
					return true, nil
				}
			}
		case "input_json_delta":
			acc, ok := accumulators[payload.Index]
			if !ok {
				return false, fmt.Errorf("anthropic: input_json_delta for unknown block index %d", payload.Index)
			}
			acc.argsBuf.WriteString(payload.Delta.PartialJSON)
		case "signature_delta":
			// Extended-thinking cryptographic signature; not surfaced.
		}
		return false, nil

	case "content_block_stop":
		var payload struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return false, fmt.Errorf("anthropic: parse content_block_stop: %w", err)
		}
		acc, ok := accumulators[payload.Index]
		if !ok {
			// Non-tool blocks (text/thinking) don't have an accumulator;
			// that's fine.
			return false, nil
		}
		delete(accumulators, payload.Index)
		args := acc.argsBuf.Bytes()
		if len(args) == 0 {
			// Tool call with no input — emit an empty JSON object
			// rather than leaving Arguments nil so consumers that
			// json.Unmarshal unconditionally don't blow up.
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

	case "message_delta":
		var payload struct {
			Usage struct {
				OutputTokens             int `json:"output_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			return false, fmt.Errorf("anthropic: parse message_delta: %w", err)
		}
		// message_delta usage is authoritative for output_tokens; it
		// reports the running total, so overwrite rather than add.
		if payload.Usage.OutputTokens > 0 {
			usage.outputTokens = payload.Usage.OutputTokens
		}
		if payload.Usage.CacheCreationInputTokens > 0 {
			usage.cacheCreationTokens = payload.Usage.CacheCreationInputTokens
		}
		if payload.Usage.CacheReadInputTokens > 0 {
			usage.cacheReadTokens = payload.Usage.CacheReadInputTokens
		}
		return false, nil

	case "message_stop":
		emit(buildUsageChunk(*usage))
		return true, nil

	case "error":
		var payload struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(ev.Data, &payload)
		msg := payload.Error.Message
		if msg == "" {
			msg = string(ev.Data)
		}
		return true, fmt.Errorf("anthropic: stream error: %s", msg)
	}

	// Unknown event types: silently skipped. Future Anthropic event
	// additions should not break existing clients.
	return false, nil
}

// buildUsageChunk turns the accumulated streamUsage into a terminal
// StreamChunkUsage. Cost is computed via computeCost (which reads the
// canonical budget pricing table), so the cache-write premium is
// priced correctly.
func buildUsageChunk(u streamUsage) hippo.StreamChunk {
	cached := u.cacheCreationTokens + u.cacheReadTokens
	cost, _ := computeCost(u.model, u.inputTokens, u.outputTokens,
		u.cacheCreationTokens, u.cacheReadTokens)
	return hippo.StreamChunk{
		Type: hippo.StreamChunkUsage,
		Usage: &hippo.Usage{
			InputTokens:  u.inputTokens,
			OutputTokens: u.outputTokens,
			CachedTokens: cached,
		},
		CostUSD:  cost,
		Provider: "anthropic",
		Model:    u.model,
	}
}
