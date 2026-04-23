# hippo — Current State

**Version:** `v1.0.0-beta`
**Snapshot taken:** 2026-04-21 14:36 +04:00
**Head commit:** `67a880b` — _web: persist spend across restarts_ (2026-04-21 13:35)

A living snapshot of what's in hippo at the v1.0.0-beta API freeze. Intended as
a map for new contributors and a reference for anyone reviewing the scope that
ships in the first stable cut.

---

## 1. Elevator pitch

A **single-binary, pure-Go LLM client** you can import as a library or run as a
standalone web UI. It unifies Anthropic, OpenAI, and Ollama behind one API;
persists typed memory to local SQLite with semantic recall; enforces a USD
budget; speaks Model Context Protocol; and ships with an embedded
designer-grade web UI. One ~19 MB binary, CGO-free, minimum dependency tree.

Everything runs on localhost by default — no outbound calls except to the
provider endpoints you explicitly configured.

---

## 2. Library surface (`github.com/mahdi-salmanzade/hippo`)

### Core types

| Type              | Purpose                                                      |
| ----------------- | ------------------------------------------------------------ |
| `Brain`           | The main dispatcher — wraps providers + router + budget + memory + tools. Built via `hippo.New(options…)`, closed with `Close()`. |
| `Provider`        | Interface a backend implements (`anthropic`, `openai`, `ollama`). |
| `Router`          | Chooses provider/model per call. `router/yaml` is the shipping implementation. |
| `BudgetTracker`   | USD cap enforcement. `budget.New(...)` gives an in-memory tracker. |
| `Memory`          | Record store interface. `memory/sqlite` is the shipping implementation (FTS5 + optional vector index). |
| `Tool`            | LLM-callable function. Registered via `WithTools(...)` or surfaced from an MCP server via `WithMCPClients(...)`. |
| `Call`            | Per-request input: task, privacy, prompt, messages, tools, model pin, metadata, memory scope. |
| `Response`        | Result: text, usage, cost, model, provider, decision reason. |
| `StreamChunk`     | One event in a streaming turn (`text`, `thinking`, `tool_call`, `tool_result`, `usage`, `error`). |

### Options (`hippo.New`)

`WithProvider`, `WithRouter`, `WithBudget`, `WithMemory`, `WithEmbedder`,
`WithTools`, `WithMCPClients`, `WithLogger`, `WithMaxToolHops`.

### Task kinds

`classify` / `reason` / `generate` / `protect` — free-text strings backed by
the policy YAML's per-task routing rules.

### Providers (`providers/*`)

| Package     | Auth        | Streaming | Tools | Embeddings | Notes |
| ----------- | ----------- | --------- | ----- | ---------- | ----- |
| `anthropic` | API key     | ✓         | ✓     | —          | Claude 4.x family + Opus/Sonnet/Haiku pricing table. |
| `openai`    | API key     | ✓         | ✓     | —          | gpt-5 family pricing. |
| `ollama`    | Base URL    | ✓         | ✓     | ✓          | Local. Implements `hippo.Embedder` via `/api/embed`. |
| `gemini`    | _scaffold_  | —         | —     | —          | Deferred to v1.1. |
| `openrouter`| _scaffold_  | —         | —     | —          | Deferred to v1.1. |

### Routing (`router/yaml`)

YAML policy → per-task `prefer:` + `fallback:` slug lists (`provider:model`),
privacy tiers (`cloud_ok`, `sensitive_redact`, `local_only`), per-task
`max_cost_usd` cap, hot-reload on file mtime change.

**Model pin override** _(added this session)_: `Call.Model` wins over the
policy's task-based pick. Falls through to policy if no provider can price the
pinned model.

### Budget (`budget`)

In-memory USD tracker. `EstimateCost` uses an embedded pricing table; `Charge`
bumps the running total; `Remaining` clamps at zero. Hot-swappable via
`Brain` rebuild.

### Memory (`memory/sqlite`)

Pure-Go SQLite (`modernc.org/sqlite`, no CGO). Tables: `memories`,
`memory_tags`, `memory_embeddings`, `memory_embedding_meta`.

- **Kinds**: `profile`, `semantic`, `episodic`, `working` with tuned decay
  half-lives.
- **FTS5** full-text search; OR-joined tokens for multi-word keyword queries.
- **Embeddings**: lazy backfill worker, hybrid (keyword + cosine) recall,
  nucleus temporal expansion (adjacent turns pulled in automatically).
- **Prune**: auto-prune worker + manual UI button; importance-aware so
  high-value records survive.

### MCP (`mcp`)

Full Model Context Protocol client — `stdio` and `http` transports,
auto-reconnect loop, tool-name collision check, prefix scoping.

---

## 3. Embedded web UI (`web/`)

Sticky glass app-bar, Inter + JetBrains Mono, warm paper/rust/cyan palette,
dark-mode tokens ready, designer-grade everywhere. Server-rendered Go
templates + HTMX + minimal vanilla JS — no React / build step.

