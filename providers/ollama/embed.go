package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

// DefaultEmbeddingModel is what NewEmbedder uses when no model is
// specified. nomic-embed-text is 768-dimensional, CPU-fast, and
// widely-installed by default.
const DefaultEmbeddingModel = "nomic-embed-text"

// embedder implements hippo.Embedder against Ollama's embedding
// endpoints. It prefers the batch /api/embed and falls back to a
// serial loop over the older /api/embeddings for servers that haven't
// shipped the newer endpoint yet.
type embedder struct {
	baseURL    string
	model      string
	httpClient *http.Client

	dimsMu sync.Mutex
	dims   int

	// preferSingle, once set, pins the embedder to the /api/embeddings
	// fallback path. Flipped on after a /api/embed 404 so we don't
	// retry the missing endpoint every batch.
	preferSingle bool
}

// EmbedderOption configures an embedder during construction.
type EmbedderOption func(*embedder)

// WithEmbedderBaseURL overrides the default Ollama endpoint.
func WithEmbedderBaseURL(u string) EmbedderOption {
	return func(e *embedder) { e.baseURL = u }
}

// WithEmbedderModel sets the embedding model id.
func WithEmbedderModel(m string) EmbedderOption {
	return func(e *embedder) { e.model = m }
}

// WithEmbedderHTTPClient lets callers supply a pre-configured HTTP
// client (custom transport, test doubles, proxy).
func WithEmbedderHTTPClient(c *http.Client) EmbedderOption {
	return func(e *embedder) { e.httpClient = c }
}

// NewEmbedder constructs an Ollama-backed hippo.Embedder. The daemon
// does not need to be reachable at construction time; unreachable
// servers surface as errors on the first Embed call.
func NewEmbedder(opts ...EmbedderOption) hippo.Embedder {
	e := &embedder{
		baseURL: defaultBaseURL,
		model:   DefaultEmbeddingModel,
	}
	for _, o := range opts {
		o(e)
	}
	if e.httpClient == nil {
		e.httpClient = &http.Client{Timeout: 60 * time.Second}
	}
	return e
}

// Name returns "ollama:<model>". Memory stores compare this string
// byte-for-byte to decide whether an existing embedding is still
// valid, so any model override propagates as a different identity.
func (e *embedder) Name() string { return "ollama:" + e.model }

// Dimensions reports the vector length from the most recent successful
// Embed call. Zero before the first call — callers that need the
// value up front should issue a probe embedding first.
func (e *embedder) Dimensions() int {
	e.dimsMu.Lock()
	defer e.dimsMu.Unlock()
	return e.dims
}

// Embed returns one vector per input text. Uses /api/embed (batch)
// by default; falls back to a serial loop over /api/embeddings if the
// server responds with 404 on the newer endpoint.
func (e *embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	if !e.preferSingle {
		vectors, err := e.embedBatch(ctx, texts)
		if err == nil {
			e.recordDims(vectors)
			return vectors, nil
		}
		// The only error we silently downgrade on is 404 — any other
		// failure (transport, 5xx, malformed JSON) propagates.
		if !errors.Is(err, errEmbedEndpointMissing) {
			return nil, err
		}
		e.preferSingle = true
	}

	vectors, err := e.embedSerial(ctx, texts)
	if err != nil {
		return nil, err
	}
	e.recordDims(vectors)
	return vectors, nil
}

func (e *embedder) recordDims(vectors [][]float32) {
	if len(vectors) == 0 || len(vectors[0]) == 0 {
		return
	}
	e.dimsMu.Lock()
	e.dims = len(vectors[0])
	e.dimsMu.Unlock()
}

// errEmbedEndpointMissing signals that /api/embed returned 404 and
// the embedder should fall back to /api/embeddings.
var errEmbedEndpointMissing = errors.New("ollama: /api/embed missing")

// embedBatch posts every input to /api/embed in one request.
func (e *embedder) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}{Model: e.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("ollama: embed marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: embed request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, errEmbedEndpointMissing
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ollama: embed read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ollama: embed %d: %s", resp.StatusCode, string(raw))
	}

	var out struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("ollama: embed decode: %w", err)
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama: embed returned %d vectors for %d inputs",
			len(out.Embeddings), len(texts))
	}
	return out.Embeddings, nil
}

// embedSerial loops /api/embeddings one text at a time — the legacy
// single-input endpoint older Ollama builds (pre-0.1.44) still offer.
func (e *embedder) embedSerial(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		vec, err := e.embedOne(ctx, text)
		if err != nil {
			return nil, err
		}
		out = append(out, vec)
	}
	return out, nil
}

func (e *embedder) embedOne(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}{Model: e.model, Prompt: text})
	if err != nil {
		return nil, fmt.Errorf("ollama: embed marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: embed request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ollama: embed read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("ollama: embed %d: %s", resp.StatusCode, string(raw))
	}
	var out struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("ollama: embed decode: %w", err)
	}
	return out.Embedding, nil
}
