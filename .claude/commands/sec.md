---
description: Hand a security audit brief to the `security` agent. Without args, audits the current branch against `main`.
argument-hint: [optional focus area; defaults to "current diff vs main"]
allowed-tools: Bash(git diff:*), Bash(git status:*), Bash(git log:*)
---

Invoke the `security` subagent.

Current branch state:

!git status --short
!git log --oneline -n 5
!git diff main --stat 2>/dev/null || git diff --stat

Focus:

$ARGUMENTS
