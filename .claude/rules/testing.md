# Testing rules

## What "covered" means here

- A change is covered when every behavioural branch the change introduced is exercised by a test that would fail if the branch broke.
- Coverage % is a guideline, not a gate. Coverage of error paths and invariants matters more than total %.
- `go test -cover -coverprofile=cover.out ./...` and `go tool cover -func=cover.out` are the source of truth.

## What we test

- **`internal/config`** - JSON+YAML loading (both extensions), validation, defaults (worker clamp, rate-limit nil case), the accepted and rejected `auth` values. The tests also cover unknown keys at every level in both formats, content after the first document, empty files, and the load of both example files.
- **`internal/client`** - error classification, mailbox cache update on create (no new LIST), fetch batching boundaries and AppendMessage streaming (no ReadAll).
  Error paths and cancellation run against scripted fake servers. These tests cover:
  - a refusal of each command;
  - a context that ends while LIST, STATUS, FETCH, CREATE or APPEND waits for a reply;
  - CRAM-MD5 failure and success;
  - an unknown auth mechanism that `New` refuses before it dials;
  - a very large message count from EXAMINE or STATUS, which must not cause a large allocation (`Test_FetchMessageMap_When_ServerReportsHugeMessageCount_Then_AllocationStaysSmall`);
  - `withCancel` after stop, and shared limiters that draw from one budget.

  The error class has these tests:
  - `TestClassifyError` and `TestClassifyError_When_WrapperTextHoldsAPhrase_Then_LeafDecides` - the text of a wrapper (a folder name or a label) never changes the class, and a leaf reply keeps its class under each wrapper;
  - `TestCanRevive` - only a transient or throttled give-up can revive;
  - `TestIsAlreadyExistsErr` - the CREATE refusal texts, with the Gmail text `Duplicate folder name`;
  - `Test_FetchMessageMap_When_ConnectionDropsAndNameHoldsThrottlePhrase_Then_ReconnectsAndRetries` and `Test_StreamMessagesByUIDs_When_ConnectionDropsAndNameHoldsThrottlePhrase_Then_ReconnectsAndStreams` - a folder name that holds a throttle phrase does not stop the reconnect;
  - `Test_FetchMessageMap_When_RefusalEchoesTransientPhraseOfFolderName_Then_ReconnectsOnce` - a refusal whose text holds a transient phrase because it echoes the folder name costs one reconnect, and then `FetchMessageMap` returns the refusal;
  - `Test_CreateMailbox_When_RetriedCreateAnswersDuplicateFolderName_Then_ReportsExisting` - a CREATE that the server applied before it dropped the connection does not fail when the retry gets the Gmail reply.

  The reconnect attempts have these tests:
  - `Test_safeCall_When_ReconnectLoginFailsTemporarily_Then_RetriesAndRecovers` - a temporary authentication failure gets the backoff, and the next attempt restores the session;
  - `Test_reconnect_When_LoginAlwaysFailsTemporarily_Then_StopsAfterMaxAttempts` - a temporary failure uses all attempts and never waits for the throttle cool-down;
  - `Test_reconnect_When_LoginFailsWithBadCredentials_Then_GivesUpAfterOneAttempt` - a refusal of the credentials ends `reconnect` at the first attempt;
  - `Test_reconnect_When_EveryAttemptFails_Then_NoWaitFollowsTheLastAttempt` - `reconnect` waits only between two attempts;
  - `Test_reconnect_When_LastAttemptFailsPermanently_Then_ReturnsTheRefusal` and `Test_reconnect_When_LastAttemptSucceeds_Then_Reconnects` - the last attempt still runs, and its result is the result of `reconnect`.

  The give-up and revive gate has these tests:
  - `Test_safeCall_When_WithinReviveInterval_Then_FailsFastWithoutDial` - inside `reviveInterval`, a call returns the stored give-up with no dial, login attempt, wait or log line;
  - `Test_safeCall_When_ReviveIntervalPassed_Then_OneLoginRestoresTheSession` - after `reviveInterval`, one login attempt restores the session and the full reconnect budget;
  - `Test_safeCall_When_ReviveProbeFails_Then_OneAttemptAndCooldownRestarts` - a failed attempt is one login with no backoff, and `reviveInterval` starts again;
  - `Test_safeCall_When_ReconnectWasRefusedPermanently_Then_LaterCallsSendNoLogin` - a permanent give-up is final, also when `reviveDur` is zero;
  - `Test_safeCall_When_ReconnectGaveUpOnUnknownRefusal_Then_LaterCallsSendNoLogin` - an unknown-class give-up is final;
  - `Test_safeCall_When_CancelledDuringReviveProbe_Then_ReturnsCanceledAndKeepsGiveUp` - a cancelled login attempt returns `context.Canceled` and keeps the earlier give-up;
  - `Test_safeCall_When_EarlierReconnectGaveUp_Then_NextCallWaitsAndReconnects` - after a give-up that can revive, the next call waits until the reconnect interval ends, logs in once and runs `fn` once;
  - `Test_safeCall_When_ReviveProbeIsRefusedPermanently_Then_NoFurtherLogin` - a permanent refusal of the login attempt is final;
  - `Test_safeCall_When_RevivedSessionFailsTransiently_Then_DoesNotReconnectAgain` - a call that reconnected first runs `fn` one time, and the transient error goes to the caller;
  - `Test_safeCall_When_CancelledWithoutSession_Then_ReturnsCanceledWithoutDial`, `Test_reconnect_When_CancelledWhileWaitingForLock_Then_ReturnsCanceledNotTheGiveUp` and `Test_safeCall_When_CancelledDuringWaitBeforeReconnect_Then_ReturnsCanceledWithoutLogin` - a cancel comes before the stored give-up, ends the wait, and no LOGIN follows;
  - `Test_safeCall_When_CancelledDuringReconnectWait_Then_ReturnsCanceledWithoutAnotherLogin` - a cancel ends the wait between two reconnect attempts, no LOGIN follows, `fn` does not run again and `c.mu` is free;
  - `Test_reconnect_When_ClientIsCancelled_Then_ReturnsCanceledWithoutDial` - a cancelled Client neither dials nor logs in, and `c.mu` is free;
  - `Test_safeCall_When_CancelledRightAfterReconnect_Then_ReturnsCanceledWithoutRunningFn` and `Test_AppendMessage_When_CancelledRightAfterReconnect_Then_SendsNoAppend` - a cancel that comes right after a reconnect returns `context.Canceled`, `safeCall` does not run `fn` again, and `AppendMessage` sends no APPEND;
  - `Test_safeCall_When_SessionMissingAfterReconnect_Then_ReturnsErrorWithoutRunningFn` - when a reconnect leaves no session, `safeCall` returns an error and does not run `fn` again;
  - `Test_Client_When_EarlierReconnectGaveUp_Then_OperationsReconnect` - each public operation that uses `safeCall` logs in again on a Client that has no session;
  - the "account lock" and "account lockout" cases of `Test_safeCall_When_ServerStillRefusesAfterGiveUp_Then_EachCallSpendsOneAttempt` - after an account-lock reply, the Client sends no further login;
  - `Test_AppendMessage_When_RepairGaveUpTransiently_Then_NextAppendProbesAfterCooldown` - `AppendMessage` goes through the same gate.

  The `selectIfNeeded` check has these tests:
  - `Test_selectIfNeeded_shortCircuitsOnNetwork` - a second call for the selected folder sends no EXAMINE;
  - `Test_selectIfNeeded_When_FolderDiffers_Then_SelectsAgain` - a different folder gets a new EXAMINE;
  - `Test_selectIfNeeded_When_SessionEndedByBye_Then_DoesNotShortCircuit` - after a BYE, the call does not use the old selection;
  - `Test_StreamMessagesByUIDs_When_ExamineOfAnotherFolderFailed_Then_SelectsAgain` - after a failed EXAMINE of another folder, the earlier folder gets a new EXAMINE;
  - `Test_selectIfNeeded_afterReconnect_reSelects` - the session after a reconnect gets a new EXAMINE;
  - `Test_safeCall_When_ByeLandsBetweenSelectAndFetch_Then_ReconnectsAndRetries` - a BYE between the EXAMINE and the FETCH gets a reconnect, and the retry selects again;
  - `Test_StreamMessagesByUIDs_When_ServerSendsByeBetweenBatches_Then_ReconnectsAndDeliversTheRest` - a BYE after the first batch gets a reconnect, the next batch selects the folder on the new session, and the stream does not fetch a delivered UID again;
  - `Test_StreamMessagesByUIDs_When_StreamSpansBatches_Then_SelectsFolderOnce` - all batches of one stream share one EXAMINE, also for a non-ASCII name that go-imap sends in modified UTF-7;
  - `Test_StreamMessagesByUIDs_When_SessionEndedAfterEarlierSelect_Then_ReselectsOnANewSession` - the new session does not keep a selection made before a BYE: the stream logs in again, selects again and delivers each UID once.

  The APPEND retry has these tests:
  - `Test_AppendMessage_When_SessionEndedWhileIdle_Then_ReconnectsAndAppends` - a session that ended with a BYE or a closed socket gets a reconnect before the APPEND;
  - `Test_AppendMessage_When_FailsBeforeBodyIsRead_Then_RetriesOnce` - a transient failure before go-imap reads the body gets one reconnect and one APPEND, and the first connection gets no APPEND;
  - `Test_AppendMessage_When_BodyWasSent_Then_RepairsWithoutRetry` - after go-imap read the body (an empty body too), the call repairs the session and sends no second APPEND;
  - `Test_AppendMessage_When_ServerStopsReadingLiteral_Then_WatchdogTripsAndRepairs` - a failure in the middle of the literal gets a repair and no second APPEND;
  - `Test_AppendMessage_When_RevivedSessionFailsBeforeBodyIsRead_Then_DoesNotReconnectAgain`, `Test_AppendMessage_When_RevivedSessionFailsTransiently_Then_DoesNotReconnectAgain` and `Test_AppendMessage_When_RetryGoesSilent_Then_WatchdogTripsWithoutAnotherReconnect` - one call reconnects a maximum of one time;
  - `Test_AppendMessage_When_RetryReconnectIsRefused_Then_ReturnsReconnectError` - a refused login for the retry returns the reconnect error, and the next call returns the same refusal with no new login;
  - `Test_AppendMessage_When_ReconnectBeforeAppendIsRefused_Then_ReturnsReconnectError` - when the server ended the session while idle and refuses each login attempt, `AppendMessage` returns the reconnect error and sends no APPEND;
  - `Test_AppendMessage_When_ContextEndsDuringRetry_Then_ReturnsContextError` - a context that ends while the retry waits returns its error, with no further reconnect;
  - `Test_AppendMessage_When_ContextEndsDuringReconnectBeforeAppend_Then_ReturnsContextError` and `Test_AppendMessage_When_ContextEndsDuringRetryReconnect_Then_ReturnsContextError` - a context that ends while a login waits returns the context error, not the reconnect error, and no APPEND reaches a session;
  - `Test_AppendMessage_When_RetriedAfterEarlyFailure_Then_WireMatchesADirectAppend` - the retry sends the same flags, date and bytes as an APPEND that needed no retry, and exactly one complete literal reaches the server;
  - `Test_readTracker` - `readTracker` marks a body as read only through `Read`, and it does not offer `WriteTo`, so `io.Copy` cannot move the body without a `Read`;
  - `Test_AppendMessage_When_RepairWasRefusedPermanently_Then_NextAppendSendsNoLogin` and `Test_AppendMessage_When_ReconnectWasRefusedPermanently_Then_LaterAppendsSendNoLogin` - after a permanent refusal, `AppendMessage` sends no LOGIN and no APPEND.
