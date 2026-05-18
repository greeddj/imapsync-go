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
8. **Input parsing** — IMAP responses come from a partially-trusted server. The `go-imap` library does the parsing; trust boundary is at that library's API. Any code that interprets server-supplied strings (folder names, message ids, headers) must not pass them to anything sensitive unsanitised.
9. **Resource exhaustion** — rate limits and worker clamps are the defence. Any code path that allocates per-message must be bounded by the batch size (`uidFetchBatchSize = 500`).
10. **Reconnect storms** — `reconnectInterval = 10s`, `maxReconnectAttempts = 5`, `throttledBackoff = 5m` are the protections. Any change weakens defence-in-depth against being treated as an attacker by the remote.

## Combined-surface review

A change can be safe per-file and unsafe in combination. Look for:

- A new error path that bypasses `safeCall` (skips reconnect bookkeeping + cancellation).
- A new IMAP op that reads or writes the message store *without* going through `selectIfNeeded` (could land writes in the wrong folder if a race happens during reconnect).
- New goroutines that don't honour `ctx` — Ctrl-C must terminate the program promptly. Goroutines outliving the program are a DoS on the user's terminal.
- Per-connection state that should be global (e.g. accidentally creating a per-Client rate limiter — defeats the global cap).

## What to report

For every finding, report:

- **Severity** — info / low / medium / high / critical.
- **Location** — file:line.
- **Description** — what the issue is in one sentence.
- **Impact** — what can go wrong, with which actor as the threat.
- **Recommendation** — concrete fix.

Group findings by file. Lead with the highest severity. If there are zero findings, say so explicitly with the list of things you checked.
