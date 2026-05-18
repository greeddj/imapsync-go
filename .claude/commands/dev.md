---
description: Hand a precise implementation brief to the `developer` agent. Use only when the architect has already produced the brief (or when explicitly bypassing architect for a trivial change).
argument-hint: <implementation brief or terse description for trivial changes>
---

Invoke the `developer` subagent. The prompt below is the brief. If the brief is missing pieces the developer agent requires (files to touch, contracts to preserve, tests required, out-of-scope), the developer will report back asking for clarification — do not pre-fill those gaps yourself.

Brief:

$ARGUMENTS
