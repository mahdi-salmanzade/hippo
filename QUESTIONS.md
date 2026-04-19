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
