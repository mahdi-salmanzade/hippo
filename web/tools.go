package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/mahdi-salmanzade/hippo"
)

// Built-in hippo tools surfaced to the chat. These make the model
// first-class aware of local state — spend, memory, policy — so the UI
// suggestion chips ("Show today's spend", "Validate my policy") aren't
// just empty promises. Each tool is a small wrapper around read-only
// operations already exposed elsewhere in the web package.
//
// # Why closures, not struct references
//
// BrainBundle is swapped atomically on /config POST and /policy POST
// (ReplaceBundle). A tool registered once at server start must always
// see the *current* bundle — not the one that was in scope at
// registration time. Hence the func() *BrainBundle indirection.
//
// # Why read-only
//
// These tools run inside a model turn without user confirmation. We
// expose lookups (spend totals, memory search, policy contents) but
// deliberately not mutations (no prune, no write). If the model asks
// to mutate, the user goes through the UI.

// newBuiltinTools constructs the default tool set for the web server.
// bundle is a live accessor — it must return the current *BrainBundle
// at invocation time, not a cached snapshot.
func newBuiltinTools(state *State, cfg *Config, bundle func() *BrainBundle) []hippo.Tool {
	return []hippo.Tool{
		&spendTool{state: state, bundle: bundle},
		&memorySearchTool{bundle: bundle},
		&policyReadTool{cfg: cfg},
	}
}

// ── hippo_spend ─────────────────────────────────────────────────────

type spendTool struct {
	state  *State
	bundle func() *BrainBundle
}

func (t *spendTool) Name() string { return "hippo_spend" }
func (t *spendTool) Description() string {
	return "Returns the user's LLM spend summary. Fields:\n" +
		"- completed_usd, completed_calls: totals over finished turns\n" +
		"- pending_calls: turns currently in flight (including this one)\n" +
		"- by_provider, by_task, by_model: breakdowns across completed turns\n" +
		"- budget.spent_usd / budget.remaining_usd: daily budget status\n" +
		"Use this whenever the user asks about cost, spend, budget, or how " +
		"much they've used. Data is local — nothing leaves the user's machine. " +
		"When pending_calls > 0, tell the user how many turns are still " +
		"completing and that their cost will be reflected once they finish — " +
		"don't pretend the pending turns have zero cost."
}
func (t *spendTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}
func (t *spendTool) Execute(ctx context.Context, args json.RawMessage) (hippo.ToolResult, error) {
	// Split completed vs pending so the model can talk accurately
	// about the current tool-calling turn instead of either
	// undercounting (ignoring the pending row) or overcounting
	// (treating a $0 placeholder as real spend).
	out := map[string]any{
		"completed_usd":   t.state.TotalSpend(),
		"completed_calls": t.state.CompletedCount(),
		"pending_calls":   t.state.PendingCount(),
		"by_provider":     t.state.SpendByProvider(),
		"by_task":         t.state.SpendByTask(),
		"by_model":        t.state.SpendByModel(),
	}
	if b := t.bundle(); b != nil && b.Budget != nil {
		out["budget"] = map[string]any{
			"spent_usd":     b.Budget.Spent(),
			"remaining_usd": b.Budget.Remaining(),
		}
	}
	buf, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return hippo.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	return hippo.ToolResult{Content: string(buf)}, nil
}

// ── hippo_memory_search ─────────────────────────────────────────────

type memorySearchTool struct {
	bundle func() *BrainBundle
}

