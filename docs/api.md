# REST API

A JSON API under `/api/v1/` for scripts and integrations. It is closed until `API_TOKEN` is set; every request sends the token, and `ALLOWED_IPS` applies:

```sh
curl -H "Authorization: Bearer $API_TOKEN" "https://errors.example.com/api/v1/list_issues?project=backend"
```

The API and the [MCP tools](mcp.md) are the same endpoints with the same names, inputs and outputs, so anything an agent does can be scripted, and the other way round. A few endpoints are REST only (marked below); managing projects, notifications and users is for agents only, since it acts as a user and the API has only a token. Status changes made through the API are recorded in the issue history as "API token".

## Conventions

- Each endpoint is `/api/v1/<name>`. Endpoints that only read are `GET` with the input as query parameters (`?project=backend&limit=50`; a list takes repeated or comma-separated values: `issue_ids=1,2`). Endpoints that change data are `POST` with the input as a JSON object.
- Errors: a status code with `{"error": "message"}`: 400 for invalid input (unknown parameters too), 401 without a valid token, 404 for missing records and unknown endpoints.
- Lists return `{"items": [...], "next_cursor": "..."}`. Pass `cursor=<next_cursor>` for the next page; there is no `next_cursor` on the last one. `limit` is 1–100, default 20.
- `project` takes a project id (`1`) or name (`backend`). `level` takes one level.
- Times are RFC 3339 in UTC, e.g. `2026-11-11T10:00:00Z`.
- Free text (titles, log lines) is returned as stored; the MCP tools return it with IPs, hosts, emails and credentials replaced, since it goes to a model provider.

## Endpoints

| Endpoint | Input | |
|---|---|---|
| `GET list_projects` | `limit`, `cursor` | Projects: `id`, `name`, `unresolved_issues`, `dsn`. |
| `GET list_issues` | see below | Issues: `id`, `project`, `title`, `location`, `level`, `status`, `event_count`, `first_seen`, `last_seen`, `resolved_at`. |
| `GET get_issues` | `issue_ids` (1–100) | `{"issues": [...], "missing_issue_ids": [...]}`. |
| `GET get_issue` | `issue_id`, `event_id` | The latest (or given) event of an issue as a text prompt for a coding agent: exception, stack, request, breadcrumbs, related logs, history. |
| `GET list_issue_events` | `issue_id`, `limit`, `cursor` | Events of an issue, newest first: `event_id`, `timestamp`, `level`, `environment`, `release`, `trace_id`. |
| `GET get_event` | `project`, `event_id` | Like `get_issue`, for an event ID (32 hex characters, with or without dashes). |
| `GET list_logs` | `project`, `level`, `query` (text in the line), `trace_id`, `since`, `until`, `limit`, `cursor` | Logs, newest first: `time`, `project`, `level`, `body`, `trace_id`. |
| `POST update_issue_status` | `issue_ids` (1–100), `status` | Set `resolved`, `ignored` or `unresolved` on all of them or none; returns the updated issues. |
| `GET get_event_payload` | `project`, `event_id` | REST only. The event with the full JSON the SDK sent (credentials removed) in `payload`. |
| `GET list_trace` | `trace_id` | REST only. Events (without payloads) and logs, with their attributes, of one trace, up to 100 of each. |
| `POST delete_issue` | `issue_id` | Delete the issue with its events: `{"deleted": <id>}` (agents: admins only). |

### list_issues input

| Parameter | |
|---|---|
| `project`, `level` | Filters. |
| `status` | `unresolved` (default), `resolved`, `ignored` or `all`. |
| `query` | Text in the title or location. |
| `since`, `until` | Time range, `since` inclusive, `until` exclusive. |
| `date_by` | What the range applies to: `event` (default: any event of the issue in the range), `first_seen` or `last_seen`. |
| `sort` | `last_seen` (default), `first_seen`, `event_count`, `id`, each newest or most first, or with `_asc`; `project` (A–Z) or `project_desc`. |
| `limit`, `cursor` | Paging. |

## Example: resolve every unresolved issue of a project

```sh
api="https://errors.example.com/api/v1"
auth="Authorization: Bearer $API_TOKEN"

ids=$(curl -s -H "$auth" "$api/list_issues?project=backend&limit=100" | jq -c '[.items[].id]')
curl -s -X POST -H "$auth" -d "{\"issue_ids\":$ids,\"status\":\"resolved\"}" "$api/update_issue_status"
```
