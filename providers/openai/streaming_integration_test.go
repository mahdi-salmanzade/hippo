package openai

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/internal/dotenv"
)

// TestIntegrationStreamGpt5Nano exercises the streaming path against
// real OpenAI infrastructure. Same gating shape as the non-stream
// integration test.
func TestIntegrationStreamGpt5Nano(t *testing.T) {
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

	ch, err := p.Stream(ctx, hippo.Call{
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
		t.Errorf("text chunks = %d, want >= 2", textCount)
	}
	if terminal == nil {
		t.Fatal("no terminal usage chunk")
	}
	if terminal.Usage.InputTokens <= 0 || terminal.Usage.OutputTokens <= 0 {
		t.Errorf("usage = %+v, want both > 0", terminal.Usage)
	}
	if terminal.CostUSD <= 0 {
		t.Errorf("cost = %v, want > 0", terminal.CostUSD)
	}
	t.Logf("streamed: %d text chunks, %d in / %d out / $%.6f",
		textCount, terminal.Usage.InputTokens, terminal.Usage.OutputTokens, terminal.CostUSD)
}
