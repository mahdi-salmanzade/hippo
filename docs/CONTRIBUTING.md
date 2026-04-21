# Contributing to hippo

Thanks for your interest. hippo is in alpha and moving fast; please
open an issue before starting a non-trivial change so we can align on
scope.

## Getting set up

Clone, build, test:

```bash
git clone https://github.com/mahdi-salmanzade/hippo
cd hippo
go test ./...
```

That's it. No generators, no Makefile, no build container.

## Running integration tests

Integration tests hit real provider APIs (Anthropic, OpenAI) and cost a
fraction of a cent per run. They're gated on `HIPPO_RUN_INTEGRATION=1`
so a normal `go test ./...` stays offline.

```bash
HIPPO_RUN_INTEGRATION=1 \
  ANTHROPIC_API_KEY=sk-... \
  go test -run Integration -v ./providers/anthropic/...
```

The MCP `stdio` integration test (`TestStdioIntegrationEchoServer`)
runs unconditionally - it builds `examples/mcp/echo_server` into a
temp binary and drives it over stdin/stdout, so there's no external
dependency.

The fresh-HOME first-run smoke test lives at
`scripts/fresh-home-test.sh` and runs in CI on Linux after the test
matrix.

## Design record

hippo was built in ten passes over 48 hours in April 2026. Every pass
added one layer (provider, memory, router, streaming, tools, web UI,
MCP). The design decisions from each pass - alternatives considered,
why one was picked - are recorded in [QUESTIONS.md](../QUESTIONS.md).
Read it before proposing an architectural change; the same ground has
probably been walked.

## Ground rules

- **Go 1.23+ only.** No build tags for older toolchains.
- **CGO_ENABLED=0.** Every PR must build and test with CGO disabled.
  If your change needs a C library, open an issue first - the answer
  is usually "find a pure-Go alternative or don't do it."
- **Minimal dependencies.** Currently allowed third-party deps are
  `modernc.org/sqlite` and `gopkg.in/yaml.v3`. Adding a new one
  requires a brief justification in the PR description.
- **stdlib first.** `net/http`, `encoding/json`, `log/slog`, `context`,
  and `bufio` cover most needs. Please use them.
- **Idiomatic Go.** Functional options, interfaces over structs for
  plug points, errors as values with typed sentinels,
  `context.Context` as the first argument on public methods, channels
  for streaming - not callbacks.

## PR checklist

Before opening a PR, please confirm:

- [ ] `go vet ./...` clean.
- [ ] `go test ./...` green offline.
- [ ] `staticcheck ./...` clean (install via
      `go install honnef.co/go/tools/cmd/staticcheck@latest`).
- [ ] `bash scripts/fresh-home-test.sh` passes if your change touches
      the CLI or the web UI.
- [ ] Binary size stayed under 22 MB
      (`CGO_ENABLED=0 go build ./cmd/hippo && ls -lh hippo`).
- [ ] No new dependencies - or if there are, the PR description
      explains why stdlib wouldn't work.
- [ ] Public-API changes are reflected in godoc comments in the same
      commit as the code.

## Commit style

- One logical change per commit.
- Imperative mood in commit subjects ("add budget tracker", not
  "added …").
- Scoped prefix where it helps:
  `mcp:`, `web:`, `cmd/hippo:`, `providers/anthropic:`, `docs:`.
- Green-at-each-commit. If splitting creates useless intermediate
  stubs, collapse.
- PR descriptions should explain *why* the change, not *what* the diff
  shows.

## Reporting issues

File at https://github.com/mahdi-salmanzade/hippo/issues with:

- Go version (`go version`)
- OS / architecture
- hippo version (`hippo version`)
- A minimal repro, ideally as a `go test` case

## Security

Please email security issues privately rather than filing a public
issue.

## Code of conduct

Be kind. Assume good faith. That's the whole thing.
