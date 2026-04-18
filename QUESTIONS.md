# Open design questions

Questions raised during the scaffolding pass. None block compilation; all
want a decision before the implementation pass.

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
- `hippo.Scope` — parameters passed to `Memory.Recall`.

They're deliberately separate: the first is user intent, the second is
retrieval parameters. The Brain translates between them. Happy with the
split, or do you want them collapsed?
