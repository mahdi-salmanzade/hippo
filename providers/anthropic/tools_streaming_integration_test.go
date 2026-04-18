package anthropic

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/internal/dotenv"
)

func TestIntegrationStreamWithTools(t *testing.T) {
	if os.Getenv("HIPPO_RUN_INTEGRATION") != "1" {
		t.Skip("set HIPPO_RUN_INTEGRATION=1 to run")
	}
	_ = dotenv.Load()
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}

	p, err := New(WithAPIKey(key), WithModel("claude-haiku-4-5"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var invoked bool
	brain, err := hippo.New(hippo.WithProvider(p), hippo.WithTools(echoTool{invoked: &invoked}))
	if err != nil {
		t.Fatalf("Brain: %v", err)
	}
	defer brain.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ch, err := brain.Stream(ctx, hippo.Call{
		Prompt:    "Please use the echo tool with the text 'streamerHippo' and then tell me what it echoed.",
		MaxTokens: 400,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var sawToolCall, sawToolResult bool
	var finalText strings.Builder
	for chunk := range ch {
		switch chunk.Type {
		case hippo.StreamChunkToolCall:
			sawToolCall = true
		case hippo.StreamChunkToolResult:
			sawToolResult = true
		case hippo.StreamChunkText:
			finalText.WriteString(chunk.Delta)
		case hippo.StreamChunkError:
			t.Fatalf("stream error: %v", chunk.Error)
		}
	}
	if !sawToolCall {
		t.Error("no StreamChunkToolCall observed")
	}
	if !sawToolResult {
		t.Error("no StreamChunkToolResult observed")
	}
	if !invoked {
		t.Error("echo tool was not executed")
	}
	if !strings.Contains(strings.ToLower(finalText.String()), "streamerhippo") {
		t.Errorf("final text %q missing echoed word", finalText.String())
	}
}
