package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

// fastRetries reduces the retry base delay to 1ms so rate-limit tests
// don't sleep multiple seconds between attempts.
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
	c, ok := pr.(*provider)
	if !ok {
		t.Fatalf("unexpected concrete type %T", pr)
	}
	if c.model != defaultModel {
		t.Errorf("default model = %q, want %q", c.model, defaultModel)
	}
	if c.baseURL != defaultBaseURL {
		t.Errorf("default base URL = %q, want %q", c.baseURL, defaultBaseURL)
	}
	if c.timeout != defaultTimeout {
		t.Errorf("default timeout = %v, want %v", c.timeout, defaultTimeout)
	}
	if c.httpClient == nil {
		t.Error("httpClient is nil, want initialized")
	}
	if c.httpClient.Timeout != defaultTimeout {
		t.Errorf("httpClient.Timeout = %v, want %v", c.httpClient.Timeout, defaultTimeout)
	}
}

func TestModelsDerivedFromPricingTable(t *testing.T) {
	pr, _ := New(WithAPIKey("k"))
	models := pr.Models()
	expected := map[string]bool{
		"gpt-5":      false,
		"gpt-5-mini": false,
		"gpt-5-nano": false,
	}
	for _, m := range models {
		if _, ok := expected[m.ID]; ok {
			expected[m.ID] = true
		}
		if m.ContextTokens <= 0 {
			t.Errorf("model %q has ContextTokens = %d, want > 0", m.ID, m.ContextTokens)
		}
	}
	for id, found := range expected {
		if !found {
			t.Errorf("expected model %q in catalog, not found", id)
		}
	}
}

func TestEstimateCostNonZero(t *testing.T) {
	pr, _ := New(WithAPIKey("k"), WithModel("gpt-5-nano"))
	cost, err := pr.EstimateCost(hippo.Call{
		Prompt:    strings.Repeat("hello ", 1000),
		MaxTokens: 500,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cost <= 0 {
		t.Fatalf("expected positive cost estimate, got %v", cost)
	}
}

func TestEstimateCostUnknownModel(t *testing.T) {
	pr, _ := New(WithAPIKey("k"), WithModel("not-a-real-model"))
	_, err := pr.EstimateCost(hippo.Call{Prompt: "hi", MaxTokens: 10})
	if err == nil {
		t.Fatal("expected error for unknown model, got nil")
	}
}

func TestCallPromptOnlyUsesBareStringInput(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, minimalSuccessBody("gpt-5-nano"))
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL), WithModel("gpt-5-nano"))
	if _, err := pr.Call(context.Background(), hippo.Call{Prompt: "hello"}); err != nil {
		t.Fatalf("Call: %v", err)
	}

	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("parse request body: %v", err)
	}
	var input any
	if err := json.Unmarshal(parsed["input"], &input); err != nil {
		t.Fatalf("parse input field: %v", err)
	}
	s, ok := input.(string)
	if !ok {
		t.Fatalf("input = %T, want string", input)
	}
	if s != "hello" {
		t.Errorf("input string = %q, want %q", s, "hello")
	}
	if inst, ok := parsed["instructions"]; ok && string(inst) != `""` && len(inst) > 0 {
		// instructions should either be absent (omitempty dropped it)
		// or the empty string; anything else means a system message
		// leaked in.
		var asStr string
		if err := json.Unmarshal(inst, &asStr); err == nil && asStr != "" {
			t.Errorf("instructions = %q, want empty for prompt-only call", asStr)
		}
	}
}

