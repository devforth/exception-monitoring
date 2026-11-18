# Coding agents (MCP)

The server is an [MCP](https://modelcontextprotocol.io) server at `/mcp` (Streamable HTTP), so coding agents such as Claude Code and Codex can list issues, read an error with its stack, request and logs, and change issue statuses. A typical request to an agent: "explain issue 42 and find the cause in this repository".

The **MCP** page of the UI has the exact commands for your server. The agent acts as the user who connected it, with that user's role, and every status change it makes is recorded under that user's name.

## Connect with OAuth (recommended)

Add the server; at the first use the agent opens the browser, you log in and press **Allow**.

Claude Code:

```sh
claude mcp add --transport http exception-monitoring https://errors.example.com/mcp
```

Codex:

```sh
codex mcp add exception-monitoring --url https://errors.example.com/mcp
```

The name is `APP_NAME` in lowercase with dashes (`exception-monitoring` by default); with a custom name the MCP page shows it. Connected agents are listed on the MCP page, where they can be disconnected. Access lasts an hour and is renewed by the agent for up to 30 days of inactivity.

Agents identify themselves either with a Client ID Metadata Document (their `client_id` is an https address of a document they publish; the consent page shows that site) or by registering with the server; both work without configuration.

## Connect with a personal token

For clients without OAuth, or servers without a browser: on the MCP page press **Generate token** and send it as `Authorization: Bearer <token>`. Each user has one personal token; regenerating it stops the old one. **Copy setup prompt** gives a prompt that has an agent add the server with the token itself.

Claude Code:

```sh
claude mcp add --transport http --scope user exception-monitoring https://errors.example.com/mcp \
  --header "Authorization: Bearer <token>"
```

Codex, in `~/.codex/config.toml`:

```toml
[mcp_servers.exception-monitoring]
url = "https://errors.example.com/mcp"
http_headers = { Authorization = "Bearer <token>" }
```

Keep the token out of repositories (`--scope user` in Claude Code).

## Agent skill

The [exception-monitoring skill](../skills/exception-monitoring/SKILL.md) tells an agent how to do the rest on its own: deploy the server, connect itself, add the Sentry SDK to an app with the DSN from `list_projects` and check that a test error arrives. Install it with `npx skills add devforth/exception-monitoring -g`, or copy the folder to `~/.claude/skills/` (Claude Code) or `~/.agents/skills/` (Codex).

## Tools

An agent acts as the user who connected it and is offered only the tools of that user's role; the role is read at every request, so a change applies at once. The data tools are the endpoints of the [REST API](api.md) with the same names and inputs; the management tools are for agents only, since the REST API has a token but no user.

| Tool | Role | |
|---|---|---|
| `list_issues`, `get_issues` | all | Issues filtered by project, status, level, text and dates; several by ID. |
| `get_issue`, `get_event` | all | An issue with its latest (or a given) event: exception, stack with source, request, breadcrumbs, related logs and status history, written as a prompt for the agent. |
| `list_issue_events`, `list_logs` | all | Events of an issue; logs filtered by project, level, text and trace. |
| `list_projects` | all | Projects with their issue counts and DSNs, so an agent can connect an app itself. |
| `update_issue_status` | all | Resolve, ignore or reopen up to 100 issues at once. |
| `get_account`, `update_account_name` | all | Who the agent acts for; change the display name. |
| `list_agents`, `disconnect_agent` | all | Connected agents and personal tokens (admins: also of users they manage). |
| `delete_issue` | admin | Delete an issue with its events. |
| `get_project`, `create_project`, `update_project`, `delete_project` | admin | Project settings: name, color, recorded levels. |
| `list_users`, `create_user`, `update_user`, `reset_user_password`, `delete_user` | admin | Users and roles; a new or reset password is temporary and returned once. |
| `list_connections`, `create_connection`, `update_connection`, `send_test_message`, `retry_connection`, `delete_connection`, `list_deliveries` | admin | Notification connections and the delivery log. |
| `start_email_verification`, `confirm_email` | admin | Confirm an email address with the code its owner received. |
| `fetch_skill` | all | Detailed guidance for the agent on analyzing errors, fetching data and changing statuses. |

Not available to agents: changing your own password (the UI asks for the current one) and generating personal tokens. Tools that delete data or reset passwords tell the agent to show what will change and wait for your confirmation in chat.

Notifications and the issue page carry the request for an agent, e.g. `Exception Monitoring MCP: get_issue {"issue_id":42}`, to paste into the agent.

## What leaves the server

Everything an agent reads is sent to its model provider. The server therefore never returns raw event payloads or log attributes, and replaces IP addresses, hosts, email addresses, credentials and home directories in the text with placeholders. Source lines are included only for your own code.

## Security

- Serve the server over HTTPS (the MCP page warns when agents would connect over plain HTTP from another machine).
- `ALLOWED_IPS` applies to MCP as well.
- Deleting a user, or resetting their password from the Users page, disconnects their agents.
