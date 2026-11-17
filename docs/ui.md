# Settings in the web UI

What the fields of the settings pages mean. The UI shows the same text as hints (the question marks next to the fields), so this page is their source: change a hint here. Settings that come from the environment are in [Configuration](configuration.md).

## Projects

A project is one service (backend, frontend, worker) with its own DSN. Projects from `PROJECTS` keep their name and key from the environment; the rest of their settings change here.

| Field | |
|---|---|
| Color | Marks the project's badge in lists and the header of its emails. |
| DSN | The address the app's Sentry SDK sends errors and logs to. The key in it only allows sending to this project, so it can be in browser code. |
| Errors and messages | Event levels to store. Events of other levels are dropped when they arrive: not stored, no notifications. Unchanged, the project follows RECORD_EVENT_LEVELS. |
| Logs | Log levels to store; others are dropped when they arrive. Unchanged, the project follows RECORD_LOG_LEVELS. |

Retention fields are the variables of [Retention](configuration.md#retention) for this project only; left empty, the global value applies.

## Notifications

A connection sends new issues and issues that happen again after being resolved to one place: an email address, a Telegram chat or a Slack channel.

| Field | |
|---|---|
| Type | Email needs a mail server (SMTP_HOST); Telegram and Slack need only the tokens entered below. |
| Email address | Gets a 6-digit code first; the connection sends nothing until the code is entered. An address confirmed before is accepted at once. |
| Telegram bot token | The token @BotFather gives for your bot. The bot must be a member of the chat. Stored encrypted. |
| Chat ID | The chat the bot writes to: a number (groups and channels start with -100) or @channel_name of a public channel. To find the number, write a message in the chat and press Find chat ID: it is chat.id in the answer. |
| Incoming webhook URL | A Slack incoming webhook; it posts to the channel chosen when it was created. Mattermost and Rocket.Chat webhooks work too. Stored encrypted. |
| Slack bot token | Optional, instead of the webhook: a bot token (xoxb-…) with the chat:write scope. The bot posts each error and puts the prompt for a coding agent in a thread under it. Stored encrypted. |
| Channel ID | The channel the Slack bot posts to (C0123456789, at the bottom of the channel details). Invite the bot to it first. |
| Projects | Projects whose issues are sent. All projects includes the ones added later. |
| Levels | Levels that are sent. Events of other levels are still stored, they only do not notify. |
| Active | Off: nothing is sent, and messages waiting for a retry fail. |
| Include Open issue link | Adds a link to the issue page (needs PUBLIC_URL). Off by default for chats, which people outside the team may read. |
