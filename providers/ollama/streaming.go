package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

// stream opens a streaming /api/chat request and returns a channel
// of incremental hippo.StreamChunk values. Ollama streams NDJSON
// (one JSON object per line) rather than SSE; each object is a
// full chatResponse with Message.Content being the incremental delta.
// The terminal object has Done=true and carries the final usage plus
// any tool calls.
func (p *provider) stream(ctx context.Context, c hippo.Call) (<-chan hippo.StreamChunk, error) {
	model := p.model
	if c.Model != "" {
		model = c.Model
	}
	maxTokens := c.MaxTokens
	if maxTokens == 0 {
		maxTokens = 1024
	}

	req, err := p.buildRequest(c, model, maxTokens, true)
	if err != nil {
		return nil, err
	}
	reqBody, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal stream request: %w", err)
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
	go p.readNDJSONStream(ctx, httpResp, out)
	return out, nil
}

// openStream is the streaming handshake against /api/chat. Same retry
// shape as the other providers' streaming helpers: 429 and 5xx retry
// up to 3 attempts with exponential backoff, network errors surface
// immediately, ctx cancellation wins.
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
			p.baseURL+"/api/chat", bytes.NewReader(reqBody))
		if err != nil {
			return nil, fmt.Errorf("ollama: build stream request: %w", err)
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("accept", "application/x-ndjson")

		httpResp, err := p.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("ollama: stream request failed: %w", err)
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
	return nil, nil
}

// readNDJSONStream drains the NDJSON body, translates each line into
// a hippo.StreamChunk, and closes out when done. json.Decoder reads a
// sequence of JSON objects from the body with no manual line
// splitting - perfect for Ollama's framing.
//
// Terminal handling: when an object arrives with Done=true, emit any
// reassembled tool calls FIRST, then the terminal StreamChunkUsage.
// This matches the ordering consumers expect from the cloud providers
// (tool calls precede the terminal usage chunk so a consumer that
// stops reading after tool_call still sees every intermediate event).
func (p *provider) readNDJSONStream(ctx context.Context, httpResp *http.Response, out chan<- hippo.StreamChunk) {
	defer close(out)
	defer httpResp.Body.Close()

	// Body-close-on-ctx watcher, same pattern as Anthropic/OpenAI.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			httpResp.Body.Close()
		case <-done:
		}
	}()

	emit := func(chunk hippo.StreamChunk) bool {
		select {
		case out <- chunk:
			return true
		case <-ctx.Done():
			return false
		}
	}

	dec := json.NewDecoder(httpResp.Body)
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		var obj chatResponse
		if err := dec.Decode(&obj); err != nil {
			if errors.Is(err, io.EOF) {
				// Clean EOF without a Done=true terminal - treat as
				// a mid-stream failure so callers don't mistake it
				// for a successful usage chunk.
				emit(hippo.StreamChunk{Type: hippo.StreamChunkError,
					Error: errors.New("ollama: stream ended without done=true terminal")})
				return
			}
			if ctx.Err() != nil {
				return
			}
			emit(hippo.StreamChunk{Type: hippo.StreamChunkError,
				Error: fmt.Errorf("ollama: stream decode: %w", err)})
			return
		}

		if !obj.Done {
			if obj.Message.Content != "" {
				if !emit(hippo.StreamChunk{Type: hippo.StreamChunkText, Delta: obj.Message.Content}) {
					return
				}
			}
			continue
		}

		// Terminal: emit any tool calls before the usage chunk.
		for _, tc := range mapToolCalls(obj.Message.ToolCalls) {
			tc := tc
			if !emit(hippo.StreamChunk{Type: hippo.StreamChunkToolCall, ToolCall: &tc}) {
				return
			}
		}
		emit(hippo.StreamChunk{
			Type: hippo.StreamChunkUsage,
			Usage: &hippo.Usage{
				InputTokens:  obj.PromptEvalCount,
				OutputTokens: obj.EvalCount,
			},
			CostUSD:  0,
			Provider: "ollama",
			Model:    obj.Model,
		})
		return
	}
}
