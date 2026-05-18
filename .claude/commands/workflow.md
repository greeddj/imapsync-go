---
description: Run the full architect-led multi-agent workflow on a task. Architect plans, developer implements, tester audits, security reviews, tech-writer finalises docs. Architect reports back to main thread when done.
argument-hint: <free-form task description>
---

Engage the full multi-agent workflow described in [.claude/rules/workflow.md](../rules/workflow.md).

Step 1 — invoke the `architect` subagent with the user's task below. Pass it verbatim.

Step 2 — the architect either rejects (in which case you relay the rejection back to the user and stop), asks one clarifying question (relay it), or proceeds. If it proceeds and has the `Agent` tool, it will run developer → tester → security → tech-writer itself and return the final structured report.

Step 3 — if the architect lacks the `Agent` tool (sandboxed mode), it returns an implementation brief. In that case, follow the fallback flow from [.claude/rules/workflow.md](../rules/workflow.md): you (main thread) invoke `developer`, then `architect` for review, then `tester`, then `architect`, then `security`, then `architect`, then `tech-writer`, then `architect` for the final report.

Do not implement anything yourself. Your role here is dispatcher and reporter.

Task from user:

$ARGUMENTS
