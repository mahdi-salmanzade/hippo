package hippo

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"
)

// Brain is the top-level hippo entry point. A Brain bundles a set of
// Providers, an optional Memory, an optional BudgetTracker, and an
// optional Router into a single object against which you issue Calls.
//
// A Brain is safe for concurrent use by multiple goroutines. Close the
// Brain when you are done to release memory and provider resources.
type Brain struct {
	cfg    config
	logger *slog.Logger
}

// New constructs a Brain from the supplied options.
//
// New never blocks on network I/O; provider authentication is deferred
// until the first Call.
//
// No-router default. If WithRouter is not supplied, Brain uses the
// first registered provider for every Call (after the privacy check).
// Multiple providers without a router logs a warning but still picks
// the first. This is the "simplest working setup" tier:
//
//	// simplest: one provider, no router
//	b, _ := hippo.New(hippo.WithProvider(p))
//
//	// with routing: policy picks provider+model per Call.Task
//	b, _ := hippo.New(
//	    hippo.WithProvider(anthropic.New(...)),
//	    hippo.WithProvider(openai.New(...)),
//	    hippo.WithRouter(yaml.Load("")), // embedded default policy
//	)
//
// The no-router path exists so new users can start with a single
// WithProvider and get a working Brain immediately; it is not a
// shortcut for the full router flow. For anything beyond a single
// provider, pass a Router explicitly.
func New(opts ...Option) (*Brain, error) {
	c := config{
		logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		maxToolHops:      defaultMaxToolHops,
		maxParallelTools: defaultMaxParallelTools,
	}
	for _, opt := range opts {
		if err := opt(&c); err != nil {
			return nil, err
		}
	}
	if err := mergeMCPTools(&c); err != nil {
		return nil, err
	}
	return &Brain{cfg: c, logger: c.logger}, nil
}

// mergeMCPTools folds every WithMCPClients source's Tools() into
// c.tools. Runs after all options are applied so the final set is
// the union of WithTools + all MCP sources, and a single ToolSet
// constructor enforces name uniqueness across both.
//
// If no MCP sources were registered this is a no-op and c.tools
// passes through unchanged.
func mergeMCPTools(c *config) error {
	if len(c.mcpSources) == 0 {
		return nil
	}
	var tools []Tool
	if c.tools != nil {
		tools = append(tools, c.tools.All()...)
	}
	for _, src := range c.mcpSources {
		tools = append(tools, src.Tools()...)
	}
	if len(tools) == 0 {
		return nil
	}
	ts, err := NewToolSet(tools...)
	if err != nil {
		return fmt.Errorf("hippo: merge MCP tools: %w", err)
	}
	c.tools = ts
	return nil
}

// Embedder returns the Embedder attached via WithEmbedder, or nil if
// the Brain was constructed without semantic-memory support.
func (b *Brain) Embedder() Embedder { return b.cfg.embedder }

