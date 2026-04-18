package anthropic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

// fastRetries reduces the retry base delay to 1ms for the duration of
// the test. Tests that exercise retry behaviour use this so they don't
// sleep multiple seconds.
func fastRetries(t *testing.T) {
	t.Helper()
	old := retryBaseDelay
	retryBaseDelay = time.Millisecond
	t.Cleanup(func() { retryBaseDelay = old })
}

func TestNewRequiresAPIKey(t *testing.T) {
	_, err := New()
	if err == nil {
		t.Fatal("expected error when WithAPIKey is omitted, got nil")
	}
	if !strings.Contains(err.Error(), "API key") {
		t.Fatalf("expected error mentioning API key, got: %v", err)
	}
}

func TestNewDefaults(t *testing.T) {
	pr, err := New(WithAPIKey("test-key"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	concrete, ok := pr.(*provider)
	if !ok {
		t.Fatalf("unexpected concrete type %T", pr)
	}
	if concrete.model != defaultModel {
		t.Errorf("default model = %q, want %q", concrete.model, defaultModel)
	}
	if concrete.baseURL != defaultBaseURL {
		t.Errorf("default base URL = %q, want %q", concrete.baseURL, defaultBaseURL)
	}
	if concrete.timeout != defaultTimeout {
		t.Errorf("default timeout = %v, want %v", concrete.timeout, defaultTimeout)
	}
	if concrete.httpClient == nil {
		t.Error("httpClient is nil, want initialized")
	}
	if concrete.httpClient.Timeout != defaultTimeout {
		t.Errorf("httpClient.Timeout = %v, want %v", concrete.httpClient.Timeout, defaultTimeout)
	}
}

func TestModelsIncludesExpected(t *testing.T) {
	pr, _ := New(WithAPIKey("k"))
	models := pr.Models()
	if len(models) < 3 {
		t.Fatalf("expected at least 3 models, got %d", len(models))
	}
	expected := map[string]bool{
		"claude-opus-4-7":   false,
		"claude-sonnet-4-6": false,
		"claude-haiku-4-5":  false,
	}
	for _, m := range models {
		if _, ok := expected[m.ID]; ok {
			expected[m.ID] = true
		}
	}
	for id, found := range expected {
		if !found {
			t.Errorf("expected model %q in catalog, not found", id)
		}
	}
}

func TestEstimateCostNonZero(t *testing.T) {
	pr, _ := New(WithAPIKey("k"), WithModel("claude-haiku-4-5"))
	cost, err := pr.EstimateCost(hippo.Call{
		Prompt:    strings.Repeat("hello ", 1000), // ~6000 chars → ~1500 tokens
		MaxTokens: 500,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cost <= 0 {
		t.Fatalf("expected positive cost estimate, got %v", cost)
	}
}

func TestCallParsesMockResponse(t *testing.T) {
	const body = `{
		"id": "msg_123",
		"type": "message",
		"role": "assistant",
		"content": [{"type": "text", "text": "Hello, world!"}],
		"model": "claude-haiku-4-5",
		"stop_reason": "end_turn",
		"usage": {
			"input_tokens": 10,
			"output_tokens": 5,
			"cache_creation_input_tokens": 2,
			"cache_read_input_tokens": 3
		}
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q, want /v1/messages", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("x-api-key = %q, want test-key", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") != anthropicVersion {
			t.Errorf("anthropic-version = %q, want %q",
				r.Header.Get("anthropic-version"), anthropicVersion)
		}
		if r.Header.Get("content-type") != "application/json" {
			t.Errorf("content-type = %q, want application/json", r.Header.Get("content-type"))
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(
		WithAPIKey("test-key"),
		WithBaseURL(server.URL),
		WithModel("claude-haiku-4-5"),
	)

	resp, err := pr.Call(context.Background(), hippo.Call{Prompt: "hi"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != "Hello, world!" {
		t.Errorf("Text = %q, want %q", resp.Text, "Hello, world!")
	}
	if resp.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", resp.Provider)
	}
	if resp.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q, want claude-haiku-4-5", resp.Model)
	}
	if resp.Usage.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", resp.Usage.OutputTokens)
	}
	if resp.Usage.CachedTokens != 5 { // 2 creation + 3 read
		t.Errorf("CachedTokens = %d, want 5", resp.Usage.CachedTokens)
	}
	// Haiku 4.5: $1/Mtok in, $5/Mtok out, cache_write $1.25/Mtok, cache_read $0.10/Mtok
	// cost = 10*1e-6 + 5*5e-6 + 2*1.25e-6 + 3*0.10e-6 = 1e-5 + 2.5e-5 + 2.5e-6 + 3e-7
	// = 3.78e-5
	const want = 3.78e-5
	const tolerance = 1e-9
	if diff := resp.CostUSD - want; diff > tolerance || diff < -tolerance {
		t.Errorf("CostUSD = %v, want %v", resp.CostUSD, want)
	}
	if resp.LatencyMS < 0 {
		t.Errorf("LatencyMS = %d, want >= 0", resp.LatencyMS)
	}
}

func TestCallHandles429(t *testing.T) {
	fastRetries(t)

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{
			"id":"msg_ok","type":"message","role":"assistant",
			"content":[{"type":"text","text":"finally"}],
			"model":"claude-haiku-4-5",
			"usage":{"input_tokens":1,"output_tokens":1}
		}`)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(
		WithAPIKey("k"),
		WithBaseURL(server.URL),
		WithModel("claude-haiku-4-5"),
	)
	resp, err := pr.Call(context.Background(), hippo.Call{Prompt: "hi"})
	if err != nil {
		t.Fatalf("unexpected error after retry: %v", err)
	}
	if resp.Text != "finally" {
		t.Errorf("Text = %q, want %q", resp.Text, "finally")
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestCallHandlesAuthError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("bad"), WithBaseURL(server.URL))
	_, err := pr.Call(context.Background(), hippo.Call{Prompt: "hi"})
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !errors.Is(err, hippo.ErrAuthentication) {
		t.Errorf("err = %v, want wrapping hippo.ErrAuthentication", err)
	}
	if !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Errorf("err does not include upstream message: %v", err)
	}
}

func TestCallHonorsContextCancel(t *testing.T) {
	// The handler selects on r.Context().Done() first for the happy
	// case (connection closed mid-request → server context cancels),
	// with a short fallback so test teardown doesn't stall. Note:
	// Go's server does not reliably cancel r.Context() during an
	// in-flight handler when the client cancels — it's cancelled
	// after the handler returns. The fallback is what actually
	// bounds cleanup time in practice.
	const handlerFallback = 300 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(handlerFallback):
			w.WriteHeader(http.StatusGatewayTimeout)
		}
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := pr.Call(ctx, hippo.Call{Prompt: "hi"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error on context cancel, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want wrapping context.Canceled", err)
	}
	// Client-side cancellation must be fast regardless of what the
	// server does — the client does not wait for the server to
	// acknowledge.
	if elapsed > 200*time.Millisecond {
		t.Errorf("Call took %v, expected < 200ms (client-side cancel should be immediate)", elapsed)
	}
}

func TestCallReturnsModelNotFoundOn400InvalidModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"model: not a valid model id"}}`)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	_, err := pr.Call(context.Background(), hippo.Call{Prompt: "hi"})
	if !errors.Is(err, hippo.ErrModelNotFound) {
		t.Errorf("err = %v, want wrapping hippo.ErrModelNotFound", err)
	}
}

func TestCallReturnsUnavailableOn5xxAfterRetries(t *testing.T) {
	fastRetries(t)

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"try again later"}}`)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	_, err := pr.Call(context.Background(), hippo.Call{Prompt: "hi"})
	if !errors.Is(err, hippo.ErrProviderUnavailable) {
		t.Errorf("err = %v, want wrapping hippo.ErrProviderUnavailable", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3 (retries exhausted)", got)
	}
}

