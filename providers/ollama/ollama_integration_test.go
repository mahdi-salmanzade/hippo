package ollama

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

// ollamaReachable returns true when a local Ollama daemon answers
// /api/version within 2 seconds at the default base URL. Used to
// auto-skip integration tests when a daemon isn't running - distinct
// from HIPPO_RUN_INTEGRATION, which is the explicit opt-in.
func ollamaReachable(baseURL string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/version", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// pickModel returns the name of an installed model suitable for a
// cheap integration test. Prefers small, fast models. Falls back to
// whatever the daemon has installed first.
func pickModel(t *testing.T, p *provider) string {
	t.Helper()
	models := p.Models()
	if len(models) == 0 {
		t.Skip("no models installed on the local Ollama daemon")
	}
	// Prefer the known-small ones.
	preferred := []string{
		"qwen2.5:0.5b",
		"qwen2.5:1.5b",
		"phi3.5:3.8b",
		"phi3:3.8b",
		"llama3.2:1b",
		"llama3.2:3b",
	}
	installed := map[string]bool{}
	for _, m := range models {
		installed[m.ID] = true
	}
	for _, p := range preferred {
		if installed[p] {
			return p
		}
	}
	return models[0].ID
}

func TestIntegrationCallRoundtrip(t *testing.T) {
	if os.Getenv("HIPPO_RUN_INTEGRATION") != "1" {
		t.Skip("set HIPPO_RUN_INTEGRATION=1 to run")
	}
	if !ollamaReachable(defaultBaseURL) {
		t.Skip("local ollama daemon not reachable at " + defaultBaseURL)
	}

	pr, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	model := pickModel(t, pr.(*provider))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := pr.Call(ctx, hippo.Call{
		Model:     model,
		Prompt:    "Reply with exactly: OK",
		MaxTokens: 20,
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.Text == "" {
		t.Error("empty response text")
	}
	if resp.Usage.InputTokens <= 0 || resp.Usage.OutputTokens <= 0 {
		t.Errorf("usage = %+v, want both > 0", resp.Usage)
	}
	if resp.Provider != "ollama" {
		t.Errorf("Provider = %q", resp.Provider)
	}
	t.Logf("model=%s resp=%q usage=%+v latency=%dms", model, resp.Text, resp.Usage, resp.LatencyMS)
}

func TestIntegrationStreamRoundtrip(t *testing.T) {
	if os.Getenv("HIPPO_RUN_INTEGRATION") != "1" {
		t.Skip("set HIPPO_RUN_INTEGRATION=1 to run")
	}
	if !ollamaReachable(defaultBaseURL) {
		t.Skip("local ollama daemon not reachable at " + defaultBaseURL)
	}

	pr, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	model := pickModel(t, pr.(*provider))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ch, err := pr.Stream(ctx, hippo.Call{
		Model:     model,
		Prompt:    "Count from 1 to 5, one number per sentence.",
		MaxTokens: 200,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var textCount int
	var terminal *hippo.StreamChunk
	for c := range ch {
		switch c.Type {
		case hippo.StreamChunkText:
			textCount++
		case hippo.StreamChunkUsage:
			cc := c
			terminal = &cc
		case hippo.StreamChunkError:
			t.Fatalf("stream error: %v", c.Error)
		}
	}
	if textCount < 2 {
		t.Errorf("text chunks = %d, want >= 2 (stream should be incremental)", textCount)
	}
	if terminal == nil {
		t.Fatal("no terminal usage chunk")
	}
	t.Logf("streamed: model=%s chunks=%d usage=%+v", model, textCount, terminal.Usage)
}
