# Changelog

All notable changes are recorded here. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/). Semantic versioning.

## v1.0.0 - 2026-04-23

First stable release. Public API frozen; breaking changes from this
point require a deprecation cycle.

### Since v1.0.0-beta

- **Fix**: `web.State.SpendByTask` / `SpendByModel` now skip pending
  (in-flight) rows, matching `SpendByProvider` and the `hippo_spend`
  tool's "breakdowns across completed turns" contract. Pending rows
  have `CostUSD = 0` so sums were unaffected; the defect was the
  polluted task/model key set. (`e98a914`)
- **Cleanup**: remove unused `workingHalfLifeHours` /
  `episodicHalfLifeHours` / `profileHalfLifeHours` constants in
  `memory/sqlite/recall.go`; decay math lives entirely in the SQL
  fragment. (`954bfc9`)
- **Cleanup**: reword the `web/config.go` package doc so staticcheck
  stops flagging prose as a malformed `//go:embed` directive.
  (`e96352a`)
- **Test**: fix polling loop in `brain_tools_test.go` that exited
  after one iteration instead of polling for 2s. (`156b0ec`)
- **Docs**: `CurrentState.md` snapshot of the beta cut (`5f93e21`);
  correct the web test count in that file (`f472b1f`); README
  accuracy sweep — status line, stripped-binary size. (`eb2537d`)

### Since v0.2.0 (incorporating v1.0.0-beta)

- `memory/sqlite`: OR-join FTS tokens so multi-word keyword queries
  recall. (`173ba28`)
- `web`: redesign UI with hippo design system — tokens, primitives,
  all five pages rebuilt; polish pass on the design port. (`1dde176`,
  `c84b06d`)
- `web`: conversation history, markdown rendering, built-in tools
  (`hippo_spend`, `hippo_memory_search`, `hippo_policy_read`); router
  respects `Call.Model`. (`7fdae23`)
- `web`: chat history drawer with SQLite persistence. (`7995a02`)
- `web`: persist turn metadata; make spend tool see in-flight calls.
  (`3cb4bce`)
- `web`: split completed vs pending in `hippo_spend` output.
  (`8da76ef`)
- `web`: pre-format `hippo_spend` summary so answers stay consistent
  across Opus runs. (`5ddb92f`)
- `web`: persist spend across restarts via `~/.hippo/spend.json`;
  budget re-seeded from the loaded ring on startup. (`67a880b`)

### Carrying forward from v0.2.0

- Providers: Anthropic (Messages API), OpenAI (Responses API),
  Ollama — streaming, tool use, unified `StreamChunk` events.
- Typed memory with semantic recall: `MemoryQuery.Semantic`,
  `HybridWeight`, `TemporalExpansion`. Pure-Go cosine; FTS5 keyword;
  nucleus temporal expansion pulls conversation-adjacent turns at
  half-score.
- Lazy embedding backfill worker; importance decay (Working 24h,
  Episodic 30d, Profile never).
- Auto-prune worker with per-kind policy.
- Schema migration framework; v0.1.0 databases upgrade in-place.
- Embedder interface with Ollama implementation (`nomic-embed-text`,
  768-dim).
- Cost-aware routing: YAML policy with `prefer` / `fallback` per
  task, privacy tiers, per-task cost caps, mtime-polled hot-reload.
- MCP client: stdio and Streamable HTTP transports, protocol
  `2025-06-18`, exponential-backoff reconnect.
- Embedded web UI: chat, spend dashboard, config, policy editor,
  MCP server management. No Node, no build step.
- CLI: `hippo serve`, `hippo init`, `hippo version`.

### Known limitations carried into v1.0.0

- Only Ollama exposes an `Embedder` today; cloud embedders slot in as
  additional `hippo.Embedder` implementations without API changes.
- Full-scan cosine similarity stays cheap up to ~10K records; past
  that an ANN index is a future conversation.
- Gemini and OpenRouter providers are scaffolded in `providers/` but
  not implemented (planned v1.1).
