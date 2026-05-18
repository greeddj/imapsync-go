---
name: coverage-audit
description: "Apply when auditing test coverage on changed packages. Used by the `tester` agent. Encodes the project's stance: branch and invariant coverage matter more than total %, deliberate exclusions must be recorded with reasoning."
---

# Coverage audit

Read [.claude/rules/testing.md](../../rules/testing.md) first.

## Commands

```sh
go test -cover -coverprofile=cover.out ./internal/<pkg>
go tool cover -func=cover.out | sort -k3 -n   # bottom of the list = lowest coverage
go tool cover -html=cover.out -o cover.html   # visual gap map, open locally if needed
go test -race ./internal/<pkg>                 # required for any concurrent code
```

For the whole tree at once:

```sh
go test -coverprofile=cover.out ./... && go tool cover -func=cover.out | sort -k3 -n
```

## How to read the output

- **0% functions** — either dead code (delete in a separate change) or an untested public surface (gap to fill).
- **functions < 50%** — usually means happy path is tested, error/edge cases aren't. Find which branch is untested with the HTML view.
- **functions > 90%** — fine. Don't chase 100% on getters and constructors.

## What matters more than %

For every focus invariant the architect named, find the test that exercises it. If the invariant is "X must happen", there should be a test that fails when X doesn't happen. Run the test, then mentally remove the code that enforces X, then re-read the test — would it still pass? If yes, the test isn't covering the invariant; it's covering the side effect.

## Deliberate exclusions

If a branch is genuinely untestable at unit level, document it. Acceptable reasons:

- Requires a real IMAP server / real TLS handshake / real DNS.
- Tests the absence of a race (statistical, not deterministic).
- Re-tests stdlib or vendored library behaviour.

Format for proposing an exclusion to the architect:

```
Symbol: internal/<pkg>.<func>
Branch: <which branch>
Why not tested at unit level: <one sentence>
Suggested CLAUDE.md note: "Deliberate test exclusion — <branch> is …, exercised via <integration-mechanism-or-nothing>"
```

The architect decides whether to accept and where to record.

## Don't

- Don't lower coverage by deleting a test "to clean up".
- Don't game coverage by adding tests that import the function but assert nothing meaningful.
- Don't modify pre-existing tests without architect approval.
