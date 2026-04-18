package hippo

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
)

// Tool is a capability the LLM can invoke. hippo provides the
// contract; consumers provide the implementation. Tools are
// registered at Brain construction via WithTools and are immutable
// for the Brain's lifetime — there is no registry mutation API on
// purpose, so the set of capabilities a Brain exposes is a
// declarative part of its configuration rather than runtime state.
//
// # Thread-safety
//
// Execute MAY be called concurrently for the same tool across
// different Calls and within a single Call (parallel tool use).
// Implementations must be safe for concurrent use. The Brain
// serialises neither tool construction nor tool execution.
//
// # Naming
//
// Tool names must match [a-zA-Z_][a-zA-Z0-9_]{0,63}. Providers
// enforce similar restrictions (Anthropic in particular rejects
// dashes and most punctuation); NewToolSet validates at
// registration time rather than letting a bad name crash at call
// time.
type Tool interface {
	// Name is the unique identifier the LLM uses to invoke this
	// tool. Must match the pattern above.
	Name() string

	// Description is shown to the LLM. It should explain what the
	// tool does, when to use it, and any important caveats — treat
	// this as prompt-engineering surface, it materially affects how
	// often and how correctly the tool is called.
	Description() string

	// Schema is a JSON Schema (draft-07 subset) describing the
	// tool's arguments as a JSON object. Providers translate this
	// to their native parameter format; returning invalid JSON here
	// will surface as a provider-side validation error on the first
	// Call that exposes the tool.
	Schema() json.RawMessage

	// Execute runs the tool. args is the JSON object the model
	// produced, conforming to Schema() (providers validate the
	// shape; hippo does not re-validate before calling Execute).
	//
	// Return (ToolResult{Content, IsError: false}, nil) for normal
	// success. For expected failures — "file not found", "API rate
	// limited", etc. — return (ToolResult{Content: <message>,
	// IsError: true}, nil) so the LLM sees the failure and can
	// decide how to recover.
	//
	// Return a non-nil error only for unexpected failures (panics,
	// infrastructure issues). hippo recovers panics and converts
	// returned errors into IsError: true results that are fed to
	// the LLM; the error itself is surfaced to the caller only when
	// max tool hops is hit with nothing but consecutive errors.
	Execute(ctx context.Context, args json.RawMessage) (ToolResult, error)
}

// ToolResult is the value a tool returns after execution. hippo
// threads the Content string back to the LLM as the assistant's
// "tool" message; IsError is transcribed into the provider's
// native "tool failed" flag (Anthropic's is_error, OpenAI's
// implicit-on-non-2xx for function_call_output, etc.).
type ToolResult struct {
	// Content is the tool's output, shown to the LLM verbatim. Plain
	// text or JSON-as-string both work; providers render them
	// identically. Keep it short when you can — every byte is an
	// input token on the next provider turn.
	Content string

	// IsError signals a tool-level failure to the LLM. Distinct from
	// an Execute returning a non-nil error: IsError is an expected
	// failure (the tool ran to completion, the answer is "this
	// operation cannot succeed"); a returned error is an unexpected
	// failure (the tool couldn't run at all).
	IsError bool

	// Meta is caller metadata, not sent to the LLM. Use it for
	// tracing, logging, or side-channel information a Brain
	// consumer wants to correlate with other observability. hippo
	// passes Meta through untouched.
	Meta map[string]any
}

// ToolSet is hippo's internal tool registry, populated once at
// startup via WithTools. It is exported as a concrete type (rather
// than an interface) because hippo owns the shape — there is no
// good reason to plug in a different registry, and exposing only
// read methods keeps the "registered at New, immutable thereafter"
// contract visible at the type level.
type ToolSet struct {
	tools map[string]Tool
}

// nameRE is the validation pattern for Tool.Name(). Mirrors what
// every provider documents plus a conservative 64-character cap.
// Providers accept slightly different character sets (Anthropic:
// [a-zA-Z0-9_-], OpenAI: [a-zA-Z0-9_-], Ollama: permissive), so the
// hippo constraint is the strict intersection.
var nameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,63}$`)

// NewToolSet constructs a ToolSet from a slice of tools. Returns an
// error if any tool has an invalid name or if two tools share a
// name — both are configuration bugs we'd rather catch at startup
// than at the first Call that tries to dispatch them.
func NewToolSet(tools ...Tool) (*ToolSet, error) {
	ts := &ToolSet{tools: make(map[string]Tool, len(tools))}
	for _, t := range tools {
		if t == nil {
			return nil, fmt.Errorf("hippo: NewToolSet: nil tool")
		}
		name := t.Name()
		if !nameRE.MatchString(name) {
			return nil, fmt.Errorf("hippo: NewToolSet: invalid tool name %q (must match %s)",
				name, nameRE.String())
		}
		if _, dup := ts.tools[name]; dup {
			return nil, fmt.Errorf("hippo: NewToolSet: duplicate tool name %q", name)
		}
		ts.tools[name] = t
	}
	return ts, nil
}

// Get returns the tool registered under name, or (nil, false) when
// the name is unknown. The lookup is safe for concurrent use; the
// map is never written after NewToolSet returns.
func (ts *ToolSet) Get(name string) (Tool, bool) {
	if ts == nil {
		return nil, false
	}
	t, ok := ts.tools[name]
	return t, ok
}

// Names returns the registered tool names, sorted alphabetically
// so callers that emit them in logs / docs get deterministic order.
func (ts *ToolSet) Names() []string {
	if ts == nil {
		return nil
	}
	out := make([]string, 0, len(ts.tools))
	for k := range ts.tools {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Len returns the number of registered tools. Safe for concurrent
// use.
func (ts *ToolSet) Len() int {
	if ts == nil {
		return 0
	}
	return len(ts.tools)
}

// All returns the registered tools as a slice. Order matches
// Names(). Useful for providers that need to iterate the full set
// when translating to a native tool schema.
func (ts *ToolSet) All() []Tool {
	if ts == nil {
		return nil
	}
	names := ts.Names()
	out := make([]Tool, 0, len(names))
	for _, n := range names {
		out = append(out, ts.tools[n])
	}
	return out
}