func (t *memorySearchTool) Name() string { return "hippo_memory_search" }
func (t *memorySearchTool) Description() string {
	return "Searches the user's local memory store (SQLite + optional vector " +
		"index) and returns matching records with timestamp, kind, importance, " +
		"content, and tags. Use this whenever the user asks about what they " +
		"remember, what's been stored, or wants to recall a past detail. " +
		"Modes: 'keyword' (FTS), 'semantic' (embedding similarity — requires " +
		"an embedder), 'hybrid' (blend), 'recent' (most recent regardless of " +
		"query). Default 'hybrid'."
}
func (t *memorySearchTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "search query"},
			"mode":  {"type": "string", "enum": ["keyword","semantic","hybrid","recent"], "default": "hybrid"},
			"limit": {"type": "integer", "minimum": 1, "maximum": 50, "default": 10}
		},
		"required": ["query"],
		"additionalProperties": false
	}`)
}
func (t *memorySearchTool) Execute(ctx context.Context, args json.RawMessage) (hippo.ToolResult, error) {
	var p struct {
		Query string `json:"query"`
		Mode  string `json:"mode"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return hippo.ToolResult{Content: "invalid arguments: " + err.Error(), IsError: true}, nil
	}
	if p.Limit <= 0 || p.Limit > 50 {
		p.Limit = 10
	}
	b := t.bundle()
	if b == nil || b.Memory == nil {
		return hippo.ToolResult{Content: "memory is not configured on this hippo instance", IsError: true}, nil
	}
	q := hippo.MemoryQuery{Limit: p.Limit}
	switch p.Mode {
	case "semantic":
		q.Semantic = true
	case "hybrid", "":
		q.Semantic = true
		q.HybridWeight = 0.6
		q.TemporalExpansion = 30 * time.Minute
	case "recent":
		p.Query = "" // fall through to recency
	case "keyword":
		// Plain FTS.
	}
	recs, err := b.Memory.Recall(ctx, p.Query, q)
	if err != nil {
		return hippo.ToolResult{Content: "recall failed: " + err.Error(), IsError: true}, nil
	}
	// Return a compact view — dropping embeddings to keep input tokens low.
	type view struct {
		ID         string    `json:"id"`
		Kind       string    `json:"kind"`
		Timestamp  time.Time `json:"timestamp"`
		Content    string    `json:"content"`
		Tags       []string  `json:"tags,omitempty"`
		Importance float64   `json:"importance"`
		Source     string    `json:"source,omitempty"`
	}
	out := make([]view, 0, len(recs))
	for _, r := range recs {
		out = append(out, view{
			ID:         r.ID,
			Kind:       string(r.Kind),
			Timestamp:  r.Timestamp,
			Content:    r.Content,
			Tags:       r.Tags,
			Importance: r.Importance,
			Source:     r.Source,
		})
	}
	buf, err := json.MarshalIndent(map[string]any{
		"count":   len(out),
		"records": out,
	}, "", "  ")
	if err != nil {
		return hippo.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	return hippo.ToolResult{Content: string(buf)}, nil
}

// ── hippo_policy_read ───────────────────────────────────────────────

type policyReadTool struct {
	cfg *Config
}

func (t *policyReadTool) Name() string { return "hippo_policy_read" }
func (t *policyReadTool) Description() string {
	return "Reads the current hippo routing policy YAML from disk and returns " +
		"its full contents as a string. Use this when the user asks to " +
		"validate, explain, inspect, or summarize their policy. The policy " +
		"defines which provider/model handles each task and any budget/fallback " +
		"rules. Read-only — does not modify the file."
}
func (t *policyReadTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}
func (t *policyReadTool) Execute(ctx context.Context, args json.RawMessage) (hippo.ToolResult, error) {
	path, err := ExpandPath(t.cfg.PolicyPath)
	if err != nil {
		return hippo.ToolResult{Content: "bad policy path: " + err.Error(), IsError: true}, nil
	}
	if path == "" {
		return hippo.ToolResult{Content: "no policy file configured", IsError: true}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return hippo.ToolResult{Content: fmt.Sprintf("policy file %s does not exist", path), IsError: true}, nil
		}
		return hippo.ToolResult{Content: "read failed: " + err.Error(), IsError: true}, nil
	}
	return hippo.ToolResult{Content: fmt.Sprintf("# path: %s\n\n%s", path, data)}, nil
}
