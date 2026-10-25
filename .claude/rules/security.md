# Security rules

Audit scope is **this binary, plus what it can do to the host and to the IMAP servers it talks to**.

## Standing checklist

Run through these on every security review:

1. **`go tool govulncheck ./...`** — must be clean. If a finding is unreachable, document why (file + line) rather than ignoring silently. Part of `just check`.
2. **Dependency surface** — `go.mod` direct deps reviewed. Any new direct dep needs justification: what does it do, who maintains it, does it have a CVE history. Vendored, so the source is always inspectable under `vendor/`.
3. **TLS** — connections to IMAP must verify the server cert by default. Any `InsecureSkipVerify` requires an opt-in config flag *and* a runtime warning.
4. **Credentials** — passwords come from config or env. Never logged, never printed to stdout, never included in error messages routed back to the user. Audit `fmt.*` / `log.*` call sites near credential handling.
5. **Path handling** — folder names come from remote IMAP servers. They must never be used to construct local filesystem paths without sanitisation. Today the binary does not write user-controlled paths to disk; if that changes, this is a hard review point.
6. **Command exec** — there is no `os/exec` in the runtime path. Adding any is a review trigger.
7. **Network surface** — the binary connects out to user-configured IMAP servers. It does not listen. If a listening socket is added, that is a major architectural change that needs an explicit threat model.
8. **Input parsing** - IMAP responses come from a partially-trusted server. The `go-imap` library does the parsing; trust boundary is at that library's API. Any code that interprets server-supplied strings (folder names, message ids, headers) must not pass them to anything sensitive unsanitised. The terminal counts as sensitive: see item 11.
9. **Resource exhaustion** — rate limits and worker clamps are the defence. Any code path that allocates per-message must be bounded by the batch size (`uidFetchBatchSize = 500`).
10. **Reconnect storms** — `reconnectInterval = 10s`, `maxReconnectAttempts = 5`, `throttledBackoff = 5m` are the protections. Any change weakens defence-in-depth against being treated as an attacker by the remote.
11. **Terminal output** - text from an IMAP server (folder names, reply text in errors, Message-Ids, go-imap log lines) can contain control, bidi and invisible characters. Such text can move the cursor, clear the screen or make a false status line. A new print site must show it through `termsafe.Escape`. A `%q` verb is accepted only at the existing sites, because `%q` keeps graphic invisible runes (see "Known limitations"). Escape where the program prints the text, never in values that go back to a server.

## Combined-surface review

A change can be safe per-file and unsafe in combination. Look for:

- A new error path that bypasses `safeCall` (skips reconnect bookkeeping + cancellation).
- A new IMAP op that reads or writes the message store *without* going through `selectIfNeeded` (could land writes in the wrong folder if a race happens during reconnect).
- New goroutines that don't honour `ctx` — Ctrl-C must terminate the program promptly. Goroutines outliving the program are a DoS on the user's terminal.
- Per-connection state that should be global (e.g. accidentally creating a per-Client rate limiter — defeats the global cap).
- A new print site that shows server text without `termsafe.Escape`. A `%q` verb is accepted only at the existing sites in `internal/app/sync.go`. Check `fmt.*`, `log.*`, `pw.Log`, tracker messages and error output.
- A server value that reaches IMAP after escaping: a SELECT, EXAMINE, STATUS, CREATE or APPEND argument, plan data, a mapping, a map key, or an error value that `classifyError` or `isAlreadyExistsErr` reads. Escaping changes the bytes, so the name no longer matches the real folder.

## Known limitations

- **go-imap logs before `routeErrorLog` runs.** `imapclient.New` creates its logger on `os.Stderr`. It reads the greeting and the implicit CAPABILITY probe before `routeErrorLog` can run. A line that go-imap logs in that window reaches stderr unescaped, in every mode.
  - In go-imap v1.2.1, such lines are parse errors and network errors. A parse error holds server text in only two places: one of the characters `(`, `{` or `"`, and a literal length that `strconv` quotes. Neither can carry a control sequence.
  - Audit this again on each go-imap upgrade.
  - A full fix needs go-imap v2, or a fork with a logger option. A patch of `vendor/` does not last, because `just deps` rewrites it.
- **go-imap v1.2.1 panics on some untagged responses.** An untagged `EXPUNGE` or `FETCH` without a message number makes the reader goroutine index an empty slice. A hostile server can stop the process this way. No mail is lost, and the next run continues the sync. A fix needs go-imap v2.
- **go-imap v1.2.1 reads a literal of any announced size.** The server sets the size in `{N}`, up to 4 GiB, and go-imap allocates it. A hostile server can exhaust the memory. `imapclient` has no setting for the maximum literal size. A fix needs go-imap v2.
- **UTF-8 terminals only.** `termsafe.Escape` assumes a UTF-8 terminal. A terminal that acts on 8-bit C1 bytes inside multi-byte runes is out of scope.
- **Backslash is not escaped.** A folder named `\x1b` looks the same as a folder that contains an ESC character. Both forms are inert text.
- **Existing `%q` sites keep graphic invisible runes.** The `%q` sites in `internal/app/sync.go` are safe against control sequences. They keep runes such as `\u3164`, so a folder name can look like another name in those lines. The plan preview uses `Escape`.

## What to report

For every finding, report:

- **Severity** — info / low / medium / high / critical.
- **Location** — file:line.
- **Description** — what the issue is in one sentence.
- **Impact** — what can go wrong, with which actor as the threat.
- **Recommendation** — concrete fix.

Group findings by file. Lead with the highest severity. If there are zero findings, say so explicitly with the list of things you checked.