- **`internal/ratelimit`** - token starvation, nil-limiter pass-through, read+write directions independent.
- **`internal/utils`** - `AskConfirm` and its helpers. The tests cover the `-y/--confirm` flow. `AskConfirm` reads each answer from one shared buffered reader.
- **`internal/app`** - sync plan diff (Message-Id intersection), subfolder expansion, delimiter reconciliation, worker pool dispatch.
  The `runSync` and `runShow` tests run end to end against a fake IMAP server that holds mailboxes in memory. These tests cover:
  - the plan-confirm-execute order, and the rule that `--quiet` needs `--confirm`;
  - an idempotent second run, and the delimiter rewrite on both sides;
  - nested folder creation, and one rate-limit budget per direction that all workers share;
  - a read-only source (no write commands, no `\Seen` change);
  - a cancel at each boundary between sync phases;
  - the refusal paths (login, LIST, EXAMINE, CREATE, APPEND);
  - a source that drops the connection on a folder with a throttle phrase in its name, where the folder is still copied (`TestRunSync_When_SourceDropsOnFolderNamedLikeThrottle_Then_FolderIsStillCopied`);
  - a CREATE that gets the Gmail reply `Duplicate folder name`, where the sync completes (`TestRunSync_When_CreateAnswersDuplicateFolderName_Then_SyncCompletes`);
  - a destination that refuses LOGIN in the middle of a run, where the worker sends one refused LOGIN (`TestRunSync_When_DestinationRefusesLoginMidRun_Then_WorkerSendsOneRefusedLogin`);
  - the `ActionSync` and `ActionShow` adapters, which run through a urfave/cli root (the TLS-only dial and the config load errors).