- MCP prompts and resources aren't supported — tools only.
- Per-conversation memory scoping not implemented; memory shares a
  single namespace per install.
- Config YAML round-trip strips inline comments; a fixed header is
  regenerated on every save. See QUESTIONS.md Q9.1.
- MCP servers that fail the initial 10-second connect log+skip at
  bundle construction; the Client's background reconnect loop
  recovers them, but the Brain isn't rebuilt automatically when they
  come online. See QUESTIONS.md Q10.3.

### Binary size

- 13 MB stripped (`CGO_ENABLED=0 go build -ldflags "-s -w"`); 19 MB
  with symbols and path-trim. No new dependencies since v0.1.0.

## v1.0.0-beta - 2026-04-21

Beta promotion. No functional changes from v0.2.0 - this tag freezes
the public API ahead of the v1.0 cut. Breaking changes from here on
require a deprecation cycle.

### Changed

- Version string bumped from `0.2.0` to `1.0.0-beta` across the CLI,
  web UI footer, and MCP initialize handshake.
- README, roadmap, and in-code decision comments re-scoped from "v0.2
  tuned defaults" to "v1.0 tuned defaults".
- Roadmap: v1.0 → API freeze + final docs; deferred-provider work
  (Gemini, OpenRouter) and per-conversation memory scoping moved to
  v1.1.

## v0.2.0 - 2026-04-19

Semantic memory and operational hygiene.

### Added

- **Embedder interface** (`hippo.Embedder`) and Ollama-backed
  implementation in `providers/ollama`. Default model:
  `nomic-embed-text` (768-dimensional, CPU-fast).
- **Vector recall.** `MemoryQuery.Semantic`, `HybridWeight`, and
  `TemporalExpansion` flags enable cosine-similarity scoring over
  stored embeddings, with FTS5 keyword signal blended at a
  caller-chosen weight. Pure-Go cosine - no ANN dependency.
- **Nucleus temporal expansion** - each semantic hit pulls in records
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
- **Web UI `/memory` page** - browse, keyword / semantic / hybrid /
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

- 19 MB CGO-off (unchanged from v0.1.0 - no new dependencies).

## v0.1.0 - 2026-04-19

Initial release. Single-binary Go LLM client with:

- **Providers** - Anthropic (Messages API), OpenAI (Responses API), Ollama.
- **Streaming** across all providers with unified `StreamChunk` events
  and tool-call reassembly.
- **Tools** - plug-and-play local tool registration with parallel execution,
  tool-hop cap, panic recovery, and structured errors fed back to the LLM.
- **MCP client** - stdio and Streamable HTTP transports, targeting
  protocol `2025-06-18`, with exponential-backoff reconnect.
- **Memory** - SQLite backend with FTS5 keyword search and typed kinds
  (working / episodic / profile); schema auto-migrated on `Open`.
- **Routing** - YAML policy with hot-reload, privacy-tier enforcement,
  per-task cost caps.
- **Budget** - consolidated pricing table, per-call estimate, running
  spend accounting.
- **Web UI** - embedded via `go:embed` (htmx, no Node toolchain);
  localhost-only by default with token auth for network bindings; chat,
  spend dashboard, config, policy editor, MCP server management.
- **CLI** - `hippo serve`, `hippo init`, `hippo version`.

Built during April 17–19, 2026.

### Known limitations

- No semantic memory - keyword FTS5 only. Embeddings targeted for v0.2.
- Gemini and OpenRouter providers are scaffolded in `providers/` but
  not implemented.
- MCP prompts and resources aren't supported - tools only.
- Web UI has no markdown rendering or syntax highlighting in chat.
- Config YAML round-trip strips inline comments; a fixed header is
  regenerated on every save. See QUESTIONS.md Q9.1.
- MCP servers that fail the initial 10-second connect log+skip at
  bundle construction; the Client's background reconnect loop will
  recover them, but the Brain isn't rebuilt automatically when they
  come online. See QUESTIONS.md Q10.3.

### Binary size

- 19 MB (Mach-O arm64, CGO off). Expect ~20 MB on Linux amd64.