// Call dispatches c to a provider and returns the response.
//
// Flow:
//
//  1. Route + budget-gate + memory-hydrate (same as before tools).
//  2. Attach the registered ToolSet to the outgoing Call as
//     c.Tools so the provider can advertise them to the model.
//  3. Tool-execution loop: dispatch to the provider; if the
//     response has no tool calls, return it. Otherwise, execute
//     the tools in parallel (bounded by WithMaxParallelTools),
//     append the assistant turn + tool-result messages, and go
//     around again. Cap the loop at WithMaxToolHops (default 10).
//  4. Aggregate usage + cost across all turns, charge the budget
//     once, record one episode.
//
// When the hop cap is hit, Call returns with the final turn's
// response and Response.Err = ErrMaxToolHopsExceeded - the response
// is still usable; callers can inspect ToolCalls and continue
// manually or prompt the model differently.
func (b *Brain) Call(ctx context.Context, c Call) (*Response, error) {
	if len(b.cfg.providers) == 0 {
		return nil, ErrNoProviderAvailable
	}

	decision, err := b.decide(ctx, c)
	if err != nil {
		return nil, err
	}

	p := providerByName(b.cfg.providers, decision.Provider)
	if p == nil {
		return nil, fmt.Errorf("hippo: router picked unregistered provider %q", decision.Provider)
	}

	if b.cfg.budget != nil {
		remaining := b.cfg.budget.Remaining()
		if decision.EstimatedCostUSD > remaining {
			return nil, fmt.Errorf("%w: estimate $%.6f > remaining $%.6f",
				ErrBudgetExceeded, decision.EstimatedCostUSD, remaining)
		}
	}
	if c.MaxCostUSD > 0 && decision.EstimatedCostUSD > c.MaxCostUSD {
		return nil, fmt.Errorf("%w: estimate $%.6f > Call.MaxCostUSD $%.6f",
			ErrBudgetExceeded, decision.EstimatedCostUSD, c.MaxCostUSD)
	}

	memoryHits := b.hydrateMemory(ctx, c)
	enriched := injectMemory(c, memoryHits)
	if decision.Model != "" {
		enriched.Model = decision.Model
	}
	enriched.Tools = b.toolsForCall(c)

	// Initial message set: memory-injected system + user/assistant
	// content + the Prompt appended as the last user turn. The
	// provider's buildRequestBody would do the same flattening, but
	// we need the messages as hippo.Message values so the loop can
	// append assistant + tool-result turns between provider calls.
	messages := messagesFromCall(enriched)

	var totalUsage Usage
	var totalCost float64
	finalResp := &Response{}
	hops := 0

	for {
		turnCall := enriched
		turnCall.Messages = messages
		turnCall.Prompt = ""

		resp, err := p.Call(ctx, turnCall)
		if err != nil {
			return nil, err
		}

		totalUsage = addUsage(totalUsage, resp.Usage)
		totalCost += resp.CostUSD
		finalResp = resp

		if len(resp.ToolCalls) == 0 {
			break
		}

		hops++
		if hops > b.cfg.maxToolHops {
			finalResp.Err = ErrMaxToolHopsExceeded
			break
		}

		outcomes := b.executeToolsParallel(ctx, resp.ToolCalls)

		messages = append(messages, Message{
			Role:      "assistant",
			Content:   resp.Text,
			ToolCalls: resp.ToolCalls,
		})
		for _, o := range outcomes {
			messages = append(messages, Message{
				Role:       "tool",
				ToolCallID: o.CallID,
				Name:       o.ToolName,
				Content:    o.Result.Content,
			})
		}
	}

	finalResp.Usage = totalUsage
	finalResp.CostUSD = totalCost
	if finalResp.Provider == "" {
		finalResp.Provider = decision.Provider
	}
	if finalResp.Model == "" {
		finalResp.Model = decision.Model
	}
	finalResp.MemoryHits = recordIDs(memoryHits)

	if b.cfg.budget != nil {
		if err := b.cfg.budget.Charge(finalResp.Provider, finalResp.Model, totalUsage); err != nil {
			b.logger.Warn("hippo: budget charge failed", "err", err,
				"provider", finalResp.Provider, "model", finalResp.Model)
		}
	}

	if b.cfg.memory != nil {
		go b.recordEpisode(c, finalResp)
	}

	return finalResp, nil
}

// toolsForCall returns the tool set that should be advertised to the
// provider for this Call. Explicit Call.Tools wins over the Brain's
// registered ToolSet; otherwise, the Brain's tools are used.
//
// The distinction matters: a caller who passes their own Tools on a
// single Call expects those to be the full set (no implicit
// augmentation with Brain-wide tools), whereas a caller who passes
// nothing expects the Brain's registered tools.
func (b *Brain) toolsForCall(c Call) []Tool {
	if len(c.Tools) > 0 {
		return c.Tools
	}
	if b.cfg.tools == nil {
		return nil
	}
	return b.cfg.tools.All()
}

// messagesFromCall flattens the initial (Messages, Prompt) pair into
// a single []Message. The tool loop operates on this slice, appending
// further turns as it goes.
func messagesFromCall(c Call) []Message {
	msgs := make([]Message, 0, len(c.Messages)+1)
	msgs = append(msgs, c.Messages...)
	if c.Prompt != "" {
		msgs = append(msgs, Message{Role: "user", Content: c.Prompt})
	}
	return msgs
}

