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
3. **Build sync plan** (`buildSyncPlan`): for every mapping, `FetchMessageMap` (src) and `FetchMessageIDSet` (dst) run in parallel — both are envelope-only via `BODY.PEEK[HEADER.FIELDS (MESSAGE-ID)] + UID`. The diff produces sorted `[]uint32` source UIDs per plan; bodies are not pulled at planning time. The "what would be synced" preview is printed and confirmed before any writes.
4. **Pre-create destination folders** for the active plans (sequential, with per-folder mutex on `Client.folderLocks`). New mailboxes are added back into the cache.
5. **Execute via worker pool**: `newSyncWorkerPool` builds N persistent workers (each owns one src + one dst `Client` for the entire sync). Plans are dispatched through a chan-of-workers semaphore. A single `progress.Writer` covers all plans, with one tracker per plan appended up front. Per-message UI updates are throttled to ~10 Hz.

Idempotency comes from the Message-Id diff. There is no UID-based bookkeeping; re-running on a partially completed sync is safe and resumes.

### 3. Connection resilience (`internal/client/client.go`)

- `safeCall(fn)` wraps every IMAP op. Errors are routed through `classifyError`: only `ClassTransient` triggers a reconnect-and-retry; `ClassThrottled` and `ClassPermanent` are surfaced to the caller. Add new IMAP operations through `safeCall` rather than calling `c.Client` methods directly.
- `Reconnect` enforces a minimum interval (`reconnectInterval = 10s`), exponential backoff up to `maxReconnectAttempts = 5`, and a longer cool-down (`throttledBackoff = 5m`) when the previous attempt got `ClassThrottled`.
- All `time.Sleep` calls in the reconnect path go through `sleepCtx`, which bails out on the internal cancel channel (closed by `Cancel()`) — Ctrl-C aborts immediately instead of waiting for the backoff to elapse.
- `Cancel` + `withCancel(ctx)` bridge `context.Context` to the underlying `*client.Client`: when the context is canceled, the connection is `Terminate()`d so blocked IMAP calls return immediately. Every long-running public method on `Client` should call `defer c.withCancel(ctx)()` and check `ctx.Err()` after each blocking step.
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
