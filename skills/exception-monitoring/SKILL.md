---
name: exception-monitoring
description: Set up and use Exception Monitoring, a self-hosted error and log monitor that receives data from the official Sentry SDKs. Use when the user wants to deploy the server, report an app's errors and logs to it (add a Sentry SDK with a DSN), connect a coding agent to its MCP server, investigate the errors it collected, or script its REST API.
---

# Exception Monitoring

A self-hosted server (one Docker container with an embedded database) that accepts errors and logs from the official Sentry SDKs through a DSN, groups errors into issues, notifies by email/Telegram/Slack and serves them to coding agents over MCP at `<server>/mcp`.

## Documentation

Read the page for the task before acting; it has the exact commands and settings. Prefer these over memory: they match the server version.

| Task | Page |
|---|---|
| Deploy, HTTPS, upgrades | https://raw.githubusercontent.com/devforth/exception-monitoring/main/docs/deployment.md |
| Environment variables | https://raw.githubusercontent.com/devforth/exception-monitoring/main/docs/configuration.md |
| Connect an app (SDK setup, levels, grouping) | https://raw.githubusercontent.com/devforth/exception-monitoring/main/docs/sdk.md |
| Connect a coding agent (MCP) | https://raw.githubusercontent.com/devforth/exception-monitoring/main/docs/mcp.md |
| REST API | https://raw.githubusercontent.com/devforth/exception-monitoring/main/docs/api.md |

## Rules for every task

- Secrets (`ADMIN_PASSWORD`, `API_TOKEN`, `SECRET_KEY`, SMTP passwords, personal MCP tokens) go into an env file or the agent's user-level config, never into a repository, a commit or your answer. Generate them with `openssl rand -hex 32`; check that the env file is gitignored.
- Ask before changing a remote server, a shared deployment or another team's app, and before sending test errors to a monitor other people watch.
- A DSN key is public by design (browsers embed it); it only allows sending to its project. It is still configuration: put it in the app's env or config, not scattered through code.
- Verify each step (health check, `list_projects`, a test error arriving) and report what you checked. If something fails, show the error instead of trying other URLs or settings at random.

## Deploy the server

1. Read deployment.md and configuration.md. Ask where it runs (this machine for a try, or a server with a domain) and whether a reverse proxy with HTTPS already exists.
2. Use the `compose.yml` from deployment.md and an `.env` next to it with at least `ADMIN_PASSWORD` (12+ characters) and, on a server, `SECRET_KEY` (otherwise generated into the data directory), `PUBLIC_URL=https://<domain>` and `TRUSTED_PROXIES`. Let the user choose the admin password or tell them where it is stored; do not print it.
3. Start it with `docker compose up -d` (or let the user start it if they prefer), then check `curl -fsS <server>/health`.
4. Tell the user to log in as `admin` and add a project per service on the **Projects** page (or set `PROJECTS`).

## Connect this agent (MCP)

If the tools `list_projects`, `list_issues` and `get_issue` are already available, skip this.

1. The endpoint is `<server>/mcp` (the **MCP** page of the UI shows it and the server's name). Use HTTPS unless the server is on this machine.
2. Prefer OAuth: add the server without a token, e.g. `claude mcp add --transport http <name> <server>/mcp` or `codex mcp add <name> --url <server>/mcp`. The user signs in and allows access in the browser at the first use.
3. Without a browser, the user generates a personal token on the MCP page and you add it at user level only, as mcp.md shows; never into a project file.
4. If the client must restart to load the server, say so. Then call `list_projects` and tell the user which projects you see.

## Connect an app

1. Find the app's platform and framework, and whether it already uses a Sentry SDK. If it reports to another Sentry server, ask before switching it.
2. Get the DSN from `list_projects` (each project has `dsn`), or ask the user to copy it from the Projects page. Use one project per service (backend, frontend, worker); if the right one is missing, ask the user to add it.
3. Follow sdk.md for the platform: install the official SDK, initialize it as early as the platform requires, read the DSN from the app's env or config, turn on logs (`enableLogs` / `enable_logs`). Tracing (`tracesSampleRate`) links logs to the failing request; spans are not stored.
4. Browser apps: the user's site must be in `ALLOWED_ORIGINS` (or it is `*`), and the server must be reachable from browsers.
5. Verify: with the user's consent, trigger a test error (e.g. `Sentry.captureException(new Error("exception-monitoring test"))`), then find it with `list_issues`. Only `error` and `fatal` events are stored by default; a test message at `info` is dropped unless the project records that level.

## Investigate errors

Once connected, the MCP server's own instructions and `fetch_skill` cover finding, reading and resolving issues. In short: `get_issue {"issue_id": "<id>"}` gives the error with stack, request, breadcrumbs and related logs; explain the cause with evidence from the code before proposing a fix, and change an issue's status only when the user asks or confirms.

## REST API

For scripts and CI, not for agents (they use MCP). Requests send `Authorization: Bearer $API_TOKEN`, read from the environment; see api.md.
