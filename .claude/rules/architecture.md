# Architectural invariants

Read [CLAUDE.md](../../CLAUDE.md) first - this file only adds review-time emphasis, not new rules.

## Non-negotiable layering

- `cmd/...` imports `internal/...`. **Never the other way around.**
- `internal/client` defines its own minimal `ProgressWriter` / `ProgressTracker` interfaces. It must not import `internal/progress` (which pulls in go-pretty). Same shape for any future cross-package callback: declare the interface where it is consumed, satisfy it where it is produced.
- `internal/progress` and `internal/ratelimit` are leaves - they depend on stdlib + one external package each. Keep them that way.
- `internal/termsafe` is a leaf that uses only the standard library. It must not import `internal/progress`. Keep it that way, so `internal/client` can import it without a path to go-pretty.
- `internal/app` orchestrates; it owns the worker pool, the plan, and the confirmation prompts. It does not reach into `*imap.*` types - that abstraction lives in `internal/client`.
- `ActionSync` / `ActionShow` stay thin adapters that pass `client.Options{UseTLS: true}` to `runSync` / `runShow`. Only tests pass plaintext options. A config, flag or environment switch for plaintext is a security review point.

## Sync flow invariants

1. **Plan, confirm, execute** - never stream-as-you-go. The Message-Id diff must be computed and shown before any write to the destination. `--quiet` hides the plan, so it requires `--confirm`. After the prompt, the check of a destination after CREATE only removes messages from a plan. It never adds a UID and never asks a second question.
2. **Idempotency = Message-Id diff**, not UID bookkeeping. Do not add a state store.
3. **`BODY.PEEK[...]` everywhere a body or header is fetched** - `FetchRFC822` sets `\Seen` on the source, which is a write a sync tool must not perform.
4. **`AppendMessage` streams the literal** - `msg.GetBody(...)` is handed straight to `cli.Append`. No `io.ReadAll`. After a `ClassTransient` failure, it reconnects and sends the APPEND again one time, only when go-imap did not start to read the body. After go-imap started to read the body, it sends no second APPEND, because the server can already have the message. The next run's Message-Id diff copies it. Do not move `AppendMessage` into `safeCall`, because `safeCall` retries also after a partial read.
5. **`StreamMessagesByUIDs` batches at `uidFetchBatchSize = 500`.** Do not unbatch - servers reject longer arg lists.
6. **`selectIfNeeded` over `cli.Select`.** It sends no EXAMINE when the go-imap session reports the folder as selected, which saves a round-trip per batch. A BYE, a failed EXAMINE and a reconnect each clear that state. Do not store the selected folder in the Client: a stored copy can differ from the real session.
7. **Mailbox cache populated by one `LIST "" "*"` per Client.** `CreateMailbox` adds the new mailbox to the cache. Do not re-LIST eagerly.
8. **`FetchMessageMap` limits the size hint of its map to `maxMessageMapHint`.** The server sets the message count, and a very large count must not cause a large allocation.

## Connection resilience

- Every IMAP op goes through `safeCall`. New ops must follow.
- `classifyError` is the single source of error policy: `Transient` → reconnect+retry, `Throttled` → backoff `throttledBackoff = 5m`, `Permanent` → surface.
- `classifyError` matches text on the leaf error only. Text that our wrappers add, such as a folder name or a label, never sets the class. Wrap an error with `%w`. A `%v` of an error makes a new leaf that holds the wrapper text.
- `safeCall` starts at most one `reconnect` for each call. After a `reconnect` that returns nil, `safeCall` and `AppendMessage` check again whether `Cancel()` ran. They return an error and send no command when `reconnect` leaves no session.
- All sleeps in the reconnect path use `sleepCtx` so Ctrl-C is honoured.
- The give-up and revive gate in `reconnect` holds the give-up of a Client. A `Permanent` or `Unknown` give-up is final. After a `Transient` or `Throttled` give-up, a Client makes at most one login attempt per `reviveInterval = 5m`.
  - The gate runs under `c.mu`, after the cancellation check and before any wait, logout or dial.
  - Only `New` and `reconnect` call `connectAndLogin`. A new caller would bypass the gate.
  - A reply that reports a locked, disabled, suspended or blocked account is `Permanent`.
  - A login reply that reports a temporary outage of the authentication service is `Transient`. The locked-account rule comes first.
  - A change to the gate is a security review point. See [security.md](security.md) item 10.
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
- Folder creation uses per-path `sync.Mutex` from `Client.folderLocks`. Do not replace with a global mutex - concurrent creates of unrelated paths must not serialise.
- The worker pool is **persistent**: each worker owns one src + one dst `Client` for the whole sync. Do not reconnect per plan.

## Tooling invariants

- `fieldalignment` is enforced. Reorder struct fields, never silence the linter.
- Config format is extension-driven (`.json` / `.yaml` / `.yml`). Add new fields to both example files.
- Config decoding is strict: `DisallowUnknownFields` for JSON and `KnownFields(true)` for YAML. Do not relax it. A new config field needs a `json` tag and a `yaml` tag with the same key. If the keys differ, one format rejects the key.
- Deps are vendored. After `go.mod` changes, `just deps` (which runs `mod tidy && mod vendor`) is mandatory.
- Releases are produced by `goreleaser`. Do not hand-edit `dist/`.
- The container build context is a per-platform tree (`<goos>/<goarch>/dist/imapsync-go`). `dockers_v2` in `.goreleaser.yml`, the `ARG TARGETPLATFORM` copy in `Dockerfile`, and the staging step in the Justfile `oci` target must agree on it. `latest` is emitted only when the git tag carries no prerelease part - `.Prerelease` is read from the tag, not from `.Version`, so a local snapshot still tags `latest`.

## When in doubt

Re-read the six numbered sections in [CLAUDE.md](../../CLAUDE.md) "Architecture". Those are the invariants the architect enforces.