// addUsage sums two Usage values. Used to aggregate counts across
// tool-loop turns so Brain charges the budget once with the total
// and exposes the total on the final Response.
func addUsage(a, b Usage) Usage {
	return Usage{
		InputTokens:  a.InputTokens + b.InputTokens,
		OutputTokens: a.OutputTokens + b.OutputTokens,
		CachedTokens: a.CachedTokens + b.CachedTokens,
	}
}

// toolOutcome is the result of one tool execution during the loop.
// CallID and ToolName correlate the outcome back to the ToolCall
// that produced it so the feedback message has the right id/name
// fields (providers use these differently - Anthropic tool_use_id,
// OpenAI call_id, Ollama tool_call_id).
type toolOutcome struct {
	CallID   string
	ToolName string
	Result   ToolResult
}

// executeToolsParallel runs a batch of ToolCalls concurrently,
// bounded by WithMaxParallelTools, and returns the outcomes in the
// SAME ORDER as the input calls (not completion order). Order
// preservation is load-bearing: providers correlate tool results by
// position on some models and by id on others, so consistent
// positional order avoids a whole category of subtle bugs.
//
// Panics inside Tool.Execute are recovered and surfaced as an
// IsError:true result containing the panic value. Non-nil errors
// from Execute are likewise converted to IsError:true so the LLM
// sees the failure and can correct course. Unknown tool names
// become IsError results with "tool not found". Context
// cancellation during execution produces IsError results with
// "cancelled" - the loop itself checks ctx.Err afterwards.
func (b *Brain) executeToolsParallel(ctx context.Context, calls []ToolCall) []toolOutcome {
	outcomes := make([]toolOutcome, len(calls))
	if len(calls) == 0 {
		return outcomes
	}

	concurrency := b.cfg.maxParallelTools
	if concurrency <= 0 {
		concurrency = defaultMaxParallelTools
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for i := range calls {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				outcomes[i] = toolOutcome{
					CallID:   calls[i].ID,
					ToolName: calls[i].Name,
					Result:   ToolResult{Content: "tool execution cancelled", IsError: true},
				}
				return
			}
			outcomes[i] = b.executeOneTool(ctx, calls[i])
		}()
	}
	wg.Wait()
	return outcomes
}

// executeOneTool looks up the tool in the registry and runs it with
// panic recovery and error conversion. Never returns a raw error -
// all failure modes collapse into ToolResult{IsError: true, Content: <msg>}
// so the loop can feed them back to the model uniformly.
func (b *Brain) executeOneTool(ctx context.Context, call ToolCall) (out toolOutcome) {
	out.CallID = call.ID
	out.ToolName = call.Name

	defer func() {
		if r := recover(); r != nil {
			out.Result = ToolResult{
				Content: fmt.Sprintf("tool %q panicked: %v", call.Name, r),
				IsError: true,
			}
			b.logger.Warn("hippo: tool panicked",
				"tool", call.Name, "panic", r)
		}
	}()

	if b.cfg.tools == nil {
		out.Result = ToolResult{
			Content: fmt.Sprintf("%s: no tools registered on this Brain", ErrToolNotFound),
			IsError: true,
		}
		return out
	}
	tool, ok := b.cfg.tools.Get(call.Name)
	if !ok {
		out.Result = ToolResult{
			Content: fmt.Sprintf("%s: %q", ErrToolNotFound, call.Name),
			IsError: true,
		}
		return out
	}

	result, err := tool.Execute(ctx, call.Arguments)
	if err != nil {
		out.Result = ToolResult{
			Content: fmt.Sprintf("tool %q returned error: %v", call.Name, err),
			IsError: true,
		}
		return out
	}
	out.Result = result
	return out
}

