package hippo

import (
	"context"
	"time"
)

// TaskKind classifies the intent of a Call so the router can pick an
// appropriate provider/model. Task kinds are coarse on purpose: routing
// policy is expressed per-kind, not per-prompt.
type TaskKind string

const (
	// TaskClassify is a short, structured-output task (intent detection,
	// labelling, routing). Cheap, fast models preferred.
	TaskClassify TaskKind = "classify"
	// TaskReason is a multi-step reasoning task where quality matters
	// more than cost.
	TaskReason TaskKind = "reason"
	// TaskGenerate is freeform text generation (summaries, drafts,
	// explanations).
	TaskGenerate TaskKind = "generate"
	// TaskProtect signals that the input is sensitive and must route to
	// a provider whose Privacy tier matches.
	TaskProtect TaskKind = "protect"
	// TaskEmbed asks for an embedding vector, not a completion.
	TaskEmbed TaskKind = "embed"
)

// PrivacyTier describes where a Call is allowed to run.
//
// Providers also declare their own tier via Provider.Privacy; the router
// must not send a Call to a provider whose tier is weaker than the Call
// requires.
type PrivacyTier int

const (
	// PrivacyCloudOK allows any provider, including hosted APIs.
	PrivacyCloudOK PrivacyTier = iota
	// PrivacySensitiveRedact allows hosted providers but requires the
	// memory/router layer to redact PII before sending.
	PrivacySensitiveRedact
	// PrivacyLocalOnly restricts execution to local providers (Ollama,
	// future on-device models).
	PrivacyLocalOnly
)

// MemoryScope controls how much of the Brain's memory is attached to a Call
// before it is sent to the provider. The zero value is MemoryScopeNone.
type MemoryScope struct {
	// Mode is one of the MemoryScope* constants.
	Mode MemoryScopeMode
	// Tags constrains retrieval to records with any of these tags. Only
	// consulted when Mode == MemoryScopeByTags.
	Tags []string
}

// MemoryScopeMode enumerates the modes available to MemoryScope.
type MemoryScopeMode int

const (
	// MemoryScopeNone attaches no memory.
	MemoryScopeNone MemoryScopeMode = iota
	// MemoryScopeRecent attaches the most recent working-memory records.
	MemoryScopeRecent
	// MemoryScopeFull attaches all scopes subject to token budget.
	MemoryScopeFull
	// MemoryScopeByTags attaches records matching Scope.Tags.
	MemoryScopeByTags
)

// Call is the unit of work submitted to Brain.Call and Brain.Stream.
//
// Zero values are meaningful: an empty Call is a plain text generation
// request with no memory, no tools, and router-chosen model.
type Call struct {
	// Task classifies the intent; the router uses it to pick a provider.
	Task TaskKind
	// Privacy sets the minimum privacy tier required. Providers weaker
	// than this are excluded from routing.
	Privacy PrivacyTier
	// Prompt is a shorthand for a single user-role message. If Messages
	// is also set, Prompt is appended as the last user message.
	Prompt string
	// Messages is the full conversation transcript for multi-turn calls.
	Messages []Message
	// Tools are the tool/function definitions exposed to the model.
	Tools []Tool
	// Model optionally pins a specific model (e.g. "claude-sonnet-4-6").
	// When empty, the router decides.
	Model string
	// MaxTokens is the maximum number of output tokens to generate.
	MaxTokens int
	// MaxCostUSD is a hard ceiling for this single call. Exceeding this
	// estimate results in ErrBudgetExceeded before the call is made.
	MaxCostUSD float64
	// UseMemory controls how much memory is injected. Zero value =
	// MemoryScopeNone.
	UseMemory MemoryScope
	// Metadata is arbitrary per-call context for logging and routing
	// hooks. Not sent to the provider.
	Metadata map[string]any
}

// Message is a single turn in a conversation.
type Message struct {
	// Role is one of "system", "user", "assistant", "tool".
	Role string
	// Content is the textual body of the message.
	Content string
	// ToolCalls is set on assistant messages that invoked tools.
	ToolCalls []ToolCall
	// ToolCallID correlates a "tool" role message to the ToolCall it
	// answers.
	ToolCallID string
	// Name is the tool name for "tool" role messages.
	Name string
}

