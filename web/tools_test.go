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
	if n, _ := parsed["completed_calls"].(float64); int(n) != 2 {
		t.Errorf("completed_calls = %v; want 2", parsed["completed_calls"])
	}
	if n, _ := parsed["pending_calls"].(float64); int(n) != 0 {
		t.Errorf("pending_calls = %v; want 0", parsed["pending_calls"])
	}
	if total, _ := parsed["completed_usd"].(float64); total < 0.0020 || total > 0.0022 {
		t.Errorf("completed_usd = %v; want ~0.0021", parsed["completed_usd"])
	}
	// Provider / task / model arrays must all be present.
	for _, k := range []string{"by_provider", "by_task", "by_model"} {
		if _, ok := parsed[k]; !ok {
			t.Errorf("missing key %q in tool output: %s", k, res.Content)
		}
	}
}

func TestSpendToolExposesPendingCalls(t *testing.T) {
	state := NewState()
	// One completed call, one placeholder mid-stream.
	state.Record(CallRecord{Provider: "anthropic", Model: "claude-sonnet-4-6", Task: "generate", CostUSD: 0.005})
	state.Record(CallRecord{Task: "generate", Pending: true})

	tool := &spendTool{state: state, bundle: func() *BrainBundle { return nil }}
	res, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var parsed map[string]any
	_ = json.Unmarshal([]byte(res.Content), &parsed)

	if n, _ := parsed["completed_calls"].(float64); int(n) != 1 {
		t.Errorf("completed_calls = %v; want 1", parsed["completed_calls"])
	}
	if n, _ := parsed["pending_calls"].(float64); int(n) != 1 {
		t.Errorf("pending_calls = %v; want 1 (tool must surface the in-flight turn)", parsed["pending_calls"])
	}
	if total, _ := parsed["completed_usd"].(float64); total < 0.0049 || total > 0.0051 {
		t.Errorf("completed_usd = %v; want ~0.005 (pending row must not inflate totals)", parsed["completed_usd"])
	}
	// The summary must mention the pending turn explicitly — that's
	// the whole point of pre-formatting it. If the string doesn't say
	// "in flight" the model will skip it half the time.
	summary, _ := parsed["summary"].(string)
	if !strings.Contains(summary, "in flight") {
		t.Errorf("summary missing 'in flight' note:\n%s", summary)
	}
	if !strings.Contains(summary, "0.005") {
		t.Errorf("summary missing completed total:\n%s", summary)
	}
}

func TestSpendToolSummaryEmptyState(t *testing.T) {
	tool := &spendTool{state: NewState(), bundle: func() *BrainBundle { return nil }}
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{}`))
	var parsed map[string]any
	_ = json.Unmarshal([]byte(res.Content), &parsed)
	summary, _ := parsed["summary"].(string)
	if !strings.Contains(summary, "No completed") {
		t.Errorf("empty-state summary should say 'No completed…'; got:\n%s", summary)
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
