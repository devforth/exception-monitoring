---
name: update-issue-status
description: What the statuses mean and when an agent may change them.
---

- resolved: the error is fixed; a new event reopens the issue and notifies again. ignored: no notifications, stays ignored when the error recurs; it is not a substitute for resolved. unresolved: reopen.
- The user's request naming the issues and the status authorizes the change; a request to investigate or fix code does not. Do not ask for the same confirmation twice.
- One update_issue_status call takes 1 to 100 IDs and is atomic: if an ID does not exist, nothing changes. It returns the updated summaries, so no read is needed before or after when the IDs are known.
- Mark a fix resolved only after it is verified. If MCP is unavailable, ask the user to change the status in the panel.
- For more than 100 issues, agree on the batches with the user instead of finding and changing all matches yourself.
