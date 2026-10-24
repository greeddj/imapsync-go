# Testing rules

## What "covered" means here

- A change is covered when every behavioural branch the change introduced is exercised by a test that would fail if the branch broke.
- Coverage % is a guideline, not a gate. Coverage of error paths and invariants matters more than total %.
- `go test -cover -coverprofile=cover.out ./...` and `go tool cover -func=cover.out` are the source of truth.

## What we test

- **`internal/config`** — JSON+YAML loading (both extensions), validation, defaults (worker clamp, rate-limit nil case), env interpolation.
- **`internal/client`** - error classification, mailbox cache update on create (no new LIST), fetch batching boundaries, reconnect generation bump, Select-cache short-circuit, AppendMessage streaming (no ReadAll).
  Error paths and cancellation run against scripted fake servers. These tests cover:
  - a refusal of each command;
  - a context that ends while LIST, STATUS, FETCH, CREATE or APPEND waits for a reply;
  - CRAM-MD5 failure and success;
  - `withCancel` after stop, and shared limiters that draw from one budget.
- **`internal/ratelimit`** — token starvation, nil-limiter pass-through, read+write directions independent.
- **`internal/utils`** - `AskConfirm` and its helpers. The tests cover the `-y/--confirm` flow. `AskConfirm` reads each answer from one shared buffered reader.
- **`internal/app`** - sync plan diff (Message-Id intersection), subfolder expansion, delimiter reconciliation, worker pool dispatch.
  The `runSync` and `runShow` tests run end to end against a fake IMAP server that holds mailboxes in memory. These tests cover:
  - the plan-confirm-execute order, and the rule that `--quiet` needs `--confirm`;
  - an idempotent second run, and the delimiter rewrite on both sides;
  - nested folder creation, and one rate-limit budget per direction that all workers share;
  - a read-only source (no write commands, no `\Seen` change);
  - a cancel at each boundary between sync phases;
  - the refusal paths (login, LIST, EXAMINE, CREATE, APPEND);
  - the `ActionSync` and `ActionShow` adapters, which run through a urfave/cli root (the TLS-only dial and the config load errors).
- **`internal/progress`** — interface compliance against the contract `internal/client` expects.

## What we deliberately don't test

If a behaviour is hard to test, test it at integration level. If you cannot do that, record it in this file as a deliberate exclusion and give the reason. These are examples of acceptable exclusions:

- The TLS handshake itself (delegated to stdlib).
- `goreleaser` output (delegated to goreleaser).
- The `urfave/cli/v3` flag parsing wiring in `cmd/imapsync-go/main.go` (declarative; failure mode is a CLI parse error from the library).

### Deliberate exclusions in `internal/client`

- **`client.New`** - the `Test_New_*` tests cover CRAM-MD5, dial failure, LIST refusal, cancel during LOGIN and shared limiters. Only `internal/app` tests cover two branches: the `tls.Dialer` path and the default dial timeout. The `tls.Dialer` test uses a plaintext server, so a handshake error is expected. The standard library handles a successful TLS handshake, so no test covers it.
- **`Client.SetPrefix`, `Client.GetDelimiter`** — trivial one-liners; observable in all fake-server tests that set a delimiter.
- **`Client.SetProgressTracker`** — symmetric with `SetProgressWriter`; both are nil-and-non-nil atomic.Pointer stores already exercised by `SetProgressWriter` tests.
- **`realSleepCtx`** — production sleep implementation. Tests substitute the package-level `sleepCtx` var; the real implementation is stdlib `time.NewTimer` and its correctness is delegated to stdlib.
- **`AppendMessage` mid-APPEND transient retry** - the code hands `msg.GetBody(...)` straight to `cli.Append`, with no `io.ReadAll`. The client consumes part of the literal before it detects a mid-APPEND error, so nothing remains to retry. The next sync run recovers through the Message-Id diff. A test of this path needs a mock that fails in the middle of a literal. Such a mock does not test real go-imap behaviour. Two nearby paths have tests:
  - `Test_AppendMessage_repairsConnectionAfterServerLogout` covers the reconnect after a failed APPEND;
  - `Test_AppendMessage_whenContextExpiresMidAppend` covers a context that ends mid-APPEND.
