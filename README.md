# hippo

**A Go LLM client with a memory.**

🚧 **Alpha — building in public.** APIs will change without notice until v0.1.

hippo is a pure-Go library for talking to LLMs. It has three properties that
set it apart from other Go LLM clients:

1. **Unified providers.** One API over Anthropic, OpenAI, Gemini, Ollama, and
   OpenRouter — plus a `Provider` interface if you want to plug in your own.
2. **Memory-aware.** Persistent typed memory (working / episodic / profile) is
   a first-class primitive, not something you bolt on top.
3. **Cost-aware.** A live pricing table, per-call budget enforcement, and a
   router that picks the cheapest model able to satisfy your privacy and
   quality constraints.

All of it compiles to a single static binary (`CGO_ENABLED=0`), with a
minimal dependency tree.

## Install

```bash
go get github.com/mahdi-salmanzade/hippo
```

Requires Go 1.23 or newer.

## Quick start

```go
package main

import (
    "context"
    "fmt"
    "os"

    "github.com/mahdi-salmanzade/hippo"
    "github.com/mahdi-salmanzade/hippo/budget"
    "github.com/mahdi-salmanzade/hippo/memory/sqlite"
    "github.com/mahdi-salmanzade/hippo/providers/anthropic"
)

func main() {
    store, _ := sqlite.Open("hippo.db")

    b, err := hippo.New(
        hippo.WithProvider(anthropic.New(os.Getenv("ANTHROPIC_API_KEY"))),
        hippo.WithMemory(store),
        hippo.WithBudget(budget.New(budget.WithCeiling(5.00))),
    )
    if err != nil { panic(err) }
    defer b.Close()

    resp, err := b.Call(context.Background(), hippo.Call{
        Task:      hippo.TaskGenerate,
        Prompt:    "What did we discuss about rate limiting yesterday?",
        UseMemory: hippo.MemoryScope{Mode: hippo.MemoryScopeRecent},
    })
    if err != nil { panic(err) }

    fmt.Println(resp.Text)
    fmt.Printf("cost: $%.4f  |  memory hits: %d\n",
        resp.CostUSD, len(resp.MemoryHits))
}
```

## Examples

- [`examples/basic`](./examples/basic) — minimal single-provider Call
- [`examples/streaming`](./examples/streaming) — streaming with SSE
- [`examples/memory`](./examples/memory) — persist a fact, retrieve it on a later Call
- [`examples/routing`](./examples/routing) — YAML-driven policy routing across three providers

## Why hippo?

There are already good Go LLM clients. hippo's wedge is the combination of
memory and cost, packaged as a single binary.

- [**any-llm-go**](https://github.com/mozilla-ai/any-llm-go) (Mozilla) is the
  closest analogue on the unified-provider axis. It's lean and focused. hippo
  is broader: memory, budget, and a routing policy are all in the box.
- [**LangChainGo**](https://github.com/tmc/langchaingo) is a port of a much
  bigger ecosystem. If you want chains, agents, and hundreds of integrations,
  use it. hippo deliberately does not go there — we stay small so you can
  build agents *on top* of hippo, not inside it.
- **LiteLLM** is Python, widely used as a cost-aware gateway. Running it as
  a sidecar adds a process and a network hop. hippo folds the gateway, the
  memory store, and the client into one in-process Go library — no extra
  services, no CGO.

Respect to each of these projects; they shaped hippo's design.

## Roadmap

- **v0.1** — Anthropic + OpenAI + Ollama providers, SQLite memory backend,
  YAML routing, embedded pricing table.
- **v0.2** — Gemini + OpenRouter providers, streaming across all providers,
  tool calling normalised across providers.
- **v0.3** — MCP (Model Context Protocol) integration, semantic retrieval via
  local embeddings.
- **v0.4** — `hippo` CLI (run, ask, budget, memory subcommands).

## License

MIT © 2026 Mahdi Salmanzade.

## Credits

hippo's design borrows ideas from
[any-llm-go](https://github.com/mozilla-ai/any-llm-go) (Mozilla),
[mem0](https://github.com/mem0ai/mem0), and
[MemMachine](https://github.com/memmachine-ai/memmachine). None of them are
bundled; the debt is intellectual.
