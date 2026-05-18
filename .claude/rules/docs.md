# Documentation rules

## What is documentation here

- [CLAUDE.md](../../CLAUDE.md) — the architectural contract for both humans and Claude Code. Source of truth for layering, invariants, conventions.
- [README.md](../../README.md) — user-facing: install, configure, run, examples.
- `config.example.json` / `config.example.yaml` — executable docs. Every config field must appear in both with realistic values.
- godoc comments — only on identifiers used across packages and only when the identifier name does not explain itself.
- WHY-comments inside functions — only where the reasoning is non-obvious (a hidden invariant, a workaround, a deliberate surprise).

## What documentation must reflect

- The current file & symbol names (rename in code = rename in docs).
- The current set of CLI flags, config fields, and Justfile targets.
- The current architectural invariants in CLAUDE.md "Architecture" — if behaviour changed, this section is updated in the same change set.

## What documentation must not contain

- Task / ticket references (PR numbers, issue ids, "added for X").
- Implementation diary entries ("first we tried…").
- Marketing language. Be precise; users reading docs want to know what the tool does, not how excited the author is.
- WHAT-descriptions of code that the code itself already shows.
- Stale examples. If a config field was removed or renamed, both `config.example.*` files lose / gain it.

## Comment audit checklist

For each comment in the diff:

1. Is it WHY or WHAT? — WHAT comments go.
2. Does it reference the current task or PR? — that text goes.
3. Does it describe a now-removed behaviour? — fix or delete.
4. Is the identifier rename-safe? — comment must not reference an old name.
5. Could the reader infer the same information from the identifier and the function signature? — delete the comment.

## Doc audit checklist

For each docs file in the diff:

1. Does every CLI flag mentioned still exist? (`go run ./cmd/imapsync-go --help` is the oracle.)
2. Does every Justfile target mentioned still exist? (`just --list`.)
3. Does every config field mentioned still exist? (Compare against `internal/config/config.go`.)
4. Is every code-block path correct? (`Read` the path; if it 404s, fix.)
5. If CLAUDE.md "Architecture" was touched, do the numbered sections still match `internal/`?

If the audit surfaces a mismatch and the cause is unclear (docs wrong vs. code wrong), do not silently rewrite — report to architect.