- **`safeCall` post-reconnect `isCancelled()` branch** — exercising it requires `Cancel()` to race between reconnect-completion and retry-fn dispatch. The window is a single `c.isCancelled()` call; deterministic coverage would require a production hook (e.g. an injected callback between reconnect and retry), which adds test-only complexity with no safety benefit beyond the existing `Cancel` interrupt tests.
- **`connectAndLogin` `imapclient.New` failure branch** (the `conn.Close(); return err` branch after the greeting read in `connectAndLogin`) - the branch runs only when the server accepts the TCP connection but closes it before it sends the IMAP greeting. The branch has no imapsync logic. A test of it checks the go-imap greeting-read error handling, which the library owns. The `stallingLoginHandler` test already covers the normal greeting path. A handler that closes the connection in silence adds no coverage of imapsync code.
- **Post-success `ctx.Err()` re-checks** - each check runs after a call succeeds. `withCancel` terminates the connection asynchronously, so the outcome at that boundary is a race. The checks are in these places:
  - `StreamMessagesByUIDs`, at the top of the second and later batches;
  - `CreateMailbox`, after the parent walk;
  - `createParentFolders`, at the top of the loop;
  - `ListMailboxes`, after the LIST;
  - `getFolderSize`, after success.
- **`StreamMessagesByUIDs` `cbErr` check at the top of the batch loop** - unreachable code, not untested code. The check after each batch returns `cbErr` before the next batch starts, so the top check never sees a non-nil value.
- **`createParentFolders` `hasMailbox` error branch** - unreachable code, not untested code. `client.New` loads the mailbox cache and no code resets it, so `hasMailbox` never returns an error.

### Deliberate exclusions in `internal/app`

- **`runFolderSync` post-append `ctx.Err()` check** - The check stops the loop when an APPEND finishes after the context is canceled. `withCancel` registers a `context.AfterFunc` that calls `Cancel()`, and `Cancel()` terminates the connection asynchronously. An APPEND that finishes at the same time can return nil, not an error. This is a race window, and the test has no deterministic hook to reach it. The `(synced, errors)` result is the same with or without the check. The check saves at most one extra iteration.
- **`app.traceTracker`** - a diagnostic that writes to stderr. It is active only when `IMAPSYNC_TRACE` is set.
- **`ActionSync` verbose `Fetching config...` line** - `ActionSync` writes it to `os.Stdout` before the config loads. It is the only output that does not go through the injected writer.
- **Boundary `ctx.Err()` re-checks** - a cancel must land between two statements with no observable event, so a deterministic test is not possible. `TestRunSync_When_CanceledAtPhaseBoundary_Then_NothingMoreIsSent` and `TestRunSync_When_CanceledDuringAppend_Then_NoFurtherPlanIsStarted` test the end-to-end property. The re-checks are in these places:
  - `runSync`, after the connect errgroup;
  - `runSync`, after the scan progress writer stops;
  - `runSync`, at the top of the dispatch loop;
  - `expandMappingsWithSubfolders`, in the per-mapping loop and the per-subfolder loop.
- **`expandMappingsWithSubfolders` non-context `getSubfolders` error branch** - unreachable code, not untested code. `client.New` always loads the mailbox cache, so `getSubfolders` fails only when the context ends.

### Deliberate exclusions in `cmd/imapsync-go`

- **`main` and `run` in `cmd/imapsync-go/main.go`** - the signal-to-context wiring, the exit-code mapping and `os.Exit` need a subprocess and real signals. The tests in `cmd/imapsync-go/commands` cover the command tree. A test sends SIGQUIT to its own process to cover `installQuitDump`, with an injected exit function.
- **`helpers.latestTag`** - the function makes a live call to the GitHub API. The tests skip it through `IMAPSYNC_OFFLINE` or `CI`. The tests cover only the offline return and the `Version` format.

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
