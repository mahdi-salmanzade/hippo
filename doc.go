// Package hippo is a Go LLM client with a memory.
//
// hippo wraps multiple LLM providers (Anthropic, OpenAI, Gemini, Ollama,
// OpenRouter) behind a single, idiomatic Go API and adds two primitives that
// most clients leave to the application layer:
//
//   - Persistent, typed memory (working / episodic / profile)
//   - A live pricing table with per-call budget enforcement and cost-aware
//     routing
//
// hippo is designed to compile to a single static binary (CGO_ENABLED=0) with
// a minimal dependency footprint. Providers, memory backends, and router
// implementations are interfaces, so you can swap in your own.
//
// The top-level type is Brain. Construct one with New and the functional
// options in options.go, then issue Calls against it:
//
//	b, err := hippo.New(
//	    hippo.WithProvider(anthropic.New(os.Getenv("ANTHROPIC_API_KEY"))),
//	    hippo.WithMemory(sqlite.Open("hippo.db")),
//	    hippo.WithBudget(budget.Daily(5.00)),
//	)
//	if err != nil { /* ... */ }
//	defer b.Close()
//
//	resp, err := b.Call(ctx, hippo.Call{
//	    Task:    hippo.TaskGenerate,
//	    Prompt:  "Summarise today's standup notes.",
//	    UseMemory: hippo.MemoryScopeRecent,
//	})
//
// See the examples/ directory for runnable end-to-end samples.
package hippo