func TestCallMessagesWithSystemGoesToInstructions(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, minimalSuccessBody("gpt-5-nano"))
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL), WithModel("gpt-5-nano"))
	_, err := pr.Call(context.Background(), hippo.Call{
		Messages: []hippo.Message{
			{Role: "system", Content: "You are a pirate."},
			{Role: "user", Content: "Hello."},
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	var parsed struct {
		Instructions string          `json:"instructions"`
		Input        json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(gotBody, &parsed); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if parsed.Instructions != "You are a pirate." {
		t.Errorf("instructions = %q, want %q", parsed.Instructions, "You are a pirate.")
	}

	var msgs []inputMessage
	if err := json.Unmarshal(parsed.Input, &msgs); err != nil {
		t.Fatalf("parse input array: %v", err)
	}
	for _, m := range msgs {
		if m.Role == "system" {
			t.Errorf("input contains system-role message %+v; should have been moved to instructions", m)
		}
	}
	if len(msgs) != 1 || msgs[0].Role != "user" || msgs[0].Content != "Hello." {
		t.Errorf("input messages = %+v, want one user message \"Hello.\"", msgs)
	}
}

func TestCallParsesMessageOutput(t *testing.T) {
	const body = `{
		"id": "resp_123",
		"object": "response",
		"model": "gpt-5-nano",
		"status": "completed",
		"output": [
			{"type": "message", "role": "assistant",
			 "content": [{"type": "output_text", "text": "Hello, world!"}]}
		],
		"usage": {
			"input_tokens": 10,
			"output_tokens": 5,
			"input_tokens_details": {"cached_tokens": 3},
			"output_tokens_details": {"reasoning_tokens": 0}
		}
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %q, want /v1/responses", r.URL.Path)
		}
		if got := r.Header.Get("authorization"); got != "Bearer test-key" {
			t.Errorf("authorization = %q, want %q", got, "Bearer test-key")
		}
		if r.Header.Get("content-type") != "application/json" {
			t.Errorf("content-type = %q, want application/json", r.Header.Get("content-type"))
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("test-key"), WithBaseURL(server.URL), WithModel("gpt-5-nano"))
	resp, err := pr.Call(context.Background(), hippo.Call{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Text != "Hello, world!" {
		t.Errorf("Text = %q, want %q", resp.Text, "Hello, world!")
	}
	if resp.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", resp.Provider)
	}
	if resp.Model != "gpt-5-nano" {
		t.Errorf("Model = %q, want gpt-5-nano", resp.Model)
	}
	if resp.Usage.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", resp.Usage.OutputTokens)
	}
	if resp.Usage.CachedTokens != 3 {
		t.Errorf("CachedTokens = %d, want 3", resp.Usage.CachedTokens)
	}
	if resp.LatencyMS < 0 {
		t.Errorf("LatencyMS = %d, want >= 0", resp.LatencyMS)
	}
}

func TestCallParsesReasoningOutput(t *testing.T) {
	const body = `{
		"id": "resp_r",
		"object": "response",
		"model": "gpt-5",
		"status": "completed",
		"output": [
			{"type": "reasoning",
			 "summary": [{"type": "summary_text", "text": "Thinking about it..."}]},
			{"type": "message", "role": "assistant",
			 "content": [{"type": "output_text", "text": "Final answer."}]}
		],
		"usage": {
			"input_tokens": 20,
			"output_tokens": 8,
			"input_tokens_details": {"cached_tokens": 0},
			"output_tokens_details": {"reasoning_tokens": 4}
		}
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL), WithModel("gpt-5"))
	resp, err := pr.Call(context.Background(), hippo.Call{Prompt: "solve"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Thinking != "Thinking about it..." {
		t.Errorf("Thinking = %q, want %q", resp.Thinking, "Thinking about it...")
	}
	if resp.Text != "Final answer." {
		t.Errorf("Text = %q, want %q", resp.Text, "Final answer.")
	}
}

func TestCallComputesCostViaBudget(t *testing.T) {
	const body = `{
		"id": "resp_c",
		"object": "response",
		"model": "gpt-5-nano",
		"status": "completed",
		"output": [{"type": "message", "role": "assistant",
			"content": [{"type": "output_text", "text": "ok"}]}],
		"usage": {
			"input_tokens": 1000,
			"output_tokens": 100,
			"input_tokens_details": {"cached_tokens": 500},
			"output_tokens_details": {"reasoning_tokens": 0}
		}
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL), WithModel("gpt-5-nano"))
	resp, err := pr.Call(context.Background(), hippo.Call{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// gpt-5-nano: in $0.10/Mtok, out $0.40/Mtok, cached $0.01/Mtok.
	// plain_in = (1000-500)*0.10/M = 5e-5
	// cached   = 500*0.01/M = 5e-6
	// out      = 100*0.40/M = 4e-5
	// total    = 9.5e-5
	const want = 9.5e-5
	const tol = 1e-9
	if diff := resp.CostUSD - want; diff > tol || diff < -tol {
		t.Errorf("CostUSD = %v, want %v", resp.CostUSD, want)
	}
}

func TestCallHandles429(t *testing.T) {
	fastRetries(t)

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"type":"rate_limit","message":"slow down"}}`)
			return
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, minimalSuccessBody("gpt-5-nano"))
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL), WithModel("gpt-5-nano"))
	resp, err := pr.Call(context.Background(), hippo.Call{Prompt: "hi"})
	if err != nil {
		t.Fatalf("unexpected error after retry: %v", err)
	}
	if resp.Text == "" {
		t.Error("Text empty after retry")
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestCallHandles401(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"invalid api key"}}`)
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
	if !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("err missing upstream message: %v", err)
	}
}

func TestCallHandlesContentPolicyError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"content_policy_violation","message":"Your request was rejected as a result of our safety system."}}`)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL))
	_, err := pr.Call(context.Background(), hippo.Call{Prompt: "bad prompt"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, hippo.ErrContentPolicy) {
		t.Errorf("err = %v, want wrapping hippo.ErrContentPolicy", err)
	}
}

