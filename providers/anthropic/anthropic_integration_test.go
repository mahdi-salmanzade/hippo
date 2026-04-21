package anthropic

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/internal/dotenv"
)

// TestIntegrationCallHaiku makes a real call to Anthropic's cheapest
// model (claude-haiku-4-5) to exercise the full request/response path
// end-to-end.
//
// Two gates, both required:
//   - HIPPO_RUN_INTEGRATION=1 - explicit opt-in. Prevents accidental
//     network calls during routine `go test ./...` runs.
//   - ANTHROPIC_API_KEY - set via .env (auto-loaded below) or the
//     process environment.
//
// Expected cost per run: < $0.001 (a few dozen tokens each direction).
func TestIntegrationCallHaiku(t *testing.T) {
	if os.Getenv("HIPPO_RUN_INTEGRATION") != "1" {
		t.Skip("set HIPPO_RUN_INTEGRATION=1 to run (hits real API)")
	}
	_ = dotenv.Load() // best-effort, picks up repo-root .env for local dev
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping integration test")
	}

	p, err := New(WithAPIKey(key), WithModel("claude-haiku-4-5"))
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
	if resp.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", resp.Provider)
	}
	if resp.Model == "" {
		t.Error("resp.Model is empty")
	}
	t.Logf("response: %q (%d in / %d out / $%.6f / %dms)",
		resp.Text, resp.Usage.InputTokens, resp.Usage.OutputTokens,
		resp.CostUSD, resp.LatencyMS)
}