// decide resolves the Call to a Decision. With a Router configured,
// it delegates; without one, it falls back to the Pass 1 behaviour
// (first registered provider, with a privacy check). The no-router
// fallback exists so hippo.New() with zero options stays usable.
func (b *Brain) decide(ctx context.Context, c Call) (Decision, error) {
	if b.cfg.router != nil {
		budget := math.Inf(1)
		if b.cfg.budget != nil {
			budget = b.cfg.budget.Remaining()
		}
		d, err := b.cfg.router.Route(ctx, c, b.cfg.providers, budget)
		if err != nil {
			return Decision{}, fmt.Errorf("hippo: route: %w", err)
		}
		return d, nil
	}

	// No router - legacy "first provider" dispatch.
	if len(b.cfg.providers) > 1 {
		b.logger.Warn("hippo: multiple providers + no router; using first (register WithRouter to route)",
			"count", len(b.cfg.providers),
			"chosen", b.cfg.providers[0].Name(),
		)
	}
	p := b.cfg.providers[0]
	if p.Privacy() < c.Privacy {
		return Decision{}, ErrPrivacyViolation
	}
	return Decision{Provider: p.Name()}, nil
}

// hydrateMemory recalls memory records for c and returns them, or nil
// if memory is not configured, the call opted out, or Recall failed.
// A failed recall is logged at Warn - memory is a helper, not a hard
// dependency - and an empty slice is returned. Call and Stream both
// use this so the hydration behaviour stays identical across paths.
func (b *Brain) hydrateMemory(ctx context.Context, c Call) []Record {
	if c.UseMemory.Mode == MemoryScopeNone || b.cfg.memory == nil {
		return nil
	}
	query, q := memoryQueryFromScope(c.UseMemory, c.Prompt)
	hits, err := b.cfg.memory.Recall(ctx, query, q)
	if err != nil {
		b.logger.Warn("hippo: memory recall failed (serving without memory)", "err", err)
		return nil
	}
	return hits
}

// providerByName returns the registered provider with the given
// Name, or nil if absent. Linear scan: provider lists are small
// (typically < 10 entries).
func providerByName(providers []Provider, name string) Provider {
	for _, p := range providers {
		if p.Name() == name {
			return p
		}
	}
	return nil
}

// Stream is the streaming counterpart to Call. It runs the same
// routing, budget-gate, and memory-hydration synchronously before
// opening the provider stream; on success it returns a receive-only
// channel of StreamChunk values that spans the entire tool-execution
// loop, not a single provider turn.
//
// Semantics:
//   - Errors during route / budget / hydration return (nil, err); no
//     channel is opened.
//   - On success, the channel emits:
//   - StreamChunkText / StreamChunkThinking / StreamChunkToolCall
//     forwarded from each provider turn.
//   - StreamChunkToolResult after hippo executes a tool
//     (preserves ordering: for any ToolCallID, the ToolCall
//     chunk arrives strictly before the ToolResult chunk).
//   - Exactly one terminal StreamChunkUsage with usage+cost
//     aggregated across every turn.
//   - On mid-stream failure, one terminal StreamChunkError
//     replaces the usage chunk.
//   - Per-turn provider Usage chunks are NOT forwarded - they fold
//     into the final aggregated total so the consumer sees the
//     stream as one continuous conversation, not N concatenated turns.
//   - Callers MUST fully drain the channel or cancel ctx to avoid
//     leaking the provider-side reader goroutine.
func (b *Brain) Stream(ctx context.Context, c Call) (<-chan StreamChunk, error) {
	if len(b.cfg.providers) == 0 {
		return nil, ErrNoProviderAvailable
	}

	decision, err := b.decide(ctx, c)
	if err != nil {
		return nil, err
	}

	p := providerByName(b.cfg.providers, decision.Provider)
	if p == nil {
		return nil, fmt.Errorf("hippo: router picked unregistered provider %q", decision.Provider)
	}

	if b.cfg.budget != nil {
		remaining := b.cfg.budget.Remaining()
		if decision.EstimatedCostUSD > remaining {
			return nil, fmt.Errorf("%w: estimate $%.6f > remaining $%.6f",
				ErrBudgetExceeded, decision.EstimatedCostUSD, remaining)
		}
	}
	if c.MaxCostUSD > 0 && decision.EstimatedCostUSD > c.MaxCostUSD {
		return nil, fmt.Errorf("%w: estimate $%.6f > Call.MaxCostUSD $%.6f",
			ErrBudgetExceeded, decision.EstimatedCostUSD, c.MaxCostUSD)
	}

	memoryHits := b.hydrateMemory(ctx, c)

	// Open the first provider stream synchronously so handshake
	// failures (bad credentials, unreachable server) surface as the
	// error return of Brain.Stream - with no channel for the caller
	// to drain. Subsequent-turn handshake errors inside the tool
	// loop become terminal StreamChunkError chunks instead.
	enriched := injectMemory(c, memoryHits)
	if decision.Model != "" {
		enriched.Model = decision.Model
	}
	enriched.Tools = b.toolsForCall(c)
	firstMessages := messagesFromCall(enriched)
	firstCall := enriched
	firstCall.Messages = firstMessages
	firstCall.Prompt = ""

	firstCh, err := p.Stream(ctx, firstCall)
	if err != nil {
		return nil, err
	}

	out := make(chan StreamChunk, 16)
	go b.streamWithTools(ctx, c, decision, p, memoryHits, enriched, firstMessages, firstCh, out)
	return out, nil
}

