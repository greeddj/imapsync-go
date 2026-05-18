---
description: Run check+lint+test then `go run -race` the binary (runs `just run`).
allowed-tools: Bash(just run:*), Bash(just run)
---

Run `just run`. If `check`, `lint`, or `test` fail, stop and report — do not proceed to the binary launch.

!just run