- **`internal/progress`** - interface compliance against the contract `internal/client` expects.
- **`internal/termsafe`** - `Escape` and `Writer` have 100% coverage. The tests are table tests, a fuzz target, `Writer` tests and a test that `Escape` does not allocate memory when nothing needs escaping.
- **Terminal output in `internal/client` and `internal/app`** - end-to-end tests with fake servers use a folder name, a reply and a Message-Id that contain control and bidi characters. They check the escaped output, and they check that the names on the wire stay byte-exact.
- **`cmd/imapsync-go`** - `TestReportError` covers the exit code and the escaped `Error:` line. `TestFetchTag` covers `helpers.fetchTag` against an `httptest` server.

## What we deliberately don't test

If a behaviour is hard to test, test it at integration level. If you cannot do that, record it in this file as a deliberate exclusion and give the reason. These are examples of acceptable exclusions:

- The TLS handshake itself (delegated to stdlib).
- `goreleaser` output (delegated to goreleaser).
- The `urfave/cli/v3` flag parsing wiring in `cmd/imapsync-go/main.go` (declarative; failure mode is a CLI parse error from the library).

### Deliberate exclusions in `internal/client`

- **`client.New`** - the `Test_New_*` tests cover CRAM-MD5, dial failure, LIST refusal, cancel during LOGIN and shared limiters. Only `internal/app` tests cover two branches: the `tls.Dialer` path and the default dial timeout. The `tls.Dialer` test uses a plaintext server, so a handshake error is expected. The standard library handles a successful TLS handshake, so no test covers it.
- **`Client.SetPrefix`, `Client.GetDelimiter`** - trivial one-liners; observable in all fake-server tests that set a delimiter.
- **`Client.SetProgressTracker`** - symmetric with `SetProgressWriter`; both are nil-and-non-nil atomic.Pointer stores already exercised by `SetProgressWriter` tests.
- **`realSleepCtx`** - production sleep implementation. Tests substitute the package-level `sleepCtx` var; the real implementation is stdlib `time.NewTimer` and its correctness is delegated to stdlib.
- **Post-success `ctx.Err()` re-checks** - each check runs after a call succeeds. `withCancel` terminates the connection asynchronously, so the outcome at that boundary is a race. The checks are in these places:
  - `StreamMessagesByUIDs`, at the top of the second and later batches;
  - `CreateMailbox`, after the parent walk;
  - `createParentFolders`, at the top of the loop;
  - `ListMailboxes`, after the LIST;
  - `getFolderSize`, after success.
