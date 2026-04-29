---
name: imapsync-test
description: Run tests for the imapsync-go Go repo via Justfile or direct go test. Use when the user asks to run tests or verify a fix. Covers full-repo runs and tight package-level loops. Do NOT use for lint/static analysis (see imapsync-check) or builds (see imapsync-build).
---

# imapsync-go — Tests

## When to use

- "прогони тесты" / "run tests" / "verify fix"
- Tight loop on a single package after a fix

## Commands

| Intent | Command |
|---|---|
| Full test run (matches CI) | `just test` |
| Tight loop on one package | `go test ./internal/<pkg>` |
| Single test by name | `go test ./internal/<pkg> -run TestName` |
| Race detector | `go test -race ./...` |
| Full repo, plain | `go test ./...` |

`just test` is just `go test ./...` — no extra `check` chain on top of it. If you want static analysis as well, run `just check` separately (or `just run`, which chains `check + lint + test`).

## Where tests live

Tests are colocated with source. Currently:

- `internal/config/config_test.go`
- `internal/utils/utils_test.go`

Other packages (`internal/client`, `internal/app`, `internal/progress`, `cmd/...`) are not currently covered. New tests for those go next to the source.

## Repo-specific gotchas

- **Vendored project.** `vendor/` is committed; `go test ./...` resolves through it automatically (Go's default when `vendor/` exists at module root). No `-mod vendor` flag needed in commands.
- **Network-touching code.** `internal/client` talks to a real IMAP server in production. Don't write tests that dial real hosts in `go test ./...` — use a fake `dialFn` (the `Client` struct exposes one for exactly this reason) or stub at a higher level.
- **Worker concurrency.** Tests that exercise `internal/app` sync-plan execution can be sensitive to `cfg.Workers` and the chunked goroutine pattern in `sync.go`. Set `Workers: 1` for deterministic ordering.
- **Context cancellation.** Many `internal/client` methods bridge ctx to the IMAP connection via `withCancel`. Tests that expect cancellation to unblock a fetch must actually cancel the ctx — closing a channel won't propagate.

## Workflow

1. Reproduce failure with `go test ./<failing-pkg>` (fast).
2. Fix → re-run package test.
3. Before handoff: `just test` for the full sweep, then `just check` + `just lint` if you also want static gates.
