package ollama

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

type echoTool struct{ invoked *bool }

func (e echoTool) Name() string        { return "echo" }
func (e echoTool) Description() string { return "Echoes the input text exactly as given." }
func (e echoTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)
}
func (e echoTool) Execute(_ context.Context, args json.RawMessage) (hippo.ToolResult, error) {
	*e.invoked = true
	var in struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(args, &in)
	return hippo.ToolResult{Content: in.Text}, nil
}

// toolCapableFamilies is the conservative list of installed-model
// name prefixes that reliably emit structured tool_calls. Models
// outside this list may still speak JSON blobs in their content,
// but hippo only parses the structured field — so we skip if none
// of these are installed rather than flake on partial support.
var toolCapableFamilies = []string{
	"llama3.1", "llama3.2", "llama3.3",
	"qwen2.5", "qwen3",
	"mistral",
}

func pickToolCapableModel(p *provider) string {
	for _, m := range p.Models() {
		lower := strings.ToLower(m.ID)
		for _, fam := range toolCapableFamilies {
			if strings.HasPrefix(lower, fam) {
				return m.ID
			}
		}
	}
	return ""
}

func TestIntegrationCallWithTools(t *testing.T) {
	if os.Getenv("HIPPO_RUN_INTEGRATION") != "1" {
		t.Skip("set HIPPO_RUN_INTEGRATION=1 to run")
	}
	if !ollamaReachable(defaultBaseURL) {
		t.Skip("local ollama daemon not reachable")
	}

	pr, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	model := pickToolCapableModel(pr.(*provider))
	if model == "" {
		t.Skip("no tool-capable model installed (need e.g. llama3.1, qwen2.5, mistral)")
	}

	var invoked bool
	brain, err := hippo.New(hippo.WithProvider(pr), hippo.WithTools(echoTool{invoked: &invoked}))
	if err != nil {
		t.Fatalf("Brain: %v", err)
	}
	defer brain.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	resp, err := brain.Call(ctx, hippo.Call{
		Model:     model,
		Prompt:    "Please use the echo tool with the text 'hippopotamus' and then tell me what it echoed.",
		MaxTokens: 400,
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !invoked {
		t.Fatalf("echo tool not invoked (model=%s, final=%q)", model, resp.Text)
	}
	t.Logf("model=%s final=%q", model, resp.Text)
}