// streamWithTools is the multi-turn streaming loop. It calls
// provider.Stream once per turn, forwards non-terminal chunks,
// accumulates the turn's tool calls and usage, then - if the model
// wanted tools - executes them, emits StreamChunkToolResult chunks,
// and loops. The caller-visible channel stays open across turns so
// the stream reads as one continuous event sequence.
//
// The first turn's provider channel is handed in from Stream() so
// first-turn handshake errors can surface to the caller as the
// error return of Brain.Stream before the goroutine starts. Later
// turns' handshake errors become terminal StreamChunkError chunks.
func (b *Brain) streamWithTools(
	ctx context.Context,
	original Call,
	decision Decision,
	p Provider,
	memoryHits []Record,
	enriched Call,
	messages []Message,
	firstCh <-chan StreamChunk,
	out chan<- StreamChunk,
) {
	defer close(out)

	var totalUsage Usage
	var totalCost float64
	var fullText, fullThinking strings.Builder
	var allToolCalls []ToolCall
	hops := 0
	// echoedModel tracks the actual model id the provider reports on
	// its per-turn Usage chunks, so the terminal aggregated chunk
	// carries the dated variant (e.g. "claude-haiku-4-5-20250930")
	// rather than the caller's alias - and so a no-router Brain
	// whose decision.Model is empty still gets a usable Model on the
	// final chunk.
	echoedModel := decision.Model
	echoedProvider := decision.Provider

	emit := func(chunk StreamChunk) bool {
		select {
		case out <- chunk:
			return true
		case <-ctx.Done():
			return false
		}
	}

	providerCh := firstCh
	for {
		if ctx.Err() != nil {
			return
		}
		if providerCh == nil {
			turnCall := enriched
			turnCall.Messages = messages
			turnCall.Prompt = ""
			ch, err := p.Stream(ctx, turnCall)
			if err != nil {
				emit(StreamChunk{Type: StreamChunkError, Error: err})
				return
			}
			providerCh = ch
		}

		var turnToolCalls []ToolCall
		var turnUsage *Usage
		var turnText strings.Builder
		streamErrored := false

	turnLoop:
		for chunk := range providerCh {
			if ctx.Err() != nil {
				return
			}
			switch chunk.Type {
			case StreamChunkText:
				turnText.WriteString(chunk.Delta)
				fullText.WriteString(chunk.Delta)
				if !emit(chunk) {
					return
				}
			case StreamChunkThinking:
				fullThinking.WriteString(chunk.Delta)
				if !emit(chunk) {
					return
				}
			case StreamChunkToolCall:
				if chunk.ToolCall != nil {
					turnToolCalls = append(turnToolCalls, *chunk.ToolCall)
					allToolCalls = append(allToolCalls, *chunk.ToolCall)
				}
				if !emit(chunk) {
					return
				}
			case StreamChunkUsage:
				// Buffer - one aggregated usage chunk lands at the
				// very end, not per turn. Capture the model/provider
				// the adapter stamped (more specific than the router's
				// decision - the decision's Model can be empty for
				// no-router Brains, and is always the alias rather
				// than the dated variant the API echoed back). Also
				// accumulate CostUSD so a Brain with no budget tracker
				// still reports a real cost on the terminal chunk
				// (budget.EstimateCost requires the tracker; the
				// provider's pre-computed CostUSD does not).
				u := chunk.Usage
				turnUsage = u
				totalCost += chunk.CostUSD
				if chunk.Model != "" {
					echoedModel = chunk.Model
				}
				if chunk.Provider != "" {
					echoedProvider = chunk.Provider
				}
			case StreamChunkError:
				if !emit(chunk) {
					return
				}
				streamErrored = true
				break turnLoop
			}
		}

		if streamErrored {
			return
		}

		if turnUsage != nil {
			totalUsage = addUsage(totalUsage, *turnUsage)
		}

		if len(turnToolCalls) == 0 {
			b.emitTerminalStreamUsage(ctx, original, echoedProvider, echoedModel, memoryHits, out,
				totalUsage, &totalCost, fullText.String(), fullThinking.String(), allToolCalls,
				nil)
			return
		}

		hops++
		if hops > b.cfg.maxToolHops {
			b.emitTerminalStreamUsage(ctx, original, echoedProvider, echoedModel, memoryHits, out,
				totalUsage, &totalCost, fullText.String(), fullThinking.String(), allToolCalls,
				ErrMaxToolHopsExceeded)
			return
		}

		outcomes := b.executeToolsParallel(ctx, turnToolCalls)

		for i := range outcomes {
			o := outcomes[i]
			result := o.Result
			if !emit(StreamChunk{
				Type:       StreamChunkToolResult,
				ToolCallID: o.CallID,
				ToolResult: &result,
			}) {
				return
			}
		}

		messages = append(messages, Message{
			Role:      "assistant",
			Content:   turnText.String(),
			ToolCalls: turnToolCalls,
		})
		for _, o := range outcomes {
			messages = append(messages, Message{
				Role:       "tool",
				ToolCallID: o.CallID,
				Name:       o.ToolName,
				Content:    o.Result.Content,
			})
		}

		// Next iteration opens a fresh provider stream.
		providerCh = nil
	}
}

