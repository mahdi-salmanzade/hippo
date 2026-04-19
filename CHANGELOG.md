# Changelog

All notable changes are recorded here. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/). Semantic versioning.

## v0.1.0 — 2026-04-19

Initial release. Single-binary Go LLM client with:

- **Providers** — Anthropic (Messages API), OpenAI (Responses API), Ollama.
- **Streaming** across all providers with unified `StreamChunk` events
  and tool-call reassembly.
- **Tools** — plug-and-play local tool registration with parallel execution,
  tool-hop cap, panic recovery, and structured errors fed back to the LLM.
- **MCP client** — stdio and Streamable HTTP transports, targeting
  protocol `2025-06-18`, with exponential-backoff reconnect.
- **Memory** — SQLite backend with FTS5 keyword search and typed kinds
  (working / episodic / profile); schema auto-migrated on `Open`.
- **Routing** — YAML policy with hot-reload, privacy-tier enforcement,
  per-task cost caps.
- **Budget** — consolidated pricing table, per-call estimate, running
  spend accounting.
- **Web UI** — embedded via `go:embed` (htmx, no Node toolchain);
  localhost-only by default with token auth for network bindings; chat,
  spend dashboard, config, policy editor, MCP server management.
- **CLI** — `hippo serve`, `hippo init`, `hippo version`.

Built during April 17–19, 2026.

### Known limitations

- No semantic memory — keyword FTS5 only. Embeddings targeted for v0.2.
- Gemini and OpenRouter providers are scaffolded in `providers/` but
  not implemented.
- MCP prompts and resources aren't supported — tools only.
- Web UI has no markdown rendering or syntax highlighting in chat.
- Config YAML round-trip strips inline comments; a fixed header is
  regenerated on every save. See QUESTIONS.md Q9.1.
- MCP servers that fail the initial 10-second connect log+skip at
  bundle construction; the Client's background reconnect loop will
  recover them, but the Brain isn't rebuilt automatically when they
  come online. See QUESTIONS.md Q10.3.

### Binary size

- 19 MB (Mach-O arm64, CGO off). Expect ~20 MB on Linux amd64.
