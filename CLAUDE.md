# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Common commands

The project uses [`just`](https://github.com/casey/just) as its task runner. The `Justfile` is the source of truth for tooling invocations.

- `just deps` - `go mod tidy && go mod vendor` (deps are vendored; run after touching `go.mod`)
- `just lint` - `golangci-lint run ./... --timeout=5m`
- `just fix` - `go fmt ./...`, `go fix ./...` and `go tool fieldalignment -fix ./...`
- `just test` - `go test ./...` (run a single test with `go test ./internal/config -run TestName`)
- `just check` - runs `go vet`, `staticcheck`, `govulncheck`, and `fieldalignment` (these are declared as Go `tool` directives in `go.mod`, invoked via `go tool`)
- `just run` - `check` + `lint` + `test`, then `go run -race ./cmd/imapsync-go/main.go`
- `just build` - produces `dist/imapsync-go` (CGO disabled, trimpath, version metadata injected via `-ldflags -X main.{Version,Commit,Date,BuiltBy}`)
- `just build_linux` - Linux/amd64 cross-build
- `just oci [executor=podman] [tag=local]` - cross-builds the Linux binary, stages it under `dist/oci/linux/amd64/dist/`, then builds a `linux/amd64` image from `Dockerfile` with `dist/oci` as the build context

`fieldalignment` is enforced; if it complains about a struct, reorder fields rather than disabling the check.

Releases are produced by `goreleaser` (`.goreleaser.yml`) - do not hand-edit `dist/`.

The container image is built from the per-platform tree `dockers_v2` assembles: each binary lands at `<goos>/<goarch>/dist/imapsync-go` in the build context, following the `dist/` prefix in the build's `binary:` value, and `Dockerfile` resolves it through `ARG TARGETPLATFORM`. `just oci` reproduces the same layout under `dist/oci`. `.goreleaser.yml`, `Dockerfile` and the Justfile `oci` target share that path shape; moving one requires moving the other two. Multi-platform images are assembled in the publish phase, so on a tagged release `--skip=publish` skips the image entirely; `goreleaser release --snapshot` still builds them locally, which is how to build one by hand.

## Architecture

Single-binary IMAP-to-IMAP sync tool. Six things to internalize before making changes.

### 1. Layering

```
cmd/imapsync-go/main.go         - urfave/cli/v3 wiring, version flags, signal-to-context
cmd/imapsync-go/commands/       - thin subcommand definitions (sync, show); they only declare flags and call into internal/app
internal/app/sync.go            - sync orchestration: ActionSync reads flags and config, runSync connects, builds the plan, confirms, dispatches to worker pool
internal/app/show.go            - show orchestration: ActionShow reads flags and config, runShow runs parallel ListMailboxes via errgroup, formatted table, then a list of the folders it cannot read (ErrSilentExit)
internal/app/worker.go          - syncWorker pool + runFolderSync helper consumed by sync.go
internal/client/client.go       - go-imap wrapper: Options, dial+TLS, guardConn wiring (watchdog, rate limit), bounded greeting and logout, reconnect with the give-up and revive gate, Select that trusts the go-imap session state
internal/client/watchdog.go     - idleConn: inactivity watchdog for commands in flight
internal/client/fetch.go        - single-pass FetchMessageMap (Message-Id+UID), StreamMessagesByUIDs (batched UID FETCH, a retry fetches only the UIDs not yet delivered)
internal/client/mailbox.go      - MailboxExists / IsNoSelect / ListSubfolders / CreateMailbox via the cache, ListMailboxes (a folder that cannot be read carries its reason in MailboxInfo.Err)
internal/client/cache.go        - per-Client mailboxCache (one LIST "" "*"), a created mailbox is added to it, the names that LIST marks \Noselect or \NonExistent are kept
internal/client/append.go       - AppendMessage streaming the literal directly from the fetched body, one retry only before go-imap reads the body
internal/client/errors.go       - classifyError: Transient / Permanent / Throttled, text matched on leaf errors only; canRevive; isAlreadyExistsErr; ErrSelectRefused marks a server refusal of SELECT or EXAMINE
internal/client/provider.go     - DetectProvider, used by sync.go to print quota warnings
internal/config/                - JSON+YAML config loader (extension-driven, unknown keys rejected), validation
internal/progress/              - go-pretty progress writer/tracker abstraction (interfaces consumed by client)
internal/ratelimit/             - net.Conn wrapper applying golang.org/x/time/rate, installed by guardConn
internal/termsafe/              - Escape and Writer: make text from IMAP servers safe to print on a UTF-8 terminal (stdlib only)
internal/utils/                 - `AskConfirm(ctx, in *bufio.Reader, out io.Writer, prompt)` and friends
```

`ActionSync` and `ActionShow` are thin CLI adapters. They pass `client.Options{UseTLS: true}`, `os.Stdout` and `os.Stdin` to `runSync` / `runShow`. Tests pass plaintext options, a buffer and a scripted reader to dial the fake IMAP servers. Do not add a config, flag or environment switch that disables TLS.

`internal/client` defines minimal `ProgressWriter` / `ProgressTracker` interfaces it consumes; `internal/progress` implements them. Preserve this split when adding progress hooks - the alternative pulls `internal/progress` (which depends on go-pretty) into the client package.

`internal/termsafe` is a leaf package that uses only the standard library. `internal/client`, `internal/app` and `cmd/imapsync-go` import it. It does not import `internal/progress`.

`cmd/...` is the CLI entry point and depends on `internal/...`. Never import `cmd/` from `internal/`.

### 2. Idempotent sync via Message-Id matching (`internal/app/sync.go`)

The sync flow is **plan, confirm, execute** - not stream-as-you-go:

1. **Expand mappings**: each configured mapping is expanded with subfolders. `expandMappingsWithSubfolders` calls `client.ListSubfolders`, which serves the answer from the per-Client mailbox cache (one `LIST "" "*"` populates it on first connect).
   - The expansion skips each source name that `Client.IsNoSelect` reports. This applies to the mapped name and to each subfolder.
   - `IsNoSelect` reads the mailbox cache. The cache marks a name when the same `LIST "" "*"` gives it the `\Noselect` or `\NonExistent` attribute. The attribute can have any letter case.
   - The expansion keeps the subfolders of a skipped name.
   - With `--verbose` and without `--quiet`, the expansion prints one escaped "Folder ... skipped" line for each skipped name, in name order.
   - A mapped source name that LIST leaves out stays in the expansion, also when LIST shows its subfolders. Step 3 scans it.
2. **Delimiter reconciliation**: `folderDelimiter(path, serverDelimiter)` returns the detected delimiter and whether it matches the server's. On mismatch the user is prompted to rewrite delimiters in-place; `-y/--confirm` auto-accepts.
3. **Build sync plan** (`buildSyncPlan`): two goroutines run under an `errgroup` - one walks every src mapping calling `FetchMessageMap`, the other walks every dst mapping calling `MailboxExists` + `FetchMessageIDSet`. Trackers advance independently so a slow dst does not stall the src progress bar. After both sides complete a slot, `maybeDiff` fires (guarded by `atomic.Int32` reaching 2 + a `sync.Mutex` for race-detector cleanliness), computes the Message-Id diff, and immediately frees the maps to keep peak memory proportional to one folder at a time. All fetches are envelope-only via `BODY.PEEK[HEADER.FIELDS (MESSAGE-ID)] + UID`; bodies are not pulled at planning time. The "what would be synced" preview is printed and confirmed before any writes. `--quiet` hides the preview and the prompt, so `runSync` refuses `--quiet` without `-y/--confirm` before it connects to a server.
   - The destination goroutine calls `IsNoSelect` after `MailboxExists`. It does not call `FetchMessageIDSet` for a `\Noselect` destination.
   - A failed source scan gives a folder failure (`FolderFailure` in `SyncSummary.Failures`). Its error wraps `errSourceScan` ("cannot scan source folder").
   - A source mapping is a skip (`FolderSkip` in `SyncSummary.Skipped`), not a folder failure, when all these conditions are true:
     - the scan error wraps `client.ErrSelectRefused`. `FetchMessageMap` adds this marker only when the server refuses the folder with a reply that is not transient and not throttled;
     - the name is not INBOX, in any letter case;
     - the mailbox cache does not hold the name;
     - the mailbox cache holds at least one subfolder of the name (`isHierarchyLevel`).
   - A skip does not change the exit code.
   - A `\Noselect` destination is a folder failure when the source scan succeeds and finds at least one message with a Message-Id. When the source scan succeeds and finds no such message, the mapping gives no plan and no folder failure. When the source scan fails, the source rules above apply, and the destination gives no second folder failure.
   - The error of a `\Noselect` destination failure wraps `errDestinationNoSelect`.
   - A scan failure of a destination that is not `\Noselect` stops the run before any write.
   - After the scan, `runSync` does these steps in this order, before the plan preview and the prompt:
     - if the context ended, it prints nothing and returns the context error;
     - without `--quiet`, it prints one escaped line for each skip;
     - it prints the folder-failure list, also with `--quiet`.
   - A folder failure gives no plan, so the run creates no destination folder for it.
   - When the plan has no message to copy and a folder failure exists, the run shows no prompt. It prints the error summary line and exits with code 1 (`ErrSilentExit`).
   - After the copy, when a folder failure exists, the run exits with code 1 (`ErrSilentExit`). Without `--quiet`, it first lists the folder failures again. It always prints the error summary line, with the count of failed folders.
4. **Pre-create destination folders** for the active plans (sequential, with per-folder mutex on `Client.folderLocks`). New mailboxes are added back into the cache. A name that LIST marks `\Noselect` counts as existing, so the client sends no CREATE for it, also when it is a parent.
   - `CreateMailbox` gives `created == false` and no error when the CREATE reply says that the folder exists, or when the mailbox cache already holds the name. The planning scan did not find that folder, so its plan has no diff against the folder. `runSync` keeps each such folder in a set.
   - After the creation loop and before the worker pool, `recheckExistingDestinations` checks each plan whose destination is in this set. It runs in sequence on the two planning clients, sends no LIST and starts no goroutine. All checks end before the first APPEND.
   - `recheckPlan` reads the destination first with `FetchMessageIDSet`. If the folder holds no Message-Id, the plan does not change and the run sends no source command. If not, it reads the source again with `FetchMessageMap`, and `rediffPlan` keeps the planned UIDs that the new diff still lists. `rediffPlan` also counts each dropped UID in a `recheckDrops` value, by cause.
   - The check only removes messages. It never adds a UID to a plan and never shows a second prompt.
   - The two counts of `recheckDrops` are `held` and `gone`. `held` counts the dropped UIDs that the source still lists and whose Message-Id the destination holds. `gone` counts the dropped UIDs that the source no longer lists. `held`, `gone` and the kept UIDs add up to the planned UIDs.
   - Without `--quiet`, `runSync` prints up to two escaped lines for each plan that loses messages. M is the number of planned messages in both lines. Each line shows only when its count is more than 0.
     - First line, for `held`: `Destination folder ... already exists and holds N of the M planned messages from ..., so they are not copied`.
     - Second line, for `gone`: `Source folder ... no longer holds N of the M planned messages for ..., so they are not copied`.
   - With `--quiet`, the check prints neither line. The check removes a plan that has no UID left.
   - A scan that fails gives a folder failure. Its error wraps `errDestinationRecheck` for the destination or `errSourceScan` for the source. `runSync` adds it to `SyncSummary.Failures` and prints the new failures at once, also with `--quiet`. The run ends with exit code 1 (`ErrSilentExit`).
   - When the check leaves no plan, the run prints no "Syncing messages" line and opens no worker connection. It ends in the same way as the case with no message to copy.
   - When the context ends during the check, `runSync` returns the context error. It prints no failure line and sends no APPEND.
5. **Execute via worker pool**: `newSyncWorkerPool` builds N persistent workers (each owns one src + one dst `Client` for the entire sync). Plans are dispatched through a chan-of-workers semaphore. A single `progress.Writer` covers all plans, with one tracker per plan appended up front. Per-message UI updates are throttled to ~10 Hz. At the end, `logoutAll` logs out all workers at the same time, then the two planning clients, so a silent server costs at most two logout limits.

Idempotency comes from the Message-Id diff. There is no UID-based bookkeeping; re-running on a partially completed sync is safe and resumes.

### 3. Connection resilience (`internal/client/client.go`, `internal/client/watchdog.go`)

- `safeCall(fn)` wraps every IMAP op. Errors are routed through `classifyError`: only `ClassTransient` triggers a reconnect-and-retry; `ClassThrottled` and `ClassPermanent` are surfaced to the caller. Add new IMAP operations through `safeCall` rather than calling `c.Client` methods directly. `safeCall` runs each attempt of `fn` through `call`, which also opens one watchdog span.
- `classifyError` gives the class of an error. `io.EOF`, `net.ErrClosed`, each `net.Error` and `imapclient.ErrNoMailboxSelected` are `ClassTransient`. When a BYE ends the session after the EXAMINE, go-imap returns `ErrNoMailboxSelected` and does not send the command. A reconnect gives a new session, and the retry selects the folder again. For all other errors, `classifyLeaves` reads the text.
- `classifyLeaves` calls `errors.Unwrap` until it gets the leaf error and matches text on that leaf only. For an error that wraps a list, it classifies each item in order and returns the first class that is not `ClassUnknown`. So text that our wrappers add, such as a folder name or the `[label]` prefix, never sets the class. A folder name that contains `exceeded quota` cannot change the class of a dropped connection to `ClassThrottled`. Wrap an error with `%w`. A `%v` makes a new leaf that holds the wrapper text.
- `classifyMessage` matches phrases in the text of the leaf error, in this order. The first rule that matches decides:
  1. the locked-account phrases (see the locked-account item below): `ClassPermanent`;
  2. `too many simultaneous`, `session expired`, or `exceeded` with `bandwidth`, `limit` or `quota`: `ClassThrottled`;
  3. `not logged in` or `imap: connection closed`: `ClassTransient`;
  4. `temporary authentication failure`, or `authentication` with `temporarily unavailable`: `ClassTransient`;
  5. `authentication` with `fail`, `invalid credentials` or `no such mailbox`: `ClassPermanent`;
  6. all other text: `ClassUnknown`.
- Rule 4 matches an outage of the authentication service, for example the Dovecot reply `[UNAVAILABLE] Temporary authentication failure`. It comes before rule 5, so `reconnect` retries a LOGIN that gets this reply with backoff, and does not give up at once. go-imap v1 drops RFC 5530 response codes such as `[UNAVAILABLE]` from its errors, so the text decides.
- `reconnect` enforces a minimum interval (`reconnectInterval = 10s`), exponential backoff up to `maxReconnectAttempts = 5`, and a longer cool-down (`throttledBackoff = 5m`) when the previous attempt got `ClassThrottled`. `reconnect` waits only between two attempts. When the last attempt fails, `reconnect` returns the error immediately.
- The give-up and revive gate limits the login attempts of a Client after a failed `reconnect`. A login attempt is one dial and one LOGIN or AUTHENTICATE. When `reconnect` gives up, it stores the error in `giveUpErr` and stamps `lastReconnect`. This stored error is the give-up. A cancelled `reconnect` stores nothing, and a cancelled login attempt keeps the earlier give-up.
- The give-up and revive gate runs in `reconnect` under `c.mu`, after the cancellation check. So after `Cancel()`, `reconnect` returns `context.Canceled`, not the stored give-up. The gate runs before the `reconnectInterval` wait, the logout of the old session and any dial.
- `canRevive` reads the class of the stored give-up, and the class decides what the give-up and revive gate does:
  - `ClassPermanent` or `ClassUnknown`: the give-up is final. Each later call returns the stored error at once and does not dial, until the run ends. `ClassUnknown` is final because some servers refuse wrong credentials with text that no rule matches.
  - `ClassTransient` or `ClassThrottled`: calls return the stored error until `reviveInterval = 5m` passes. Then one call makes one login attempt, with no backoff and no throttle wait.
    - If that login attempt succeeds, `giveUpErr` becomes nil. The next `reconnect` then has all `maxReconnectAttempts` and the initial backoff again.
    - If that login attempt fails, `reconnect` stamps `lastReconnect` again, so `reviveInterval` starts again. A permanent or unknown failure makes the give-up final.
- While the give-up and revive gate holds a give-up, each Client makes at most one login attempt per `reviveInterval`. Clients do not share the gate. During `sync`, one side has one planning Client and one Client for each worker. Workers are at most 10, so one side has at most 11 Clients.
- Each Client stores the interval as `reviveDur`, and `New` sets it. A zero value turns the wait off (test fixtures only), like `logoutDur`. No config field or CLI flag sets it.
- `safeCall` and `AppendMessage` call `reconnect` when the Client holds no session. `AppendMessage` also calls it when `sessionEnded` reports that the session has ended. Only `New` and `reconnect` call `connectAndLogin`, so every login after `New` goes through the give-up and revive gate. A new caller of `connectAndLogin` would bypass the gate.
- `safeCall` starts at most one `reconnect` for each call:
  - When the Client holds no session, `safeCall` calls `reconnect` first. Then it runs `fn` one time and returns the error of `fn`, also when the error is transient.
  - After a transient error of `fn`, `safeCall` calls `reconnect` and runs `fn` one more time.
- `safeCall` and `AppendMessage` get a new session from `reconnectSession`, which calls `reconnect`. When `reconnect` returns an error, `reconnectSession` returns that error. When `reconnect` returns nil, `reconnectSession` checks again whether `Cancel()` ran. If `Cancel()` ran, it returns `context.Canceled`. If the Client holds no session, it returns an error. In each of these three cases, `safeCall` does not run `fn` after the reconnect, and `AppendMessage` does not send the APPEND.
- A reply text that contains `locked`, `lockout`, `disabled`, `suspended` or `blocked` is `ClassPermanent`. This rule comes before the throttle and outage rules, because a login attempt on a locked account can make the lock longer. So a give-up on such a reply is final, and that Client makes no more login attempts.
- All `time.Sleep` calls in the reconnect path go through `sleepCtx`, which bails out on the internal cancel channel (closed by `Cancel()`) - Ctrl-C aborts immediately instead of waiting for the backoff to elapse.
- `Cancel` + `withCancel(ctx)` bridge `context.Context` to the underlying `*client.Client`: when the context is canceled, the connection is `Terminate()`d so blocked IMAP calls return immediately. `withCancel` registers `Cancel` through `context.AfterFunc`. After the returned stop function runs, a later cancellation does not change the client. Every long-running public method on `Client` should call `defer c.withCancel(ctx)()` and check `ctx.Err()` after each blocking step. `connectAndLogin` also does this and stores the unauthenticated `cli` in `c.c` before calling `Login` so `Cancel()` can `Terminate()` a stalled LOGIN.
- `connectAndLogin` checks for cancellation again after it stores the new session in `c.c`. `Cancel` sets the cancelled flag before it reads `c.c`, so one side always sees the other. Thus a `Cancel()` between the greeting and the store stops the login: `connectAndLogin` does not send LOGIN and returns `context.Canceled`. During the dial and the greeting, `ctx` ends the wait: the caller context in `New`, or the internal context that `Cancel()` ends in `reconnect`.
- `loadMailboxCache` (the first `LIST "" "*"` in `New`) runs under `withCancel(ctx)` and checks `ctx.Err()`, so Ctrl-C stops `New` during that LIST. When a cancellation causes a failure, `connectAndLogin` and `loadMailboxCache` return `ctx.Err()`, or `context.Canceled` after a `Cancel()`. `cmd/imapsync-go/main.go` then prints `Cancelled.` and exits with code 130. It does not print a connection error.
- TCP keepalive is enabled at the dialer (`tcpKeepAlivePeriod = 30s` idle interval, via `newDialer`) so half-open sockets after a Wi-Fi flap are detected within the OS-default keepalive envelope rather than hanging indefinitely.
- `connectAndLogin` calls `greet(ctx, conn)` in place of a bare `imapclient.New`. `greet` closes the connection when `ctx` ends, or when the greeting does not arrive within `dialTimeout` (default `defaultDialTimeout = 30s`). The limit also covers the CAPABILITY probe that go-imap sends when the greeting has no capability list. A timer closes the socket, not `conn.SetDeadline`, because that probe clears connection deadlines. The timeout error wraps `os.ErrDeadlineExceeded`, so `classifyError` returns `ClassTransient` and `reconnect` retries it. `greet` wraps all other failures with the prefix `imap greeting:`.
- `logout(cli)` sends LOGOUT and waits a maximum of `logoutTimeout = 2s`. Each Client stores this limit as `logoutDur`, and a zero value means no limit (test fixtures only). If the server does not answer, `logout` terminates the connection and returns an error that wraps `errLogoutTimeout`. `logout` always closes the socket before it returns, because go-imap closes the socket only when the server sends BYE. `logout` does not use `safeCall`, so a logout never triggers a reconnect. `Logout()`, `reconnect` (when it discards the old session under `c.mu`) and the cleanup after a failed LOGIN or AUTHENTICATE call `logout`.
- `idleConn` (`internal/client/watchdog.go`) is the inactivity watchdog for commands in flight. `idleTimeout = 5 * time.Minute` is its limit. Each Client stores the limit as `idleDur`, and `New` sets it. A zero value turns the watchdog off (test fixtures only), like `logoutDur`. No config field or CLI flag sets the limit.
- `connectAndLogin` calls `guardConn` after `dialFn` and before `greet`. `guardConn` wraps the dialed transport in `idleConn`. When a limiter is set, it then wraps the result in `ratelimit.Conn`. So the watchdog is the innermost wrapper, and a wait for rate-limit tokens does not run inside it. `dialFn` only dials TCP or TLS.
- `c.idle` holds the watchdog of the session in `c.c`. `connectAndLogin` stores `c.idle` before `c.c`, so a reader that sees the new session also sees its watchdog. `call` loads `c.idle` again on each attempt, so a retry after a reconnect uses the new watchdog.
- A span is the time while a command is in flight. The watchdog counts only time inside a span. `safeCall` runs each attempt of `fn` through `call`, which opens one span. `AppendMessage` runs each `cli.Append` attempt through `call`, which opens one span. `connectAndLogin` opens one span around LOGIN or AUTHENTICATE, for the first connect and for each reconnect. A go-imap call outside `safeCall` must open its own span.
- The watchdog does not cover the greeting or LOGOUT. The greeting keeps its `dialTimeout` limit, and LOGOUT keeps `logoutTimeout`.
- The watchdog closes the transport when all these conditions are true:
  - a span is open;
  - at least one `Read` or `Write` is blocked on the transport;
  - no byte moved in either direction for `idleDur`.
- Any byte in either direction resets the idle time to zero. The watchdog limits inactivity, not total time, so a transfer that moves bytes is never cut. A session that is idle between commands is never closed.
- When the watchdog trips, the pending call fails. `idleErr` wraps its error with `errIdleTimeout` (`no data from server`) and `os.ErrDeadlineExceeded`, so `classifyError` returns `ClassTransient`. `safeCall` then reconnects and retries once, inside the `reconnectInterval`, `maxReconnectAttempts` and backoff limits. Example error text: `imap: connection closed: no data from server within 5m0s: i/o timeout`. Before the reconnect, `noteIdleTrip` writes one line with the reason to the progress writer, or to the tracker when no writer is set. It does this also without `--verbose`, because a trip is rare.
- A LOGIN that gets no reply during a reconnect trips too, so `reconnect` never holds `c.mu` without a limit. A trip during an APPEND follows the `AppendMessage` rules in Section 5.
- A trip never calls `Cancel` and never sets the cancelled flag. Each cancellation check runs before the error handling. So Ctrl-C returns `context.Canceled`, also after a trip.
- `StreamMessagesByUIDs` ends the span while `onMessage` runs, because that time is our own work (usually the destination APPEND). At the end of a batch the source is done and silent. Code that runs caller work inside a `safeCall` closure must also end the span, and open it again after. A wait for read tokens runs outside `idleConn`, so it never counts.
- Accepted edge: the watchdog cannot see a wait for write tokens. If one literal write (up to 32 KiB) waits `idleTimeout` or more for tokens, the watchdog trips. This needs an upload limit below about 110 bytes per second for each worker that uploads at the same time. The limiter is shared between the workers. `runSync` prints a warning before the plan when `--bps-up` is below `client.MinUploadBPS(workers)`, which computes that floor from `maxLiteralWrite` and `idleTimeout`.
- Each connection has one timer, and `Close` stops it. Without an open span the timer does not run. A timer that is pending when a span ends fires once, finds no span and stops.
- `selectIfNeeded(cli, folder)` sends no EXAMINE only when `cli.State()` is `imap.SelectedState` and `cli.Mailbox()` has the name `folder`. go-imap clears the selected mailbox after a BYE and after a failed EXAMINE. A new session after a reconnect has no selected mailbox. The Client does not store a copy of the selected folder, because a copy can differ from the session. **Use it instead of `cli.Select` directly** - `StreamMessagesByUIDs` saves one round-trip per 500-message batch this way.
- `StreamMessagesByUIDs` batches `UID FETCH` in chunks of `uidFetchBatchSize = 500` to avoid IMAP "Too long argument" errors. Don't unbatch.
- `FetchMessageMap` sizes its map with `min(total, maxMessageMapHint)`, and `maxMessageMapHint = 1 << 16`. The server sets `total`, from EXAMINE or from STATUS MESSAGES. A very large count must not cause a large allocation. The map size still increases to the real number of messages.
- A retry of a `StreamMessagesByUIDs` batch (after a transient error, through `safeCall`) fetches only the UIDs whose body `onMessage` did not receive. If `onMessage` received all of them, the retry sends no EXAMINE or FETCH command. The code does not deliver a second response with the body for the same UID. So a retry creates no duplicate APPEND on the destination. A response without a body does not mark its UID.
- Folder creation uses a per-path `sync.Mutex` (`folderLocks` map) to make concurrent `CreateMailbox` calls for nested paths race-free; parents are created walking down the hierarchy.
- `CreateMailbox` and `createParentFolders` treat a CREATE refusal as success when `isAlreadyExistsErr` matches the error text: `already exists`, `mailbox exists` or `duplicate folder name` (the Gmail text). go-imap v1 drops the `[ALREADYEXISTS]` code, so the text decides. They give `isAlreadyExistsErr` the error from `safeCall`, before a wrapper adds a folder name. They add the mailbox to the cache.
- `c.pw` and `c.tracker` are `atomic.Pointer[progressWriterRef|progressTrackerRef]`. Read via `c.progressWriter()` / `c.progressTracker()`; do not access the fields directly.
- go-imap writes every read error to stderr, including the error that each `Terminate` causes. `greet` calls `routeErrorLog` right after `imapclient.New` returns, also for a client that go-imap returns with an error. Without `--verbose`, the log goes to `io.Discard`. With `--verbose`, the logger output goes through `termsafe.NewWriter`, because the lines quote raw server responses. The function changes the output of the existing `*log.Logger` with `SetOutput`, because the reader goroutine already runs and a field write would race. Section 6 describes the lines that go-imap logs before `routeErrorLog` runs.

**Known limitation - Ctrl+C blocked inside a literal write.** go-imap sends a synchronizing literal in two steps. First the client sends `{N}\r\n`. Then it waits on `<-w.continues` (`vendor/github.com/emersion/go-imap/write.go:158`) for the `+` reply of the server. Only the response handlers of the reader goroutine send on that channel.

If the socket closes before the server sends `+` or a tagged reply, the reader goroutine ends and the wait never ends. These events close the socket: `Cancel()` / `Terminate()`, TCP keepalive expiry (`tcpKeepAlivePeriod = 30s` idle interval, several minutes on macOS and Linux) and a watchdog trip. The goroutine that writes the literal stays blocked until the process exits.

The watchdog does not change this limitation. It closes the socket in this window, but a closed socket does not release the wait. Only the worker inside the in-flight operation is affected, and other workers continue.

Users can press Ctrl+\ (SIGQUIT) to get an immediate goroutine dump and exit. A fix needs one of two changes. The first patches `writeLiteral` in the vendored go-imap, to `select` on a done channel with the `continues` receive. The second moves to go-imap v2.

### 4. Rate limiting (`internal/ratelimit`)

- `ratelimit.NewLimiter(bps)` returns `nil` when `bps <= 0` - the caller's nil-check is the "unlimited" signal.
- `ratelimit.New(net.Conn, read, write *rate.Limiter)` wraps a connection so reads and writes block until tokens are available. `guardConn` installs the wrapper in `connectAndLogin`, after `dialFn` returns and outside `idleConn`. So go-imap is unaware of throttling, and the watchdog never counts a wait for read tokens (see Section 3).
- One limiter per direction is shared across every `Client` that talks to the same side (one `srcReadLim` for all source workers, one `dstWriteLim` for all destination workers). That makes the BPS budget a global cap, not per-connection.

### 5. Body fetch semantics

`StreamMessagesByUIDs` fetches via `BODY.PEEK[]` (`fullBodyPeekSection`), not `FetchRFC822`. The two are functionally equivalent for transferring the message but `FetchRFC822` sets `\Seen` on every message - a state mutation a sync tool must not introduce on the source. If you add new fetch sites, mirror this choice.

`AppendMessage` passes `msg.GetBody(...)` straight to `cli.Append` - no `io.ReadAll` round-trip - to halve peak RAM with large attachments. go-imap reads the body one time.

Before the APPEND, `AppendMessage` calls `reconnectSession` in these cases:

- the Client holds no session;
- `sessionEnded` reports that `cli.LoggedOut()` is closed, after a BYE or a closed socket.

After a BYE, go-imap refuses the APPEND with `Not logged in`. After a closed socket without a BYE, an APPEND can wait for the `+` continuation with no time limit (see the Known limitation in Section 3).

`readTracker` records the first `Read` call of go-imap on the body. The APPEND gets one retry when all these conditions are true:

- the error is `ClassTransient`;
- go-imap did not start to read the body, so the server did not get the message;
- this call did not reconnect before;
- the context did not end, and `Cancel()` did not run.

For the retry, `AppendMessage` calls `reconnectSession` one time and sends the APPEND again.

After go-imap started to read the body, `AppendMessage` does not send the APPEND again. The server can already have the message, and a second APPEND can make a duplicate. If the context ended, `AppendMessage` returns the context error and does not repair the session. If not, it calls `reconnect` to repair the session when the error is `ClassTransient`, this call did not reconnect before, and `Cancel()` did not run. Then it returns the APPEND error. The next sync run copies the message through the Message-Id diff.

One `AppendMessage` call reconnects a maximum of one time. `AppendMessage` does not use `safeCall`, because `safeCall` sends a command again after a transient error also when go-imap read the body. Each attempt runs through `call`, so each attempt has its own watchdog span.

### 6. Terminal output (`internal/termsafe`)

An IMAP server can send folder names, error text and Message-Ids that contain control, bidi or invisible characters. On a terminal, such text can move the cursor, clear the screen or make a false status line.

**Rule.** Show text from an IMAP server on the terminal only through `termsafe.Escape`. This covers:

- folder names (LIST, the mailbox cache, plans, and the mappings that come from them);
- reply text inside errors;
- envelope Message-Ids;
- go-imap error-log lines.

Text from the operator (labels, server, user, config path) is trusted, so print it as is. A new output site must follow this rule.

**Existing `%q` sites.** Some output sites in `internal/app/sync.go` quote server text with a `%q` verb. They stay as they are. A new output site must not use `%q` for server text. These sites are:

- the delimiter lines in verbose mode;
- the delimiter-mismatch lines and the "Fixed source" and "Fixed destination" lines;
- the "Failed to create folder" and "Created folder" logs;
- the "Mapping ... skipped" line;
- the "scan destination folder" error, which `reportError` also escapes.

`strconv.Quote` escapes each rune for which `strconv.IsPrint` is false. These runes are:

- all C0, DEL and C1 controls;
- all Cf runes, bidi controls too;
- `\u2028` and `\u2029`, private-use and unassigned code points;
- each invalid UTF-8 byte.

So `%q` is safe against control sequences.

`%q` keeps graphic but invisible runes, such as `\u3164`. Two different names can look the same in a `%q` line. `Escape` escapes these runes.

Do not replace `%q` with `Escape` at these sites: the output loses the escape of the quote characters. Do not put `Escape` inside `%q`: the output gets two layers of escapes. The verbose Message-Id line in `append.go` also uses `%q`. `Client.log` escapes the whole message, so `Escape` covers that line too.

**What `Escape` does.** `Escape(s)` keeps a rune only if `strconv.IsGraphic` is true and the rune is not in `unicode.Other_Default_Ignorable_Code_Point`. All other runes become a visible Go escape in lowercase hex: `\a \b \f \n \r \t \v`, `\xNN`, `\uNNNN` or `\UNNNNNNNN`. Each invalid UTF-8 byte also gets an escape.

`Escape` escapes these runes:

- C0 (tab, CR and LF too), DEL and C1;
- Cf runes: all bidi controls (U+202A to U+202E and U+2066 to U+2069), LRM, RLM, ALM, zero-width characters, BOM, soft hyphen and tags;
- U+2028 and U+2029, private-use and unassigned code points;
- graphic but invisible runes, such as U+3164 and U+034F.

`Escape` does not change letters of any script, punctuation, symbols, emoji, variation selectors (U+FE0F), spaces (U+00A0 too), backslash and quotes. If nothing needs escaping, it returns the input and does not allocate memory.

Backslash is not escaped, on purpose. A folder named `\x1b` looks the same as a folder that contains an ESC character. Both forms are inert text.

The guarantee holds for UTF-8 terminals. A terminal that acts on 8-bit C1 bytes inside multi-byte runes is out of scope.

`termsafe.Writer` (from `NewWriter`) escapes each `Write` and keeps one trailing newline raw, so each `log.Logger` line stays one line.

**Presentation boundary.** Escape only where the program prints the text. These values stay byte-exact:

- the arguments of SELECT, EXAMINE, STATUS, CREATE and APPEND, which go back to a server;
- plan data, mappings and map keys;
- error values. `classifyError` and `isAlreadyExistsErr` match on raw server text. The `Err` fields of `MailboxInfo`, `FolderFailure` and `FolderSkip` also hold raw server text, and the print sites escape them.

A tracker line carries our own SGR colour codes. Escape the server parts before you compose the line, never the whole line.

**Where the program escapes.**

- `Client.log` escapes the whole composed message for both sinks. No format of `log` carries colour codes, so every caller of `log` is covered.
- Two direct sinks escape on their own: the missing-Message-Id warning in `fetch.go` and the per-mailbox tracker line in `mailbox.go`.
- `internal/app/sync.go` escapes these sites:
  - the plan preview;
  - the folders-to-create list;
  - the creation tracker;
  - the "already exists" log;
  - the error text of the "Failed to create folder" log (the folder name there stays `%q`);
  - the waiting trackers and the scan trackers;
  - the found-subfolder line;
  - the folder-failure list (`printFolderFailures`), which shows the source folder, the destination folder and the reason. `runSync` prints it after the scan, also with `--quiet`. Without `--quiet`, `runSync` prints it again after the copy;
  - the "Folder ... skipped" line in `expandMappingsWithSubfolders`, for each source name that the mailbox cache marks `\Noselect` or `\NonExistent`. Only the name is server text. `expandMappingsWithSubfolders` prints the line only with `--verbose` and without `--quiet`;
  - the skip line for an unlisted parent that refuses to open (`printFolderSkips`). It shows the name, a fixed reason and the error text with the server reply. `runSync` prints it also without `--verbose`, but only without `--quiet`;
  - the "already exists and holds" line and the "no longer holds" line of `recheckExistingDestinations`. Each line shows the destination folder and the source folder. `runSync` prints each line only without `--quiet`. The failures of the same check go through `printFolderFailures`.
- `internal/app/show.go` escapes these sites:
  - the folder cell of each table row, also on the "unreadable" and "container" rows;
  - each line of the unreadable-folder list (`printUnreadableFolders`), which shows the folder name and the reason.
- `internal/app/worker.go` escapes every tracker line and log, and the verbose Message-Id line. `trackerReason` escapes the inline error reason first, then cuts it to 40 bytes on a rune boundary.
- `reportError` in `cmd/imapsync-go/main.go` escapes the final `Error:` line, line breaks too. So a multi-line error prints on one line. A raw line break would let a server make a false line after `Error:`.
- `cmd/imapsync-go/helpers` escapes the GitHub release tag that the root `--help` shows for builds without `-X main.Version`.

**Known limitation - go-imap logs before `routeErrorLog` runs.** `routeErrorLog` (Section 3) covers the go-imap error log. But `imapclient.New` creates its logger on `os.Stderr`. It also reads the greeting and the implicit CAPABILITY probe before `routeErrorLog` can run. A line that go-imap logs in that window reaches stderr unescaped, in every mode.

In go-imap v1.2.1, such lines are parse errors and network errors. A parse error holds server text in only two places: one of the characters `(`, `{` or `"`, and a literal length that `strconv` quotes. Neither can carry a control sequence. Audit this again on each go-imap upgrade. A full fix needs go-imap v2, or a fork with a logger option. A patch of `vendor/` does not last, because `just deps` rewrites it.

## Conventions

- Configuration format is detected from the file extension: `.json` / `.yaml` / `.yml` (see `internal/config/config.New`). The CLI accepts either; sample files are `config.example.json` / `config.example.yaml`. The loader rejects a key that `Config` does not define. This applies at all levels, in both formats. JSON matches a key without regard to letter case, as `encoding/json` does, so `SRC` loads as `src`. YAML matches only the exact letter case. The error names the key and the file. The loader also rejects content after the first JSON value or YAML document. An empty or null YAML document after the first document has no settings, so the loader accepts it.
- `auth` accepts only an empty value, `login` or `cram-md5`, in any letter case. The loaded value keeps the letter case from the file. `config.New` rejects other values with `ErrAuthUnsupported`. The error names the block (`src` or `dst`) and the supported values. It never shows the value from the file, because the value can be a password in the wrong field. `client.New` also refuses an unknown `Options.Auth` before it dials. Its error also does not show the value.
- Workers are clamped to `[1, 10]` in `config.New`. A value out of range changes to the nearest limit.
- Optional `rate_limit` block in config (`down_bps`, `up_bps`, `max_connections`); CLI flags `--bps-down`, `--bps-up`, `--max-connections` override it. A flag that the user sets, or its environment variable, overrides the config value also when it is 0. For `--bps-down` and `--bps-up`, 0 removes the limit. For `--max-connections`, 0 uses the `--workers` value. `config.New` tests `IsSet`, not a non-zero value.
- `config.New` rejects a `map` item with an empty or missing `src` or `dst` (`ErrMapSrcRequired`, `ErrMapDstRequired`). The error gives the position of the item, counted from 1. A `map` with no items is valid: `sync` then mirrors every source folder.
- `ErrSilentExit` makes `reportError` return exit code 1 and print no `Error:` line. A command returns it after it prints its failures on stdout. `sync` uses it for failed messages and failed folders. `show` uses it for folders that it cannot read.
- `client.New(ctx, addr, user, pass, Options{...})` - keep call-sites using the `Options` struct rather than positional bools, and pass the parent context so the dial honours cancellation.
- Dependencies are vendored under `vendor/`. The module requires Go 1.27.3 or newer; an older host toolchain is refused unless `GOTOOLCHAIN` is left on `auto` to fetch one.

## Multi-agent workflow

This repo ships a `.claude/` directory with five role-specific subagents and a shared rule set. They exist to keep non-trivial changes architecturally honest:

- **architect** (opus) - gatekeeper. Validates the brief, may reject, orchestrates the rest.
- **developer** (sonnet) - implements within a precise brief; writes WHY-comments and unit tests.
- **tester** (sonnet) - runs the suite, audits coverage and invariants, proposes deliberate exclusions.
- **security** (opus) - CVE + attack-surface audit; reasons about combined risk.
- **tech-writer** (sonnet) - comment hygiene + CLAUDE.md / README.md / `config.example.*` sync.

Entry points:

- `/architect <task>` - hand the task to the architect (the architect decides whether to proceed).
- `/workflow <task>` - full architect-led delegation chain (preferred for non-trivial changes).
- `/dev`, `/qa`, `/sec`, `/docs` - invoke a single agent directly (use when you know which step you need).
- `/deps`, `/lint`, `/test`, `/check`, `/fix`, `/build`, `/build-linux`, `/oci`, `/run` - thin wrappers over the `Justfile` targets.

The rules each agent reads live in `.claude/rules/`:

- [workflow.md](.claude/rules/workflow.md) - delegation protocol (architect-led + fallback).
- [architecture.md](.claude/rules/architecture.md) - layering and sync-flow invariants the architect enforces.
- [go-style.md](.claude/rules/go-style.md) - Go style, stricter than community defaults.
- [testing.md](.claude/rules/testing.md) - what is tested, what is deliberately excluded.
- [security.md](.claude/rules/security.md) - standing security checklist + combined-surface checks.
- [docs.md](.claude/rules/docs.md) - comment + doc audit rules.

Skills in `.claude/skills/` (auto-loaded by agents when relevant): `idiomatic-go-review`, `imap-sync-correctness`, `coverage-audit`, `security-audit`, `doc-sync`.