- **`greet` tie between the context and the timer** - the context and `dialTimeout` can end at the same moment. Both events close the connection, so they race. Both resulting errors describe the failure correctly. No deterministic test can reach the tie. Two tests cover the separate causes:
  - `Test_New_When_ServerNeverGreets_Then_ContextCancelReturns` covers the context;
  - `Test_New_When_ServerNeverGreets_Then_FailsAfterDialTimeout` covers the timer.
- **`greet` failure paths that route the logger of a dropped client** - `greet` calls `routeErrorLog` also when it drops the client, so the reader of that client does not write to stderr. `imapclient.New` creates its logger on `os.Stderr`, and the dropped client is not reachable from a test. A check needs a capture of `os.Stderr` for the whole process. Two tests cover the routing on the success path:
  - `Test_New_ErrorLogFollowsVerbose` covers the logger output of a connected client;
  - `Test_routeErrorLog` covers the escaped lines and the quiet discard.
- **`idleConn` ordering choices** - each of these four orderings closes a window of a few instructions against a concurrent timer callback:
  - `Read` and `Write` stamp the last activity before they count themselves busy;
  - `begin` stamps before it counts the span;
  - `fire` uses `CompareAndSwap` to move the state from open to tripped;
  - `connectAndLogin` stores `c.idle` before `c.c`.

  A deterministic test needs a production hook between two atomic operations. The tests cover the behaviour on both sides of each ordering.
