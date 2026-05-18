---
name: doc-sync
description: Apply when verifying that comments and docs match the current code state. Used by the `tech-writer` agent. Encodes the project's WHY-only comment rule and the doc surfaces that must stay in sync (CLAUDE.md, README.md, config.example.{json,yaml}).
---

# Doc sync

Read [.claude/rules/docs.md](../../rules/docs.md) first.

## What stays in sync with code

| Doc surface | Source of truth for its content |
|---|---|
| CLAUDE.md "Common commands" | `Justfile` (`just --list`) |
| CLAUDE.md "Architecture" sections 1–5 | `internal/` layout and behaviour |
| CLAUDE.md "Conventions" | `internal/config/config.go`, CLI flag definitions, vendored state |
| README.md install/run | binary name, Justfile targets |
| README.md examples | actual CLI flags and config examples |
| `config.example.json` | every field in `internal/config/config.go`, populated with realistic values |
| `config.example.yaml` | same fields and same values as the JSON, just YAML-formatted |

## Comment audit one-liner

For changed Go files:

```sh
git diff main -- '*.go' | grep -E '^\+\s*//'
```

For each match, apply the checklist in [.claude/rules/docs.md](../../rules/docs.md) "Comment audit checklist".

## CLI surface check

```sh
go run ./cmd/imapsync-go --help
go run ./cmd/imapsync-go sync --help
go run ./cmd/imapsync-go show --help
```

Compare against README.md and CLAUDE.md mentions. If a flag is documented but missing from `--help`, docs are stale.

## Config surface check

```sh
grep -E '^\s*[A-Z][a-zA-Z]*\s+' internal/config/config.go | grep 'json:\|yaml:'
```

Every field tagged here must appear in both example files.

## When the two example configs disagree

They must have the same set of fields and equivalent values. If they don't, fix the one that drifted — usually you can infer which is current by checking the most recent commit that touched `internal/config/config.go`.

## When code and docs disagree and you can't tell which is right

Do not silently pick a winner. Report to architect with:

- Where docs say X (file:line).
- Where code now does Y (file:line).
- Which one is consistent with the rest of the system, in your best judgement.
- Your recommendation, but flagged as recommendation, not action.