### Pages

| Route      | What it does |
| ---------- | ------------ |
| `/`        | Redirect: to `/chat` if a Brain is built, else `/config`. |
| `/chat`    | Chat playground. Controls strip (provider / model / task / memory toggle / tool count pill / new-chat / history-drawer). Bubble messages with hippo-head avatar on the assistant. Composer with rust send button. Empty state with suggestion chips (embedder-aware). |
| `/memory`  | Search + segmented mode (keyword/semantic/hybrid/recent). Zebra table: timestamp, kind pill (`episodic` cyan / `semantic` rust / `working` amber / `profile` moss), 4-bar signal for importance, inline tag run, hover-reveal delete. Right rail: Backfill status (auto-refresh every 5 s), Manual prune. |
| `/spend`   | Hero row: cumulative-cost sparkline + daily-budget ring. 3-col breakdowns (by provider / task / model). Zebra recent-calls table (htmx-polled). Warm empty-state copy when `CallCount == 0`. |
| `/config`  | Provider cards (glyph + toggle + show/hide key + default-model select + base URL for Ollama), Brain Summary card with route, MCP servers section, Budget + Memory grid, sticky-save bar on dirty. |
| `/policy`  | Path + validity pill header, YAML editor with line numbers, right rail (Live Validation, Hot-swap), sticky save. |

### Chat feature set _(largely built or refined today)_

- **Conversation history** — client-side transcript sent as `history` JSON on
  every POST so the model sees the full thread (was single-turn before).
- **Markdown rendering** — deterministic DOM-building renderer handles
  paragraphs, `#`–`######` headings, `-` / `*` / `1.` lists, `**bold**`,
  `*italic*`, `` `code` ``, ` ``` `fenced blocks` ``` `, and GFM tables.
  Applied on `done` so streaming stays cheap.
- **Drawer (history sidebar)** — left-slide aside with session list
  (title / first-line preview / relative timestamp / msg count / hover-delete).
  Click to rehydrate a past conversation; Esc / scrim / close button dismiss.
  "New chat" is lazy — the session is only created on the first send so
  empty sessions never pollute the list.
- **Built-in tools (always registered on the Brain)**:
  - `hippo_spend` — returns `completed_usd`, `completed_calls`,
    `pending_calls`, breakdowns, budget status, and a pre-formatted
    `summary` string the model is instructed to present faithfully.
  - `hippo_memory_search` — wraps `Memory.Recall` with `query`, `mode`,
    `limit` args; returns JSON records with kind/importance/tags.
  - `hippo_policy_read` — returns the current `policy.yaml` contents.
- **Spend tool sees the in-flight turn** — the stream handler records an
  `id`ed placeholder before `Brain.Stream` runs, then patches it on the
  `usage` event. Pending rows count toward `call_count` but are hidden
  from the Recent Calls table and don't inflate `completed_usd`.
- **Per-turn meta persistence** — assistant rows save model / cost /
  latency / token counts as JSON in `chat_messages.metadata`. Loading a
  past conversation rebuilds the meta line under each bubble plus a
  short `Apr 21 · 12:34` timestamp.

### Assets

- `web/static/app.css` — full design-token system (light + dark-mode
  variables), primitives (btn / pill / toggle / segmented / card /
  table-zebra / sticky-save), per-page styles.
- `web/static/app.js` — vanilla JS: flash auto-dismiss, sticky-save dirty
  tracking, API-key show/hide, policy line numbering, chat streaming,
  markdown renderer, drawer.
- `web/static/htmx.min.js` — vendored.
- Inline SVG icons (clock, plus, send, search, eye, trash, …) and the
  simplified `HippoMark` SVG inside `layout.html`.
- `hippo.svg` at repo root — standalone nav mark.

---

## 4. Storage layout

All paths default under `~/.hippo/` and are configurable in `config.yaml`.

| File                           | Purpose | Configurable at |
| ------------------------------ | ------- | --------------- |
| `~/.hippo/config.yaml`         | Single YAML — providers, budget, memory, chat, spend, server, policy path, MCP servers. Mode `0600`. | `hippo serve --config` |
| `~/.hippo/policy.yaml`         | Router policy. Hot-reloaded on save. | `policy_path` |
| `~/.hippo/memory.db`           | Typed memory (SQLite — FTS5 + embeddings). | `memory.db_path` |
| `~/.hippo/chats.db`            | Chat drawer sessions + messages (SQLite, WAL, FK cascade). | `chat.db_path` |
| `~/.hippo/spend.json`          | Recent-calls ring persisted across restarts. _(added today)_ | `spend.persist_path` |

---

## 5. Config shape (`web.Config`)

