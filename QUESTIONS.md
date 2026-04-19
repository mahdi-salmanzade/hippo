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

### Q9.1 — Comment preservation on `web.Config.Save`

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
   — surfaces the diff but leaves the user to reconcile.

**Decision.** Option 2. Save rewrites the file with a known-good
header comment, and commits no effort to preserving mid-file
annotations. The rationale: the web UI is the primary editor for
anyone who cares about the config; power users who want comments can
keep them in a sibling `policy.yaml` (which hippo's router hot-reloads
untouched) or accept the tradeoff.

The header explicitly warns that the file is mode 0600 and that the
UI is the recommended editor. If user confusion lands (issue traffic),
revisit with Option 1.

### Q9.2 — Session map TTL

5-minute TTL between POST /chat and GET /chat/stream. The JS opens
the EventSource immediately, so the real window is ~100ms; the
5-minute cap is just a safety net against leaking sessions from
browsers that crashed mid-turn. No need to configure.

## Pass 10 questions

### Q10.1 — Prefix separator for MCP tool names

**Context.** The spec copy used examples like `dubai.search`, but
hippo's `Tool.Name()` pattern is `^[a-zA-Z_][a-zA-Z0-9_]{0,63}$` —
dots are not permitted. The root cause is downstream: Anthropic and
OpenAI both require `^[a-zA-Z0-9_-]{1,64}$` for function names and
reject dots at request time. Relaxing hippo's pattern would not help
because the provider adapters would fail later.

**Decision.** Use `_` as the prefix separator. A server named `echo`
exposing tool `echo` appears to hippo as `echo_echo`. Tools whose
remote name contains characters that can't be expressed in the
provider's allowed set (say `search.v2`) are skipped with a Warn log
— the caller can either rename on the server side or add an explicit
prefix that normalizes the surrounding name.

If MCP ever standardises prefix handling itself, revisit.

### Q10.2 — Protocol version tolerance

hippo targets MCP `2025-06-18`. Version mismatch logs a Warn but does
not fail the connection; real-world MCP servers vary across
`2024-11-05`, `2025-03-26`, and newer revisions, and the parts hippo
actually uses (initialize, tools/list, tools/call) have stayed
stable. If a revision lands that changes the tool-call payload
shape, we'll add a pinned compatibility matrix.

### Q10.3 — Bundle-time connect timeout: one-shot vs retry

Servers that fail the initial connect get a single 10-second attempt,
then are logged and skipped. The Client's own reconnect loop then
continues in the background — but we don't re-add the skipped client
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
