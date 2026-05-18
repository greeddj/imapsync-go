---
name: idiomatic-go-review
description: Apply when reviewing Go code in this repo for idiom, allocation cost, struct layout, error wrapping, concurrency safety. Triggered by review-style requests on `.go` files, by the `architect` agent when forming verdicts, and by the `developer` agent self-checking before reporting. Loads the project's Go-style rule set.
---

# Idiomatic Go review

You are reviewing Go in `imapsync-go`. The conventions are stricter than the community defaults — read [.claude/rules/go-style.md](../../rules/go-style.md) for the full list before reviewing.

## Fast checklist (apply in order)

1. **Comments** — are there any WHAT-comments? Any TODOs referencing the current PR / task / issue? Any multi-line blocks describing the code? All three are violations.
2. **Allocations** — any `io.ReadAll` on something that could stream? Any `fmt.Sprintf` in a loop? Any `[]byte ↔ string` round-trip? Any slice/map without a `make(..., n)` when `n` is knowable?
3. **Struct layout** — fields ordered by descending size to satisfy `fieldalignment`?
4. **Error wrapping** — `fmt.Errorf("...: %w", err)` everywhere; sentinel errors as package-level `var Err...`?
5. **Error classification** — new IMAP ops use `safeCall` and route errors through `classifyError`? No `strings.Contains(err.Error(), ...)` above the `client` package?
6. **Concurrency** — every goroutine has a `ctx` it honours? Atomic fields not mixed with mutex-protected fields in the same struct? `errgroup.Group` for fan-out?
7. **API shape** — context first, `Options` struct over positional bools, no package-level mutable state?
8. **Tests** — table-driven where shape repeats? `t.Helper()` on helpers? `t.Parallel()` where safe? No mocks of internal packages?
9. **Names** — exported names are nouns for types, verbs for methods? No stutter (`client.NewClient` is wrong, `client.New` is right)?

## Allocation-conscious patterns already in this codebase (reference these)

- `AppendMessage` — streams `msg.GetBody(...)` directly to `cli.Append`, no `ReadAll`.
- `StreamMessagesByUIDs` — batched UID FETCH in chunks of 500, ranges a channel.
- `FetchMessageMap` / `FetchMessageIDSet` — envelope-only via `BODY.PEEK[HEADER.FIELDS (MESSAGE-ID)]`, no full body.
- `Client.pw` / `Client.tracker` — `atomic.Pointer`, never raw fields.
- Per-folder `sync.Mutex` map (`folderLocks`) — fine-grained, not a global mutex.

If a new piece of code is heavier than the equivalent in this list, push back. The codebase has a high bar; new contributions inherit it.

## How to report findings

Group by file. For each finding: `file:line — what — why it matters — suggested fix`. Lead with the highest-impact items (allocations on hot paths, missing `safeCall` wrapping, broken layering). Polish items (comment style, naming) go at the bottom.
