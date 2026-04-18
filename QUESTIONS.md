# Open design questions

Questions raised during the scaffolding pass. None block compilation; all
want a decision before the implementation pass.

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
