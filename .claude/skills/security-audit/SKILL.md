---
name: security-audit
description: Apply when auditing changes for security in this repo. Used by the `security` agent. Encodes the threat model (operator trusted, IMAP servers + network partially trusted, no listening sockets) and the standing checklist.
---

# Security audit

Read [.claude/rules/security.md](../../rules/security.md) first.

## Threat model in one paragraph

The operator running the binary is trusted. The IMAP servers and the network between them are partially trusted — they may be slow, hostile, or compromised. The local host must not be put at risk by anything the binary does in response to server input. The binary does not listen; if it did, that would be a separate threat model that does not exist yet.

## Standing checks (run every time)

```sh
go tool govulncheck ./...   # known CVEs in vendored deps
go vet ./...                # stdlib smell test
go tool staticcheck ./...   # additional smells
```

`just check` runs all of these.

## Combined-surface checks

Run these greps on the changed files explicitly — single-file review misses them:

```sh
# IMAP ops bypassing safeCall
git diff main -- 'internal/client/**/*.go' | grep -E '^\+.*c\.Client\.' | grep -v safeCall

# new goroutines
git diff main -- '**/*.go' | grep -E '^\+.*go func'

# new exec / unsafe / reflect
git diff main -- '**/*.go' | grep -E '^\+.*(os/exec|unsafe\.|reflect\.)'

# new fmt/log near "pass" or "Password" or "Pass"
git diff main -- '**/*.go' | grep -E '^\+.*(fmt|log)\.' | grep -i pass

# new InsecureSkipVerify
git diff main -- '**/*.go' | grep -E '^\+.*InsecureSkipVerify'
```

Each hit is a candidate finding. Investigate, then either dismiss with reason or report.

## Reconnect-storm regression check

If `internal/client/client.go` changed:

- `reconnectInterval` still ≥ 10s?
- `maxReconnectAttempts` still ≤ 5?
- `throttledBackoff` still ≥ 5m?

Any weakening makes the binary look like an attacker to the remote — that's both a security and a reputation issue.

## Severity calibration for this repo

- **Critical** — credentials leaked to stdout / log / error message; arbitrary code exec from server input; arbitrary file write from server input.
- **High** — TLS verification disabled by default; new listening socket; race that could land messages in the wrong folder.
- **Medium** — IMAP op bypassing `safeCall`; new goroutine without `ctx`; new direct dep with unaudited CVE history; reconnect protections weakened.
- **Low** — verbose error message that leaks server-side info to the user (not a remote disclosure but harms least-privilege).
- **Info** — style / defence-in-depth suggestion with no concrete attack.

## Reporting

Group findings by file. Within a file, sort by severity desc. For each finding, give location (file:line), one-sentence description, impact paragraph, recommendation. If you find nothing, say so explicitly with the list of things you checked, so the architect can see the audit was performed and not skipped.
