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
- `just oci [executor=podman] [tag=local]` — builds a Linux binary then a container image from `Dockerfile`

`fieldalignment` is enforced; if it complains about a struct, reorder fields rather than disabling the check.

Releases are produced by `goreleaser` (`.goreleaser.yml`) — do not hand-edit `dist/`.

## Architecture

Single-binary IMAP-to-IMAP sync tool. Five things to internalize before making changes.

### 1. Layering

```
cmd/imapsync-go/main.go         — urfave/cli/v3 wiring, version flags, signal-to-context
cmd/imapsync-go/commands/       — thin subcommand definitions (sync, show); they only declare flags and call into internal/app
internal/app/sync.go            — sync orchestration: load config, connect, build plan, confirm, dispatch to worker pool
internal/app/show.go            — show orchestration: parallel ListMailboxes via errgroup, formatted table
internal/app/worker.go          — syncWorker pool + runFolderSync helper consumed by sync.go
internal/client/client.go       — go-imap wrapper: Options, dial+TLS+rate-limit wiring, reconnect, generation-aware Select
internal/client/fetch.go        — single-pass FetchMessageMap (Message-Id+UID), StreamMessagesByUIDs (batched UID FETCH)
internal/client/mailbox.go      — MailboxExists / ListSubfolders / CreateMailbox via the cache
internal/client/cache.go        — per-Client mailboxCache (one LIST "" "*"), invalidated on Create
internal/client/append.go       — AppendMessage streaming the literal directly from the fetched body
internal/client/errors.go       — classifyError: Transient / Permanent / Throttled
internal/client/provider.go     — DetectProvider, used by sync.go to print quota warnings
internal/config/                — JSON+YAML config loader (extension-driven), validation
internal/progress/              — go-pretty progress writer/tracker abstraction (interfaces consumed by client)
internal/ratelimit/             — net.Conn wrapper applying golang.org/x/time/rate at the dialer
internal/utils/                 — `AskConfirm` and friends
```

`internal/client` defines minimal `ProgressWriter` / `ProgressTracker` interfaces it consumes; `internal/progress` implements them. Preserve this split when adding progress hooks — the alternative pulls `internal/progress` (which depends on go-pretty) into the client package.

`cmd/...` is the CLI entry point and depends on `internal/...`. Never import `cmd/` from `internal/`.

### 2. Idempotent sync via Message-Id matching (`internal/app/sync.go`)

The sync flow is **plan, confirm, execute** — not stream-as-you-go:

1. **Expand mappings**: each configured mapping is expanded with subfolders. `expandMappingsWithSubfolders` calls `client.ListSubfolders`, which serves the answer from the per-Client mailbox cache (one `LIST "" "*"` populates it on first connect).
2. **Delimiter reconciliation**: `folderDelimiter(path, serverDelimiter)` returns the detected delimiter and whether it matches the server's. On mismatch the user is prompted to rewrite delimiters in-place; `-y/--confirm` auto-accepts.
3. **Build sync plan** (`buildSyncPlan`): two goroutines run under an `errgroup` — one walks every src mapping calling `FetchMessageMap`, the other walks every dst mapping calling `MailboxExists` + `FetchMessageIDSet`. Trackers advance independently so a slow dst does not stall the src progress bar. After both sides complete a slot, `maybeDiff` fires (guarded by `atomic.Int32` reaching 2 + a `sync.Mutex` for race-detector cleanliness), computes the Message-Id diff, and immediately frees the maps to keep peak memory proportional to one folder at a time. All fetches are envelope-only via `BODY.PEEK[HEADER.FIELDS (MESSAGE-ID)] + UID`; bodies are not pulled at planning time. The "what would be synced" preview is printed and confirmed before any writes.
4. **Pre-create destination folders** for the active plans (sequential, with per-folder mutex on `Client.folderLocks`). New mailboxes are added back into the cache.
5. **Execute via worker pool**: `newSyncWorkerPool` builds N persistent workers (each owns one src + one dst `Client` for the entire sync). Plans are dispatched through a chan-of-workers semaphore. A single `progress.Writer` covers all plans, with one tracker per plan appended up front. Per-message UI updates are throttled to ~10 Hz.

Idempotency comes from the Message-Id diff. There is no UID-based bookkeeping; re-running on a partially completed sync is safe and resumes.

### 3. Connection resilience (`internal/client/client.go`)

