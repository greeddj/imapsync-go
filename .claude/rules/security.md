# Security rules

Audit scope is **this binary, plus what it can do to the host and to the IMAP servers it talks to**.

## Standing checklist

Run through these on every security review:

1. **`go tool govulncheck ./...`** - must be clean. If a finding is unreachable, document why (file + line) rather than ignoring silently. Part of `just check`.
2. **Dependency surface** - `go.mod` direct deps reviewed. Any new direct dep needs justification: what does it do, who maintains it, does it have a CVE history. Vendored, so the source is always inspectable under `vendor/`.
3. **TLS** - connections to IMAP must verify the server cert by default. Any `InsecureSkipVerify` requires an opt-in config flag *and* a runtime warning.
4. **Credentials** - passwords come from config or env. Never logged, never printed to stdout, never included in error messages routed back to the user. Audit `fmt.*` / `log.*` call sites near credential handling. The error for an unsupported `auth` value must not show the value, because an operator can paste a secret into this field. `config.New` names the block and the supported values. `client.New` returns a fixed error. Neither error shows the value.
5. **Path handling** - folder names come from remote IMAP servers. They must never be used to construct local filesystem paths without sanitisation. Today the binary does not write user-controlled paths to disk; if that changes, this is a hard review point.
6. **Command exec** - there is no `os/exec` in the runtime path. Adding any is a review trigger.
7. **Network surface** - the binary connects out to user-configured IMAP servers. It does not listen. If a listening socket is added, that is a major architectural change that needs an explicit threat model.
8. **Input parsing** - IMAP responses come from a partially-trusted server. The `go-imap` library does the parsing; trust boundary is at that library's API. Any code that interprets server-supplied strings (folder names, message ids, headers) must not pass them to anything sensitive unsanitised. The terminal counts as sensitive: see item 11.
9. **Resource exhaustion** - rate limits and worker clamps are the defence. Any code path that allocates per-message must be bounded by the batch size (`uidFetchBatchSize = 500`). A message count from the server, such as EXISTS or STATUS MESSAGES, must not set the size of an allocation without a limit. `FetchMessageMap` and `FetchMessageCopies` limit the size hint of their map to `maxMessageMapHint`. A server can also send many copies of one Message-Id. The scan sorts each group one time, after the FETCH, and the copy matcher sorts the copies. So the cost for k copies is O(k log k), not quadratic. `printCaseVariantDestinations` warns about destination names that differ only in letter case. It groups the names with maps and sorts each group one time. It finds each name in the sorted destination LIST with a binary search. So its cost for n names is O(n log n). A mirror run takes these names from the source server, so a hostile server can list many spellings of one name. Under the one-copy rule, the worker downloads all standby copies of a waiting message from one folder, in `uidFetchBatchSize` batches under the shared read limiter. A throttle stop ends a batch in flight: `StreamMessagesByUIDs` closes the source session, so the worker does not download the rest of the batch. A destination that gave up makes the worker download no body, because `EnsureSession` fails before the source is read.
10. **Reconnect storms** - `reconnectInterval = 10s`, `maxReconnectAttempts = 5`, `throttledBackoff = 5m` and `reviveInterval = 5m` are the protections. Any change makes it more likely that the remote server treats the client as an attacker. The give-up and revive gate in `reconnect` has these rules:
    - Each Client makes at most one login attempt per `reviveInterval` after a `ClassTransient` or `ClassThrottled` give-up. During `sync`, one side has at most 11 Clients: one planning Client and at most 10 workers.
    - The gate runs under `c.mu`, before any wait, logout or dial. Only `New` and `reconnect` call `connectAndLogin`, so every later login goes through the gate.
    - A `ClassPermanent` or `ClassUnknown` give-up is final for the run. Each later call returns the stored error and does not dial.
    - A reply that reports a locked, disabled, suspended or blocked account is `ClassPermanent`. A login attempt on a locked account can make the lock longer.
    - A login reply that reports a temporary outage of the authentication service (for example `[UNAVAILABLE] Temporary authentication failure`) is `ClassTransient`. `reconnect` retries it with backoff, up to `maxReconnectAttempts`. The locked-account rule comes first, so a locked-account reply never reaches this rule.
    - `EnsureSession` goes through the gate. It reaches `reconnect` only through `reconnectSession`. It sends no command on a working session. While the gate refuses, it returns the stored give-up and sends no dial and no LOGIN. After `reviveInterval`, it makes the one revive login, as the next `AppendMessage` would.

    A change to any of these values or rules is a security review point.
