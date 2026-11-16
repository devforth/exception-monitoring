---
name: fetch-data
description: Filters, paging and level names for list_issues, list_issue_events and list_logs.
---

- Narrow on the server: project (name or ID), status (`unresolved` by default, `all` for every status), level, query (text in the title or location), since/until (RFC 3339, e.g. 2026-11-11T10:00:00Z), sort (`last_seen`, `first_seen` or `event_count`).
- Level names differ on purpose: issues use fatal, error, warning, info, debug; logs use fatal, error, warn, info, debug, trace.
- list_logs with trace_id returns the logs of one request or browser session; get_issue prints the trace ID of its event.
- A page has 20 rows unless limit says otherwise (1 to 100). With next_cursor, more rows exist: say so instead of presenting the page as complete, and fetch the next page with the same filters only when the task needs it.
- There is no aggregation tool. For totals per project use list_projects; do not count or chart from a partial page.
- Read details (get_issue) only for the rows that matter, not for every row of a list.
- Present issues as a table with ID, project, level, title, events (event_count) and last seen (last_seen, as a date and time or "N min ago"). Add first seen when sorting or filtering by it, and the status when the list mixes statuses. A ranking ("most events", "oldest") always shows the value it ranks by; rows with equal values still show it in every row.
- Logs: time, project, level and the line; events: time, level, environment and release.