// emitTerminalStreamUsage wraps up a streaming Call: charges budget,
// emits the aggregated StreamChunkUsage (and an error chunk if the
// stream ended on a hop cap), and queues the episode record.
//
// providerName / modelName are the echoed values captured from the
// provider's per-turn Usage chunks - NOT decision.Provider/.Model
// directly - so (a) a no-router Brain whose decision.Model is empty
// still gets a populated Model on the terminal chunk, and (b) the
// dated variant the API echoed back ("claude-haiku-4-5-20250930")
// flows through rather than the caller's alias. finalErr non-nil
// means hop cap hit - emit the usage first so the consumer has the
// aggregated state, then the error.
func (b *Brain) emitTerminalStreamUsage(
	ctx context.Context,
	original Call,
	providerName, modelName string,
	memoryHits []Record,
	out chan<- StreamChunk,
	totalUsage Usage,
	totalCost *float64,
	fullText, fullThinking string,
	allToolCalls []ToolCall,
	finalErr error,
) {
	cost := *totalCost
	if b.cfg.budget != nil {
		if est, err := b.cfg.budget.EstimateCost(providerName, modelName, totalUsage); err == nil {
			cost = est
		}
		if err := b.cfg.budget.Charge(providerName, modelName, totalUsage); err != nil {
			b.logger.Warn("hippo: stream budget charge failed", "err", err,
				"provider", providerName, "model", modelName)
		}
	}

	usage := totalUsage
	select {
	case out <- StreamChunk{
		Type:     StreamChunkUsage,
		Usage:    &usage,
		CostUSD:  cost,
		Provider: providerName,
		Model:    modelName,
	}:
	case <-ctx.Done():
		return
	}

	if b.cfg.memory != nil {
		resp := &Response{
			Text:       fullText,
			Thinking:   fullThinking,
			ToolCalls:  allToolCalls,
			Usage:      totalUsage,
			CostUSD:    cost,
			Provider:   providerName,
			Model:      modelName,
			MemoryHits: recordIDs(memoryHits),
		}
		go b.recordEpisode(original, resp)
	}

	if finalErr != nil {
		select {
		case out <- StreamChunk{Type: StreamChunkError, Error: finalErr}:
		case <-ctx.Done():
		}
	}
}