11. **Terminal output** - text from an IMAP server (folder names, reply text in errors, Message-Ids, go-imap log lines) can contain control, bidi and invisible characters. Such text can move the cursor, clear the screen or make a false status line. A new print site must show it through `termsafe.Escape`. A `%q` verb is accepted only at the existing sites, because `%q` keeps graphic invisible runes (see "Known limitations"). Escape where the program prints the text, never in values that go back to a server. The list of folders that cannot be synced, the skip lines and the list of folders that `show` cannot read hold reply text. Their print sites escape the folder name and the reason. The left-out lines, the rename notices and the "already exists and holds" lines also show folder names from LIST. Their print sites escape the names. A left-out name is raw server text, and it can hold an invalid UTF-8 byte. The case-variant warning, the mapping override line and the standby line show folder names and Message-Ids. Their print sites escape them. The `errSourceConflict` error holds a source name, and `reportError` escapes it. The throttle stop line shows the reply text of the server, and `printThrottleStop` escapes it. The labels in that line are operator text.

## Combined-surface review

A change can be safe per-file and unsafe in combination. Look for:

- A new error path that bypasses `safeCall` (skips reconnect bookkeeping + cancellation).
- A new IMAP op that reads or writes the message store *without* going through `selectIfNeeded` (could land writes in the wrong folder if a race happens during reconnect).
- New goroutines that don't honour `ctx` - Ctrl-C must terminate the program promptly. Goroutines outliving the program are a DoS on the user's terminal.
- Per-connection state that should be global (e.g. accidentally creating a per-Client rate limiter - defeats the global cap).
- A new print site that shows server text without `termsafe.Escape`. A `%q` verb is accepted only at the existing sites in `internal/app/sync.go`. Check `fmt.*`, `log.*`, `pw.Log`, tracker messages and error output.
- Server text, such as a folder name, in the wrapper text of an error that `classifyError` reads. `classifyError` matches text on the leaf error only, so wrapper text cannot set the class. A folder name that contains `exceeded quota` or `not logged in` must not change a retry decision. Wrap an error with `%w`. A `%v` or `%s` of an error makes a new leaf that holds the wrapper text. The reply of a server can repeat a folder name. This is a known limitation (see "A refusal that repeats a folder name can set its class" in "Known limitations").
- A second APPEND after go-imap started to read the body, for example when `AppendMessage` moves into `safeCall`. The server can already have the message, so a second APPEND makes a duplicate on the destination. `AppendMessage` retries one time, only when go-imap did not start to read the body.
- A planned UID that a session uses without a UIDVALIDITY check. A UID holds only for one UIDVALIDITY, so after a change the UID can point to another message. `StreamMessagesByUIDs` checks the value on every attempt of every batch. `recheckDestination` checks it after each source scan. When the plan holds a value, a server that sends another value, or none, stops the copy of that folder. A plan value of 0 skips the check, because the server sent no value at scan time. A new path that fetches by planned UID must pass the value of the plan. A standby stream passes the value of its own source folder, not the value of the plan.
- A UID 0 in a plan. go-imap writes UID 0 as `*` in a UID set, and `*` selects the message with the largest UID, not the planned message. The source scan fails for a message that has a Message-Id and no UID (`errMissingUID`). The destination scan only counts such a copy.
- An APPEND of an empty or missing body. An empty message has no Message-Id, so each run adds one more message to the destination. `AppendMessage` returns `ErrEmptyBody` or `ErrMissingBody` before it sends any command.
- A raw LIST name that is not valid modified UTF-7, in a SELECT, EXAMINE, CREATE or APPEND, in a mapping or in a plan. `MailboxNames` leaves such names out. `InvalidMailboxNames` is only for the print sites and for the check of the destination LIST.
- A plan rule that comes from a server capability. The rule for X-GM-EXT-1 on the destination, which `client.New` reads after LOGIN, may only remove copies from a sync plan. It never adds one. The copies that it removes can stand by for a planned copy only when `AppendMessage` refuses the body of that planned copy as empty or missing. Only the capability of the destination counts. A change to the trigger, or to the copies that the rule removes, is a review point.
- A server value that reaches IMAP after escaping: a SELECT, EXAMINE, STATUS, CREATE or APPEND argument, plan data, a mapping, a map key, or an error value that `classifyError` or `isAlreadyExistsErr` reads. Escaping changes the bytes, so the name no longer matches the real folder.

## Known limitations