- `safeCall(fn)` wraps every IMAP op. Errors are routed through `classifyError`: only `ClassTransient` triggers a reconnect-and-retry; `ClassThrottled` and `ClassPermanent` are surfaced to the caller. Add new IMAP operations through `safeCall` rather than calling `c.Client` methods directly.
- `Reconnect` enforces a minimum interval (`reconnectInterval = 10s`), exponential backoff up to `maxReconnectAttempts = 5`, and a longer cool-down (`throttledBackoff = 5m`) when the previous attempt got `ClassThrottled`.
- All `time.Sleep` calls in the reconnect path go through `sleepCtx`, which bails out on the internal cancel channel (closed by `Cancel()`) — Ctrl-C aborts immediately instead of waiting for the backoff to elapse.
- `Cancel` + `withCancel(ctx)` bridge `context.Context` to the underlying `*client.Client`: when the context is canceled, the connection is `Terminate()`d so blocked IMAP calls return immediately. Every long-running public method on `Client` should call `defer c.withCancel(ctx)()` and check `ctx.Err()` after each blocking step. `connectAndLogin` also does this and stores the unauthenticated `cli` in `c.c` before calling `Login` so `Cancel()` can `Terminate()` a stalled LOGIN.
- TCP keepalive is enabled at the dialer (`tcpKeepAlivePeriod = 30s` idle interval, via `newDialer`) so half-open sockets after a Wi-Fi flap are detected within the OS-default keepalive envelope rather than hanging indefinitely.
- `selectIfNeeded(cli, folder)` short-circuits when the folder is already selected on this connection. Reconnect bumps `connGen` and clears `selectedFolder`, so the cached path is never wrong after a session flip. **Use it instead of `cli.Select` directly** — `StreamMessagesByUIDs` saves one round-trip per 500-message batch this way.
- `StreamMessagesByUIDs` batches `UID FETCH` in chunks of `uidFetchBatchSize = 500` to avoid IMAP "Too long argument" errors. Don't unbatch.
- Folder creation uses a per-path `sync.Mutex` (`folderLocks` map) to make concurrent `CreateMailbox` calls for nested paths race-free; parents are created walking down the hierarchy.
- `c.pw` and `c.tracker` are `atomic.Pointer[progressWriterRef|progressTrackerRef]`. Read via `c.progressWriter()` / `c.progressTracker()`; do not access the fields directly.

### 4. Rate limiting (`internal/ratelimit`)

- `ratelimit.NewLimiter(bps)` returns `nil` when `bps <= 0` — the caller's nil-check is the "unlimited" signal.
- `ratelimit.New(net.Conn, read, write *rate.Limiter)` wraps a connection so reads and writes block until tokens are available. The wrapper is installed inside `client.Client.dialFn` after a successful dial, so go-imap is unaware of throttling.
- One limiter per direction is shared across every `Client` that talks to the same side (one `srcReadLim` for all source workers, one `dstWriteLim` for all destination workers). That makes the BPS budget a global cap, not per-connection.

### 5. Body fetch semantics

`StreamMessagesByUIDs` fetches via `BODY.PEEK[]` (`fullBodyPeekSection`), not `FetchRFC822`. The two are functionally equivalent for transferring the message but `FetchRFC822` sets `\Seen` on every message — a state mutation a sync tool must not introduce on the source. If you add new fetch sites, mirror this choice.

`AppendMessage` passes `msg.GetBody(...)` straight to `cli.Append` — no `io.ReadAll` round-trip — to halve peak RAM with large attachments. The trade-off is that a transient mid-APPEND error cannot be retried (the literal has been partially consumed); the next sync run picks the message up via the Message-Id diff.

## Conventions

- Configuration format is detected from the file extension: `.json` / `.yaml` / `.yml` (see `internal/config/config.New`). The CLI accepts either; sample files are `config.example.json` / `config.example.yaml`.
- Workers are clamped to `[1, 10]` in `config.New` — anything out of range falls back to `1`.
- Optional `rate_limit` block in config (`down_bps`, `up_bps`, `max_connections`); CLI flags `--bps-down`, `--bps-up`, `--max-connections` override it.
- `client.New(ctx, addr, user, pass, Options{...})` — keep call-sites using the `Options` struct rather than positional bools, and pass the parent context so the dial honours cancellation.
- Dependencies are vendored under `vendor/`. Build with whatever the host Go provides; the module pins `go 1.26`.

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
