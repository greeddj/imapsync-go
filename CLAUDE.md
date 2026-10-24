# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Common commands

The project uses [`just`](https://github.com/casey/just) as its task runner. The `Justfile` is the source of truth for tooling invocations.

- `just deps` — `go mod tidy && go mod vendor` (deps are vendored; run after touching `go.mod`)
- `just lint` — `golangci-lint run ./... --timeout=5m`
- `just test` — `go test ./...` (run a single test with `go test ./internal/config -run TestName`)
- `just check` — runs `go vet`, `staticcheck`, `govulncheck`, and `fieldalignment` (these are declared as Go `tool` directives in `go.mod`, invoked via `go tool`)
- `just run` — `check` + `lint` + `test`, then `go run -race ./cmd/imapsync-go/main.go`
- `just build` — produces `dist/imapsync-go` (CGO disabled, trimpath, version metadata injected via `-ldflags -X main.{Version,Commit,Date,BuiltBy}`)
- `just build_linux` — Linux/amd64 cross-build
- `just oci [executor=podman] [tag=local]` - cross-builds the Linux binary, stages it under `dist/oci/linux/amd64/dist/`, then builds a `linux/amd64` image from `Dockerfile` with `dist/oci` as the build context

`fieldalignment` is enforced; if it complains about a struct, reorder fields rather than disabling the check.

Releases are produced by `goreleaser` (`.goreleaser.yml`) — do not hand-edit `dist/`.

The container image is built from the per-platform tree `dockers_v2` assembles: each binary lands at `<goos>/<goarch>/dist/imapsync-go` in the build context, following the `dist/` prefix in the build's `binary:` value, and `Dockerfile` resolves it through `ARG TARGETPLATFORM`. `just oci` reproduces the same layout under `dist/oci`. `.goreleaser.yml`, `Dockerfile` and the Justfile `oci` target share that path shape; moving one requires moving the other two. Multi-platform images are assembled in the publish phase, so on a tagged release `--skip=publish` skips the image entirely; `goreleaser release --snapshot` still builds them locally, which is how to build one by hand.

## Architecture

Single-binary IMAP-to-IMAP sync tool. Five things to internalize before making changes.

### 1. Layering

```
cmd/imapsync-go/main.go         — urfave/cli/v3 wiring, version flags, signal-to-context
cmd/imapsync-go/commands/       — thin subcommand definitions (sync, show); they only declare flags and call into internal/app
internal/app/sync.go            — sync orchestration: ActionSync reads flags and config, runSync connects, builds the plan, confirms, dispatches to worker pool
internal/app/show.go            — show orchestration: ActionShow reads flags and config, runShow runs parallel ListMailboxes via errgroup, formatted table
internal/app/worker.go          — syncWorker pool + runFolderSync helper consumed by sync.go
internal/client/client.go       - go-imap wrapper: Options, dial+TLS, guardConn wiring (watchdog, rate limit), bounded greeting and logout, reconnect, generation-aware Select
internal/client/watchdog.go     - idleConn: inactivity watchdog for commands in flight
internal/client/fetch.go        - single-pass FetchMessageMap (Message-Id+UID), StreamMessagesByUIDs (batched UID FETCH, a retry fetches only the UIDs not yet delivered)
internal/client/mailbox.go      — MailboxExists / ListSubfolders / CreateMailbox via the cache
internal/client/cache.go        — per-Client mailboxCache (one LIST "" "*"), a created mailbox is added to it
internal/client/append.go       — AppendMessage streaming the literal directly from the fetched body
internal/client/errors.go       — classifyError: Transient / Permanent / Throttled
internal/client/provider.go     — DetectProvider, used by sync.go to print quota warnings
internal/config/                — JSON+YAML config loader (extension-driven), validation
internal/progress/              — go-pretty progress writer/tracker abstraction (interfaces consumed by client)
internal/ratelimit/             - net.Conn wrapper applying golang.org/x/time/rate, installed by guardConn
internal/utils/                 — `AskConfirm(ctx, in *bufio.Reader, out io.Writer, prompt)` and friends
```

`ActionSync` and `ActionShow` are thin CLI adapters. They pass `client.Options{UseTLS: true}`, `os.Stdout` and `os.Stdin` to `runSync` / `runShow`. Tests pass plaintext options, a buffer and a scripted reader to dial the fake IMAP servers. Do not add a config, flag or environment switch that disables TLS.

`internal/client` defines minimal `ProgressWriter` / `ProgressTracker` interfaces it consumes; `internal/progress` implements them. Preserve this split when adding progress hooks — the alternative pulls `internal/progress` (which depends on go-pretty) into the client package.

`cmd/...` is the CLI entry point and depends on `internal/...`. Never import `cmd/` from `internal/`.

### 2. Idempotent sync via Message-Id matching (`internal/app/sync.go`)

The sync flow is **plan, confirm, execute** — not stream-as-you-go:

1. **Expand mappings**: each configured mapping is expanded with subfolders. `expandMappingsWithSubfolders` calls `client.ListSubfolders`, which serves the answer from the per-Client mailbox cache (one `LIST "" "*"` populates it on first connect).
2. **Delimiter reconciliation**: `folderDelimiter(path, serverDelimiter)` returns the detected delimiter and whether it matches the server's. On mismatch the user is prompted to rewrite delimiters in-place; `-y/--confirm` auto-accepts.
3. **Build sync plan** (`buildSyncPlan`): two goroutines run under an `errgroup` — one walks every src mapping calling `FetchMessageMap`, the other walks every dst mapping calling `MailboxExists` + `FetchMessageIDSet`. Trackers advance independently so a slow dst does not stall the src progress bar. After both sides complete a slot, `maybeDiff` fires (guarded by `atomic.Int32` reaching 2 + a `sync.Mutex` for race-detector cleanliness), computes the Message-Id diff, and immediately frees the maps to keep peak memory proportional to one folder at a time. All fetches are envelope-only via `BODY.PEEK[HEADER.FIELDS (MESSAGE-ID)] + UID`; bodies are not pulled at planning time. The "what would be synced" preview is printed and confirmed before any writes. `--quiet` hides the preview and the prompt, so `runSync` refuses `--quiet` without `-y/--confirm` before it connects to a server.
4. **Pre-create destination folders** for the active plans (sequential, with per-folder mutex on `Client.folderLocks`). New mailboxes are added back into the cache.
5. **Execute via worker pool**: `newSyncWorkerPool` builds N persistent workers (each owns one src + one dst `Client` for the entire sync). Plans are dispatched through a chan-of-workers semaphore. A single `progress.Writer` covers all plans, with one tracker per plan appended up front. Per-message UI updates are throttled to ~10 Hz. At the end, `logoutAll` logs out all workers at the same time, then the two planning clients, so a silent server costs at most two logout limits.

Idempotency comes from the Message-Id diff. There is no UID-based bookkeeping; re-running on a partially completed sync is safe and resumes.

### 3. Connection resilience (`internal/client/client.go`, `internal/client/watchdog.go`)

- `safeCall(fn)` wraps every IMAP op. Errors are routed through `classifyError`: only `ClassTransient` triggers a reconnect-and-retry; `ClassThrottled` and `ClassPermanent` are surfaced to the caller. Add new IMAP operations through `safeCall` rather than calling `c.Client` methods directly. `safeCall` runs each attempt of `fn` through `call`, which also opens one watchdog span.
- `Reconnect` enforces a minimum interval (`reconnectInterval = 10s`), exponential backoff up to `maxReconnectAttempts = 5`, and a longer cool-down (`throttledBackoff = 5m`) when the previous attempt got `ClassThrottled`.
- All `time.Sleep` calls in the reconnect path go through `sleepCtx`, which bails out on the internal cancel channel (closed by `Cancel()`) — Ctrl-C aborts immediately instead of waiting for the backoff to elapse.
- `Cancel` + `withCancel(ctx)` bridge `context.Context` to the underlying `*client.Client`: when the context is canceled, the connection is `Terminate()`d so blocked IMAP calls return immediately. `withCancel` registers `Cancel` through `context.AfterFunc`. After the returned stop function runs, a later cancellation does not change the client. Every long-running public method on `Client` should call `defer c.withCancel(ctx)()` and check `ctx.Err()` after each blocking step. `connectAndLogin` also does this and stores the unauthenticated `cli` in `c.c` before calling `Login` so `Cancel()` can `Terminate()` a stalled LOGIN.
- `connectAndLogin` checks for cancellation again after it stores the new session in `c.c`. `Cancel` sets the cancelled flag before it reads `c.c`, so one side always sees the other. Thus a `Cancel()` between the greeting and the store stops the login: `connectAndLogin` does not send LOGIN and returns `context.Canceled`. During the dial and the greeting, `ctx` ends the wait: the caller context in `New`, or the internal context that `Cancel()` ends in `reconnect`.
- `loadMailboxCache` (the first `LIST "" "*"` in `New`) runs under `withCancel(ctx)` and checks `ctx.Err()`, so Ctrl-C stops `New` during that LIST. When a cancellation causes a failure, `connectAndLogin` and `loadMailboxCache` return `ctx.Err()`, or `context.Canceled` after a `Cancel()`. `cmd/imapsync-go/main.go` then prints `Cancelled.` and exits with code 130. It does not print a connection error.
- TCP keepalive is enabled at the dialer (`tcpKeepAlivePeriod = 30s` idle interval, via `newDialer`) so half-open sockets after a Wi-Fi flap are detected within the OS-default keepalive envelope rather than hanging indefinitely.
- `connectAndLogin` calls `greet(ctx, conn)` in place of a bare `imapclient.New`. `greet` closes the connection when `ctx` ends, or when the greeting does not arrive within `dialTimeout` (default `defaultDialTimeout = 30s`). The limit also covers the CAPABILITY probe that go-imap sends when the greeting has no capability list. A timer closes the socket, not `conn.SetDeadline`, because that probe clears connection deadlines. The timeout error wraps `os.ErrDeadlineExceeded`, so `classifyError` returns `ClassTransient` and `reconnect` retries it. `greet` wraps all other failures with the prefix `imap greeting:`.
- `logout(cli)` sends LOGOUT and waits a maximum of `logoutTimeout = 2s`. Each Client stores this limit as `logoutDur`, and a zero value means no limit (test fixtures only). If the server does not answer, `logout` terminates the connection and returns an error that wraps `errLogoutTimeout`. `logout` always closes the socket before it returns, because go-imap closes the socket only when the server sends BYE. `logout` does not use `safeCall`, so a logout never triggers a reconnect. `Logout()`, `reconnect` (when it discards the old session under `c.mu`) and the cleanup after a failed LOGIN or AUTHENTICATE call `logout`.
- `idleConn` (`internal/client/watchdog.go`) is the inactivity watchdog for commands in flight. `idleTimeout = 5 * time.Minute` is its limit. Each Client stores the limit as `idleDur`, and `New` sets it. A zero value turns the watchdog off (test fixtures only), like `logoutDur`. No config field or CLI flag sets the limit.
- `connectAndLogin` calls `guardConn` after `dialFn` and before `greet`. `guardConn` wraps the dialed transport in `idleConn`. When a limiter is set, it then wraps the result in `ratelimit.Conn`. So the watchdog is the innermost wrapper, and a wait for rate-limit tokens does not run inside it. `dialFn` only dials TCP or TLS.
- `c.idle` holds the watchdog of the session in `c.c`. `connectAndLogin` stores `c.idle` before `c.c`, so a reader that sees the new session also sees its watchdog. `call` loads `c.idle` again on each attempt, so a retry after a reconnect uses the new watchdog.
- A span is the time while a command is in flight. The watchdog counts only time inside a span. `safeCall` runs each attempt of `fn` through `call`, which opens one span. `AppendMessage` opens one span around `cli.Append`. `connectAndLogin` opens one span around LOGIN or AUTHENTICATE, for the first connect and for each reconnect. A go-imap call outside `safeCall` must open its own span.
- The watchdog does not cover the greeting or LOGOUT. The greeting keeps its `dialTimeout` limit, and LOGOUT keeps `logoutTimeout`.
- The watchdog closes the transport when all these conditions are true:
  - a span is open;
  - at least one `Read` or `Write` is blocked on the transport;
  - no byte moved in either direction for `idleDur`.
- Any byte in either direction resets the idle time to zero. The watchdog limits inactivity, not total time, so a transfer that moves bytes is never cut. A session that is idle between commands is never closed.
- When the watchdog trips, the pending call fails. `idleErr` wraps its error with `errIdleTimeout` (`no data from server`) and `os.ErrDeadlineExceeded`, so `classifyError` returns `ClassTransient`. `safeCall` then reconnects and retries once, inside the `reconnectInterval`, `maxReconnectAttempts` and backoff limits. Example error text: `imap: connection closed: no data from server within 5m0s: i/o timeout`. Before the reconnect, `noteIdleTrip` writes one line with the reason to the progress writer, or to the tracker when no writer is set. It does this also without `--verbose`, because a trip is rare.
- A LOGIN that gets no reply during a reconnect trips too, so `reconnect` never holds `c.mu` without a limit. `AppendMessage` returns the error and repairs the connection (existing behaviour). The next run copies that message through the Message-Id diff.
- A trip never calls `Cancel` and never sets the cancelled flag. Each cancellation check runs before the error handling. So Ctrl-C returns `context.Canceled`, also after a trip.
- `StreamMessagesByUIDs` ends the span while `onMessage` runs, because that time is our own work (usually the destination APPEND). At the end of a batch the source is done and silent. Code that runs caller work inside a `safeCall` closure must also end the span, and open it again after. A wait for read tokens runs outside `idleConn`, so it never counts.
- Accepted edge: the watchdog cannot see a wait for write tokens. If one literal write (up to 32 KiB) waits `idleTimeout` or more for tokens, the watchdog trips. This needs an upload limit below about 110 bytes per second for each worker that uploads at the same time. The limiter is shared between the workers.
- Each connection has one timer, and `Close` stops it. Without an open span the timer does not run. A timer that is pending when a span ends fires once, finds no span and stops.
- `selectIfNeeded(cli, folder)` short-circuits when the folder is already selected on this connection. Reconnect bumps `connGen` and clears `selectedFolder`, so the cached path is never wrong after a session flip. **Use it instead of `cli.Select` directly** — `StreamMessagesByUIDs` saves one round-trip per 500-message batch this way.
- `StreamMessagesByUIDs` batches `UID FETCH` in chunks of `uidFetchBatchSize = 500` to avoid IMAP "Too long argument" errors. Don't unbatch.
- A retry of a `StreamMessagesByUIDs` batch (after a transient error, through `safeCall`) fetches only the UIDs whose body `onMessage` did not receive. If `onMessage` received all of them, the retry sends no SELECT or FETCH command. The code does not deliver a second response with the body for the same UID. So a retry creates no duplicate APPEND on the destination. A response without a body does not mark its UID.
- Folder creation uses a per-path `sync.Mutex` (`folderLocks` map) to make concurrent `CreateMailbox` calls for nested paths race-free; parents are created walking down the hierarchy.
- `c.pw` and `c.tracker` are `atomic.Pointer[progressWriterRef|progressTrackerRef]`. Read via `c.progressWriter()` / `c.progressTracker()`; do not access the fields directly.
- go-imap writes every read error to stderr, including the error that each `Terminate` causes. `connectAndLogin` calls `muteErrorLog`, which sends that log to `io.Discard` unless `--verbose` is set. The function changes the output of the existing `*log.Logger` with `SetOutput`, because the reader goroutine already runs and a field write would race.

**Known limitation - Ctrl+C blocked inside a literal write.** go-imap sends a synchronizing literal in two steps. First the client sends `{N}\r\n`. Then it waits on `<-w.continues` (`vendor/github.com/emersion/go-imap/write.go:158`) for the `+` reply of the server. Only the response handlers of the reader goroutine send on that channel.

If the socket closes before the server sends `+` or a tagged reply, the reader goroutine ends and the wait never ends. These events close the socket: `Cancel()` / `Terminate()`, TCP keepalive expiry (`tcpKeepAlivePeriod = 30s` idle interval, several minutes on macOS and Linux) and a watchdog trip. The goroutine that writes the literal stays blocked until the process exits.

The watchdog does not change this limitation. It closes the socket in this window, but a closed socket does not release the wait. Only the worker inside the in-flight operation is affected, and other workers continue.

Users can press Ctrl+\ (SIGQUIT) to get an immediate goroutine dump and exit. A fix needs one of two changes. The first patches `writeLiteral` in the vendored go-imap, to `select` on a done channel with the `continues` receive. The second moves to go-imap v2.

### 4. Rate limiting (`internal/ratelimit`)

- `ratelimit.NewLimiter(bps)` returns `nil` when `bps <= 0` — the caller's nil-check is the "unlimited" signal.
- `ratelimit.New(net.Conn, read, write *rate.Limiter)` wraps a connection so reads and writes block until tokens are available. `guardConn` installs the wrapper in `connectAndLogin`, after `dialFn` returns and outside `idleConn`. So go-imap is unaware of throttling, and the watchdog never counts a wait for read tokens (see Section 3).
- One limiter per direction is shared across every `Client` that talks to the same side (one `srcReadLim` for all source workers, one `dstWriteLim` for all destination workers). That makes the BPS budget a global cap, not per-connection.

### 5. Body fetch semantics

`StreamMessagesByUIDs` fetches via `BODY.PEEK[]` (`fullBodyPeekSection`), not `FetchRFC822`. The two are functionally equivalent for transferring the message but `FetchRFC822` sets `\Seen` on every message — a state mutation a sync tool must not introduce on the source. If you add new fetch sites, mirror this choice.

`AppendMessage` passes `msg.GetBody(...)` straight to `cli.Append` — no `io.ReadAll` round-trip — to halve peak RAM with large attachments. The trade-off is that a transient mid-APPEND error cannot be retried (the literal has been partially consumed); the next sync run picks the message up via the Message-Id diff.

## Conventions

- Configuration format is detected from the file extension: `.json` / `.yaml` / `.yml` (see `internal/config/config.New`). The CLI accepts either; sample files are `config.example.json` / `config.example.yaml`.
- Workers are clamped to `[1, 10]` in `config.New`. A value out of range changes to the nearest limit.
- Optional `rate_limit` block in config (`down_bps`, `up_bps`, `max_connections`); CLI flags `--bps-down`, `--bps-up`, `--max-connections` override it.
- `client.New(ctx, addr, user, pass, Options{...})` — keep call-sites using the `Options` struct rather than positional bools, and pass the parent context so the dial honours cancellation.
- Dependencies are vendored under `vendor/`. The module requires Go 1.27.3 or newer; an older host toolchain is refused unless `GOTOOLCHAIN` is left on `auto` to fetch one.

## Multi-agent workflow

This repo ships a `.claude/` directory with five role-specific subagents and a shared rule set. They exist to keep non-trivial changes architecturally honest:

- **architect** (opus) — gatekeeper. Validates the brief, may reject, orchestrates the rest.
- **developer** (sonnet) — implements within a precise brief; writes WHY-comments and unit tests.
- **tester** (sonnet) — runs the suite, audits coverage and invariants, proposes deliberate exclusions.
- **security** (opus) — CVE + attack-surface audit; reasons about combined risk.
- **tech-writer** (sonnet) — comment hygiene + CLAUDE.md / README.md / `config.example.*` sync.

Entry points:

- `/architect <task>` — hand the task to the architect (the architect decides whether to proceed).
- `/workflow <task>` — full architect-led delegation chain (preferred for non-trivial changes).
- `/dev`, `/qa`, `/sec`, `/docs` — invoke a single agent directly (use when you know which step you need).
- `/deps`, `/lint`, `/test`, `/check`, `/fix`, `/build`, `/build-linux`, `/oci`, `/run` — thin wrappers over the `Justfile` targets.

The rules each agent reads live in `.claude/rules/`:

- [workflow.md](.claude/rules/workflow.md) — delegation protocol (architect-led + fallback).
- [architecture.md](.claude/rules/architecture.md) — layering and sync-flow invariants the architect enforces.
- [go-style.md](.claude/rules/go-style.md) — Go style, stricter than community defaults.
- [testing.md](.claude/rules/testing.md) — what is tested, what is deliberately excluded.
- [security.md](.claude/rules/security.md) — standing security checklist + combined-surface checks.
- [docs.md](.claude/rules/docs.md) — comment + doc audit rules.

Skills in `.claude/skills/` (auto-loaded by agents when relevant): `idiomatic-go-review`, `imap-sync-correctness`, `coverage-audit`, `security-audit`, `doc-sync`.
