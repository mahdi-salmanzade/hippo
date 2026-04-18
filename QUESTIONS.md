# Open design questions

All scaffolding questions resolved as of 2026-04-18.

Two deferrals, for the record, with no code impact until the
implementation pass:

- `modernc.org/sqlite` will be added to `go.mod` alongside the first
  real `memory/sqlite` code (per Q4). Until then `go.sum` stays empty.
- The `cmd/hippo` CLI framework choice (cobra vs stdlib `flag` vs a
  lighter option) is deferred to v0.4 (per Q5).
