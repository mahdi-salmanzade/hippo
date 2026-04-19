# hippo

**A Go LLM client with a memory.**

hippo is a pure-Go library for talking to LLMs. Four properties set it apart
from other Go LLM clients:

1. **Unified providers.** One API over Anthropic, OpenAI, and Ollama —
   plus a `Provider` interface if you want to plug in your own.
2. **Memory-aware.** Persistent typed memory (working / episodic / profile)
   is a first-class primitive, not something you bolt on top.
3. **Cost-aware.** A live pricing table, per-call budget enforcement, and a
   router that picks the cheapest model able to satisfy your privacy and
   quality constraints.
4. **MCP-native.** Tools exposed by any Model Context Protocol server
   surface as first-class `hippo.Tool` instances over stdio or
   Streamable HTTP, with automatic reconnect.

All of it compiles to a single static binary (`CGO_ENABLED=0`), with a
minimal dependency tree.

## Install

```bash
go install github.com/mahdi-salmanzade/hippo/cmd/hippo@latest
```

Or as a library:

```bash
go get github.com/mahdi-salmanzade/hippo
```

Requires Go 1.23 or newer.

## Web UI (`hippo serve`)

```bash
hippo init               # create ~/.hippo/config.yaml
hippo serve --open       # launch the UI on http://127.0.0.1:7844
```

The UI runs inside the single binary — templates, CSS, JS, and htmx
are all embedded via `go:embed`. No Node toolchain, no npm, no build
step. Pages:

- **Chat** — pick provider/model/task, toggle memory/tools, stream
  responses live via SSE.
- **Spend** — total, per-provider and per-task spend; recent-calls
  table polled every 3s.
- **Config** — add/remove provider credentials and default models,
  edit budget, toggle memory. Saving reconstructs the Brain.
- **Policy** — edit the routing YAML in-browser; save validates and
  hot-swaps the router.
- **MCP servers** — add stdio or HTTP MCP servers directly from the
  config page; Test button performs a live handshake and reports the
  tool count.

Binding is localhost-only by default. To expose on the network, set
`server.auth_token` in the config (or pass `--auth-token`); the server
refuses to start on a non-localhost address without one.

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

## MCP

```go
client, _ := mcp.Connect(ctx, []string{"npx", "-y", "@scope/your-mcp-server"},
    mcp.WithPrefix("scope"))
defer client.Close()

brain, _ := hippo.New(
    hippo.WithProvider(anthropic.New(anthropic.WithAPIKey(apiKey))),
    hippo.WithMCPClients(client),
)
```

Both stdio and Streamable HTTP transports are supported. The client
targets MCP protocol version `2025-06-18` and auto-reconnects with
exponential backoff if a server drops.

## Examples

- [`examples/basic`](./examples/basic) — minimal single-provider Call
- [`examples/streaming`](./examples/streaming) — streaming with SSE
- [`examples/memory`](./examples/memory) — persist a fact, retrieve it on a later Call
- [`examples/routing`](./examples/routing) — YAML-driven policy routing across three providers
- [`examples/mcp`](./examples/mcp) — connect to an MCP server and use its tools from Anthropic

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
  YAML routing, embedded pricing table, web UI + `hippo serve`, MCP client
  (stdio + Streamable HTTP).
- **v0.2** — Semantic retrieval via local embeddings, per-conversation memory
  scoping, MCP prompts + resources.
- **v0.3** — Gemini + OpenRouter providers, extra CLI subcommands
  (run, ask, budget, memory).

## License

MIT © 2026 Mahdi Salmanzade.

## Credits

hippo's design borrows ideas from
[any-llm-go](https://github.com/mozilla-ai/any-llm-go) (Mozilla),
[mem0](https://github.com/mem0ai/mem0), and
[MemMachine](https://github.com/memmachine-ai/memmachine). None of them are
bundled; the debt is intellectual.
