package openai

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/internal/dotenv"
)

// TestIntegrationCallGpt5Nano makes a real call to OpenAI's cheapest
// Responses-API model (gpt-5-nano) to exercise the full request/response
// path end-to-end.
//
// Two gates, both required:
//   - HIPPO_RUN_INTEGRATION=1 - explicit opt-in. Prevents accidental
//     network calls during routine `go test ./...` runs.
//   - OPENAI_API_KEY - set via .env (auto-loaded below) or the process
//     environment.
//
// Expected cost per run: under $0.001.
func TestIntegrationCallGpt5Nano(t *testing.T) {
	if os.Getenv("HIPPO_RUN_INTEGRATION") != "1" {
		t.Skip("set HIPPO_RUN_INTEGRATION=1 to run (hits real API)")
	}
	_ = dotenv.Load()
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set; skipping integration test")
	}

	p, err := New(WithAPIKey(key), WithModel("gpt-5-nano"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := p.Call(ctx, hippo.Call{
		Prompt:    "Reply with exactly: OK",
		MaxTokens: 20,
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if resp.Text == "" {
		t.Error("resp.Text is empty")
	}
	if resp.Usage.InputTokens <= 0 {
		t.Errorf("Usage.InputTokens = %d, want > 0", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens <= 0 {
		t.Errorf("Usage.OutputTokens = %d, want > 0", resp.Usage.OutputTokens)
	}
	if resp.CostUSD <= 0 {
		t.Errorf("CostUSD = %v, want > 0", resp.CostUSD)
	}
	if resp.LatencyMS <= 0 {
		t.Errorf("LatencyMS = %d, want > 0", resp.LatencyMS)
	}
	if resp.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", resp.Provider)
	}
	if resp.Model == "" {
		t.Error("resp.Model is empty")
	}
	t.Logf("response: %q (%d in / %d out / $%.6f / %dms)",
		resp.Text, resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.CostUSD, resp.LatencyMS)
}
