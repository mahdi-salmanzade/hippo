# Changelog

All notable changes are recorded here. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/). Semantic versioning.

## v0.2.0 — 2026-04-19

Semantic memory and operational hygiene.

### Added

- **Embedder interface** (`hippo.Embedder`) and Ollama-backed
  implementation in `providers/ollama`. Default model:
  `nomic-embed-text` (768-dimensional, CPU-fast).
- **Vector recall.** `MemoryQuery.Semantic`, `HybridWeight`, and
  `TemporalExpansion` flags enable cosine-similarity scoring over
  stored embeddings, with FTS5 keyword signal blended at a
  caller-chosen weight. Pure-Go cosine — no ANN dependency.
- **Nucleus temporal expansion** — each semantic hit pulls in records
  within ±TemporalExpansion of its timestamp at half-score, so a
  conversation-adjacent turn isn't left behind just because it
  doesn't individually match the query.
- **Lazy embedding backfill worker.** `store.StartBackfill` embeds
  records missing an embedding in batches, sleeping between runs so
  it doesn't peg the embedding server. Safe to run concurrently with
  Add/Recall/Prune.
- **Importance decay.** Effective importance =
  `base × pow(0.5, age/half_life) × (1 + ln(1 + access_count)/10)`.
  Working half-life 24h, Episodic 30d, Profile never decays.
  `MinImportance` filters against effective, not base.
- **Auto-prune worker** with per-kind policy: Working max-age
  (default 7d), Episodic max-age + effective-importance cutoff
  (default 90d / 0.2), Profile untouched.
- **Migration framework.** Versioned schema with `schema_version`
  table; v0.1.0 databases migrate in-place without data loss.
  Repopulates FTS5 for pre-Pass-2 installations that skipped it.
- **Web UI `/memory` page** — browse, keyword / semantic / hybrid /
  recent search, paginated; sidebar shows live backfill progress and
  a manual-prune button; per-record delete.
- **Example** `examples/semantic/main.go` demonstrating hybrid +
  nucleus retrieval over a 20-record toy corpus.

### Changed

- `memory/sqlite.Open` now runs the migration framework in place of
  the inlined Pass 2 `applySchema`. Existing callers see no API
  change; existing databases upgrade automatically on next open.
- Recall returns effective (decayed) importance in `Record.Importance`
  rather than the base value. Matches the cutoff semantics and gives
  callers a signal they can sort on without running the decay math.

### Known limitations

- Only Ollama exposes an Embedder today. Cloud embedders (OpenAI,
  Anthropic Voyage) slot in as additional `hippo.Embedder`
  implementations without API changes.
- Full-scan cosine similarity stays cheap up to ~10K records; past
  that an ANN index (hnswlib-go, faiss-cpu) becomes a Pass 12
  conversation.
- Semantic results cache and access-count writes contend with heavy
  concurrent reads on SQLite's single-writer model; not a real
  problem for single-user daemons but may surface under benchmarks.

### Binary size

- 19 MB CGO-off (unchanged from v0.1.0 — no new dependencies).

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