// Tool declares a callable function the model may invoke.
type Tool struct {
	// Name is a unique identifier for the tool.
	Name string
	// Description is a natural-language summary shown to the model.
	Description string
	// JSONSchema is the JSON Schema document describing the tool's input.
	// It is kept as raw bytes so providers can forward it verbatim.
	JSONSchema []byte
}

// ToolCall is a model-emitted request to invoke a Tool.
type ToolCall struct {
	// ID is the provider-assigned identifier, echoed back on the tool
	// response message.
	ID string
	// Name is the Tool.Name that was called.
	Name string
	// Arguments is the raw JSON arguments object.
	Arguments []byte
}

// StreamChunk is one incremental update from Brain.Stream.
//
// Exactly one of Delta, ToolCall, or Err will be set per chunk. Final is
// true on the terminal chunk, which also carries the authoritative Usage.
type StreamChunk struct {
	// Delta is appended text.
	Delta string
	// Thinking is appended extended-thinking text, when the provider
	// supports it.
	Thinking string
	// ToolCall is a completed tool call. Providers that stream partial
	// tool arguments accumulate them internally and emit on completion.
	ToolCall *ToolCall
	// Usage is populated on the terminal chunk.
	Usage *Usage
	// CostUSD is populated on the terminal chunk.
	CostUSD float64
	// Final is true exactly once per stream.
	Final bool
	// Err is set if the stream ended with an error.
	Err error
}

// Usage reports token accounting for a Call.
type Usage struct {
	// InputTokens is the prompt token count.
	InputTokens int
	// OutputTokens is the completion token count.
	OutputTokens int
	// CachedTokens is the portion of InputTokens that hit a provider
	// prompt cache (0 if the provider does not report this).
	CachedTokens int
}

// Response is the result of a non-streaming Brain.Call.
type Response struct {
	// Text is the assistant's textual reply.
	Text string
	// ToolCalls is non-empty if the model decided to call tools.
	ToolCalls []ToolCall
	// Thinking is the extended-thinking trace, if the provider supports
	// it and the caller requested it.
	Thinking string
	// Usage is the token accounting reported by the provider.
	Usage Usage
	// CostUSD is the computed cost using hippo's pricing table.
	CostUSD float64
	// Provider is the Provider.Name that served the call.
	Provider string
	// Model is the concrete model id used.
	Model string
	// LatencyMS is wall-clock time from Call invocation to response.
	LatencyMS int64
	// MemoryHits lists the IDs of memory records attached to this Call.
	MemoryHits []string
	// ReceivedAt is when the response was finalised locally.
	ReceivedAt time.Time
}

// ModelInfo describes one model offered by a Provider.
type ModelInfo struct {
	// ID is the provider-specific identifier passed on the wire.
	ID string
	// DisplayName is a human-readable label.
	DisplayName string
	// ContextTokens is the maximum context window in tokens.
	ContextTokens int
	// MaxOutputTokens is the hard cap on completion length.
	MaxOutputTokens int
	// SupportsTools is true if the model accepts tool definitions.
	SupportsTools bool
	// SupportsStreaming is true if the model can be streamed.
	SupportsStreaming bool
	// SupportsEmbeddings is true for embedding-only models.
	SupportsEmbeddings bool
}

// Provider is the contract every LLM backend must satisfy.
//
// The same interface is re-exported from the providers package as
// providers.Provider via a type alias so concrete provider packages have a
// natural name to implement without creating an import cycle back to hippo.
type Provider interface {
	// Name returns a short, stable identifier (e.g. "anthropic").
	Name() string
	// Models enumerates the models this provider exposes.
	Models() []ModelInfo
	// Privacy reports the strongest tier this provider can satisfy.
	Privacy() PrivacyTier
	// EstimateCost returns a pre-flight USD estimate for a Call. It must
	// be cheap: the router may call it many times across candidates.
	EstimateCost(c Call) (float64, error)
	// Call executes a Call synchronously and returns the full Response.
	Call(ctx context.Context, c Call) (*Response, error)
	// Stream executes a Call and emits StreamChunk values on the
	// returned channel. The channel is closed when the stream ends
	// (success or error).
	Stream(ctx context.Context, c Call) (<-chan StreamChunk, error)
}
