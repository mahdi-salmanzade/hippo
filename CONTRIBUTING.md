# Contributing to hippo

Thanks for your interest. hippo is in alpha and moving fast; please open an
issue before starting a non-trivial change so we can align on scope.

## Ground rules

- **Go 1.23+ only.** No build tags for older toolchains.
- **CGO_ENABLED=0.** Every PR must build and test with CGO disabled. If your
  change needs a C library, open an issue first — the answer is usually
  "find a pure-Go alternative or don't do it."
- **Minimal dependencies.** The currently allowed third-party deps are:
  - `modernc.org/sqlite` — pure-Go SQLite
  - `gopkg.in/yaml.v3` — YAML parsing
  - `github.com/stretchr/testify` — test assertions
  Adding a new dep requires a brief justification in the PR description.
- **stdlib first.** `net/http`, `encoding/json`, `log/slog`, `context`, and
  `bufio` cover most needs. Please use them.
- **Idiomatic Go.** Functional options, interfaces over structs for plug
  points, errors as values with typed sentinels, `context.Context` as the
  first argument on every public method, channels for streaming — not
  callbacks.

## Local workflow

```bash
go vet ./...
go test ./...
staticcheck ./...  # install with: go install honnef.co/go/tools/cmd/staticcheck@latest
```

CI runs the same three commands on Go 1.23 and 1.24. If they pass locally
they should pass in CI.

## Commit and PR style

- One logical change per commit.
- Imperative mood in commit subjects ("add budget tracker", not "added …").
- PR descriptions should explain *why* the change, not *what* the diff shows.
- If you touch public API, update godoc comments in the same commit.

## Reporting issues

Include:
- Go version (`go version`)
- OS / architecture
- A minimal repro, ideally as a `go test` case

## Security

Please email security issues privately rather than filing a public issue.
