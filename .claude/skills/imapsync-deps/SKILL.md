---
name: imapsync-deps
description: Run mutating maintenance commands for the imapsync-go Go repo — go mod tidy/vendor sync and automated fixers. Use ONLY when the user explicitly asks to update dependencies, re-vendor, or apply automated code fixes. These commands rewrite tracked files (vendor/, go.mod, go.sum, source files) — do not invoke them as part of routine validation.
---

# imapsync-go — Dependency Sync & Auto-Fix (mutating)

## When to use

- "обнови зависимости" / "update deps" / "re-vendor"
- "примени fieldalignment -fix" / "apply auto-fix"
- After `go get` of a new module, before lint

**Do not run these as part of routine checks.** They mutate tracked files. Note that `just check` and `just build*` already chain `just deps` — that's intentional but still mutating; if the user wants strictly read-only validation, drop down to `go vet` / `go tool ...` directly (see `imapsync-check`).

## Commands

| Intent | Command | Mutates |
|---|---|---|
| Sync go.mod and vendor/ | `just deps` | `go.mod`, `go.sum`, `vendor/` |
| Apply `fieldalignment -fix` | `go tool fieldalignment -fix ./...` | source files across the repo |
| Apply `go fix` | `go fix ./...` | source files across the repo |

`just deps` runs:
```
go mod tidy
go mod vendor
```

There is no `just fix` target in this repo — the auto-fixers are invoked directly via `go tool` / `go fix`.

## Adding a new direct dependency

1. `go get <module>@<version>`.
2. `just deps` to sync `vendor/`.
3. `just lint && just check` to confirm.

This repo does not use a `depguard` allowlist (unlike some sister projects), so no `.golangci.yaml` allowlist edit is required when adding deps.

## fieldalignment -fix caveats

`fieldalignment -fix` reorders struct fields. Normally safe for internal types, but be cautious with these public/observable types:

- `internal/client/client.go`: `Client`, `MailboxInfo`, `ProgressWriter` / `ProgressTracker` (interfaces, not subject to fieldalignment, but listed for awareness).
- `internal/config/config.go`: `Config`, `Credentials`, `DirectoryMapping` — these mirror the JSON/YAML config schema. Field reordering is invisible to JSON/YAML decoding, but be deliberate.
- `internal/app/sync.go`: `FolderSyncPlan`, `SyncSummary`.

Run `go tool fieldalignment ./...` first to see suggestions, then decide whether `-fix` makes sense package-by-package.

## Workflow

1. State explicitly to the user that this is a mutating operation before running.
2. Run the targeted command.
3. Show `git diff --stat` afterward so the user can review scope.
4. Suggest `just lint && just test` to confirm nothing regressed.
