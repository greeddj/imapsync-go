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

Single-binary IMAP-to-IMAP sync tool. Three things to internalize before making changes:

### 1. The CLI → app → client layering

```
cmd/imapsync-go/main.go         — urfave/cli/v3 wiring, version flags, signal-to-context
cmd/imapsync-go/commands/       — thin subcommand definitions (sync, show); they only declare flags and call into internal/app
internal/app/{sync,show}.go     — orchestration: load config, connect, build plan, confirm, execute
internal/client/client.go       — go-imap wrapper with reconnect/retry, delimiter caching, mailbox creation
internal/config/                — JSON+YAML config loader (extension-driven), validation
internal/progress/              — go-pretty progress writer/tracker abstraction (interfaces consumed by client)
internal/utils/                 — `AskConfirm` and friends
```

`internal/client` defines minimal `ProgressWriter` / `ProgressTracker` interfaces it consumes; `internal/progress` implements them. This is deliberate to avoid a circular import — preserve it when adding progress hooks.

`cmd/...` is the CLI entry point and depends on `internal/...`. Never import `cmd/` from `internal/`.

### 2. Idempotent sync via Message-Id matching (`internal/app/sync.go`)

The sync flow is **plan, confirm, execute** — not stream-as-you-go:

1. **Expand mappings**: each configured mapping is expanded with subfolders via `LIST` on the source server (`expandMappingsWithSubfolders`).
2. **Delimiter reconciliation**: `validateFolderPath` / `detectDelimiter` compare configured paths against each server's hierarchy delimiter (cached in `Client.delimiter` at connect time). On mismatch the user is prompted to rewrite delimiters in-place; `-y/--confirm` auto-accepts.
3. **Build sync plan** (`buildSyncPlan`): for every mapping, fetch envelope-only Message-Ids from both sides in parallel (`FetchMessageIDs`), diff to find IDs present on source but missing on dest, then `FetchMessagesByIDs` pulls full bodies **only for the diff**. The "what would be synced" preview is printed and confirmed before any writes.
4. **Pre-create destination folders** for the active plans (sequential, with per-folder mutex in `client.getFolderLock`).
5. **Execute in chunks** of `cfg.Workers` plans concurrently. Each goroutine opens its **own** `client.New(...)` to the destination — go-imap clients are not safe to share across folders/goroutines. The source client is reused for fetch.

Idempotency comes from the Message-Id diff. There is no UID-based bookkeeping; re-running on a partially completed sync is safe and will resume.

### 3. Connection resilience (`internal/client/client.go`)

- `safeCall(fn)` wraps every IMAP op. On `io.EOF` / `net.ErrClosed` / `net.Error` it calls `Reconnect` and retries once. Add new IMAP operations through `safeCall` rather than calling `c.Client` methods directly.
- `Reconnect` enforces a minimum interval (`reconnectInterval = 10s`) and exponential backoff up to `maxReconnectAttempts = 5`.
- `Cancel` + `withCancel(ctx)` bridge `context.Context` to the underlying `*client.Client`: when the context is canceled, the connection is `Terminate()`d so blocked IMAP calls return immediately. Every long-running public method on `Client` should call `defer c.withCancel(ctx)()` and check `ctx.Err()` after each blocking step.
- `FetchMessagesByIDs` batches `UID FETCH` in chunks of `uidFetchBatchSize = 500` to avoid IMAP "Too long argument" errors. Don't unbatch.
- Folder creation uses a per-path `sync.Mutex` (`folderLocks` map) to make concurrent `CreateMailbox` calls for nested paths race-free; parents are created walking down the hierarchy.

## Conventions

- Configuration format is detected from the file extension: `.json` / `.yaml` / `.yml` (see `internal/config/config.New`). The CLI accepts either; sample files are `config.example.json` / `config.example.yaml`.
- Workers are clamped to `[1, 10]` in `config.New` — anything out of range falls back to `1`.
- Dependencies are vendored under `vendor/`. Build with whatever the host Go provides; the module pins `go 1.26`.
