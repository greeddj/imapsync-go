---
description: Hand the task to the `architect` agent. Architect validates, plans, may reject, and (if delegation is available) orchestrates the full workflow end-to-end.
argument-hint: <free-form task description>
---

Invoke the `architect` subagent with the task below. Pass the user's request verbatim. Do not pre-process. Do not implement anything yourself — the architect decides whether to proceed, what to delegate, and to whom.

Task from user:

$ARGUMENTS
