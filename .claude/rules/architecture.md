# Architectural invariants

Read [CLAUDE.md](../../CLAUDE.md) first — this file only adds review-time emphasis, not new rules.

## Non-negotiable layering

- `cmd/...` imports `internal/...`. **Never the other way around.**
- `internal/client` defines its own minimal `ProgressWriter` / `ProgressTracker` interfaces. It must not import `internal/progress` (which pulls in go-pretty). Same shape for any future cross-package callback: declare the interface where it is consumed, satisfy it where it is produced.
- `internal/progress` and `internal/ratelimit` are leaves — they depend on stdlib + one external package each. Keep them that way.
- `internal/termsafe` is a leaf that uses only the standard library. It must not import `internal/progress`. Keep it that way, so `internal/client` can import it without a path to go-pretty.
- `internal/app` orchestrates; it owns the worker pool, the plan, and the confirmation prompts. It does not reach into `*imap.*` types — that abstraction lives in `internal/client`.
- `ActionSync` / `ActionShow` stay thin adapters that pass `client.Options{UseTLS: true}` to `runSync` / `runShow`. Only tests pass plaintext options. A config, flag or environment switch for plaintext is a security review point.

## Sync flow invariants

1. **Plan, confirm, execute** — never stream-as-you-go. The Message-Id diff must be computed and shown before any write to the destination. `--quiet` hides the plan, so it requires `--confirm`.
2. **Idempotency = Message-Id diff**, not UID bookkeeping. Do not add a state store.
3. **`BODY.PEEK[...]` everywhere a body or header is fetched** — `FetchRFC822` sets `\Seen` on the source, which is a write a sync tool must not perform.
4. **`AppendMessage` streams the literal** — `msg.GetBody(...)` is handed straight to `cli.Append`. No `io.ReadAll`. Transient mid-APPEND failures are tolerated because the next run re-diffs.
5. **`StreamMessagesByUIDs` batches at `uidFetchBatchSize = 500`.** Do not unbatch — servers reject longer arg lists.
6. **`selectIfNeeded` over `cli.Select`.** Generation-aware caching saves a round-trip per batch and is invalidated correctly on reconnect.
7. **Mailbox cache populated by one `LIST "" "*"` per Client.** `CreateMailbox` adds the new mailbox to the cache. Do not re-LIST eagerly.

## Connection resilience

- Every IMAP op goes through `safeCall`. New ops must follow.
- `classifyError` is the single source of error policy: `Transient` → reconnect+retry, `Throttled` → backoff `throttledBackoff = 5m`, `Permanent` → surface.
- All sleeps in the reconnect path use `sleepCtx` so Ctrl-C is honoured.
- Every long-running public `Client` method runs `defer c.withCancel(ctx)()` and checks `ctx.Err()` after each blocking step.
- `c.pw` / `c.tracker` are `atomic.Pointer`. Read via `progressWriter()` / `progressTracker()`. Direct field access is a bug.
- A go-imap call outside `safeCall` opens its own watchdog span, as `AppendMessage` and `connectAndLogin` do. Without a span, a silent server can hold that call forever.
- Caller work inside a `safeCall` closure ends the span while it runs and opens it again after, as `StreamMessagesByUIDs` does around `onMessage`. Otherwise slow work on the other side counts as server silence.

## Rate limiting

- One limiter per direction is shared across all `Client`s that talk to the same side. Per-connection limiters would defeat the global cap.
- `ratelimit.NewLimiter(bps)` returning `nil` is the "unlimited" sentinel. Callers must nil-check, not call methods on it.

## Terminal output

- Show text from an IMAP server on the terminal only through `termsafe.Escape`. A new output site must use `Escape`. A `%q` verb is accepted only at the sites that already use it, in `internal/app/sync.go`. `%q` is safe against control sequences, but it keeps graphic invisible runes such as `\u3164`.
- Escape at the presentation boundary: the place that prints text or composes a display line. Do not escape earlier.
- These values stay byte-exact: arguments that go back to a server (SELECT, EXAMINE, STATUS, CREATE, APPEND), plan data, mappings, map keys and error values.
- `classifyError` and `isAlreadyExistsErr` match on raw server text. Do not give them escaped text.
- A coloured tracker line carries our own SGR codes. Escape the server parts before you compose the line, never the whole line.
- A new output site that shows server text follows this rule. Operator text (labels, server, user, config path) is trusted.

## Concurrency

- Workers are clamped `[1, 10]` in `config.New`. A value out of range changes to the nearest limit. Do not surface this as a runtime panic.
- Folder creation uses per-path `sync.Mutex` from `Client.folderLocks`. Do not replace with a global mutex — concurrent creates of unrelated paths must not serialise.
- The worker pool is **persistent**: each worker owns one src + one dst `Client` for the whole sync. Do not reconnect per plan.

## Tooling invariants

- `fieldalignment` is enforced. Reorder struct fields, never silence the linter.
- Config format is extension-driven (`.json` / `.yaml` / `.yml`). Add new fields to both example files.
- Deps are vendored. After `go.mod` changes, `just deps` (which runs `mod tidy && mod vendor`) is mandatory.
- Releases are produced by `goreleaser`. Do not hand-edit `dist/`.
- The container build context is a per-platform tree (`<goos>/<goarch>/dist/imapsync-go`). `dockers_v2` in `.goreleaser.yml`, the `ARG TARGETPLATFORM` copy in `Dockerfile`, and the staging step in the Justfile `oci` target must agree on it. `latest` is emitted only when the git tag carries no prerelease part - `.Prerelease` is read from the tag, not from `.Version`, so a local snapshot still tags `latest`.

## When in doubt

Re-read the six numbered sections in [CLAUDE.md](../../CLAUDE.md) "Architecture". Those are the invariants the architect enforces.
