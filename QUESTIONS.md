# Open design questions

All scaffolding questions resolved as of 2026-04-18.

Two deferrals, for the record, with no code impact until the
implementation pass:

- `modernc.org/sqlite` was added to `go.mod` in Pass 2 alongside the
  first real `memory/sqlite` code.
- The `cmd/hippo` CLI framework choice landed in Pass 9: stdlib `flag`.
  Cobra would add a dependency tree the single-binary target doesn't
  need; hippo's surface (serve/init/version) is small enough that
  hand-parsed subcommands win on both size and clarity.

## Pass 9 questions

### Q9.1 - Comment preservation on `web.Config.Save`

**Context.** `~/.hippo/config.yaml` is editable by hand; users will
annotate it with their own comments. `yaml.v3.Marshal` of a struct
doesn't round-trip comments, so the first Save-after-edit wipes them.

**Options considered.**

1. Drive Save through `yaml.Node` trees, preserving the decoded
   node structure including comments. Works but requires maintaining
   parallel read/write paths for every field and pushing edits into
   the tree by hand.
2. Strip comments on save, regenerate a fixed header block every
   write.
3. Leave the file alone on save and emit a sibling `.last-saved.yaml`
   - surfaces the diff but leaves the user to reconcile.

**Decision.** Option 2. Save rewrites the file with a known-good
header comment, and commits no effort to preserving mid-file
annotations. The rationale: the web UI is the primary editor for
anyone who cares about the config; power users who want comments can
keep them in a sibling `policy.yaml` (which hippo's router hot-reloads
untouched) or accept the tradeoff.

The header explicitly warns that the file is mode 0600 and that the
UI is the recommended editor. If user confusion lands (issue traffic),
revisit with Option 1.

### Q9.2 - Session map TTL

5-minute TTL between POST /chat and GET /chat/stream. The JS opens
the EventSource immediately, so the real window is ~100ms; the
5-minute cap is just a safety net against leaking sessions from
browsers that crashed mid-turn. No need to configure.

## Pass 10 questions

### Q10.1 - Prefix separator for MCP tool names

**Context.** The spec copy used examples like `dubai.search`, but
hippo's `Tool.Name()` pattern is `^[a-zA-Z_][a-zA-Z0-9_]{0,63}$` -
dots are not permitted. The root cause is downstream: Anthropic and
OpenAI both require `^[a-zA-Z0-9_-]{1,64}$` for function names and
reject dots at request time. Relaxing hippo's pattern would not help
because the provider adapters would fail later.

**Decision.** Use `_` as the prefix separator. A server named `echo`
exposing tool `echo` appears to hippo as `echo_echo`. Tools whose
remote name contains characters that can't be expressed in the
provider's allowed set (say `search.v2`) are skipped with a Warn log
- the caller can either rename on the server side or add an explicit
prefix that normalizes the surrounding name.

If MCP ever standardises prefix handling itself, revisit.

### Q10.2 - Protocol version tolerance

hippo targets MCP `2025-06-18`. Version mismatch logs a Warn but does
not fail the connection; real-world MCP servers vary across
`2024-11-05`, `2025-03-26`, and newer revisions, and the parts hippo
actually uses (initialize, tools/list, tools/call) have stayed
stable. If a revision lands that changes the tool-call payload
shape, we'll add a pinned compatibility matrix.

### Q10.3 - Bundle-time connect timeout: one-shot vs retry

Servers that fail the initial connect get a single 10-second attempt,
then are logged and skipped. The Client's own reconnect loop then
continues in the background - but we don't re-add the skipped client
to the Brain after the fact, so a server that comes online after
bundle construction stays out of the current Brain. The user can
re-save the config page to rebuild.

Alternative considered: defer the Brain build until every MCP server
has either connected or given up. Rejected because it turns "my
stdio server is broken" into "hippo serve hangs for 10s × N",
which is the wrong failure mode for first-run UX. Good enough for
v0.1.0; if users report they want eventual-consistency with
live-connecting servers, we'd add a "rebuild Brain on MCP connect"
path.

## Pass 11 questions

### Q11.1 - Decay formula wording: half-life vs e-folding time

Spec prose used "half-life 24h". First implementation passed that
value through `exp(-age/τ)` with τ=24, which is e-folding time, not
half-life. Test expected 0.25 at 48h but got 1/e² ≈ 0.135. Switched
the SQL expression to `pow(0.5, age/half_life)` so the literal
meaning of half-life holds.

modernc.org/sqlite exposes `pow()`, `ln()`, and `exp()`. If a future
pinned driver drops them, the Go-side fallback is the equivalent
`exp(-age × ln(2) / half_life)` - algebraically identical, no
additional round-trip.

### Q11.2 - `WithEmbedder` on Brain: carry-through, not auto-start

Spec: "If also WithMemory is set, Brain starts the backfill worker
at construction time and tears it down on Close()." Implementation
skipped that - Brain just records the embedder via `Brain.Embedder()`;
the store starts its own backfill when the caller asks
(`sqlite.Store.StartBackfill`). The web package calls `StartBackfill`
explicitly in `BuildBrain` so end users get the auto-start
experience, but library callers who import hippo directly control
the worker lifecycle without surprise goroutines.

Rationale: the `hippo.Memory` interface is deliberately narrow
(Add/Recall/Prune/Close); adding an optional StartBackfill method
there would bleed a backend concern into the contract. A type
assertion in the web bundle builder keeps the split clean.

### Q11.3 - Full-scan cosine - good enough for v0.2

Cosine over 10K × 768-dim records fits in ~30 MB and scans under
10 ms on a laptop. For single-user daemons (hippo's primary target)
this is more than enough. ANN indexes (hnswlib-go, usearch) are a
v0.3+ conversation if anyone brings real corpus sizes.

### Q11.4 - FTS tokenisation of hyphenated fixtures

The migration-reconcile test originally used `bare-legacy` as a
fixture string. FTS5's `porter unicode61` tokeniser treats hyphens
as word boundaries, so a quoted-phrase MATCH for `"bare-legacy"` hit
zero rows even though the content was indexed. Test was reshaped to
query by recency rather than FTS so the assertion covers data
preservation, not tokenisation edge cases.

No user-facing fix: hippo doesn't escape hyphens in search input
because the underlying tokenisation is the right behaviour for
humans searching natural text. Users who want to bypass the
tokeniser can pick semantic mode on `/memory`.

### Q11.5 - `Record.Importance` returns effective, not base

Recall now writes the decayed value into the returned record's
Importance field instead of echoing the base. It's the value callers
most often want - "how confident is this hit, right now?" - and the
one the MinImportance cutoff runs against, so returning a different
value would be confusing. Callers that need the base value can look
it up by ID on the store's `DB()`; that escape hatch is expected to
stay rare.
