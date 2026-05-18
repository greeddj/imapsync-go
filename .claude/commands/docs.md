---
description: Hand a docs/comment audit brief to the `tech-writer` agent. Without args, audits the current branch against `main`.
argument-hint: [optional focus area; defaults to "current diff vs main"]
allowed-tools: Bash(git diff:*), Bash(git status:*)
---

Invoke the `tech-writer` subagent.

Current branch state:

!git status --short
!git diff main --stat 2>/dev/null || git diff --stat

Focus:

$ARGUMENTS
