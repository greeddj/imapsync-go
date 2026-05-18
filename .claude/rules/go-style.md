# Go style rules for imapsync-go

These rules are stricter than the Go community defaults. The architect enforces them at review time.

## Allocations and ressources

- Prefer streaming to buffering. Examples already in the codebase: `AppendMessage` hands `msg.GetBody(...)` straight to `cli.Append` rather than `io.ReadAll`; `StreamMessagesByUIDs` ranges over a channel of messages, not a slice.
- Pre-size slices and maps when the upper bound is known (`make([]T, 0, n)`, `make(map[K]V, n)`). The sync planner does this for UID slices — follow the pattern.
- Avoid `fmt.Sprintf` on hot paths. Use `strconv` or `strings.Builder` when concatenating in a loop.
- `[]byte`/`string` conversions are not free. Don't roundtrip when a byte slice would do.
- `bytes.Buffer` / `strings.Builder` over `+=`.
- Don't pass large structs by value through hot paths; either pointer or shrink the struct.
- Field alignment is enforced by `go tool fieldalignment` (in `just check`). Reorder fields rather than ignoring the lint.

## Concurrency

- One channel direction per parameter (`chan<- T`, `<-chan T`) at function boundaries.
- Goroutines created in library code must have a defined termination condition tied to the caller's context. No fire-and-forget.
- `errgroup.Group` with a context-derived cancel for fan-out (`internal/app/show.go` uses this for parallel `ListMailboxes`).
- Atomic access to fields that cross goroutines (`atomic.Pointer`, `atomic.Int64`). Mixing atomics with mutex-protected fields in the same struct is a smell — pick one.

## Errors

- `fmt.Errorf("...: %w", err)` for wrapping; never `errors.New(err.Error())`.
- Sentinel errors as package-level `var Err... = errors.New("...")`.
- Classify errors at the boundary (`classifyError` in `internal/client/errors.go`); don't sprinkle `strings.Contains(err.Error(), ...)` further up the stack.
- Return the error or handle it. Never log-and-return.

## API shape

- Constructors take a context as the first parameter when they perform I/O (`client.New(ctx, ...)`).
- Optional arguments live in an `Options` struct — never positional bools. See `client.Options`.
- Exported names are nouns for types, verbs for methods. Avoid stutter (`client.Client` is acceptable for the package; `client.NewClient` is not — use `client.New`).
- No package-level mutable state.

## Comments

- Default: **no comment**. Identifiers should explain themselves.
- Write a comment only when WHY is non-obvious: an invariant, a workaround for a specific bug, a deliberately surprising choice.
- Never describe WHAT — the reader can read the code.
- Never reference the current task / PR / issue / "added for X". Those belong in the commit message and rot in the source.
- Exported types and functions need a single-line godoc starting with the identifier name, only when the identifier is part of a public-ish surface (i.e. used across packages). Internal helpers don't.

## Tests

- One `_test.go` per source file. Subtests with `t.Run`, table-driven where the cases share shape.
- Test names: `TestFunc_When_Then` or `TestFunc_Case` — readable English in the subtest name, not a punctuation soup.
- Use the real types — no mocks of internal packages. For IMAP, use the same `client.Client` and dial a fake (see existing tests). For nothing else.
- `t.Helper()` on any helper.
- `t.Cleanup` over `defer` in tests that mutate global state.
- `t.Parallel()` whenever possible.

## What we don't do

- No `init()` functions outside of CLI command registration.
- No `panic` in library code. Validation errors are returned. Programmer errors (impossible states) get a `panic("unreachable: <reason>")`.
- No reflection.
- No empty interfaces (`any`) in exported APIs.
- No backwards-compatibility shims when we can just change the code (per the project's "no half-finished implementations" rule).
