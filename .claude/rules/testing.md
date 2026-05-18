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

### `internal/client` deliberate exclusions (added with P3.1 fake-server suite)

- **`client.New` full constructor** — covered transitively by every fake-server test through `connectAndLogin`. The TLS dial path is delegated to stdlib and covered at integration level.
- **`Client.SetPrefix`, `Client.GetDelimiter`** — trivial one-liners; observable in all fake-server tests that set a delimiter.
- **`Client.SetProgressTracker`** — symmetric with `SetProgressWriter`; both are nil-and-non-nil atomic.Pointer stores already exercised by `SetProgressWriter` tests.
- **`realSleepCtx`** — production sleep implementation. Tests substitute the package-level `sleepCtx` var; the real implementation is stdlib `time.NewTimer` and its correctness is delegated to stdlib.
- **`Client.getFolderSize`** — requires the fake server to emit `RFC822.SIZE` in FETCH responses, which it currently does not. Used only by `show`; observable in smoke tests.
- **`AppendMessage` mid-APPEND transient retry** — the streaming-literal design (`msg.GetBody(...)` handed straight to `cli.Append`, no `io.ReadAll`) means the literal has been partially consumed before a mid-APPEND error is detected. There is nothing to retry: the literal stream is exhausted. Idempotency is recovered by the next sync run's Message-Id diff. This path is architecturally untestable in isolation without a mock that simulates a partial-write failure mid-literal, which would not test real go-imap behaviour.
- **`safeCall` post-reconnect `isCancelled()` branch** — exercising it requires `Cancel()` to race between reconnect-completion and retry-fn dispatch. The window is a single `c.isCancelled()` call; deterministic coverage would require a production hook (e.g. an injected callback between reconnect and retry), which adds test-only complexity with no safety benefit beyond the existing `Cancel` interrupt tests.
- **`connectAndLogin` `imapclient.New` failure branch** (`internal/client/client.go:369-373`) — fires only when the server accepts the TCP connection but closes it before sending the IMAP greeting. The branch is `_ = conn.Close(); return err` — no imapsync logic. Exercising it would test go-imap's greeting-read error handling, which is delegated to that library. The `stallingLoginHandler` approach already covers the normal greeting path; a "silent close" handler buys no coverage of imapsync-owned code.

### `internal/app` deliberate exclusions (added with B.1a fake-server suite)

- **`ActionSync`, `ActionShow`** — top-level urfave/cli action entry points; require a fully running config and live IMAP connections. Tested implicitly through their sub-functions (`buildSyncPlan`, `expandMappingsWithSubfolders`, `runFolderSync`, `newSyncWorkerPool`), each at 80–100% line coverage.
- **`printAccountInfo`** — go-pretty table renderer hitting `os.Stdout` directly. A meaningful test requires either a refactor to take `io.Writer` (out of scope for B.1a) or stdout capture; neither buys coverage of real behaviour because the function is a thin layout wrapper.

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
