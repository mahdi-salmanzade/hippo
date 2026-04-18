package hippo

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeTool is a minimal Tool for tests that don't care about
// execution semantics — they just need something implementing the
// interface with a given name.
type fakeTool struct {
	name        string
	description string
	schema      string
	exec        func(ctx context.Context, args json.RawMessage) (ToolResult, error)
}

func (f *fakeTool) Name() string                 { return f.name }
func (f *fakeTool) Description() string          { return f.description }
func (f *fakeTool) Schema() json.RawMessage      { return json.RawMessage(f.schema) }
func (f *fakeTool) Execute(ctx context.Context, args json.RawMessage) (ToolResult, error) {
	if f.exec == nil {
		return ToolResult{Content: "ok"}, nil
	}
	return f.exec(ctx, args)
}

func newFake(name string) *fakeTool {
	return &fakeTool{name: name, description: name + " tool", schema: `{"type":"object"}`}
}

func TestNewToolSetRejectsDuplicateNames(t *testing.T) {
	_, err := NewToolSet(newFake("same"), newFake("same"))
	if err == nil {
		t.Fatal("expected duplicate-name error, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("err = %v, want to mention 'duplicate'", err)
	}
}

func TestNewToolSetRejectsInvalidName(t *testing.T) {
	cases := []struct{ name, desc string }{
		{"", "empty"},
		{"1leading_digit", "leading digit"},
		{"has space", "space"},
		{"dash-name", "dash"},
		{"dot.name", "dot"},
		{strings.Repeat("x", 65), "too long"},
	}
	for _, tc := range cases {
		_, err := NewToolSet(newFake(tc.name))
		if err == nil {
			t.Errorf("NewToolSet(%q) = nil, want error (%s)", tc.name, tc.desc)
		}
	}

	// Control: 64-char name + common legal shapes pass.
	good := []string{"a", "A", "_x", "x9", "alpha_beta", strings.Repeat("x", 64)}
	for _, g := range good {
		if _, err := NewToolSet(newFake(g)); err != nil {
			t.Errorf("NewToolSet(%q): unexpected error %v", g, err)
		}
	}
}

func TestNewToolSetRejectsNilTool(t *testing.T) {
	_, err := NewToolSet(newFake("ok"), nil)
	if err == nil {
		t.Fatal("expected nil-tool error, got nil")
	}
}

func TestToolSetGetAndNames(t *testing.T) {
	ts, err := NewToolSet(newFake("bravo"), newFake("alpha"))
	if err != nil {
		t.Fatalf("NewToolSet: %v", err)
	}
	if got, ok := ts.Get("alpha"); !ok || got.Name() != "alpha" {
		t.Errorf("Get(alpha) = (%v, %v)", got, ok)
	}
	if _, ok := ts.Get("missing"); ok {
		t.Error("Get(missing) = ok=true")
	}
	names := ts.Names()
	if len(names) != 2 || names[0] != "alpha" || names[1] != "bravo" {
		t.Errorf("Names() = %v, want [alpha bravo] (sorted)", names)
	}
}

func TestToolSetLen(t *testing.T) {
	empty := &ToolSet{}
	if empty.Len() != 0 {
		t.Errorf("zero-value Len() = %d, want 0", empty.Len())
	}
	ts, _ := NewToolSet(newFake("a"), newFake("b"), newFake("c"))
	if ts.Len() != 3 {
		t.Errorf("Len() = %d, want 3", ts.Len())
	}
}

func TestToolSetNilSafe(t *testing.T) {
	// Nil *ToolSet should behave like an empty one — the Brain
	// holds a pointer that may be nil when WithTools wasn't called.
	var ts *ToolSet
	if _, ok := ts.Get("anything"); ok {
		t.Error("nil ToolSet Get returned ok=true")
	}
	if got := ts.Names(); got != nil {
		t.Errorf("nil ToolSet Names() = %v, want nil", got)
	}
	if got := ts.Len(); got != 0 {
		t.Errorf("nil ToolSet Len() = %d, want 0", got)
	}
	if got := ts.All(); got != nil {
		t.Errorf("nil ToolSet All() = %v, want nil", got)
	}
}

func TestWithToolsOption(t *testing.T) {
	b, err := New(WithTools(newFake("alpha"), newFake("bravo")))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.cfg.tools == nil {
		t.Fatal("cfg.tools = nil after WithTools")
	}
	if b.cfg.tools.Len() != 2 {
		t.Errorf("cfg.tools.Len() = %d, want 2", b.cfg.tools.Len())
	}
	if _, ok := b.cfg.tools.Get("alpha"); !ok {
		t.Error("tools.Get(alpha) missing after WithTools")
	}
}

func TestWithToolsRejectsBadName(t *testing.T) {
	_, err := New(WithTools(newFake("bad-name")))
	if err == nil {
		t.Fatal("New(WithTools(bad-name)) succeeded; expected validation error")
	}
}

func TestWithToolsEmptyDisables(t *testing.T) {
	b, err := New(WithTools())
	if err != nil {
		t.Fatalf("New(WithTools()) empty: %v", err)
	}
	if b.cfg.tools != nil {
		t.Errorf("cfg.tools = %v, want nil when WithTools called with no args", b.cfg.tools)
	}
}

func TestWithMaxToolHopsDefault(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.cfg.maxToolHops != defaultMaxToolHops {
		t.Errorf("default maxToolHops = %d, want %d", b.cfg.maxToolHops, defaultMaxToolHops)
	}
}

func TestWithMaxToolHopsOverride(t *testing.T) {
	b, err := New(WithMaxToolHops(3))
	if err != nil {
		t.Fatal(err)
	}
	if b.cfg.maxToolHops != 3 {
		t.Errorf("maxToolHops = %d, want 3", b.cfg.maxToolHops)
	}
	// Zero / negative means "use default".
	b2, _ := New(WithMaxToolHops(0))
	if b2.cfg.maxToolHops != defaultMaxToolHops {
		t.Errorf("WithMaxToolHops(0) kept maxToolHops = %d, want default", b2.cfg.maxToolHops)
	}
}

func TestWithMaxParallelToolsDefault(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b.cfg.maxParallelTools != defaultMaxParallelTools {
		t.Errorf("default maxParallelTools = %d, want %d",
			b.cfg.maxParallelTools, defaultMaxParallelTools)
	}
}

func TestWithMaxParallelToolsOverride(t *testing.T) {
	b, _ := New(WithMaxParallelTools(1))
	if b.cfg.maxParallelTools != 1 {
		t.Errorf("maxParallelTools = %d, want 1", b.cfg.maxParallelTools)
	}
}

func TestUnmarshalArgsWithToolCall(t *testing.T) {
	type Args struct {
		City string `json:"city"`
		N    int    `json:"n"`
	}
	tc := ToolCall{
		ID:        "call_1",
		Name:      "get_weather",
		Arguments: json.RawMessage(`{"city":"SF","n":3}`),
	}
	got, err := UnmarshalArgs[Args](tc)
	if err != nil {
		t.Fatalf("UnmarshalArgs: %v", err)
	}
	if got.City != "SF" || got.N != 3 {
		t.Errorf("UnmarshalArgs = %+v, want {SF 3}", got)
	}

	// Malformed JSON returns the zero value and an error.
	bad := ToolCall{Arguments: json.RawMessage(`{"city":`)}
	zero, err := UnmarshalArgs[Args](bad)
	if err == nil {
		t.Fatal("UnmarshalArgs on bad JSON returned nil error")
	}
	if zero != (Args{}) {
		t.Errorf("UnmarshalArgs returned non-zero %+v on error", zero)
	}
}

// compile-time assertion: the fakeTool is a Tool.
var _ Tool = (*fakeTool)(nil)

// keep errors referenced in this file so test discipline doesn't
// need a second import cycle later.
var _ = errors.New