func TestCallHonorsContextCancel(t *testing.T) {
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
	if elapsed > 200*time.Millisecond {
		t.Errorf("Call took %v, expected < 200ms (client-side cancel should be immediate)", elapsed)
	}
}

func TestCallHandlesDatedModelID(t *testing.T) {
	// OpenAI occasionally echoes a dated model id ("gpt-5-2026-02-15")
	// when the request used the alias. Prefix-match (via budget.Lookup)
	// must still produce a non-zero cost.
	const body = `{
		"id": "resp_dated",
		"object": "response",
		"model": "gpt-5-2026-02-15",
		"status": "completed",
		"output": [{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],
		"usage": {
			"input_tokens": 100,
			"output_tokens": 50,
			"input_tokens_details": {"cached_tokens": 0},
			"output_tokens_details": {"reasoning_tokens": 0}
		}
	}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	pr, _ := New(WithAPIKey("k"), WithBaseURL(server.URL), WithModel("gpt-5"))
	resp, err := pr.Call(context.Background(), hippo.Call{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.CostUSD <= 0 {
		t.Errorf("CostUSD = %v, want > 0 for dated model id", resp.CostUSD)
	}
}

func TestStreamReturnsNotImplemented(t *testing.T) {
	pr, _ := New(WithAPIKey("k"))
	ch, err := pr.Stream(context.Background(), hippo.Call{Prompt: "hi"})
	if ch != nil {
		t.Error("Stream returned non-nil channel, want nil")
	}
	if !errors.Is(err, hippo.ErrNotImplemented) {
		t.Errorf("err = %v, want hippo.ErrNotImplemented", err)
	}
}

func TestOrgAndProjectHeadersSent(t *testing.T) {
	var gotOrg, gotProj string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrg = r.Header.Get("openai-organization")
		gotProj = r.Header.Get("openai-project")
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, minimalSuccessBody("gpt-5-nano"))
	}))
	t.Cleanup(server.Close)

	pr, _ := New(
		WithAPIKey("k"),
		WithBaseURL(server.URL),
		WithModel("gpt-5-nano"),
		WithOrganization("org-abc"),
		WithProject("proj-xyz"),
	)
	if _, err := pr.Call(context.Background(), hippo.Call{Prompt: "hi"}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if gotOrg != "org-abc" {
		t.Errorf("openai-organization header = %q, want org-abc", gotOrg)
	}
	if gotProj != "proj-xyz" {
		t.Errorf("openai-project header = %q, want proj-xyz", gotProj)
	}
}

// minimalSuccessBody returns a valid Responses-API success JSON with
// one message-type output item, usable as a generic "OK" mock.
func minimalSuccessBody(model string) string {
	return `{
		"id":"resp_ok","object":"response","model":"` + model + `","status":"completed",
		"output":[{"type":"message","role":"assistant",
			"content":[{"type":"output_text","text":"ok"}]}],
		"usage":{"input_tokens":1,"output_tokens":1,
			"input_tokens_details":{"cached_tokens":0},
			"output_tokens_details":{"reasoning_tokens":0}}
	}`
}