// memoryQueryFromScope translates a user-intent MemoryScope (what a
// Call asks for) into a concrete backend MemoryQuery (retrieval
// parameters) plus the full-text query string Recall should use.
//
// Mode semantics:
//   - Recent: "what happened lately" - empty query string, time
//     window, ordered by recency.
//   - ByTags: "records with these tags" - empty query string, tag
//     filter, ordered by recency.
//   - Full: "search everything relevant to the prompt" - prompt
//     feeds FTS5, no time filter, broader limit.
//
// The prompt is only used when Full mode asks for content matching.
// For Recent and ByTags, passing the prompt to Recall would turn a
// recency/tag lookup into an AND of every prompt token against
// record contents, which almost never matches.
func memoryQueryFromScope(s MemoryScope, prompt string) (string, MemoryQuery) {
	switch s.Mode {
	case MemoryScopeRecent:
		return "", MemoryQuery{
			Since: time.Now().Add(-24 * time.Hour),
			Limit: 5,
		}
	case MemoryScopeByTags:
		return "", MemoryQuery{
			Tags:  s.Tags,
			Limit: 10,
		}
	case MemoryScopeFull:
		return prompt, MemoryQuery{Limit: 20}
	default: // MemoryScopeNone - caller shouldn't reach here, but be safe.
		return "", MemoryQuery{}
	}
}

// injectMemory prepends recalled records to the outgoing Call as a
// system-role Message so providers that have a dedicated system
// field (Anthropic, OpenAI) fold it into the right place and
// providers that don't still see it first in the message list.
func injectMemory(c Call, records []Record) Call {
	if len(records) == 0 {
		return c
	}
	var b strings.Builder
	b.WriteString("[Relevant context from memory]\n")
	for _, r := range records {
		b.WriteString("- ")
		b.WriteString(r.Timestamp.Format(time.RFC3339))
		b.WriteString(": ")
		b.WriteString(r.Content)
		b.WriteByte('\n')
	}
	sysMsg := Message{Role: "system", Content: b.String()}
	c.Messages = append([]Message{sysMsg}, c.Messages...)
	return c
}

// recordEpisode stores an Episodic record summarising this Call.
// Summaries keep content raw (no LLM-summarisation) - the
// MemMachine-inspired "raw is queen" policy from Pass 2 applies
// here too. Response.Text is truncated to 500 chars for the trace
// body, which is enough to re-recognise the interaction later
// without blowing up the DB on long completions.
func (b *Brain) recordEpisode(c Call, resp *Response) {
	var content strings.Builder
	content.WriteString("prompt: ")
	content.WriteString(c.Prompt)
	content.WriteString("\n→ response: ")
	if len(resp.Text) > 500 {
		content.WriteString(resp.Text[:500])
		content.WriteString("…")
	} else {
		content.WriteString(resp.Text)
	}

	var tags []string
	if c.Task != "" {
		tags = append(tags, "task:"+string(c.Task))
	}
	if resp.Provider != "" {
		tags = append(tags, "provider:"+resp.Provider)
	}

	rec := &Record{
		Kind:       MemoryEpisodic,
		Timestamp:  time.Now(),
		Content:    content.String(),
		Tags:       tags,
		Importance: 0.5,
		Source:     "brain.Call",
	}
	if err := b.cfg.memory.Add(context.Background(), rec); err != nil {
		b.logger.Warn("hippo: episode record failed", "err", err)
	}
}

// recordIDs extracts the ID of every record in the slice.
func recordIDs(records []Record) []string {
	if len(records) == 0 {
		return nil
	}
	out := make([]string, len(records))
	for i, r := range records {
		out[i] = r.ID
	}
	return out
}

// Close releases all resources held by the Brain, including the Memory
// backend (if any). Safe to call more than once.
func (b *Brain) Close() error {
	// TODO: close memory, flush budget tracker, close provider
	// resources. Aggregate errors.
	if b.cfg.memory != nil {
		return b.cfg.memory.Close()
	}
	return nil
}