- **go-imap logs before `routeErrorLog` runs.** `imapclient.New` creates its logger on `os.Stderr`. It reads the greeting and the implicit CAPABILITY probe before `routeErrorLog` can run. A line that go-imap logs in that window reaches stderr unescaped, in every mode.
  - In go-imap v1.2.1, such lines are parse errors and network errors. A parse error holds server text in only two places: one of the characters `(`, `{` or `"`, and a literal length that `strconv` quotes. Neither can carry a control sequence.
  - Audit this again on each go-imap upgrade.
  - A full fix needs go-imap v2, or a fork with a logger option. A patch of `vendor/` does not last, because `just deps` rewrites it.
- **go-imap v1.2.1 panics on some untagged responses.** A panic on the reader goroutine stops the process. The run loses no mail, and the next run continues the sync. A fix needs go-imap v2. A hostile server can cause a panic in these cases:
  - An untagged `EXPUNGE` or `FETCH` without a message number. The reader goroutine indexes an empty slice.
  - An untagged `FETCH` with a message number and no item after the word `FETCH`, for example `* 5 FETCH`. go-imap removes the name `FETCH`, so only the message number stays:
    - the FETCH handler (`responses.Fetch`, `vendor/github.com/emersion/go-imap/responses/fetch.go:21-30`) checks only that the response has one or more fields, then reads the second field;
    - the handler for data that no command asked for (`handleUnilateral`, `vendor/github.com/emersion/go-imap/client/client.go:439-441`) reads the second field and does not check the number of fields;
    - while the UID FETCH of a body stream runs, `bodyFetchHandler` (`internal/client/fetch.go`) gets the response first and refuses it. go-imap then ends that UID FETCH with an error. Only that batch fails, and the stream continues with the next batch;
    - at all other times, one of the two go-imap handlers gets the response and panics. Examples are `scanMessageIDs`, the size scan of `ListMailboxes`, all other commands and the time between commands;
    - go-imap v1 has no API for a handler that stays between commands. So a check in our scan code cannot stop this panic.
  - A FETCH or UID FETCH whose command write fails. go-imap then closes the response channel. A handler that already runs can then send on the closed channel, and this send panics. A hostile server must reset the connection at that exact moment.
- **go-imap v1.2.1 reads a literal of any announced size.** The server sets the size in `{N}`, up to 4 GiB, and go-imap allocates it. A hostile server can exhaust the memory. `imapclient` has no setting for the maximum literal size. A fix needs go-imap v2.
- **A refusal before the continuation request hides the reason.** When the destination refuses an APPEND with a tagged NO before it sends `+`, go-imap v1 returns only `imap: cannot send literal: no continuation request received`. It drops the reply. The class is `ClassUnknown`, so the refusal counts as one refused APPEND and the run goes on. A throttle reply in this form does not stop the run. A fix needs go-imap v2.
- **A refusal that repeats a folder name can set its class.** `classifyError` reads the reply text. Some servers repeat the folder name in a refusal, for example the Dovecot reply `Mailbox doesn't exist: NAME`.
  - A throttle phrase in NAME, such as `Quota exceeded`, gives `ClassThrottled`. A person or a program can delete or rename a folder during the run. Then a refusal of EXAMINE or APPEND can stop the whole run with the `Sync stopped` line. A refusal in the CREATE step or in the check after CREATE can also stop it.
  - In the planning scan, such a refusal of an unlisted parent is a folder failure, not a skip.
  - A transient phrase in NAME, such as `Not logged in`, costs one reconnect in a Message-Id scan. In the copy, it costs one reconnect for each batch of `uidFetchBatchSize` UIDs and one for `ExistingUIDs`. An APPEND refusal that comes after go-imap sends the body costs one reconnect for each message, because `AppendMessage` repairs the session. `reconnectInterval` keeps the reconnects of one Client at least 10 seconds apart.
  - The run loses no mail. The next run makes a new plan from the current folders.
  - A full fix needs go-imap v2. go-imap v1 keeps the response code only in the status response, and `Select`, `Create` and `Append` return only the text.
- **UTF-8 terminals only.** `termsafe.Escape` assumes a UTF-8 terminal. A terminal that acts on 8-bit C1 bytes inside multi-byte runes is out of scope.
- **Backslash is not escaped.** A folder named `\x1b` looks the same as a folder that contains an ESC character. Both forms are inert text.
- **Existing `%q` sites keep graphic invisible runes.** The `%q` sites in `internal/app/sync.go` are safe against control sequences. They keep runes such as `\u3164`, so a folder name can look like another name in those lines. The plan preview uses `Escape`.

## What to report

For every finding, report:

- **Severity** - info / low / medium / high / critical.
- **Location** - file:line.
- **Description** - what the issue is in one sentence.
- **Impact** - what can go wrong, with which actor as the threat.
- **Recommendation** - concrete fix.

Group findings by file. Lead with the highest severity. If there are zero findings, say so explicitly with the list of things you checked.
