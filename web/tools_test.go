package web

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpendToolReturnsLocalState(t *testing.T) {
	state := NewState()
	state.Record(CallRecord{Provider: "anthropic", Model: "claude-sonnet-4-6", Task: "generate", CostUSD: 0.002})
	state.Record(CallRecord{Provider: "openai", Model: "gpt-5", Task: "classify", CostUSD: 0.0001})

	tool := &spendTool{state: state, bundle: func() *BrainBundle { return nil }}
	res, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", res.Content)
	}
	// Content must be JSON carrying the totals we recorded.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(res.Content), &parsed); err != nil {
		t.Fatalf("output not JSON: %v\n%s", err, res.Content)
	}
	if n, _ := parsed["call_count"].(float64); int(n) != 2 {
		t.Errorf("call_count = %v; want 2", parsed["call_count"])
	}
	if total, _ := parsed["total_usd"].(float64); total < 0.0020 || total > 0.0022 {
		t.Errorf("total_usd = %v; want ~0.0021", parsed["total_usd"])
	}
	// Provider / task / model arrays must all be present.
	for _, k := range []string{"by_provider", "by_task", "by_model"} {
		if _, ok := parsed[k]; !ok {
			t.Errorf("missing key %q in tool output: %s", k, res.Content)
		}
	}
}

func TestMemorySearchToolWithoutMemory(t *testing.T) {
	tool := &memorySearchTool{bundle: func() *BrainBundle { return nil }}
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"query":"hello"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true when no memory configured")
	}
}

func TestMemorySearchToolRejectsBadJSON(t *testing.T) {
	tool := &memorySearchTool{bundle: func() *BrainBundle { return &BrainBundle{} }}
	res, err := tool.Execute(context.Background(), json.RawMessage(`not json`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError=true for bad args")
	}
}

func TestPolicyReadToolReturnsFileContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	body := "version: 1\ndefault:\n  provider: anthropic\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{PolicyPath: path}
	tool := &policyReadTool{cfg: cfg}
	res, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected IsError: %s", res.Content)
	}
	if !strings.Contains(res.Content, "provider: anthropic") {
		t.Errorf("output missing policy body:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "path:") {
		t.Errorf("output missing path prefix:\n%s", res.Content)
	}
}

func TestPolicyReadToolMissingFile(t *testing.T) {
	cfg := &Config{PolicyPath: filepath.Join(t.TempDir(), "does-not-exist.yaml")}
	tool := &policyReadTool{cfg: cfg}
	res, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError for missing file")
	}
}

func TestBuiltinToolsRegisteredInBrain(t *testing.T) {
	// Sanity-check the wiring: Server.New seeds builtinTools and they
	// flow into BuildBrain. We exercise via freshServer.
	srv := freshServer(t)
	names := srv.builtinToolNames()
	want := []string{"hippo_spend", "hippo_memory_search", "hippo_policy_read"}
	for _, w := range want {
		found := false
		for _, n := range names {
			if n == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("builtin tool %q missing; got %v", w, names)
		}
	}
}