```yaml
providers:
  anthropic: {enabled: true, api_key: "sk-ant-…", default_model: claude-sonnet-4-6}
  openai:    {enabled: false, api_key: "",        default_model: gpt-5-mini}
  ollama:    {enabled: true,  base_url: http://127.0.0.1:11434, default_model: llama3.3:70b}

budget:
  ceiling_usd: 10.00

policy_path: ~/.hippo/policy.yaml

memory:
  enabled: true
  db_path: ~/.hippo/memory.db
  embedder: {provider: ollama, model: nomic-embed-text}
  prune:   {interval: 24h, keep_importance_above: 0.4}

chat:
  db_path: ~/.hippo/chats.db

spend:
  persist_path: ~/.hippo/spend.json

server:
  addr: 127.0.0.1:7844
  auth_token: ""

mcp:
  servers:
    - {name: gh, transport: stdio, command: [npx, -y, @upstream/gh-mcp], enabled: true}
```

All fields are optional; defaults match the example.

---

## 6. CLI (`cmd/hippo`)

```
hippo <subcommand> [flags]

Subcommands:
  serve    start the web UI
  init     create ~/.hippo/config.yaml with defaults
  version  print version, commit, Go version
```

Notable `serve` flags: `--config`, `--addr`, `--bind`, `--auth-token`,
`--log-level`, `--open` (launch default browser).

---

## 7. Session log — what got built today (2026-04-21)

Chronological via `git log` — eight commits over ~5 hours.

| Time (+04:00) | Commit    | Summary |
| ------------- | --------- | ------- |
| 10:05 | `173ba28` | `memory/sqlite`: OR-join FTS tokens so multi-word keyword queries recall. |
| 10:42 | `1dde176` | `web`: redesign UI with hippo design system. Tokens + primitives + all 5 pages rebuilt. |
| 11:08 | `c84b06d` | `web`: polish pass on the design port. SVG mark back in empty state, scrollbar fixes, ring `$0.00`, warm empty-state copy, suggestion gating on `HasEmbedder`, nav-underline centering, memory tag inline run. |
| 11:10 | `842c089` | `docs`: v1.0.0-beta bump, README logo + thanks, em-dash sweep. |
| 12:10 | `7fdae23` | `web`: conversation history, markdown rendering, built-in tools. Transcript wiring, DOM-node markdown renderer, `hippo_spend` / `hippo_memory_search` / `hippo_policy_read`, router respects `Call.Model`. |
| 12:21 | `7995a02` | `web`: chat history drawer with SQLite persistence. `ChatStore`, `/api/chats`, auto-titles, left-slide drawer, rehydration. |
| 13:06 | `3cb4bce` | `web`: persist turn metadata; make spend tool see in-flight calls. `ChatTurnMeta` JSON column, `GetFull`, `CallRecord.ID + Pending`, optimistic placeholder + update-by-id. |
| 13:10 | `8da76ef` | `web`: split `completed` vs `pending` in `hippo_spend` output. |
| 13:13 | `5ddb92f` | `web`: pre-format `hippo_spend` summary so answers stay consistent across Opus runs. |
| 13:35 | `67a880b` | `web`: persist spend across restarts. `~/.hippo/spend.json` with atomic tmp+rename; budget re-seeded from loaded ring on startup; pending rows dropped on load. |

---

## 8. Testing

`go test ./...` → all packages green at this snapshot. Notable coverage:

- `web/`: **56 tests** — handlers, MCP, chat store (round-trip + meta +
  auto-title + cascade delete + rename), state (ring cap + pending flow +
  persistence round-trip + seeded loading), spend tool (shape + pending +
  summary), built-in-tool wiring, memory search guards, policy read paths.
- `router/yaml/`: router dispatch, prefer/fallback, privacy tier, cost
  cap, hot-reload, model-pin override.
- `memory/sqlite/`: migrations, recall (keyword / semantic / hybrid /
  nucleus), backfill worker, prune, decay.
- Provider packages: contract tests for each (anthropic, openai, ollama)
  with recorded fixtures + integration tests gated by env.

---

## 9. What's deferred to v1.0 final → v1.1

- **Providers**: Gemini, OpenRouter (scaffolds in tree; not wired).
- **Per-conversation memory scoping**: memory currently shares a single
  namespace per install.
- **Dark-mode toggle UI**: tokens already defined; just needs a switch in
  the app-bar.
- **Live-updating sparkline / ring on the Spend page**: currently static
  on render; htmx can be wired to re-fetch on window-segment change.
- **Today-only filter on Spend**: recent-calls ring is time-agnostic.
  `spent today` label is approximate on a multi-day-old ring.

---

## 10. Roadmap notes from this session

Open questions the user and assistant flagged while building:

- **Spend-tool freshness** — even with the placeholder fix, the tool reports
  `$0` for the *current* cost until the `usage` event fires. A proper fix
  would estimate cost upfront via `Provider.EstimateCost` against the Call,
  but that's a v1.1+ concern.
- **Conversation rename/UI** — drawer rows have a rename endpoint
  (`PATCH /api/chats/{id}`) but no inline-edit affordance yet.
- **Branching / forking chats** — not implemented; each session is linear.

---

_This file is a snapshot; regenerate by walking the git log + code after
significant changes. Not auto-maintained._
