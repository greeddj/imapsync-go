---
name: imapsync-check
description: Run static-analysis quality gates for the imapsync-go Go repo via Justfile — lint, vet, staticcheck, govulncheck, fieldalignment. Use when the user asks to lint the code, run quality gates, diagnose a single failing analyzer, or verify changes before commit/PR. Do NOT use for running tests (see imapsync-test) or builds (see imapsync-build).
---

# imapsync-go — Quality Gates

## When to use

- "прогони линтер" / "lint" / "golangci"
- "проверь код" / "запусти проверки" / "quality gates"
- Diagnose a single analyzer: vet, staticcheck, govulncheck, fieldalignment
- Pre-commit / pre-PR validation of static checks (без тестов)

## Commands

| Intent | Command |
|---|---|
| Full lint pass | `just lint` |
| All static gates in one go | `just check` |
| Lint + check + tests, then run | `just run` |

There are no per-analyzer subtargets in this repo's `Justfile` — `just check` runs the four analyzers as one block. For a tight loop on a single analyzer, drop down to `go tool` directly.

## Direct Go equivalents (tight loop)

When iterating on a fix, prefer the single failing analyzer over the full chain:

```sh
go vet ./...
go tool staticcheck ./...
go tool govulncheck ./...
go tool fieldalignment ./...
```

The `go tool` invocations work because `staticcheck`, `govulncheck`, and `fieldalignment` are declared as Go `tool` directives in `go.mod`.

## Repo-specific gotchas

- **`just check` runs `just deps` first.** That means it executes `go mod tidy && go mod vendor` before the analyzers. If you only want analysis without touching `vendor/`, use the `go tool ...` commands directly. See `imapsync-deps` if dep mutation is intentional.
- **`just lint` is independent** of `just check`. Run both for full pre-commit coverage. `just run` chains `check + lint + test + go run -race`.
- **`fieldalignment`** can suggest reordering fields in public types. Don't blindly apply `-fix` to `Client`, `Config`, `Credentials`, `DirectoryMapping`, `MailboxInfo`, `FolderSyncPlan`, `SyncSummary` — they are part of observable surface area. See `imapsync-deps` for intentional fixes.
- **`govulncheck`** failures are real and not masked. If a vendored dep has an advisory, decide whether to bump or accept; the gate will block the chain.

## Workflow

1. Classify scope: single analyzer failure → run that `go tool ...` directly; broad change → `just check`.
2. Iterate on the failing gate only; expand to `just check` once it passes.
3. After code changes, run `just lint` separately — it's not part of `just check`.
