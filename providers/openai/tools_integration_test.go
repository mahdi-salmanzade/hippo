package openai

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mahdi-salmanzade/hippo"
	"github.com/mahdi-salmanzade/hippo/internal/dotenv"
)

type echoTool struct{ invoked *bool }

func (e echoTool) Name() string        { return "echo" }
func (e echoTool) Description() string { return "Echoes the input text exactly as given. Use this when asked to repeat a specific word or phrase." }
func (e echoTool) Schema() json.RawMessage {
	// strict:true requires `additionalProperties: false` and every
	// property listed in required on OpenAI's side. Keep the schema
	// narrow.
	return json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`)
}
func (e echoTool) Execute(_ context.Context, args json.RawMessage) (hippo.ToolResult, error) {
	*e.invoked = true
	var in struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(args, &in)
	return hippo.ToolResult{Content: in.Text}, nil
}

func TestIntegrationCallWithTools(t *testing.T) {
	if os.Getenv("HIPPO_RUN_INTEGRATION") != "1" {
		t.Skip("set HIPPO_RUN_INTEGRATION=1 to run")
	}
	_ = dotenv.Load()
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}

	p, err := New(WithAPIKey(key), WithModel("gpt-5-nano"))
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

	resp, err := brain.Call(ctx, hippo.Call{
		Prompt:    "Please use the echo tool with the text 'hippopotamus' and then tell me what it echoed.",
		MaxTokens: 400,
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !invoked {
		t.Fatal("echo tool was not invoked")
	}
	if !strings.Contains(strings.ToLower(resp.Text), "hippopotamus") {
		t.Errorf("final text %q does not mention the echoed word", resp.Text)
	}
	t.Logf("final: %s", resp.Text)
}
