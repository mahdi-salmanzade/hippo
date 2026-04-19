# hippo

**A single-binary Go LLM client with memory, cost-awareness, and MCP.**

[![CI](https://github.com/mahdi-salmanzade/hippo/actions/workflows/ci.yml/badge.svg)](https://github.com/mahdi-salmanzade/hippo/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.23+-00ADD8)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](./LICENSE)
[![Release](https://img.shields.io/github/v/release/mahdi-salmanzade/hippo?include_prereleases)](https://github.com/mahdi-salmanzade/hippo/releases)

hippo is a pure-Go LLM client you import as a library or run as a standalone
binary. It unifies Anthropic, OpenAI, and Ollama behind one API; persists
typed memory in SQLite; enforces a USD budget; and speaks Model Context
Protocol out of the box. Everything ships in one 19 MB binary, CGO-free,
with a minimum dependency tree.

> **Status: v0.2.0 — alpha.** Semantic memory, nucleus retrieval, and
> auto-prune all shipped in v0.2; the public API may still change in
> breaking ways before v1.0. See the [changelog](./CHANGELOG.md).

## Quick start (library)

```bash
go get github.com/mahdi-salmanzade/hippo
```

```go
package main

import (
    "context"
    "fmt"
    "os"

    "github.com/mahdi-salmanzade/hippo"
    "github.com/mahdi-salmanzade/hippo/providers/anthropic"
)

func main() {
    p, _ := anthropic.New(anthropic.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")))
    b, _ := hippo.New(hippo.WithProvider(p))
    defer b.Close()

    resp, _ := b.Call(context.Background(), hippo.Call{
        Task:   hippo.TaskGenerate,
        Prompt: "Say hi in two words.",
    })
    fmt.Println(resp.Text, "—", resp.Model, "— $", resp.CostUSD)
}
```

## Quick start (UI)

```bash
go install github.com/mahdi-salmanzade/hippo/cmd/hippo@latest
hippo init               # writes ~/.hippo/config.yaml (mode 0600)
hippo serve --open       # opens http://127.0.0.1:7844 in your browser
```

The web UI binds localhost-only by default. Expose it on the network by
setting `server.auth_token` in the config (or passing `--auth-token`); the
server refuses to start on a non-localhost address without one.

## What you can build with it

### Unified providers

Anthropic (Messages API), OpenAI (Responses API), and Ollama (local) are
first-class. Each provider is one package under `providers/`; adding a new
one means implementing the `hippo.Provider` interface.

```go
b, _ := hippo.New(
    hippo.WithProvider(anthropic.New(anthropic.WithAPIKey(ak))),
    hippo.WithProvider(openai.New(openai.WithAPIKey(ok))),
    hippo.WithProvider(ollama.New(ollama.WithBaseURL("http://localhost:11434"))),
)
```

Streaming works across all three with a single channel-based API that
folds tool calls and thinking traces into one event stream.

### Typed memory with semantic recall

Memory is working / episodic / profile, not an undifferentiated vector
blob. Records carry kind, tags, importance, and an optional embedding.
The SQLite backend uses FTS5 for keyword recall and pure-Go cosine
similarity over stored vectors for semantic recall — no ANN index
dependency for the v0.2 scale (up to ~10K records).

```go
// Local embedder via Ollama — no cloud key required.
emb := ollama.NewEmbedder(ollama.WithEmbedderModel("nomic-embed-text"))
store, _ := sqlite.Open("~/.hippo/memory.db", sqlite.WithEmbedder(emb))

// Backfill worker fills embeddings for older records in the background.
stop, _ := store.(interface {
    StartBackfill(context.Context, sqlite.BackfillConfig) (func(), error)
}).StartBackfill(ctx, sqlite.BackfillConfig{Embedder: emb})
defer stop()

b, _ := hippo.New(
    hippo.WithProvider(p),
    hippo.WithMemory(store),
    hippo.WithEmbedder(emb),
)

// Ask a semantic question: the query text is embedded, cosine-scored
// against the store, and nucleus-expanded by a one-hour window around
// each hit so conversation-adjacent turns come along for the ride.
recs, _ := store.Recall(ctx, "billing refactor wip", hippo.MemoryQuery{
    Semantic:          true,
    HybridWeight:      0.6,
    TemporalExpansion: 1 * time.Hour,
    Limit:             5,
})
```

Importance decays per-kind (Working 24h half-life, Episodic 30d,
Profile never) so stale records fall behind in ranking automatically.
`MinImportance` cutoffs run against the decayed value; each recall
also bumps an access_count that boosts frequently-retrieved rows.

### Cost-aware routing

A YAML policy picks provider and model per `Call.Task`, constrained by
privacy tier, budget ceiling, and an embedded pricing table. Hot-reload
on file edit is built in.

```yaml
tasks:
  reason:
    prefer: [anthropic:claude-opus-4-7, anthropic:claude-sonnet-4-6]
    fallback: [openai:gpt-5, openai:gpt-5-mini]
    max_cost_usd: 0.10
```

### Tools, with or without MCP

Register local Go tools via `WithTools`; they execute in parallel with
bounded concurrency and feed results back through the provider's
tool-call loop.

```go
b, _ := hippo.New(hippo.WithProvider(p), hippo.WithTools(weatherTool, searchTool))
```

Or connect a Model Context Protocol server — stdio or Streamable HTTP —
and its tools surface automatically:

```go
client, _ := mcp.Connect(ctx, []string{"npx", "-y", "@scope/server"},
    mcp.WithPrefix("scope"))
defer client.Close()

b, _ := hippo.New(hippo.WithProvider(p), hippo.WithMCPClients(client))
```

hippo targets MCP protocol `2025-06-18`; older servers are tolerated with
a warning. Reconnect with exponential backoff runs in the background.

### Single-binary UI

`hippo serve` embeds templates, CSS, htmx, and static assets via
`go:embed` — no Node, no npm, no build step. Provider credentials,
routing policy, MCP servers, and spend dashboards all edit in the
browser.

## Why hippo?

| | hippo | any-llm-go | LiteLLM | LangChainGo |
|---|---|---|---|---|
| Single binary | ✓ | ✓ | ✗ (Python) | ✗ |
| Built-in memory store | ✓ | ✗ | ✗ | ✓ (many, heavy) |
| Budget tracking | ✓ | ✗ | ✓ | ✗ |
| Routing policy | ✓ (YAML, hot-reload) | ✗ | ✓ | partial |
| MCP client | ✓ | ✗ | ✗ | partial |
| Embedded web UI | ✓ | ✗ | ✓ (Python) | ✗ |
| Dependencies | 1 (SQLite) + YAML | minimal | huge | large |

Respect to each project; they shaped hippo's design. hippo's wedge is the
combination — memory + cost + tools + MCP — packaged as one binary.

## Examples

- [`examples/basic`](./examples/basic) — minimal single-provider Call
- [`examples/streaming`](./examples/streaming) — streaming with SSE
- [`examples/memory`](./examples/memory) — persist and retrieve across calls
- [`examples/semantic`](./examples/semantic) — semantic + hybrid + nucleus recall
- [`examples/routing`](./examples/routing) — YAML policy across three providers
- [`examples/tools`](./examples/tools) — parallel local tool execution
- [`examples/mcp`](./examples/mcp) — MCP server tools via Anthropic

## Docs

- [Install guide](./docs/INSTALL.md) — paths, prerequisites, troubleshooting
- [Contributing](./docs/CONTRIBUTING.md) — local workflow, PR checklist
- [Changelog](./CHANGELOG.md) — release history
- [QUESTIONS.md](./QUESTIONS.md) — design decision record

## Roadmap

- **v0.3** — Gemini + OpenRouter providers, per-conversation memory
  scoping, MCP prompts and resources, extra CLI subcommands.
- **v1.0** — API freeze.

## Credits

hippo's design borrows ideas from
[any-llm-go](https://github.com/mozilla-ai/any-llm-go) (Mozilla),
[mem0](https://github.com/mem0ai/mem0), and
[MemMachine](https://github.com/memmachine-ai/memmachine). None of them are
bundled; the debt is intellectual.

## License

MIT © 2026 Mahdi Salmanzade. See [LICENSE](./LICENSE).
