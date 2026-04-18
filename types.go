package hippo

import (
	"encoding/json"
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
	// MemoryScopeByTags attaches records matching MemoryScope.Tags.
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

// Tool is defined in tool.go.

// ToolCall is a model-emitted request to invoke a Tool.
type ToolCall struct {
	// ID is the provider-assigned identifier, echoed back on the tool
	// response message. Ollama, which does not assign its own IDs,
	// gets a synthetic "tool_<index>" id from the adapter.
	ID string
	// Name is the Tool.Name() that was called.
	Name string
	// Arguments is the raw JSON arguments object the model produced.
	// It conforms to the Tool's Schema() by the time the model emits
	// it (providers enforce schema-aware generation); hippo does not
	// re-validate before passing it to the Tool.
	Arguments json.RawMessage
}

// StreamChunkType discriminates StreamChunk variants. Each chunk sets
// exactly one variant, and the Type field tells the consumer which
// fields to look at. Two of the five types (Usage, Error) are terminal:
// the stream channel always closes immediately after one of them.
type StreamChunkType string

const (
	// StreamChunkText is an incremental text delta. Delta holds the
	// new text to append to any prior text the consumer has buffered.
	StreamChunkText StreamChunkType = "text"
	// StreamChunkThinking is an incremental reasoning/thinking delta
	// for providers that expose a reasoning trace (Anthropic extended
	// thinking, OpenAI reasoning summary). Delta holds the new text.
	StreamChunkThinking StreamChunkType = "thinking"
	// StreamChunkToolCall is a fully reassembled tool call. Providers
	// stream tool arguments as partial-JSON fragments; hippo buffers
	// them and emits one StreamChunkToolCall per call once the arguments
	// JSON is complete. Consumers never see partial tool arguments.
	StreamChunkToolCall StreamChunkType = "tool_call"
	// StreamChunkToolResult is emitted after hippo has executed a
	// tool in response to a StreamChunkToolCall. ToolCallID matches
	// the ToolCall.ID of the call that produced it; ToolResult
	// carries the executed result. For any given ToolCallID the
	// StreamChunkToolCall arrives strictly before its
	// StreamChunkToolResult — consumers can rely on this ordering
	// when threading a UI.
	//
	// Tool results are NOT fed back through the provider via the
	// stream — that happens internally before the next provider
	// turn begins. These chunks are purely for the consumer's
	// observability (showing "tool X returned Y" in a UI, logging
	// a trace, etc).
	StreamChunkToolResult StreamChunkType = "tool_result"
	// StreamChunkUsage is the terminal chunk on a successful stream.
	// Carries the authoritative Usage plus the provider, model, and
	// computed cost. The stream channel closes after this chunk is
	// delivered.
	StreamChunkUsage StreamChunkType = "usage"
	// StreamChunkError is the terminal chunk on a failed mid-stream.
	// Error carries the cause. The stream channel closes after this
	// chunk is delivered. Handshake failures surface as the error
	// return of Stream/Brain.Stream, not as a StreamChunkError.
	StreamChunkError StreamChunkType = "error"
)

// StreamChunk is one incremental update from Brain.Stream or a
// provider's Stream method. Type identifies the variant; only the
// fields documented for that variant are populated.
//
// Each stream emits zero or more non-terminal chunks (Text, Thinking,
// ToolCall) followed by exactly one terminal chunk (Usage on success,
// Error on failure). The channel closes after the terminal chunk.
// Consumers MUST fully drain the channel or cancel the stream's ctx
// to avoid leaking the provider-side reader goroutine.
type StreamChunk struct {
	// Type discriminates the chunk variant. Always set.
	Type StreamChunkType

	// Delta carries the incremental text for StreamChunkText and
	// StreamChunkThinking chunks. Deltas are not cumulative — consumers
	// concatenate them to reconstruct the full text.
	Delta string

	// ToolCall is set on StreamChunkToolCall chunks. One tool call per
	// chunk; parallel tool calls arrive as separate chunks.
	ToolCall *ToolCall

	// ToolResult is set on StreamChunkToolResult chunks. ToolCallID
	// is the ID of the StreamChunkToolCall that produced it, so
	// consumers can render "tool X (call id Y) returned Z" without
	// tracking positional order.
	ToolResult *ToolResult
	ToolCallID string

	// Usage, CostUSD, Provider, Model are populated on the terminal
	// StreamChunkUsage chunk. Usage is the provider-reported token
	// accounting; CostUSD is computed by the Brain (or by the provider
	// when called directly).
	Usage    *Usage
	CostUSD  float64
	Provider string
	Model    string

	// Error is set on the terminal StreamChunkError chunk.
	Error error
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
	// Err is non-nil when the Call completed but something
	// non-fatal happened worth reporting to the caller — most
	// commonly ErrMaxToolHopsExceeded, which means the response is
	// still usable but the tool-execution loop stopped before the
	// model was done. Distinct from the Call's own return error,
	// which represents a total failure (no usable Response at all).
	Err error
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

// MemoryKind classifies a Record's temporal role.
//
// hippo distinguishes three kinds of memory, modelled after the
// cognitive-science triplet:
//
//   - Working: short-lived context for the current task.
//   - Episodic: timestamped events the Brain has observed.
//   - Profile: long-lived facts about the user or environment.
type MemoryKind string

const (
	// MemoryWorking: short-lived per-session context.
	MemoryWorking MemoryKind = "working"
	// MemoryEpisodic: timestamped events.
	MemoryEpisodic MemoryKind = "episodic"
	// MemoryProfile: durable facts about the user/environment.
	MemoryProfile MemoryKind = "profile"
)

// Record is one entry in the memory store.
//
// Content is kept in its raw form; hippo deliberately does not summarise
// content before storage, to preserve fidelity for later retrieval. If
// summarisation is desired it should happen at Recall time or in the
// application layer.
type Record struct {
	// ID uniquely identifies the record. Empty on Add; the backend
	// assigns one.
	ID string
	// Kind is the temporal role of this record.
	Kind MemoryKind
	// Timestamp is when the event occurred (not when it was stored).
	Timestamp time.Time
	// Content is the raw text. Not pre-processed.
	Content string
	// Tags are arbitrary labels for filtering and retrieval.
	Tags []string
	// Importance is a 0..1 heuristic used to weight retrieval and to
	// exempt records from Prune when high enough. Backend-specific.
	Importance float64
	// Embedding is an optional vector representation of Content. Filled
	// lazily by backends that support embeddings. A non-nil Embedding
	// must match the backend's configured dimensionality.
	Embedding []float32
	// Source optionally identifies the origin of the record (e.g. a
	// Call ID, a conversation ID, a file path).
	Source string
}

// MemoryQuery is the retrieval-parameter shape for Memory.Recall.
//
// It is intentionally distinct from MemoryScope: MemoryScope is user
// intent declared on a Call ("attach recent memory"), MemoryQuery is
// the concrete filter passed to a backend ("records of these kinds,
// matching these tags, since this time, at most N, of at least this
// importance").
//
// The zero value selects the most-recent records across all kinds, up
// to a backend-chosen default limit.
type MemoryQuery struct {
	// Kinds restricts matches to these MemoryKinds. Empty means any.
	Kinds []MemoryKind
	// Tags requires a record to have at least one of these tags. Empty
	// means no tag filter.
	Tags []string
	// Since restricts matches to records on or after this timestamp.
	// Zero value means no lower bound.
	Since time.Time
	// Until restricts matches to records on or before this timestamp.
	// Zero value means no upper bound.
	Until time.Time
	// Limit caps the number of records returned. Zero uses the backend
	// default (typically 10).
	Limit int
	// MinImportance filters out records below this importance score.
	MinImportance float64
}

// Decision is the Router's response: which provider to call, which model
// to use, and how much it is expected to cost.
type Decision struct {
	// Provider is the Provider.Name to dispatch to.
	Provider string
	// Model is the concrete model id to pass to the provider.
	Model string
	// EstimatedCostUSD is the Router's pre-flight cost estimate.
	EstimatedCostUSD float64
	// Reason is a human-readable one-liner explaining the choice.
	Reason string
}