- **`AppendMessage` with a server that is silent between the `{N}` line and the `+` continuation** - go-imap blocks the caller on `<-w.continues`, and a closed socket does not release it. A test of this window would hang. The cause is the same as in the Known limitation in `CLAUDE.md`. Two nearby paths have tests:
  - `Test_AppendMessage_When_ServerNeverAnswersAfterLiteral_Then_WatchdogTripsAndRepairs` covers a server that is silent after the literal;
  - `Test_AppendMessage_When_ServerStopsReadingLiteral_Then_WatchdogTripsAndRepairs` covers a server that stops reading the literal.

### Deliberate exclusions in `internal/app`

- **`runFolderSync` post-append `ctx.Err()` check** - The check stops the loop when an APPEND finishes after the context is canceled. `withCancel` registers a `context.AfterFunc` that calls `Cancel()`, and `Cancel()` terminates the connection asynchronously. An APPEND that finishes at the same time can return nil, not an error. This is a race window, and the test has no deterministic hook to reach it. The `(synced, errors)` result is the same with or without the check. The check saves at most one extra iteration.
- **`app.traceTracker`** - a diagnostic that writes to stderr. It is active only when `IMAPSYNC_TRACE` is set.
- **`ActionSync` verbose `Fetching config...` line** - `ActionSync` writes it to `os.Stdout` before the config loads. It is the only output that does not go through the injected writer.
- **Boundary `ctx.Err()` re-checks** - a cancel must land between two statements with no observable event, so a deterministic test is not possible. `TestRunSync_When_CanceledAtPhaseBoundary_Then_NothingMoreIsSent` and `TestRunSync_When_CanceledDuringAppend_Then_NoFurtherPlanIsStarted` test the end-to-end property. The re-checks are in these places:
  - `runSync`, after the connect errgroup;
  - `runSync`, after the scan progress writer stops;
  - `runSync`, at the top of the dispatch loop;
  - `expandMappingsWithSubfolders`, in the per-mapping loop, after `getSubfolders` and in the per-subfolder loop.

### Deliberate exclusions in `cmd/imapsync-go`

- **`main` and `run` in `cmd/imapsync-go/main.go`** - the signal-to-context wiring and `os.Exit` need a subprocess and real signals. `TestReportError` covers the exit-code mapping through `reportError`. The tests in `cmd/imapsync-go/commands` cover the command tree. A test sends SIGQUIT to its own process to cover `installQuitDump`, with an injected exit function.
- **`helpers.latestTag`** - the function makes a live call to the GitHub API. The tests skip it through `IMAPSYNC_OFFLINE` or `CI`. `TestFetchTag` covers `fetchTag`, which sends the request, against an `httptest` server. The other tests cover only the offline return and the `Version` format.

If the tester finds an untested branch that does not match an existing exclusion, the choice is:

1. Add a test (preferred).
2. If the branch is genuinely untestable in isolation (e.g. network races), propose an integration test or add it to the exclusions list with reasoning.

## Test commands

- `just test` - full suite.
- `go test ./internal/client -run TestFunc` - single test.
- `go test -race ./...` - race detector. Run before declaring done on anything that touches goroutines.
- `go test -cover ./internal/<pkg>` - coverage for one package.
- `go test -coverprofile=cover.out ./... && go tool cover -func=cover.out | sort -k3 -n` - coverage report sorted by % (find the holes fast).

## When a test fails

- Do not "fix" by adjusting the assertion to match observed behaviour. Find the regression.
- If the failure is in a test the developer just wrote and the new code is correct, the test is wrong - fix the test, explain in the developer→architect report.
- If the failure is in a pre-existing test, stop. Report to architect. Do not modify pre-existing tests without explicit instruction.

## Race detector

`just run` runs `go run -race`. If you introduce concurrency, run `go test -race ./...` locally and report the result.
