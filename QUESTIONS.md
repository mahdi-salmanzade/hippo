# Open design questions

Questions raised during the scaffolding pass. None block compilation; all
want a decision before the implementation pass.

## 1. Where does the `Provider` interface live?

**Tension.** The spec places the `Provider` interface in
`providers/provider.go`, but its method signatures reference `hippo.Call`,
`hippo.Response`, `hippo.StreamChunk`, etc. If the interface is defined in
the `providers` package, `providers` imports the root `hippo` package. For
`options.go` to accept a `Provider` (via `WithProvider`), the root package
must in turn import `providers` — a cycle.

**What I did.** Defined the canonical `Provider` interface in the root
`hippo` package (in `types.go`), and re-exported it from
`providers/provider.go` via a type alias:

```go
// providers/provider.go
type Provider = hippo.Provider
```

This keeps the file layout the spec asked for, avoids the cycle, and still
gives concrete provider packages a natural sibling to reference.

**Question.** Accept the alias, or flatten providers so the interface lives
only in the root package (no `providers/provider.go` file at all)?

## 2. Same tension for `Memory`, `Router`, `Tracker` — resolved differently

`memory.Memory` uses only memory-local types (`Record`, `Scope`), so it
**does not** need to import root. I kept its full definition in
`memory/memory.go`, and at root `options.go` declared a marker interface
`hippo.Memory` with just `Close()`. Any `memory.Memory` structurally
satisfies `hippo.Memory`; the `Brain` will type-assert up to the rich
interface when it needs `Add`/`Recall`/`Prune`.

`router.Router` and `budget.Tracker` **do** use `hippo.Call`, so they can't
be aliased the same way. I applied the same marker-interface trick there:
the rich interfaces live in their subpackages; root `options.go` has a
`Router` with just `Name()` and a `BudgetTracker` with just `Remaining()`.

**Question.** The marker-interface approach works but is slightly
surprising. Alternatives:
- Move `Call`, `Response`, etc. into a dedicated `hippo/core` or
  `hippo/types` package that everyone imports. Cleaner dep graph, but
  changes the public import path for the core types.
- Put the rich interfaces in the root package and make the subpackages
  alias-only. Mirror of what I did for `Provider`; moves some code out of
  files the spec said to put it in.

Which trade-off do you prefer?

## 3. `pricing.yaml` location for `go:embed`

`go:embed` cannot reference files above the importing package's directory.
The spec puts `pricing.yaml` at the repo root but embeds it from
`budget/pricing.go`, which sits one level down. Options:

- Move `pricing.yaml` to `budget/pricing.yaml` (changes the layout).
- Keep it at the root and embed from a root-level file (e.g. a new
  `embed.go`) that exports the bytes for `budget` to parse.
- Duplicate it: keep `pricing.yaml` at root for easy editing, generate or
  copy into `budget/` at build time.

I left `pricing.yaml` at the root for now with no `go:embed` directive, so
`budget.DefaultPricing()` returns an empty table. Please pick one.

## 4. `modernc.org/sqlite` not yet in `go.mod`

The SQLite backend stub in `memory/sqlite/sqlite.go` does not import
`modernc.org/sqlite` yet. That keeps `go.sum` empty and `go build ./...`
fast. The dep will be added in the implementation pass. Flagging so it
isn't a surprise when it appears.

## 5. CLI framework for `cmd/hippo`

The spec notes "cobra comes later". Cobra pulls in a non-trivial dep tree.
Alternatives that are lighter: `flag` (stdlib), `github.com/peterbourgon/ff`,
or a hand-rolled subcommand dispatcher. Worth deciding before v0.4 so the
CLI shape reflects the choice.

## 6. Tool-call argument shape

`ToolCall.Arguments` is `[]byte` (raw JSON) in the current design so
providers can pass through without re-serialising. Callers then
`json.Unmarshal` into their own struct. Alternative: `map[string]any`, more
convenient for ad-hoc use but slower and allocates more. I leaned toward
raw bytes; open to changing.

## 7. `MemoryScope` duplication

There are two scope types:
- `hippo.MemoryScope` — what a Call requests (`None` / `Recent` / `Full` /
  `ByTags`).
- `memory.Scope` — parameters passed to `Memory.Recall`.

They're deliberately separate: the first is user intent, the second is
retrieval parameters. The Brain translates between them. Happy with the
split, or do you want them collapsed?
