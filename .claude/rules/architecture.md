# Architectural invariants

Read [CLAUDE.md](../../CLAUDE.md) first — this file only adds review-time emphasis, not new rules.

## Non-negotiable layering

- `cmd/...` imports `internal/...`. **Never the other way around.**
- `internal/client` defines its own minimal `ProgressWriter` / `ProgressTracker` interfaces. It must not import `internal/progress` (which pulls in go-pretty). Same shape for any future cross-package callback: declare the interface where it is consumed, satisfy it where it is produced.
- `internal/progress` and `internal/ratelimit` are leaves — they depend on stdlib + one external package each. Keep them that way.
- `internal/app` orchestrates; it owns the worker pool, the plan, and the confirmation prompts. It does not reach into `*imap.*` types — that abstraction lives in `internal/client`.

## Sync flow invariants

1. **Plan, confirm, execute** — never stream-as-you-go. The Message-Id diff must be computed and shown before any write to the destination.
2. **Idempotency = Message-Id diff**, not UID bookkeeping. Do not add a state store.
3. **`BODY.PEEK[...]` everywhere a body or header is fetched** — `FetchRFC822` sets `\Seen` on the source, which is a write a sync tool must not perform.
4. **`AppendMessage` streams the literal** — `msg.GetBody(...)` is handed straight to `cli.Append`. No `io.ReadAll`. Transient mid-APPEND failures are tolerated because the next run re-diffs.
5. **`StreamMessagesByUIDs` batches at `uidFetchBatchSize = 500`.** Do not unbatch — servers reject longer arg lists.
6. **`selectIfNeeded` over `cli.Select`.** Generation-aware caching saves a round-trip per batch and is invalidated correctly on reconnect.
7. **Mailbox cache populated by one `LIST "" "*"` per Client.** Invalidated on `CreateMailbox`. Do not re-LIST eagerly.

## Connection resilience

- Every IMAP op goes through `safeCall`. New ops must follow.
- `classifyError` is the single source of error policy: `Transient` → reconnect+retry, `Throttled` → backoff `throttledBackoff = 5m`, `Permanent` → surface.
- All sleeps in the reconnect path use `sleepCtx` so Ctrl-C is honoured.
- Every long-running public `Client` method runs `defer c.withCancel(ctx)()` and checks `ctx.Err()` after each blocking step.
- `c.pw` / `c.tracker` are `atomic.Pointer`. Read via `progressWriter()` / `progressTracker()`. Direct field access is a bug.

## Rate limiting

- One limiter per direction is shared across all `Client`s that talk to the same side. Per-connection limiters would defeat the global cap.
- `ratelimit.NewLimiter(bps)` returning `nil` is the "unlimited" sentinel. Callers must nil-check, not call methods on it.

## Concurrency

- Workers are clamped `[1, 10]` in `config.New`. Out-of-range falls back to 1. Do not surface this as a runtime panic.
- Folder creation uses per-path `sync.Mutex` from `Client.folderLocks`. Do not replace with a global mutex — concurrent creates of unrelated paths must not serialise.
- The worker pool is **persistent**: each worker owns one src + one dst `Client` for the whole sync. Do not reconnect per plan.

## Tooling invariants

- `fieldalignment` is enforced. Reorder struct fields, never silence the linter.
- Config format is extension-driven (`.json` / `.yaml` / `.yml`). Add new fields to both example files.
- Deps are vendored. After `go.mod` changes, `just deps` (which runs `mod tidy && mod vendor`) is mandatory.
- Releases are produced by `goreleaser`. Do not hand-edit `dist/`.

## When in doubt

Re-read the five numbered sections in [CLAUDE.md](../../CLAUDE.md) "Architecture". Those are the invariants the architect enforces.
