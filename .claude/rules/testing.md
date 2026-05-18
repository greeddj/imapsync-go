# Testing rules

## What "covered" means here

- A change is covered when every behavioural branch the change introduced is exercised by a test that would fail if the branch broke.
- Coverage % is a guideline, not a gate. Coverage of error paths and invariants matters more than total %.
- `go test -cover -coverprofile=cover.out ./...` and `go tool cover -func=cover.out` are the source of truth.

## What we test

- **`internal/config`** — JSON+YAML loading (both extensions), validation, defaults (worker clamp, rate-limit nil case), env interpolation.
- **`internal/client`** — error classification, cache invalidation on create, fetch batching boundaries, reconnect generation bump, Select-cache short-circuit, AppendMessage streaming (no ReadAll).
- **`internal/ratelimit`** — token starvation, nil-limiter pass-through, read+write directions independent.
- **`internal/utils`** — `AskConfirm` and friends; trivial but they cover the `-y/--confirm` flow.
- **`internal/app`** — sync plan diff (Message-Id intersection), subfolder expansion, delimiter reconciliation, worker pool dispatch.
- **`internal/progress`** — interface compliance against the contract `internal/client` expects.

## What we deliberately don't test

If something is hard to test, it must be either tested at integration level or documented as a deliberate exclusion in `CLAUDE.md` with reasoning. Examples of acceptable exclusions:

- The TLS handshake itself (delegated to stdlib).
- `goreleaser` output (delegated to goreleaser).
- The `urfave/cli/v3` flag parsing wiring in `cmd/imapsync-go/main.go` (declarative; failure mode is a CLI parse error from the library).

If the tester finds an untested branch that does not match an existing exclusion, the choice is:

1. Add a test (preferred).
2. If the branch is genuinely untestable in isolation (e.g. network races), propose an integration test or add it to the exclusions list with reasoning.

## Test commands

- `just test` — full suite.
- `go test ./internal/client -run TestFunc` — single test.
- `go test -race ./...` — race detector. Run before declaring done on anything that touches goroutines.
- `go test -cover ./internal/<pkg>` — coverage for one package.
- `go test -coverprofile=cover.out ./... && go tool cover -func=cover.out | sort -k3 -n` — coverage report sorted by % (find the holes fast).

## When a test fails

- Do not "fix" by adjusting the assertion to match observed behaviour. Find the regression.
- If the failure is in a test the developer just wrote and the new code is correct, the test is wrong — fix the test, explain in the developer→architect report.
- If the failure is in a pre-existing test, stop. Report to architect. Do not modify pre-existing tests without explicit instruction.

## Race detector

`just run` runs `go run -race`. If you introduce concurrency, run `go test -race ./...` locally and report the result.