func TestLookupPricingHandlesDatedModelIDs(t *testing.T) {
	// Anthropic often echoes "claude-haiku-4-5-20250930" when the
	// request used "claude-haiku-4-5". Prefix-match (implemented in
	// budget.Lookup, consumed here) must resolve it.
	got, ok := lookupPricing("claude-haiku-4-5-20250930")
	if !ok {
		t.Fatal("lookupPricing returned ok=false for dated haiku id")
	}
	want, _ := lookupPricing("claude-haiku-4-5")
	if got != want {
		t.Errorf("lookupPricing = %+v, want %+v", got, want)
	}

	if _, ok := lookupPricing("some-other-model-99"); ok {
		t.Error("lookupPricing returned ok=true for unknown model")
	}
}

func TestComputeCostMatchesBudgetRates(t *testing.T) {
	// computeCost is the Anthropic-specific cost path (separate
	// cache_write bucket). Verify it computes against the canonical
	// budget table, not a duplicate local one.
	cost, ok := computeCost("claude-haiku-4-5", 10, 5, 2, 3)
	if !ok {
		t.Fatal("computeCost returned ok=false for known model")
	}
	// Haiku: in $1, out $5, cache_write $1.25, cache_read $0.10 per Mtok.
	// 10e-6 + 5*5e-6 + 2*1.25e-6 + 3*0.10e-6 = 3.78e-5
	const want = 3.78e-5
	if diff := cost - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("computeCost = %v, want %v", cost, want)
	}
}

// compile-time assertion: keep imports in use across all test
// permutations.
var _ = fmt.Sprint