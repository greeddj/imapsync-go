# Multi-agent workflow

Authoritative description of how `architect`, `developer`, `tester`, `security` and `tech-writer` cooperate on this repo.

## Roles in one line

- **architect** (opus) — gatekeeper, planner, reviewer of architecture. Sole entry point for non-trivial changes. May reject. Delegates to others.
- **developer** (sonnet) — implements what architect specifies; writes code + comments + unit tests; runs targeted `just lint` / `just test` on changed packages.
- **tester** (sonnet) — runs full test suite, audits coverage, identifies missing cases, marks deliberate exclusions in `CLAUDE.md`.
- **security** (opus) — CVE / dependency audit; static review of attack surface (auth, parsing, network, file I/O, command exec); reasons about combined risk that a single-file review would miss.
- **tech-writer** (sonnet) — verifies comments match the project rule "default to no comments, only WHY-comments"; refreshes `CLAUDE.md` and `README.md` when behaviour changed.

## Default flow — architect-led with delegation

Used when the architect agent has the `Agent` tool available. Main thread only kicks off the architect and receives the final report.

```
main thread → architect → developer → architect → tester → architect → security → architect → (developer/tester loop if needed) → tech-writer → architect → main thread
```

Step by step:

1. **main thread** distils the user's intent into a concrete, file-grounded brief and hands it to `architect`.
2. **architect** reads `CLAUDE.md`, the directories the change touches, and `git log`. Validates the brief against the layering rules in [architecture.md](architecture.md). Three outcomes:
   - **reject** — back to main thread with the reason and a counter-proposal.
   - **clarify** — ask main thread one focused question.
   - **proceed** — write a precise implementation brief for `developer` (files to touch, contracts to preserve, tests to add).
3. **developer** implements, writes WHY-comments only (see [go-style.md](go-style.md)), adds unit tests, runs `just lint` and `just test` on the changed packages, and reports a structured diff + test summary to `architect`.
4. **architect** reviews the diff against the original brief. Either sends a correction back to `developer` or proceeds.
5. **architect** writes a test brief for `tester` (which packages to focus on, which edge cases to verify, which invariants must be exercised).
6. **tester** runs the full suite, computes coverage on changed packages, lists gaps. For each gap: either propose a new test (and write it) or recommend that `CLAUDE.md` records the deliberate exclusion with a reason. Reports back to `architect`.
7. **architect** routes failures back to `developer`, then back through `tester`, until clean.
8. **architect** calls `security`. Security audits the diff, the surrounding subsystem, and the combination of changes for new attack surface (see [security.md](security.md)). Findings go back to `architect`.
9. **architect** loops `developer`/`tester` again if security flagged anything.
10. When the code is at "written, won't get better" level, `architect` calls `tech-writer` to verify/update comments and docs.
11. **tech-writer** confirms, `architect` reports completion back to main thread with: summary, files changed, test coverage delta, security verdict, doc updates.

## Fallback flow — main-thread orchestration

If subagents cannot call `Agent` themselves (sandboxed mode), main thread plays the architect's dispatcher role:

- main thread invokes `architect` for analysis and a written brief → receives brief.
- main thread invokes `developer` with that brief → receives diff/report.
- main thread invokes `architect` for review of the diff → receives go/no-go + test brief.
- main thread invokes `tester` with test brief → receives report.
- main thread invokes `architect` for review of test report → receives go/no-go.
- main thread invokes `security` → receives report.
- main thread invokes `architect` for final review and doc brief.
- main thread invokes `tech-writer` → receives confirmation.
- main thread reports back to user.

The semantics are identical; only who holds the pen changes.

## Hard rules

- The architect is the **only** agent that talks to the main thread mid-flight. Other agents always report to whoever called them.
- The architect never writes code — it only reads, plans, reviews, delegates.
- The developer never escalates scope on its own. If something outside the brief looks wrong, it reports and asks; it does not silently refactor.
- The tester never deletes or weakens existing tests without architect approval.
- The security agent is non-blocking for trivial doc/test-only changes; the architect may skip it for those.
- The tech-writer never invents behaviour. If docs and code disagree, it reports the disagreement to the architect rather than picking a winner.
