---
name: imap-sync-correctness
description: Apply when reviewing or writing code that touches the IMAP sync flow — anything in internal/client, internal/app/sync.go, internal/app/worker.go, internal/app/show.go. Encodes the non-obvious correctness traps specific to IMAP and to this tool's plan-confirm-execute model.
---

# IMAP sync correctness

This is a sync tool. The single most important property is "running it again does the right thing". Every rule below exists to protect that property.

## Read first

- [.claude/rules/architecture.md](../../rules/architecture.md) — sync-flow invariants.
- [CLAUDE.md](../../../CLAUDE.md) "Architecture" sections 2–5.

## Traps that bite

1. **`FetchRFC822` sets `\Seen`.** Use `BODY.PEEK[...]` everywhere. A sync tool must not mutate source state. If you see `FetchRFC822` anywhere outside a test, that's a bug.
2. **Bypassing `safeCall`.** Direct `c.Client.<method>` calls skip the reconnect+classify+cancel pipeline. New IMAP ops always go through `safeCall`.
3. **Skipping `selectIfNeeded`.** Calling `cli.Select` directly costs a round-trip per batch and races with reconnect. Always use `selectIfNeeded`.
4. **Unbatched UID FETCH.** `uidFetchBatchSize = 500` exists because some servers reject longer arg lists with "Too long argument". Do not unbatch.
5. **Streaming APPEND retry.** `AppendMessage` consumes the message body once. A transient mid-APPEND error cannot be retried inside this run — the literal is gone. The diff catches it on the next run. Do not "fix" this by adding `io.ReadAll` — that doubles peak RAM.
6. **Per-connection rate limiters.** The limiters are global by design (one per direction, shared across all Clients on that side). Per-connection limiters would defeat the BPS cap. Don't introduce them.
7. **Reconnect storms.** `reconnectInterval = 10s`, `maxReconnectAttempts = 5`, `throttledBackoff = 5m`. Don't weaken these "for testing" without reverting before commit.
8. **Mailbox cache.** Populated by one `LIST "" "*"` per Client. Invalidated only on `CreateMailbox`. Re-LISTing on every check is a regression.
9. **`atomic.Pointer` fields.** `c.pw`, `c.tracker`. Always read via `c.progressWriter()` / `c.progressTracker()`. Direct field access is a data race.
10. **Worker pool is persistent.** One src + one dst `Client` per worker, for the whole sync. Do not reconnect per plan.
11. **`sleepCtx` for all reconnect-path sleeps.** Plain `time.Sleep` makes Ctrl-C hang for up to 5 minutes.
12. **`defer c.withCancel(ctx)()` on every long-running public `Client` method.** Plus `ctx.Err()` checks after each blocking step. Otherwise Ctrl-C only takes effect at the next IMAP round-trip.

## Plan-confirm-execute is sacred

`internal/app/sync.go` is structured as:

1. expand mappings,
2. reconcile delimiters,
3. build the full Message-Id diff,
4. **print the plan and confirm**,
5. pre-create destination folders,
6. dispatch to worker pool.

A streaming "fetch one, append one, repeat" model would also work but it would lose the confirmation step and make the operation impossible to dry-run. Any refactor that collapses steps 3 and 6 is a regression.

## Idempotency is Message-Id-based

There is no UID bookkeeping. There is no resume file. Re-running on a partially completed sync re-diffs and continues. If you find yourself wanting to add a state store, stop and explain the use case to the architect — there is probably a way to express it as part of the diff instead.
