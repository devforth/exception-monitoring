# Exception Monitoring

> **Status: early development.** Nothing is released yet.

A lightweight, self-hosted service for monitoring application errors and logs.

It collects exceptions and logs from your backend and frontend apps. Repeated errors are grouped into issues. Your team is notified when something breaks. Everything runs on your own server, so your data never leaves your infrastructure.

## Goals

- **Lightweight.** A small, self-contained container that is easy to run next to your app.
- **Self-hosted.** Errors and logs are stored locally and are not sent to any third-party service.
- **Easy to connect.** You add monitoring to an existing app with minimal changes.
- **Short, actionable alerts.** Notifications contain only what you need to react.

## Planned features

- Error tracking for backend and frontend applications.
- Grouping of repeated errors into issues, with resolve/ignore and regression detection.
- Log search with links between logs and related errors.
- Notifications via Email, Telegram and Slack.
- A web UI for browsing issues and logs, with users and roles (superadmin, admin, user) and a history of who changed each issue's status.
- Multiple projects per instance.
- Data retention limits.
- An MCP server with OAuth sign-in and revocable tokens, so coding agents can read errors and logs and help explain them.

## License

[MIT](LICENSE) © Devforth.io
