---
name: analyze-errors
description: Investigating an issue from its stack trace, events and logs, and matching it to the repository.
---

1. get_issue returns the latest event, or the one given by event_id, with stack frames and source, breadcrumbs, the request and up to 20 logs written during it. Start there; do not fetch the same data again.
2. Match the release and the in-app frames to the repository; deployed line numbers may differ from the current source.
3. Explain the failing path with file paths and lines, give concrete reproduction steps, and quote the frames, breadcrumbs or logs that support it. Say what is missing instead of guessing.
4. Fetch more only for a specific gap: list_issue_events to compare occurrences (environment, release), get_issue with an event_id for one of them, list_logs with the trace_id for more of the request's logs.
5. Change code only when the user asks for a fix. After the fix is verified, ask whether to mark the issue resolved (see update-issue-status).
